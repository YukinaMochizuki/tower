package hangar

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/guest/guestops"
	"github.com/YukinaMochizuki/tower/internal/guest/vmssh"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// A VM Instance is a hangar VirtualMachine t-<uid> booting from a Disk of
// the same name, which the provider makes from the hangar Image its base
// image maps to (Config.BaseImages), or from its golden's CustomImage.
// tower reaches root in it with its own SSH (internal/guest/vmssh), as
// hostd does in its VMs, over hangar's port forward to the VM's port 22:
// hangar's own exec into a VM logs in as the image's default user, never
// root. cloud-init gives root the target's key (Config.SSHKey), and the
// hostname every tower VM has. The VM's sshd makes its own host keys, as a
// new instance, and the provider pins the one it first answers with on the
// Instance (status.ssh_host_key): every later connection, by this tower or
// one started since, must present it (docs/decisions/traffic.md).

// DefaultReadyTimeout is how long an operation in a VM just started waits
// for its ssh, as hostd's does.
const DefaultReadyTimeout = 5 * time.Minute

// vmMachineOf is the Machine a VirtualMachine runs as, as hangar names it
// (hangar: internal/apiserver/virtualmachines.go, vmMachineName).
func vmMachineOf(vm string) string { return "vm-" + vm }

// machineObj is what the provider reads of an instance's Container or
// VirtualMachine.
type machineObj struct {
	vm        bool
	name, uid string
	// stopped says it is asked to be stopped; ready is its Ready condition,
	// and generation the object's, which a Ready of an older one does not
	// speak for.
	stopped    bool
	ready      *hangarmeta.Condition
	generation int64
	deleted    bool
	// ipv4 is a VM's first interface's address as its status gives it,
	// with its prefix length; a container's is its machine's.
	ipv4 string
}

// address is the object's address on its first network, "" until it has
// one: a VM's from its status, a container's from its machine m.
func (o machineObj) address(m *hangarcompute.Machine) string {
	if !o.vm {
		return addressOf(m)
	}
	p, err := netip.ParsePrefix(o.ipv4)
	if err != nil {
		return ""
	}
	return p.Addr().String()
}

func containerObj(ct *hangarcompute.Container) machineObj {
	return machineObj{
		name: ct.GetMetadata().GetName(), uid: ct.GetMetadata().GetUid(), deleted: ct.GetMetadata().GetDeleteTime() != nil,
		stopped:    ct.GetSpec().GetRunState() == hangarcompute.ContainerRunState_CONTAINER_RUN_STATE_STOPPED,
		ready:      conditionOf(ct.GetStatus().GetConditions(), "Ready"),
		generation: ct.GetMetadata().GetGeneration(),
	}
}

func vmObj(vm *hangarcompute.VirtualMachine) machineObj {
	o := machineObj{
		vm: true, name: vm.GetMetadata().GetName(), uid: vm.GetMetadata().GetUid(), deleted: vm.GetMetadata().GetDeleteTime() != nil,
		stopped:    vm.GetSpec().GetRunState() == hangarcompute.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED,
		ready:      conditionOf(vm.GetStatus().GetConditions(), "Ready"),
		generation: vm.GetMetadata().GetGeneration(),
	}
	if ifs := vm.GetStatus().GetInterfaces(); len(ifs) > 0 {
		o.ipv4 = ifs[0].GetIpv4()
	}
	return o
}

// machine is the Machine it runs as.
func (o machineObj) machine() string {
	if o.vm {
		return vmMachineOf(o.name)
	}
	return machineOf(o.name)
}

// convergeVM makes hangar hold a VM instance as its spec says: its Network,
// its root Disk, and its VirtualMachine.
func (t *Target) convergeVM(ctx context.Context, in *envv1alpha1.Instance) (observed, error) {
	spec := in.GetSpec()
	if len(spec.GetEnv()) > 0 {
		return failed("Refused", errors.New("a VM takes no env")), nil
	}
	if t.key == nil {
		return failed("NoSSHKey", errors.New("this tower has no ssh key for the target's VMs")), nil
	}
	name := objectName(in.GetMetadata().GetUid())
	vm, err := t.api.VirtualMachines().Get(ctx, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return observed{}, err
	}
	disk, err := t.api.Disks().Get(ctx, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return observed{}, err
	}
	switch in.GetStatus().GetPhase() {
	case envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED, envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE:
		if vm == nil || disk == nil || vm.GetMetadata().GetDeleteTime() != nil {
			return observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE, reason: "Gone", msg: "the instance is not in hangar any more"}, nil
		}
	}
	network := ""
	if spec.GetNetwork() != "" {
		network = networkName(t.cfg.Target, in.GetMetadata().GetProject(), spec.GetNetwork())
		if err := t.ensureNetwork(ctx, network, in.GetMetadata().GetProject(), spec.GetNetwork()); err != nil {
			return t.applyFailed(err)
		}
	}
	if disk == nil {
		size := spec.GetResources().GetDisk()
		if size == "" {
			size = DefaultDisk
		}
		var source *hangarstorage.DiskSource
		var wait *observed
		if g := spec.GetDiskGolden(); g != "" {
			source, size, wait, err = t.goldenSource(ctx, in, g, size)
		} else {
			source, size, wait, err = t.baseImageSource(ctx, spec.GetImage(), size)
		}
		if err != nil {
			return observed{}, err
		}
		if wait != nil {
			return *wait, nil
		}
		if _, err := t.api.Disks().Apply(ctx, &hangarstorage.Disk{
			Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: t.labels(in)},
			Spec:     &hangarstorage.DiskSpec{Size: size, Source: source},
		}); err != nil {
			return t.applyFailed(err)
		}
	}
	want := t.vmSpec(in, name, network)
	if vm == nil || !proto.Equal(vm.GetSpec(), want) {
		if vm.GetMetadata().GetDeleteTime() != nil {
			return pending("Deleting", "an earlier VM of the name is being deleted"), nil
		}
		meta := &hangarmeta.ObjectMeta{Name: name, Labels: t.labels(in)}
		if vm, err = t.api.VirtualMachines().Apply(ctx, &hangarcompute.VirtualMachine{Metadata: meta, Spec: want}); err != nil {
			return t.applyFailed(err)
		}
	}
	return t.observe(ctx, in, vmObj(vm))
}

// baseImageSource is the source of a VM's root disk made from the hangar
// Image its base image maps to, and the disk's size: at least the image's.
// A base image that maps to none, or to an Image hangar does not have,
// fails the instance.
func (t *Target) baseImageSource(ctx context.Context, base, size string) (*hangarstorage.DiskSource, string, *observed, error) {
	image, ok := t.cfg.BaseImages[base]
	if !ok {
		f := failed("UnknownBaseImage", fmt.Errorf("base image %q is no hangar Image on target %s: its base_images map does not name it", base, t.cfg.Target))
		return nil, "", &f, nil
	}
	img, err := t.api.Image(ctx, image)
	if errors.Is(err, ErrNotFound) {
		f := failed("BaseImageNotInHangar", fmt.Errorf("base image %q is hangar Image %q, which hangar does not have", base, image))
		return nil, "", &f, nil
	}
	if err != nil {
		return nil, "", nil, err
	}
	for _, min := range []string{img.GetSpec().GetMinDiskSize(), img.GetStatus().GetDiskSize()} {
		size = atLeast(size, min)
	}
	return &hangarstorage.DiskSource{Source: &hangarstorage.DiskSource_Image{Image: &hangarstorage.ImageSource{Name: image}}}, size, nil, nil
}

// atLeast is size, or min where min is more.
func atLeast(size, min string) string {
	if min == "" {
		return size
	}
	want, werr := quantity.Bytes(size)
	have, herr := quantity.Bytes(min)
	if werr == nil && herr == nil && have > want {
		return min
	}
	return size
}

// vmSpec is the VirtualMachine the instance is, as hangar keeps what its
// client wrote.
func (t *Target) vmSpec(in *envv1alpha1.Instance, name, network string) *hangarcompute.VirtualMachineSpec {
	spec := in.GetSpec()
	s := &hangarcompute.VirtualMachineSpec{
		RootDisk: &hangarcompute.VirtualMachineRootDisk{Source: &hangarcompute.VirtualMachineRootDisk_Disk{Disk: name}},
		RunState: hangarcompute.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_RUNNING,
		UserData: vmssh.UserData{Authorized: t.key.Authorized, MaxSessions: vmMaxSessions}.String(),
	}
	if spec.GetRunState() == envv1alpha1.RunState_RUN_STATE_STOPPED {
		s.RunState = hangarcompute.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED
	}
	if r := spec.GetResources(); r.GetCpu() != "" || r.GetMemory() != "" {
		s.Resources = &hangarcompute.VirtualMachineResources{Cpu: r.GetCpu(), Memory: r.GetMemory()}
	}
	s.Interfaces = interfaces(network, spec.GetAlias())
	return s
}

// interfaces are a machine's on its workstream network, if it has one,
// named there by its alias too (hangar's NetworkInterface.aliases).
func interfaces(network, alias string) []*hangarcompute.NetworkInterface {
	if network == "" {
		return nil
	}
	i := &hangarcompute.NetworkInterface{Network: network}
	if alias != "" {
		i.Aliases = []string{alias}
	}
	return []*hangarcompute.NetworkInterface{i}
}

// vmConns are the provider's SSH connections to its VMs, one a VM, by the
// VirtualMachine's uid, each for one boot of it. Once closed, as the
// provider ends, it holds none: one handed to it is closed. A connection is
// closed outside its lock, and outside the target's: closing waits on the
// connection, which a frozen guest may hold.
type vmConns struct {
	mu     sync.Mutex
	conns  map[string]*vmConn
	closed bool
}

// get is a VM's connection, if it has one live at the boot.
func (v *vmConns) get(uid string, boot time.Time) *vmConn {
	v.mu.Lock()
	defer v.mu.Unlock()
	c := v.conns[uid]
	if v.closed || c == nil || !c.boot.Equal(boot) || c.ended() {
		return nil
	}
	return c
}

// set keeps a VM's connection, closing one it replaces; false says the
// provider has ended, and c is closed.
func (v *vmConns) set(uid string, c *vmConn) bool {
	v.mu.Lock()
	if v.closed {
		v.mu.Unlock()
		go c.end()
		return false
	}
	if v.conns == nil {
		v.conns = map[string]*vmConn{}
	}
	old := v.conns[uid]
	v.conns[uid] = c
	v.mu.Unlock()
	if old != nil && old != c {
		go old.end()
	}
	return true
}

// forget closes a VM's connection, as it stops or goes.
func (v *vmConns) forget(uid string) {
	v.mu.Lock()
	c := v.conns[uid]
	delete(v.conns, uid)
	v.mu.Unlock()
	if c != nil {
		go c.end()
	}
}

// closeAll closes every connection, as the provider ends, and refuses any
// after.
func (v *vmConns) closeAll() {
	v.mu.Lock()
	v.closed = true
	conns := v.conns
	v.conns = nil
	v.mu.Unlock()
	for _, c := range conns {
		c.end()
	}
}

// vmMaxSessions is what a hangar VM's sshd is set to open on one
// connection (vmssh.UserData.MaxSessions); sessionsPerConn the provider
// opens at most, below it, so the sshd refuses none. One more operation
// waits for a session to end.
const (
	vmMaxSessions   = 64
	sessionsPerConn = vmMaxSessions - 4
)

// Keepalives of a VM's connection: how often its sshd is asked whether it
// answers, and how long it has to.
const (
	DefaultKeepAliveEvery   = 15 * time.Second
	DefaultKeepAliveTimeout = 15 * time.Second
)

// vmConn is the provider's connection to one boot of a VM, its sessions
// bounded by a semaphore.
type vmConn struct {
	boot     time.Time
	conn     *vmssh.Conn
	sessions chan struct{}
	done     chan struct{}
	once     sync.Once
	// alive asks the VM's sshd whether it answers.
	alive func() bool
}

// newVMConn watches conn: once it ends, it is closed, and its sessions
// fail.
func newVMConn(boot time.Time, conn *vmssh.Conn, alive func() bool) *vmConn {
	c := &vmConn{boot: boot, conn: conn, sessions: make(chan struct{}, sessionsPerConn), done: make(chan struct{}), alive: alive}
	go func() {
		_ = conn.Wait()
		c.end()
	}()
	return c
}

// end closes the connection, once.
func (c *vmConn) end() {
	c.once.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

func (c *vmConn) ended() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// vmDial says how vmOps connects: wait is how long it waits for the VM's
// ssh; reconcile says the caller is a reconcile, which makes no connection
// and waits for none.
type vmDial struct {
	wait      time.Duration
	reconcile bool
}

// errUnreached is a VM the provider holds no connection to, which a
// reconcile does not make: what runs in it is not known.
var errUnreached = errors.New("the provider holds no connection to the VM")

// vmOps are the operations in a running VM, over the provider's SSH
// connection to it, which it makes (waiting for the VM's sshd, as a VM just
// started boots), copying tower-guest in, unless it has one for the VM's
// boot already. One VM's first uses wait for one another: one connects. A
// reconcile (d.reconcile) uses a connection already there, or none.
func (t *Target) vmOps(ctx context.Context, p placed, m *hangarcompute.Machine, d vmDial) (guestops.Ops, error) {
	if len(t.guest.data) == 0 {
		return guestops.Ops{}, fmt.Errorf("this tower has no tower-guest to run in hangar's machines: %w", provider.ErrFailed)
	}
	if t.key == nil {
		return guestops.Ops{}, fmt.Errorf("this tower has no ssh key for target %s's VMs: %w", t.cfg.Target, provider.ErrFailed)
	}
	boot := m.GetStatus().GetBootTime().AsTime()
	ops := func(c *vmConn) guestops.Ops {
		r := vmRunner{c: c}
		if !d.reconcile {
			// A connection that ended as an operation opened its session is
			// made again, for it.
			r.reconnect = func(ctx context.Context) (*vmConn, error) { return t.vmConn(ctx, p, boot, d.wait) }
		}
		return guestops.Ops{R: r, ProcRoot: vmssh.ProcRoot(p.name)}
	}
	if c := t.vms.get(p.uid, boot); c != nil {
		return ops(c), nil
	}
	if d.reconcile {
		// Connecting is the users' operations' to do: they may wait for the
		// VM's sshd, and a reconcile waits for nothing of the VM's.
		return guestops.Ops{}, errUnreached
	}
	c, err := t.vmConn(ctx, p, boot, d.wait)
	if err != nil {
		return guestops.Ops{}, err
	}
	return ops(c), nil
}

// vmConn is the provider's connection to the VM's boot, made if it holds
// none live: one caller connects at a time, the rest wait for it.
func (t *Target) vmConn(ctx context.Context, p placed, boot time.Time, wait time.Duration) (*vmConn, error) {
	unlock := t.procs.lock(p.uid)
	defer unlock()
	if c := t.vms.get(p.uid, boot); c != nil {
		return c, nil
	}
	// The host key it pins as it is now: another connection may have
	// pinned it since p was read.
	o, err := t.c.Get(ctx, registry.Instance, client.Key{Project: p.ref.Project, Name: p.ref.Name})
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, fmt.Errorf("instance %s/%s: %w", p.ref.Project, p.ref.Name, provider.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("reading instance %s/%s: %w: %w", p.ref.Project, p.ref.Name, err, provider.ErrUnavailable)
	}
	if in := o.(*envv1alpha1.Instance); in.GetMetadata().GetUid() == p.in.GetMetadata().GetUid() {
		p.in = in
	}
	conn, err := t.dialVM(ctx, p, wait)
	if err != nil {
		return nil, err
	}
	if err := conn.InstallGuest(ctx, t.guest.hash, func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(t.guest.data)), nil }); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("VM %s: copying tower-guest in: %w: %w", p.name, err, provider.ErrUnavailable)
	}
	conn.KeepAlive(t.cfg.KeepAliveEvery, t.cfg.KeepAliveTimeout)
	c := newVMConn(boot, conn, func() bool { return conn.Alive(t.cfg.KeepAliveTimeout) })
	if !t.vms.set(p.uid, c) {
		return nil, fmt.Errorf("the provider of target %s has ended: %w", t.cfg.Target, provider.ErrUnavailable)
	}
	return c, nil
}

// hostKeyAlgorithms are what a first connection asks a VM for, Ed25519
// first, as OpenSSH does.
var hostKeyAlgorithms = []string{
	ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256,
}

// dialVM connects to a VM's root over hangar's port forward to its port
// 22, waiting up to wait for its sshd while its machine runs. The VM must
// answer with the host key its Instance pins; one whose Instance pins none
// yet has the key it answers with pinned before the connection is used.
func (t *Target) dialVM(ctx context.Context, p placed, wait time.Duration) (*vmssh.Conn, error) {
	pinned := p.in.GetStatus().GetSshHostKey()
	var want ssh.PublicKey
	algorithms := hostKeyAlgorithms
	if pinned != "" {
		k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pinned))
		if err != nil {
			return nil, fmt.Errorf("instance %s/%s pins a host key that is not one: %w", p.ref.Project, p.ref.Name, provider.ErrFailed)
		}
		want = k
		algorithms = []string{k.Type()}
		if k.Type() == ssh.KeyAlgoRSA {
			algorithms = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
		}
	}
	var seen ssh.PublicKey
	check := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if want != nil && !bytes.Equal(key.Marshal(), want.Marshal()) {
			return hostKeyChanged(p.ref, key, want)
		}
		seen = key
		return nil
	}
	conn, err := vmssh.Dialer{
		What: fmt.Sprintf("instance %s/%s's VM %s", p.ref.Project, p.ref.Name, p.name), Addr: p.name, ReadyTimeout: wait,
		Config: vmssh.ClientConfig(*t.key, check, algorithms),
		Dial: func(ctx context.Context) (net.Conn, error) {
			return t.api.PortForward(ctx, PluralVirtualMachines, p.name, 22)
		},
		Alive: func(ctx context.Context) error {
			m, err := t.api.Machine(ctx, vmMachineOf(p.name))
			if err != nil {
				return providerError(err)
			}
			if !ownedBy(m, p.uid) || m.GetStatus().GetPhase() != hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING {
				return fmt.Errorf("instance %s/%s stopped as it booted: %w: %w", p.ref.Project, p.ref.Name, errNotRunning, provider.ErrFailed)
			}
			return nil
		},
	}.DialUntilReady(ctx)
	if err != nil {
		if errors.Is(err, vmssh.ErrRefused) || errors.Is(err, provider.ErrFailed) || ctx.Err() != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", err, provider.ErrUnavailable)
	}
	if pinned == "" {
		if err := t.pinHostKey(ctx, p, seen); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

// hostKeyChanged is a VM that answered with another host key than its
// Instance pins: refused, for good.
func hostKeyChanged(ref provider.Instance, got, want ssh.PublicKey) error {
	return fmt.Errorf("instance %s/%s: its VM answered with host key %s, not %s, the one it first answered with; tower refuses it (a VM whose keys were made again is a new instance): %w: %w",
		ref.Project, ref.Name, ssh.FingerprintSHA256(got), ssh.FingerprintSHA256(want), vmssh.ErrRefused, provider.ErrFailed)
}

// pinHostKey records the host key a VM first answered with on its
// Instance, unless one is there: then it must be that one.
func (t *Target) pinHostKey(ctx context.Context, p placed, key ssh.PublicKey) error {
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	k := client.Key{Project: p.ref.Project, Name: p.ref.Name}
	for range 10 {
		o, err := t.c.Get(ctx, registry.Instance, k)
		if connect.CodeOf(err) == connect.CodeNotFound {
			return fmt.Errorf("instance %s/%s: %w", k.Project, k.Name, provider.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("reading instance %s/%s: %w: %w", k.Project, k.Name, err, provider.ErrUnavailable)
		}
		in := o.(*envv1alpha1.Instance)
		if in.GetMetadata().GetUid() != p.in.GetMetadata().GetUid() {
			return fmt.Errorf("instance %s/%s was made again: %w", k.Project, k.Name, provider.ErrNotFound)
		}
		if have := in.GetStatus().GetSshHostKey(); have != "" {
			if have == line {
				return nil
			}
			want, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(have))
			if perr != nil {
				return fmt.Errorf("instance %s/%s pins a host key that is not one: %w", k.Project, k.Name, provider.ErrFailed)
			}
			return hostKeyChanged(p.ref, key, want)
		}
		st := proto.Clone(in.GetStatus()).(*envv1alpha1.InstanceStatus)
		if st == nil {
			st = &envv1alpha1.InstanceStatus{}
		}
		st.SshHostKey = line
		rv := in.GetMetadata().GetResourceVersion()
		_, err = t.c.UpdateStatus(ctx, registry.Instance, k, st, &rv)
		if connect.CodeOf(err) == connect.CodeAborted {
			continue
		}
		if err != nil {
			return fmt.Errorf("pinning instance %s/%s's host key: %w: %w", k.Project, k.Name, err, provider.ErrUnavailable)
		}
		t.log.Info("pinned a VM's host key as it first answered", "project", k.Project, "instance", k.Name, "host_key", ssh.FingerprintSHA256(key))
		return nil
	}
	return fmt.Errorf("instance %s/%s kept changing as its host key was pinned: %w", k.Project, k.Name, provider.ErrUnavailable)
}

// vmRunner runs tower-guest shim in a VM, a session on the provider's
// connection to it, waiting for room among its sessions as long as its ctx
// lets it. A session's own error, as its caller's writer failing, ends that
// session alone, and is said as it is. Only the connection ending, which
// its keepalive or its sshd's not answering says, fails the sessions on it
// as unavailable, and closes it; an operation whose session could not open
// on a connection that ended is tried once more on a new one, as nothing of
// it ran. A session the sshd refused leaves the connection be.
type vmRunner struct {
	c *vmConn
	// reconnect makes a new connection; nil, as a reconcile's, makes none.
	reconnect func(ctx context.Context) (*vmConn, error)
}

func (r vmRunner) RunShim(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	c := r.c
	for try := 0; ; try++ {
		code, err := c.runShim(ctx, stdin, stdout, stderr)
		if err == nil || ctx.Err() != nil {
			return code, err
		}
		var refused *ssh.OpenChannelError
		switch {
		case errors.As(err, &refused):
			return 0, fmt.Errorf("the VM's sshd refused a session: %w: %w", err, provider.ErrUnavailable)
		case !c.ended() && c.alive():
			// The connection answers: the error was the session's own.
			return 0, err
		}
		c.end()
		if !errors.Is(err, vmssh.ErrNoSession) || r.reconnect == nil || try > 0 {
			return 0, fmt.Errorf("the VM's ssh: %w: %w", err, provider.ErrUnavailable)
		}
		if c, err = r.reconnect(ctx); err != nil {
			return 0, err
		}
	}
}

// runShim runs a session once there is room for it.
func (c *vmConn) runShim(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	select {
	case c.sessions <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-c.done:
		return 0, fmt.Errorf("the connection ended: %w", vmssh.ErrNoSession)
	}
	defer func() { <-c.sessions }()
	return c.conn.RunShim(ctx, stdin, stdout, stderr)
}
