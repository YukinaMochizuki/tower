package apiserver_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// autoStart is a start of a shell line, which the fake runs until it is
// ended, asking tower to route it.
func autoStart(box, line, dir string) *envv1alpha1.StartProcessRequest {
	return &envv1alpha1.StartProcessRequest{
		Environment: boxEnv(box), Command: shellLine(line), Dir: dir,
		Boost: &envv1alpha1.BoostIntent{Size: &envv1alpha1.BoostIntent_Auto{Auto: true}},
	}
}

// smallBox is a box of 1 vCPU and 2Gi, as boxes are heading to.
func smallBox(b *envv1alpha1.Box) {
	b.Spec.Resources = &envv1alpha1.Resources{Cpu: "1", Memory: "2Gi"}
}

// boxRef is the box's instance.
func (w *world) boxRef(box string) provider.Instance {
	return provider.Instance{Project: "web", Name: w.box(box).GetStatus().GetInstance()}
}

// writeBoxFile writes a file in the box's instance.
func (w *world) writeBoxFile(box, path, content string) {
	w.t.Helper()
	if err := w.fake.WriteFile(w.t.Context(), w.boxRef(box), path, 0o644, 0, bytes.NewReader([]byte(content))); err != nil {
		w.t.Fatal(err)
	}
}

// ranInBox runs line in the box without asking for a boost, ends it with
// usage and reads its exit, so its task's run is recorded.
func (w *world) ranInBox(box, line string, usage provider.ProcessUsage) {
	w.t.Helper()
	envs := w.as(workstream).envs
	id := startProcess(w.t, envs, &envv1alpha1.StartProcessRequest{Environment: boxEnv(box), Command: shellLine(line)})
	if err := w.fake.EndProcessWith(w.boxRef(box), id, provider.ProcessExit{Usage: &usage}); err != nil {
		w.t.Fatal(err)
	}
	if _, err := envs.ListProcesses(w.t.Context(), connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: boxEnv(box)})); err != nil {
		w.t.Fatal(err)
	}
}

func dur(d time.Duration) *time.Duration { return &d }

// boosted says a start is pending its boost, or started on it.
func boosted(res *envv1alpha1.StartProcessResponse) bool {
	return res.GetPending() != nil || res.GetRanOn().GetKind() == envv1alpha1.RanOnKind_RAN_ON_KIND_BOOST
}

func bytesOf(b int64) *int64 { return &b }

// TestAutoWithNoTaskRunsInTheBox: a line with no task the repository
// declares runs in the box, at once; its answer and its exit say why.
func TestAutoWithNoTaskRunsInTheBox(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev", smallBox)
	res := w.start(autoStart("dev", "git status && ./scripts/e2e.sh | tail -50", ""))
	if res.GetProcess() == "" || res.GetRanOn().GetKind() != envv1alpha1.RanOnKind_RAN_ON_KIND_BOX || !strings.Contains(res.GetRouted(), "no task found in the line") {
		t.Fatalf("a line with no task answered %v", res)
	}
	if _, ok := w.boost("dev"); ok {
		t.Error("a line with no task made a boost")
	}
	if err := w.fake.EndProcess(w.boxRef("dev"), res.GetProcess(), 0); err != nil {
		t.Fatal(err)
	}
	_, end, err := attachProcess(t.Context(), w.as(workstream).envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: res.GetProcess()})
	if err != nil {
		t.Fatal(err)
	}
	if end.GetExit().GetRouted() != res.GetRouted() {
		t.Errorf("its exit says it was routed %q, its start %q", end.GetExit().GetRouted(), res.GetRouted())
	}
	if p := w.listed("dev")[res.GetProcess()]; p.GetExit().GetRouted() != res.GetRouted() {
		t.Errorf("listed, its exit says %q", p.GetExit().GetRouted())
	}
}

// TestAutoSendsAHeavyToolToABoost: a heavy tool called directly, with no
// runs recorded, goes to a boost of the target's boost_resources; each
// answer to its start, and its exit, says why. Its run on the boost,
// light enough for the box, sends the next run to the box.
func TestAutoSendsAHeavyToolToABoost(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev", smallBox)
	req := autoStart("dev", "cd /workspace && go test ./... -run X 2>&1 | tail -50", "")
	first := w.start(req)
	const why = "go test ./...: no runs recorded yet; go test is a heavy tool: on a boost of 8 vCPU and 8192Mi"
	if first.GetPending().GetReason() != envv1alpha1.PendingReason_PENDING_REASON_BOOST_STARTING || first.GetRouted() != why {
		t.Fatalf("a heavy tool's first start answered %v, want pending BOOST_STARTING saying %q", first, why)
	}
	res := w.startBoosted(req)
	if res.GetRanOn().GetKind() != envv1alpha1.RanOnKind_RAN_ON_KIND_BOOST || res.GetRouted() != why {
		t.Fatalf("started, it answered %v", res)
	}
	if b, _ := w.boost("dev"); b.GetSpec().GetResources().GetCpu() != "8" || b.GetSpec().GetResources().GetMemory() != "8192Mi" {
		t.Errorf("its boost is of %v", b.GetSpec().GetResources())
	}
	b, _ := w.boost("dev")
	bref := provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}
	ps, _ := w.fake.ListProcesses(t.Context(), bref)
	if err := w.fake.EndProcessWith(bref, ps[len(ps)-1].ID, provider.ProcessExit{Usage: &provider.ProcessUsage{
		MemoryPeak: bytesOf(600 << 20), CPUUsage: dur(30 * time.Second), Wall: time.Minute,
	}}); err != nil {
		t.Fatal(err)
	}
	_, end, err := attachProcess(t.Context(), w.as(workstream).envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: res.GetProcess()})
	if err != nil {
		t.Fatal(err)
	}
	if end.GetExit().GetRouted() != why {
		t.Errorf("its exit says it was routed %q", end.GetExit().GetRouted())
	}

	next := w.start(req)
	if next.GetRanOn().GetKind() != envv1alpha1.RanOnKind_RAN_ON_KIND_BOX ||
		next.GetRouted() != "go test ./...: its last run, on a boost, used 0.5 vCPU on average and peaked at 600Mi, which this 1 vCPU box, holding its processes to 1.2Gi, holds: in the box" {
		t.Errorf("after a light run on a boost, it answered %v", next)
	}
	// Another pattern is another task, with no runs.
	w.writeBoxFile("dev", "/workspace/internal/x/x.go", "package x\n")
	if other := w.start(autoStart("dev", "go test ./internal/x/", "")); !strings.HasPrefix(other.GetRouted(), "go test ./internal/x/: no runs recorded yet") || !boosted(other) {
		t.Errorf("another package pattern answered %v", other)
	}
}

// execsRun counts the commands run in the fake by Exec, not as a
// process, that run make: routing runs nothing to judge a task (a boost
// being made runs its own setup).
func (w *world) execsRun() int {
	n := 0
	for _, x := range w.fake.Execs() {
		if !x.Process && slices.ContainsFunc(x.Command, func(a string) bool { return strings.Contains(a, "make") }) {
			n++
		}
	}
	return n
}

// TestAMakeTargetLearnsFromItsFirstRun: a make, just or task target with
// no runs runs in the box, nothing run to judge it; its first run's
// measure decides the next, in any box of the repository.
func TestAMakeTargetLearnsFromItsFirstRun(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev", smallBox)
	w.readyBox("other", smallBox)
	w.writeBoxFile("dev", "/workspace/Makefile", "test:\n\tgo test ./...\n")
	w.writeBoxFile("dev", "/workspace/svc/Makefile", "test:\n")
	execs := w.execsRun()
	for _, tc := range []struct{ line, why string }{
		{"make test 2>&1 | tee log", "make test: no runs recorded yet; its first run here is measured, and decides the next: in the box"},
		{"make -C svc test", "make test (in svc): no runs recorded yet; its first run here is measured"},
		{"just build", "just build: no runs recorded yet; its first run here is measured"},
		{"task lint", "task lint: no runs recorded yet; its first run here is measured"},
		{"make gen && go test ./...", "make gen && go test ./...: go test ./...: no runs recorded yet; go test is a heavy tool"},
	} {
		res := w.start(autoStart("dev", tc.line, ""))
		if boosted(res) != strings.Contains(tc.why, "heavy tool") || !strings.HasPrefix(res.GetRouted(), tc.why) {
			t.Errorf("%q answered %v, want it saying %q", tc.line, res, tc.why)
		}
	}
	if n := w.execsRun(); n != execs {
		t.Errorf("routing ran %d commands in the box", n-execs)
	}
	// Its first run, in one box, waited for a CPU: the next, in another,
	// goes to a boost.
	w.ranInBox("dev", "make test", provider.ProcessUsage{CPUUsage: dur(40 * time.Second), CPUWait: dur(24 * time.Second), Wall: time.Minute})
	if res := w.start(autoStart("other", "make test", "")); !boosted(res) ||
		res.GetRouted() != "make test: its last run in this 1 vCPU box waited for a CPU 40% of its 1m0s: on a boost of 8 vCPU and 8192Mi" {
		t.Errorf("after its first run, make test answered %v", res)
	}
}

// TestAutoFollowsScripts: a package.json script with no runs is judged
// by its text and the scripts it runs; a runner's shorthand is a script
// when package.json has it.
func TestAutoFollowsScripts(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev", smallBox)
	w.writeBoxFile("dev", "/workspace/package.json", `{"scripts":{
		"lint":"eslint . && prettier --check .",
		"check":"pnpm lint && pnpm run typecheck",
		"typecheck":"tsc --noEmit",
		"build":"vite build",
		"pretest":"tsc -b",
		"test":"node test.js",
		"loop":"pnpm loop"
	}}`)
	w.writeBoxFile("dev", "/workspace/web/package.json", `{"scripts":{"build":"node build.js","lint":"vitest run"}}`)
	for _, tc := range []struct {
		line  string
		boost bool
		why   string
	}{
		{"pnpm lint", false, "pnpm lint: no runs recorded yet, and its script runs no heavy tool: in the box"},
		{"pnpm check | tail -20", true, "pnpm check: no runs recorded yet; its script runs tsc, a heavy tool: on a boost of 8 vCPU and 8192Mi"},
		{"npm run build", true, "npm build: no runs recorded yet; its script runs vite build, a heavy tool"},
		{"npm test", true, "npm test: no runs recorded yet; its script runs tsc, a heavy tool"},
		{"pnpm test", false, "pnpm test: no runs recorded yet, and its script runs no heavy tool"},
		{"pnpm loop", false, "pnpm loop: no runs recorded yet, and its script runs no heavy tool"},
		{"pnpm run missing", false, "pnpm missing: no runs recorded yet, and its package.json has no such script: in the box"},
		// A script under cd is read from that directory's package.json.
		{"cd web && pnpm build", false, "pnpm build (in web): no runs recorded yet, and its script runs no heavy tool"},
		{"cd web && pnpm run lint", true, "pnpm lint (in web): no runs recorded yet; its script runs vitest, a heavy tool"},
		{"pnpm prettier --check .", false, "no task found"},
	} {
		res := w.start(autoStart("dev", tc.line, ""))
		if boosted(res) != tc.boost || !strings.HasPrefix(res.GetRouted(), tc.why) {
			t.Errorf("%q answered %v, want boosted %v saying %q", tc.line, res, tc.boost, tc.why)
		}
	}
}

// TestAutoRoutesByRecords: a task with runs is routed by its last ones,
// not by what it runs: a run in the box that waited for a CPU a fifth of
// its time, or kept all its CPUs busy, for 30 seconds or more, sends the
// next to a boost of boost_resources; a peak within 80% of the box's
// memory, or an OOM, to a boost of 1.5 times the peak; light runs to the
// box. The records are the repository's, read by each of its boxes.
func TestAutoRoutesByRecords(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev", smallBox)
	w.readyBox("other", smallBox)
	execs := w.execsRun()
	light := provider.ProcessUsage{MemoryPeak: bytesOf(500 << 20), CPUUsage: dur(10 * time.Second), CPUWait: dur(time.Second), Wall: time.Minute}
	w.ranInBox("dev", "make test", light)
	w.ranInBox("dev", "make test", provider.ProcessUsage{CPUUsage: dur(40 * time.Second), CPUWait: dur(24 * time.Second), Wall: time.Minute})
	w.ranInBox("dev", "make test", light)
	w.ranInBox("dev", "make busy", provider.ProcessUsage{CPUUsage: dur(57 * time.Second), CPUWait: dur(time.Second), Wall: time.Minute})
	w.ranInBox("dev", "make mem", provider.ProcessUsage{MemoryPeak: bytesOf(1100 << 20), CPUUsage: dur(time.Second), Wall: time.Minute})
	oom := true
	w.ranInBox("dev", "make oom", provider.ProcessUsage{MemoryPeak: bytesOf(1200 << 20), OOMKilled: &oom, Wall: time.Minute})
	w.ranInBox("dev", "make lint", light)
	// A short run that waited is no reason: 30 seconds or more.
	w.ranInBox("dev", "make short", provider.ProcessUsage{CPUUsage: dur(10 * time.Second), CPUWait: dur(10 * time.Second), Wall: 29 * time.Second})

	for _, tc := range []struct {
		line  string
		boost bool
		why   string
	}{
		{"make test", true, "make test: of its last 3 runs, one in this 1 vCPU box waited for a CPU 40% of its 1m0s: on a boost of 8 vCPU and 8192Mi"},
		{"make busy", true, "make busy: its last run kept all the CPUs of this 1 vCPU box busy for 1m0s: on a boost of 8 vCPU and 8192Mi"},
		// The box of 2Gi holds its processes to 1280Mi; a boost holding
		// them to 1.5 times a peak T has min(T+768Mi, 2T).
		{"make mem", true, "make mem: its last run in this box peaked at 1.1Gi, of the 1.2Gi this box holds its processes to: on a boost of 4 vCPU and 2418Mi"},
		{"make oom", true, "make oom: its last run in this box ran out of memory at 1.2Gi, of the 1.2Gi this box holds its processes to: on a boost of 5 vCPU and 2568Mi"},
		{"make lint", false, "make lint: its last run came near neither this box's CPUs nor its memory: in the box"},
		{"make short", false, "make short: its last run came near neither"},
	} {
		for _, box := range []string{"dev", "other"} {
			res := w.start(autoStart(box, tc.line, ""))
			if boosted(res) != tc.boost || !strings.HasPrefix(res.GetRouted(), tc.why) {
				t.Errorf("%q in %s answered %v, want boosted %v saying %q", tc.line, box, res, tc.boost, tc.why)
			}
			if tk := res.GetPending().GetTicket(); tk != "" {
				w.start(&envv1alpha1.StartProcessRequest{Environment: boxEnv(box), Boost: autoStart(box, "", "").GetBoost(), Pending: tk, Cancel: true, Command: shellLine("x")})
			}
		}
	}
	if n := w.execsRun(); n != execs {
		t.Errorf("routing ran %d commands in the box", n-execs)
	}

	// Three light runs since, the one that waited is past what is read.
	for range 3 {
		w.ranInBox("dev", "make test", light)
	}
	if res := w.start(autoStart("dev", "make test", "")); boosted(res) ||
		res.GetRouted() != "make test: its last 3 runs came near neither this box's CPUs nor its memory: in the box" {
		t.Errorf("after three light runs, make test answered %v", res)
	}
}

// TestAnExplicitBoostWins: a start that asks for no boost runs a heavy
// task in the box, one that asks for a boost runs a line with no task on
// it, and neither says it was routed.
func TestAnExplicitBoostWins(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev", smallBox)
	w.ranInBox("dev", "make test", provider.ProcessUsage{MemoryPeak: bytesOf(1900 << 20), Wall: time.Minute})
	res := w.start(&envv1alpha1.StartProcessRequest{Environment: boxEnv("dev"), Command: shellLine("make test")})
	if res.GetRanOn().GetKind() != envv1alpha1.RanOnKind_RAN_ON_KIND_BOX || res.GetRouted() != "" {
		t.Errorf("a heavy task asking for no boost answered %v", res)
	}
	on := w.startBoosted(boostOn("dev", "sh", "-c", "sleep; ls"))
	if on.GetRanOn().GetKind() != envv1alpha1.RanOnKind_RAN_ON_KIND_BOOST || on.GetRouted() != "" {
		t.Errorf("a line with no task asking for a boost answered %v", on)
	}
	sized := w.start(boostOf(boostOn("dev", "ls"), "2", "4Gi"))
	if sized.GetRouted() != "" {
		t.Errorf("a start asking for a size says it was routed: %v", sized)
	}
}

// TestAutoIsNeverRefusedForWantOfBoosts: on a hostd target a heavy task
// asking for auto runs in the box, saying why.
func TestAutoIsNeverRefusedForWantOfBoosts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("dev")
	res := w.start(autoStart("dev", "go test ./...", ""))
	if res.GetProcess() == "" || res.GetRouted() != "its box's target makes no boosts: it runs in the box" {
		t.Errorf("auto on a hostd target answered %v", res)
	}
}

// TestAutoAskedAgainKeepsItsRouting: a pending start asked again keeps
// the size and the why it was routed with; one whose ticket tower does
// not know asks for boost_resources, and says so.
func TestAutoAskedAgainKeepsItsRouting(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev", smallBox)
	oom := true
	w.ranInBox("dev", "make oom", provider.ProcessUsage{MemoryPeak: bytesOf(1200 << 20), OOMKilled: &oom, Wall: time.Minute})
	req := autoStart("dev", "make oom", "")
	first := w.start(req)
	if first.GetPending().GetTicket() == "" {
		t.Fatalf("the first start answered %v", first)
	}
	again := proto.Clone(req).(*envv1alpha1.StartProcessRequest)
	again.Pending = first.GetPending().GetTicket()
	// Each answer to it says why, pending ones too.
	var res *envv1alpha1.StartProcessResponse
	testutil.Eventually(t, "the routed start started", func() (bool, string) {
		res = w.start(again)
		if res.GetRouted() != first.GetRouted() {
			t.Fatalf("asked again, it answered %v; first %q", res, first.GetRouted())
		}
		return res.GetProcess() != "", res.GetPending().String()
	})
	if res.GetRouted() != first.GetRouted() || !strings.Contains(res.GetRouted(), "2568Mi") {
		t.Errorf("started, it says %q; first %q", res.GetRouted(), first.GetRouted())
	}
	if b, _ := w.boost("dev"); b.GetSpec().GetResources().GetMemory() != "2568Mi" {
		t.Errorf("its boost is of %v", b.GetSpec().GetResources())
	}
	// Asked again with its ticket once started: that start.
	if got := w.start(again); got.GetProcess() != res.GetProcess() || got.GetRouted() != res.GetRouted() {
		t.Errorf("its ticket asked again answered %v", got)
	}
	unknown := proto.Clone(req).(*envv1alpha1.StartProcessRequest)
	unknown.Pending = "t-unknown"
	got := w.start(unknown)
	if !strings.HasPrefix(got.GetRouted(), "routed to a boost when first asked; its ticket is not known since") {
		t.Errorf("an unknown ticket asked again answered %v", got)
	}
}

// planOf sets the target's report of what its Plan has left, and of its
// whole cpus (0: not told).
func (w *world) planOf(cpu, memory string, cpus int32) {
	w.t.Helper()
	o, err := w.get(registry.Target, "", "t1")
	if err != nil {
		w.t.Fatal(err)
	}
	st := proto.Clone(o.(*targetsv1alpha1.Target).GetStatus()).(*targetsv1alpha1.TargetStatus)
	st.Capacity = &targetsv1alpha1.Capacity{MemoryFree: memory, CpuFree: cpu, Cpus: cpus}
	if _, err := w.asSystem().generic.UpdateStatus(w.t.Context(), registry.Target, client.Key{Name: "t1"}, st, nil); err != nil {
		w.t.Fatal(err)
	}
}

// TestAutoThatCanNeverFitRunsInTheBox: auto is advisory. A start routed
// to a boost its target can never fit, even with no boost held, runs in
// the box, its answer and exit saying so; on, asked the same, is refused.
func TestAutoThatCanNeverFitRunsInTheBox(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev", smallBox)
	w.planOf("4", "48Gi", 0)
	res := w.start(autoStart("dev", "go test ./...", ""))
	const why = "go test ./... would go to a boost of 8 vCPU and 8192Mi, but the target can never fit it (its Plan has 4 vCPU"
	if res.GetProcess() == "" || res.GetRanOn().GetKind() != envv1alpha1.RanOnKind_RAN_ON_KIND_BOX ||
		!strings.HasPrefix(res.GetRouted(), why) || !strings.HasSuffix(res.GetRouted(), "): ran in the box") {
		t.Fatalf("auto routed to a boost that can never fit answered %v", res)
	}
	if _, ok := w.boost("dev"); ok {
		t.Error("a boost that can never fit was made")
	}
	if err := w.fake.EndProcess(w.boxRef("dev"), res.GetProcess(), 0); err != nil {
		t.Fatal(err)
	}
	_, end, err := attachProcess(t.Context(), w.as(workstream).envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: res.GetProcess()})
	if err != nil || end.GetExit().GetRouted() != res.GetRouted() {
		t.Errorf("its exit: %v, %v; want routed %q", end, err, res.GetRouted())
	}
	if _, err := w.as(workstream).envs.StartProcess(t.Context(), connect.NewRequest(boostOn("dev", "go", "test", "./..."))); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("on, of a boost that can never fit: %v, want FailedPrecondition", err)
	}
}

// TestAutoThatMustWaitStaysPending: a start routed to a boost that only
// has to wait, queued for the budget, answers pending as on does, saying
// why it was routed, so the caller decides.
func TestAutoThatMustWaitStaysPending(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{BoostBudget: &targetsv1alpha1.BoostSize{Cpu: "4", Memory: "4Gi"}})
	w.readyBox("a", smallBox)
	w.readyBox("b", smallBox, func(b *envv1alpha1.Box) { b.Spec.Workstream = "ws2" })
	w.startBoosted(boostOn("a", "true"))
	res := w.start(autoStart("b", "go test ./...", ""))
	if res.GetPending().GetReason() != envv1alpha1.PendingReason_PENDING_REASON_QUEUED_BUDGET ||
		res.GetRouted() != "go test ./...: no runs recorded yet; go test is a heavy tool: on a boost of 8 vCPU and 8192Mi" {
		t.Errorf("auto past the budget answered %v, want pending QUEUED_BUDGET saying why", res)
	}
}

// TestAutoThatComesToNeverFitRunsInTheBox: a queued start asked again
// once its boost can never fit runs in the box, saying so in its tasks'
// words as found when it was first asked.
func TestAutoThatComesToNeverFitRunsInTheBox(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{BoostBudget: &targetsv1alpha1.BoostSize{Cpu: "4", Memory: "4Gi"}})
	w.readyBox("a", smallBox)
	w.readyBox("b", smallBox, func(b *envv1alpha1.Box) { b.Spec.Workstream = "ws2" })
	w.startBoosted(boostOn("a", "true"))
	req := autoStart("b", "go test ./...", "")
	first := w.start(req)
	if first.GetPending().GetReason() != envv1alpha1.PendingReason_PENDING_REASON_QUEUED_BUDGET {
		t.Fatalf("auto past the budget answered %v", first)
	}
	// The Plan shrinks to 4 vCPU in all: a boost of 8 never fits.
	w.planOf("0", "48Gi", 4)
	req.Pending = first.GetPending().GetTicket()
	res := w.start(req)
	if res.GetRanOn().GetKind() != envv1alpha1.RanOnKind_RAN_ON_KIND_BOX ||
		!strings.HasPrefix(res.GetRouted(), "go test ./... would go to a boost of 8 vCPU and 8192Mi, but the target can never fit it") {
		t.Errorf("asked again once it can never fit, it answered %v", res)
	}
}

// TestAutoOnAMachineRunsThere: a machine has no boost; auto runs there,
// its answer saying so.
func TestAutoOnAMachineRunsThere(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Machine, machine("k3s"))
	w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	req := autoStart("", "go test ./...", "")
	req.Environment = machineEnv("k3s")
	if res := w.start(req); res.GetProcess() == "" || res.GetRouted() != "a machine has no boost: it runs on the machine" {
		t.Errorf("auto on a machine answered %v", res)
	}
}

// TestRoutedIsKeptForTheLastProcesses: why the box's processes ran in the
// box is kept for its last 100 alone.
func TestRoutedIsKeptForTheLastProcesses(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev", smallBox)
	var ids []string
	for range 101 {
		ids = append(ids, w.start(autoStart("dev", "ls", "")).GetProcess())
	}
	o, err := w.store.Get(t.Context(), store.Key{Kind: "BoxProcesses", Project: "web", Name: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	var rec struct{ Routed []struct{ ID string } }
	if err := json.Unmarshal(o.Data, &rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.Routed) != 100 || rec.Routed[0].ID != ids[1] || rec.Routed[99].ID != ids[100] {
		t.Errorf("kept the routed of %d processes, from %v to %v", len(rec.Routed), rec.Routed[0], rec.Routed[len(rec.Routed)-1])
	}
}

// TestAutoKeysNoDirectoryItCannotTrust: a directory the box does not
// have, or whose name is shaped as a token is though it has it, makes the
// line no task: it reaches no record, no log and no routed answer.
func TestAutoKeysNoDirectoryItCannotTrust(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	w := newWorld(t, worldOption{hangar: true, config: func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	w.readyBox("dev", smallBox)
	const token, missing = "ghp_a1B2c3D4e5F6g7H8i9J0", "hunter-correct-horse"
	w.writeBoxFile("dev", "/workspace/"+token+"/Makefile", "test:\n")
	for _, line := range []string{"cd " + token + " && go test ./...", "cd " + missing + " && go test ./...", "go test ./" + missing + "/...", "make -C " + missing + " test"} {
		res := w.start(autoStart("dev", line, ""))
		if res.GetRanOn().GetKind() != envv1alpha1.RanOnKind_RAN_ON_KIND_BOX || !strings.HasPrefix(res.GetRouted(), "no task found") {
			t.Errorf("%q answered %v", line, res)
		}
		if err := w.fake.EndProcessWith(w.boxRef("dev"), res.GetProcess(), provider.ProcessExit{Usage: &provider.ProcessUsage{Wall: time.Minute}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.as(workstream).envs.ListProcesses(t.Context(), connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: boxEnv("dev")})); err != nil {
		t.Fatal(err)
	}
	uses, all := w.taskUses()
	if len(uses) != 0 {
		t.Errorf("recorded %s", all)
	}
	for _, secret := range []string{token, missing} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the logs hold %q", secret)
		}
	}
}
