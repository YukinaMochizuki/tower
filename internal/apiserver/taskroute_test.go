package apiserver_test

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/provider"
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
		next.GetRouted() != "go test ./...: its last run, on a boost, used 0.5 vCPU on average and peaked at 600Mi, which this box (1 vCPU, 2.0Gi) holds: in the box" {
		t.Errorf("after a light run on a boost, it answered %v", next)
	}
	// Another pattern is another task, with no runs.
	if other := w.start(autoStart("dev", "go test ./internal/x/", "")); !strings.HasPrefix(other.GetRouted(), "go test ./internal/x/: no runs recorded yet") || !boosted(other) {
		t.Errorf("another package pattern answered %v", other)
	}
}

// fakeDryRun answers make -n in the fake as a makefile would, by target:
// what each prints, how it exits, and how many dry runs there were.
type fakeDryRun struct {
	mu   sync.Mutex
	runs []provider.ExecRequest
}

func (d *fakeDryRun) install(w *world) {
	w.fake.OnExec(func(req provider.ExecRequest) (int, bool) {
		i := slices.Index(req.Command, "-n")
		if i < 1 || req.Command[i-1] != "make" {
			return 0, false
		}
		d.mu.Lock()
		d.runs = append(d.runs, req)
		d.mu.Unlock()
		targets := strings.Join(req.Command[i+1:], " ")
		out := map[string]string{
			"test":      "go test ./...",
			"lint":      "golangci-lint run ./...\necho done",
			"check":     "make -C sub lint\ncd web && pnpm build",
			"":          "go build -o bin/x ./cmd/x",
			"lint test": "golangci-lint run\ngo test ./...",
			"web":       "cd web && pnpm lint",
			"slow":      "",
			"bad":       "",
			"gen":       "buf generate",
		}[targets]
		_, _ = fmt.Fprintln(req.Stdout, out)
		switch targets {
		case "slow":
			return 124, true
		case "bad":
			return 2, true
		}
		return 0, true
	})
}

func (d *fakeDryRun) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.runs)
}

// TestAutoReadsAMakeTargetsDryRun: a make target with no runs is judged
// by make -n in the box, as its user, in its directory: one that runs a
// heavy tool goes to a boost, any other to the box. The judgement is kept
// by the makefile's hash, so the dry run runs again only once it
// changes; a dry run that fails or overruns leaves it to the box.
func TestAutoReadsAMakeTargetsDryRun(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev", smallBox)
	dry := &fakeDryRun{}
	dry.install(w)
	w.writeBoxFile("dev", "/workspace/Makefile", "test:\n\tgo test ./...\n")
	w.writeBoxFile("dev", "/workspace/web/package.json", `{"scripts":{"build":"vite build","lint":"eslint ."}}`)

	for _, tc := range []struct {
		line, dir string
		boost     bool
		why       string
	}{
		{"make test 2>&1 | tee log", "", true, "make test: no runs recorded yet; what it runs (make -n) holds go test, a heavy tool: on a boost of 8 vCPU and 8192Mi"},
		{"make", "/workspace", true, "make: no runs recorded yet; what it runs (make -n) holds go build"},
		{"make lint", "", false, "make lint: no runs recorded yet, and what it runs (make -n) holds no heavy tool: in the box"},
		{"make check", "", true, "make check: no runs recorded yet; what it runs (make -n) holds vite build"},
		{"make web", "", false, "make web: no runs recorded yet, and what it runs (make -n) holds no heavy tool"},
		{"make slow", "", false, "make slow: no runs recorded yet, and what it runs is not known: its dry run (make -n) ran past 10s: in the box"},
		{"make bad", "", false, "make bad: no runs recorded yet, and what it runs is not known: its dry run (make -n) failed, exit 2: in the box"},
		{"make -C nowhere test", "", false, "make test (in nowhere): no runs recorded yet, and what it runs is not known: it has no makefile: in the box"},
		{"make gen && make test", "", true, "make gen && make test: make test: no runs recorded yet; what it runs (make -n) holds go test"},
	} {
		res := w.start(autoStart("dev", tc.line, tc.dir))
		if boosted(res) != tc.boost || !strings.HasPrefix(res.GetRouted(), tc.why) {
			t.Errorf("%q answered %v, want boosted %v saying %q", tc.line, res, tc.boost, tc.why)
		}
		if tk := res.GetPending().GetTicket(); tk != "" {
			w.start(&envv1alpha1.StartProcessRequest{Environment: boxEnv("dev"), Boost: autoStart("dev", "", "").GetBoost(), Pending: tk, Cancel: true, Command: shellLine("x")})
		}
	}
	dry.mu.Lock()
	runs := slices.Clone(dry.runs)
	dry.mu.Unlock()
	for _, r := range runs {
		if r.AsRoot || r.Dir != "/workspace" || r.Command[0] != "timeout" {
			t.Errorf("a dry run ran as %v", r)
		}
	}
	// Kept by the makefile's hash: not run again while it is the same.
	n := dry.count()
	if res := w.start(autoStart("dev", "make test", "")); !boosted(res) || !strings.HasPrefix(res.GetRouted(), "make test: no runs recorded yet; what it runs (make -n) holds go test") || dry.count() != n {
		t.Errorf("asked again, make test answered %v after %d dry runs, want %d", res, dry.count(), n)
	}
	// A failed one is not kept.
	w.start(autoStart("dev", "make bad", ""))
	if dry.count() != n+1 {
		t.Errorf("a failed dry run was kept: %d dry runs, want %d", dry.count(), n+1)
	}
	w.writeBoxFile("dev", "/workspace/Makefile", "test:\n\tgo test -race ./...\n")
	w.start(autoStart("dev", "make test", ""))
	if dry.count() != n+2 {
		t.Errorf("after its makefile changed, %d dry runs, want %d", dry.count(), n+2)
	}
	uses, all := w.taskUses()
	if x := uses["make test"].Expansion; x == nil || x.Heavy != "go test" || x.Source != "make -n" || len(x.File) != 64 {
		t.Errorf("make test's record holds %s", all)
	}
	if strings.Contains(all, "./...") || strings.Contains(all, "golangci") {
		t.Errorf("a record holds an expansion's text: %s", all)
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
	for _, tc := range []struct {
		line  string
		boost bool
		why   string
	}{
		{"pnpm lint", false, "pnpm lint: no runs recorded yet, and what it runs (package.json) holds no heavy tool: in the box"},
		{"pnpm check | tail -20", true, "pnpm check: no runs recorded yet; what it runs (package.json) holds tsc, a heavy tool"},
		{"npm run build", true, "npm build: no runs recorded yet; what it runs (package.json) holds vite build"},
		{"npm test", true, "npm test: no runs recorded yet; what it runs (package.json) holds tsc"},
		{"pnpm test", false, "pnpm test: no runs recorded yet, and what it runs (package.json) holds no heavy tool"},
		{"pnpm loop", false, "pnpm loop: no runs recorded yet, and what it runs (package.json) holds no heavy tool"},
		{"pnpm run missing", false, "pnpm missing: no runs recorded yet, and what it runs is not known: its package.json has no such script: in the box"},
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
	dry := &fakeDryRun{}
	dry.install(w)
	w.writeBoxFile("dev", "/workspace/Makefile", "x:\n")
	light := provider.ProcessUsage{MemoryPeak: bytesOf(500 << 20), CPUUsage: dur(10 * time.Second), CPUWait: dur(time.Second), Wall: time.Minute}
	w.ranInBox("dev", "make test", light)
	w.ranInBox("dev", "make test", provider.ProcessUsage{CPUUsage: dur(40 * time.Second), CPUWait: dur(24 * time.Second), Wall: time.Minute})
	w.ranInBox("dev", "make test", light)
	w.ranInBox("dev", "make busy", provider.ProcessUsage{CPUUsage: dur(57 * time.Second), CPUWait: dur(time.Second), Wall: time.Minute})
	w.ranInBox("dev", "make mem", provider.ProcessUsage{MemoryPeak: bytesOf(1800 << 20), CPUUsage: dur(time.Second), Wall: time.Minute})
	oom := true
	w.ranInBox("dev", "make oom", provider.ProcessUsage{MemoryPeak: bytesOf(1300 << 20), OOMKilled: &oom, Wall: time.Minute})
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
		{"make mem", true, "make mem: its last run in this box peaked at 1.8Gi of this box's 2.0Gi: on a boost of 5 vCPU and 2700Mi"},
		{"make oom", true, "make oom: its last run in this box ran out of memory at 1.3Gi of this box's 2.0Gi: on a boost of 3 vCPU and 1950Mi"},
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
	if dry.count() != 0 {
		t.Errorf("tasks with runs were judged by %d dry runs too", dry.count())
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
	w.ranInBox("dev", "make oom", provider.ProcessUsage{MemoryPeak: bytesOf(1300 << 20), OOMKilled: &oom, Wall: time.Minute})
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
	if res.GetRouted() != first.GetRouted() || !strings.Contains(res.GetRouted(), "1950Mi") {
		t.Errorf("started, it says %q; first %q", res.GetRouted(), first.GetRouted())
	}
	if b, _ := w.boost("dev"); b.GetSpec().GetResources().GetMemory() != "1950Mi" {
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
