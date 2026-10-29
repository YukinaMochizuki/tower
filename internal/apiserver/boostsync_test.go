package apiserver_test

import (
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// syncWorld is a hangar world whose instances' /workspace are real git
// checkouts, tower-guest's boost subcommands run on them in-process, and
// box dev ready, its repository holding a.txt, b.txt and a .gitignore.
type syncWorld struct {
	*world
	box, boost provider.Instance
}

func newSyncWorld(t *testing.T) *syncWorld {
	t.Helper()
	w := boostWorld(t, nil)
	w.fake.RealWorkspaces(t.TempDir(), guestbin.Boost)
	w.fake.SetRemote("https://git.example.com/api.git", "main", map[string]string{"a.txt": "a\n", "b.txt": "b\n", ".gitignore": "*.log\n"}, "c1")
	w.readyBox("dev")
	return &syncWorld{world: w, box: provider.Instance{Project: "web", Name: "box-dev"}, boost: provider.Instance{Project: "web", Name: "boost-dev"}}
}

// file is the real path of p under an instance's /workspace.
func (w *syncWorld) file(in provider.Instance, p string) string {
	return w.fake.WorkspaceDir(in, "/workspace/"+p)
}

func (w *syncWorld) write(in provider.Instance, p, text string) {
	w.t.Helper()
	f := w.file(in, p)
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte(text), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *syncWorld) read(in provider.Instance, p string) string {
	w.t.Helper()
	b, err := os.ReadFile(w.file(in, p))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// run starts a boosted shell line in dev, follows it to its end, and
// returns its start, output and exit.
func (w *syncWorld) run(line string, artifacts ...string) (*envv1alpha1.StartProcessResponse, string, *envv1alpha1.ProcessExit) {
	w.t.Helper()
	req := boostOn("dev", "sh", "-c", line)
	req.Artifacts = artifacts
	res := w.startBoosted(req)
	out, end, err := attachProcess(w.t.Context(), w.as(workstream).envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: res.GetProcess(), Follow: true})
	if err != nil {
		w.t.Fatal(err)
	}
	return res, out, end.GetExit()
}

func changed(x *envv1alpha1.ProcessExit) map[string]string {
	out := map[string]string{}
	for _, f := range x.GetBoost().GetChanged().GetFiles() {
		out[f.GetPath()] = f.GetStatus()
	}
	return out
}

// leftovers are the staging directories and reports left in an
// instance's boost directory.
func (w *syncWorld) leftovers(in provider.Instance) []string {
	entries, _ := os.ReadDir(w.file(in, ".git/"+guestbin.BoostDir))
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sync-") || strings.HasPrefix(e.Name(), "job-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestABoostedProcessRunsOnTheBoxsWorkTree: a boosted process sees the
// box's work tree as it is, uncommitted and untracked files included and
// ignored ones not; the first sync sends what changed since the commit
// the boost was checked out at; a second, what changed since the first.
// Its exit lists what it changed on the boost, which stays there, and
// copies its artifacts back to the box. Nothing of the syncs is left in
// either instance.
func TestABoostedProcessRunsOnTheBoxsWorkTree(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t)
	w.write(w.box, "a.txt", "edited in the box\n")
	w.write(w.box, "new.txt", "untracked\n")
	w.write(w.box, "build.log", "ignored\n")

	res, out, exit := w.run(`cat a.txt new.txt; ls build.log 2>/dev/null; cat a.txt > seen.txt; echo generated > b.txt; mkdir -p out; echo artifact > out/r`,
		"out", "seen.txt", "nope")
	sync := res.GetBoost()
	if sync.GetSyncFull() || sync.GetSyncFiles() != 2 || sync.GetSyncDeleted() != 0 || sync.GetSync() == nil {
		t.Errorf("the first sync: %v, want the 2 files changed since the boost's checkout", sync)
	}
	if out != "edited in the box\nuntracked\n" {
		t.Errorf("the boosted process saw %q", out)
	}
	if exit.GetCode() != 0 {
		t.Fatalf("it exited %v", exit)
	}
	if got := changed(exit); len(got) != 3 || got["seen.txt"] != "A" || got["b.txt"] != "M" || got["out/r"] != "A" ||
		exit.GetBoost().GetChanged().GetTotal() != 3 {
		t.Errorf("its changed files: %v", exit.GetBoost().GetChanged())
	}
	arts := map[string]*envv1alpha1.ArtifactResult{}
	for _, a := range exit.GetBoost().GetArtifacts() {
		arts[a.GetPath()] = a
	}
	if a := arts["out"]; !a.GetCopied() || a.GetBytes() != 9 {
		t.Errorf("artifact out: %v", a)
	}
	if a := arts["nope"]; a.GetCopied() || a.GetError() == "" {
		t.Errorf("artifact nope, not there: %v", a)
	}
	if got := w.read(w.box, "out/r"); got != "artifact\n" {
		t.Errorf("the artifact in the box: %q", got)
	}
	if got := w.read(w.box, "seen.txt"); got != "edited in the box\n" {
		t.Errorf("the artifact seen.txt in the box: %q", got)
	}
	if got := w.read(w.box, "b.txt"); got != "b\n" {
		t.Errorf("b.txt, which the job changed on the boost, is %q in the box: changed files are not copied back", got)
	}
	if _, err := os.Stat(w.file(w.boost, "build.log")); err == nil {
		t.Error("an ignored file reached the boost")
	}
	list := w.listed("dev")
	if p := list[res.GetProcess()]; !strings.HasPrefix(p.GetCommand(), "sh -c cat a.txt") || p.GetExit().GetBoost().GetChanged().GetTotal() != 3 {
		t.Errorf("the box lists %v", p)
	}
	// Read again, it says the same: settled once, its report gone.
	_, end, err := attachProcess(t.Context(), w.as(workstream).envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: res.GetProcess()})
	if b := end.GetExit().GetBoost(); err != nil || b.GetChanged().GetTotal() != 3 || b.GetError() != "" || len(b.GetArtifacts()) != 3 {
		t.Errorf("its exit read again: %v, %v", b, err)
	}

	// A second job: only what changed since; what the first wrote on the
	// boost is put back first. The artifacts copied back go from the box,
	// which would send them again.
	for _, p := range []string{"out", "seen.txt"} {
		if err := os.RemoveAll(w.file(w.box, p)); err != nil {
			t.Fatal(err)
		}
	}
	w.write(w.box, "b.txt", "b, edited\n")
	res, out, _ = w.run(`cat b.txt; ls seen.txt out 2>&1 | head -1`)
	if s := res.GetBoost(); s.GetSyncFull() || s.GetSyncFiles() != 1 {
		t.Errorf("the second sync: %v, want only b.txt", s)
	}
	if !strings.HasPrefix(out, "b, edited\n") || strings.Contains(out, "seen.txt\n") {
		t.Errorf("the second job saw %q: the box's b.txt, and none of the first job's files", out)
	}
	if l := append(w.leftovers(w.box), w.leftovers(w.boost)...); len(l) != 0 {
		t.Errorf("left after the syncs and jobs: %v", l)
	}
}

// TestABoostLackingTheBaseGetsTheWholeTree: a sync whose base the boost
// lacks is sent again, whole, in the same start.
func TestABoostLackingTheBaseGetsTheWholeTree(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t)
	w.run(`true`)
	// The last tree sent, as tower keeps it, is one the box has and the
	// boost does not.
	cmd := osexec.Command("sh", "-c", `git hash-object -w --stdin <<EOF | xargs -I{} printf '100644 blob {}\tonly-in-the-box\n' | git mktree
only in the box
EOF`)
	cmd.Dir = w.file(w.box, "")
	tree, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	key := store.Key{Kind: "BoostSync", Project: "web", Name: "dev"}
	o, err := w.store.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]string
	if err := json.Unmarshal(o.Data, &rec); err != nil {
		t.Fatal(err)
	}
	rec["tree"] = strings.TrimSpace(string(tree))
	b, _ := json.Marshal(rec)
	if _, err := w.store.Update(t.Context(), key, b, o.Revision); err != nil {
		t.Fatal(err)
	}
	w.write(w.box, "a.txt", "after\n")
	res, out, _ := w.run(`cat a.txt`)
	if !res.GetBoost().GetSyncFull() || out != "after\n" {
		t.Errorf("a sync whose base the boost lacks: %v, saw %q; want the whole tree sent", res.GetBoost(), out)
	}
}

// TestASyncBesideARunningJobKeepsItsWrites: a sync while another process
// of the box runs on the boost leaves what it wrote, and says so.
func TestASyncBesideARunningJobKeepsItsWrites(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	w.startBoosted(boostOn("dev", "sleep"))
	testutil.Eventually(t, "the boost's process running", func() bool {
		b, _ := w.boost("dev")
		return b.GetStatus().GetProcessesRunning() == 1
	})
	res := w.startBoosted(boostOn("dev", "true"))
	if !res.GetBoost().GetBaseShared() {
		t.Errorf("a sync beside a running job: %v, want base_shared", res.GetBoost())
	}
	shared := false
	for _, x := range w.fake.Execs() {
		if len(x.Command) > 2 && x.Command[1] == "boost-apply" && slices.Contains(x.Command, "--shared") {
			shared = true
		}
	}
	if !shared {
		t.Error("the sync beside a running job did not apply --shared")
	}
}

// TestASlowSyncAnswersPending: a sync not done within its wait answers
// pending SYNCING with the ticket; asked again once done, it starts.
func TestASlowSyncAnswersPending(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.srv.SetBoostSync(-1, 0)
	w.readyBox("dev")
	release := make(chan struct{})
	w.fake.OnExec(func(req provider.ExecRequest) (int, bool) {
		if len(req.Command) > 1 && req.Command[0] == provider.GuestBinary && req.Command[1] == "boost-pack" {
			<-release
		}
		return 0, false
	})
	first := w.start(boostOn("dev", "true"))
	w.waitBoost("dev", envv1alpha1.BoostPhase_BOOST_PHASE_READY)
	req := boostOn("dev", "true")
	req.Pending = first.GetPending().GetTicket()
	res := w.start(req)
	if res.GetPending().GetReason() != envv1alpha1.PendingReason_PENDING_REASON_SYNCING || res.GetPending().GetTicket() != req.Pending {
		t.Fatalf("a boosted start whose sync is slow: %v, want pending SYNCING with its ticket", res)
	}
	close(release)
	if res := w.startBoosted(req); res.GetProcess() == "" || res.GetBoost() == nil {
		t.Errorf("asked again once synced: %v", res)
	}
	packs := 0
	for _, x := range w.fake.Execs() {
		if len(x.Command) > 1 && x.Command[1] == "boost-pack" {
			packs++
		}
	}
	if packs != 1 {
		t.Errorf("%d syncs for one start asked again, want 1", packs)
	}
}

// TestArtifactsPastTheCapStay: artifacts copy back in their order until
// the cap; one past it is not copied, and says why.
func TestArtifactsPastTheCapStay(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t)
	w.srv.SetBoostSync(0, 10)
	_, _, exit := w.run(`echo 1234 > small; echo 0123456789 > big`, "small", "big")
	arts := exit.GetBoost().GetArtifacts()
	if len(arts) != 2 || !arts[0].GetCopied() || arts[1].GetCopied() || !strings.Contains(arts[1].GetError(), "past the 10 bytes") {
		t.Errorf("artifacts under a cap of 10 bytes: %v", arts)
	}
	if _, err := os.Stat(w.file(w.box, "big")); err == nil {
		t.Error("an artifact past the cap was copied")
	}
	if exit.GetCode() != 0 {
		t.Errorf("an artifact not copied changed the exit: %v", exit)
	}
}

// TestArtifactsAreRefused: artifacts are a boosted start's, of paths in
// /workspace.
func TestArtifactsAreRefused(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	envs := w.as(workstream).envs
	plain := &envv1alpha1.StartProcessRequest{Environment: boxEnv("dev"), Command: &envv1alpha1.StartProcessRequest_Argv{Argv: &envv1alpha1.Argv{Args: []string{"true"}}}, Artifacts: []string{"out"}}
	if _, err := envs.StartProcess(t.Context(), connect.NewRequest(plain)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("artifacts on a start not boosted: %v", err)
	}
	for _, bad := range []string{"/etc/passwd", "../up", ".git/config", "sub/.git/config", ""} {
		req := boostOn("dev", "true")
		req.Artifacts = []string{bad}
		if _, err := envs.StartProcess(t.Context(), connect.NewRequest(req)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("artifact %q: %v", bad, err)
		}
	}
	for _, good := range []string{".github/out", ".gitignore", "dist/.gitkeep"} {
		req := boostOn("dev", "true")
		req.Artifacts = []string{good}
		if _, err := envs.StartProcess(t.Context(), connect.NewRequest(req)); err != nil {
			t.Errorf("artifact %q: %v", good, err)
		}
	}
}

// appliedBeforeStarts says, for each process started on the boost after
// the first, whether the last boost-apply before it was --shared: a sync
// beside a process already started.
func appliedBeforeStarts(w *world) []bool {
	var out []bool
	shared, starts := false, 0
	for _, x := range w.fake.Execs() {
		switch {
		case x.Instance.Name == "boost-dev" && x.Process:
			if starts++; starts > 1 {
				out = append(out, shared)
			}
		case len(x.Command) > 1 && x.Command[1] == "boost-apply":
			shared = slices.Contains(x.Command, "--shared")
		}
	}
	return out
}

// TestTwoStartsBackToBackShareTheBoost: a second boosted start made at
// once after a first, before any status says the first runs, syncs as
// shared: the first's writes stay.
func TestTwoStartsBackToBackShareTheBoost(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	w.startBoosted(boostOn("dev", "sleep"))
	res := w.startBoosted(boostOn("dev", "sleep"))
	if !res.GetBoost().GetBaseShared() {
		t.Errorf("a start right after another: %v, want base_shared", res.GetBoost())
	}
	if got := appliedBeforeStarts(w); len(got) != 1 || !got[0] {
		t.Errorf("the sync before the second start was shared: %v, want [true]", got)
	}
}

// TestASyncOvertakenIsMadeAgain: two boosted starts whose syncs run one
// after the other before either starts: the one whose sync was overtaken
// syncs again before it starts, and the second to start syncs beside the
// first, which runs: no start runs on another's tree, and no sync puts
// back what a started process writes.
func TestASyncOvertakenIsMadeAgain(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	w.startBoosted(boostOn("dev", "true"))
	w.srv.SetBoostSync(-1, 0)
	gate := make(chan struct{})
	var once sync.Once
	w.fake.OnExec(func(req provider.ExecRequest) (int, bool) {
		if len(req.Command) > 1 && req.Command[0] == provider.GuestBinary && req.Command[1] == "boost-pack" {
			once.Do(func() { <-gate })
		}
		return 0, false
	})
	packs := func() int {
		n := 0
		for _, x := range w.fake.Execs() {
			if len(x.Command) > 1 && x.Command[1] == "boost-pack" {
				n++
			}
		}
		return n
	}
	before := packs()
	a, b := boostOn("dev", "sleep"), boostOn("dev", "sleep")
	a.Pending = w.start(a).GetPending().GetTicket() // its sync holds the lock, held at the gate
	b.Pending = w.start(b).GetPending().GetTicket() // its sync waits for the lock
	close(gate)
	w.waitSettled() // a's sync, then b's: b's is the boost's tree
	w.startBoosted(a)
	w.startBoosted(b)
	if got := packs() - before; got != 4 {
		t.Errorf("%d syncs for two starts whose syncs overtook each other, want 4: each synced again before it started", got)
	}
	if got := appliedBeforeStarts(w); len(got) != 2 || got[0] || !got[1] {
		t.Errorf("the syncs before a's and b's starts shared: %v, want [false true]: a's on its own tree, b's beside a", got)
	}
}

// TestAStaleSyncIsMadeAgain: a start asked again long after its sync
// ended syncs again, as the box may have changed since.
func TestAStaleSyncIsMadeAgain(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.srv.SetBoostSync(-1, 0)
	w.readyBox("dev")
	release := make(chan struct{})
	w.fake.OnExec(func(req provider.ExecRequest) (int, bool) {
		if len(req.Command) > 1 && req.Command[0] == provider.GuestBinary && req.Command[1] == "boost-pack" {
			<-release
		}
		return 0, false
	})
	first := w.start(boostOn("dev", "true"))
	w.waitBoost("dev", envv1alpha1.BoostPhase_BOOST_PHASE_READY)
	req := boostOn("dev", "true")
	req.Pending = first.GetPending().GetTicket()
	if res := w.start(req); res.GetPending().GetReason() != envv1alpha1.PendingReason_PENDING_REASON_SYNCING {
		t.Fatalf("%v, want SYNCING", res)
	}
	close(release)
	w.waitSettled()
	w.clock.Advance(time.Minute)
	w.startBoosted(req)
	packs := 0
	for _, x := range w.fake.Execs() {
		if len(x.Command) > 1 && x.Command[1] == "boost-pack" {
			packs++
		}
	}
	if packs != 2 {
		t.Errorf("%d syncs for a start asked again a minute after its sync, want 2", packs)
	}
}

// TestArtifactsOutliveTheNextSync: a process's artifacts reach the box
// even if nobody reads its exit before the next boosted start, whose sync
// puts back what the first wrote on the boost.
func TestArtifactsOutliveTheNextSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t)
	// The sync settles it, not the controller once it ends.
	w.srv.SetNoSettleJob()
	req := boostOn("dev", "sh", "-c", "mkdir -p out && echo kept > out/r")
	req.Artifacts = []string{"out"}
	w.startBoosted(req)
	w.write(w.box, "a.txt", "changed\n")
	w.run(`ls out 2>&1`)
	if got := w.read(w.box, "out/r"); got != "kept\n" {
		t.Errorf("the first job's artifact in the box: %q", got)
	}
}
