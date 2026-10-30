package hangar

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"path"
	"strconv"
	"strings"
	"sync"

	"connectrpc.com/connect"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/guest/guestops"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// Where tower-guest is kept in a container, on its root disk, beside the
// sha256 it was written as, and where it keeps the container's processes.
const (
	guestDir     = "/.tower"
	guestPath    = guestDir + "/tower-guest"
	guestVersion = guestDir + "/tower-guest.sha256"
	procRoot     = guestDir + "/processes"
)

// guestBinary is tower-guest as the provider writes it in.
type guestBinary struct {
	data []byte
	hash string
}

func newGuestBinary(b []byte) guestBinary {
	sum := sha256.Sum256(b)
	return guestBinary{data: b, hash: hex.EncodeToString(sum[:])}
}

// placed is an instance as hangar serves it.
type placed struct {
	ref provider.Instance
	in  *envv1alpha1.Instance
	// name is its Container's or VirtualMachine's name, vm whether it is a
	// VM; uid that object's uid, once it was read, and stopped whether it
	// is asked to be stopped.
	name, uid string
	vm        bool
	stopped   bool
}

// plural is the plural of its kind, which hangar's exec and port forward
// name.
func (p placed) plural() string {
	if p.vm {
		return PluralVirtualMachines
	}
	return PluralContainers
}

// errNotRunning is an instance whose machine does not run.
var errNotRunning = errors.New("not running")

// find reads the instance's Instance. One not there, or another target's,
// is ErrNotFound; an API that does not answer, ErrUnavailable.
func (t *Target) find(ctx context.Context, ref provider.Instance) (placed, error) {
	o, err := t.c.Get(ctx, registry.Instance, client.Key{Project: ref.Project, Name: ref.Name})
	if connect.CodeOf(err) == connect.CodeNotFound {
		return placed{}, fmt.Errorf("instance %s/%s: %w", ref.Project, ref.Name, provider.ErrNotFound)
	}
	if err != nil {
		return placed{}, fmt.Errorf("reading instance %s/%s: %w: %w", ref.Project, ref.Name, err, provider.ErrUnavailable)
	}
	in := o.(*envv1alpha1.Instance)
	if in.GetSpec().GetTarget() != t.cfg.Target {
		return placed{}, fmt.Errorf("instance %s/%s is target %q's: %w", ref.Project, ref.Name, in.GetSpec().GetTarget(), provider.ErrNotFound)
	}
	p := placed{ref: ref, in: in, name: objectName(in.GetMetadata().GetUid())}
	switch in.GetSpec().GetKind() {
	case envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER:
	case envv1alpha1.InstanceKind_INSTANCE_KIND_VM:
		p.vm = true
	default:
		return placed{}, fmt.Errorf("instance %s/%s: a hangar target runs containers and VMs, not %v: %w", ref.Project, ref.Name, in.GetSpec().GetKind(), provider.ErrFailed)
	}
	return p, nil
}

// object reads the instance's Container or VirtualMachine.
func (t *Target) object(ctx context.Context, p placed) (machineObj, error) {
	if p.vm {
		vm, err := t.api.VirtualMachines().Get(ctx, p.name)
		if err != nil {
			return machineObj{}, err
		}
		return vmObj(vm), nil
	}
	ct, err := t.api.Containers().Get(ctx, p.name)
	if err != nil {
		return machineObj{}, err
	}
	return containerObj(ct), nil
}

// machineRunning finds an instance whose machine runs, its Container or
// VirtualMachine, and that machine.
func (t *Target) machineRunning(ctx context.Context, ref provider.Instance) (placed, machineObj, *hangarcompute.Machine, error) {
	p, err := t.find(ctx, ref)
	if err != nil {
		return p, machineObj{}, nil, err
	}
	obj, err := t.object(ctx, p)
	if err != nil {
		return p, machineObj{}, nil, fmt.Errorf("instance %s/%s: %w", ref.Project, ref.Name, providerError(err))
	}
	m, err := t.api.Machine(ctx, obj.machine())
	if err != nil && !errors.Is(err, ErrNotFound) {
		return p, obj, nil, fmt.Errorf("instance %s/%s: %w", ref.Project, ref.Name, providerError(err))
	}
	p.uid, p.stopped = obj.uid, obj.stopped
	if err != nil || !ownedBy(m, obj.uid) || m.GetStatus().GetPhase() != hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING || p.stopped {
		return p, obj, nil, fmt.Errorf("instance %s/%s is %w: %w", ref.Project, ref.Name, errNotRunning, provider.ErrFailed)
	}
	return p, obj, m, nil
}

// running finds an instance whose machine runs, with tower-guest in it.
func (t *Target) running(ctx context.Context, ref provider.Instance) (placed, guestops.Ops, error) {
	p, obj, m, err := t.machineRunning(ctx, ref)
	if err != nil {
		return p, guestops.Ops{}, err
	}
	if p.vm {
		o, err := t.vmOps(ctx, p, m, vmDial{wait: t.cfg.ReadyTimeout})
		return p, o, err
	}
	o, err := t.ops(ctx, obj.name, obj.uid)
	return p, o, err
}

// ops are the operations in a running container, through tower-guest,
// which they write in first if the copy there is not this tower's. One
// container's first uses wait for one another: one writes it in.
func (t *Target) ops(ctx context.Context, container, uid string) (guestops.Ops, error) {
	if len(t.guest.data) == 0 {
		return guestops.Ops{}, fmt.Errorf("this tower has no tower-guest to run in hangar's machines: %w", provider.ErrFailed)
	}
	r := runner{api: t.api, container: container}
	if err := t.install(ctx, r, uid); err != nil {
		return guestops.Ops{}, err
	}
	return t.containerOps(ctx, r, uid)
}

// install writes tower-guest into a container unless this tower's is in
// it already.
func (t *Target) install(ctx context.Context, r runner, uid string) error {
	if t.procs.installed(uid, t.guest.hash) {
		return nil
	}
	unlock := t.procs.lock(uid)
	defer unlock()
	if !t.procs.installed(uid, t.guest.hash) {
		if err := t.installGuest(ctx, r); err != nil {
			return fmt.Errorf("container %s: writing tower-guest in: %w", r.container, providerError(err))
		}
		t.procs.setInstalled(uid, t.guest.hash)
	}
	return nil
}

// containerOps are the operations in a container whose tower-guest is
// this tower's, r running as its user. Its process operations run as root
// where they can, so its processes are measured, each started as that
// user (guestbin's runas.go); who that user is, and whether root can run,
// is learned once per container and kept.
func (t *Target) containerOps(ctx context.Context, r runner, uid string) (guestops.Ops, error) {
	o := guestops.Ops{R: r, ProcRoot: procRoot}
	w, ok := t.procs.whoRuns(uid)
	if !ok {
		unlock := t.procs.lockWho(uid)
		defer unlock()
		if w, ok = t.procs.whoRuns(uid); !ok {
			var err error
			if w, err = learnWho(ctx, r); err != nil {
				return guestops.Ops{}, fmt.Errorf("container %s: reading who runs in it: %w", r.container, err)
			}
			t.procs.setWhoRuns(uid, w)
		}
	}
	if w.root {
		root := r
		root.user = "0"
		o.ProcR, o.As = root, w.as
	}
	return o, nil
}

// whoRuns is who a container's processes run as: whether its process
// operations' shim runs as root, and the user a command started then
// drops to, nil for root.
type whoRuns struct {
	root bool
	as   *guest.Identity
}

// learnWho reads who r, the container's user, runs as, and whether a
// shim runs as root there. One that cannot leaves its processes as the
// user runs them, unmeasured; only a target that did not answer is an
// error.
func learnWho(ctx context.Context, r runner) (whoRuns, error) {
	id, err := guestops.Identity(ctx, r)
	if err != nil {
		return whoRuns{}, guestError(err)
	}
	root := r
	root.user = "0"
	rid, err := guestops.Identity(ctx, root)
	switch {
	case ctx.Err() != nil:
		return whoRuns{}, ctx.Err()
	case errors.Is(err, provider.ErrUnavailable):
		return whoRuns{}, err
	}
	// A root shim that did not run, or ran as another: no failure, its
	// processes run as before.
	if rooted := err == nil && rid.UID == 0; !rooted {
		return whoRuns{}, nil
	}
	w := whoRuns{root: true}
	if id.UID != 0 {
		w.as = &id
	}
	return w, nil
}

// errNoGuest is a container this tower's tower-guest is not in yet.
var errNoGuest = errors.New("this tower's tower-guest is not in the container yet")

// opsIfInstalled are the operations in a running container whose
// tower-guest is this tower's already, errNoGuest otherwise: what a
// reconcile reads, which writes nothing in. No process runs in a container
// before tower-guest is written in to start it.
func (t *Target) opsIfInstalled(ctx context.Context, container, uid string) (guestops.Ops, error) {
	if len(t.guest.data) == 0 {
		return guestops.Ops{}, errNoGuest
	}
	r := runner{api: t.api, container: container}
	if !t.procs.installed(uid, t.guest.hash) {
		have, err := r.guestHash(ctx)
		if err != nil {
			return guestops.Ops{}, err
		}
		if have != t.guest.hash {
			return guestops.Ops{}, errNoGuest
		}
		t.procs.setInstalled(uid, t.guest.hash)
	}
	return t.containerOps(ctx, r, uid)
}

// guestHashLine says the sha256 of the tower-guest in a container: of the
// file itself where sha256sum is there, else as it was written.
const guestHashLine = "if command -v sha256sum >/dev/null 2>&1; then sha256sum " + guestPath + " 2>/dev/null | cut -d' ' -f1; else cat " + guestVersion + " 2>/dev/null; fi; true"

// guestHash is the sha256 of the tower-guest in the container, "" for
// none.
func (r runner) guestHash(ctx context.Context) (string, error) {
	var have bytes.Buffer
	if _, err := r.sh(ctx, "0", guestHashLine, nil, &have); err != nil {
		return "", err
	}
	return strings.TrimSpace(have.String()), nil
}

// installGuest writes tower-guest into the container, as root, unless the
// copy there is this tower's, and gives the user the container runs as
// the processes' directory, /workspace while it is empty, and /repos (made
// if missing) while it is empty, as hostd does: so a process of a user
// that is not root (a devcontainer's remoteUser, say) is kept, writes its
// workspace, and has a box's later checkouts made under /repos, which
// tower-guest makes as that user. It is written to a file of its own and
// renamed into place, then read back.
func (t *Target) installGuest(ctx context.Context, r runner) error {
	have, err := r.guestHash(ctx)
	if err != nil {
		return err
	}
	if have == t.guest.hash {
		return nil
	}
	var who bytes.Buffer
	code, err := r.sh(ctx, "", "echo $(id -u):$(id -g)", nil, &who)
	if err != nil {
		return err
	}
	owner := strings.TrimSpace(who.String())
	if code != 0 || owner == "" || strings.ContainsFunc(owner, func(c rune) bool { return (c < '0' || c > '9') && c != ':' }) {
		return fmt.Errorf("reading the user the container runs as: exit %d, %q", code, owner)
	}
	script := "set -e; mkdir -p " + procRoot + "; tmp=" + guestPath + ".new.$$; cat > \"$tmp\"; chmod 0755 \"$tmp\"; mv -f \"$tmp\" " + guestPath +
		"; chown " + owner + " " + procRoot + "; echo " + t.guest.hash + " > " + guestVersion + "; " + giveDirsLine(owner, WorkspaceDir, ReposDir)
	var errOut bytes.Buffer
	code, err = r.sh(ctx, "0", script, bytes.NewReader(t.guest.data), &errOut)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit %d: %s", code, strings.TrimSpace(errOut.String()))
	}
	if have, err = r.guestHash(ctx); err != nil {
		return err
	}
	if have != t.guest.hash {
		return fmt.Errorf("tower-guest was written in as %q, not %q", have, t.guest.hash)
	}
	return nil
}

// runner runs tower-guest shim in a container by hangar's exec, as user
// ("" is the container's).
type runner struct {
	api       API
	container string
	user      string
}

func (r runner) RunShim(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	exit, err := r.api.Exec(ctx, ExecRequest{
		Plural: PluralContainers, Name: r.container,
		Start: &hangarcompute.MachineExecStart{Command: []string{guestPath, "shim"}, User: r.user},
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
	})
	if err != nil {
		return 0, providerError(err)
	}
	return exit.Code, nil
}

// sh runs a shell line in the container as user ("" is the container's),
// its stdout and stderr both to out.
func (r runner) sh(ctx context.Context, user, line string, stdin io.Reader, out io.Writer) (int, error) {
	exit, err := r.api.Exec(ctx, ExecRequest{
		Plural: PluralContainers, Name: r.container,
		Start: &hangarcompute.MachineExecStart{Command: []string{"/bin/sh", "-c", line}, User: user},
		Stdin: stdin, Stdout: out, Stderr: out,
	})
	return exit.Code, err
}

// giveDirsLine is the shell, run as root, that gives owner the workspace
// while it is empty, and the repos directory, made if missing, while it is
// empty: a directory that holds something is someone's already, and left
// as it is.
func giveDirsLine(owner, workspace, repos string) string {
	empty := func(d string) string { return "[ -d " + d + " ] && [ -z \"$(ls -A " + d + ")\" ]" }
	return "if " + empty(workspace) + "; then chown " + owner + " " + workspace + "; fi" +
		"; mkdir -p " + repos + "; if " + empty(repos) + "; then chown " + owner + " " + repos + "; fi"
}

// guestError is what an error of the guest's operations is to tower: the
// runner's are already said as tower's (RunShim), and the rest are the
// guest's, work that ran and failed.
func guestError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, guest.ErrNoProcess):
		return fmt.Errorf("%w: %w", err, provider.ErrNotFound)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, provider.ErrNotFound), errors.Is(err, provider.ErrUnavailable), errors.Is(err, provider.ErrFailed),
		errors.Is(err, provider.ErrNoSpace):
		return err
	}
	return fmt.Errorf("%w: %w", err, provider.ErrFailed)
}

func (t *Target) Exec(ctx context.Context, req provider.ExecRequest) (int, error) {
	_, o, err := t.running(ctx, req.Instance)
	if err != nil {
		return 0, err
	}
	// A VM's runner is root already (vmRunner).
	if r, ok := o.R.(runner); ok && req.AsRoot {
		r.user = "0"
		o.R = r
	}
	code, err := o.Exec(ctx, guest.ExecSpec{Command: req.Command, Env: req.Env, Dir: req.Dir, Stdin: req.Stdin, Stdout: req.Stdout, Stderr: req.Stderr})
	return code, guestError(err)
}

func (t *Target) ReadFile(ctx context.Context, in provider.Instance, p string, w io.Writer) error {
	_, o, err := t.running(ctx, in)
	if err != nil {
		return err
	}
	return guestError(o.ReadFile(ctx, p, w))
}

func (t *Target) WriteFile(ctx context.Context, in provider.Instance, p string, mode fs.FileMode, offset int64, r io.Reader) error {
	_, o, err := t.running(ctx, in)
	if err != nil {
		return err
	}
	return guestError(o.WriteFile(ctx, p, mode, offset, r))
}

// Copy sends fromPath from one container straight to the other, over the
// workstream network both are on: the bytes stay in hangar
// (docs/decisions/traffic.md). The destination listens for the source on
// a one-time token, and extracts what it sends at toPath.
func (t *Target) Copy(ctx context.Context, from provider.Instance, fromPath string, to provider.Instance, toPath string) error {
	sp, src, err := t.running(ctx, from)
	if err != nil {
		return err
	}
	dp, dst, err := t.running(ctx, to)
	if err != nil {
		return err
	}
	network := sp.in.GetSpec().GetNetwork()
	if network == "" || network != dp.in.GetSpec().GetNetwork() || sp.ref.Project != dp.ref.Project {
		return fmt.Errorf("instances %s/%s and %s/%s are not on one network: %w", from.Project, from.Name, to.Project, to.Name, provider.ErrFailed)
	}
	_, obj, m, err := t.machineRunning(ctx, to)
	if err != nil {
		return err
	}
	addr := obj.address(m)
	if addr == "" {
		return fmt.Errorf("instance %s/%s has no address yet: %w", to.Project, to.Name, provider.ErrFailed)
	}
	fromPath, toPath = path.Clean("/"+fromPath), path.Clean("/"+toPath)
	token := rand.Text()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	port := make(chan int, 1)
	received := make(chan error, 1)
	go func() {
		received <- dst.Receive(ctx, path.Dir(toPath), token, func(p int) { port <- p })
	}()
	select {
	case err := <-received:
		if err == nil {
			err = errors.New("the receiver ended before it listened")
		}
		return guestError(err)
	case p := <-port:
		if err := src.Send(ctx, fromPath, net.JoinHostPort(addr, strconv.Itoa(p)), token, path.Base(toPath)); err != nil {
			cancel()
			<-received
			return guestError(err)
		}
	}
	return guestError(<-received)
}

// processCache is what the provider keeps of each container, by its
// Container's uid: whether this tower's tower-guest is in it, and its
// processes as last listed, which are read from it while it runs and from
// here once it stopped.
type processCache struct {
	mu    sync.Mutex
	guest map[string]string
	last  map[string][]provider.Process
	// who is who each one's processes run as, kept while this tower runs:
	// a container's user is its image's.
	who map[string]whoRuns
	// installing serializes writing tower-guest into each container, and
	// learning reading who runs in it.
	installing map[string]*sync.Mutex
	learning   map[string]*sync.Mutex
}

// lock takes a container's install lock, returning its release.
func (c *processCache) lock(uid string) func() { return c.lockIn(&c.installing, uid) }

// lockWho takes the lock of learning who runs in a container, returning
// its release: apart from its install lock, which an install holds long.
func (c *processCache) lockWho(uid string) func() { return c.lockIn(&c.learning, uid) }

func (c *processCache) lockIn(locks *map[string]*sync.Mutex, uid string) func() {
	c.mu.Lock()
	if *locks == nil {
		*locks = map[string]*sync.Mutex{}
	}
	m := (*locks)[uid]
	if m == nil {
		m = &sync.Mutex{}
		(*locks)[uid] = m
	}
	c.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (c *processCache) installed(uid, hash string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.guest[uid] == hash
}

func (c *processCache) setInstalled(uid, hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.guest == nil {
		c.guest = map[string]string{}
	}
	c.guest[uid] = hash
}

func (c *processCache) whoRuns(uid string) (whoRuns, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w, ok := c.who[uid]
	return w, ok
}

func (c *processCache) setWhoRuns(uid string, w whoRuns) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.who == nil {
		c.who = map[string]whoRuns{}
	}
	c.who[uid] = w
}

// listed keeps a container's processes as listed.
func (c *processCache) listed(uid string, list []provider.Process) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[string][]provider.Process{}
	}
	c.last[uid] = list
}

// stopped records a container stopped: its processes ended with it, killed,
// and tower-guest is checked again once it runs.
func (c *processCache) stopped(uid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.guest, uid)
	for i, p := range c.last[uid] {
		if p.Exit == nil {
			c.last[uid][i].Exit = provider.Killed(provider.SignalKill)
		}
	}
}

// lastListed is a container's processes as last listed, and whether any
// were.
func (c *processCache) lastListed(uid string) ([]provider.Process, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	list, ok := c.last[uid]
	return append([]provider.Process(nil), list...), ok
}

func (c *processCache) forget(uid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.guest, uid)
	delete(c.last, uid)
	delete(c.installing, uid)
	delete(c.learning, uid)
	delete(c.who, uid)
}
