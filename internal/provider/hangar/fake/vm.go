package fake

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"

	"github.com/YukinaMochizuki/tower/internal/guest/vmssh"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
)

// A VM's filesystem is a directory of its root Disk's (diskDir), so a
// stop and a start keep it, and a capture of the disk copies it. A VM boots
// at once once applied running; its first boot as an instance (its uid,
// which is hangar's cloud-init instance-id) is cloud-init's as far as tower
// leans on it: the user_data's hostname is written to /etc/hostname, its
// ssh_authorized_keys given root if it says disable_root: false, and an
// ed25519 host key made, the VM's alone. A port forward to its port 22
// reaches an SSH server (x/crypto/ssh) speaking with that host key, which
// lets in root with those keys and runs what the provider runs as root:
// tower-guest's install line, which keeps the sha256 of what it was sent
// in the VM's filesystem, and `tower-guest shim`, which runs the real
// tower-guest as a container's does. Any other port forward dials the
// host's port, as a container's does. Like OpenSSH's, the sshd opens at
// most MaxSessions sessions on one connection, refusing the next, unless
// the user_data's drop-in (vmssh.SSHDDropIn) sets another; and for
// SetBootDelay after a VM boots, it refuses connections, as a guest still
// booting does. A frozen VM (Freeze) answers nothing on its connections.

// MaxSessions is the most sessions the fake's sshd opens on one
// connection unless the VM's user_data says otherwise, as OpenSSH's
// MaxSessions is by default.
const MaxSessions = 10

// vmState is a VM instance's, by its uid.
type vmState struct {
	booted bool
	// bootedAt is when it last booted.
	bootedAt   time.Time
	hostKey    ssh.Signer
	authorized []ssh.PublicKey
	conns      map[*ssh.ServerConn]bool
	// maxSessions is its sshd's MaxSessions; frozen says it answers
	// nothing.
	maxSessions int
	frozen      bool
	// refused counts the sessions its sshd refused, past MaxSessions;
	// sessions those open now; dials the port forwards to its port 22,
	// refused ones included.
	refused, sessions, dials int
}

// diskDir is a disk's contents, as a VM booting from it sees them.
func (f *Fake) diskDir(uid string) string { return filepath.Join(f.dir, "disks", uid) }

// AddImage adds a global Image to hangar's catalogue, at least
// minDiskSize big.
func (f *Fake) AddImage(name, minDiskSize string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.globals == nil {
		f.globals = map[string]*hangarstorage.Image{}
	}
	f.globals[name] = &hangarstorage.Image{
		Metadata: &hangarmeta.ObjectMeta{Name: name, Uid: f.uidLocked()},
		Spec:     &hangarstorage.ImageSpec{MinDiskSize: minDiskSize, DefaultUser: "ubuntu"},
	}
}

func (f *Fake) Image(_ context.Context, name string) (*hangarstorage.Image, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down != nil {
		return nil, f.down
	}
	img, ok := f.globals[name]
	if !ok {
		return nil, fmt.Errorf("image %s: %w", name, hangar.ErrNotFound)
	}
	return proto.Clone(img).(*hangarstorage.Image), nil
}

func (f *Fake) VirtualMachines() hangar.Kind[*hangarcompute.VirtualMachine] { return f.vms }

// VMNames are the project's VirtualMachines.
func (f *Fake) VMNames() []string { return f.vms.names() }

// VMHostKey is the host key a VM answers with, nil before its first boot.
func (f *Fake) VMHostKey(name string) ssh.PublicKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.vms.objs[name]
	if !ok {
		return nil
	}
	if st := f.vmStates[vm.GetMetadata().GetUid()]; st != nil && st.hostKey != nil {
		return st.hostKey.PublicKey()
	}
	return nil
}

// SetBootDelay makes every VM's sshd refuse connections for d after it
// boots.
func (f *Fake) SetBootDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bootDelay = d
}

// DropVMConnections ends every connection to a VM's sshd, as a port
// forward dropping does.
func (f *Fake) DropVMConnections(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm, ok := f.vms.objs[name]; ok {
		if st := f.vmStates[vm.GetMetadata().GetUid()]; st != nil {
			st.closeConns()
		}
	}
}

// VMConnections is how many connections a VM's sshd holds open now.
func (f *Fake) VMConnections(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm, ok := f.vms.objs[name]; ok {
		if st := f.vmStates[vm.GetMetadata().GetUid()]; st != nil {
			return len(st.conns)
		}
	}
	return 0
}

// SessionsRefused is how many sessions a VM's sshd refused, each past
// MaxSessions on its connection.
func (f *Fake) SessionsRefused(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm, ok := f.vms.objs[name]; ok {
		if st := f.vmStates[vm.GetMetadata().GetUid()]; st != nil {
			return st.refused
		}
	}
	return 0
}

// MaxSessionsOf is the sessions a VM's sshd opens on one connection, as
// its user_data set them.
func (f *Fake) MaxSessionsOf(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st := f.vmStateByName(name); st != nil {
		return st.maxSessions
	}
	return 0
}

// SetMaxSessions makes a VM's sshd open at most n sessions on a
// connection from now on, as an image whose sshd takes fewer.
func (f *Fake) SetMaxSessions(name string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st := f.vmStateByName(name); st != nil {
		st.maxSessions = n
	}
}

// Freeze makes a VM's sshd answer nothing more on the connections it
// holds, as a guest frozen; Thaw ends that.
func (f *Fake) Freeze(name string) { f.setFrozen(name, true) }

// Thaw lets a frozen VM's sshd answer again.
func (f *Fake) Thaw(name string) { f.setFrozen(name, false) }

func (f *Fake) setFrozen(name string, frozen bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st := f.vmStateByName(name); st != nil {
		st.frozen = frozen
	}
}

func (f *Fake) isFrozen(st *vmState) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return st.frozen
}

// SSHDials is how many port forwards to a VM's port 22 were asked for,
// refused ones included.
func (f *Fake) SSHDials(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st := f.vmStateByName(name); st != nil {
		return st.dials
	}
	return 0
}

// SessionsOpen is how many sessions a VM's sshd holds open now.
func (f *Fake) SessionsOpen(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if st := f.vmStateByName(name); st != nil {
		return st.sessions
	}
	return 0
}

// vmStateByName is a VM's state, nil for none. f.mu is held.
func (f *Fake) vmStateByName(name string) *vmState {
	if vm, ok := f.vms.objs[name]; ok {
		return f.vmStates[vm.GetMetadata().GetUid()]
	}
	return nil
}

// ChangeHostKey makes a VM's host key again and restarts its sshd, which
// ends every connection to it: as a guest whose keys were made again, or
// something else answering in its place.
func (f *Fake) ChangeHostKey(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.vms.objs[name]
	if !ok {
		return fmt.Errorf("no VM %s", name)
	}
	st := f.vmStates[vm.GetMetadata().GetUid()]
	if st == nil || !st.booted {
		return fmt.Errorf("VM %s never booted", name)
	}
	signer, err := newHostKey()
	if err != nil {
		return err
	}
	st.hostKey = signer
	st.closeConns()
	return nil
}

func newHostKey() (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

func (st *vmState) closeConns() {
	for c := range st.conns {
		_ = c.Close()
	}
	st.conns = map[*ssh.ServerConn]bool{}
}

// vmApplied boots or stops a VM's machine as its spec says, refusing a new
// one past the quota or naming what is not there. f.mu is held.
func (f *Fake) vmApplied(vm *hangarcompute.VirtualMachine, isNew bool) error {
	spec := vm.GetSpec()
	if len(spec.GetUserData()) > 16<<10 {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("user_data holds %d bytes, over 16 KiB", len(spec.GetUserData())))
	}
	disk, ok := f.disks.objs[spec.GetRootDisk().GetDisk()]
	if !ok {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("disk %q is not there; the fake boots VMs from a disk alone", spec.GetRootDisk().GetDisk()))
	}
	if isNew {
		if m := f.limitsLocked().Machines; m != nil && f.machinesLocked() >= *m {
			return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("the project holds %d machines, its limit", *m))
		}
	}
	for _, i := range spec.GetInterfaces() {
		if _, ok := f.networks.objs[i.GetNetwork()]; !ok {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("network %s is not there", i.GetNetwork()))
		}
	}
	if err := f.aliasesFreeLocked(vm.GetMetadata().GetName(), spec.GetInterfaces()); err != nil {
		return err
	}
	name := "vm-" + vm.GetMetadata().GetName()
	m, ok := f.machines[name]
	if !ok {
		m = &hangarcompute.Machine{
			Metadata: &hangarmeta.ObjectMeta{Name: name, Project: Project, Uid: f.uidLocked(), CreateTime: timestamppb.New(f.clock.Now()),
				OwnerReferences: []*hangarmeta.OwnerReference{{Kind: "VirtualMachine", Name: vm.GetMetadata().GetName(), Uid: vm.GetMetadata().GetUid(), Controller: true}}},
			Spec:   &hangarcompute.MachineSpec{},
			Status: &hangarcompute.MachineStatus{Phase: hangarcompute.MachinePhase_MACHINE_PHASE_STOPPED},
		}
		f.machines[name] = m
	}
	st := &hangarcompute.VirtualMachineStatus{RootDisk: disk.GetMetadata().GetName()}
	m.Spec.Interfaces = nil
	for i, ni := range spec.GetInterfaces() {
		eth := "eth" + strconv.Itoa(i)
		m.Spec.Interfaces = append(m.Spec.Interfaces, &hangarcompute.MachineInterface{Name: eth, Network: ni.GetNetwork(), Ipv4: "127.0.0.1/8", Aliases: ni.GetAliases()})
		st.Interfaces = append(st.Interfaces, &hangarcompute.VirtualMachineInterfaceStatus{Name: eth, Network: ni.GetNetwork(), Ipv4: "127.0.0.1/8"})
	}
	running := spec.GetRunState() != hangarcompute.VirtualMachineRunState_VIRTUAL_MACHINE_RUN_STATE_STOPPED
	switch {
	case running && m.GetStatus().GetPhase() != hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING:
		if err := f.bootLocked(vm, disk); err != nil {
			return err
		}
		f.vmStates[vm.GetMetadata().GetUid()].bootedAt = f.clock.Now()
		m.Status.Phase, m.Status.BootTime = hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING, timestamppb.New(f.clock.Now())
	case !running && m.GetStatus().GetPhase() == hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING:
		m.Status.Phase, m.Status.BootTime = hangarcompute.MachinePhase_MACHINE_PHASE_STOPPED, nil
		f.stopVMLocked(vm)
	}
	ready := &hangarmeta.Condition{Type: "Ready", Status: hangarmeta.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Running"}
	if !running {
		ready.Status, ready.Reason = hangarmeta.ConditionStatus_CONDITION_STATUS_FALSE, "Stopped"
	}
	st.Conditions = []*hangarmeta.Condition{ready}
	vm.Status = st
	return nil
}

// machinesLocked counts the project's machines, containers and VMs.
func (f *Fake) machinesLocked() int32 {
	return int32(len(f.containers.objs) + len(f.vms.objs))
}

// bootLocked boots a VM from its disk: its filesystem made from the disk's
// image the first time, and cloud-init's first boot of the instance.
func (f *Fake) bootLocked(vm *hangarcompute.VirtualMachine, disk *hangarstorage.Disk) error {
	root := f.diskDir(disk.GetMetadata().GetUid())
	if _, err := os.Stat(root); os.IsNotExist(err) {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return err
		}
		if src := disk.GetSpec().GetSource().GetImage(); src.GetProject() != "" {
			img, ok := f.images.objs[src.GetName()]
			if !ok {
				return connectError("failed_precondition", fmt.Sprintf("custom image %s is not there", src.GetName()))
			}
			if err := copyDir(f.imagesDir(img.GetMetadata().GetUid()), root); err != nil {
				return err
			}
		}
	}
	uid := vm.GetMetadata().GetUid()
	st := f.vmStates[uid]
	if st == nil {
		st = &vmState{conns: map[*ssh.ServerConn]bool{}}
		f.vmStates[uid] = st
	}
	if st.booted {
		return nil
	}
	hostname, rootKeys, err := cloudConfig(vm.GetSpec().GetUserData())
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if st.hostKey, err = newHostKey(); err != nil {
		return err
	}
	st.authorized = rootKeys
	st.maxSessions = MaxSessions
	if m := maxSessionsRE.FindStringSubmatch(vm.GetSpec().GetUserData()); m != nil {
		st.maxSessions, _ = strconv.Atoi(m[1])
	}
	if hostname != "" {
		if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, "etc", "hostname"), []byte(hostname+"\n"), 0o644); err != nil {
			return err
		}
	}
	st.booted = true
	return nil
}

// maxSessionsRE finds the MaxSessions a user_data's sshd drop-in sets.
var maxSessionsRE = regexp.MustCompile(`printf 'MaxSessions ([0-9]+)\\\\n' > ` + regexp.QuoteMeta(vmssh.SSHDDropIn))

// cloudConfig reads what the fake's cloud-init takes of a #cloud-config:
// its hostname, and the keys it gives root, those of ssh_authorized_keys
// if it says disable_root: false.
func cloudConfig(userData string) (hostname string, root []ssh.PublicKey, err error) {
	lines := strings.Split(userData, "\n")
	if len(lines) == 0 || lines[0] != "#cloud-config" {
		return "", nil, nil
	}
	enabled, inKeys := false, false
	var keys []ssh.PublicKey
	for _, l := range lines[1:] {
		if inKeys {
			if rest, ok := strings.CutPrefix(l, "  - "); ok {
				k, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(rest))
				if perr != nil {
					return "", nil, fmt.Errorf("ssh_authorized_keys: %w", perr)
				}
				keys = append(keys, k)
				continue
			}
			inKeys = false
		}
		switch {
		case strings.HasPrefix(l, "hostname: "):
			hostname = strings.TrimPrefix(l, "hostname: ")
		case l == "disable_root: false":
			enabled = true
		case l == "ssh_authorized_keys:":
			inKeys = true
		}
	}
	if enabled {
		root = keys
	}
	return hostname, root, nil
}

// stopVMLocked ends what a VM runs, as its machine stopping does.
func (f *Fake) stopVMLocked(vm *hangarcompute.VirtualMachine) {
	if d, ok := f.disks.objs[vm.GetSpec().GetRootDisk().GetDisk()]; ok {
		killProcessesIn(filepath.Join(f.diskDir(d.GetMetadata().GetUid()), strings.TrimPrefix(vmssh.ProcRoot(vm.GetMetadata().GetName()), "/")))
	}
	if st := f.vmStates[vm.GetMetadata().GetUid()]; st != nil {
		st.closeConns()
	}
}

// vmDeleting takes its machine with it, and what it runs. f.mu is held.
func (f *Fake) vmDeleting(vm *hangarcompute.VirtualMachine) error {
	f.stopVMLocked(vm)
	delete(f.machines, "vm-"+vm.GetMetadata().GetName())
	delete(f.vmStates, vm.GetMetadata().GetUid())
	return nil
}

// vmRunning is a running VM's uid and filesystem, by its name.
func (f *Fake) vmRunning(name string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down != nil {
		return "", "", f.down
	}
	vm, ok := f.vms.objs[name]
	if !ok {
		return "", "", connect.NewError(connect.CodeNotFound, fmt.Errorf("virtual machine %s not found", name))
	}
	if m := f.machines["vm-"+name]; m.GetStatus().GetPhase() != hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING {
		return "", "", connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("virtual machine %s is not running", name))
	}
	d := f.disks.objs[vm.GetSpec().GetRootDisk().GetDisk()]
	return vm.GetMetadata().GetUid(), f.diskDir(d.GetMetadata().GetUid()), nil
}

// vmPortForward connects to a running VM's port: its sshd's for 22, the
// host's otherwise.
func (f *Fake) vmPortForward(ctx context.Context, name string, port int32) (net.Conn, error) {
	uid, _, err := f.vmRunning(name)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	f.mu.Lock()
	booting := port == 22 && f.clock.Since(f.vmStates[uid].bootedAt) < f.bootDelay
	if port == 22 {
		f.vmStates[uid].dials++
	}
	f.mu.Unlock()
	if booting {
		// As hangar says a port nothing listens on yet.
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("virtual machine %q's port 22: connect: connection refused", name))
	}
	if port == 22 {
		if addr, err = f.sshd(); err != nil {
			return nil, err
		}
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	if port == 22 {
		// The fake's sshd is every VM's: it is told whose first.
		if _, err := io.WriteString(conn, uid+"\n"); err != nil {
			_ = conn.Close()
			return nil, connect.NewError(connect.CodeUnavailable, err)
		}
	}
	return conn, nil
}

// sshd is the address of the fake's sshd, started once.
func (f *Fake) sshd() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sshLn != nil {
		return f.sshLn.Addr().String(), nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	f.sshLn = ln
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serveSSH(conn)
		}
	}()
	return ln.Addr().String(), nil
}

// serveSSH serves one connection to a VM's sshd.
func (f *Fake) serveSSH(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	uid, err := readLine(conn)
	if err != nil {
		return
	}
	f.mu.Lock()
	st := f.vmStates[uid]
	var root string
	for _, vm := range f.vms.objs {
		if vm.GetMetadata().GetUid() == uid {
			if d, ok := f.disks.objs[vm.GetSpec().GetRootDisk().GetDisk()]; ok {
				root = f.diskDir(d.GetMetadata().GetUid())
			}
		}
	}
	if st == nil || st.hostKey == nil || root == "" {
		f.mu.Unlock()
		return
	}
	authorized := st.authorized
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(m ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if m.User() == "root" {
			for _, a := range authorized {
				if string(a.Marshal()) == string(k.Marshal()) {
					return nil, nil
				}
			}
		}
		return nil, fmt.Errorf("%s's key is not authorized", m.User())
	}}
	cfg.AddHostKey(st.hostKey)
	f.mu.Unlock()
	// A frozen VM reads nothing more from its connections.
	conn = &freezable{Conn: conn, frozen: func() bool { return f.isFrozen(st) }}
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	f.mu.Lock()
	st.conns[sc] = true
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(st.conns, sc)
		f.mu.Unlock()
		_ = sc.Close()
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ssh.DiscardRequests(reqs)
	var mu sync.Mutex
	open := 0
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "sessions only")
			continue
		}
		f.mu.Lock()
		maxSessions := st.maxSessions
		f.mu.Unlock()
		mu.Lock()
		full := open >= maxSessions
		if !full {
			open++
		}
		mu.Unlock()
		if full {
			// As OpenSSH refuses a session past MaxSessions.
			f.mu.Lock()
			st.refused++
			f.mu.Unlock()
			_ = nc.Reject(ssh.Prohibited, "open failed")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			mu.Lock()
			open--
			mu.Unlock()
			continue
		}
		f.mu.Lock()
		st.sessions++
		f.mu.Unlock()
		go func() {
			f.session(ctx, root, ch, creqs)
			mu.Lock()
			open--
			mu.Unlock()
			f.mu.Lock()
			st.sessions--
			f.mu.Unlock()
		}()
	}
}

// freezable is a connection that reads nothing while frozen.
type freezable struct {
	net.Conn
	frozen func() bool
}

// Read holds what it reads while frozen, a read already waiting as the VM
// froze included, until it thaws.
func (c *freezable) Read(p []byte) (int, error) {
	c.thawed()
	n, err := c.Conn.Read(p)
	c.thawed()
	return n, err
}

func (c *freezable) thawed() {
	for c.frozen() {
		time.Sleep(10 * time.Millisecond) //nolint:forbidigo // a frozen guest's sshd, polled
	}
}

// readLine reads a line a byte at a time, so nothing past it is read.
func readLine(r io.Reader) (string, error) {
	var b strings.Builder
	one := make([]byte, 1)
	for b.Len() < 128 {
		if _, err := io.ReadFull(r, one); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return b.String(), nil
		}
		b.WriteByte(one[0])
	}
	return "", fmt.Errorf("a line too long")
}

// session runs a session's one exec as root in the VM.
func (f *Fake) session(ctx context.Context, root string, ch ssh.Channel, reqs <-chan *ssh.Request) {
	for r := range reqs {
		if r.Type != "exec" {
			_ = r.Reply(false, nil)
			continue
		}
		var p struct{ Command string }
		if err := ssh.Unmarshal(r.Payload, &p); err != nil {
			_ = r.Reply(false, nil)
			continue
		}
		_ = r.Reply(true, nil)
		go func() {
			code := f.vmExec(ctx, root, p.Command, ch, ch, ch.Stderr())
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
			_ = ch.Close()
		}()
	}
}

// vmExec runs what the provider runs in a VM as root.
func (f *Fake) vmExec(ctx context.Context, root, line string, stdin io.Reader, stdout, stderr io.Writer) int {
	version := filepath.Join(root, strings.TrimPrefix(vmssh.GuestVersion, "/"))
	switch {
	case line == "cat "+vmssh.GuestVersion+" 2>/dev/null || true":
		b, _ := os.ReadFile(version)
		_, _ = stdout.Write(b)
		return 0
	case strings.Contains(line, vmssh.GuestBinary+".new"):
		b, err := io.ReadAll(stdin)
		if err != nil {
			return 1
		}
		sum := sha256.Sum256(b)
		if err := os.MkdirAll(filepath.Dir(version), 0o755); err != nil {
			return 1
		}
		if err := os.WriteFile(version, []byte(hex.EncodeToString(sum[:])+"\n"), 0o644); err != nil {
			return 1
		}
		f.mu.Lock()
		f.installs++
		f.mu.Unlock()
		return 0
	case line == vmssh.GuestBinary+" shim":
		if _, err := os.Stat(version); err != nil {
			_, _ = fmt.Fprintf(stderr, "sh: %s: not found\n", vmssh.GuestBinary)
			return 127
		}
		exit, err := f.shim(ctx, root, bufio.NewReader(stdin), stdout, stderr)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return exit.Code
	}
	_, _ = fmt.Fprintf(stderr, "the fake runs no %q\n", line)
	return 127
}
