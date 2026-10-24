// Package fake is an in-process provider of one target, for tests. It
// realizes the target's Instances as hostd does, following them through
// the API with a ROLE_HOSTD client and reporting their status there, and
// does the imperative work of provider.Provider in memory: an instance is a
// map of files, an artifact a copy of one, a mirror a list of commits. Its
// faults are set by the test: a boot that fails, a full disk, an instance
// that vanishes, a target that stops answering, a build or a capture that
// fails, a command that fails.
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// Conditions an Instance's status carries, as hostd sets them.
const (
	// InstanceReady says the instance runs, or why it does not.
	InstanceReady = "Ready"
	// TargetReporting says the provider reported within its interval.
	TargetReporting = "Reporting"
)

// Defaults of a fake target's disk and of what it holds.
const (
	DefaultDiskTotal = 1 << 40
	// DefaultArtifactSize is what a capture holds unless CaptureSize says.
	DefaultArtifactSize = 1 << 30
	// ImageSize is what a built image holds on the target.
	ImageSize = 1 << 30
)

var _ provider.Provider = (*Fake)(nil)

// Fake is one target's provider. Make it with New and start realizing with
// Run.
type Fake struct {
	// Target is the Target it provides.
	Target string
	clock  clockwork.Clock
	log    *slog.Logger

	mu        sync.Mutex
	instances map[provider.Instance]*instance
	artifacts map[string]*artifact
	images    map[string]bool
	mirrors   map[string]*repo
	remotes   map[string]*repo
	execs     []Exec
	// seen are the instances of the target it has reconciled, realized or
	// not, which a fault cleared reconciles again.
	seen      map[provider.Instance]bool
	addresses int
	artifactN int
	// The faults.
	bootFailures map[string]string
	vanished     map[string]bool
	unavailable  bool
	buildErr     error
	// holdBuilds, while set, holds every build until it is closed.
	holdBuilds   chan struct{}
	captureErr   error
	failCommands []string
	diskTotal    uint64
	diskFree     *uint64
	captureSize  func(provider.Instance) uint64
	onExec       func(provider.ExecRequest) (code int, handled bool)

	// ctrl is set once Run starts.
	ctrl atomic.Pointer[controller.Controller]
	c    *client.Client
}

type instance struct {
	uid     string
	running bool
	started time.Time
	address string
	files   map[string][]byte
	// base is the size of the golden its disk was cloned from, which
	// used_disk leaves out.
	base int
}

type artifact struct {
	files map[string][]byte
	size  uint64
}

// repo is a remote repository or a mirror of it: its commits on its one
// branch, oldest first, and the files at its head.
type repo struct {
	branch  string
	commits []string
	files   map[string][]byte
}

func (r *repo) head() string {
	if len(r.commits) == 0 {
		return ""
	}
	return r.commits[len(r.commits)-1]
}

// Exec is one command a fake instance ran, with the environment it was
// given.
type Exec struct {
	Instance provider.Instance
	Command  []string
	Env      map[string]string
	ExitCode int
}

// New returns a fake provider of target, on clock.
func New(target string, clock clockwork.Clock) *Fake {
	return &Fake{
		Target: target, clock: clock, log: slog.New(slog.DiscardHandler),
		instances: map[provider.Instance]*instance{}, artifacts: map[string]*artifact{}, images: map[string]bool{},
		mirrors: map[string]*repo{}, remotes: map[string]*repo{}, seen: map[provider.Instance]bool{},
		bootFailures: map[string]string{}, vanished: map[string]bool{}, diskTotal: DefaultDiskTotal,
	}
}

// Run realizes the target's Instances until ctx is done, following them
// through the API with c, which acts as the target's ROLE_HOSTD token.
func (f *Fake) Run(ctx context.Context, c *client.Client) error {
	f.mu.Lock()
	f.c = c
	f.mu.Unlock()
	// Its retries and relists run on real time: a test moving the fake
	// clock by hours would otherwise leave it waiting for a second that
	// never comes.
	ctrl := &controller.Controller{
		Name: "fake-" + f.Target, Source: controller.APISource{Client: c}, Clock: clockwork.NewRealClock(), Log: f.log,
		Reconciler: controller.ReconcilerFunc(f.reconcile),
		Watches: []controller.Watch{
			{Kind: registry.Instance.Kind, Map: controller.Self},
			// A golden made ready lets an instance cloned from it boot.
			{Kind: registry.Golden.Kind, Map: f.instancesOf, SkipResync: true},
		},
	}
	f.ctrl.Store(ctrl)
	return ctrl.Run(ctx)
}

// Status is what the fake's controller is doing, for WaitSettled.
func (f *Fake) Status() []controller.Status {
	if c := f.ctrl.Load(); c != nil {
		return []controller.Status{c.Status()}
	}
	return nil
}

func (f *Fake) instancesOf(_ context.Context, o store.Object) []controller.Key {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keys []controller.Key
	for ref := range f.seen {
		if ref.Project == o.Project {
			keys = append(keys, controller.Key{Project: ref.Project, Name: ref.Name})
		}
	}
	return keys
}

// enqueue reconciles an instance again, as a host event would.
func (f *Fake) enqueue(ref provider.Instance) {
	if c := f.ctrl.Load(); c != nil {
		c.Enqueue(controller.Key{Project: ref.Project, Name: ref.Name})
	}
}

func (f *Fake) reconcile(ctx context.Context, key controller.Key) error {
	ref := provider.Instance{Project: key.Project, Name: key.Name}
	f.mu.Lock()
	down, c := f.unavailable, f.c
	f.mu.Unlock()
	if down {
		// A target that does not answer reports nothing.
		return nil
	}
	o, err := c.Get(ctx, registry.Instance, client.Key{Project: key.Project, Name: key.Name})
	if connect.CodeOf(err) == connect.CodeNotFound || connect.CodeOf(err) == connect.CodePermissionDenied {
		f.mu.Lock()
		delete(f.instances, ref)
		delete(f.seen, ref)
		f.mu.Unlock()
		return nil
	}
	if err != nil {
		return err
	}
	in := o.(*envv1alpha1.Instance)
	f.mu.Lock()
	f.seen[ref] = true
	f.mu.Unlock()
	var base *artifact
	if g := in.GetSpec().GetDiskGolden(); g != "" {
		gobj, err := c.Get(ctx, registry.Golden, client.Key{Project: key.Project, Name: g})
		if err != nil && connect.CodeOf(err) != connect.CodeNotFound {
			return err
		}
		if gd, ok := gobj.(*envv1alpha1.Golden); ok && gd.GetStatus().GetPhase() == envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY {
			f.mu.Lock()
			base = f.artifacts[gd.GetStatus().GetArtifact()]
			f.mu.Unlock()
		}
		if base == nil {
			return f.report(ctx, c, in, envv1alpha1.InstancePhase_INSTANCE_PHASE_PENDING, "WaitingForGolden", "its golden is not ready on the target")
		}
	}
	f.mu.Lock()
	phase, reason, msg := f.realize(in, base)
	f.mu.Unlock()
	return f.report(ctx, c, in, phase, reason, msg)
}

// realize brings the instance in memory in line with its spec, and says
// what it came to. The caller holds f.mu.
func (f *Fake) realize(in *envv1alpha1.Instance, base *artifact) (envv1alpha1.InstancePhase, string, string) {
	ref := provider.Instance{Project: in.GetMetadata().GetProject(), Name: in.GetMetadata().GetName()}
	uid := in.GetMetadata().GetUid()
	if f.vanished[uid] {
		delete(f.instances, ref)
		return envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE, "Gone", "the instance is not on the target any more"
	}
	if why, ok := f.bootFailures[ref.Name]; ok {
		return envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED, "BootFailed", why
	}
	local := f.instances[ref]
	if local == nil || local.uid != uid {
		if f.free() < DefaultArtifactSize/1024 {
			return envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED, "NoSpace", "no space left on the target"
		}
		f.addresses++
		local = &instance{uid: uid, address: fmt.Sprintf("10.0.%d.%d", f.addresses/250, f.addresses%250+2), files: map[string][]byte{}}
		if base != nil {
			local.files = maps.Clone(base.files)
			local.base = filesSize(base.files)
		}
		f.instances[ref] = local
	}
	if in.GetSpec().GetRunState() == envv1alpha1.RunState_RUN_STATE_STOPPED {
		local.running = false
		return envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED, "Stopped", ""
	}
	if !local.running {
		local.running, local.started = true, f.clock.Now()
	}
	return envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, "Running", ""
}

// report writes the instance's status as hostd would, when it changed.
func (f *Fake) report(ctx context.Context, c *client.Client, in *envv1alpha1.Instance, phase envv1alpha1.InstancePhase, reason, msg string) error {
	ref := provider.Instance{Project: in.GetMetadata().GetProject(), Name: in.GetMetadata().GetName()}
	st := proto.Clone(in.GetStatus()).(*envv1alpha1.InstanceStatus)
	if st == nil {
		st = &envv1alpha1.InstanceStatus{}
	}
	st.ObservedGeneration = in.GetMetadata().GetGeneration()
	st.Phase = phase
	f.mu.Lock()
	if local := f.instances[ref]; local != nil && phase != envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE {
		st.Address = local.address
		st.UsedDisk = quantity.FormatDisk(uint64(max(0, filesSize(local.files)-local.base)))
		if local.running {
			st.StartTime = timestamppb.New(local.started)
		} else {
			st.StartTime = nil
		}
	} else {
		st.Address, st.UsedDisk, st.StartTime = "", "", nil
	}
	f.mu.Unlock()
	ready := &metav1.Condition{Type: InstanceReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg, ObservedGeneration: st.ObservedGeneration}
	if phase == envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING {
		ready.Status = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	}
	st.Conditions = conditions.Set(st.GetConditions(), ready, f.clock.Now())
	if proto.Equal(st, in.GetStatus()) {
		return nil
	}
	rv := in.GetMetadata().GetResourceVersion()
	_, err := c.UpdateStatus(ctx, registry.Instance, client.Key{Project: ref.Project, Name: ref.Name}, st, &rv)
	return err
}

// Report writes what the fake says about its target into the Target's
// status: its capacity, its facts and Reporting, leaving what tower counts
// there as it is. A target that does not answer reports nothing.
func (f *Fake) Report(ctx context.Context) error {
	f.mu.Lock()
	down, c := f.unavailable, f.c
	total, free := f.diskTotal, f.free()
	f.mu.Unlock()
	if down || c == nil {
		return nil
	}
	for range 10 {
		o, err := c.Get(ctx, registry.Target, client.Key{Name: f.Target})
		if err != nil {
			return err
		}
		t := o.(*targetsv1alpha1.Target)
		st := proto.Clone(t.GetStatus()).(*targetsv1alpha1.TargetStatus)
		if st == nil {
			st = &targetsv1alpha1.TargetStatus{}
		}
		st.Capacity = &targetsv1alpha1.Capacity{
			DiskTotal: quantity.FormatDisk(total), DiskFree: quantity.FormatDisk(free),
			MemoryTotal: "64Gi", MemoryFree: "48Gi", Cpus: 16,
		}
		st.Facts = []*targetsv1alpha1.Fact{{Name: "provider", Value: "fake"}, {Name: "zfs", Value: "ONLINE"}}
		st.LastReportTime = timestamppb.New(f.clock.Now())
		st.Conditions = conditions.Set(st.GetConditions(), &metav1.Condition{
			Type: TargetReporting, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Reported",
		}, f.clock.Now())
		rv := t.GetMetadata().GetResourceVersion()
		_, err = c.UpdateStatus(ctx, registry.Target, client.Key{Name: f.Target}, st, &rv)
		if connect.CodeOf(err) == connect.CodeAborted {
			continue
		}
		return err
	}
	return errors.New("fake: too many concurrent writers of the target's status")
}

// The faults.

// FailBoot makes the instance of the name fail to boot, saying why, until
// cleared with an empty why.
func (f *Fake) FailBoot(name, why string) {
	f.mu.Lock()
	if why == "" {
		delete(f.bootFailures, name)
	} else {
		f.bootFailures[name] = why
	}
	refs := slices.Collect(maps.Keys(f.seen))
	f.mu.Unlock()
	for _, ref := range refs {
		if ref.Name == name {
			f.enqueue(ref)
		}
	}
}

// Vanish makes the instance disappear from the target, as a host that lost
// it would; a later instance of the same name is made afresh.
func (f *Fake) Vanish(ref provider.Instance) {
	f.mu.Lock()
	if local := f.instances[ref]; local != nil {
		f.vanished[local.uid] = true
	}
	f.mu.Unlock()
	f.enqueue(ref)
}

// SetUnavailable makes the target stop answering, or answer again.
func (f *Fake) SetUnavailable(down bool) {
	f.mu.Lock()
	f.unavailable = down
	refs := slices.Collect(maps.Keys(f.seen))
	f.mu.Unlock()
	if !down {
		for _, ref := range refs {
			f.enqueue(ref)
		}
	}
}

// FailBuilds makes every build fail with err, or succeed again with nil.
func (f *Fake) FailBuilds(err error) { f.mu.Lock(); f.buildErr = err; f.mu.Unlock() }

// HoldBuilds holds every build, as a slow one, until release is called;
// a build whose caller gives up ends with its context.
func (f *Fake) HoldBuilds() (release func()) {
	ch := make(chan struct{})
	f.mu.Lock()
	f.holdBuilds = ch
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.holdBuilds = nil
			f.mu.Unlock()
			close(ch)
		})
	}
}

// FailCaptures makes every capture fail with err, or succeed again with
// nil.
func (f *Fake) FailCaptures(err error) { f.mu.Lock(); f.captureErr = err; f.mu.Unlock() }

// FailCommands makes every command whose program is one of names exit 1.
func (f *Fake) FailCommands(names ...string) { f.mu.Lock(); f.failCommands = names; f.mu.Unlock() }

// SetDiskFree fixes what the target has free, as a full disk does; nil
// counts it from what it holds again.
func (f *Fake) SetDiskFree(free *uint64) { f.mu.Lock(); f.diskFree = free; f.mu.Unlock() }

// CaptureSize sets the size of what a capture of an instance holds.
func (f *Fake) CaptureSize(size func(provider.Instance) uint64) {
	f.mu.Lock()
	f.captureSize = size
	f.mu.Unlock()
}

// OnExec, when set, runs before the fake's own commands: a command it
// handles exits with the code it returns.
func (f *Fake) OnExec(fn func(provider.ExecRequest) (code int, handled bool)) {
	f.mu.Lock()
	f.onExec = fn
	f.mu.Unlock()
}

// SetRemote makes a repository at url with commits on branch, oldest
// first, holding files at its head.
func (f *Fake) SetRemote(url, branch string, files map[string]string, commits ...string) {
	r := &repo{branch: branch, commits: commits, files: map[string][]byte{}}
	for name, content := range files {
		r.files[name] = []byte(content)
	}
	f.mu.Lock()
	f.remotes[url] = r
	f.mu.Unlock()
}

// What the test reads back.

// Execs are the commands run so far, in order.
func (f *Fake) Execs() []Exec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.execs)
}

// Files are an instance's files, or nil if it is not on the target.
func (f *Fake) Files(ref provider.Instance) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	local := f.instances[ref]
	if local == nil {
		return nil
	}
	out := map[string]string{}
	for name, b := range local.files {
		out[name] = string(b)
	}
	return out
}

// Artifacts are the names of the artifacts the target holds, sorted.
func (f *Fake) Artifacts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.artifacts))
}

// Images are the images built, sorted.
func (f *Fake) Images() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.images))
}

// RemoteHead is the commit a remote's branch is at.
func (f *Fake) RemoteHead(url string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r := f.remotes[url]; r != nil {
		return r.head()
	}
	return ""
}

// The imperative half.

func (f *Fake) answering() error {
	if f.unavailable {
		return provider.ErrUnavailable
	}
	return nil
}

// running returns the instance, which must be on the target and running.
// The caller holds f.mu.
func (f *Fake) running(ref provider.Instance) (*instance, error) {
	if err := f.answering(); err != nil {
		return nil, err
	}
	local := f.instances[ref]
	if local == nil || f.vanished[local.uid] {
		return nil, fmt.Errorf("instance %s/%s: %w", ref.Project, ref.Name, provider.ErrNotFound)
	}
	if !local.running {
		return nil, fmt.Errorf("instance %s/%s is not running: %w", ref.Project, ref.Name, provider.ErrFailed)
	}
	return local, nil
}

func (f *Fake) Exec(_ context.Context, req provider.ExecRequest) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	local, err := f.running(req.Instance)
	if err != nil {
		return 0, err
	}
	code := f.run(local, req)
	f.execs = append(f.execs, Exec{Instance: req.Instance, Command: slices.Clone(req.Command), Env: maps.Clone(req.Env), ExitCode: code})
	return code, nil
}

// run runs a command of the fake's small set: true, false, echo, env, cat
// and exit; any other exits 0 unless FailCommands names it. The caller
// holds f.mu.
func (f *Fake) run(local *instance, req provider.ExecRequest) int {
	if f.onExec != nil {
		if code, ok := f.onExec(req); ok {
			return code
		}
	}
	if len(req.Command) == 0 {
		return 127
	}
	out := req.Stdout
	if out == nil {
		out = io.Discard
	}
	name, args := req.Command[0], req.Command[1:]
	if slices.Contains(f.failCommands, name) {
		return 1
	}
	switch name {
	case "false":
		return 1
	case "echo":
		_, _ = fmt.Fprintln(out, strings.Join(args, " "))
	case "env":
		for _, k := range slices.Sorted(maps.Keys(req.Env)) {
			_, _ = fmt.Fprintf(out, "%s=%s\n", k, req.Env[k])
		}
	case "cat":
		for _, p := range args {
			b, ok := local.files[path.Clean(p)]
			if !ok {
				return 1
			}
			_, _ = out.Write(b)
		}
	case "exit":
		if len(args) == 1 {
			n, _ := strconv.Atoi(args[0])
			return n
		}
	}
	return 0
}

func (f *Fake) ReadFile(_ context.Context, in provider.Instance, p string, w io.Writer) error {
	f.mu.Lock()
	local, err := f.running(in)
	var b []byte
	if err == nil {
		var ok bool
		if b, ok = local.files[path.Clean(p)]; !ok {
			err = fmt.Errorf("%s: %w", p, fs.ErrNotExist)
		}
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func (f *Fake) WriteFile(_ context.Context, in provider.Instance, p string, _ fs.FileMode, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	local, err := f.running(in)
	if err != nil {
		return err
	}
	local.files[path.Clean(p)] = b
	return nil
}

func (f *Fake) Copy(_ context.Context, from provider.Instance, fromPath string, to provider.Instance, toPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	src, err := f.running(from)
	if err != nil {
		return err
	}
	dst, err := f.running(to)
	if err != nil {
		return err
	}
	fromPath, toPath = path.Clean(fromPath), path.Clean(toPath)
	found := false
	for name, b := range src.files {
		if rel, ok := under(name, fromPath); ok {
			dst.files[path.Join(toPath, rel)] = slices.Clone(b)
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%s: %w", fromPath, fs.ErrNotExist)
	}
	return nil
}

// under reports whether name is root or a path below it, and the rest.
func under(name, root string) (string, bool) {
	if name == root {
		return "", true
	}
	rest, ok := strings.CutPrefix(name, strings.TrimSuffix(root, "/")+"/")
	return rest, ok
}

func (f *Fake) BuildImage(ctx context.Context, req provider.BuildRequest) (string, error) {
	b, err := io.ReadAll(req.Context)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	hold := f.holdBuilds
	f.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.answering(); err != nil {
		return "", err
	}
	if f.buildErr != nil {
		return "", f.buildErr
	}
	sum := sha256.Sum256(b)
	image := "registry.tower.internal/" + req.Repository + "@sha256:" + hex.EncodeToString(sum[:])
	if !f.images[image] && f.free() < ImageSize {
		return "", provider.ErrNoSpace
	}
	f.images[image] = true
	return image, nil
}

func (f *Fake) Capture(_ context.Context, in provider.Instance) (provider.Artifact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.answering(); err != nil {
		return provider.Artifact{}, err
	}
	local := f.instances[in]
	if local == nil || f.vanished[local.uid] {
		return provider.Artifact{}, fmt.Errorf("instance %s/%s: %w", in.Project, in.Name, provider.ErrNotFound)
	}
	if local.running {
		return provider.Artifact{}, fmt.Errorf("instance %s/%s is running; capture takes a stopped one: %w", in.Project, in.Name, provider.ErrFailed)
	}
	if f.captureErr != nil {
		return provider.Artifact{}, f.captureErr
	}
	size := uint64(DefaultArtifactSize)
	if f.captureSize != nil {
		size = f.captureSize(in)
	}
	if f.free() < size {
		return provider.Artifact{}, provider.ErrNoSpace
	}
	f.artifactN++
	name := fmt.Sprintf("%s/snap-%d", f.Target, f.artifactN)
	f.artifacts[name] = &artifact{files: maps.Clone(local.files), size: size}
	return provider.Artifact{Name: name, Size: size}, nil
}

func (f *Fake) DeleteArtifact(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.answering(); err != nil {
		return err
	}
	delete(f.artifacts, name)
	return nil
}

func (f *Fake) Fetch(_ context.Context, req provider.FetchRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.answering(); err != nil {
		return "", err
	}
	r := f.remotes[req.URL]
	if r == nil {
		// A repository the test said nothing of has one commit.
		sum := sha256.Sum256([]byte(req.URL))
		r = &repo{branch: req.Branch, commits: []string{hex.EncodeToString(sum[:])[:12]}, files: map[string][]byte{"README": []byte(req.URL)}}
		f.remotes[req.URL] = r
	}
	if req.Branch != "" && req.Branch != r.branch {
		return "", fmt.Errorf("branch %q of %s: %w", req.Branch, req.URL, provider.ErrNotFound)
	}
	f.mirrors[req.Mirror] = &repo{branch: r.branch, commits: slices.Clone(r.commits), files: maps.Clone(r.files)}
	return r.head(), nil
}

func (f *Fake) Checkout(_ context.Context, in provider.Instance, mirror, ref, dir string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	local, err := f.running(in)
	if err != nil {
		return "", err
	}
	m := f.mirrors[mirror]
	if m == nil {
		return "", fmt.Errorf("mirror %s: %w", mirror, provider.ErrNotFound)
	}
	commit := m.head()
	if ref != "" && ref != m.branch {
		if !slices.Contains(m.commits, ref) {
			return "", fmt.Errorf("ref %q in mirror %s: %w", ref, mirror, provider.ErrNotFound)
		}
		commit = ref
	}
	dir = path.Clean(dir)
	for name, b := range m.files {
		local.files[path.Join(dir, name)] = slices.Clone(b)
	}
	local.files[path.Join(dir, ".git/HEAD")] = []byte(commit)
	local.files[path.Join(dir, ".git/origin")] = []byte(mirror)
	return commit, nil
}

func (f *Fake) CommitsBehind(_ context.Context, mirror, commit, branch string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.answering(); err != nil {
		return 0, err
	}
	m := f.mirrors[mirror]
	if m == nil || (branch != "" && branch != m.branch) {
		return 0, fmt.Errorf("mirror %s: %w", mirror, provider.ErrNotFound)
	}
	i := slices.Index(m.commits, commit)
	if i < 0 {
		return len(m.commits), nil
	}
	return len(m.commits) - 1 - i, nil
}

func (f *Fake) Push(_ context.Context, req provider.PushRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	local, err := f.running(req.Instance)
	if err != nil {
		return err
	}
	commit, ok := local.files[path.Join(path.Clean(req.Dir), ".git/HEAD")]
	if !ok {
		return fmt.Errorf("%s is no checkout: %w", req.Dir, provider.ErrFailed)
	}
	r := f.remotes[req.URL]
	if r == nil {
		return fmt.Errorf("repository %s: %w", req.URL, provider.ErrNotFound)
	}
	at := ""
	if req.Branch == r.branch {
		at = r.head()
	}
	if at != req.Lease {
		return fmt.Errorf("branch %s is at %q, not %q: %w", req.Branch, at, req.Lease, provider.ErrLeaseLost)
	}
	if req.Branch == r.branch {
		r.commits = append(r.commits, string(commit))
	}
	return nil
}

func (f *Fake) Ticket(_ context.Context, in provider.Instance, port int32) (provider.Ticket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.running(in); err != nil {
		return provider.Ticket{}, err
	}
	return provider.Ticket{
		URL:     fmt.Sprintf("https://%s.fake.tower.internal/%s/%s/%d", f.Target, in.Project, in.Name, port),
		Expires: f.clock.Now().Add(time.Minute),
	}, nil
}

// free is what the target has free. The caller holds f.mu.
func (f *Fake) free() uint64 {
	if f.diskFree != nil {
		return *f.diskFree
	}
	used := uint64(len(f.images)) * ImageSize
	for _, a := range f.artifacts {
		used += a.size
	}
	if used > f.diskTotal {
		return 0
	}
	return f.diskTotal - used
}

func filesSize(files map[string][]byte) int {
	n := 0
	for _, b := range files {
		n += len(b)
	}
	return n
}
