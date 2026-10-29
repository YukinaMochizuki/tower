package apiserver

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/authz"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// A boost's conditions, all the boost controller's.
const (
	BoostReady        = "Ready"
	BoostMaterialized = "Materialized"
	BoostSetUp        = "SetUp"
)

const (
	// boostExitsFinalizer holds a boost being deleted until the exits of
	// its processes are kept (finishBoost), whoever deleted it.
	boostExitsFinalizer = registry.Domain + "/boost-exits"
	// boostImageGoldenAnnotation names, on a boost's instance, the image
	// golden it was made of.
	boostImageGoldenAnnotation = registry.Domain + "/image-golden"
	// boostTicketTTL is how long a pending start's ticket is kept unless
	// asked about again: a caller gone away holds no place.
	boostTicketTTL = 5 * time.Minute
	// boostResync is how often every boost is looked at again, which is
	// how soon after boost_idle an idle one goes.
	boostResync = 30 * time.Second
	// boostStartEstimate is about how long a boost takes from its
	// admission to READY: an instance cloned from a warm golden starts in
	// about 15 seconds, then the box's checkouts and setup steps run.
	boostStartEstimate = 30 * time.Second
	// boostUseWriteEvery bounds how often a running boost's last_use_time
	// is written, so a busy boost does not move the store revision at
	// each reconcile.
	boostUseWriteEvery = time.Minute
)

// boostTicket is a boosted start not started yet: queued for room in its
// target's budget, or waiting for its box's boost to be READY.
type boostTicket struct {
	id         string
	box        controller.Key
	workstream string
	size       boostSize
	// arrived orders the queue, arrival numbering it in the order made;
	// seen is when it was last asked about.
	arrived, seen time.Time
	arrival       uint64
	// admitted says its box's boost is made for it: it waits for the
	// boost, no longer for room.
	admitted bool
	// sync is the sync made for it once its boost was READY, if any.
	sync *syncRun
}

// boostQueues are each target's boosted starts not started yet, and what
// keeps its boosts in use, in memory: an apiserver restarted loses them,
// and a caller asking with a ticket it no longer knows is queued again
// (docs/decisions/boost.md).
type boostQueues struct {
	mu      sync.Mutex
	targets map[string]*targetBoosts
}

// targetBoosts is one target's queue. Its mu is held from deciding that a
// boost fits, or that one may go, to the write that makes or deletes it,
// so two starts never both take the budget's last room and a boost never
// goes while a process is being started on it.
type targetBoosts struct {
	mu      sync.Mutex
	tickets map[string]*boostTicket
	// last is the workstream admitted last, whose turn comes last.
	last string
	// arrivals numbers the tickets in the order made.
	arrivals uint64
	// starting counts each box's processes being started on its boost.
	starting map[controller.Key]int
	// used is when a process last started on each box's boost, and starts
	// counts them: a retire sees a start since it read the boost's exits.
	used   map[controller.Key]time.Time
	starts map[controller.Key]uint64
	// syncs holds each box's syncs to its boost one at a time.
	syncs map[controller.Key]*sync.Mutex
}

// syncLock is the box's sync lock. Held mu.
func (t *targetBoosts) syncLock(box controller.Key) *sync.Mutex {
	if t.syncs == nil {
		t.syncs = map[controller.Key]*sync.Mutex{}
	}
	mu, ok := t.syncs[box]
	if !ok {
		mu = &sync.Mutex{}
		t.syncs[box] = mu
	}
	return mu
}

func (q *boostQueues) of(target string) *targetBoosts {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.targets == nil {
		q.targets = map[string]*targetBoosts{}
	}
	t, ok := q.targets[target]
	if !ok {
		t = &targetBoosts{tickets: map[string]*boostTicket{}, starting: map[controller.Key]int{}, used: map[controller.Key]time.Time{}, starts: map[controller.Key]uint64{}}
		q.targets[target] = t
	}
	return t
}

// prune drops the tickets not asked about for boostTicketTTL. Held mu.
func (t *targetBoosts) prune(now time.Time) {
	for id, tk := range t.tickets {
		if now.Sub(tk.seen) > boostTicketTTL {
			delete(t.tickets, id)
		}
	}
}

// busy says the box's boost is wanted: a process is being started on it,
// or a live ticket waits for it. Held mu.
func (t *targetBoosts) busy(box controller.Key, now time.Time) bool {
	if t.starting[box] > 0 {
		return true
	}
	for _, tk := range t.tickets {
		if tk.box == box && tk.admitted && now.Sub(tk.seen) <= boostTicketTTL {
			return true
		}
	}
	return false
}

// order is the queued tickets in the order they are admitted: first come
// first served, workstream by workstream in turn, starting after the one
// admitted last. Held mu.
func (t *targetBoosts) order() []*boostTicket {
	byWS := map[string][]*boostTicket{}
	for _, tk := range t.tickets {
		if !tk.admitted {
			byWS[tk.workstream] = append(byWS[tk.workstream], tk)
		}
	}
	first := func(a, b *boostTicket) int {
		return cmp.Or(a.arrived.Compare(b.arrived), cmp.Compare(a.arrival, b.arrival))
	}
	var wss []string
	for ws, tks := range byWS {
		slices.SortFunc(tks, first)
		wss = append(wss, ws)
	}
	slices.SortFunc(wss, func(a, b string) int {
		// The one admitted last goes last in the first round.
		if (a == t.last) != (b == t.last) {
			if a == t.last {
				return 1
			}
			return -1
		}
		return first(byWS[a][0], byWS[b][0])
	})
	var out []*boostTicket
	for round := 0; ; round++ {
		added := false
		for _, ws := range wss {
			if round < len(byWS[ws]) {
				out, added = append(out, byWS[ws][round]), true
			}
		}
		if !added {
			return out
		}
	}
}

func newTicketID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "t-" + hex.EncodeToString(b)
}

// boostAsk is the size a boost intent asks for, as the target's settings
// shape it.
func boostAsk(in *envv1alpha1.BoostIntent, set settings) (boostSize, error) {
	r := in.GetResources()
	if r == nil {
		return set.boostResources, nil
	}
	if r.GetDisk() != "" {
		return boostSize{}, invalid("boost.resources.disk: a boost's disk is its box's")
	}
	size, err := parseBoostSize(r.GetCpu(), r.GetMemory(), set.boostResources)
	if err != nil {
		return boostSize{}, invalid("boost.resources: %v", err)
	}
	return size.shaped(set.boostMax), nil
}

// boostRoom is what a target has for boosts: held by its boosts now, and
// what its Plan has left, which counts the boosts running already.
type boostRoom struct {
	budget, held boostSize
	// quota is what the Plan has left of each, as the target reports it,
	// less the boosts it does not count yet; hasCPU and hasMemory say it is
	// known.
	quota             boostSize
	hasCPU, hasMemory bool
}

// fits says a boost of size fits: within the budget, or the target holds
// no boost (one boost always may, so a budget set below one boost lets
// one at a time), and within the Plan's quota left.
func (r boostRoom) fits(size boostSize) bool {
	if r.held != (boostSize{}) && !r.budget.covers(r.held.plus(size)) {
		return false
	}
	if r.hasCPU && r.quota.milliCPU < size.milliCPU {
		return false
	}
	return !r.hasMemory || r.quota.memory >= size.memory
}

func (r *boostRoom) take(size boostSize) {
	r.held, r.quota = r.held.plus(size), r.quota.minus(size)
}

func (r *boostRoom) give(size boostSize) {
	r.held, r.quota = r.held.minus(size), r.quota.plus(size)
}

// boostsNow are the boosts on a target as admission reads them, and
// which of them its Plan's last report counts (countedByReport).
type boostsNow struct {
	boosts  []*envv1alpha1.Boost
	counted map[string]bool
}

// holds says b holds room: a FAILED boost's instance is let go, and its
// room with it.
func holds(b *envv1alpha1.Boost) bool {
	return b.GetStatus().GetPhase() != envv1alpha1.BoostPhase_BOOST_PHASE_FAILED
}

// boostsOnTargetNow lists the boosts on t, and which its Plan's last
// report counts.
func (s *Server) boostsOnTargetNow(ctx context.Context, t *targetsv1alpha1.Target) (boostsNow, error) {
	target, reported := t.GetMetadata().GetName(), t.GetStatus().GetLastReportTime()
	all, err := listTyped[*envv1alpha1.Boost](ctx, s, registry.Boost, "")
	if err != nil {
		return boostsNow{}, err
	}
	v := boostsNow{counted: map[string]bool{}}
	for _, b := range all {
		if b.GetSpec().GetTarget() != target {
			continue
		}
		v.boosts = append(v.boosts, b)
		in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: b.GetMetadata().GetProject(), Name: boostInstanceName(b.GetMetadata().GetName())})
		if err != nil {
			return boostsNow{}, err
		}
		v.counted[b.GetMetadata().GetUid()] = ok && owns(registry.Boost, b, in) && countedByReport(in, reported)
	}
	return v, nil
}

// countedByReport says the target's report of its Plan's usage, made at
// reported, counts the instance in: hangar counts a machine's cpu and
// memory once it runs, so the instance ran before the report was made.
// One that started since, or whose start is not known, is not counted
// yet.
func countedByReport(in *envv1alpha1.Instance, reported *timestamppb.Timestamp) bool {
	started := in.GetStatus().GetStartTime()
	return in.GetStatus().GetPhase() == envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING &&
		started != nil && reported != nil && started.AsTime().Before(reported.AsTime())
}

// quotaOf is the Plan's quota left as t reports it.
func quotaOf(t *targetsv1alpha1.Target) boostRoom {
	r := boostRoom{budget: settingsOf(t).boostBudget}
	capacity := t.GetStatus().GetCapacity()
	if v := capacity.GetCpuFree(); v != "" {
		if m, err := parseMilliCPU(v); err == nil {
			r.quota.milliCPU, r.hasCPU = m, true
		}
	}
	if v := capacity.GetMemoryFree(); v != "" {
		if b, err := quantity.Bytes(v); err == nil {
			r.quota.memory, r.hasMemory = b, true
		}
	}
	return r
}

// roomOf is what t has for boosts beside v's: each that holds room counts
// against the budget, and against the quota unless the report counts it
// already.
func roomOf(t *targetsv1alpha1.Target, v boostsNow) boostRoom {
	r := quotaOf(t)
	for _, b := range v.boosts {
		if !holds(b) {
			continue
		}
		size := boostSizeOf(b.GetSpec().GetResources())
		r.held = r.held.plus(size)
		if !v.counted[b.GetMetadata().GetUid()] {
			r.quota = r.quota.minus(size)
		}
	}
	return r
}

// emptyRoomOf is what t would have with none of v's boosts: what a boost
// that does not fit even then waits for in vain.
func emptyRoomOf(t *targetsv1alpha1.Target, v boostsNow) boostRoom {
	r := quotaOf(t)
	for _, b := range v.boosts {
		if v.counted[b.GetMetadata().GetUid()] {
			r.quota = r.quota.plus(boostSizeOf(b.GetSpec().GetResources()))
		}
	}
	return r
}

// admitBoosts makes the boosts of the target's queued tickets that fit,
// in their order, and stops at the first that does not: one large boost
// is not passed by smaller ones forever. A workstream's turn is spent
// only on a boost made. Held tq.mu.
func (s *Server) admitBoosts(ctx context.Context, t *targetsv1alpha1.Target, tq *targetBoosts) error {
	now := s.clock.Now()
	tq.prune(now)
	order := tq.order()
	if len(order) == 0 {
		return nil
	}
	target := t.GetMetadata().GetName()
	v, err := s.boostsOnTargetNow(ctx, t)
	if err != nil {
		return err
	}
	room := roomOf(t, v)
	for _, tk := range order {
		if slices.ContainsFunc(v.boosts, func(b *envv1alpha1.Boost) bool {
			return b.GetMetadata().GetProject() == tk.box.Project && b.GetMetadata().GetName() == tk.box.Name
		}) {
			// Its box's boost, too small or failed, is going: admitted once
			// gone.
			continue
		}
		if !room.fits(tk.size) {
			return nil
		}
		made, gone, err := s.makeBoost(ctx, target, tk)
		if err != nil {
			return err
		}
		switch {
		case gone:
			delete(tq.tickets, tk.id)
		case made:
			room.take(tk.size)
			tk.admitted, tq.last = true, tk.workstream
		default:
			// Made since it was listed, by another start of its box.
			tk.admitted = true
		}
	}
	return nil
}

// makeBoost makes the Boost of tk's box: made, or gone when its box is.
func (s *Server) makeBoost(ctx context.Context, target string, tk *boostTicket) (made, gone bool, err error) {
	box, ok, err := getTyped[*envv1alpha1.Box](ctx, s, registry.Box, resource.Key{Project: tk.box.Project, Name: tk.box.Name})
	if err != nil {
		return false, false, err
	}
	if !ok || box.GetMetadata().GetDeleteTime() != nil {
		return false, true, nil
	}
	_, err = s.backend(registry.Boost).Create(s.system(ctx), &envv1alpha1.Boost{
		Metadata: &metav1.ObjectMeta{Project: tk.box.Project, Name: tk.box.Name, OwnerReferences: []*metav1.OwnerReference{ownedBy(registry.Box, box)}},
		Spec:     &envv1alpha1.BoostSpec{Box: tk.box.Name, Resources: tk.size.resources(), Target: target},
	})
	if connect.CodeOf(err) == connect.CodeAlreadyExists {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	s.log.Info("making a boost", "project", tk.box.Project, "box", tk.box.Name, "target", target, "size", tk.size.String(), "workstream", tk.workstream)
	return true, false, nil
}

// estimateWait is about how long until tk's boost is made, as far as
// what holds the target's room now says: each boost going boost_idle
// after its last use (a running one at least that long from now), the
// tickets before tk taking their room first. Held tq.mu.
func (s *Server) estimateWait(t *targetsv1alpha1.Target, tq *targetBoosts, v boostsNow, order []*boostTicket, tk *boostTicket) time.Duration {
	now, idle := s.clock.Now(), settingsOf(t).boostIdle
	type freeing struct {
		at   time.Time
		size boostSize
	}
	var frees []freeing
	for _, b := range v.boosts {
		if !holds(b) {
			continue
		}
		at := now
		if lu := b.GetStatus().GetLastUseTime(); lu != nil && lu.AsTime().After(at) {
			at = lu.AsTime()
		}
		key := controller.Key{Project: b.GetMetadata().GetProject(), Name: b.GetMetadata().GetName()}
		if u := tq.used[key]; u.After(at) {
			at = u
		}
		frees = append(frees, freeing{at: at.Add(idle), size: boostSizeOf(b.GetSpec().GetResources())})
	}
	slices.SortFunc(frees, func(a, b freeing) int { return a.at.Compare(b.at) })
	room, when := roomOf(t, v), now
	for _, o := range order {
		for !room.fits(o.size) && len(frees) > 0 {
			room.give(frees[0].size)
			when = maxTime(when, frees[0].at)
			frees = frees[1:]
		}
		room.take(o.size)
		if o == tk {
			break
		}
	}
	return when.Sub(now) + boostStartEstimate
}

// boostsOnTarget maps a Target to the boosts on it, whose settings it
// holds.
func (s *Server) boostsOnTarget(ctx context.Context, o store.Object) []controller.Key {
	all, err := listTyped[*envv1alpha1.Boost](ctx, s, registry.Boost, "")
	if err != nil {
		return nil
	}
	var keys []controller.Key
	for _, b := range all {
		if b.GetSpec().GetTarget() != o.Name {
			continue
		}
		keys = append(keys, controller.Key{Project: b.GetMetadata().GetProject(), Name: b.GetMetadata().GetName()})
	}
	return keys
}

// validateBoost checks a boost, which only tower writes.
func validateBoost(_ context.Context, _ *Server, _ authz.Caller, _, obj proto.Message) error {
	b := obj.(*envv1alpha1.Boost)
	if b.GetSpec().GetBox() != b.GetMetadata().GetName() {
		return invalid("spec.box is the boost's own name")
	}
	if b.GetSpec().GetTarget() == "" {
		return invalid("spec.target is required")
	}
	size, err := parseBoostSize(b.GetSpec().GetResources().GetCpu(), b.GetSpec().GetResources().GetMemory(), boostSize{})
	if err == nil {
		err = size.shapeError()
	}
	if err != nil {
		return invalid("spec.resources: %v", err)
	}
	return nil
}

// boostActivity is what keeps the box in use on its boost: its processes
// running there, and a start being made or waiting for it.
func (s *Server) boostActivity(ctx context.Context, box controller.Key) (int32, error) {
	b, ok, err := getTyped[*envv1alpha1.Boost](ctx, s, registry.Boost, resource.Key{Project: box.Project, Name: box.Name})
	if err != nil || !ok {
		return 0, err
	}
	n := b.GetStatus().GetProcessesRunning()
	tq := s.boosts.of(b.GetSpec().GetTarget())
	tq.mu.Lock()
	defer tq.mu.Unlock()
	if tq.busy(box, s.clock.Now()) {
		n++
	}
	return n, nil
}

// boostAlias is a boost's name on its workstream's network.
func boostAlias(box string) string {
	n := box + "-boost"
	if len(n) <= 63 {
		return n
	}
	return childName("boost", box)
}

// boostInstanceName is the name of a boost's Instance.
func boostInstanceName(box string) string { return childName("boost", box) }

// boostState is the status a reconcile of a boost builds, written once.
type boostState struct {
	s  *Server
	st *envv1alpha1.BoostStatus
}

func (b boostState) cond(typ string, ok bool, reason, msg string) {
	c := &metav1.Condition{Type: typ, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg, ObservedGeneration: b.st.GetObservedGeneration()}
	if ok {
		c.Status = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	}
	b.st.Conditions = conditions.Set(slices.Clone(b.st.GetConditions()), c, b.s.clock.Now())
}

func (b boostState) not(phase envv1alpha1.BoostPhase, reason, msg string) {
	b.st.Phase = phase
	b.cond(BoostReady, false, reason, msg)
}

// boostRetire is why a boost is to go, if it is: why, and whether it
// goes whatever runs on it (its box stopped), its processes then lost.
type boostRetire struct {
	why   string
	force bool
}

// reconcileBoost brings a boost along: its instance made as its box's is,
// the box's secret files, checkouts and setup steps in it, then READY.
// One not READY within boost_start_timeout, or whose box failed, is
// FAILED, its instance deleted and its room freed. It is deleted once
// nothing has run on it for boost_idle, or once its box is stopped at
// workstream's request; and, deleted, its processes' exits are kept
// before it goes (finishBoost).
func (s *Server) reconcileBoost(ctx context.Context, key controller.Key) error {
	b, ok, err := getTyped[*envv1alpha1.Boost](ctx, s, registry.Boost, resource.Key{Project: key.Project, Name: key.Name})
	if err != nil || !ok {
		return err
	}
	if b.GetMetadata().GetDeleteTime() != nil {
		return s.finishBoost(key, b)
	}
	box, ok, err := getTyped[*envv1alpha1.Box](ctx, s, registry.Box, resource.Key{Project: key.Project, Name: b.GetSpec().GetBox()})
	if err != nil || !ok || box.GetMetadata().GetDeleteTime() != nil || !owns(registry.Box, box, b) {
		// Garbage collection deletes it with its box.
		return err
	}
	st := proto.Clone(b.GetStatus()).(*envv1alpha1.BoostStatus)
	if st == nil {
		st = &envv1alpha1.BoostStatus{}
	}
	st.ObservedGeneration = b.GetMetadata().GetGeneration()
	if st.Phase == envv1alpha1.BoostPhase_BOOST_PHASE_UNSPECIFIED {
		st.Phase = envv1alpha1.BoostPhase_BOOST_PHASE_PENDING
	}
	if st.LastUseTime == nil {
		st.LastUseTime = b.GetMetadata().GetCreateTime()
	}
	bs := &boostState{s: s, st: st}
	retire, waitErr := s.boostSteps(ctx, key, b, box, bs)
	if err := s.updateStatus(ctx, registry.Boost, b, st); err != nil {
		return err
	}
	if retire.why != "" {
		if err := s.retireBoost(key, b, retire); err != nil && waitErr == nil {
			waitErr = err
		}
	}
	return waitErr
}

// boostSteps brings the boost along as far as it can go now, saying in
// bs where it is; retire, when set, is why it is to go, and its error is
// one to retry.
func (s *Server) boostSteps(ctx context.Context, key controller.Key, b *envv1alpha1.Boost, box *envv1alpha1.Box, bs *boostState) (boostRetire, error) {
	st := bs.st
	boxKey := controller.Key{Project: key.Project, Name: b.GetSpec().GetBox()}
	if box.GetSpec().GetRunState() == envv1alpha1.RunState_RUN_STATE_STOPPED {
		return boostRetire{why: "its box is stopped at workstream's request", force: true}, nil
	}
	fail := func(reason, msg string) {
		if st.Phase != envv1alpha1.BoostPhase_BOOST_PHASE_FAILED {
			s.log.Warn("a boost failed", "project", key.Project, "box", boxKey.Name, "reason", reason, "message", msg)
			// boost_idle, after which it goes, counts from its failure.
			st.LastUseTime = timestamppb.New(s.clock.Now())
		}
		st.ProcessesRunning = 0
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_FAILED, reason, msg)
	}
	target, live, err := s.liveTarget(ctx, b.GetSpec().GetTarget())
	if err != nil {
		return boostRetire{}, err
	}
	if !live {
		fail("TargetGone", fmt.Sprintf("its target %q is gone", b.GetSpec().GetTarget()))
		return boostRetire{}, nil
	}
	set := settingsOf(target)
	name := boostInstanceName(key.Name)
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: key.Project, Name: name})
	if err != nil {
		return boostRetire{}, err
	}
	mine := ok && owns(registry.Boost, b, in)

	// FAILED is final: its instance goes at once, freeing its room, and it
	// stays to say why until boost_idle passes or a boosted start of its
	// box replaces it.
	if st.Phase == envv1alpha1.BoostPhase_BOOST_PHASE_FAILED {
		if mine {
			return boostRetire{}, s.dropBoostInstance(key, b, in)
		}
		return boostRetire{why: s.boostIdle(st, false, set.boostIdle)}, nil
	}

	// What it was made of, from the instance there is, whatever its status
	// said before.
	if mine {
		st.Instance, st.Alias, st.WarmGolden = name, in.GetSpec().GetAlias(), in.GetSpec().GetDiskGolden()
		st.ImageGolden = in.GetMetadata().GetAnnotations()[boostImageGoldenAnnotation]
	}

	// One not READY yet fails with its box, or past its start timeout,
	// counted from when it last was not READY.
	if st.Phase != envv1alpha1.BoostPhase_BOOST_PHASE_READY {
		ready := conditions.Find(box.GetStatus().GetConditions(), BoxReady)
		switch box.GetStatus().GetPhase() {
		case envv1alpha1.BoxPhase_BOX_PHASE_FAILED:
			fail("BoxFailed", "its box failed: "+ready.GetMessage())
			return boostRetire{}, nil
		case envv1alpha1.BoxPhase_BOX_PHASE_UNPLACED:
			fail("BoxUnplaced", "its box's project has no Placement")
			return boostRetire{}, nil
		}
		since := b.GetMetadata().GetCreateTime().AsTime()
		if c := conditions.Find(st.GetConditions(), BoostReady); c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE && c.GetLastTransitionTime() != nil {
			since = c.GetLastTransitionTime().AsTime()
		}
		if s.clock.Since(since) >= set.boostStartTimeout {
			msg := conditions.Find(st.GetConditions(), BoostReady).GetMessage()
			fail("StartTimedOut", fmt.Sprintf("not READY within the target's boost_start_timeout (%s); it was: %s", set.boostStartTimeout, msg))
			return boostRetire{}, nil
		}
	}

	p, err := s.providerFor(target.GetMetadata().GetName())
	if err != nil {
		bs.cond(BoostReady, false, "TargetUnavailable", err.Error())
		return boostRetire{}, fmt.Errorf("boost %s: %w: %w", key, controller.ErrWaiting, err)
	}
	if ok && !mine {
		// An earlier boost's, which goes with it.
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_PENDING, "Pending", "the box's earlier boost's instance is still going")
		return boostRetire{}, nil
	}
	if ok && in.GetStatus().GetPhase() == envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE {
		// Its processes went with it, their exits unread.
		if err := changeRecord(ctx, s, boxProcessesKey(boxKey), func(r *boxProcesses) bool { return r.instanceGone(in.GetMetadata().GetUid()) }); err != nil {
			return boostRetire{}, err
		}
		if _, err := s.backend(registry.Instance).Delete(s.system(ctx), resource.Key{Project: key.Project, Name: name}, nil); err != nil && !isNotFound(err) {
			return boostRetire{}, err
		}
		st.ProcessesRunning = 0
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_STARTING, "InstanceGone", "its instance vanished from the target; it is made again")
		return boostRetire{}, nil
	}
	if !ok {
		return boostRetire{}, s.makeBoostInstance(ctx, key, b, box, p, target, bs, fail)
	}

	// Its use: a process running on it, or one started through tower.
	now := s.clock.Now()
	tq := s.boosts.of(b.GetSpec().GetTarget())
	tq.mu.Lock()
	used, busy := tq.used[boxKey], tq.busy(boxKey, now)
	tq.mu.Unlock()
	st.ProcessesRunning = in.GetStatus().GetProcessesRunning()
	last := st.GetLastUseTime().AsTime()
	switch {
	case st.ProcessesRunning > 0 && now.Sub(last) >= boostUseWriteEvery:
		st.LastUseTime = timestamppb.New(now)
	case used.After(last):
		st.LastUseTime = timestamppb.New(used)
	}
	if t := in.GetMetadata().GetAnnotations()[setupStartedAnnotation]; t != "" {
		st.SetupStartTime = parseTimestamp(t)
	}
	if t := in.GetMetadata().GetAnnotations()[setupEndedAnnotation]; t != "" {
		st.SetupEndTime = parseTimestamp(t)
	}

	if why := s.boostIdle(st, busy, set.boostIdle); why != "" {
		return boostRetire{why: why}, nil
	}
	switch in.GetStatus().GetPhase() {
	case envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED:
		fail("InstanceFailed", conditions.Find(in.GetStatus().GetConditions(), "Ready").GetMessage())
		return boostRetire{}, nil
	case envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING:
	default:
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_STARTING, "Starting", "its instance is starting")
		return boostRetire{}, nil
	}
	if files := box.GetSpec().GetSecretFiles(); len(files) > 0 && in.GetMetadata().GetAnnotations()[secretFilesAnnotation] != runOf(in) {
		return boostRetire{}, s.boostJob(ctx, key, b, in, bs, fail, "secretfiles", "its secret files are being written", func(ctx context.Context, ref provider.Instance) error {
			if err := s.writeSecretFilesIn(ctx, p, key.Project, ref, files); err != nil {
				return err
			}
			return s.annotateInstance(ctx, key.Project, ref.Name, secretFilesAnnotation, runOf(in))
		})
	}
	if in.GetMetadata().GetAnnotations()[materializedAnnotation] != "true" {
		msg := "the box's repositories are being checked out"
		if st.GetSetupStartTime() != nil {
			msg = "the box's setup steps run, since " + st.GetSetupStartTime().AsTime().UTC().Format(time.RFC3339)
		}
		bs.cond(BoostMaterialized, false, "CheckingOut", "")
		return boostRetire{}, s.boostJob(ctx, key, b, in, bs, fail, "materialize", msg, func(ctx context.Context, ref provider.Instance) error {
			if done, err := s.instanceMarked(ctx, key.Project, ref.Name, materializedAnnotation); err != nil || done {
				return err
			}
			if err := s.materializeInstance(ctx, p, boxKey, box, ref, b.GetSpec().GetTarget(), true); err != nil {
				return err
			}
			return s.annotateInstance(ctx, key.Project, ref.Name, materializedAnnotation, "true")
		})
	}
	bs.cond(BoostMaterialized, true, "Materialized", "")
	if w := in.GetMetadata().GetAnnotations()[setupAnnotation]; w != "" {
		bs.cond(BoostSetUp, false, "SetupFailed", w)
	} else {
		bs.cond(BoostSetUp, true, "SetUp", "")
	}
	st.Phase = envv1alpha1.BoostPhase_BOOST_PHASE_READY
	bs.cond(BoostReady, true, "Ready", "")
	return boostRetire{}, s.settleEnded(ctx, key, b, in, p)
}

// settleEnded settles, in a job, the boost's processes that ended and
// nobody read yet: their artifacts are copied back when they end, not
// when someone asks.
func (s *Server) settleEnded(ctx context.Context, key controller.Key, b *envv1alpha1.Boost, in *envv1alpha1.Instance, p provider.Provider) error {
	boxKey := controller.Key{Project: key.Project, Name: b.GetSpec().GetBox()}
	var rec boxProcesses
	if _, err := s.getRecord(ctx, boxProcessesKey(boxKey), &rec); err != nil {
		return err
	}
	open := 0
	for _, bp := range rec.Boost {
		if bp.InstanceUID == in.GetMetadata().GetUid() && bp.RunID != "" && !bp.Settled && !bp.Gone {
			open++
		}
	}
	if open <= int(in.GetStatus().GetProcessesRunning()) {
		return nil
	}
	ref := provider.Instance{Project: key.Project, Name: in.GetMetadata().GetName()}
	return s.startBoostJob(b.GetSpec().GetTarget(), "boost-settle/"+key.String(), key, func(ctx context.Context) (bool, error) {
		// Their exits kept, their usage recorded, and settled, as before
		// a boost goes.
		if _, err := s.keepBoostExits(ctx, boxKey, b, true); err != nil {
			return false, err
		}
		ps, err := p.ListProcesses(ctx, ref)
		if err != nil {
			return false, err
		}
		var rec boxProcesses
		if _, err := s.getRecord(ctx, boxProcessesKey(boxKey), &rec); err != nil {
			return false, err
		}
		for _, bp := range rec.Boost {
			if bp.InstanceUID != in.GetMetadata().GetUid() || bp.Settled || bp.RunID == "" {
				continue
			}
			// Ended, or no longer kept by the instance, which keeps a few
			// ended ones: settled either way, from its report.
			if i := slices.IndexFunc(ps, func(p provider.Process) bool { return p.ID == bp.ProviderID }); i >= 0 && ps[i].Exit == nil {
				continue
			}
			if _, err := s.settleBoostProcess(ctx, p, boxKey, ref, bp.ID); err != nil {
				return false, err
			}
		}
		return false, nil
	})
}

// boostIdle says why a boost is to go for idleness, "" if it is not: a
// READY or FAILED one nothing runs on, no start waits for, and nothing
// ran on, nor did it change phase, for idle.
func (s *Server) boostIdle(st *envv1alpha1.BoostStatus, busy bool, idle time.Duration) string {
	if busy || st.GetProcessesRunning() > 0 {
		return ""
	}
	if st.GetPhase() != envv1alpha1.BoostPhase_BOOST_PHASE_READY && st.GetPhase() != envv1alpha1.BoostPhase_BOOST_PHASE_FAILED {
		return ""
	}
	last := st.GetLastUseTime().AsTime()
	if c := conditions.Find(st.GetConditions(), BoostReady); c.GetLastTransitionTime() != nil {
		last = maxTime(last, c.GetLastTransitionTime().AsTime())
	}
	if s.clock.Since(last) < idle {
		return ""
	}
	return fmt.Sprintf("nothing ran on it for %s", idle)
}

func parseTimestamp(v string) *timestamppb.Timestamp {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil
	}
	return timestamppb.New(t)
}

// makeBoostInstance makes the boost's instance as its box's is made
// (boxInstanceSpec), of the boost's size, from its box's warm golden
// while it is ready, once its box is READY or stopped. One whose box's
// image golden is gone fails.
func (s *Server) makeBoostInstance(ctx context.Context, key controller.Key, b *envv1alpha1.Boost, box *envv1alpha1.Box, p provider.Provider, target *targetsv1alpha1.Target, bs *boostState, fail func(reason, msg string)) error {
	st := bs.st
	if box.GetStatus().GetPhase() != envv1alpha1.BoxPhase_BOX_PHASE_READY && box.GetStatus().GetPhase() != envv1alpha1.BoxPhase_BOX_PHASE_STOPPED {
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_PENDING, "WaitingForBox", "its box is not ready: it is "+strings.TrimPrefix(box.GetStatus().GetPhase().String(), "BOX_PHASE_"))
		return nil
	}
	img, ok, err := s.liveGolden(ctx, key.Project, box.GetStatus().GetImageGolden())
	if err != nil {
		return err
	}
	if !ok || goldenPhase(img) != envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY {
		fail("ImageGoldenGone", fmt.Sprintf("its box's image golden %q is not there, or not ready", box.GetStatus().GetImageGolden()))
		return nil
	}
	boxKey := controller.Key{Project: key.Project, Name: box.GetMetadata().GetName()}
	// Its box's disk, its own cpu and memory.
	res := proto.Clone(b.GetSpec().GetResources()).(*envv1alpha1.Resources)
	res.Disk = box.GetSpec().GetResources().GetDisk()
	is, warm, err := s.boxInstanceSpec(ctx, boxKey, box, p, target, img, res, box.GetStatus().GetWarmGolden())
	if errors.Is(err, errImageEnv) {
		bs.cond(BoostReady, false, "ImageUnresolved", "retried: "+err.Error())
	}
	if err != nil {
		return err
	}
	name, alias := boostInstanceName(key.Name), boostAlias(box.GetMetadata().GetName())
	is.Alias, is.RunState = alias, envv1alpha1.RunState_RUN_STATE_RUNNING
	_, err = s.backend(registry.Instance).Create(s.system(ctx), &envv1alpha1.Instance{
		Metadata: &metav1.ObjectMeta{
			Project: key.Project, Name: name, OwnerReferences: []*metav1.OwnerReference{ownedBy(registry.Boost, b)},
			Annotations: map[string]string{boostImageGoldenAnnotation: img.GetMetadata().GetName()},
		},
		Spec: is,
	})
	if err != nil && connect.CodeOf(err) != connect.CodeAlreadyExists {
		return err
	}
	st.Instance, st.Alias, st.ImageGolden, st.WarmGolden = name, alias, img.GetMetadata().GetName(), warm
	bs.cond(BoostMaterialized, false, "Creating", "")
	bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_STARTING, "Starting", "its instance is being made")
	return nil
}

// The backoff of a boost's job that failed, or found it must try again
// later: its first retry, and the most it doubles to.
const (
	boostRetryFirst = 100 * time.Millisecond
	boostRetryMax   = time.Minute
)

// boostBackoffs are the boosts' jobs that failed, or were to try again,
// that many times in a row, and when each may run again. A job restarted
// after its failure counts on, so one that fails at once does not run at
// the controller's first retry forever. In memory: a restart tries each
// at once.
type boostBackoffs struct {
	mu sync.Mutex
	m  map[string]boostTry
}

type boostTry struct {
	n    int
	next time.Time
}

// wait is how long until job may run again, 0 if it may now.
func (b *boostBackoffs) wait(job string, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(b.m[job].next.Sub(now), 0)
}

// ended counts a run of job: one to try again later pushes its next run
// back, doubling; one that did what it was for clears it.
func (b *boostBackoffs) ended(job string, again bool, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !again {
		delete(b.m, job)
		return
	}
	if b.m == nil {
		b.m = map[string]boostTry{}
	}
	t := b.m[job]
	delay := min(boostRetryFirst<<min(t.n, 20), boostRetryMax)
	b.m[job] = boostTry{n: t.n + 1, next: now.Add(delay)}
}

// forget drops the backoffs of the boost of key, gone.
func (b *boostBackoffs) forget(key controller.Key) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for job := range b.m {
		if strings.HasSuffix(job, "/"+key.String()) {
			delete(b.m, job)
		}
	}
}

// startBoostJob starts job for the boost of key, as startJob does, unless
// its backoff has not passed: then it is ErrWaiting, and the boost's
// reconcile asks again. fn's again, or its error, backs the job off.
func (s *Server) startBoostJob(target, job string, key controller.Key, fn func(ctx context.Context) (again bool, err error)) error {
	if wait := s.boostBackoffs.wait(job, s.clock.Now()); wait > 0 {
		return fmt.Errorf("boost %s: %s runs again in %s: %w", key, job, wait, controller.ErrWaiting)
	}
	s.startJob(target, job, s.boostCtrl, key, func(ctx context.Context) error {
		again, err := fn(ctx)
		if ctx.Err() == nil {
			s.boostBackoffs.ended(job, again || err != nil, s.clock.Now())
		}
		return err
	})
	return nil
}

// boostJob runs one of the boost's instance steps as a job, as a box's
// are, saying msg while it runs; one that failed fails the boost, and one
// that waits on its target is tried again as its backoff lets it.
func (s *Server) boostJob(ctx context.Context, key controller.Key, b *envv1alpha1.Boost, in *envv1alpha1.Instance, bs *boostState, fail func(reason, msg string), step, msg string, fn func(context.Context, provider.Instance) error) error {
	job := "boost-" + step + "/" + key.String()
	ref := newStepRef("boost", key.Project, key.Name, step, in.GetMetadata().GetUid()+"/"+runOf(in), b.GetMetadata().GetGeneration())
	g, err := s.stepGate(ctx, job, ref)
	switch {
	case err != nil:
		return err
	case g.Waiting != nil:
		// Said as while it runs: a message that changed at each try would
		// queue the boost again at once, and try again without waiting.
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_STARTING, "Starting", msg)
		return fmt.Errorf("boost %s: %w: %w", key, controller.ErrWaiting, g.Waiting)
	case g.Failed != "":
		fail("StepFailed", step+": "+g.Failed)
		return nil
	}
	bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_STARTING, "Starting", msg)
	if g.Running {
		return nil
	}
	inst := provider.Instance{Project: key.Project, Name: in.GetMetadata().GetName()}
	return s.startBoostJob(b.GetSpec().GetTarget(), job, key, func(ctx context.Context) (bool, error) {
		if err := fn(ctx, inst); err != nil {
			return false, err
		}
		return false, s.stepPassed(ctx, ref)
	})
}

// dropBoostInstance deletes a FAILED boost's instance, in a job, its
// processes' exits kept first as far as they can be read, so its room is
// free for the target's queue.
func (s *Server) dropBoostInstance(key controller.Key, b *envv1alpha1.Boost, in *envv1alpha1.Instance) error {
	target := b.GetSpec().GetTarget()
	boxKey := controller.Key{Project: key.Project, Name: b.GetSpec().GetBox()}
	return s.startBoostJob(target, "boost-drop/"+key.String(), key, func(ctx context.Context) (bool, error) {
		if _, err := s.keepBoostExits(ctx, boxKey, b, true); err != nil {
			return false, err
		}
		if err := changeRecord(ctx, s, boxProcessesKey(boxKey), func(r *boxProcesses) bool { return r.instanceGone(in.GetMetadata().GetUid()) }); err != nil {
			return false, err
		}
		rv := in.GetMetadata().GetResourceVersion()
		if _, err := s.backend(registry.Instance).Delete(s.system(ctx), resource.Key{Project: key.Project, Name: in.GetMetadata().GetName()}, &rv); err != nil && !isNotFound(err) && connect.CodeOf(err) != connect.CodeAborted {
			return false, err
		}
		return false, s.admitTarget(ctx, target)
	})
}

// admitTarget admits the target's queue again, as room was freed.
func (s *Server) admitTarget(ctx context.Context, target string) error {
	t, live, err := s.liveTarget(ctx, target)
	if err != nil || !live {
		return err
	}
	tq := s.boosts.of(target)
	tq.mu.Lock()
	defer tq.mu.Unlock()
	return s.admitBoosts(ctx, t, tq)
}

// retireBoost deletes a boost, in a job. Unless forced, its processes'
// exits are read and kept first (keepBoostExits), and it goes only if
// that listing showed nothing running, no start waits for it, and nothing
// started on it since; else it is tried again as its backoff lets it.
// Deleted, it goes once finishBoost has kept what is left of its
// processes.
func (s *Server) retireBoost(key controller.Key, b *envv1alpha1.Boost, r boostRetire) error {
	target, uid := b.GetSpec().GetTarget(), b.GetMetadata().GetUid()
	boxKey := controller.Key{Project: key.Project, Name: b.GetSpec().GetBox()}
	// A forced one is not held back by an idle one's backoff.
	job := "boost-retire/" + key.String()
	if r.force {
		job = "boost-retire-forced/" + key.String()
	}
	return s.startBoostJob(target, job, key, func(ctx context.Context) (bool, error) {
		tq := s.boosts.of(target)
		tq.mu.Lock()
		read := tq.starts[boxKey]
		tq.mu.Unlock()
		if !r.force {
			running, err := s.keepBoostExits(ctx, boxKey, b, false)
			if err != nil || running {
				return true, err
			}
			if hook := s.beforeBoostRetire.Load(); hook != nil {
				(*hook)(boxKey)
			}
		}
		tq.mu.Lock()
		defer tq.mu.Unlock()
		if !r.force && (tq.busy(boxKey, s.clock.Now()) || tq.starts[boxKey] != read) {
			return true, nil
		}
		cur, ok, err := getTyped[*envv1alpha1.Boost](ctx, s, registry.Boost, resource.Key{Project: key.Project, Name: key.Name})
		if err != nil || !ok || cur.GetMetadata().GetUid() != uid || cur.GetMetadata().GetDeleteTime() != nil {
			return false, err
		}
		rv := cur.GetMetadata().GetResourceVersion()
		if _, err := s.backend(registry.Boost).Delete(s.system(ctx), resource.Key{Project: key.Project, Name: key.Name}, &rv); err != nil && !isNotFound(err) {
			return false, err
		}
		s.log.Info("deleting a boost", "project", key.Project, "box", boxKey.Name, "target", target, "why", r.why)
		return false, nil
	})
}

// finishBoost lets a boost being deleted go, in a job: the exits of its
// processes are read and kept as far as they can be, their usage
// recorded, those not read marked lost; then its finalizer goes, its
// instance after it, and the target's queue is admitted again.
func (s *Server) finishBoost(key controller.Key, b *envv1alpha1.Boost) error {
	target, uid := b.GetSpec().GetTarget(), b.GetMetadata().GetUid()
	boxKey := controller.Key{Project: key.Project, Name: b.GetSpec().GetBox()}
	return s.startBoostJob(target, "boost-finish/"+key.String(), key, func(ctx context.Context) (bool, error) {
		if _, err := s.keepBoostExits(ctx, boxKey, b, true); err != nil {
			return false, err
		}
		if err := changeRecord(ctx, s, boxProcessesKey(boxKey), func(r *boxProcesses) bool { return r.boostGone(uid) }); err != nil {
			return false, err
		}
		err := s.removeFinalizerOf(ctx, registry.Boost, resource.Key{Project: key.Project, Name: key.Name}, uid, boostExitsFinalizer)
		if err != nil && !errors.Is(err, errOtherUID) {
			return false, err
		}
		tq := s.boosts.of(target)
		tq.mu.Lock()
		delete(tq.used, boxKey)
		tq.mu.Unlock()
		// What a later boost of the box does starts afresh.
		s.boostBackoffs.forget(key)
		if s.boxCtrl != nil {
			s.boxCtrl.Enqueue(boxKey)
		}
		return false, s.admitTarget(ctx, target)
	})
}
