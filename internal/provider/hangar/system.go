package hangar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarnetwork "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"

	"github.com/YukinaMochizuki/tower/internal/guest/guestops"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/system"
)

// A hangar target's mirrors and builds are its system container's
// (internal/system): one Container of the target's own, on a network of
// its own, with a Disk at SystemRoot, running the system image, which
// joins the tailnet to push to tower's registry. tower runs tower-system
// in it, a call at a time, through hangar's exec. What a box takes of a
// mirror (a checkout, its origin, a push's objects) crosses as a tar:
// between the box and the container's server over hangar's endpoint
// where it can (endpoint.go), else relayed through tower, as the box and
// the system container are on networks hangar keeps apart
// (docs/decisions/traffic.md).

// What the system container is made with.
const (
	// SystemRoot is where its Disk is mounted.
	SystemRoot = "/var/lib/tower"
	// SystemNetworkCIDR is its network's range.
	SystemNetworkCIDR = "10.89.0.0/24"
	// DefaultSystemDisk, DefaultSystemCPU and DefaultSystemMemory size it
	// unless the config says otherwise. Its Disk (mirrors, BuildKit's
	// cache, tailscaled's state) starts at DefaultSystemDisk and grows as it
	// fills up to systemDiskMax, its root, which holds little but the image,
	// from systemRoot up to systemRootMax, as a box's do: hangar starts the
	// root at its image's size when that is larger.
	DefaultSystemDisk   = "4Gi"
	DefaultSystemCPU    = "2"
	DefaultSystemMemory = "4Gi"
	systemDiskMax       = "50Gi"
	systemRoot          = "2Gi"
	systemRootMax       = "10Gi"
	systemGrowThreshold = "80%"
	systemGrowStep      = "2x"
	// LabelRole is "system" on the system container's objects, which the
	// sweep leaves alone.
	LabelRole = "tower/role"
	// systemBinary is tower-system in the system image.
	systemBinary = "/usr/local/bin/tower-system"
	// systemCheck bounds a check of the system container, and tailnetJoin
	// its joining the tailnet.
	systemCheck = 30 * time.Second
	tailnetJoin = 2 * time.Minute
)

// systemName names the system container's Network, Disk and Container,
// apart from every other target's on the same project.
func systemName(target string) string {
	sum := sha256.Sum256([]byte(target))
	return "tower-sys-" + hex.EncodeToString(sum[:])[:12]
}

// systemState is what the provider last found of its system container.
type systemState struct {
	mu sync.Mutex
	// ready says tower-system answers in it, and why not when it does not.
	ready bool
	why   string
	// buildkit, tailnet and serving are its last status.
	buildkit string
	tailnet  bool
	serving  bool
}

func (s *systemState) set(ready bool, why string, st system.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready, s.why, s.buildkit, s.tailnet, s.serving = ready, why, st.BuildKit, st.Tailnet, st.Serving
}

// notServing says why the server in the system container is not to be
// reached, "" if it answered at the last check.
func (s *systemState) notServing() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case !s.ready:
		return "the system container does not answer"
	case !s.serving:
		return "tower-system serve does not answer in the system container"
	}
	return ""
}

func (s *systemState) get() (bool, string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready, s.why, s.buildkit, s.tailnet
}

// systemLabels are what the system container's objects carry.
func (t *Target) systemLabels() map[string]string {
	return map[string]string{LabelTarget: t.cfg.Target, LabelRole: "system"}
}

// EnsureSystem makes the system container as the config says, and checks
// that tower-system answers in it, joining it to the tailnet once with the
// config's auth key. With no system image, it makes nothing, and mirrors
// and builds fail saying so. In teardown, it makes nothing.
func (t *Target) EnsureSystem(ctx context.Context) error {
	if t.cfg.SystemImage == "" {
		t.sys.set(false, "this tower names no system image (--system-image)", system.Result{})
		return nil
	}
	ct, err := t.makeSystem(ctx)
	if errors.Is(err, errTornDown) {
		t.sys.set(false, "the target is being torn down", system.Result{})
		return nil
	}
	if err == nil {
		err = t.checkSystem(ctx, ct)
	}
	if err != nil {
		t.sys.set(false, err.Error(), system.Result{})
	}
	return err
}

// errTornDown is makeSystem's in teardown.
var errTornDown = errors.New("the target is being torn down")

// makeSystem makes the system container's objects, and returns its
// container. It holds the target's lock, which Teardown takes to put the
// target in teardown and for each pass, so it makes nothing a pass after
// that would not see.
func (t *Target) makeSystem(ctx context.Context) (*hangarcompute.Container, error) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.tornDown() {
		return nil, errTornDown
	}
	name := systemName(t.cfg.Target)
	labels := t.systemLabels()
	if _, err := t.api.Networks().Get(ctx, name); errors.Is(err, ErrNotFound) {
		if _, err := t.api.Networks().Apply(ctx, &hangarnetwork.Network{
			Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: labels},
			Spec:     &hangarnetwork.NetworkSpec{Ipv4Cidr: SystemNetworkCIDR},
		}); err != nil {
			return nil, fmt.Errorf("the system network: %w", err)
		}
	} else if err != nil {
		return nil, err
	}
	if _, err := t.api.Disks().Get(ctx, name); errors.Is(err, ErrNotFound) {
		size := or(t.cfg.SystemDisk, DefaultSystemDisk)
		if _, err := t.api.Disks().Apply(ctx, &hangarstorage.Disk{
			Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: labels},
			Spec: &hangarstorage.DiskSpec{Size: size, Grow: &hangarstorage.DiskGrow{
				Max: atLeast(systemDiskMax, size), Threshold: systemGrowThreshold, Step: systemGrowStep,
			}},
		}); err != nil {
			return nil, fmt.Errorf("the system disk: %w", err)
		}
	} else if err != nil {
		return nil, err
	}
	pull, err := t.ensurePullSecret(ctx)
	if err != nil {
		return nil, err
	}
	groups, err := t.ensureEndpointService(ctx, name, labels)
	if err != nil {
		return nil, fmt.Errorf("the system container's security groups: %w", err)
	}
	want := &hangarcompute.ContainerSpec{
		Image:         t.asPulled(t.cfg.SystemImage),
		RestartPolicy: hangarcompute.RestartPolicy_RESTART_POLICY_ALWAYS,
		RunState:      hangarcompute.ContainerRunState_CONTAINER_RUN_STATE_RUNNING,
		Resources: &hangarcompute.Resources{
			Cpu: or(t.cfg.SystemCPU, DefaultSystemCPU), Memory: or(t.cfg.SystemMemory, DefaultSystemMemory),
			Disk: systemRoot, DiskGrow: &hangarcompute.DiskGrow{Max: systemRootMax, Threshold: systemGrowThreshold, Step: systemGrowStep},
		},
		Interfaces: []*hangarcompute.NetworkInterface{{Network: name, SecurityGroups: groups}},
		Volumes:    []*hangarcompute.ContainerVolume{{Type: "volume", Source: name, Target: SystemRoot}},
		Env:        []*hangarcompute.EnvVar{{Name: "TOWER_REGISTRY", Value: t.cfg.Registry}},
	}
	if pull != "" {
		want.ImagePullSecrets = []string{pull}
	}
	if t.cfg.RegistryPlainHTTP {
		want.Env = append(want.Env, &hangarcompute.EnvVar{Name: "TOWER_REGISTRY_PLAIN_HTTP", Value: "1"})
	}
	ct, err := t.api.Containers().Get(ctx, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	keepRoot(want.Resources, ct)
	if ct == nil || !proto.Equal(ct.GetSpec(), want) {
		if ct.GetMetadata().GetDeleteTime() != nil {
			return nil, errors.New("the system container is being deleted")
		}
		if ct, err = t.api.Containers().Apply(ctx, &hangarcompute.Container{Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: labels}, Spec: want}); err != nil {
			return nil, fmt.Errorf("the system container: %w", err)
		}
	}
	return ct, nil
}

// keepRoot keeps the root of a system container made before, which hangar
// refuses to shrink: one made without a root size has hangar's default,
// larger than systemRoot, and keeps it, not growing until it is made
// again; one made with a larger root keeps that.
func keepRoot(r *hangarcompute.Resources, ct *hangarcompute.Container) {
	if ct == nil {
		return
	}
	had := ct.GetSpec().GetResources().GetDisk()
	if had == "" {
		r.Disk, r.DiskGrow = "", nil
		return
	}
	r.Disk = atLeast(r.GetDisk(), had)
	r.DiskGrow.Max = atLeast(r.GetDiskGrow().GetMax(), r.Disk)
}

// checkSystem checks that tower-system answers in the system container
// ct, joining it to the tailnet once.
func (t *Target) checkSystem(ctx context.Context, ct *hangarcompute.Container) error {
	name := ct.GetMetadata().GetName()
	m, err := t.api.Machine(ctx, machineOf(name))
	if errors.Is(err, ErrNotFound) || (err == nil && (!ownedBy(m, ct.GetMetadata().GetUid()) || m.GetStatus().GetPhase() != hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING)) {
		why := "the system container is not running yet"
		if r := conditionOf(ct.GetStatus().GetConditions(), "Ready"); r.GetMessage() != "" {
			why += ": " + r.GetMessage()
		}
		t.sys.set(false, why, system.Result{})
		return nil
	}
	if err != nil {
		return err
	}
	o := t.systemOps(name)
	check, cancel := context.WithTimeout(ctx, systemCheck)
	st, err := o.Status(check)
	cancel()
	if err != nil {
		return fmt.Errorf("tower-system does not answer in the system container: %w", err)
	}
	if !st.Tailnet && t.cfg.TailnetAuthKey != "" {
		join, cancel := context.WithTimeout(ctx, tailnetJoin)
		err := o.JoinTailnet(join, "tower-builder-"+label(t.cfg.Target), t.cfg.TailnetAuthKey)
		cancel()
		if err != nil {
			t.log.Warn("the system container did not join the tailnet", "error", err)
		} else {
			st.Tailnet = true
		}
	}
	if st.Serving {
		sync, cancel := context.WithTimeout(ctx, systemCheck)
		err := t.syncSources(sync, o)
		cancel()
		if err != nil {
			t.log.Warn("giving the system container's server the networks", "error", err)
		}
	}
	t.sys.set(true, "", st)
	return nil
}

// SystemReady reports whether tower-system answered in the system
// container at its last check.
func (t *Target) SystemReady() bool {
	ready, _, _, _ := t.sys.get()
	return ready
}

// systemOps are tower-system's calls in the named container.
func (t *Target) systemOps(name string) system.Ops {
	return system.Ops{R: systemRunner{api: t.api, container: name}}
}

// systemRunner runs tower-system call in the system container by hangar's
// exec, as root.
type systemRunner struct {
	api       API
	container string
}

func (r systemRunner) RunSystem(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	exit, err := r.api.Exec(ctx, ExecRequest{
		Plural: PluralContainers, Name: r.container,
		Start: &hangarcompute.MachineExecStart{Command: []string{systemBinary, "call"}, User: "0"},
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
	})
	if err != nil {
		return 0, providerError(err)
	}
	return exit.Code, nil
}

// sysOps are the system container's calls, once it answers.
func (t *Target) sysOps() (system.Ops, error) {
	if t.cfg.SystemImage == "" {
		return system.Ops{}, fmt.Errorf("this tower names no system image (--system-image), so a hangar target keeps no mirror and builds no image: %w", provider.ErrFailed)
	}
	ready, why, _, _ := t.sys.get()
	if !ready {
		if why == "" {
			why = "the system container has not been checked yet"
		}
		return system.Ops{}, fmt.Errorf("%s: %w", why, provider.ErrUnavailable)
	}
	return t.systemOps(systemName(t.cfg.Target)), nil
}

// mirrorOp is the system container's calls for one mirror operation, and
// what ends it, once one of the target's MirrorOps is free (mirrorSlot).
func (t *Target) mirrorOp(ctx context.Context) (system.Ops, func(), error) {
	o, err := t.sysOps()
	if err != nil {
		return system.Ops{}, nil, err
	}
	end, err := t.mirrorSlot(ctx)
	return o, end, err
}

// mirrorSlot waits for one of the target's MirrorOps, and for one of those
// work that is not interactive may take, and returns what gives them back.
func (t *Target) mirrorSlot(ctx context.Context) (func(), error) {
	background := !provider.IsInteractive(ctx)
	if background {
		select {
		case t.backgroundOps <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case t.mirrorOps <- struct{}{}:
	case <-ctx.Done():
		if background {
			<-t.backgroundOps
		}
		return nil, ctx.Err()
	}
	return func() {
		<-t.mirrorOps
		if background {
			<-t.backgroundOps
		}
	}, nil
}

// errRelayEnded ends the pipe between a box and the system container once
// one side is done with it.
var errRelayEnded = errors.New("the relay ended")

// driverError is what an error of the system container's drivers is to
// tower.
func driverError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, driver.ErrNotFound):
		return fmt.Errorf("%w: %w", err, provider.ErrNotFound)
	case errors.Is(err, driver.ErrNoSpace):
		return fmt.Errorf("%w: %w", err, provider.ErrNoSpace)
	case errors.Is(err, driver.ErrLeaseLost):
		return fmt.Errorf("%w: %w", err, provider.ErrLeaseLost)
	}
	return guestError(err)
}

func (t *Target) Fetch(ctx context.Context, req provider.FetchRequest) (string, error) {
	o, end, err := t.mirrorOp(ctx)
	if err != nil {
		return "", err
	}
	defer end()
	head, err := o.Fetch(ctx, driver.FetchSpec{Mirror: req.Mirror, URL: req.URL, Token: req.Token, Branch: req.Branch})
	return head, driverError(err)
}

func (t *Target) ReadMirror(ctx context.Context, req provider.MirrorReadRequest) (provider.MirrorRead, error) {
	o, end, err := t.mirrorOp(ctx)
	if err != nil {
		return provider.MirrorRead{}, err
	}
	defer end()
	var most int64
	if req.Content {
		most = provider.MaxMirrorContent
	}
	commit, entries, err := o.Read(ctx, req.Mirror, req.Ref, req.Paths, most)
	if err != nil {
		return provider.MirrorRead{}, driverError(err)
	}
	out := provider.MirrorRead{Commit: commit, Entries: map[string]provider.MirrorEntry{}}
	for p, e := range entries {
		out.Entries[p] = provider.MirrorEntry{Object: e.Object, Dir: e.Dir, Size: e.Size, Content: e.Content}
	}
	return out, nil
}

func (t *Target) CommitsBehind(ctx context.Context, mirror, commit, branch string) (int, error) {
	o, end, err := t.mirrorOp(ctx)
	if err != nil {
		return 0, err
	}
	defer end()
	n, err := o.CommitsBehind(ctx, mirror, commit, branch)
	return n, driverError(err)
}

// Checkout brings a clone of the mirror from the system container into
// the instance at dir, over its network's endpoint or relayed.
func (t *Target) Checkout(ctx context.Context, in provider.Instance, mirror, ref, dir string) (string, error) {
	o, err := t.sysOps()
	if err != nil {
		return "", err
	}
	p, box, err := t.running(ctx, in)
	if err != nil {
		return "", err
	}
	// Once the instance runs, which may take a VM's boot: no slot is held
	// meanwhile.
	end, err := t.mirrorSlot(ctx)
	if err != nil {
		return "", err
	}
	defer end()
	res, done, why, err := t.overEndpoint(ctx, o, p, box, system.GrantSpec{Op: system.GrantCheckout, Mirror: mirror, Ref: ref}, path.Clean("/"+dir))
	if done {
		return res.Commit, driverError(err)
	}
	t.relaying(p, system.GrantCheckout, why)
	return t.relayCheckout(ctx, o, box, mirror, ref, dir)
}

// relayCheckout relays a clone of the mirror through tower.
func (t *Target) relayCheckout(ctx context.Context, o system.Ops, box guestops.Ops, mirror, ref, dir string) (string, error) {
	pr, pw := io.Pipe()
	type result struct {
		commit string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		commit, err := o.Checkout(ctx, mirror, ref, pw)
		pw.CloseWithError(err)
		done <- result{commit, err}
	}()
	cerr := box.CopyIn(ctx, path.Clean("/"+dir), t.counted(pr))
	_ = pr.CloseWithError(errRelayEnded)
	res := <-done
	if res.err != nil && !errors.Is(res.err, errRelayEnded) {
		return "", driverError(res.err)
	}
	if cerr != nil {
		// The box's side failed, which ended the system container's.
		return "", guestError(cerr)
	}
	return res.commit, driverError(res.err)
}

// UpdateOrigin brings what brings a checkout's origin/* up to its mirror
// into the instance's checkout, over its network's endpoint or relayed.
func (t *Target) UpdateOrigin(ctx context.Context, in provider.Instance, mirror, dir string) error {
	o, err := t.sysOps()
	if err != nil {
		return err
	}
	p, box, err := t.running(ctx, in)
	if err != nil {
		return err
	}
	// Once the instance runs, which may take a VM's boot: no slot is held
	// meanwhile.
	end, err := t.mirrorSlot(ctx)
	if err != nil {
		return err
	}
	defer end()
	gitDir := path.Join(path.Clean("/"+dir), ".git")
	_, done, why, err := t.overEndpoint(ctx, o, p, box, system.GrantSpec{Op: system.GrantOrigin, Mirror: mirror}, gitDir)
	if done {
		return driverError(err)
	}
	t.relaying(p, system.GrantOrigin, why)
	pr, pw := io.Pipe()
	relayed := make(chan error, 1)
	go func() {
		err := o.Origin(ctx, mirror, pw)
		pw.CloseWithError(err)
		relayed <- err
	}()
	cerr := box.CopyIn(ctx, gitDir, t.counted(pr))
	_ = pr.CloseWithError(errRelayEnded)
	if err := <-relayed; err != nil && !errors.Is(err, errRelayEnded) {
		return driverError(err)
	}
	return guestError(cerr)
}

// Push takes the checkout's .git out of the instance into the system
// container, over its network's endpoint or relayed, which pushes it: the
// token never enters the instance.
func (t *Target) Push(ctx context.Context, req provider.PushRequest) error {
	o, err := t.sysOps()
	if err != nil {
		return err
	}
	p, box, err := t.running(ctx, req.Instance)
	if err != nil {
		return err
	}
	// Once the instance runs, which may take a VM's boot: no slot is held
	// meanwhile.
	end, err := t.mirrorSlot(ctx)
	if err != nil {
		return err
	}
	defer end()
	gitDir := path.Join(path.Clean("/"+req.Dir), ".git")
	g := system.GrantSpec{Op: system.GrantPush, Mirror: req.Mirror, URL: req.URL, Token: req.Token, Branch: req.Branch, Lease: req.Lease}
	_, done, why, err := t.overEndpoint(ctx, o, p, box, g, gitDir)
	if done {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s is no checkout: %w", req.Dir, provider.ErrFailed)
		}
		return driverError(err)
	}
	t.relaying(p, system.GrantPush, why)
	pr, pw := io.Pipe()
	copied := make(chan error, 1)
	go func() {
		err := box.CopyOut(ctx, gitDir, pw)
		pw.CloseWithError(err)
		copied <- err
	}()
	err = o.Push(ctx, driver.PushSpec{Mirror: req.Mirror, URL: req.URL, Token: req.Token, Branch: req.Branch, Lease: req.Lease}, t.counted(pr))
	_ = pr.CloseWithError(errRelayEnded)
	// The box's side failing ends the system container's call, whose own
	// error then says only that it was cut short.
	if cerr := <-copied; cerr != nil && !errors.Is(cerr, errRelayEnded) {
		if errors.Is(cerr, fs.ErrNotExist) {
			return fmt.Errorf("%s is no checkout: %w", req.Dir, provider.ErrFailed)
		}
		return guestError(cerr)
	}
	return driverError(err)
}

// BuildImage builds in the system container, a mirror's work tree there
// or a context tower sends, and returns the image as hangar pulls it: by
// RegistryHost.
func (t *Target) BuildImage(ctx context.Context, req provider.BuildRequest) (string, error) {
	o, err := t.sysOps()
	if err != nil {
		return "", err
	}
	spec := driver.BuildSpec{Repository: req.Repository, Context: req.Context, Dir: req.Dir, Dockerfile: req.Dockerfile}
	var mirror, ref string
	if req.Mirror != nil {
		mirror, ref = req.Mirror.Mirror, req.Mirror.Ref
	}
	image, err := o.Build(ctx, spec, mirror, ref, t.cfg.PushLogin)
	if err != nil {
		if errors.Is(err, driver.ErrNotFound) || errors.Is(err, driver.ErrNoSpace) || errors.Is(err, provider.ErrUnavailable) || errors.Is(err, context.Canceled) {
			return "", driverError(err)
		}
		return "", fmt.Errorf("building %s: %w: %w", req.Repository, err, provider.ErrFailed)
	}
	return t.asPulled(image), nil
}

// ResolveImage reads an image's config in the system container, from its
// registry: tower's by the name the container pushes to it by.
func (t *Target) ResolveImage(ctx context.Context, ref string) (provider.ImageConfig, error) {
	o, err := t.sysOps()
	if err != nil {
		return provider.ImageConfig{}, err
	}
	c, err := o.Resolve(ctx, t.asPushed(ref), t.cfg.PushLogin)
	return provider.ImageConfig{Digest: c.Digest, User: c.User, Env: c.Env}, driverError(err)
}

// asPulled is a reference to tower's registry by the name hangar pulls it
// by; asPushed, by the name the system container pushes to it by.
func (t *Target) asPulled(ref string) string {
	return renamed(ref, t.cfg.Registry, t.cfg.RegistryHost)
}

func (t *Target) asPushed(ref string) string {
	return renamed(ref, t.cfg.RegistryHost, t.cfg.Registry)
}

// renamed is ref with its registry from named to, when it names from.
func renamed(ref, from, to string) string {
	if from == "" || to == "" || from == to {
		return ref
	}
	if rest, ok := strings.CutPrefix(ref, from+"/"); ok {
		return to + "/" + rest
	}
	return ref
}
