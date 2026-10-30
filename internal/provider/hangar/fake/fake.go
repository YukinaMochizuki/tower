// Package fake is a hangar project in memory, for the hangar provider's
// tests: hangar.API over maps, as hangar keeps its objects (an apply
// replaces a spec and keeps what its client wrote, a uid and a create time
// are given once), with hangar's refusals that the provider leans on: a
// Disk a container mounts, or a Network a container is on, is not
// deleted, and a project over its quota refuses a machine.
//
// A container runs as soon as it is applied running. Its filesystem is a
// directory of the host's, and what runs in it runs on the host, as the
// real tower-guest (built by the test) with the absolute paths it is given
// taken under that directory: its header's, and its command's arguments.
// The installer's shell lines, which the provider runs as root to write
// tower-guest in, are answered as hangar's container would. Every
// container's address is 127.0.0.1, so a copy's sender reaches its
// receiver, and a port forward dials the host's port; hangar gives it with
// its prefix length, as 127.0.0.1/8. A VirtualMachine is in vm.go.
package fake

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarnetwork "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	hangarsecrets "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	hangartenancy "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"

	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
	"github.com/YukinaMochizuki/tower/internal/guest/proclog"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
)

// Project is the fake's project name.
const Project = "tower"

// Fake is one hangar project. Make it with New.
type Fake struct {
	// Bin, if set, is a directory of programs every container has on its
	// PATH, before the host's: the image's own, as the testbed's images
	// carry test/testbed/guestbin.
	Bin string
	// System, if set, is tower-system on the host, which a system
	// container's calls run (system.go).
	System string

	clock clockwork.Clock
	// guest is tower-guest on the host; dir holds each container's root.
	guest, dir string

	mu         sync.Mutex
	uids       int
	networks   *kind[*hangarnetwork.Network]
	disks      *kind[*hangarstorage.Disk]
	containers *kind[*hangarcompute.Container]
	vms        *kind[*hangarcompute.VirtualMachine]
	secrets    *kind[*hangarsecrets.Secret]
	images     *kind[*hangarstorage.CustomImage]
	epsvcs     *kind[*hangarnetwork.EndpointService]
	neps       *kind[*hangarnetwork.NetworkEndpoint]
	sgs        *kind[*hangarnetwork.SecurityGroup]
	machines   map[string]*hangarcompute.Machine
	// globals are hangar's catalogue (AddImage); vmStates each VM
	// instance's, by its uid; sshLn every VM's sshd. bootDelay is how long a
	// VM's sshd refuses connections after it boots (SetBootDelay).
	globals   map[string]*hangarstorage.Image
	vmStates  map[string]*vmState
	sshLn     net.Listener
	bootDelay time.Duration
	// installed is the hash of the tower-guest written into each
	// container, by its uid.
	installed map[string]string
	limits    *hangartenancy.Limits
	down      error
	// holdDeletes keeps what is deleted, being deleted, until
	// ReleaseDeletes; failPull are the images no pull of succeeds.
	holdDeletes bool
	failPull    map[string]bool
	// installs counts tower-guest written in, in every container.
	installs int
	// execsAs are the operations tower-guest shim ran, with the user each
	// was run as; noRoot fails every shim run as root.
	execsAs []ExecAs
	noRoot  bool
	// user, if set, is who a shim run as the container's user says it
	// runs as; rootAs, who one run as root does, fakeRoot if nil.
	user   *guest.Identity
	rootAs *guest.Identity
	// built are the images the fake built, by reference, with the user
	// each runs as.
	built map[string]string
	// builtEnv are the environments of the images it built.
	builtEnv map[string][]string
	// The endpoints (endpoints.go): whether the fake is a hangar without
	// them, why they are down, whether they drop what connects, how many
	// connections they took, the credentials the shims carried, how many
	// transits are given, each endpoint's listener and each system
	// container's server, by its uid.
	noEndpoints   bool
	endpointsDown string
	dropEndpoints bool
	// blackhole holds what connects to an endpoint, answering nothing;
	// serverDown keeps every system container's server down.
	blackhole     bool
	serverDown    bool
	endpointConns int
	credentials   []string
	transits      int
	proxies       map[string]*endpointProxy
	servers       map[string]*server
}

var _ hangar.API = (*Fake)(nil)

// New returns an empty project whose containers run guest, tower-guest,
// each in a directory under dir.
func New(guest, dir string, clock clockwork.Clock) *Fake {
	f := &Fake{clock: clock, guest: guest, dir: dir, machines: map[string]*hangarcompute.Machine{}, installed: map[string]string{}, vmStates: map[string]*vmState{}}
	f.networks = &kind[*hangarnetwork.Network]{f: f, what: "network", objs: map[string]*hangarnetwork.Network{}, keepStatus: true}
	f.networks.applied = f.networkApplied
	f.epsvcs = &kind[*hangarnetwork.EndpointService]{f: f, what: "endpoint service", objs: map[string]*hangarnetwork.EndpointService{}, gated: true}
	f.epsvcs.read = f.serviceRead
	f.neps = &kind[*hangarnetwork.NetworkEndpoint]{f: f, what: "network endpoint", objs: map[string]*hangarnetwork.NetworkEndpoint{}, gated: true}
	f.neps.applied = f.endpointApplied
	f.neps.deleting = f.endpointDeleting
	f.neps.read = f.endpointRead
	f.sgs = &kind[*hangarnetwork.SecurityGroup]{f: f, what: "security group", objs: map[string]*hangarnetwork.SecurityGroup{}, gated: true}
	f.sgs.deleting = f.sgDeleting
	f.disks = &kind[*hangarstorage.Disk]{f: f, what: "disk", objs: map[string]*hangarstorage.Disk{}}
	f.containers = &kind[*hangarcompute.Container]{f: f, what: "container", objs: map[string]*hangarcompute.Container{}}
	f.vms = &kind[*hangarcompute.VirtualMachine]{f: f, what: "virtual machine", objs: map[string]*hangarcompute.VirtualMachine{}}
	f.vms.applied = f.vmApplied
	f.vms.deleting = f.vmDeleting
	f.secrets = &kind[*hangarsecrets.Secret]{f: f, what: "secret", objs: map[string]*hangarsecrets.Secret{}}
	f.images = &kind[*hangarstorage.CustomImage]{f: f, what: "custom image", objs: map[string]*hangarstorage.CustomImage{}}
	f.images.applied = f.imageApplied
	f.images.deleting = f.imageDeleting
	f.disks.deleting = f.diskDeleting
	f.networks.deleting = f.networkDeleting
	f.containers.applied = f.containerApplied
	f.containers.deleting = f.containerDeleting
	return f
}

// Close ends every process of every container, as the host going would:
// for a test's end, so none outlives it.
func (f *Fake) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ct := range f.containers.objs {
		f.killProcesses(ct.GetMetadata().GetUid())
	}
	for _, vm := range f.vms.objs {
		f.stopVMLocked(vm)
	}
	for uid := range f.servers {
		f.stopServerLocked(uid)
	}
	for _, p := range f.proxies {
		_ = p.ln.Close()
	}
	if f.sshLn != nil {
		_ = f.sshLn.Close()
	}
}

// HoldDeletes makes a delete leave its object being deleted, its
// delete_time set, as hangar does until what it holds is released, until
// ReleaseDeletes.
func (f *Fake) HoldDeletes() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdDeletes = true
}

// ReleaseDeletes deletes what is being deleted, and deletes at once again.
func (f *Fake) ReleaseDeletes() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holdDeletes = false
	f.containers.releaseLocked()
	f.vms.releaseLocked()
	f.disks.releaseLocked()
	f.networks.releaseLocked()
	f.secrets.releaseLocked()
	f.images.releaseLocked()
	f.neps.releaseLocked()
	f.epsvcs.releaseLocked()
	f.sgs.releaseLocked()
}

// ExecAs is an operation tower-guest shim ran: its op, its command and
// who a started one runs as, if any, and the user hangar's exec ran the
// shim as: "" the container's, "0" root, as a VM's always is. One NoRoot
// refused has no op.
type ExecAs struct {
	Op      string
	Command []string
	As      *guest.Identity
	User    string
}

// ExecsAs are the operations tower-guest shim ran, in order.
func (f *Fake) ExecsAs() []ExecAs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.execsAs)
}

// NoRoot makes every shim run as root fail, as in a container whose
// runtime gives no root to an exec.
func (f *Fake) NoRoot() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noRoot = true
}

// ContainerUser makes a shim run as a container's user say it runs as id.
func (f *Fake) ContainerUser(id guest.Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.user = &id
}

// RootRunsAs makes a shim run as root say it runs as id, as in a container
// whose runtime maps root to another user; nil puts fakeRoot back.
func (f *Fake) RootRunsAs(id *guest.Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rootAs = id
}

// fakeRoot is who a shim run as root says it runs as: the fake runs it as
// the test's user all the same.
var fakeRoot = guest.Identity{UID: 0, GID: 0, Groups: []uint32{0}, Home: "/root"}

// Installs is how many times tower-guest was written into a container.
func (f *Fake) Installs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installs
}

// FailPull makes every pull of image fail, as a registry refusing it.
func (f *Fake) FailPull(image string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPull == nil {
		f.failPull = map[string]bool{}
	}
	f.failPull[image] = true
}

// SetLimits sets the project's quota; nil is none.
func (f *Fake) SetLimits(l *hangartenancy.Limits) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits = l
}

// SetDown makes every call fail with err, as a hangar that does not
// answer; nil answers again.
func (f *Fake) SetDown(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = err
}

func (f *Fake) Networks() hangar.Kind[*hangarnetwork.Network]         { return f.networks }
func (f *Fake) Disks() hangar.Kind[*hangarstorage.Disk]               { return f.disks }
func (f *Fake) Containers() hangar.Kind[*hangarcompute.Container]     { return f.containers }
func (f *Fake) Secrets() hangar.Kind[*hangarsecrets.Secret]           { return f.secrets }
func (f *Fake) CustomImages() hangar.Kind[*hangarstorage.CustomImage] { return f.images }
func (f *Fake) ProjectName() string                                   { return Project }

func (f *Fake) Machine(_ context.Context, name string) (*hangarcompute.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down != nil {
		return nil, f.down
	}
	m, ok := f.machines[name]
	if !ok {
		return nil, fmt.Errorf("machine %s: %w", name, hangar.ErrNotFound)
	}
	return proto.Clone(m).(*hangarcompute.Machine), nil
}

func (f *Fake) Project(_ context.Context) (*hangartenancy.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down != nil {
		return nil, f.down
	}
	return &hangartenancy.Project{
		Metadata: &hangarmeta.ObjectMeta{Name: Project},
		Status:   &hangartenancy.ProjectStatus{Limits: proto.Clone(f.limitsLocked()).(*hangartenancy.Limits), Usage: f.usageLocked()},
	}, nil
}

func (f *Fake) limitsLocked() *hangartenancy.Limits {
	if f.limits == nil {
		return &hangartenancy.Limits{}
	}
	return f.limits
}

// usageLocked counts the project as hangar does, a Disk at its size.
func (f *Fake) usageLocked() *hangartenancy.Usage {
	var gi int64
	for _, d := range f.disks.objs {
		n, _ := strconv.ParseInt(strings.TrimSuffix(d.GetSpec().GetSize(), "Gi"), 10, 64)
		gi += n
	}
	return &hangartenancy.Usage{Machines: f.machinesLocked(), Networks: int32(len(f.networks.objs)), NetworkEndpoints: int32(len(f.neps.objs)), Disk: strconv.FormatInt(gi, 10) + "Gi"}
}

// Objects are the project's objects of a kind, for tests to look at.
func (f *Fake) ContainerNames() []string { return f.containers.names() }
func (f *Fake) DiskNames() []string      { return f.disks.names() }
func (f *Fake) NetworkNames() []string   { return f.networks.names() }
func (f *Fake) SecretNames() []string    { return f.secrets.names() }

// Secret is a secret as applied, its data included.
func (f *Fake) Secret(name string) *hangarsecrets.Secret {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.secrets.objs[name]; ok {
		return proto.Clone(s).(*hangarsecrets.Secret)
	}
	return nil
}

// root is a container's filesystem on the host.
func (f *Fake) root(uid string) string { return filepath.Join(f.dir, uid) }

// containerApplied runs or stops a container's machine as its spec says,
// refusing a new one past the quota. f.mu is held.
func (f *Fake) containerApplied(ct *hangarcompute.Container, isNew bool) error {
	if isNew {
		if m := f.limitsLocked().Machines; m != nil && f.machinesLocked() >= *m {
			return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("the project holds %d machines, its limit", *m))
		}
		for _, v := range ct.GetSpec().GetVolumes() {
			if _, ok := f.disks.objs[v.GetSource()]; !ok {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("disk %s is not there", v.GetSource()))
			}
		}
		for _, i := range ct.GetSpec().GetInterfaces() {
			if _, ok := f.networks.objs[i.GetNetwork()]; !ok {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("network %s is not there", i.GetNetwork()))
			}
		}
	}
	if err := f.aliasesFreeLocked(ct.GetMetadata().GetName(), ct.GetSpec().GetInterfaces()); err != nil {
		return err
	}
	for _, i := range ct.GetSpec().GetInterfaces() {
		for _, g := range i.GetSecurityGroups() {
			if sg, ok := f.sgs.objs[g]; !ok || sg.GetSpec().GetNetwork() != i.GetNetwork() {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("security group %s is not one of network %s", g, i.GetNetwork()))
			}
		}
	}
	name := "ct-" + ct.GetMetadata().GetName()
	m, ok := f.machines[name]
	if !ok {
		if err := os.MkdirAll(f.root(ct.GetMetadata().GetUid()), 0o755); err != nil {
			return err
		}
		if err := f.fillVolumes(ct); err != nil {
			return err
		}
		m = &hangarcompute.Machine{
			Metadata: &hangarmeta.ObjectMeta{Name: name, Project: Project, Uid: f.uidLocked(), CreateTime: timestamppb.New(f.clock.Now()),
				OwnerReferences: []*hangarmeta.OwnerReference{{Kind: "Container", Name: ct.GetMetadata().GetName(), Uid: ct.GetMetadata().GetUid(), Controller: true}}},
			Spec:   &hangarcompute.MachineSpec{},
			Status: &hangarcompute.MachineStatus{Phase: hangarcompute.MachinePhase_MACHINE_PHASE_STOPPED},
		}
		f.machines[name] = m
	}
	m.Spec.Interfaces = nil
	for i, ni := range ct.GetSpec().GetInterfaces() {
		m.Spec.Interfaces = append(m.Spec.Interfaces, &hangarcompute.MachineInterface{Name: "eth" + strconv.Itoa(i), Network: ni.GetNetwork(), Ipv4: "127.0.0.1/8", Aliases: ni.GetAliases()})
	}
	running := ct.GetSpec().GetRunState() != hangarcompute.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	if f.failPull[ct.GetSpec().GetImage()] {
		// Its node cannot pull it: the machine waits, and Ready says why.
		m.Status.Phase = hangarcompute.MachinePhase_MACHINE_PHASE_PENDING
		ct.Status = &hangarcompute.ContainerStatus{Conditions: []*hangarmeta.Condition{{
			Type: "Ready", Status: hangarmeta.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "ImagePullFailed",
			Message: "pulling " + ct.GetSpec().GetImage() + ": the registry refused it",
		}}}
		return nil
	}
	switch {
	case running && m.GetStatus().GetPhase() != hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING:
		m.Status.Phase, m.Status.BootTime = hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING, timestamppb.New(f.clock.Now())
	case !running && m.GetStatus().GetPhase() == hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING:
		m.Status.Phase, m.Status.BootTime = hangarcompute.MachinePhase_MACHINE_PHASE_STOPPED, nil
		f.killProcesses(ct.GetMetadata().GetUid())
		f.stopServerLocked(ct.GetMetadata().GetUid())
	}
	return nil
}

// aliasesFreeLocked refuses an alias another machine holds on the same
// network, as hangar's admission does. f.mu is held.
func (f *Fake) aliasesFreeLocked(self string, ifs []*hangarcompute.NetworkInterface) error {
	held := func(other string, theirs []*hangarcompute.NetworkInterface) error {
		// other is as hangar names a holder: its kind and name.
		if other == "container "+self || other == "virtual machine "+self {
			return nil
		}
		for _, mine := range ifs {
			for _, them := range theirs {
				if mine.GetNetwork() != them.GetNetwork() {
					continue
				}
				for _, a := range mine.GetAliases() {
					if slices.Contains(them.GetAliases(), a) {
						return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("alias %q is held on network %q by %s", a, mine.GetNetwork(), other))
					}
				}
			}
		}
		return nil
	}
	for _, ct := range f.containers.objs {
		if err := held("container "+ct.GetMetadata().GetName(), ct.GetSpec().GetInterfaces()); err != nil {
			return err
		}
	}
	for _, vm := range f.vms.objs {
		if err := held("virtual machine "+vm.GetMetadata().GetName(), vm.GetSpec().GetInterfaces()); err != nil {
			return err
		}
	}
	return nil
}

// containerDeleting takes its machine and filesystem with it. f.mu is
// held.
func (f *Fake) containerDeleting(ct *hangarcompute.Container) error {
	f.killProcesses(ct.GetMetadata().GetUid())
	f.stopServerLocked(ct.GetMetadata().GetUid())
	delete(f.machines, "ct-"+ct.GetMetadata().GetName())
	delete(f.installed, ct.GetMetadata().GetUid())
	return os.RemoveAll(f.root(ct.GetMetadata().GetUid()))
}

// killProcesses ends every process of a container, as its machine
// stopping does.
func (f *Fake) killProcesses(uid string) {
	killProcessesIn(filepath.Join(f.root(uid), strings.TrimPrefix(procRoot, "/")))
}

// killProcessesIn ends every process a tower-guest supervises under root,
// its processes' directory.
func killProcessesIn(root string) {
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(root, e.Name(), proclog.PIDFile))
		if err != nil {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	}
}

func (f *Fake) diskDeleting(d *hangarstorage.Disk) error {
	for _, ct := range f.containers.objs {
		for _, v := range ct.GetSpec().GetVolumes() {
			if v.GetSource() == d.GetMetadata().GetName() {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("disk %s is mounted by container %s", v.GetSource(), ct.GetMetadata().GetName()))
			}
		}
	}
	for _, vm := range f.vms.objs {
		if vm.GetSpec().GetRootDisk().GetDisk() == d.GetMetadata().GetName() {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("disk %s is VM %s's root disk", d.GetMetadata().GetName(), vm.GetMetadata().GetName()))
		}
	}
	return os.RemoveAll(f.diskDir(d.GetMetadata().GetUid()))
}

func (f *Fake) networkDeleting(n *hangarnetwork.Network) error {
	for _, ct := range f.containers.objs {
		for _, i := range ct.GetSpec().GetInterfaces() {
			if i.GetNetwork() == n.GetMetadata().GetName() {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("network %s has container %s on it", i.GetNetwork(), ct.GetMetadata().GetName()))
			}
		}
	}
	for _, vm := range f.vms.objs {
		for _, i := range vm.GetSpec().GetInterfaces() {
			if i.GetNetwork() == n.GetMetadata().GetName() {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("network %s has VM %s on it", i.GetNetwork(), vm.GetMetadata().GetName()))
			}
		}
	}
	// Its endpoints are its own, which go with it.
	for _, ne := range f.neps.objs {
		if ne.GetSpec().GetNetwork() == n.GetMetadata().GetName() {
			_ = f.neps.deleteLocked(ne)
		}
	}
	return nil
}

func (f *Fake) uidLocked() string {
	f.uids++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", f.uids)
}

// running is a running container's uid, by its object's name.
func (f *Fake) running(plural, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down != nil {
		return "", f.down
	}
	if plural != hangar.PluralContainers {
		// tower reaches a VM by its own SSH over a port forward, never by
		// hangar's exec, which logs in as the image's default user.
		return "", connect.NewError(connect.CodeUnimplemented, fmt.Errorf("the fake execs in no %s", plural))
	}
	ct, ok := f.containers.objs[name]
	if !ok {
		return "", connect.NewError(connect.CodeNotFound, fmt.Errorf("container %s not found", name))
	}
	if m := f.machines["ct-"+name]; m.GetStatus().GetPhase() != hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING {
		return "", connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("container %s is not running", name))
	}
	return ct.GetMetadata().GetUid(), nil
}

// Where the provider keeps tower-guest and its processes in a container.
const (
	guestPath    = "/.tower/tower-guest"
	guestVersion = "/.tower/tower-guest.sha256"
	procRoot     = "/.tower/processes"
)

func (f *Fake) Exec(ctx context.Context, req hangar.ExecRequest) (hangar.ExecExit, error) {
	uid, err := f.running(req.Plural, req.Name)
	if err != nil {
		return hangar.ExecExit{}, err
	}
	stdin := req.Stdin
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	stdout, stderr := orDiscard(req.Stdout), orDiscard(req.Stderr)
	cmd := req.Start.GetCommand()
	switch {
	case len(cmd) == 2 && cmd[0] == guestPath && cmd[1] == "shim":
		f.mu.Lock()
		have := f.installed[uid]
		f.mu.Unlock()
		if have == "" {
			_, _ = fmt.Fprintf(stderr, "exec: %s: no such file\n", guestPath)
			return hangar.ExecExit{Code: 127}, nil
		}
		f.mu.Lock()
		noRoot := f.noRoot
		f.mu.Unlock()
		if noRoot && req.Start.GetUser() == "0" {
			f.mu.Lock()
			f.execsAs = append(f.execsAs, ExecAs{User: "0"})
			f.mu.Unlock()
			_, _ = fmt.Fprintln(stderr, "exec: unable to run as user 0")
			return hangar.ExecExit{Code: 126}, nil
		}
		return f.shim(ctx, f.root(uid), req.Start.GetUser(), stdin, stdout, stderr)
	case len(cmd) == 3 && cmd[0] == "/bin/sh" && cmd[1] == "-c":
		return f.installer(uid, cmd[2], stdin, stdout)
	case len(cmd) == 2 && cmd[0] == systemBinary && cmd[1] == "call":
		return f.system(ctx, req.Name, uid, stdin, stdout, stderr)
	}
	return hangar.ExecExit{}, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("the fake runs no %q", cmd))
}

// installer answers the provider's shell lines that write tower-guest in.
func (f *Fake) installer(uid, line string, stdin io.Reader, stdout io.Writer) (hangar.ExecExit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(line, "sha256sum"):
		if h := f.installed[uid]; h != "" {
			_, _ = fmt.Fprintln(stdout, h)
		}
		return hangar.ExecExit{}, nil
	case strings.HasPrefix(line, "echo $(id -u):$(id -g)"):
		_, _ = fmt.Fprintf(stdout, "%d:%d\n", os.Getuid(), os.Getgid())
		return hangar.ExecExit{}, nil
	case strings.Contains(line, guestPath+".new"):
		b, err := io.ReadAll(stdin)
		if err != nil {
			return hangar.ExecExit{}, err
		}
		sum := sha256.Sum256(b)
		if err := os.MkdirAll(filepath.Join(f.root(uid), strings.TrimPrefix(procRoot, "/")), 0o755); err != nil {
			return hangar.ExecExit{}, err
		}
		f.installed[uid] = hex.EncodeToString(sum[:])
		f.installs++
		return hangar.ExecExit{}, nil
	}
	return hangar.ExecExit{}, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("the fake's shell runs no %q", line))
}

// shim runs tower-guest shim on the host, the absolute paths of its header
// and its command's arguments taken under root, its command's working
// directory root unless it names one. user is who hangar's exec ran it as,
// which ExecsAs records; it runs as the test's user all the same, but for
// saying who it runs as (root as root, fakeRoot, and ContainerUser's as
// the user), and a command it starts runs as the test's user too, not as
// its start names.
func (f *Fake) shim(ctx context.Context, root, user string, stdin io.Reader, stdout, stderr io.Writer) (hangar.ExecExit, error) {
	in := bufio.NewReader(stdin)
	typ, data, err := guestbin.ReadFrame(in)
	if err != nil || typ != guestbin.FrameHeader {
		return hangar.ExecExit{}, fmt.Errorf("the shim's header: %w", err)
	}
	var h guestbin.Header
	if err := json.Unmarshal(data, &h); err != nil {
		return hangar.ExecExit{}, err
	}
	under := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Join(root, p)
		}
		return p
	}
	if (h.Op == guestbin.OpFetch || h.Op == guestbin.OpPost) && h.Token != "" {
		f.mu.Lock()
		f.credentials = append(f.credentials, h.Token)
		f.mu.Unlock()
	}
	f.mu.Lock()
	f.execsAs = append(f.execsAs, ExecAs{Op: h.Op, Command: slices.Clone(h.Command), As: h.As, User: user})
	f.mu.Unlock()
	f.mu.Lock()
	says, rootAs := f.user, f.rootAs
	f.mu.Unlock()
	if user == "0" {
		says = cmp.Or(rootAs, &fakeRoot)
	}
	if h.Op == guestbin.OpIdentity && says != nil {
		if err := json.NewEncoder(stdout).Encode(says); err != nil {
			return hangar.ExecExit{}, err
		}
		return hangar.ExecExit{}, nil
	}
	h.As = nil
	h.Path, h.Root = under(h.Path), under(h.Root)
	h.Dir = under(h.Dir)
	if h.Dir == "" {
		h.Dir = root
	}
	for i := 1; i < len(h.Command); i++ {
		h.Command[i] = under(h.Command[i])
	}
	var hdr bytes.Buffer
	if err := guestbin.WriteHeader(&hdr, h); err != nil {
		return hangar.ExecExit{}, err
	}
	cmd := exec.Command(f.guest, "shim")
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if f.Bin != "" {
		cmd.Env = append(os.Environ(), "PATH="+f.Bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	pipe, err := cmd.StdinPipe()
	if err != nil {
		return hangar.ExecExit{}, err
	}
	if err := cmd.Start(); err != nil {
		return hangar.ExecExit{}, err
	}
	// Its stdin ends with the call, as an exec's does as its WebSocket
	// closes: how the call's end reaches what it runs.
	go func() {
		_, _ = io.Copy(pipe, io.MultiReader(&hdr, in))
		_ = pipe.Close()
	}()
	stop := context.AfterFunc(ctx, func() { _ = pipe.Close() })
	defer stop()
	err = cmd.Wait()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return hangar.ExecExit{}, err
	}
	if ctx.Err() != nil {
		return hangar.ExecExit{}, ctx.Err()
	}
	return hangar.ExecExit{Code: cmd.ProcessState.ExitCode()}, nil
}

func (f *Fake) PortForward(ctx context.Context, plural, name string, port int32) (net.Conn, error) {
	if plural == hangar.PluralVirtualMachines {
		return f.vmPortForward(ctx, name, port)
	}
	if _, err := f.running(plural, name); err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return conn, nil
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// object is what a hangar object of every kind is.
type object interface {
	proto.Message
	GetMetadata() *hangarmeta.ObjectMeta
}

// kind is one kind's objects, by name.
type kind[T object] struct {
	f    *Fake
	what string
	objs map[string]T
	// applied, if set, is told of an object applied, before it is kept;
	// its error refuses the apply. deleting, if set, may refuse a delete.
	applied  func(obj T, isNew bool) error
	deleting func(obj T) error
	// read, if set, writes an object's status as it is read; gated kinds
	// are hangar#224's (WithoutEndpoints); keepStatus keeps the status an
	// object was given across applies.
	read       func(obj T)
	gated      bool
	keepStatus bool
}

func (k *kind[T]) names() []string {
	k.f.mu.Lock()
	defer k.f.mu.Unlock()
	var out []string
	for n := range k.objs {
		out = append(out, n)
	}
	return out
}

func (k *kind[T]) Get(_ context.Context, name string) (T, error) {
	k.f.mu.Lock()
	defer k.f.mu.Unlock()
	var zero T
	if k.f.down != nil {
		return zero, k.f.down
	}
	if err := k.f.unimplementedLocked(k.gated); err != nil {
		return zero, err
	}
	if !validName.MatchString(name) {
		return zero, connectError("invalid_argument", fmt.Sprintf("name: %q must be 1-63 lowercase letters, digits or '-', starting and ending with a letter or digit", name))
	}
	o, ok := k.objs[name]
	if !ok {
		return zero, fmt.Errorf("%s %s: %w", k.what, name, hangar.ErrNotFound)
	}
	if k.read != nil {
		k.read(o)
	}
	return proto.Clone(o).(T), nil
}

func (k *kind[T]) List(_ context.Context, selector string) ([]T, error) {
	k.f.mu.Lock()
	defer k.f.mu.Unlock()
	if k.f.down != nil {
		return nil, k.f.down
	}
	if err := k.f.unimplementedLocked(k.gated); err != nil {
		return nil, err
	}
	want := map[string]string{}
	for _, term := range strings.Split(selector, ",") {
		if k, v, ok := strings.Cut(term, "="); ok {
			want[k] = v
		}
	}
	var out []T
	for _, o := range k.objs {
		match := true
		for key, v := range want {
			if o.GetMetadata().GetLabels()[key] != v {
				match = false
			}
		}
		if match {
			if k.read != nil {
				k.read(o)
			}
			out = append(out, proto.Clone(o).(T))
		}
	}
	return out, nil
}

func (k *kind[T]) Apply(_ context.Context, obj T) (T, error) {
	k.f.mu.Lock()
	defer k.f.mu.Unlock()
	var zero T
	if k.f.down != nil {
		return zero, k.f.down
	}
	if err := k.f.unimplementedLocked(k.gated); err != nil {
		return zero, err
	}
	o := proto.Clone(obj).(T)
	meta := o.GetMetadata()
	if meta == nil || meta.GetName() == "" {
		return zero, connect.NewError(connect.CodeInvalidArgument, errors.New("metadata.name is required"))
	}
	meta.Project = Project
	for key := range meta.GetLabels() {
		// hangar keeps its own domain's keys to itself.
		if prefix, _, ok := strings.Cut(key, "/"); ok && (prefix == "hangarlab.cloud" || strings.HasSuffix(prefix, ".hangarlab.cloud")) {
			return zero, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("label %q uses a prefix reserved for the system", key))
		}
	}
	old, had := k.objs[meta.GetName()]
	if had {
		meta.Uid, meta.CreateTime, meta.Generation = old.GetMetadata().GetUid(), old.GetMetadata().GetCreateTime(), old.GetMetadata().GetGeneration()+1
		if k.keepStatus {
			keepStatus(old, o)
		}
	} else {
		meta.Uid, meta.CreateTime, meta.Generation = k.f.uidLocked(), timestamppb.New(k.f.clock.Now()), 1
	}
	if k.applied != nil {
		if err := k.applied(o, !had); err != nil {
			return zero, err
		}
	}
	k.objs[meta.GetName()] = o
	return proto.Clone(o).(T), nil
}

func (k *kind[T]) Delete(_ context.Context, name string) error {
	k.f.mu.Lock()
	defer k.f.mu.Unlock()
	if k.f.down != nil {
		return k.f.down
	}
	if err := k.f.unimplementedLocked(k.gated); err != nil {
		return err
	}
	o, ok := k.objs[name]
	if !ok {
		return nil
	}
	if k.f.holdDeletes {
		if o.GetMetadata().GetDeleteTime() == nil {
			o.GetMetadata().DeleteTime = timestamppb.New(k.f.clock.Now())
		}
		return nil
	}
	return k.deleteLocked(o)
}

func (k *kind[T]) deleteLocked(o T) error {
	if k.deleting != nil {
		if err := k.deleting(o); err != nil {
			return err
		}
	}
	delete(k.objs, o.GetMetadata().GetName())
	return nil
}

// releaseLocked deletes what is being deleted, what hangar refuses to
// delete yet staying so. f.mu is held.
func (k *kind[T]) releaseLocked() {
	for _, o := range k.objs {
		if o.GetMetadata().GetDeleteTime() != nil {
			_ = k.deleteLocked(o)
		}
	}
}

// connectError is a hangar refusal of the code, as Connect says it.
func connectError(code, msg string) error {
	var c connect.Code
	if err := c.UnmarshalText([]byte(code)); err != nil {
		c = connect.CodeUnknown
	}
	return connect.NewError(c, errors.New(msg))
}

// validName is what hangar takes as an object's name: a DNS label.
var validName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// keepStatus copies old's status field into o.
func keepStatus[T object](old, o T) {
	fd := old.ProtoReflect().Descriptor().Fields().ByName("status")
	if fd != nil && old.ProtoReflect().Has(fd) {
		o.ProtoReflect().Set(fd, old.ProtoReflect().Get(fd))
	}
}
