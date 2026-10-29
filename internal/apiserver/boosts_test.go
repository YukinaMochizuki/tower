package apiserver_test

import (
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
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
// deletes its boost.
func TestAStoppedBoxLosesItsBoost(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	w.startBoosted(boostOn("dev", "true"))
	b := proto.Clone(w.box("dev")).(*envv1alpha1.Box)
	b.Spec.RunState = envv1alpha1.RunState_RUN_STATE_STOPPED
	w.mustApply(registry.Box, b)
	testutil.Eventually(t, "the stopped box's boost gone", func() bool { _, ok := w.boost("dev"); return !ok })
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
