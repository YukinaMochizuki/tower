package apiserver_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/fake"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// TestABoxComesUp: a box goes PENDING, BUILDING, STARTING, READY: its
// repository mirrored, its image built, its instance made on its target
// and, with no warm golden, its repository checked out from the mirror and
// its dependencies installed.
func TestABoxComesUp(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.fake.SetRemote("https://git.example.com/api.git", "main", map[string]string{"go.mod": "module api\n"}, "c1", "c2")
	// The target does not answer at first, so the box waits for its
	// repository's mirror.
	w.fake.SetUnavailable(true)
	w.mustApply(registry.Repository, repository("api"))
	phases := w.phases("dev")
	w.mustApply(registry.Box, box("dev", "api"))
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_PENDING)
	w.fake.SetUnavailable(false)
	w.advance(controllerMaxWait)
	b := w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	want := []envv1alpha1.BoxPhase{
		envv1alpha1.BoxPhase_BOX_PHASE_PENDING, envv1alpha1.BoxPhase_BOX_PHASE_BUILDING,
		envv1alpha1.BoxPhase_BOX_PHASE_STARTING, envv1alpha1.BoxPhase_BOX_PHASE_READY,
	}
	if got := phases(); !slices.Equal(got, want) {
		t.Errorf("phases %v, want %v", got, want)
	}
	st := b.GetStatus()
	if st.GetTarget() != "t1" || st.GetImageGolden() == "" || st.GetWarmGolden() != "" || st.GetInstance() == "" || st.GetAlias() != "dev" {
		t.Errorf("status %v", st)
	}
	in, ok := w.instance(st.GetInstance())
	if !ok {
		t.Fatal("the box's instance is not there")
	}
	img, _ := w.golden(st.GetImageGolden())
	if s := in.GetSpec(); s.GetTarget() != "t1" || s.GetKind() != envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER ||
		s.GetImage() != img.GetStatus().GetArtifact() || s.GetDiskGolden() != "" || s.GetNetwork() != "ws1" || s.GetAlias() != "dev" ||
		s.GetResources().GetMemory() != "4Gi" {
		t.Errorf("instance spec %v", s)
	}
	files := w.fake.Files(provider.Instance{Project: "web", Name: st.GetInstance()})
	if files["/workspace/go.mod"] != "module api\n" || files["/workspace/.git/HEAD"] != "c2" {
		t.Errorf("the checkout in the box: %v", files)
	}
	if !slices.ContainsFunc(w.fake.Execs(), func(e fake.Exec) bool { return e.Command[0] == "install-dependencies" }) {
		t.Errorf("the slow path installed no dependencies: %v", w.fake.Execs())
	}
	// A settled box stays quiet.
	w.waitSettled()
	rv := w.box("dev").GetMetadata().GetResourceVersion()
	w.waitSettled()
	if now := w.box("dev").GetMetadata().GetResourceVersion(); now != rv {
		t.Errorf("a settled box was written again: at %d, then at %d", rv, now)
	}
}

// TestBoxesOfTheSameInputsShareAnImage: two boxes of one repository and
// add-ons share one image golden, built once; other add-ons are another.
func TestBoxesOfTheSameInputsShareAnImage(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("one", "api"))
	w.mustApply(registry.Box, box("two", "api"))
	browser := box("three", "api")
	browser.Spec.Addons = []string{"browser"}
	w.mustApply(registry.Box, browser)
	one := w.waitBox("one", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	two := w.waitBox("two", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	three := w.waitBox("three", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	if one.GetStatus().GetImageGolden() != two.GetStatus().GetImageGolden() {
		t.Errorf("two boxes of the same inputs have images %q and %q", one.GetStatus().GetImageGolden(), two.GetStatus().GetImageGolden())
	}
	if three.GetStatus().GetImageGolden() == one.GetStatus().GetImageGolden() {
		t.Error("a box with another add-on shares the image")
	}
	if n := len(w.fake.Images()); n != 2 {
		t.Errorf("%d images built, want 2", n)
	}
}

// TestABoxWaitsForAPlacement: a box of a project with no Placement is
// UNPLACED, and comes up once one is made.
func TestABoxWaitsForAPlacement(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{unplaced: true})
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	w.mustApply(registry.Machine, machine("k3s"))
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_UNPLACED)
	w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_UNPLACED)
	w.waitSettled()
	if n := len(w.fake.Images()); n != 0 {
		t.Errorf("%d images built for an unplaced box", n)
	}
	w.mustApply(registry.Placement, placement("web", "t1"))
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
}

// TestABuildFailureFailsTheBox: an image that does not build fails its box
// with the reason.
func TestABuildFailureFailsTheBox(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.fake.FailBuilds(fmt.Errorf("step 3/7: npm ci exited 1: %w", provider.ErrFailed))
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	b := w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_FAILED)
	c := condition(b.GetStatus().GetConditions(), "Ready")
	if c.GetReason() != "ImageBuildFailed" || !strings.Contains(c.GetMessage(), "npm ci exited 1") {
		t.Errorf("Ready %v, want ImageBuildFailed saying why", c)
	}
	g, _ := w.golden(b.GetStatus().GetImageGolden())
	if g.GetStatus().GetPhase() != envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED {
		t.Errorf("the golden: %v", g.GetStatus())
	}
}

// readyBox makes repository api and a box of it, and waits until it is
// READY.
func (w *world) readyBox(name string, mutate ...func(*envv1alpha1.Box)) *envv1alpha1.Box {
	w.t.Helper()
	if _, ok := w.getRepo("api"); !ok {
		w.mustApply(registry.Repository, repository("api"))
	}
	b := box(name, "api")
	for _, m := range mutate {
		m(b)
	}
	w.mustApply(registry.Box, b)
	return w.waitBox(name, envv1alpha1.BoxPhase_BOX_PHASE_READY)
}

func (w *world) getRepo(name string) (*envv1alpha1.Repository, bool) {
	o, err := w.get(registry.Repository, "web", name)
	if err != nil {
		return nil, false
	}
	return o.(*envv1alpha1.Repository), true
}

// TestAnIdleBoxStopsAndWakes: a box unused for its target's
// idle_stop_after is stopped, its disk kept, and Wake brings the same box
// back, on the same instance.
func TestAnIdleBoxStopsAndWakes(t *testing.T) {
	t.Parallel()
	w := newWorld(t, idleAfter(time.Hour))
	before := w.readyBox("dev")
	w.advance(30 * time.Minute)
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Fatalf("a box used half an hour ago is %v", p)
	}
	w.advance(31 * time.Minute)
	stopped := w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
	if r := stopped.GetStatus().GetStoppedReason(); r != "Idle" {
		t.Errorf("stopped_reason %q, want Idle", r)
	}
	in, _ := w.instance(stopped.GetStatus().GetInstance())
	if in.GetSpec().GetRunState() != envv1alpha1.RunState_RUN_STATE_STOPPED || in.GetStatus().GetPhase() != envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED {
		t.Errorf("the idle box's instance: %v / %v", in.GetSpec().GetRunState(), in.GetStatus().GetPhase())
	}
	wctx, cancel := context.WithTimeout(t.Context(), testutil.Wait)
	defer cancel()
	if err := w.srv.Wake(wctx, "web", "dev"); err != nil {
		t.Fatal(err)
	}
	woken := w.box("dev")
	if woken.GetStatus().GetPhase() != envv1alpha1.BoxPhase_BOX_PHASE_READY || woken.GetStatus().GetStoppedReason() != "" {
		t.Errorf("after Wake: %v", woken.GetStatus())
	}
	after, _ := w.instance(woken.GetStatus().GetInstance())
	if woken.GetMetadata().GetUid() != before.GetMetadata().GetUid() || after.GetMetadata().GetUid() != in.GetMetadata().GetUid() {
		t.Error("Wake made another box or another instance")
	}
	// Woken, it is used now: it is not stopped again at once.
	w.advance(time.Minute)
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("a box just woken is %v", p)
	}
}

// TestUseKeepsABoxRunning: Touch moves a box's last use, flushed to its
// status at most once a minute: calls between flushes write nothing, and
// idleness is measured from the last use, also after tower restarts.
func TestUseKeepsABoxRunning(t *testing.T) {
	t.Parallel()
	w := newWorld(t, idleAfter(time.Hour))
	w.readyBox("dev")
	w.advance(40 * time.Minute)
	rev, err := w.store.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	used := w.clock.Now()
	for range 100 {
		w.srv.Touch("web", "dev")
	}
	if now, _ := w.store.Revision(t.Context()); now != rev {
		t.Errorf("Touch moved the store from revision %d to %d", rev, now)
	}
	w.advance(time.Minute)
	testutil.Eventually(t, "the use flushed", func() (bool, string) {
		got := w.box("dev").GetStatus().GetLastUseTime()
		return got != nil && got.AsTime().Equal(used), fmt.Sprintf("last_use_time %v, want %v", got, used)
	})
	// Restarted, tower measures idleness from the use it flushed.
	w.restart()
	w.advance(30 * time.Minute) // 70 minutes after it became ready, 31 after its use
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Fatalf("a box used 31 minutes ago is %v", p)
	}
	w.advance(30 * time.Minute)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
}

// TestKeepPreventsIdleStop: a box workstream keeps is never stopped for
// idleness; once let go, it is idle as any other. A box running a process
// is not either: TestARunningProcessKeepsABox.
func TestKeepPreventsIdleStop(t *testing.T) {
	t.Parallel()
	w := newWorld(t, idleAfter(time.Hour))
	w.readyBox("kept", func(b *envv1alpha1.Box) { b.Spec.Keep = true })
	w.readyBox("idle")
	w.advance(2 * time.Hour)
	w.waitBox("idle", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
	if p := w.box("kept").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("a kept box is %v after two idle hours", p)
	}
	kept := w.box("kept")
	kept.Spec.Keep = false
	w.mustApply(registry.Box, kept)
	w.advance(time.Minute)
	w.waitBox("kept", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
}

// TestARequestedStopStays: a box workstream stops is STOPPED for
// "Requested", and Wake does not start it.
func TestARequestedStopStays(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	b := w.readyBox("dev")
	b.Spec.RunState = envv1alpha1.RunState_RUN_STATE_STOPPED
	w.mustApply(registry.Box, b)
	stopped := w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
	if r := stopped.GetStatus().GetStoppedReason(); r != "Requested" {
		t.Errorf("stopped_reason %q, want Requested", r)
	}
	if err := w.srv.Wake(t.Context(), "web", "dev"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("waking a box stopped by request: %v, want failed precondition", err)
	}
	stopped.Spec.RunState = envv1alpha1.RunState_RUN_STATE_RUNNING
	w.mustApply(registry.Box, stopped)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
}

// TestAMachineComesUpAndIsNeverIdle: a machine's golden is converged once
// in a builder VM and captured, its instance cloned from it and configured
// with its own vars; it is never stopped for idleness.
func TestAMachineComesUpAndIsNeverIdle(t *testing.T) {
	t.Parallel()
	w := newWorld(t, idleAfter(time.Hour))
	m := machine("k3s")
	m.Spec.Blueprint.ExposePorts = []int32{6443}
	w.mustApply(registry.Machine, m)
	ready := w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	st := ready.GetStatus()
	if st.GetAddress() == "" || st.GetAlias() != "k3s" || st.GetTarget() != "t1" {
		t.Errorf("status %v", st)
	}
	g, _ := w.golden(st.GetGolden())
	in, _ := w.instance(st.GetInstance())
	if g.GetStatus().GetPhase() != envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY || in.GetSpec().GetDiskGolden() != g.GetMetadata().GetName() ||
		in.GetSpec().GetKind() != envv1alpha1.InstanceKind_INSTANCE_KIND_VM || in.GetSpec().GetImage() != "ubuntu-24.04" ||
		!slices.Equal(in.GetSpec().GetExposePorts(), []int32{6443}) {
		t.Errorf("golden %v, instance %v", g.GetStatus(), in.GetSpec())
	}
	// The golden phase is every task but those tagged configure, which the
	// machine runs alone, as workstream's untagged blueprints are written.
	var tags []string
	for _, e := range w.fake.Execs() {
		if e.Command[0] == "ansible-playbook" {
			tags = append(tags, e.Command[3])
		}
	}
	if !slices.Equal(tags, []string{"--skip-tags=configure", "--tags=configure"}) {
		t.Errorf("the blueprint's phases ran as %v", tags)
	}
	// The builder went once its golden was captured.
	if _, ok := w.instance("build-" + g.GetMetadata().GetName()); ok {
		t.Error("the builder instance is still there")
	}
	w.advance(3 * time.Hour)
	if p := w.machine("k3s").GetStatus().GetPhase(); p != envv1alpha1.MachinePhase_MACHINE_PHASE_READY {
		t.Errorf("a machine is %v after three idle hours", p)
	}
	// A second machine of the same blueprint shares the golden.
	w.mustApply(registry.Machine, machine("k3s-2"))
	if second := w.waitMachine("k3s-2", envv1alpha1.MachinePhase_MACHINE_PHASE_READY); second.GetStatus().GetGolden() != st.GetGolden() {
		t.Errorf("two machines of one blueprint have goldens %q and %q", st.GetGolden(), second.GetStatus().GetGolden())
	}
}

// TestAMachineRunsAsItsRunAsUser: a blueprint naming a run_as user has
// the user made, as root, in its golden's builder before its golden
// phase, and its golden phase, configure phase and ready check run as it,
// its secrets still reaching its configure phase, and the ready check in
// the user's home (a directory it may read, where its login would
// start); the user is part of what its golden is. One without run_as runs
// all of it as root.
func TestAMachineRunsAsItsRunAsUser(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Secret, secret("web", "k3s", map[string]string{"TOKEN": "t"}))
	const check = "test -x ~/.local/bin/tool"
	m := machine("dev")
	m.Spec.Blueprint.RunAs = &envv1alpha1.RunAs{User: "dev"}
	m.Spec.Blueprint.ReadyCheck = &envv1alpha1.ReadyCheck{Command: check}
	m.Spec.Secrets = []*envv1alpha1.SecretRef{{Secret: "k3s", Key: "TOKEN", As: "K3S_TOKEN"}}
	w.mustApply(registry.Machine, m)
	st := w.waitMachine("dev", envv1alpha1.MachinePhase_MACHINE_PHASE_READY).GetStatus()
	g, _ := w.golden(st.GetGolden())
	if got := g.GetSpec().GetMachine().GetRunAs(); got != "dev" {
		t.Errorf("the golden's inputs say run_as %q", got)
	}
	builder := "build-" + g.GetMetadata().GetName()

	var ran []string
	for _, e := range w.fake.Execs() {
		switch {
		case len(e.Command) > 4 && e.Command[0] == "sh" && e.Command[1] == "-c" && e.Command[3] == "sh":
			if e.Command[4] != "dev" || len(e.Env) > 0 {
				t.Errorf("the user was made as %v with %v", e.Command[3:], e.Env)
			}
			ran = append(ran, "prepare "+e.Instance.Name)
		case slices.Contains(e.Command, "ansible-playbook"):
			if !slices.Equal(e.Command[:4], []string{"runuser", "-u", "dev", "--"}) {
				t.Errorf("a phase ran as %v", e.Command)
			}
			phase := e.Command[7]
			if phase == "--tags=configure" && e.Env["K3S_TOKEN"] != "t" {
				t.Error("the configure phase, run as the user, was not given its secret")
			}
			ran = append(ran, phase+" "+e.Instance.Name)
		case slices.Contains(e.Command, check):
			if !slices.Equal(e.Command, []string{"runuser", "-u", "dev", "--", "sh", "-c", `cd "$HOME" || exit; exec sh -c "$1"`, "sh", check}) {
				t.Errorf("the ready check ran as %v", e.Command)
			}
			ran = append(ran, "ready "+e.Instance.Name)
		}
	}
	want := []string{
		"prepare " + builder, "--skip-tags=configure " + builder,
		"prepare " + st.GetInstance(), "--tags=configure " + st.GetInstance(),
		"ready " + st.GetInstance(),
	}
	if !slices.Equal(ran, want) {
		t.Errorf("ran %v, want %v", ran, want)
	}

	// The same blueprint as root is another golden, and runs as root.
	before := len(w.fake.Execs())
	root := machine("root")
	root.Spec.Blueprint.ReadyCheck = &envv1alpha1.ReadyCheck{Command: check}
	w.mustApply(registry.Machine, root)
	rst := w.waitMachine("root", envv1alpha1.MachinePhase_MACHINE_PHASE_READY).GetStatus()
	if rst.GetGolden() == st.GetGolden() {
		t.Error("a blueprint with and without run_as share a golden")
	}
	if rg, _ := w.golden(rst.GetGolden()); rg.GetSpec().GetMachine().GetRunAs() != "" {
		t.Errorf("a golden without run_as has inputs %v", rg.GetSpec().GetMachine())
	}
	var rootRan []string
	for _, e := range w.fake.Execs()[before:] {
		if e.Command[0] == "runuser" || (e.Command[0] == "sh" && len(e.Command) > 3) {
			t.Errorf("a machine without run_as ran %v", e.Command)
		}
		if e.Command[0] == "ansible-playbook" || slices.Equal(e.Command, []string{"sh", "-c", check}) {
			rootRan = append(rootRan, e.Command[len(e.Command)-1])
		}
	}
	if !slices.Equal(rootRan, []string{"blueprint.yml", "blueprint.yml", check}) {
		t.Errorf("a machine without run_as ran %v", rootRan)
	}
}

// readyCheck answers a machine's ready check in the fake: it passes once
// pass is set, and prints why not until then.
type readyCheck struct {
	command string
	pass    atomic.Bool
	tries   atomic.Int32
	env     atomic.Bool
}

func (r *readyCheck) on(f *fake.Fake) {
	f.OnExec(func(req provider.ExecRequest) (int, bool) {
		if !slices.Equal(req.Command, []string{"sh", "-c", r.command}) {
			return 0, false
		}
		r.tries.Add(1)
		if len(req.Env) > 0 {
			r.env.Store(true)
		}
		if r.pass.Load() {
			return 0, true
		}
		_, _ = io.WriteString(req.Stdout, "node k3s NotReady\n")
		return 1, true
	})
}

// waitMachineTicking waits for the machine to be in phase as the clock
// moves on by the ready check's interval at each look.
func (w *world) waitMachineTicking(name string, phase envv1alpha1.MachinePhase) *envv1alpha1.Machine {
	w.t.Helper()
	var m *envv1alpha1.Machine
	testutil.Eventually(w.t, fmt.Sprintf("machine %s %v", name, phase), func() (bool, string) {
		if m = w.machine(name); m.GetStatus().GetPhase() == phase {
			return true, ""
		}
		w.clock.Advance(5 * time.Second)
		return false, m.GetStatus().String()
	})
	return m
}

// TestAMachineIsReadyOnceItsReadyCheckPasses: a configured machine stays
// CONFIGURING while its blueprint's ready check fails, tried every 5
// seconds in it without its secrets, and is READY once it passes; after
// the machine starts again it is checked again.
func TestAMachineIsReadyOnceItsReadyCheckPasses(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	check := &readyCheck{command: "k3s kubectl get nodes 2>/dev/null | grep -q ' Ready '"}
	check.on(w.fake)
	w.mustApply(registry.Secret, secret("web", "k3s", map[string]string{"TOKEN": "t"}))
	m := machine("k3s")
	m.Spec.Blueprint.ReadyCheck = &envv1alpha1.ReadyCheck{Command: check.command}
	m.Spec.Secrets = []*envv1alpha1.SecretRef{{Secret: "k3s", Key: "TOKEN", As: "K3S_TOKEN"}}
	w.mustApply(registry.Machine, m)
	testutil.Eventually(t, "the ready check tried three times", func() (bool, string) {
		if check.tries.Load() >= 3 {
			return true, ""
		}
		w.clock.Advance(5 * time.Second)
		return false, w.machine("k3s").GetStatus().String()
	})
	st := w.machine("k3s").GetStatus()
	if st.GetPhase() != envv1alpha1.MachinePhase_MACHINE_PHASE_CONFIGURING ||
		condition(st.GetConditions(), apiserver.MachineConfigured).GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE ||
		condition(st.GetConditions(), apiserver.MachineReady).GetReason() != "WaitingForReady" {
		t.Fatalf("a configured machine whose ready check fails: %v", st)
	}
	check.pass.Store(true)
	w.waitMachineTicking("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	if check.env.Load() {
		t.Error("the ready check was given an environment")
	}

	// Started again, it is checked again.
	check.pass.Store(false)
	tries := check.tries.Load()
	stop := w.machine("k3s")
	stop.Spec.RunState = envv1alpha1.RunState_RUN_STATE_STOPPED
	w.mustApply(registry.Machine, stop)
	w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_STOPPED)
	w.advance(time.Minute)
	start := w.machine("k3s")
	start.Spec.RunState = envv1alpha1.RunState_RUN_STATE_RUNNING
	w.mustApply(registry.Machine, start)
	w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_CONFIGURING)
	testutil.Eventually(t, "the ready check tried again", func() (bool, string) {
		return check.tries.Load() > tries, ""
	})
	check.pass.Store(true)
	w.waitMachineTicking("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
}

// TestAMachineWhoseReadyCheckNeverPassesFails: a ready check that has not
// passed by its timeout fails the machine with the end of its output, as
// any instance step fails: it runs again once its backoff passes.
func TestAMachineWhoseReadyCheckNeverPassesFails(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	check := &readyCheck{command: "true-never"}
	check.on(w.fake)
	m := machine("k3s")
	m.Spec.Blueprint.ReadyCheck = &envv1alpha1.ReadyCheck{Command: check.command, Timeout: durationpb.New(30 * time.Second)}
	w.mustApply(registry.Machine, m)
	failed := w.waitMachineTicking("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_FAILED)
	msg := condition(failed.GetStatus().GetConditions(), apiserver.MachineReady).GetMessage()
	if !strings.Contains(msg, "did not pass in 30s") || !strings.Contains(msg, "node k3s NotReady") {
		t.Errorf("a machine whose ready check never passed failed with %q", msg)
	}
	if n := check.tries.Load(); n < 2 || n > 7 {
		t.Errorf("the ready check was tried %d times in 30 seconds, 5 seconds apart", n)
	}
	// It stays failed, and is not tried again, until its backoff passes.
	tries := check.tries.Load()
	w.advance(time.Minute)
	again := w.machine("k3s").GetStatus()
	if again.GetPhase() != envv1alpha1.MachinePhase_MACHINE_PHASE_FAILED || condition(again.GetConditions(), apiserver.MachineReady).GetMessage() != msg ||
		!strings.Contains(msg, "; tried again at ") || check.tries.Load() != tries {
		t.Errorf("a minute after its ready check failed, tried %d times more: %v", check.tries.Load()-tries, again)
	}
	check.pass.Store(true)
	w.advance(apiserver.StepRetryFirst)
	w.waitMachineTicking("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
}

// TestAMachinesSecretsReachOnlyItsCommands: a machine's secret values
// reach its configure phase as that command's environment, and no object,
// event, log line or database file.
func TestAMachinesSecretsReachOnlyItsCommands(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	const canary = "canary-k3s-Zq81"
	w.mustApply(registry.Secret, secret("web", "k3s", map[string]string{"TOKEN": canary}))
	m := machine("k3s")
	m.Spec.Secrets = []*envv1alpha1.SecretRef{{Secret: "k3s", Key: "TOKEN", As: "K3S_TOKEN"}}
	w.mustApply(registry.Machine, m)
	w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	given := false
	for _, e := range w.fake.Execs() {
		if e.Env["K3S_TOKEN"] == canary {
			given = slices.Contains(e.Command, "--tags=configure")
		}
	}
	if !given {
		t.Errorf("the configure phase was not given the secret: %v", w.fake.Execs())
	}
	w.waitSettled()
	w.nowhere(canary, &logs)
}

// nowhere fails the test if any object or event the store holds, any of
// its files, or logs holds value.
func (w *world) nowhere(value string, logs *syncBuffer) {
	t := w.t
	t.Helper()
	err := w.store.Watch(t.Context(), store.WatchOptions{SinceRevision: 0}, func(ev store.Event) error {
		if strings.Contains(string(ev.Object.Data), value) {
			t.Errorf("event %d of %s %s holds the secret", ev.Revision, ev.Object.Kind, ev.Object.Name)
		}
		if rev, _ := w.store.Revision(t.Context()); ev.Revision >= rev {
			return errDone
		}
		return nil
	})
	if !errors.Is(err, errDone) {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(w.dbDir, "*"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), value) {
			t.Errorf("%s holds the secret", filepath.Base(f))
		}
	}
	if strings.Contains(logs.String(), value) {
		t.Error("the log holds the secret")
	}
}

var errDone = errors.New("done")

// TestARestartMidBuildFinishesTheWork: tower restarted while a box's image
// builds finds the golden building, builds it again, and brings the box up.
func TestARestartMidBuildFinishesTheWork(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	release := w.fake.HoldBuilds()
	defer release()
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_BUILDING)
	w.restart()
	release()
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	if n := len(w.fake.Images()); n != 1 {
		t.Errorf("%d images built, want 1", n)
	}
}

// TestAVanishedInstanceIsRemade: an instance its target lost is reported
// on its box and made again; the box comes back READY.
func TestAVanishedInstanceIsRemade(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	b := w.readyBox("dev")
	before, _ := w.instance(b.GetStatus().GetInstance())
	stop := w.phases("dev")
	w.fake.Vanish(provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()})
	testutil.Eventually(t, "its instance made again", func() (bool, string) {
		in, ok := w.instance(b.GetStatus().GetInstance())
		return ok && in.GetMetadata().GetUid() != before.GetMetadata().GetUid(), fmt.Sprint(in)
	})
	after := w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	if got := stop(); !slices.Contains(got, envv1alpha1.BoxPhase_BOX_PHASE_STARTING) {
		t.Errorf("phases after the instance vanished: %v, want it STARTING again", got)
	}
	if after.GetMetadata().GetUid() != b.GetMetadata().GetUid() {
		t.Error("the box was made again rather than its instance")
	}
}

// TestAPlacementWhoseTargetGoesSaysSo: a Placement whose Target the system
// deleted stays, Ready=False.
func TestAPlacementWhoseTargetGoesSaysSo(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ready := func() *metav1.Condition {
		o, err := w.get(registry.Placement, "", "web")
		if err != nil {
			t.Fatal(err)
		}
		return condition(o.(*targetsv1alpha1.Placement).GetStatus().GetConditions(), "Ready")
	}
	testutil.Eventually(t, "the placement ready", func() (bool, string) {
		c := ready()
		return c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE, fmt.Sprint(c)
	})
	if _, err := w.asSystem().generic.Delete(t.Context(), registry.Target, client.Key{Name: "t1"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the placement not ready", func() (bool, string) {
		c := ready()
		return c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE && c.GetReason() == "TargetGone", fmt.Sprint(c)
	})
}

// warmOf is the names of the warm goldens held for each repository.
func (w *world) warmOf() map[string][]string {
	w.t.Helper()
	res, err := w.asSystem().generic.List(w.t.Context(), registry.Golden, client.ListOptions{Project: "web"})
	if err != nil {
		w.t.Fatal(err)
	}
	out := map[string][]string{}
	for _, o := range res.Items {
		g := o.(*envv1alpha1.Golden)
		if repo := g.GetSpec().GetBoxWarm().GetRepository(); repo != "" && g.GetMetadata().GetDeleteTime() == nil &&
			g.GetStatus().GetPhase() == envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY {
			out[repo] = append(out[repo], g.GetMetadata().GetName())
		}
	}
	return out
}

// TestWarmGoldensFollowUse: the repositories most used get warm goldens
// from the top down while their sizes fit the warm budget; one never used
// gets none, one that falls out loses its; a box of a warm repository is
// cloned from it, skipping the install.
func TestWarmGoldensFollowUse(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{settings: &targetsv1alpha1.TargetSettings{WarmBudget: "25Gi"}})
	w.fake.CaptureSize(func(provider.Instance) uint64 { return 10 << 30 })
	open := func(repo string, n int) {
		for i := range n {
			w.readyBox(fmt.Sprintf("%s-%d", repo, i), func(b *envv1alpha1.Box) { b.Spec.Checkouts[0].Repository = repo })
		}
	}
	for _, r := range []string{"a", "b", "c", "d"} {
		w.mustApply(registry.Repository, repository(r))
	}
	open("a", 3)
	open("b", 2)
	open("c", 1)
	// Five minutes of running counts, and every repository's use is counted
	// again.
	w.advance(5 * time.Minute)
	testutil.Eventually(t, "warm goldens for the two most used", func() (bool, string) {
		got := w.warmOf()
		return len(got) == 2 && len(got["a"]) == 1 && len(got["b"]) == 1, fmt.Sprint(got)
	})
	w.waitSettled()
	if got := w.warmOf(); len(got["c"]) != 0 || len(got["d"]) != 0 {
		t.Errorf("warm goldens %v: c does not fit, d was never used", got)
	}
	for repo, want := range map[string]bool{"a": true, "b": true, "c": false, "d": false} {
		r, _ := w.getRepo(repo)
		if r.GetStatus().GetUse().GetWarm() != want {
			t.Errorf("repository %s: use %v, want warm %t", repo, r.GetStatus().GetUse(), want)
		}
	}
	if a, _ := w.getRepo("a"); a.GetStatus().GetUse().GetBoxesOpened() != 3 || a.GetStatus().GetUse().GetBoxSeconds() == 0 {
		t.Errorf("repository a's use %v, want 3 boxes and their seconds", a.GetStatus().GetUse())
	}
	if n := len(w.fake.Artifacts()); n != 2 {
		t.Errorf("the target holds %d artifacts, want the two warm goldens': %v", n, w.fake.Artifacts())
	}

	// A box of a warm repository is cloned from its golden.
	installs := func() int {
		n := 0
		for _, e := range w.fake.Execs() {
			if e.Command[0] == "install-dependencies" {
				n++
			}
		}
		return n
	}
	before := installs()
	warmA := w.warmOf()["a"][0]
	cloned := w.readyBox("a-warm", func(b *envv1alpha1.Box) { b.Spec.Checkouts[0].Repository = "a" })
	if cloned.GetStatus().GetWarmGolden() != warmA {
		t.Errorf("a box of a warm repository: warm_golden %q, want %q", cloned.GetStatus().GetWarmGolden(), warmA)
	}
	if in, _ := w.instance(cloned.GetStatus().GetInstance()); in.GetSpec().GetDiskGolden() != warmA {
		t.Errorf("its instance's disk is cloned from %q, want %q", in.GetSpec().GetDiskGolden(), warmA)
	}
	if n := installs(); n != before {
		t.Errorf("a box cloned from a warm golden installed its dependencies again (%d, then %d)", before, n)
	}

	// d, used most now, does not take b's place within b's minimum hold;
	// once it has passed, it does, and b falls out.
	open("d", 9)
	w.advance(5 * time.Minute)
	w.advance(time.Minute)
	if got := w.warmOf(); len(got["b"]) != 1 || len(got["d"]) != 0 {
		t.Errorf("within b's minimum hold: warm goldens %v, want b's kept and none for d", got)
	}
	w.advance(apiserver.DefaultWarmMinHold)
	w.advance(time.Minute)
	testutil.Eventually(t, "d warm, b out", func() (bool, string) {
		got := w.warmOf()
		var uses []string
		for _, r := range []string{"a", "b", "c", "d"} {
			repo, _ := w.getRepo(r)
			uses = append(uses, r+":"+repo.GetStatus().GetUse().String())
		}
		return len(got["d"]) == 1 && len(got["b"]) == 0 && len(got["a"]) == 1, fmt.Sprint(got, uses)
	})
	w.waitSettled()
	if n := len(w.fake.Artifacts()); n != 2 {
		t.Errorf("the target holds %d artifacts, want two: b's went with its golden: %v", n, w.fake.Artifacts())
	}
}

// TestAnUnusedGoldenIsPruned: a golden nothing was made from for the prune
// delay and nothing references is deleted; one a box still uses is not.
func TestAnUnusedGoldenIsPruned(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	kept := w.readyBox("kept")
	keptGolden, _ := w.golden(kept.GetStatus().GetImageGolden())
	gone := w.readyBox("gone", func(b *envv1alpha1.Box) { b.Spec.Addons = []string{"desktop"} })
	if _, err := w.asSystem().generic.Delete(t.Context(), registry.Box, client.Key{Project: "web", Name: "gone"}); err != nil {
		t.Fatal(err)
	}
	w.advance(pruneAfter / 2)
	if _, ok := w.golden(gone.GetStatus().GetImageGolden()); !ok {
		t.Fatal("a golden was pruned before its delay")
	}
	w.advance(pruneAfter)
	testutil.Eventually(t, "the unused golden pruned", func() bool {
		_, ok := w.golden(gone.GetStatus().GetImageGolden())
		return !ok
	})
	w.waitSettled()
	// The same golden: a box whose golden went would make another of its
	// name.
	if g, ok := w.golden(kept.GetStatus().GetImageGolden()); !ok || g.GetMetadata().GetUid() != keptGolden.GetMetadata().GetUid() {
		t.Error("the golden a box uses was pruned")
	}
}

// TestTheTargetsStatusCountsWhatItHolds: tower writes its counts and usage
// into the target's status, beside what the provider reports there, and
// DiskPressure when free disk is under the warm budget and a margin.
func TestTheTargetsStatusCountsWhatItHolds(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{settings: &targetsv1alpha1.TargetSettings{WarmBudget: "100Gi"}})
	w.readyBox("dev")
	if err := w.fake.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	target := func() *targetsv1alpha1.TargetStatus {
		o, err := w.get(registry.Target, "", "t1")
		if err != nil {
			t.Fatal(err)
		}
		return o.(*targetsv1alpha1.Target).GetStatus()
	}
	testutil.Eventually(t, "the target's counts and the provider's report", func() (bool, string) {
		st := target()
		return st.GetCounts().GetInstances()["RUNNING"] == 1 && st.GetCounts().GetGoldens() == 1 &&
			st.GetCapacity().GetDiskTotal() != "" && len(st.GetFacts()) > 0 &&
			condition(st.GetConditions(), "Reporting").GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE &&
			condition(st.GetConditions(), "DiskPressure").GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE, st.String()
	})
	low := uint64(50 << 30)
	w.fake.SetDiskFree(&low)
	if err := w.fake.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "DiskPressure", func() (bool, string) {
		st := target()
		return condition(st.GetConditions(), "DiskPressure").GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE &&
			st.GetCounts().GetInstances()["RUNNING"] == 1, st.String()
	})
}

// TestAStaleWarmGoldenIsRebuilt: a warm golden more commits behind its
// branch than warm_max_commits_behind, or older than warm_max_age, is
// rebuilt, and the old one goes once the new one is ready.
func TestAStaleWarmGoldenIsRebuilt(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{settings: &targetsv1alpha1.TargetSettings{
		WarmMaxCommitsBehind: 2, WarmMaxAge: durationpb.New(48 * time.Hour), IdleStopAfter: durationpb.New(1000 * time.Hour),
	}})
	url := "https://git.example.com/api.git"
	w.fake.SetRemote(url, "main", nil, "c1")
	w.readyBox("dev")
	w.advance(5 * time.Minute)
	var first string
	testutil.Eventually(t, "a warm golden", func() (bool, string) {
		got := w.warmOf()["api"]
		if len(got) == 1 {
			first = got[0]
		}
		return len(got) == 1, fmt.Sprint(got)
	})
	g, _ := w.golden(first)
	if c := g.GetSpec().GetBoxWarm().GetCommit(); c != "c1" {
		t.Fatalf("the warm golden checks out %q, want c1", c)
	}
	// Two commits behind is within the bound; three is not.
	w.fake.SetRemote(url, "main", nil, "c1", "c2", "c3")
	w.advance(10 * time.Minute) // the mirror is fetched again
	w.advance(time.Minute)
	if got := w.warmOf()["api"]; len(got) != 1 || got[0] != first {
		t.Fatalf("a warm golden two commits behind was rebuilt: %v", got)
	}
	w.fake.SetRemote(url, "main", nil, "c1", "c2", "c3", "c4")
	w.advance(10 * time.Minute)
	var second string
	testutil.Eventually(t, "a warm golden of c4, the old one gone", func() (bool, string) {
		got := w.warmOf()["api"]
		if len(got) != 1 || got[0] == first {
			return false, fmt.Sprint(got)
		}
		g, _ := w.golden(got[0])
		second = got[0]
		return g.GetSpec().GetBoxWarm().GetCommit() == "c4", fmt.Sprint(got)
	})
	// Past its age, the same commit is built again.
	w.advance(49 * time.Hour)
	testutil.Eventually(t, "a warm golden rebuilt for its age", func() (bool, string) {
		got := w.warmOf()["api"]
		return len(got) == 1 && got[0] != second, fmt.Sprint(got)
	})
}

// TestANeverUsedRepositoryGetsNoWarmGolden: with room in the budget, a
// repository no box was opened with gets no warm golden.
func TestANeverUsedRepositoryGetsNoWarmGolden(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{settings: &targetsv1alpha1.TargetSettings{WarmBudget: "100Gi"}})
	w.mustApply(registry.Repository, repository("unused"))
	w.readyBox("dev")
	w.advance(5 * time.Minute)
	testutil.Eventually(t, "a warm golden for the used repository", func() (bool, string) {
		got := w.warmOf()
		return len(got["api"]) == 1, fmt.Sprint(got)
	})
	w.waitSettled()
	if got := w.warmOf(); len(got["unused"]) != 0 {
		t.Errorf("a repository never used has warm goldens %v", got["unused"])
	}
}
