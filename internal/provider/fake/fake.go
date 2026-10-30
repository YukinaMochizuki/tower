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
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
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
	// contexts are the images' build contexts.
	contexts map[string][]byte
	// users are the users built images run as; builds, every build.
	users map[string]string
	// envs are the environments of the images built, by reference.
	envs   map[string][]string
	builds []Build
	// imageConfigs and missingImages answer ResolveImage for images the
	// test named.
	imageConfigs  map[string]provider.ImageConfig
	missingImages map[string]bool
	mirrors       map[string]*repo
	remotes       map[string]*repo
	execs         []Exec
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
	// mirrorBuildErr fails builds from a mirror's work tree alone.
	mirrorBuildErr error
	// resolveErr fails every ResolveImage, as a registry that errors does.
	resolveErr error
	// holdBuilds, while set, holds every build until it is closed;
	// holdFetches every fetch.
	holdBuilds, holdFetches chan struct{}
	// fetches counts the fetches started.
	fetches     int
	captureErr  error
	checkoutErr error
	// ws, if set, makes each instance's /workspace a real directory
	// (RealWorkspaces).
	ws *workspaces
	// listErrs fail the listing of each instance's processes.
	listErrs     map[string]error
	checkouts    map[provider.Instance]int
	failCommands []string
	diskTotal    uint64
	diskFree     *uint64
	captureSize  func(provider.Instance) uint64
	onExec       func(provider.ExecRequest) (code int, handled bool)
	// The process limits: a log's cap and the finished processes kept.
	processLogCap     int64
	finishedProcesses int
	following         atomic.Int32

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
	// modes are the modes files were last written with at offset 0.
	modes map[string]fs.FileMode
	// base is the size of the golden its disk was cloned from, which
	// used_disk leaves out.
	base int
	// processes are its processes, oldest first.
	processes []*process
	processN  int
	// streams are the direct streams open to it (SetStreams).
	streams int
	// links are its symbolic links, by path, to what they name.
	links map[string]string
}

// resolve is the file a path of the instance names: what a link there
// names, or the path.
func (in *instance) resolve(p string) string {
	p = path.Clean(p)
	if t, ok := in.links[p]; ok {
		return t
	}
	return p
}

// linkCommand reads a command that links a path to a file, as tower
// links a box's secret file to its value in memory.
func linkCommand(cmd []string) (target, at string, ok bool) {
	if len(cmd) != 6 || cmd[0] != "sh" || cmd[1] != "-c" || !strings.Contains(cmd[2], "ln -sfn") {
		return "", "", false
	}
	return path.Clean(cmd[4]), path.Clean(cmd[5]), true
}

// Links are an instance's symbolic links, by path.
func (f *Fake) Links(ref provider.Instance) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if local := f.instances[ref]; local != nil {
		return maps.Clone(local.links)
	}
	return nil
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
	// AsRoot says it was asked to run as root.
	AsRoot bool
	// Process says it was started as a process (StartProcess), whose
	// exit ListProcesses tells, and MemoryMax what it was asked to be held
	// to.
	Process   bool
	MemoryMax int64
}

// New returns a fake provider of target, on clock.
func New(target string, clock clockwork.Clock) *Fake {
	return &Fake{
		Target: target, clock: clock, log: slog.New(slog.DiscardHandler),
		instances: map[provider.Instance]*instance{}, artifacts: map[string]*artifact{}, images: map[string]bool{}, contexts: map[string][]byte{},
		users: map[string]string{}, envs: map[string][]string{}, imageConfigs: map[string]provider.ImageConfig{}, missingImages: map[string]bool{},
		mirrors: map[string]*repo{}, remotes: map[string]*repo{}, seen: map[provider.Instance]bool{},
		bootFailures: map[string]string{}, vanished: map[string]bool{}, diskTotal: DefaultDiskTotal,
		processLogCap: provider.DefaultProcessLogCap, finishedProcesses: provider.DefaultFinishedProcesses,
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
		// What it holds in memory goes as it stops.
		for name := range local.files {
			if strings.HasPrefix(name, provider.SecretsDir+"/") {
				delete(local.files, name)
				delete(local.modes, name)
			}
		}
		// Stopping an instance ends its processes.
		for _, p := range local.processes {
			p.end(provider.Killed(provider.SignalKill))
		}
		f.pruneProcesses(local)
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
		st.ProcessesRunning = int32(local.processesRunning())
		st.StreamsOpen = int32(local.streams)
	} else {
		st.Address, st.UsedDisk, st.StartTime, st.ProcessesRunning, st.StreamsOpen = "", "", nil, 0, 0
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

// FailResolves makes every ResolveImage fail with err, as a registry that
// answers with an error does, or succeed again with nil.
func (f *Fake) FailResolves(err error) { f.mu.Lock(); f.resolveErr = err; f.mu.Unlock() }

// FailBuilds makes every build fail with err, or succeed again with nil.
func (f *Fake) FailBuilds(err error) { f.mu.Lock(); f.buildErr = err; f.mu.Unlock() }

// FailMirrorBuilds makes every build from a mirror's work tree (a
// devcontainer's Dockerfile) fail with err, or succeed again with nil, as
// a build step without DNS does; other builds are left as they are.
func (f *Fake) FailMirrorBuilds(err error) { f.mu.Lock(); f.mirrorBuildErr = err; f.mu.Unlock() }

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

// HoldFetches holds every fetch, started and counted, until release is
// called; a fetch whose caller gives up ends with its context.
func (f *Fake) HoldFetches() (release func()) {
	ch := make(chan struct{})
	f.mu.Lock()
	f.holdFetches = ch
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			f.holdFetches = nil
			f.mu.Unlock()
			close(ch)
		})
	}
}

// LoseMirrors drops every mirror, as a disk made again does: the
// repositories' status in tower keeps the heads they had.
func (f *Fake) LoseMirrors() { f.mu.Lock(); f.mirrors = map[string]*repo{}; f.mu.Unlock() }

// Fetches is how many fetches have started.
func (f *Fake) Fetches() int { f.mu.Lock(); defer f.mu.Unlock(); return f.fetches }

// FailCaptures makes every capture fail with err, or succeed again with
// nil.
func (f *Fake) FailCaptures(err error) { f.mu.Lock(); f.captureErr = err; f.mu.Unlock() }

// FailCheckouts makes every checkout fail with err, or succeed again with
// nil.
func (f *Fake) FailCheckouts(err error) { f.mu.Lock(); f.checkoutErr = err; f.mu.Unlock() }

// Checkouts is how many checkouts were asked of the instance, failed ones
// too.
func (f *Fake) Checkouts(in provider.Instance) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checkouts[in]
}

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
	// A link reads as the file it names.
	for at, target := range local.links {
		if b, ok := local.files[target]; ok {
			out[at] = string(b)
		}
	}
	return out
}

// Mode is the mode an instance's file was last written with, and whether
// it was written.
func (f *Fake) Mode(ref provider.Instance, p string) (fs.FileMode, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	local := f.instances[ref]
	if local == nil {
		return 0, false
	}
	m, ok := local.modes[local.resolve(p)]
	return m, ok
}

// BuildContext is the build context an image was built from, nil for one
// never built.
func (f *Fake) BuildContext(image string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.contexts[image])
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

func (f *Fake) Exec(ctx context.Context, req provider.ExecRequest) (int, error) {
	f.mu.Lock()
	local, err := f.running(req.Instance)
	if err != nil {
		f.mu.Unlock()
		return 0, err
	}
	f.execs = append(f.execs, Exec{Instance: req.Instance, Command: slices.Clone(req.Command), Env: maps.Clone(req.Env), ExitCode: -1, AsRoot: req.AsRoot})
	i := len(f.execs) - 1
	if onExec := f.onExec; onExec != nil && len(req.Command) > 0 && req.Command[0] == provider.GuestBinary {
		// A test's hook sees tower-guest's commands first.
		f.mu.Unlock()
		if code, ok := onExec(req); ok {
			f.mu.Lock()
			f.execs[i].ExitCode = code
			f.mu.Unlock()
			return code, nil
		}
		f.mu.Lock()
		if local, err = f.running(req.Instance); err != nil {
			f.mu.Unlock()
			return 0, err
		}
	}
	if answer, ok := plainGuest(f.ws, req.Command); ok {
		if i := slices.Index(req.Command, "--out"); req.Command[1] == "boost-pack" && i > 0 && i+1 < len(req.Command) {
			// What it packed, for Copy to copy.
			local.files[path.Join(path.Clean(req.Command[i+1]), "manifest.json")] = []byte(answer)
		}
		f.execs[i].ExitCode = 0
		f.mu.Unlock()
		if req.Stdout != nil {
			_, _ = io.WriteString(req.Stdout, answer)
		}
		return 0, nil
	}
	if args, ok := f.ws.guestArgs(req.Instance, req.Command); ok {
		guest := f.ws.guest
		f.mu.Unlock()
		stdout, stderr := req.Stdout, req.Stderr
		if stdout == nil {
			stdout = io.Discard
		}
		if stderr == nil {
			stderr = io.Discard
		}
		code := guest(args, stdout, stderr)
		f.mu.Lock()
		f.execs[i].ExitCode = code
		f.mu.Unlock()
		return code, nil
	}
	// What the command reads of the fake, taken now: it runs, writing to
	// its caller and reading its stdin, without holding f.mu.
	if target, at, ok := linkCommand(req.Command); ok {
		if local.links == nil {
			local.links = map[string]string{}
		}
		local.links[at] = target
		f.execs[i].ExitCode = 0
		f.mu.Unlock()
		return 0, nil
	}
	cmd := command{files: maps.Clone(local.files), failing: slices.Clone(f.failCommands), onExec: f.onExec}
	for at, target := range local.links {
		if b, ok := local.files[target]; ok {
			cmd.files[at] = b
		}
	}
	f.mu.Unlock()
	if len(req.Command) > 0 && req.Command[0] == "sleep" {
		// Runs until it is killed, as a command does when its call ends.
		<-ctx.Done()
		return 0, ctx.Err()
	}
	code := cmd.run(req)
	f.mu.Lock()
	f.execs[i].ExitCode = code
	f.mu.Unlock()
	return code, nil
}

// command is what a fake command reads of the fake: the instance's files
// as they were when it started, the commands that fail, and OnExec.
type command struct {
	files   map[string][]byte
	failing []string
	onExec  func(provider.ExecRequest) (int, bool)
}

// run runs a command of the fake's small set: true, false, echo, env,
// cat, exit, emit (each argument "out:text" or "err:text" writes a line of
// text to stdout or stderr, in order) and sleep (until it is killed); any
// other exits 0 unless FailCommands names it.
func (c command) run(req provider.ExecRequest) int {
	if c.onExec != nil {
		if code, ok := c.onExec(req); ok {
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
	if slices.Contains(c.failing, name) {
		return 1
	}
	switch name {
	case "false":
		return 1
	case "echo":
		_, _ = fmt.Fprintln(out, strings.Join(args, " "))
	case "emit":
		for _, a := range args {
			w := out
			if text, ok := strings.CutPrefix(a, "err:"); ok {
				if w = req.Stderr; w == nil {
					w = io.Discard
				}
				a = text
			} else {
				a = strings.TrimPrefix(a, "out:")
			}
			_, _ = fmt.Fprintln(w, a)
		}
	case "env":
		for _, k := range slices.Sorted(maps.Keys(req.Env)) {
			_, _ = fmt.Fprintf(out, "%s=%s\n", k, req.Env[k])
		}
	case "cat":
		for _, p := range args {
			b, ok := c.files[path.Clean(p)]
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
	if real := f.ws.path(in, p); err == nil && real != "" {
		f.mu.Unlock()
		b, err := os.ReadFile(real)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		_, err = w.Write(b)
		return err
	}
	if err == nil {
		var ok bool
		if b, ok = local.files[local.resolve(p)]; !ok {
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

func (f *Fake) WriteFile(_ context.Context, in provider.Instance, p string, mode fs.FileMode, offset int64, r io.Reader) error {
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
	p = local.resolve(p)
	if offset != 0 {
		if have := int64(len(local.files[p])); have != offset {
			return fmt.Errorf("%s holds %d bytes; a chunk at %d does not start where it ends: %w", p, have, offset, provider.ErrFailed)
		}
		b = append(slices.Clone(local.files[p]), b...)
	} else {
		if local.modes == nil {
			local.modes = map[string]fs.FileMode{}
		}
		local.modes[p] = mode
	}
	local.files[p] = b
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
	if from, to := f.ws.path(from, fromPath), f.ws.path(to, toPath); from != "" && to != "" {
		if _, err := os.Lstat(from); err != nil {
			return fmt.Errorf("%s: %w", fromPath, fs.ErrNotExist)
		}
		return copyPath(from, to)
	}
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

// Build is a build the fake was asked for, and the image it made.
type Build struct {
	Repository      string
	Mirror          *provider.MirrorSource
	Dir, Dockerfile string
	Image           string
}

// Builds are the builds asked for so far, in order.
func (f *Fake) Builds() []Build {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.builds)
}

// SetImage says what ResolveImage answers for ref.
func (f *Fake) SetImage(ref string, cfg provider.ImageConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.imageConfigs[ref] = cfg
}

// SetMissingImage makes ref one the registry lacks.
func (f *Fake) SetMissingImage(ref string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.missingImages[ref] = true
}

// inTree is a path of a work tree or a tar, "" its root; one leaving it is
// none.
func inTree(p string) (string, error) {
	c := path.Clean(p)
	switch {
	case c == ".":
		return "", nil
	case path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../"):
		return "", fmt.Errorf("%q leaves the work tree: %w", p, provider.ErrFailed)
	}
	return c, nil
}

// mirrorAt is a mirror at ref, as Checkout resolves it, and the commit.
// The caller holds f.mu.
func (f *Fake) mirrorAt(mirror, ref string) (*repo, string, error) {
	m := f.mirrors[mirror]
	if m == nil {
		return nil, "", fmt.Errorf("mirror %s: %w", mirror, provider.ErrNotFound)
	}
	if ref == "" || ref == m.branch {
		return m, m.head(), nil
	}
	if f.ws != nil && f.ws.fake[ref] != "" {
		// A real commit RealWorkspaces made of a fake one.
		ref = f.ws.fake[ref]
	}
	if !slices.Contains(m.commits, ref) {
		return nil, "", fmt.Errorf("ref %q in mirror %s: %w", ref, mirror, provider.ErrNotFound)
	}
	return m, ref, nil
}

// tarOf is files as a tar, in name order.
func tarOf(files map[string][]byte) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg})
		_, _ = tw.Write(files[name])
	}
	_ = tw.Close()
	return b.Bytes()
}

// filesOf reads a tar's regular files by their cleaned names.
func filesOf(b []byte) (map[string][]byte, error) {
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")] = data
	}
}

// BuildImage builds a context sent or a mirror's files, its Dockerfile
// one that starts with FROM, naming the image by the hash of the context
// and where in it the build is. The image runs as its last USER, else as
// what it is FROM.
func (f *Fake) BuildImage(ctx context.Context, req provider.BuildRequest) (string, error) {
	var b []byte
	if req.Context != nil {
		var err error
		if b, err = io.ReadAll(req.Context); err != nil {
			return "", err
		}
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
	// A build needs room to unpack its context before anything else.
	if f.free() < ImageSize {
		return "", provider.ErrNoSpace
	}
	if req.Mirror != nil && f.mirrorBuildErr != nil {
		return "", f.mirrorBuildErr
	}
	if m := req.Mirror; m != nil {
		repo, _, err := f.mirrorAt(m.Mirror, m.Ref)
		if err != nil {
			return "", err
		}
		b = tarOf(repo.files)
	}
	files, err := filesOf(b)
	if err != nil {
		return "", fmt.Errorf("the build context: %w: %w", err, provider.ErrFailed)
	}
	dir, err := inTree(req.Dir)
	if err != nil {
		return "", err
	}
	if dir != "" && !slices.ContainsFunc(slices.Collect(maps.Keys(files)), func(f string) bool { return strings.HasPrefix(f, dir+"/") }) {
		return "", fmt.Errorf("building %s: the context holds no directory %s: %w", req.Repository, dir, provider.ErrFailed)
	}
	name := req.Dockerfile
	if name == "" {
		name = path.Join(dir, "Dockerfile")
	}
	if name, err = inTree(name); err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(files[name])), "\n")
	from, ok := strings.CutPrefix(lines[0], "FROM ")
	if !ok {
		return "", fmt.Errorf("building %s: %s does not start with FROM: %w", req.Repository, name, provider.ErrFailed)
	}
	sum := sha256.Sum256(append(slices.Clone(b), []byte("\x00"+dir+"\x00"+name)...))
	image := "registry.tower.internal/" + req.Repository + "@sha256:" + hex.EncodeToString(sum[:])
	base := f.resolveLocked(strings.TrimSpace(from))
	user := base.User
	for _, l := range lines {
		if u, ok := strings.CutPrefix(strings.TrimSpace(l), "USER "); ok {
			user = strings.TrimSpace(u)
		}
	}
	f.images[image] = true
	f.contexts[image] = b
	f.users[image] = user
	f.envs[image] = dockerfileEnv(base.Env, lines)
	f.builds = append(f.builds, Build{Repository: req.Repository, Mirror: req.Mirror, Dir: req.Dir, Dockerfile: req.Dockerfile, Image: image})
	return image, nil
}

// dockerfileEnv is the environment of an image built from a Dockerfile of
// lines, FROM an image of env: each ENV, as NAME=value or NAME value, over
// it.
func dockerfileEnv(env []string, lines []string) []string {
	out := slices.Clone(env)
	set := func(name, value string) {
		out = slices.DeleteFunc(out, func(kv string) bool { return strings.HasPrefix(kv, name+"=") })
		out = append(out, name+"="+value)
	}
	for _, l := range lines {
		rest, ok := strings.CutPrefix(strings.TrimSpace(l), "ENV ")
		if !ok {
			continue
		}
		if name, value, ok := strings.Cut(strings.TrimSpace(rest), " "); ok && !strings.Contains(name, "=") {
			set(name, strings.TrimSpace(value))
			continue
		}
		for _, kv := range strings.Fields(rest) {
			if name, value, ok := strings.Cut(kv, "="); ok {
				set(name, value)
			}
		}
	}
	return out
}

// resolveLocked is what an image is: as the test set it, as built, or a
// digest of its name, running as root. The caller holds f.mu.
func (f *Fake) resolveLocked(ref string) provider.ImageConfig {
	if cfg, ok := f.imageConfigs[ref]; ok {
		return cfg
	}
	// A name by the digest its tag resolved to is that image.
	if name, digest, ok := strings.Cut(ref, "@"); ok {
		for tagged, cfg := range f.imageConfigs {
			if cfg.Digest == digest && strings.HasPrefix(name, tagged) {
				return cfg
			}
		}
	}
	if f.images[ref] {
		_, digest, _ := strings.Cut(ref, "@")
		return provider.ImageConfig{Digest: digest, User: f.users[ref], Env: f.envs[ref]}
	}
	if _, digest, ok := strings.Cut(ref, "@"); ok {
		return provider.ImageConfig{Digest: digest}
	}
	sum := sha256.Sum256([]byte(ref))
	return provider.ImageConfig{Digest: "sha256:" + hex.EncodeToString(sum[:])}
}

func (f *Fake) ResolveImage(_ context.Context, ref string) (provider.ImageConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.answering(); err != nil {
		return provider.ImageConfig{}, err
	}
	if f.resolveErr != nil {
		return provider.ImageConfig{}, f.resolveErr
	}
	if f.missingImages[ref] {
		return provider.ImageConfig{}, fmt.Errorf("image %s: %w", ref, provider.ErrNotFound)
	}
	return f.resolveLocked(ref), nil
}

// object is a fake git object id: a file's is the hash of its content, a
// directory's the hash of what is under it.
func object(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d:", len(p))
		_, _ = h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))[:40]
}

// ReadMirror reads a mirror's files, which every commit of it holds.
func (f *Fake) ReadMirror(_ context.Context, req provider.MirrorReadRequest) (provider.MirrorRead, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.answering(); err != nil {
		return provider.MirrorRead{}, err
	}
	m, commit, err := f.mirrorAt(req.Mirror, req.Ref)
	if err != nil {
		return provider.MirrorRead{}, err
	}
	out := provider.MirrorRead{Commit: commit, Entries: map[string]provider.MirrorEntry{}}
	for _, p := range req.Paths {
		clean, err := inTree(p)
		if err != nil {
			return provider.MirrorRead{}, err
		}
		if data, ok := m.files[clean]; ok && clean != "" {
			e := provider.MirrorEntry{Object: object(data), Size: int64(len(data))}
			if req.Content && len(data) <= provider.MaxMirrorContent {
				e.Content = slices.Clone(data)
			}
			out.Entries[p] = e
			continue
		}
		prefix := clean + "/"
		if clean == "" {
			prefix = ""
		}
		var parts [][]byte
		for _, name := range slices.Sorted(maps.Keys(m.files)) {
			if rest, ok := strings.CutPrefix(name, prefix); ok {
				parts = append(parts, []byte(rest), m.files[name])
			}
		}
		if len(parts) > 0 {
			out.Entries[p] = provider.MirrorEntry{Object: object(parts...), Dir: true}
		}
	}
	return out, nil
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

func (f *Fake) Fetch(ctx context.Context, req provider.FetchRequest) (string, error) {
	f.mu.Lock()
	f.fetches++
	hold := f.holdFetches
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
	if f.checkouts == nil {
		f.checkouts = map[provider.Instance]int{}
	}
	f.checkouts[in]++
	if f.checkoutErr != nil {
		return "", f.checkoutErr
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
	if f.ws.path(in, dir) != "" {
		return f.ws.checkout(in, mirror, m, commit, dir)
	}
	for name, b := range m.files {
		local.files[path.Join(dir, name)] = slices.Clone(b)
	}
	local.files[path.Join(dir, ".git/HEAD")] = []byte(commit)
	local.files[path.Join(dir, ".git/origin")] = []byte(mirror)
	local.files[path.Join(dir, OriginRef)] = []byte(m.head())
	return commit, nil
}

// OriginRef is the file of a fake checkout holding the commit its
// origin's branch is at.
const OriginRef = ".git/refs/remotes/origin/HEAD"

func (f *Fake) UpdateOrigin(_ context.Context, in provider.Instance, mirror, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	local, err := f.running(in)
	if err != nil {
		return err
	}
	m := f.mirrors[mirror]
	if m == nil {
		return fmt.Errorf("mirror %s: %w", mirror, provider.ErrNotFound)
	}
	dir = path.Clean(dir)
	if real := f.ws.path(in, dir); real != "" {
		if _, _, err := f.ws.realCommit(mirror, m, m.head()); err != nil {
			return err
		}
		_, err := gitIn(real, "fetch", "-q", "origin")
		return err
	}
	if string(local.files[path.Join(dir, ".git/origin")]) != mirror {
		return fmt.Errorf("%s is no checkout of mirror %s: %w", dir, mirror, provider.ErrFailed)
	}
	local.files[path.Join(dir, OriginRef)] = []byte(m.head())
	return nil
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
