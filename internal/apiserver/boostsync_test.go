package apiserver_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
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
	return newSyncWorldOf(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n", ".gitignore": "*.log\n"})
}

// newSyncWorldOf is newSyncWorld whose repository holds files, every one
// committed, one its .gitignore matches too.
func newSyncWorldOf(t *testing.T, files map[string]string) *syncWorld {
	t.Helper()
	w := boostWorld(t, nil)
	w.fake.RealWorkspaces(t.TempDir(), guestbin.Boost)
	w.fake.SetRemote("https://git.example.com/api.git", "main", files, "c1")
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
func (w *syncWorld) run(line string) (*envv1alpha1.StartProcessResponse, string, *envv1alpha1.ProcessExit) {
	w.t.Helper()
	res := w.startBoosted(boostOn("dev", "sh", "-c", line))
	out, end, err := attachProcess(w.t.Context(), w.as(workstream).envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: res.GetProcess(), Follow: true})
	if err != nil {
		w.t.Fatal(err)
	}
	return res, out, end.GetExit()
}

// moved are the paths a process's write-back moved, with their status.
func moved(x *envv1alpha1.ProcessExit) map[string]string {
	out := map[string]string{}
	for _, f := range x.GetBoost().GetBack().GetMoved() {
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

// TestABoostedJobIsAsIfItRanInTheBox: a boosted process sees the box's
// files as they are, untracked and ignored ones too, never .ws-cache; the
// first sync sends what the box changed since the golden both were made
// of, the next what changed since. Once it ends, what it wrote, ignored
// files too, an executable with its mode, and what it deleted are the
// box's, and its exit says so. Nothing of the syncs is left in either
// instance.
func TestABoostedJobIsAsIfItRanInTheBox(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t)
	w.write(w.box, "a.txt", "edited in the box\n")
	w.write(w.box, "new.txt", "untracked\n")
	w.write(w.box, "build.log", "ignored\n")
	w.write(w.box, ".ws-cache/x", "the box's cache\n")

	res, out, exit := w.run(`cat a.txt new.txt build.log; ls .ws-cache/x 2>/dev/null; mkdir -p bin && printf '#!/bin/sh\necho built\n' > bin/app && chmod +x bin/app && echo generated > b.txt && rm new.txt`)
	sync := res.GetBoost()
	if !sync.GetSyncFull() || sync.GetSyncFiles() != 3 || sync.GetSyncDeleted() != 0 || sync.GetSync() == nil {
		t.Errorf("the first sync: %v, want the 3 files the box changed", sync)
	}
	if out != "edited in the box\nuntracked\nignored\n" {
		t.Errorf("the boosted process saw %q", out)
	}
	if exit.GetCode() != 0 {
		t.Fatalf("it exited %v", exit)
	}
	back := exit.GetBoost().GetBack()
	if got := moved(exit); len(got) != 3 || got["bin/app"] != "A" || got["b.txt"] != "M" || got["new.txt"] != "D" ||
		back.GetTotal() != 3 || back.GetFiles() != 2 || back.GetDeleted() != 1 || back.GetError() != "" || len(back.GetConflicts()) != 0 {
		t.Errorf("its write-back: %v", back)
	}
	if got := w.read(w.box, "b.txt"); got != "generated\n" {
		t.Errorf("b.txt the job wrote is %q in the box", got)
	}
	if info, err := os.Stat(w.file(w.box, "bin/app")); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Errorf("bin/app in the box: %v, %v; want it executable", info, err)
	}
	if _, err := os.Stat(w.file(w.box, "new.txt")); err == nil {
		t.Error("new.txt the job deleted is still in the box")
	}
	if _, err := os.Stat(w.file(w.boost, ".ws-cache/x")); err == nil {
		t.Error(".ws-cache reached the boost")
	}
	list := w.listed("dev")
	if p := list[res.GetProcess()]; !strings.HasPrefix(p.GetCommand(), "sh -c cat a.txt") || p.GetExit().GetBoost().GetBack().GetTotal() != 3 {
		t.Errorf("the box lists %v", p)
	}
	// Read again, it says the same: settled once, its report gone.
	_, end, err := attachProcess(t.Context(), w.as(workstream).envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: res.GetProcess()})
	if b := end.GetExit().GetBoost().GetBack(); err != nil || b.GetTotal() != 3 || b.GetError() != "" {
		t.Errorf("its exit read again: %v, %v", b, err)
	}

	// A second job: only what the box changed since; what the first wrote
	// is the box's, so not sent again.
	w.write(w.box, "a.txt", "a, edited again\n")
	res, out, _ = w.run(`cat a.txt; ls bin/app`)
	if s := res.GetBoost(); s.GetSyncFull() || s.GetSyncFiles() != 1 || s.GetSyncDeleted() != 0 {
		t.Errorf("the second sync: %v, want only a.txt", s)
	}
	if out != "a, edited again\nbin/app\n" {
		t.Errorf("the second job saw %q", out)
	}
	if l := append(w.leftovers(w.box), w.leftovers(w.boost)...); len(l) != 0 {
		t.Errorf("left after the syncs and jobs: %v", l)
	}
}

// TestATrackedFileAnIgnoreRuleMatchesIsSynced: a file the repository
// tracks though its .gitignore matches it reaches the boost; a job's
// change to it comes back, and the box's next change to it reaches the
// boost. One ignored and untracked travels as any file now.
func TestATrackedFileAnIgnoreRuleMatchesIsSynced(t *testing.T) {
	t.Parallel()
	w := newSyncWorldOf(t, map[string]string{"a.txt": "a\n", ".gitignore": "*.log\n*.toml\n", ".devcontainer/mise.toml": "[tools]\n"})
	w.write(w.box, "a.txt", "edited\n")
	res, out, exit := w.run(`cat .devcontainer/mise.toml a.txt; echo job >> .devcontainer/mise.toml`)
	if out != "[tools]\nedited\n" || exit.GetCode() != 0 || res.GetBoost().GetSyncFiles() != 1 {
		t.Errorf("a sync: %v, saw %q, exit %v", res.GetBoost(), out, exit)
	}
	if got := moved(exit); len(got) != 1 || got[".devcontainer/mise.toml"] != "M" {
		t.Errorf("a job that edited mise.toml moved %v", exit.GetBoost().GetBack())
	}
	if got := w.read(w.box, ".devcontainer/mise.toml"); got != "[tools]\njob\n" {
		t.Errorf("mise.toml in the box: %q", got)
	}
	w.write(w.box, ".devcontainer/mise.toml", "[tools]\ngo = \"1.25\"\n")
	res, out, _ = w.run(`cat .devcontainer/mise.toml`)
	if s := res.GetBoost(); s.GetSyncFiles() != 1 || out != "[tools]\ngo = \"1.25\"\n" {
		t.Errorf("the box's change: %v, saw %q", s, out)
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

// TestWhatIsPastTheCapStays: a file past what one process brings back
// stays on the boost, and is said; the rest comes back; the process does
// not fail.
func TestWhatIsPastTheCapStays(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t)
	w.srv.SetBoostSync(0, 10)
	_, _, exit := w.run(`echo 1234 > small; echo 0123456789 > big`)
	back := exit.GetBoost().GetBack()
	if over := back.GetOverCap(); len(over) != 1 || over[0].GetPath() != "big" || over[0].GetBytes() != 11 || back.GetFiles() != 1 {
		t.Errorf("a write-back under a cap of 10 bytes: %v", back)
	}
	if _, err := os.Stat(w.file(w.box, "big")); err == nil {
		t.Error("a file past the cap came back")
	}
	if w.read(w.box, "small") != "1234\n" {
		t.Error("a file under the cap did not come back")
	}
	if exit.GetCode() != 0 {
		t.Errorf("a file not brought back changed the exit: %v", exit)
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
	a.Pending = w.start(a).GetPending().GetTicket()
	// a's sync holds the lock, held at the gate, before b's is made: b's
	// waits for the lock.
	testutil.Eventually(t, "a's sync holding the box's sync lock", func() bool { return packs() == before+1 })
	b.Pending = w.start(b).GetPending().GetTicket()
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

// TestAJobsWritesOutliveTheNextSync: what a process wrote reaches the box
// even if nobody reads its exit before the next boosted start, whose sync
// would undo it on the boost.
func TestAJobsWritesOutliveTheNextSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t)
	// The sync settles it, not the controller once it ends.
	w.srv.SetNoSettleJob()
	w.startBoosted(boostOn("dev", "sh", "-c", "mkdir -p out && echo kept > out/r"))
	w.write(w.box, "a.txt", "changed\n")
	_, out, _ := w.run(`cat out/r 2>&1`)
	if got := w.read(w.box, "out/r"); got != "kept\n" || out != "kept\n" {
		t.Errorf("the first job's write in the box: %q; the second job saw %q", got, out)
	}
}

// packsOf counts the syncs packed so far.
func packsOf(w *world) int {
	n := 0
	for _, x := range w.fake.Execs() {
		if len(x.Command) > 1 && x.Command[1] == "boost-pack" {
			n++
		}
	}
	return n
}

// gatePacks holds each boost-pack until the returned channel lets one
// through.
func gatePacks(w *world) chan<- struct{} {
	tokens := make(chan struct{}, 16)
	w.fake.OnExec(func(req provider.ExecRequest) (int, bool) {
		if len(req.Command) > 1 && req.Command[0] == provider.GuestBinary && req.Command[1] == "boost-pack" {
			<-tokens
		}
		return 0, false
	})
	return tokens
}

// TestAStartWaitsForTheSyncLockNoLongerThanItsWait: a start whose sync is
// done, waiting for the box's sync lock behind another start's sync,
// answers SYNCING within the inline wait, and starts when asked again
// once the lock is free.
func TestAStartWaitsForTheSyncLockNoLongerThanItsWait(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	w.startBoosted(boostOn("dev", "true"))
	w.srv.SetBoostSync(-1, 0)
	tokens := gatePacks(w)
	b := boostOn("dev", "sleep")
	b.Pending = w.start(b).GetPending().GetTicket()
	tokens <- struct{}{}
	w.waitSettled() // b's sync is done; b has not started
	a := boostOn("dev", "sleep")
	a.Pending = w.start(a).GetPending().GetTicket() // a's sync holds the lock, at the gate
	testutil.Eventually(t, "a's sync holding the box's sync lock", func() bool { return packsOf(w) == 3 })

	w.srv.SetBoostSync(300*time.Millisecond, 0)
	answered := make(chan *envv1alpha1.StartProcessResponse, 1)
	asked := time.Now()
	go func() {
		res, err := w.as(workstream).envs.StartProcess(context.Background(), connect.NewRequest(b))
		if err != nil {
			t.Error(err)
		}
		answered <- res.Msg
	}()
	select {
	case res := <-answered:
		answered <- res
		if res.GetPending().GetReason() != envv1alpha1.PendingReason_PENDING_REASON_SYNCING || res.GetPending().GetTicket() != b.Pending {
			t.Errorf("a start waiting for the sync lock answered %v, want SYNCING with its ticket", res)
		}
		if took := time.Since(asked); took > 3*time.Second {
			t.Errorf("it answered after %s, past its 300ms wait", took)
		}
	case <-time.After(5 * time.Second):
		t.Error("a start waiting for the box's sync lock did not answer within 5s, with a wait of 300ms")
	}
	w.srv.SetBoostSync(-1, 0)
	// a's sync, and b's again: a's overtook it.
	for range 8 {
		tokens <- struct{}{}
	}
	<-answered
	w.waitSettled()
	if res := w.startBoosted(b); res.GetProcess() == "" {
		t.Errorf("asked again once the lock is free: %v", res)
	}
}

// TestASyncWaitsForAStartToStart: a sync of the box waits, for the box's
// sync lock, for a start that is starting its process: it applies once
// the process runs, beside it.
func TestASyncWaitsForAStartToStart(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	w.startBoosted(boostOn("dev", "true"))
	applies := func() int {
		n := 0
		for _, x := range w.fake.Execs() {
			if len(x.Command) > 1 && x.Command[1] == "boost-apply" {
				n++
			}
		}
		return n
	}
	var once sync.Once
	b := boostOn("dev", "sleep")
	w.srv.SetBeforeBoostStart(func(_, _ string) {
		once.Do(func() {
			// a holds the lock, about to start its process: b's start and
			// its sync now.
			w.srv.SetBoostSync(-1, 0)
			before := applies()
			res, err := w.as(workstream).envs.StartProcess(context.Background(), connect.NewRequest(b))
			if err != nil {
				t.Error(err)
				return
			}
			b.Pending = res.Msg.GetPending().GetTicket()
			testutil.Eventually(t, "b's sync waiting for the lock, or applied", func() bool {
				return w.srv.BoostSyncsWaiting("t1", "web", "dev") > 0 || applies() > before
			})
		})
	})
	w.startBoosted(boostOn("dev", "sleep"))
	w.waitSettled()
	w.startBoosted(b)
	if got := appliedBeforeStarts(w); len(got) != 2 || !got[1] {
		t.Errorf("the syncs before a's and b's starts shared: %v, want b's beside a, applied once a ran", got)
	}
	// b's sync was applied after a's process started.
	var order []string
	for _, x := range w.fake.Execs() {
		switch {
		case x.Instance.Name == "boost-dev" && x.Process:
			order = append(order, "start")
		case len(x.Command) > 1 && x.Command[1] == "boost-apply":
			order = append(order, "apply")
		}
	}
	if len(order) < 4 || order[len(order)-4] != "apply" || order[len(order)-3] != "start" || order[len(order)-2] != "apply" {
		t.Errorf("applies and starts in order %v, want ... apply, start (a), apply (b), start (b)", order)
	}
}

// TestASyncStaysGoodForAReask: a start asked again 20 seconds after its
// sync ended, as a caller asking every 10 seconds may be, starts from it.
func TestASyncStaysGoodForAReask(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.srv.SetBoostSync(-1, 0)
	w.readyBox("dev")
	tokens := gatePacks(w)
	first := w.start(boostOn("dev", "true"))
	w.waitBoost("dev", envv1alpha1.BoostPhase_BOOST_PHASE_READY)
	req := boostOn("dev", "true")
	req.Pending = first.GetPending().GetTicket()
	if res := w.start(req); res.GetPending().GetReason() != envv1alpha1.PendingReason_PENDING_REASON_SYNCING {
		t.Fatalf("%v, want SYNCING", res)
	}
	tokens <- struct{}{}
	w.waitSettled()
	w.clock.Advance(20 * time.Second)
	for range 4 {
		tokens <- struct{}{} // so a sync made again ends, rather than hangs
	}
	if res := w.start(req); res.GetProcess() == "" {
		t.Errorf("asked again 20s after its sync: %v, want started", res)
	}
	if packs := packsOf(w); packs != 1 {
		t.Errorf("%d syncs, want 1", packs)
	}
}

// TestContentStillNotTheBoxsIsSaidSo: a tracked file whose content on the
// boost is not the box's, under the same records, is sent again; if it
// still is not, the start fails saying so.
func TestContentStillNotTheBoxsIsSaidSo(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t)
	w.run(`true`)
	// a.txt on the boost: other content, the same size and mtime.
	f := w.file(w.boost, "a.txt")
	info, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	// Each apply that sends a.txt again sends the boost's content.
	w.fake.OnExec(func(req provider.ExecRequest) (int, bool) {
		if len(req.Command) > 1 && req.Command[0] == provider.GuestBinary && req.Command[1] == "boost-apply" {
			i := slices.Index(req.Command, "--staging")
			corrupt(t, w.fake.WorkspaceDir(req.Instance, req.Command[i+1]+"/files.tar"), "a.txt", "B\n")
		}
		return 0, false
	})
	w.write(w.box, "b.txt", "b, edited\n")
	_, err = w.as(workstream).envs.StartProcess(t.Context(), connect.NewRequest(boostOn("dev", "true")))
	if connect.CodeOf(err) != connect.CodeInternal || !strings.Contains(err.Error(), "a.txt is not the box's, sent again") {
		t.Errorf("content still not the box's: %v", err)
	}
	forced := 0
	for _, x := range w.fake.Execs() {
		if len(x.Command) > 1 && x.Command[1] == "boost-pack" && slices.Contains(x.Command, "--force") {
			forced++
		}
	}
	if forced != 1 {
		t.Errorf("%d syncs sent a.txt again, want 1", forced)
	}
}

// corrupt rewrites the tar file's entry of name, if it holds one, to
// content of its size.
func corrupt(t *testing.T, file, name, content string) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		return
	}
	var out bytes.Buffer
	tr, tw := tar.NewReader(bytes.NewReader(b)), tar.NewWriter(&out)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		body, _ := io.ReadAll(tr)
		if hdr.Name == name && len(body) == len(content) {
			body = []byte(content)
		}
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write(body)
	}
	_ = tw.Close()
	if err := os.WriteFile(file, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestASharedSyncWithoutItsLastWaits: a boosted start beside a running
// process of the box, whose last sync tower does not know, is not synced
// over what that one writes: it waits, SYNCING, and starts once that one
// ended.
func TestASharedSyncWithoutItsLastWaits(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	id := w.startBoosted(boostOn("dev", "sleep")).GetProcess()
	testutil.Eventually(t, "the boost's process running", func() bool {
		b, _ := w.boost("dev")
		return b.GetStatus().GetProcessesRunning() == 1
	})
	key := store.Key{Kind: "BoostSync", Project: "web", Name: "dev"}
	o, err := w.store.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.store.Delete(t.Context(), key, o.Revision); err != nil {
		t.Fatal(err)
	}
	req := boostOn("dev", "true")
	res := w.start(req)
	if res.GetProcess() != "" || !strings.Contains(res.GetPending().GetMessage(), "last sync is not known") {
		t.Fatalf("a start beside a running one, its last sync unknown: %v, want pending SYNCING", res)
	}
	b, _ := w.boost("dev")
	bref := provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}
	ps, _ := w.fake.ListProcesses(t.Context(), bref)
	for _, p := range ps {
		if p.Exit == nil {
			if err := w.fake.EndProcessWith(bref, p.ID, provider.ProcessExit{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	req.Pending = res.GetPending().GetTicket()
	if res := w.startBoosted(req); res.GetProcess() == "" || res.GetProcess() == id || res.GetBoost().GetBaseShared() {
		t.Errorf("asked again once it ended: %v", res)
	}
}

// TestAProcessStartedBeforeTheUpgradeSaysSo: a boosted process started
// under an earlier tower, which kept no sync's manifest for it, is
// settled saying nothing was brought back.
func TestAProcessStartedBeforeTheUpgradeSaysSo(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t)
	w.srv.SetNoSettleJob()
	res := w.startBoosted(boostOn("dev", "sh", "-c", "echo x > x.txt"))
	key := store.Key{Kind: "BoxProcesses", Project: "web", Name: "dev"}
	o, err := w.store.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(o.Data, &rec); err != nil {
		t.Fatal(err)
	}
	for _, p := range rec["boost"].([]any) {
		delete(p.(map[string]any), "syncId")
		delete(p.(map[string]any), "settled")
		delete(p.(map[string]any), "run")
	}
	b, _ := json.Marshal(rec)
	if _, err := w.store.Update(t.Context(), key, b, o.Revision); err != nil {
		t.Fatal(err)
	}
	_, end, err := attachProcess(t.Context(), w.as(workstream).envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: res.GetProcess(), Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	if r := end.GetExit().GetBoost(); !strings.Contains(r.GetError(), "started before this tower") || r.GetBack() != nil {
		t.Errorf("a process started before the upgrade: %v", r)
	}
	if _, err := os.Stat(w.file(w.box, "x.txt")); err == nil {
		t.Error("its write came back")
	}
}

// TestAStartedTicketIsAnsweredItsStart: a ticket asked about again after
// its process started, as by a caller that lost the answer, is answered
// that process and where it ran, never a second start; while its start is
// under way, pending; once dropped, a start anew.
func TestAStartedTicketIsAnsweredItsStart(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	req := boostOn("dev", "sleep")
	req.Pending = w.start(req).GetPending().GetTicket()
	w.waitBoost("dev", envv1alpha1.BoostPhase_BOOST_PHASE_READY)
	var during *envv1alpha1.StartProcessResponse
	var once sync.Once
	w.srv.SetBeforeBoostStart(func(_, _ string) {
		once.Do(func() {
			res, err := w.as(workstream).envs.StartProcess(context.Background(), connect.NewRequest(req))
			if err != nil {
				t.Error(err)
				return
			}
			during = res.Msg
		})
	})
	started := w.start(req) // its answer lost
	if started.GetProcess() == "" {
		t.Fatalf("the start: %v", started)
	}
	if during.GetProcess() != "" || during.GetPending().GetTicket() != req.Pending {
		t.Errorf("asked again while its start was under way: %v, want pending with its ticket", during)
	}
	again := w.start(req)
	if again.GetProcess() != started.GetProcess() || !proto.Equal(again.GetRanOn(), started.GetRanOn()) {
		t.Errorf("asked again once started: %v, want its start %v", again, started)
	}
	if n := boostProcessesStarted(w); n != 1 {
		t.Errorf("%d processes started on the boost for one ticket asked three times, want 1", n)
	}
	w.clock.Advance(6 * time.Minute)
	if res := w.start(req); res.GetProcess() == started.GetProcess() {
		t.Errorf("a ticket asked again past its life: %v, want a start anew", res)
	}
}

// boostProcessesStarted counts the processes started on dev's boost.
func boostProcessesStarted(w *world) int {
	n := 0
	for _, x := range w.fake.Execs() {
		if x.Instance.Name == "boost-dev" && x.Process {
			n++
		}
	}
	return n
}

// TestAJobsWriteBackOutlivesALaterSync: a process still running when the
// box syncs for another keeps what it wrote so far, which that sync,
// beside it, leaves; and what its write-back compares the box with: all
// it wrote comes back once it ends.
func TestAJobsWriteBackOutlivesALaterSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t)
	w.fake.GuestProcessesInBackground()
	first := w.startBoosted(boostOn("dev", "sh", "-c", `echo early > early.out; while [ ! -e go ]; do sleep 0.05; done; echo first > first.out`))
	testutil.Eventually(t, "the first process running, early.out written", func() bool {
		b, _ := w.world.boost("dev")
		_, err := os.Stat(w.file(w.boost, "early.out"))
		return b.GetStatus().GetProcessesRunning() == 1 && err == nil
	})
	w.write(w.box, "a.txt", "for the second\n")
	_, out, _ := w.run(`cat a.txt`)
	if out != "for the second\n" {
		t.Fatalf("the second saw %q", out)
	}
	w.write(w.boost, "go", "")
	_, end, err := attachProcess(t.Context(), w.as(workstream).envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: first.GetProcess(), Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	if back := end.GetExit().GetBoost().GetBack(); back.GetError() != "" || moved(end.GetExit())["first.out"] != "A" {
		t.Errorf("the first's write-back: %v", back)
	}
	if got, early := w.read(w.box, "first.out"), w.read(w.box, "early.out"); got != "first\n" || early != "early\n" {
		t.Errorf("first.out in the box: %q, early.out %q", got, early)
	}
}

// TestAWriteBackWaitsForItsBox: a boosted process that ended while its box
// is being made again is not settled without its changes: it stays
// unsettled, its boost is kept, and once the box is ready what it wrote
// comes back.
func TestAWriteBackWaitsForItsBox(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t)
	w.fake.GuestProcessesInBackground()
	res := w.startBoosted(boostOn("dev", "sh", "-c", `while [ ! -e go ]; do sleep 0.05; done; echo x > x.out`))
	w.fake.FailCheckouts(fmt.Errorf("the mirror does not answer: %w", provider.ErrUnavailable))
	w.fake.Vanish(w.box)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STARTING)
	w.write(w.boost, "go", "")
	settled := func() (bool, bool) {
		o, err := w.store.Get(t.Context(), store.Key{Kind: "BoxProcesses", Project: "web", Name: "dev"})
		if err != nil {
			t.Fatal(err)
		}
		var rec struct {
			Boost []struct {
				ID      string          `json:"id"`
				Exit    json.RawMessage `json:"exit"`
				Settled bool            `json:"settled"`
			} `json:"boost"`
		}
		if err := json.Unmarshal(o.Data, &rec); err != nil {
			t.Fatal(err)
		}
		for _, p := range rec.Boost {
			if p.ID == res.GetProcess() {
				return len(p.Exit) > 0 && string(p.Exit) != "null", p.Settled
			}
		}
		return false, false
	}
	testutil.Eventually(t, "its process ended, its exit kept", func() bool { ended, _ := settled(); return ended })
	if err := w.srv.SettleBoostProcess(t.Context(), "t1", "web", "dev", "boost-dev", res.GetProcess()); !errors.Is(err, apiserver.ErrWriteBackLater) {
		t.Fatalf("settled while its box starts: %v, want written back later", err)
	}
	if _, done := settled(); done {
		t.Fatal("a process settled while its box was not ready")
	}
	// Long past boost_idle: its boost is kept, holding what it wrote.
	w.clock.Advance(11 * time.Minute)
	w.fake.FailCheckouts(nil)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	_, end, err := attachProcess(t.Context(), w.as(workstream).envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: res.GetProcess(), Follow: true})
	if err != nil || moved(end.GetExit())["x.out"] != "A" || w.read(w.box, "x.out") != "x\n" {
		t.Errorf("once its box is ready: %v, %v; x.out in the box %q", end.GetExit().GetBoost(), err, w.read(w.box, "x.out"))
	}
}

// TestAStopWritesBackFirst: a box stopped at workstream's request gets
// what its boost's ended processes wrote before its instance stops.
func TestAStopWritesBackFirst(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t)
	w.srv.SetNoSettleJob()
	res := w.startBoosted(boostOn("dev", "sh", "-c", "echo y > y.out"))
	w.setRunState("dev", envv1alpha1.RunState_RUN_STATE_STOPPED)
	testutil.Eventually(t, "the stopped box's boost gone", func() bool { _, ok := w.world.boost("dev"); return !ok })
	if got := w.read(w.box, "y.out"); got != "y\n" {
		t.Errorf("y.out in the stopped box: %q", got)
	}
	w.setRunState("dev", envv1alpha1.RunState_RUN_STATE_RUNNING)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	if p := w.listed("dev")[res.GetProcess()]; moved(p.GetExit())["y.out"] != "A" {
		t.Errorf("its exit: %v", p.GetExit().GetBoost())
	}
}

// TestAStartedTicketSurvivesARestart: a ticket that started its process,
// asked again after the apiserver restarted, is answered that process;
// given up, its process is sent TERM.
func TestAStartedTicketSurvivesARestart(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	req := boostOn("dev", "sleep")
	req.Pending = w.start(req).GetPending().GetTicket()
	w.waitBoost("dev", envv1alpha1.BoostPhase_BOOST_PHASE_READY)
	started := w.start(req)
	if started.GetProcess() == "" {
		t.Fatalf("the start: %v", started)
	}
	w.restart()
	again := w.start(req)
	if again.GetProcess() != started.GetProcess() || !proto.Equal(again.GetRanOn(), started.GetRanOn()) {
		t.Errorf("asked again after a restart: %v, want its start %v", again, started)
	}
	n := 0
	for _, p := range w.listed("dev") {
		if p.GetRanOn().GetKind() == envv1alpha1.RanOnKind_RAN_ON_KIND_BOOST {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d boost processes for one ticket, want 1", n)
	}
	cancel := proto.Clone(req).(*envv1alpha1.StartProcessRequest)
	cancel.Cancel = true
	if res := w.start(cancel); res.GetProcess() != started.GetProcess() {
		t.Errorf("a started ticket given up: %v, want its process", res)
	}
	testutil.Eventually(t, "the given-up process ended", func() bool {
		return !w.listed("dev")[started.GetProcess()].GetRunning()
	})
}
