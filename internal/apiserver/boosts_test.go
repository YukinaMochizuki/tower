package apiserver_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// boostOn is a boosted start of argv in box, at the target's default
// size.
func boostOn(box string, args ...string) *envv1alpha1.StartProcessRequest {
	return &envv1alpha1.StartProcessRequest{
		Environment: boxEnv(box), Command: &envv1alpha1.StartProcessRequest_Argv{Argv: &envv1alpha1.Argv{Args: args}},
		Boost: &envv1alpha1.BoostIntent{Size: &envv1alpha1.BoostIntent_On{On: true}},
	}
}

// boostOf asks for a boost of cpu and memory.
func boostOf(req *envv1alpha1.StartProcessRequest, cpu, memory string) *envv1alpha1.StartProcessRequest {
	req.Boost = &envv1alpha1.BoostIntent{Size: &envv1alpha1.BoostIntent_Resources{Resources: &envv1alpha1.Resources{Cpu: cpu, Memory: memory}}}
	return req
}

func (w *world) start(req *envv1alpha1.StartProcessRequest) *envv1alpha1.StartProcessResponse {
	w.t.Helper()
	res, err := w.as(workstream).envs.StartProcess(w.t.Context(), connect.NewRequest(req))
	if err != nil {
		w.t.Fatal(err)
	}
	return res.Msg
}

// boost is the box's boost, if any.
func (w *world) boost(box string) (*envv1alpha1.Boost, bool) {
	w.t.Helper()
	o, err := w.get(registry.Boost, "web", box)
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, false
	}
	if err != nil {
		w.t.Fatal(err)
	}
	return o.(*envv1alpha1.Boost), true
}

func (w *world) waitBoost(box string, phase envv1alpha1.BoostPhase) *envv1alpha1.Boost {
	w.t.Helper()
	var b *envv1alpha1.Boost
	testutil.Eventually(w.t, "boost "+box+" "+phase.String(), func() (bool, string) {
		var ok bool
		b, ok = w.boost(box)
		return ok && b.GetStatus().GetPhase() == phase, b.GetStatus().String()
	})
	return b
}

// startBoosted asks for req until it starts, with the ticket each pending
// answer gives, the boost coming up meanwhile; it returns the start.
func (w *world) startBoosted(req *envv1alpha1.StartProcessRequest) *envv1alpha1.StartProcessResponse {
	w.t.Helper()
	req = proto.Clone(req).(*envv1alpha1.StartProcessRequest)
	var res *envv1alpha1.StartProcessResponse
	testutil.Eventually(w.t, "the boosted start started", func() (bool, string) {
		res = w.start(req)
		if res.GetProcess() != "" {
			return true, ""
		}
		req.Pending = res.GetPending().GetTicket()
		return false, res.GetPending().String()
	})
	return res
}

func boostWorld(t *testing.T, set *targetsv1alpha1.TargetSettings) *world {
	return newWorld(t, worldOption{hangar: true, settings: set})
}

// TestABoostedProcessRunsOnItsBoost: a boosted start answers at once that
// the box's boost is starting, with a ticket; the boost is made as the
// box's instance is, of the target's default size, on its network under an
// alias of its own; asked again once it is READY, the process starts on it
// with an id of tower's, and every later process call on the box reaches
// it, listed with the box's own, each saying where it runs.
func TestABoostedProcessRunsOnItsBoost(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	box := w.readyBox("dev")
	own := startProcess(t, w.as(workstream).envs, &envv1alpha1.StartProcessRequest{Environment: boxEnv("dev"), Command: &envv1alpha1.StartProcessRequest_Argv{Argv: &envv1alpha1.Argv{Args: []string{"sleep"}}}})

	first := w.start(boostOn("dev", "sleep"))
	if first.GetProcess() != "" || first.GetPending().GetReason() != envv1alpha1.PendingReason_PENDING_REASON_BOOST_STARTING || first.GetPending().GetTicket() == "" ||
		first.GetPending().GetEstimatedWait() == nil {
		t.Fatalf("a boosted start of a box with no boost answered %v, want pending BOOST_STARTING with a ticket and a wait", first)
	}
	b := w.waitBoost("dev", envv1alpha1.BoostPhase_BOOST_PHASE_READY)
	in, ok := w.instance(b.GetStatus().GetInstance())
	if !ok {
		t.Fatalf("the boost's instance %q is not there", b.GetStatus().GetInstance())
	}
	boxIn, _ := w.instance(box.GetStatus().GetInstance())
	if got := in.GetSpec(); got.GetResources().GetCpu() != "8" || got.GetResources().GetMemory() != "8192Mi" ||
		got.GetImage() != boxIn.GetSpec().GetImage() || got.GetDiskGolden() != boxIn.GetSpec().GetDiskGolden() ||
		got.GetNetwork() != "ws1" || got.GetAlias() != "dev-boost" {
		t.Errorf("the boost's instance is %v; the box's %v", got, boxIn.GetSpec())
	}
	if b.GetStatus().GetSetupStartTime() == nil || b.GetStatus().GetSetupEndTime() == nil {
		t.Errorf("a READY boost says no setup times: %v", b.GetStatus())
	}
	if w.fake.Checkouts(provider.Instance{Project: "web", Name: in.GetMetadata().GetName()}) == 0 {
		t.Error("the box's repository was not checked out in its boost")
	}

	req := boostOn("dev", "sleep")
	req.Pending = first.GetPending().GetTicket()
	res := w.start(req)
	id := res.GetProcess()
	if !strings.HasPrefix(id, "boost-") || res.GetRanOn().GetKind() != envv1alpha1.RanOnKind_RAN_ON_KIND_BOOST || res.GetRanOn().GetResources().GetCpu() != "8" {
		t.Fatalf("asked again once the boost is READY: %v", res)
	}
	envs := w.as(workstream).envs
	list := must[envv1alpha1.ListProcessesResponse](t)(envs.ListProcesses(t.Context(), connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: boxEnv("dev")})))
	where := map[string]envv1alpha1.RanOnKind{}
	for _, p := range list.GetProcesses() {
		where[p.GetProcess()] = p.GetRanOn().GetKind()
	}
	if where[own] != envv1alpha1.RanOnKind_RAN_ON_KIND_BOX || where[id] != envv1alpha1.RanOnKind_RAN_ON_KIND_BOOST || len(where) != 2 {
		t.Errorf("the box lists %v, want %s in the box and %s on its boost", list.GetProcesses(), own, id)
	}

	// It runs on the boost's instance, by its id there.
	bref := provider.Instance{Project: "web", Name: in.GetMetadata().GetName()}
	ps, err := w.fake.ListProcesses(t.Context(), bref)
	if err != nil || len(ps) != 1 || !strings.HasSuffix(id, "-"+ps[0].ID) {
		t.Fatalf("the boost's instance runs %+v (%v), want the process %s", ps, err, id)
	}
	if err := w.fake.WriteProcess(bref, ps[0].ID, provider.Stdout, "16 cpus\n"); err != nil {
		t.Fatal(err)
	}
	must[envv1alpha1.SignalProcessResponse](t)(envs.SignalProcess(t.Context(), connect.NewRequest(&envv1alpha1.SignalProcessRequest{
		Environment: boxEnv("dev"), Process: id, Signal: envv1alpha1.Signal_SIGNAL_TERM,
	})))
	out, end, err := attachProcess(t.Context(), envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: id, Follow: true})
	if err != nil || out != "16 cpus\n" || end.GetExit().GetSignal() != envv1alpha1.Signal_SIGNAL_TERM {
		t.Errorf("attaching to the boost's process: %q, %v, %v", out, end, err)
	}
	if _, _, err := attachProcess(t.Context(), envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: "boost-0000-p9"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("attaching to a boost process the box never had: %v, want NotFound", err)
	}
}

// TestABoostGoesWhenIdle: a boost nothing has run on for boost_idle is
// deleted, its instance with it; while a process runs on it, neither it
// nor its box goes. Its processes' exits are kept: listed, and an attach
// answers the exit, with the output gone. Its run's usage is recorded as
// run on a boost.
func TestABoostGoesWhenIdle(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{IdleStopAfter: durationpb.New(30 * time.Minute)})
	w.readyBox("dev")
	id := w.startBoosted(boostOn("dev", "sleep")).GetProcess()
	b, _ := w.boost("dev")
	bref := provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}
	testutil.Eventually(t, "the boost's process reported running", func() bool {
		b, _ := w.boost("dev")
		return b.GetStatus().GetProcessesRunning() == 1
	})

	w.advance(time.Hour)
	if _, ok := w.boost("dev"); !ok {
		t.Fatal("a boost running a process went")
	}
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("a box whose boost runs a process is %v after an hour", p)
	}

	ps, _ := w.fake.ListProcesses(t.Context(), bref)
	peak := int64(3 << 30)
	if err := w.fake.EndProcessWith(bref, ps[0].ID, provider.ProcessExit{Code: 0, Usage: &provider.ProcessUsage{MemoryPeak: &peak, Wall: time.Minute}}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the boost's process reported ended", func() bool {
		b, _ := w.boost("dev")
		return b.GetStatus().GetProcessesRunning() == 0
	})
	w.advance(9 * time.Minute)
	if _, ok := w.boost("dev"); !ok {
		t.Fatal("a boost went 9 minutes after its process ended, before boost_idle")
	}
	w.advance(2 * time.Minute)
	testutil.Eventually(t, "the idle boost deleted", func() bool {
		_, ok := w.boost("dev")
		_, inOK := w.instance(bref.Name)
		return !ok && !inOK
	})

	envs := w.as(workstream).envs
	list := must[envv1alpha1.ListProcessesResponse](t)(envs.ListProcesses(t.Context(), connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: boxEnv("dev")})))
	if len(list.GetProcesses()) != 1 || list.GetProcesses()[0].GetProcess() != id || list.GetProcesses()[0].GetExit().GetMemoryPeakBytes() != peak ||
		list.GetProcesses()[0].GetRanOn().GetKind() != envv1alpha1.RanOnKind_RAN_ON_KIND_BOOST {
		t.Errorf("after its boost went, the box lists %v", list.GetProcesses())
	}
	_, end, err := attachProcess(t.Context(), envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: id})
	if err != nil || end.GetExit() == nil || end.GetExit().GetCode() != 0 {
		t.Errorf("attaching to a process of a boost gone: %v, %v", end, err)
	}
	if _, err := envs.SignalProcess(t.Context(), connect.NewRequest(&envv1alpha1.SignalProcessRequest{
		Environment: boxEnv("dev"), Process: id, Signal: envv1alpha1.Signal_SIGNAL_TERM,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("signalling a process of a boost gone: %v, want FailedPrecondition", err)
	}
	uses, _ := w.commandUses()
	recorded := false
	for _, u := range uses {
		for _, run := range u.Runs {
			recorded = recorded || (run.Process == id && run.RanOn.Kind == "boost" && run.RanOn.Memory == "8192Mi" && run.MemoryPeak != nil && *run.MemoryPeak == peak)
		}
	}
	if !recorded {
		t.Errorf("the boost's run is not recorded as run on a boost of 8192Mi: %+v", uses)
	}

	// The next boosted start makes another, whose ids are its own.
	again := w.startBoosted(boostOn("dev", "true")).GetProcess()
	if again == id || !strings.HasPrefix(again, "boost-") {
		t.Errorf("a process of the next boost is %q; the gone one's was %q", again, id)
	}
}

// TestBoostsQueueOnTheBudget: with boost_budget below one boost, one
// boost at a time runs. A second box's boosted start answers at once that
// it is queued, first in line, with about how long; it may be given up;
// and asked again once the first boost is gone, its boost is made.
func TestBoostsQueueOnTheBudget(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{BoostBudget: &targetsv1alpha1.BoostSize{Cpu: "4", Memory: "4Gi"}})
	w.readyBox("a")
	w.readyBox("b", func(b *envv1alpha1.Box) { b.Spec.Workstream = "ws2" })
	w.startBoosted(boostOn("a", "true"))

	q := w.start(boostOn("b", "true"))
	p := q.GetPending()
	if p.GetReason() != envv1alpha1.PendingReason_PENDING_REASON_QUEUED_BUDGET || p.GetPosition() != 1 || p.GetEstimatedWait().AsDuration() < 10*time.Minute {
		t.Fatalf("a boost past the budget answered %v, want queued first, for at least boost_idle", q)
	}
	if _, ok := w.boost("b"); ok {
		t.Fatal("a boost past the budget was made")
	}
	// Given up, nothing waits; asked again with a ticket it no longer
	// holds, it is queued anew.
	cancel := boostOn("b", "true")
	cancel.Pending, cancel.Cancel = p.GetTicket(), true
	if res := w.start(cancel); res.GetPending() != nil || res.GetProcess() != "" {
		t.Errorf("giving a ticket up answered %v", res)
	}
	again := boostOn("b", "true")
	again.Pending = p.GetTicket()
	p = w.start(again).GetPending()
	if p.GetReason() != envv1alpha1.PendingReason_PENDING_REASON_QUEUED_BUDGET || p.GetPosition() != 1 {
		t.Fatalf("asked again after giving up: %v", p)
	}

	// The first boost goes once idle; the second is made when asked again.
	w.advance(11 * time.Minute)
	testutil.Eventually(t, "the first boost gone", func() bool { _, ok := w.boost("a"); return !ok })
	res := w.startBoosted(func() *envv1alpha1.StartProcessRequest {
		r := boostOn("b", "true")
		r.Pending = p.GetTicket()
		return r
	}())
	if res.GetRanOn().GetKind() != envv1alpha1.RanOnKind_RAN_ON_KIND_BOOST {
		t.Errorf("the queued start, once it fits: %v", res)
	}
}

// TestASmallerBoostIsReplaced: a boost smaller than asked is deleted and
// made again larger once nothing runs on it; while something does, the
// start answers that the boost is busy.
func TestASmallerBoostIsReplaced(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	small := w.startBoosted(boostOf(boostOn("dev", "sleep"), "2", "2Gi"))
	b, _ := w.boost("dev")
	testutil.Eventually(t, "the small boost's process running", func() bool {
		b, _ := w.boost("dev")
		return b.GetStatus().GetProcessesRunning() == 1
	})
	busy := w.start(boostOf(boostOn("dev", "true"), "4", "8Gi"))
	if busy.GetPending().GetReason() != envv1alpha1.PendingReason_PENDING_REASON_BOOST_BUSY {
		t.Fatalf("a larger boost asked of a busy smaller one: %v", busy)
	}
	must[envv1alpha1.SignalProcessResponse](t)(w.as(workstream).envs.SignalProcess(t.Context(), connect.NewRequest(&envv1alpha1.SignalProcessRequest{
		Environment: boxEnv("dev"), Process: small.GetProcess(), Signal: envv1alpha1.Signal_SIGNAL_KILL,
	})))
	testutil.Eventually(t, "the small boost's process ended", func() bool {
		b, _ := w.boost("dev")
		return b.GetStatus().GetProcessesRunning() == 0
	})
	req := boostOf(boostOn("dev", "true"), "4", "8Gi")
	req.Pending = busy.GetPending().GetTicket()
	res := w.startBoosted(req)
	nb, _ := w.boost("dev")
	if nb.GetMetadata().GetUid() == b.GetMetadata().GetUid() || nb.GetSpec().GetResources().GetMemory() != "8192Mi" || res.GetRanOn().GetResources().GetCpu() != "4" {
		t.Errorf("after the replacement the boost is %v, the start %v", nb.GetSpec(), res)
	}
}

// TestAStoppedBoxLosesItsBoost: stopping a box at workstream's request
// deletes its boost whatever runs on it; the exit of what had ended is
// kept, and what still ran is reported ended, its exit lost.
func TestAStoppedBoxLosesItsBoost(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	ended := w.startBoosted(boostOn("dev", "true")).GetProcess()
	running := w.start(boostOn("dev", "sleep")).GetProcess()
	testutil.Eventually(t, "the boost's process running", func() bool {
		b, _ := w.boost("dev")
		return b.GetStatus().GetProcessesRunning() == 1
	})
	w.setRunState("dev", envv1alpha1.RunState_RUN_STATE_STOPPED)
	testutil.Eventually(t, "the stopped box's boost gone", func() bool { _, ok := w.boost("dev"); return !ok })
	w.setRunState("dev", envv1alpha1.RunState_RUN_STATE_RUNNING)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	got := w.listed("dev")
	if p := got[ended]; p.GetRunning() || p.GetExit().GetCode() != 0 || p.GetExit().GetLost() || p.GetCommand() != "true" {
		t.Errorf("the ended process, after its boost went: %v", p)
	}
	if p := got[running]; p.GetRunning() || !p.GetExit().GetLost() || p.GetExit().GetCode() != 255 {
		t.Errorf("the process that still ran, after its boost went: %v, want ended, its exit lost, code 255", p)
	}
	_, end, err := attachProcess(t.Context(), w.as(workstream).envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: running, Follow: true})
	if err != nil || !end.GetExit().GetLost() || end.GetExit().GetCode() != 255 {
		t.Errorf("following a process whose boost went: %v, %v; want its end, lost", end, err)
	}
}

// setRunState sets the box's run_state, as workstream does, again on a
// conflict with tower writing its status.
func (w *world) setRunState(box string, rs envv1alpha1.RunState) {
	w.t.Helper()
	testutil.Eventually(w.t, "the box's run_state set", func() (bool, string) {
		b := proto.Clone(w.box(box)).(*envv1alpha1.Box)
		b.Spec.RunState = rs
		_, err := w.asSystem().generic.Apply(w.t.Context(), registry.Box, b)
		return err == nil, fmt.Sprint(err)
	})
}

// listed is the box's processes through ListProcesses, by id.
func (w *world) listed(box string) map[string]*envv1alpha1.ProcessInfo {
	w.t.Helper()
	list := must[envv1alpha1.ListProcessesResponse](w.t)(w.as(workstream).envs.ListProcesses(w.t.Context(), connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: boxEnv(box)})))
	out := map[string]*envv1alpha1.ProcessInfo{}
	for _, p := range list.GetProcesses() {
		out[p.GetProcess()] = p
	}
	return out
}

// TestADeletedBoxsBoostKeepsItsUsage: deleting a box deletes its boost
// and its instance, and the usage of the boost's processes not read yet
// is recorded first, as run on a boost.
func TestADeletedBoxsBoostKeepsItsUsage(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	id := w.startBoosted(boostOn("dev", "sleep")).GetProcess()
	b, _ := w.boost("dev")
	bref := provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}
	ps, _ := w.fake.ListProcesses(t.Context(), bref)
	peak := int64(5 << 30)
	if err := w.fake.EndProcessWith(bref, ps[0].ID, provider.ProcessExit{Usage: &provider.ProcessUsage{MemoryPeak: &peak, Wall: time.Minute}}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.asSystem().generic.Delete(t.Context(), registry.Box, client.Key{Project: "web", Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the boost and its instance gone with the box", func() bool {
		_, ok := w.boost("dev")
		_, inOK := w.instance(bref.Name)
		return !ok && !inOK
	})
	// Kept by whichever of the box's deletion and the settling of its
	// ended process reads its exit first; the settling may end after the
	// instance is gone.
	testutil.Eventually(t, "the deleted box's boost process recorded", func() (bool, string) {
		uses, _ := w.commandUses()
		for _, u := range uses {
			for _, run := range u.Runs {
				if run.Process == id && run.RanOn.Kind == "boost" && run.MemoryPeak != nil && *run.MemoryPeak == peak && u.Repository == "api" {
					return true, ""
				}
			}
		}
		return false, fmt.Sprintf("%+v", uses)
	})
}

// TestABoostThatDoesNotStartFails: a boost not READY within the target's
// boost_start_timeout is FAILED, saying why; its instance goes and its
// room is free at once, so another box's boost is made under a budget
// for one; and the box's next boosted start replaces it.
func TestABoostThatDoesNotStartFails(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{
		BoostBudget:       &targetsv1alpha1.BoostSize{Cpu: "4", Memory: "4Gi"},
		BoostStartTimeout: durationpb.New(5 * time.Minute),
	})
	w.readyBox("a")
	w.readyBox("b", func(b *envv1alpha1.Box) { b.Spec.Workstream = "ws2" })
	w.fake.FailCheckouts(fmt.Errorf("the mirror does not answer: %w", provider.ErrUnavailable))
	first := w.start(boostOn("a", "true"))
	w.waitBoost("a", envv1alpha1.BoostPhase_BOOST_PHASE_STARTING)
	queued := w.start(boostOn("b", "true")).GetPending()
	if queued.GetReason() != envv1alpha1.PendingReason_PENDING_REASON_QUEUED_BUDGET {
		t.Fatalf("a second boost beside a starting one: %v", queued)
	}
	w.advance(4 * time.Minute)
	if b, _ := w.boost("a"); b.GetStatus().GetPhase() != envv1alpha1.BoostPhase_BOOST_PHASE_STARTING {
		t.Fatalf("a boost starting for 4 minutes of 5 is %v", b.GetStatus().GetPhase())
	}
	w.advance(2 * time.Minute)
	failed := w.waitBoost("a", envv1alpha1.BoostPhase_BOOST_PHASE_FAILED)
	if c := condition(failed.GetStatus().GetConditions(), "Ready"); c.GetReason() != "StartTimedOut" || !strings.Contains(c.GetMessage(), "5m0s") {
		t.Errorf("the timed out boost says %v", c)
	}
	testutil.Eventually(t, "the failed boost's instance gone", func() bool { _, ok := w.instance(failed.GetStatus().GetInstance()); return !ok })

	w.fake.FailCheckouts(nil)
	again := boostOn("b", "true")
	again.Pending = queued.GetTicket()
	if res := w.startBoosted(again); res.GetRanOn().GetKind() != envv1alpha1.RanOnKind_RAN_ON_KIND_BOOST {
		t.Errorf("the queued boost, once the failed one let its room go: %v", res)
	}
	// a's next start replaces its failed boost, once b's is gone.
	w.advance(11 * time.Minute)
	testutil.Eventually(t, "b's boost gone", func() bool { _, ok := w.boost("b"); return !ok })
	replace := boostOn("a", "true")
	replace.Pending = first.GetPending().GetTicket()
	res := w.startBoosted(replace)
	if nb, _ := w.boost("a"); nb.GetMetadata().GetUid() == failed.GetMetadata().GetUid() || res.GetProcess() == "" {
		t.Errorf("a's boost after its failure: %v, start %v", nb, res)
	}
}

// TestABoostWaitingForRoomSaysSo: a boost whose instance waits for room on
// its target's nodes answers PENDING_REASON_NODE_FULL, with the target's
// words and no estimate, not BOOST_STARTING; it keeps its room in
// boost_budget, so another box's boost is queued behind it; once there is
// room it starts by itself, and the start asked again runs on it. One
// still waiting at boost_start_timeout fails, saying what it waited for.
func TestABoostWaitingForRoomSaysSo(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{
		BoostBudget:       &targetsv1alpha1.BoostSize{Cpu: "4", Memory: "4Gi"},
		BoostStartTimeout: durationpb.New(5 * time.Minute),
	})
	w.readyBox("a")
	w.readyBox("b", func(b *envv1alpha1.Box) { b.Spec.Workstream = "ws2" })
	const full = "no ready node has 8Gi of memory free"
	w.fake.FillTarget(full)
	req := boostOn("a", "true")
	var p *envv1alpha1.PendingStart
	testutil.Eventually(t, "the boosted start pending NODE_FULL", func() (bool, string) {
		p = w.start(req).GetPending()
		req.Pending = p.GetTicket()
		return p.GetReason() == envv1alpha1.PendingReason_PENDING_REASON_NODE_FULL, p.String()
	})
	if !strings.Contains(p.GetMessage(), full) || !strings.Contains(p.GetMessage(), "starts by itself") || !strings.Contains(p.GetMessage(), "boost_start_timeout (5m0s)") ||
		p.GetEstimatedWait() != nil {
		t.Errorf("a boost waiting for room: %v", p)
	}
	if b, _ := w.boost("a"); condition(b.GetStatus().GetConditions(), "Ready").GetReason() != provider.ReasonWaitingForRoom {
		t.Errorf("the boost waiting for room is %v", b.GetStatus())
	}
	queued := w.start(boostOn("b", "true")).GetPending()
	if queued.GetReason() != envv1alpha1.PendingReason_PENDING_REASON_QUEUED_BUDGET {
		t.Fatalf("a second boost beside one waiting for room: %v", queued)
	}
	w.fake.FillTarget("")
	if res := w.startBoosted(req); res.GetRanOn().GetKind() != envv1alpha1.RanOnKind_RAN_ON_KIND_BOOST {
		t.Fatalf("the boost once there is room: %v", res)
	}

	// b's, admitted once a's is gone, waits past its timeout.
	w.fake.FillTarget(full)
	w.advance(11 * time.Minute)
	testutil.Eventually(t, "a's boost gone", func() bool { _, ok := w.boost("a"); return !ok })
	again := boostOn("b", "true")
	again.Pending = queued.GetTicket()
	testutil.Eventually(t, "b's boosted start pending NODE_FULL", func() (bool, string) {
		p := w.start(again).GetPending()
		return p.GetReason() == envv1alpha1.PendingReason_PENDING_REASON_NODE_FULL, p.String()
	})
	w.advance(6 * time.Minute)
	failed := w.waitBoost("b", envv1alpha1.BoostPhase_BOOST_PHASE_FAILED)
	if c := condition(failed.GetStatus().GetConditions(), "Ready"); c.GetReason() != "StartTimedOut" || !strings.Contains(c.GetMessage(), full) {
		t.Errorf("the boost that waited for room past its timeout says %v", c)
	}
}

// TestAFailedBoostFreesItsRoom: a boost whose instance fails to boot is
// FAILED at once, holding no room; one whose box failed fails too.
func TestAFailedBoostFreesItsRoom(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{BoostBudget: &targetsv1alpha1.BoostSize{Cpu: "4", Memory: "4Gi"}})
	w.readyBox("a")
	w.readyBox("b")
	w.fake.FailBoot("boost-a", "no room on the node")
	w.start(boostOn("a", "true"))
	failed := w.waitBoost("a", envv1alpha1.BoostPhase_BOOST_PHASE_FAILED)
	if c := condition(failed.GetStatus().GetConditions(), "Ready"); c.GetReason() != "InstanceFailed" || !strings.Contains(c.GetMessage(), "no room on the node") {
		t.Errorf("the boost whose instance did not boot says %v", c)
	}
	if res := w.startBoosted(boostOn("b", "true")); res.GetProcess() == "" {
		t.Errorf("a boost beside a FAILED one under a budget for one: %v", res)
	}

	// A boost of a box that failed.
	w.fake.FailBuilds(fmt.Errorf("npm ci exited 1: %w", provider.ErrFailed))
	bb := box("bad")
	bb.Spec.Image = "example.com/bad:1"
	w.mustApply(registry.Box, bb)
	bad := w.waitBox("bad", envv1alpha1.BoxPhase_BOX_PHASE_FAILED)
	w.mustApply(registry.Boost, &envv1alpha1.Boost{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: "bad", OwnerReferences: []*metav1.OwnerReference{{
			ApiVersion: registry.Box.APIVersion(), Kind: "Box", Name: "bad", Uid: bad.GetMetadata().GetUid(), Controller: true,
		}}},
		Spec: &envv1alpha1.BoostSpec{Box: "bad", Target: "t1", Resources: &envv1alpha1.Resources{Cpu: "2", Memory: "2Gi"}},
	})
	if c := condition(w.waitBoost("bad", envv1alpha1.BoostPhase_BOOST_PHASE_FAILED).GetStatus().GetConditions(), "Ready"); c.GetReason() != "BoxFailed" {
		t.Errorf("the boost of a failed box says %v", c)
	}
}

// TestABoostKeepsWhatItWasMadeOf: its status names its instance, alias
// and goldens from the instance there is, whatever its status lost.
func TestABoostKeepsWhatItWasMadeOf(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	box := w.readyBox("dev")
	w.startBoosted(boostOn("dev", "true"))
	b, _ := w.boost("dev")
	lost := proto.Clone(b).(*envv1alpha1.Boost)
	lost.Status.Instance, lost.Status.Alias, lost.Status.ImageGolden, lost.Status.WarmGolden = "", "", "", ""
	if _, err := w.asSystem().generic.UpdateStatus(t.Context(), registry.Boost, client.Key{Project: "web", Name: "dev"}, lost.GetStatus(), nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the boost's status made again from its instance", func() (bool, string) {
		got, _ := w.boost("dev")
		st := got.GetStatus()
		return st.GetInstance() == b.GetStatus().GetInstance() && st.GetAlias() == "dev-boost" &&
			st.GetImageGolden() == box.GetStatus().GetImageGolden() && st.GetWarmGolden() == box.GetStatus().GetWarmGolden(), st.String()
	})
}

// TestARetireRacingAStartLeavesTheBoost: a process started on an idle
// boost between its exits being read and its deletion being decided
// keeps it; the start is not lost to a boost deleted under it.
func TestARetireRacingAStartLeavesTheBoost(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{BoostIdle: durationpb.New(time.Minute)})
	w.readyBox("dev")
	w.startBoosted(boostOn("dev", "true"))
	b, _ := w.boost("dev")
	var once sync.Once
	started := make(chan *envv1alpha1.StartProcessResponse, 1)
	release := make(chan struct{})
	w.srv.SetBeforeBoostRetire(func(_, box string) {
		once.Do(func() {
			res, err := w.as(workstream).envs.StartProcess(context.Background(), connect.NewRequest(boostOn(box, "sleep")))
			if err != nil {
				t.Error(err)
				started <- nil
			} else {
				started <- res.Msg
			}
			// The retire goes on once the boost says the process runs, so
			// nothing writes the boost while its deletion is decided.
			<-release
		})
	})
	w.waitSettled()
	w.clock.Advance(2 * time.Minute)
	res := <-started
	if res.GetProcess() == "" {
		close(release)
		t.Fatalf("the start racing the retire: %v", res)
	}
	testutil.Eventually(t, "the boost says its new process runs", func() bool {
		b, _ := w.boost("dev")
		return b.GetStatus().GetProcessesRunning() == 1
	})
	close(release)
	w.waitSettled()
	nb, ok := w.boost("dev")
	if !ok || nb.GetMetadata().GetUid() != b.GetMetadata().GetUid() || nb.GetMetadata().GetDeleteTime() != nil {
		t.Fatalf("the boost a process just started on was deleted: %v", nb)
	}
	if p := w.listed("dev")[res.GetProcess()]; !p.GetRunning() {
		t.Errorf("the process started during the retire: %v", p)
	}
}

// TestAStartWaitingForItsBoostKeepsIt: a ready boost a pending start has
// not asked for again yet is not reclaimed for idleness while its ticket
// lives, so the start, asked again, runs on it.
func TestAStartWaitingForItsBoostKeepsIt(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{BoostIdle: durationpb.New(time.Minute)})
	w.readyBox("dev")
	first := w.start(boostOn("dev", "true"))
	b := w.waitBoost("dev", envv1alpha1.BoostPhase_BOOST_PHASE_READY)
	w.advance(3 * time.Minute)
	req := boostOn("dev", "true")
	req.Pending = first.GetPending().GetTicket()
	res := w.start(req)
	nb, _ := w.boost("dev")
	if res.GetProcess() == "" || nb.GetMetadata().GetUid() != b.GetMetadata().GetUid() {
		t.Errorf("asked again 3 minutes after its boost was READY, with boost_idle 1 minute: %v; the boost %v", res, nb.GetMetadata())
	}
}

// TestAKeptExitIsNotCountedAgain: a process kept from a boost gone is
// never recorded as a run from its record, which holds no command text:
// its usage was recorded before the boost went, or is lost.
func TestAKeptExitIsNotCountedAgain(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	box := w.readyBox("dev")
	rec := fmt.Sprintf(`{"boxUid":%q,"repository":"api","boost":[{"id":"boost-0123456789ab-p7","boostUid":"gone","instance":"boost-dev",
		"instanceUid":"gone","providerId":"p7","cpu":"8","memory":"8192Mi","coarse":"go test","started":"2026-10-29T00:00:00Z",
		"exit":{"Code":0,"Signal":0,"Usage":{"MemoryPeak":1024,"Wall":60000000000}},"gone":true}]}`, box.GetMetadata().GetUid())
	if _, err := w.store.Create(t.Context(), store.Key{Kind: "BoxProcesses", Project: "web", Name: "dev"}, []byte(rec)); err != nil {
		t.Fatal(err)
	}
	if p := w.listed("dev")["boost-0123456789ab-p7"]; p.GetExit().GetMemoryPeakBytes() != 1024 || p.GetCommand() != "go test" {
		t.Fatalf("the kept process listed: %v", p)
	}
	uses, all := w.commandUses()
	if len(uses) != 0 {
		t.Errorf("a kept exit was recorded as a run: %s", all)
	}
}

// TestTwoStartsRaceForTheLastRoom: two boosted starts at once, with room
// in the budget for one of them, make one boost; the other is queued.
func TestTwoStartsRaceForTheLastRoom(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{BoostBudget: &targetsv1alpha1.BoostSize{Cpu: "12", Memory: "12Gi"}})
	for _, name := range []string{"c", "a", "b"} {
		w.readyBox(name)
	}
	w.startBoosted(boostOf(boostOn("c", "true"), "4", "4Gi"))
	var wg sync.WaitGroup
	res := make([]*envv1alpha1.StartProcessResponse, 2)
	for i, name := range []string{"a", "b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := w.as(workstream).envs.StartProcess(t.Context(), connect.NewRequest(boostOf(boostOn(name, "true"), "8", "8Gi")))
			if err != nil {
				t.Error(err)
				return
			}
			res[i] = r.Msg
		}()
	}
	wg.Wait()
	reasons := map[envv1alpha1.PendingReason]int{}
	for _, r := range res {
		reasons[r.GetPending().GetReason()]++
	}
	_, a := w.boost("a")
	_, b := w.boost("b")
	if reasons[envv1alpha1.PendingReason_PENDING_REASON_BOOST_STARTING] != 1 || reasons[envv1alpha1.PendingReason_PENDING_REASON_QUEUED_BUDGET] != 1 || a == b {
		t.Errorf("two starts for the last room answered %v and %v; boosts made: a %v, b %v", res[0], res[1], a, b)
	}
}

// TestTheQueueTakesWorkstreamsInTurn: queued starts are admitted first
// come first served within a workstream and workstream by workstream in
// turn, the workstream admitted last going last; and an apiserver
// restarted, which loses the queue, queues a ticket it no longer knows
// again with a new one.
func TestTheQueueTakesWorkstreamsInTurn(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{BoostBudget: &targetsv1alpha1.BoostSize{Cpu: "1", Memory: "1Gi"}})
	in := func(ws string) func(*envv1alpha1.Box) { return func(b *envv1alpha1.Box) { b.Spec.Workstream = ws } }
	w.readyBox("h", in("ws3"))
	w.readyBox("a1", in("ws1"))
	w.readyBox("a2", in("ws1"))
	w.readyBox("b1", in("ws2"))
	w.startBoosted(boostOn("h", "true"))
	tickets := map[string]string{}
	for _, name := range []string{"a1", "a2", "b1"} {
		tickets[name] = w.start(boostOn(name, "true")).GetPending().GetTicket()
	}
	position := func(name string) int32 {
		req := boostOn(name, "true")
		req.Pending = tickets[name]
		p := w.start(req).GetPending()
		tickets[name] = p.GetTicket()
		return p.GetPosition()
	}
	if got := []int32{position("a1"), position("b1"), position("a2")}; got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("positions of a1, b1 and a2: %v, want 1, 2, 3: ws1 and ws2 in turn", got)
	}

	w.restart()
	req := boostOn("a2", "true")
	req.Pending = tickets["a2"]
	p := w.start(req).GetPending()
	if p.GetReason() != envv1alpha1.PendingReason_PENDING_REASON_QUEUED_BUDGET || p.GetTicket() == tickets["a2"] || p.GetPosition() != 1 {
		t.Errorf("a ticket asked again after a restart: %v, want queued anew, first, with a new ticket", p)
	}
	if _, ok := w.boost("h"); !ok {
		t.Error("a boost did not survive a restart")
	}
}

// TestABoostThatNeverFitsIsRefused: a boost larger than the Plan has left
// even with no boost held is refused at once, not queued.
func TestABoostThatNeverFitsIsRefused(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{BoostMax: &targetsv1alpha1.BoostSize{Cpu: "16", Memory: "64Gi"}})
	w.readyBox("dev")
	o, err := w.get(registry.Target, "", "t1")
	if err != nil {
		t.Fatal(err)
	}
	st := proto.Clone(o.(*targetsv1alpha1.Target).GetStatus()).(*targetsv1alpha1.TargetStatus)
	st.Capacity = &targetsv1alpha1.Capacity{MemoryFree: "48Gi", CpuFree: "16"}
	if _, err := w.asSystem().generic.UpdateStatus(t.Context(), registry.Target, client.Key{Name: "t1"}, st, nil); err != nil {
		t.Fatal(err)
	}
	_, err = w.as(workstream).envs.StartProcess(t.Context(), connect.NewRequest(boostOf(boostOn("dev", "true"), "16", "64Gi")))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "even with no boost") {
		t.Errorf("a boost past the Plan's quota: %v, want FailedPrecondition", err)
	}
}

// TestBoostsAreRefused: a boost is a hangar box's; workstream reads one
// and writes none; and a size is shaped to the target's max.
func TestBoostsAreRefused(t *testing.T) {
	t.Parallel()
	t.Run("on a hostd target", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		w.readyBox("dev")
		_, err := w.as(workstream).envs.StartProcess(t.Context(), connect.NewRequest(boostOn("dev", "true")))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("a boost on a hostd target: %v, want FailedPrecondition", err)
		}
	})
	t.Run("by role, and sized", func(t *testing.T) {
		t.Parallel()
		w := boostWorld(t, nil)
		w.readyBox("dev")
		res := w.startBoosted(boostOf(boostOn("dev", "true"), "32", "64Gi"))
		if r := res.GetRanOn().GetResources(); r.GetCpu() != "8" || r.GetMemory() != "16384Mi" {
			t.Errorf("a boost past boost_max is %v, want the max, 8 and 16Gi", r)
		}
		b, _ := w.boost("dev")
		if _, err := w.as(workstream).generic.Get(t.Context(), registry.Boost, client.Key{Project: "web", Name: "dev"}); err != nil {
			t.Errorf("workstream reading a boost: %v", err)
		}
		if err := applyAs(t, w.as(workstream), registry.Boost, b); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("workstream writing a boost: %v, want PermissionDenied", err)
		}
	})
	t.Run("settings out of shape", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.setup()
		tg := hangarTarget("t9")
		tg.Spec.Settings = &targetsv1alpha1.TargetSettings{BoostResources: &targetsv1alpha1.BoostSize{Cpu: "8", Memory: "2Gi"}}
		if err := applyAs(t, e.as(admin), registry.Target, tg); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("boost_resources of 8 vCPU and 2Gi: %v, want InvalidArgument", err)
		}
		tg.Spec.Settings = &targetsv1alpha1.TargetSettings{BoostMax: &targetsv1alpha1.BoostSize{Cpu: "4", Memory: "4Gi"}}
		if err := applyAs(t, e.as(admin), registry.Target, tg); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("boost_max below the default boost_resources: %v, want InvalidArgument", err)
		}
	})
}

// TestAnIdleBoostGoesOnlyOnAListing: an idle READY boost goes only once a
// listing of its processes shows nothing running; while its instance's
// listing fails, even as not found, it stays.
func TestAnIdleBoostGoesOnlyOnAListing(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{BoostIdle: durationpb.New(time.Minute)})
	w.readyBox("dev")
	w.startBoosted(boostOn("dev", "true"))
	b, _ := w.boost("dev")
	w.fake.FailListProcesses("boost-dev", fmt.Errorf("instance web/boost-dev: %w", provider.ErrNotFound))
	w.advance(2 * time.Minute)
	w.advance(2 * time.Minute)
	if nb, ok := w.boost("dev"); !ok || nb.GetMetadata().GetDeleteTime() != nil {
		t.Fatalf("an idle boost whose processes could not be listed was deleted: %v", nb)
	}
	w.fake.FailListProcesses("boost-dev", nil)
	w.advance(2 * time.Minute)
	testutil.Eventually(t, "the idle boost gone once listed", func() bool {
		nb, ok := w.boost("dev")
		return !ok || nb.GetMetadata().GetUid() != b.GetMetadata().GetUid()
	})
}

// TestABoostStepBacksOff: a boost's step that keeps waiting on its target
// is tried again less and less often, not at the controller's first
// retry each time.
func TestABoostStepBacksOff(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	w.fake.FailCheckouts(fmt.Errorf("the mirror does not answer: %w", provider.ErrUnavailable))
	w.start(boostOn("dev", "true"))
	w.waitBoost("dev", envv1alpha1.BoostPhase_BOOST_PHASE_STARTING)
	in := provider.Instance{Project: "web", Name: "boost-dev"}
	testutil.Eventually(t, "the boost's first checkout", func() bool { return w.fake.Checkouts(in) > 0 })
	before := w.fake.Checkouts(in)
	for range 120 {
		w.advance(500 * time.Millisecond)
	}
	// One minute: tries at about 0.1, 0.3, 0.7, 1.5, 3.1, 6.3, 12.7,
	// 25.5 and 51.1 seconds, not every 100ms.
	if n := w.fake.Checkouts(in) - before; n > 15 {
		t.Errorf("a step waiting on its target was tried %d times in a minute", n)
	}
}

// setPlan writes the target's report of its Plan as hangar makes it, at
// the world's now: cpu and memory left of a whole.
func (w *world) setPlan(cpuFree, memoryFree string, cpus int32, memoryTotal string) {
	w.t.Helper()
	o, err := w.get(registry.Target, "", "t1")
	if err != nil {
		w.t.Fatal(err)
	}
	st := proto.Clone(o.(*targetsv1alpha1.Target).GetStatus()).(*targetsv1alpha1.TargetStatus)
	st.Capacity = &targetsv1alpha1.Capacity{CpuFree: cpuFree, MemoryFree: memoryFree, Cpus: cpus, MemoryTotal: memoryTotal}
	st.LastReportTime = timestamppb.New(w.clock.Now())
	if _, err := w.asSystem().generic.UpdateStatus(w.t.Context(), registry.Target, client.Key{Name: "t1"}, st, nil); err != nil {
		w.t.Fatal(err)
	}
}

// ask is a start as workstream makes it, its error kept.
func (w *world) ask(req *envv1alpha1.StartProcessRequest) (*envv1alpha1.StartProcessResponse, error) {
	res, err := w.as(workstream).envs.StartProcess(w.t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// planWithOneBoost is a target whose Plan has 12 of 24 vCPU left, boxes a
// and b ready, and a's boost of 8 vCPU and 8Gi admitted; a's ticket.
func planWithOneBoost(t *testing.T) (*world, *envv1alpha1.StartProcessRequest) {
	t.Helper()
	w := boostWorld(t, &targetsv1alpha1.TargetSettings{BoostBudget: &targetsv1alpha1.BoostSize{Cpu: "32", Memory: "64Gi"}})
	w.readyBox("a")
	w.readyBox("b")
	w.setPlan("12", "44Gi", 24, "64Gi")
	req := boostOf(boostOn("a", "true"), "8", "8Gi")
	first := w.start(req)
	if first.GetPending().GetReason() != envv1alpha1.PendingReason_PENDING_REASON_BOOST_STARTING {
		t.Fatalf("a's start on a Plan with room: %v, want its boost made", first)
	}
	req.Pending = first.GetPending().GetTicket()
	return w, req
}

// TestABoostTheReportCountsBeforeItBootsHoldsItsRoom: hangar counts a
// boost in its Plan's usage from when it is made, before tower knows it
// booted; its box's start asked again starts on it, and another box's
// queues: neither is refused as never fitting, though the Plan has less
// left than a boost.
func TestABoostTheReportCountsBeforeItBootsHoldsItsRoom(t *testing.T) {
	t.Parallel()
	w, a := planWithOneBoost(t)
	// Reported as hangar counts it: a's boost made, not booted before.
	w.setPlan("4", "36Gi", 24, "64Gi")
	res, err := w.ask(a)
	if err != nil {
		t.Fatalf("a's start asked again, its boost made: %v", err)
	}
	a.Pending = res.GetPending().GetTicket()
	if res := w.startBoosted(a); res.GetProcess() == "" {
		t.Errorf("a's start: %v", res)
	}
	res, err = w.ask(boostOf(boostOn("b", "true"), "8", "8Gi"))
	if err != nil || res.GetPending().GetReason() != envv1alpha1.PendingReason_PENDING_REASON_QUEUED_BUDGET {
		t.Errorf("b's start beside a's boost: %v, %v; want queued", res, err)
	}
}

// TestABoostTheReportCaughtUpWithHoldsItsRoom: as above, once the
// report is made after the boost booted.
func TestABoostTheReportCaughtUpWithHoldsItsRoom(t *testing.T) {
	t.Parallel()
	w, a := planWithOneBoost(t)
	w.waitBoost("a", envv1alpha1.BoostPhase_BOOST_PHASE_READY)
	w.advance(time.Minute)
	w.setPlan("4", "36Gi", 24, "64Gi")
	if res := w.startBoosted(a); res.GetProcess() == "" {
		t.Errorf("a's start asked again: %v", res)
	}
	res, err := w.ask(boostOf(boostOn("b", "true"), "8", "8Gi"))
	if err != nil || res.GetPending().GetReason() != envv1alpha1.PendingReason_PENDING_REASON_QUEUED_BUDGET {
		t.Errorf("b's start beside a's boost: %v, %v; want queued", res, err)
	}
}

// TestABoostMadeHoldsItsRoomWhateverThePlan: a box whose boost is made,
// of a size that suffices, starts on it whatever its Plan says since, a
// Plan shrunk below one boost too; another box's start is refused.
func TestABoostMadeHoldsItsRoomWhateverThePlan(t *testing.T) {
	t.Parallel()
	w, a := planWithOneBoost(t)
	w.setPlan("0", "0", 4, "64Gi")
	res, err := w.ask(a)
	if err != nil {
		t.Fatalf("a's start asked again, its ticket admitted: %v", err)
	}
	a.Pending = res.GetPending().GetTicket()
	if res := w.startBoosted(a); res.GetProcess() == "" {
		t.Errorf("a's start: %v", res)
	}
	if res := w.startBoosted(boostOf(boostOn("a", "true"), "8", "8Gi")); res.GetProcess() == "" {
		t.Errorf("a's next start, on its boost: %v", res)
	}
	if _, err := w.ask(boostOf(boostOn("b", "true"), "8", "8Gi")); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "even with no boost") {
		t.Errorf("b's start on a Plan of 4 vCPU: %v, want refused", err)
	}
}
