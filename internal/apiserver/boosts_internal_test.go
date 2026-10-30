package apiserver

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
)

// TestTheBoostQueueTakesWorkstreamsInTurn: first come first served within
// a workstream, workstreams in turn, the one admitted last going last;
// a ticket not asked about for five minutes is dropped.
func TestTheBoostQueueTakesWorkstreamsInTurn(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	tk := func(id, ws string, at int) *boostTicket {
		return &boostTicket{id: id, workstream: ws, box: controller.Key{Project: "p", Name: id}, arrived: t0.Add(time.Duration(at) * time.Second), seen: t0.Add(time.Duration(at) * time.Second)}
	}
	q := &targetBoosts{tickets: map[string]*boostTicket{}}
	for _, x := range []*boostTicket{tk("a1", "ws1", 1), tk("a2", "ws1", 2), tk("a3", "ws1", 3), tk("b1", "ws2", 4), tk("c1", "ws3", 5)} {
		q.tickets[x.id] = x
	}
	ids := func() (out []string) {
		for _, x := range q.order() {
			out = append(out, x.id)
		}
		return out
	}
	for _, tc := range []struct {
		last string
		want []string
	}{
		{"", []string{"a1", "b1", "c1", "a2", "a3"}},
		{"ws1", []string{"b1", "c1", "a1", "a2", "a3"}},
		{"ws2", []string{"a1", "c1", "b1", "a2", "a3"}},
	} {
		q.last = tc.last
		if got := ids(); !equal(got, tc.want) {
			t.Errorf("last admitted %q: order %v, want %v", tc.last, got, tc.want)
		}
	}
	q.tickets["a1"].admitted, q.last = true, "ws1"
	if got := ids(); !equal(got, []string{"b1", "c1", "a2", "a3"}) {
		t.Errorf("with a1 admitted: %v", got)
	}
	q.tickets["b1"].seen = t0.Add(10 * time.Minute)
	q.prune(t0.Add(5*time.Minute + 30*time.Second))
	if got := ids(); !equal(got, []string{"b1"}) {
		t.Errorf("after five minutes unasked, the queue holds %v, want b1 alone", got)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBoostSizes: a size is held to the max, then its vCPUs moved into
// hangar's shapes; the room a target has lets one boost in whatever the
// budget, and a Plan's quota left caps it.
func TestBoostSizes(t *testing.T) {
	const gi = 1 << 30
	most := boostSize{milliCPU: 8000, memory: 16 * gi}
	for _, tc := range []struct{ in, want boostSize }{
		{boostSize{8000, 8 * gi}, boostSize{8000, 8 * gi}},
		{boostSize{32000, 64 * gi}, boostSize{8000, 16 * gi}},
		{boostSize{8000, 2 * gi}, boostSize{4000, 2 * gi}},       // at most 4 vCPU for 2Gi
		{boostSize{1000, 16 * gi}, boostSize{2000, 16 * gi}},     // at least 2 vCPU for 16Gi
		{boostSize{2000, 512 << 20}, boostSize{1000, 512 << 20}}, // one vCPU always
	} {
		got := tc.in.shaped(most)
		if got != tc.want || got.shapeError() != nil {
			t.Errorf("%v shaped is %v (%v), want %v", tc.in, got, got.shapeError(), tc.want)
		}
	}
	r := boostRoom{budget: boostSize{4000, 4 * gi}}
	big := boostSize{8000, 8 * gi}
	if !r.fits(big) {
		t.Error("a target holding no boost refused one past its budget")
	}
	r.take(big)
	if r.fits(boostSize{1000, gi}) {
		t.Error("a target over its budget took another boost")
	}
	r = boostRoom{budget: boostSize{16000, 16 * gi}, quota: boostSize{0, 4 * gi}, hasMemory: true}
	if r.fits(big) || !r.fits(boostSize{2000, 4 * gi}) {
		t.Error("the Plan's memory left does not cap a boost")
	}
}

// TestTheRoomOfATarget: a boost holds budget unless FAILED; it counts
// against the Plan's quota left until its instance runs, which the
// target's report counts from then on; and with no boost held, the
// quota left is the report's plus what its running boosts hold.
func TestTheRoomOfATarget(t *testing.T) {
	const gi = 1 << 30
	tg := &targetsv1alpha1.Target{
		Spec: &targetsv1alpha1.TargetSpec{Settings: &targetsv1alpha1.TargetSettings{
			BoostBudget: &targetsv1alpha1.BoostSize{Cpu: "32", Memory: "32Gi"},
		}},
		Status: &targetsv1alpha1.TargetStatus{Capacity: &targetsv1alpha1.Capacity{MemoryFree: "20Gi", CpuFree: "20"}},
	}
	boost := func(uid string, phase envv1alpha1.BoostPhase) *envv1alpha1.Boost {
		return &envv1alpha1.Boost{
			Metadata: &metav1.ObjectMeta{Uid: uid},
			Spec:     &envv1alpha1.BoostSpec{Resources: &envv1alpha1.Resources{Cpu: "4", Memory: "4Gi"}},
			Status:   &envv1alpha1.BoostStatus{Phase: phase},
		}
	}
	v := boostsNow{
		boosts: []*envv1alpha1.Boost{
			boost("running", envv1alpha1.BoostPhase_BOOST_PHASE_READY),
			boost("starting", envv1alpha1.BoostPhase_BOOST_PHASE_STARTING),
			boost("failed", envv1alpha1.BoostPhase_BOOST_PHASE_FAILED),
		},
		counted: map[string]bool{"running": true, "failed": true},
	}
	r := roomOf(tg, v)
	if r.held != (boostSize{8000, 8 * gi}) {
		t.Errorf("held %v, want the running and starting boosts, 8 vCPU and 8Gi", r.held)
	}
	if !r.hasCPU || !r.hasMemory || r.quota != (boostSize{16000, 16 * gi}) {
		t.Errorf("quota left %v, want the report's less the starting boost, 16 vCPU and 16Gi", r.quota)
	}
	if !r.fits(boostSize{16000, 16 * gi}) || r.fits(boostSize{16000, 17 * gi}) {
		t.Error("the quota left does not bound a boost")
	}
	e := emptyRoomOf(tg, v)
	if e.held != (boostSize{}) || e.quota != (boostSize{28000, 28 * gi}) {
		t.Errorf("with no boost held: %v held, %v left; want none, and the report's plus the counted boosts, 28 vCPU and 28Gi", e.held, e.quota)
	}
	// One the report may count, made though not booted, is added back
	// too, up to the Plan's whole.
	v.mayCount = map[string]bool{"starting": true}
	if e := emptyRoomOf(tg, v); e.quota != (boostSize{32000, 32 * gi}) {
		t.Errorf("with no boost held, one made but not booted: %v left, want 32 vCPU and 32Gi", e.quota)
	}
	whole := proto.Clone(tg).(*targetsv1alpha1.Target)
	whole.Status.Capacity.Cpus, whole.Status.Capacity.MemoryTotal = 30, "24Gi"
	if e := emptyRoomOf(whole, v); e.quota != (boostSize{30000, 24 * gi}) {
		t.Errorf("with no boost held, on a Plan of 30 vCPU and 24Gi: %v left, want its whole", e.quota)
	}
	if r := roomOf(&targetsv1alpha1.Target{}, v); r.hasCPU || r.hasMemory || !r.fits(boostSize{8000, 8 * gi}) {
		t.Error("a target that reports no quota bounds a boost by it")
	}
}

// TestTheReportCountsWhatRanBeforeIt: the target's report of its Plan's
// usage counts a boost's instance only if it ran before the report.
func TestTheReportCountsWhatRanBeforeIt(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	in := func(phase envv1alpha1.InstancePhase, started *timestamppb.Timestamp) *envv1alpha1.Instance {
		return &envv1alpha1.Instance{Status: &envv1alpha1.InstanceStatus{Phase: phase, StartTime: started}}
	}
	running := envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING
	report := timestamppb.New(t0)
	for _, tc := range []struct {
		what string
		in   *envv1alpha1.Instance
		at   *timestamppb.Timestamp
		want bool
	}{
		{"running before the report", in(running, timestamppb.New(t0.Add(-time.Minute))), report, true},
		{"running since the report", in(running, timestamppb.New(t0.Add(time.Second))), report, false},
		{"running, its start not known", in(running, nil), report, false},
		{"pending", in(envv1alpha1.InstancePhase_INSTANCE_PHASE_PENDING, timestamppb.New(t0.Add(-time.Minute))), report, false},
		{"no report yet", in(running, timestamppb.New(t0.Add(-time.Minute))), nil, false},
	} {
		if got := countedByReport(tc.in, tc.at); got != tc.want {
			t.Errorf("%s: counted %v, want %v", tc.what, got, tc.want)
		}
	}
}

// TestOnlyTheLastProcessesKeepTheirChangedFiles: a box's processes past
// the last ten keep how many paths were written back, not the list.
func TestOnlyTheLastProcessesKeepTheirChangedFiles(t *testing.T) {
	var r boxProcesses
	for i := range 12 {
		r.add(boostProcess{ID: string(rune('a' + i)), Run: &runResult{Back: &writeBack{Total: 1, Moved: []guestbin.Moved{{Path: "x", Status: "M"}}}}})
	}
	for i, p := range r.Boost {
		if keep := i >= 2; (p.Run.Back.Moved != nil) != keep || p.Run.Back.Total != 1 {
			t.Errorf("process %d of 12: %+v", i, p.Run)
		}
	}
}
