package apiserver

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
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
	// boostTicketTTL is how long a pending start's ticket is kept unless
	// asked about again: a caller gone away holds no place.
	boostTicketTTL = 2 * time.Minute
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
	// arrived orders the queue; seen is when it was last asked about.
	arrived, seen time.Time
	// admitted says its box's boost is made for it: it waits for the
	// boost, no longer for room.
	admitted bool
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
	// starting counts each box's processes being started on its boost.
	starting map[controller.Key]int
	// used is when a process last started on each box's boost.
	used map[controller.Key]time.Time
}

func (q *boostQueues) of(target string) *targetBoosts {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.targets == nil {
		q.targets = map[string]*targetBoosts{}
	}
	t, ok := q.targets[target]
	if !ok {
		t = &targetBoosts{tickets: map[string]*boostTicket{}, starting: map[controller.Key]int{}, used: map[controller.Key]time.Time{}}
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
	first := func(a, b *boostTicket) int { return cmp.Or(a.arrived.Compare(b.arrived), cmp.Compare(a.id, b.id)) }
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
	// quota is what the Plan has left of each, less the boosts made since
	// the target last reported; hasCPU and hasMemory say it is known.
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

// targetBoostsOf lists the boosts on target.
func (s *Server) targetBoostsOf(ctx context.Context, target string) ([]*envv1alpha1.Boost, error) {
	all, err := listTyped[*envv1alpha1.Boost](ctx, s, registry.Boost, "")
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(b *envv1alpha1.Boost) bool { return b.GetSpec().GetTarget() != target }), nil
}

// roomOf is what t has for boosts beside boosts.
func roomOf(t *targetsv1alpha1.Target, boosts []*envv1alpha1.Boost) boostRoom {
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
	reported := t.GetStatus().GetLastReportTime().AsTime()
	for _, b := range boosts {
		size := boostSizeOf(b.GetSpec().GetResources())
		r.held = r.held.plus(size)
		// Made since the report, which does not count it yet.
		if b.GetMetadata().GetCreateTime().AsTime().After(reported) {
			r.quota = r.quota.minus(size)
		}
	}
	return r
}

// admitBoosts makes the boosts of the target's queued tickets that fit,
// in their order, and stops at the first that does not: one large boost
// is not passed by smaller ones forever. Held tq.mu.
func (s *Server) admitBoosts(ctx context.Context, t *targetsv1alpha1.Target, tq *targetBoosts) error {
	now := s.clock.Now()
	tq.prune(now)
	order := tq.order()
	if len(order) == 0 {
		return nil
	}
	target := t.GetMetadata().GetName()
	boosts, err := s.targetBoostsOf(ctx, target)
	if err != nil {
		return err
	}
	room := roomOf(t, boosts)
	for _, tk := range order {
		if slices.ContainsFunc(boosts, func(b *envv1alpha1.Boost) bool {
			return b.GetMetadata().GetProject() == tk.box.Project && b.GetMetadata().GetName() == tk.box.Name
		}) {
			// Its box's boost, too small, is going: admitted once gone.
			continue
		}
		if !room.fits(tk.size) {
			return nil
		}
		made, err := s.makeBoost(ctx, target, tk)
		if err != nil {
			return err
		}
		if made {
			room.take(tk.size)
		}
		tk.admitted, tq.last = true, tk.workstream
	}
	return nil
}

// makeBoost makes the Boost of tk's box, reporting false if its box is
// gone.
func (s *Server) makeBoost(ctx context.Context, target string, tk *boostTicket) (bool, error) {
	box, ok, err := getTyped[*envv1alpha1.Box](ctx, s, registry.Box, resource.Key{Project: tk.box.Project, Name: tk.box.Name})
	if err != nil || !ok || box.GetMetadata().GetDeleteTime() != nil {
		return false, err
	}
	_, err = s.backend(registry.Boost).Create(s.system(ctx), &envv1alpha1.Boost{
		Metadata: &metav1.ObjectMeta{Project: tk.box.Project, Name: tk.box.Name, OwnerReferences: []*metav1.OwnerReference{ownedBy(registry.Box, box)}},
		Spec:     &envv1alpha1.BoostSpec{Box: tk.box.Name, Resources: tk.size.resources(), Target: target},
	})
	if connect.CodeOf(err) == connect.CodeAlreadyExists {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s.log.Info("making a boost", "project", tk.box.Project, "box", tk.box.Name, "target", target, "size", tk.size.String(), "workstream", tk.workstream)
	return true, nil
}

// estimateWait is about how long until tk's boost is made, as far as
// what holds the target's room now says: each boost going boost_idle
// after its last use (a running one at least that long from now), the
// tickets before tk taking their room first. ok is false when it does
// not fit even once every boost is gone. Held tq.mu.
func (s *Server) estimateWait(t *targetsv1alpha1.Target, tq *targetBoosts, boosts []*envv1alpha1.Boost, order []*boostTicket, tk *boostTicket) (time.Duration, bool) {
	now, idle := s.clock.Now(), settingsOf(t).boostIdle
	type freeing struct {
		at   time.Time
		size boostSize
	}
	var frees []freeing
	for _, b := range boosts {
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
	room, when := roomOf(t, boosts), now
	for _, o := range order {
		for !room.fits(o.size) {
			if len(frees) == 0 {
				return 0, false
			}
			room.give(frees[0].size)
			when = maxTime(when, frees[0].at)
			frees = frees[1:]
		}
		room.take(o.size)
		if o == tk {
			break
		}
	}
	return when.Sub(now) + boostStartEstimate, true
}

// boostsOnTarget maps a Target to the boosts on it, whose settings it
// holds.
func (s *Server) boostsOnTarget(ctx context.Context, o store.Object) []controller.Key {
	boosts, err := s.targetBoostsOf(ctx, o.Name)
	if err != nil {
		return nil
	}
	keys := make([]controller.Key, 0, len(boosts))
	for _, b := range boosts {
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

// reconcileBoost brings a boost along: its instance made as its box's is,
// the box's secret files, checkouts and setup steps in it, then READY;
// and it is deleted once nothing has run on it for boost_idle, or once
// its box is stopped at workstream's request.
func (s *Server) reconcileBoost(ctx context.Context, key controller.Key) error {
	b, ok, err := getTyped[*envv1alpha1.Boost](ctx, s, registry.Boost, resource.Key{Project: key.Project, Name: key.Name})
	if err != nil || !ok || b.GetMetadata().GetDeleteTime() != nil {
		return err
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
	if retire != "" {
		s.retireBoost(key, b, retire)
	}
	return waitErr
}

// boostSteps brings the boost along as far as it can go now, saying in
// bs where it is; retire, when set, is why it is to go, and its error is
// one to retry.
func (s *Server) boostSteps(ctx context.Context, key controller.Key, b *envv1alpha1.Boost, box *envv1alpha1.Box, bs *boostState) (retire string, err error) {
	st := bs.st
	boxKey := controller.Key{Project: key.Project, Name: b.GetSpec().GetBox()}
	if box.GetSpec().GetRunState() == envv1alpha1.RunState_RUN_STATE_STOPPED {
		return "its box is stopped at workstream's request", nil
	}
	target, live, err := s.liveTarget(ctx, b.GetSpec().GetTarget())
	if err != nil {
		return "", err
	}
	if !live {
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_FAILED, "TargetGone", fmt.Sprintf("its target %q is gone", b.GetSpec().GetTarget()))
		return "", nil
	}
	set := settingsOf(target)
	p, err := s.providerFor(target.GetMetadata().GetName())
	if err != nil {
		bs.cond(BoostReady, false, "TargetUnavailable", err.Error())
		return "", fmt.Errorf("boost %s: %w: %w", key, controller.ErrWaiting, err)
	}
	name := boostInstanceName(key.Name)
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: key.Project, Name: name})
	if err != nil {
		return "", err
	}
	if ok && !owns(registry.Boost, b, in) {
		// An earlier boost's, which goes with it.
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_PENDING, "Pending", "the box's earlier boost's instance is still going")
		return "", nil
	}
	if ok && in.GetStatus().GetPhase() == envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE {
		if _, err := s.backend(registry.Instance).Delete(s.system(ctx), resource.Key{Project: key.Project, Name: name}, nil); err != nil && !isNotFound(err) {
			return "", err
		}
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_STARTING, "InstanceGone", "its instance vanished from the target; it is made again")
		return "", nil
	}
	if !ok {
		return "", s.makeBoostInstance(ctx, key, b, box, p, target, bs)
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

	if retire := s.boostIdle(st, busy, set.boostIdle); retire != "" {
		return retire, nil
	}
	switch in.GetStatus().GetPhase() {
	case envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED:
		msg := conditions.Find(in.GetStatus().GetConditions(), "Ready").GetMessage()
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_FAILED, "InstanceFailed", msg)
		return "", nil
	case envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING:
	default:
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_STARTING, "Starting", "its instance is starting")
		return "", nil
	}
	if files := box.GetSpec().GetSecretFiles(); len(files) > 0 && in.GetMetadata().GetAnnotations()[secretFilesAnnotation] != runOf(in) {
		return "", s.boostJob(ctx, key, b, in, bs, "secretfiles", "its secret files are being written", func(ctx context.Context, ref provider.Instance) error {
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
		return "", s.boostJob(ctx, key, b, in, bs, "materialize", msg, func(ctx context.Context, ref provider.Instance) error {
			if done, err := s.instanceMarked(ctx, key.Project, ref.Name, materializedAnnotation); err != nil || done {
				return err
			}
			if err := s.materializeInstance(ctx, p, boxKey, box, ref, b.GetSpec().GetTarget()); err != nil {
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
	return "", nil
}

// boostIdle says why a boost is to go for idleness, "" if it is not: a
// READY or FAILED one nothing runs on, no start waits for, and nothing
// ran on for idle.
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
// (boxInstanceSpec), of the boost's size, once its box is READY.
func (s *Server) makeBoostInstance(ctx context.Context, key controller.Key, b *envv1alpha1.Boost, box *envv1alpha1.Box, p provider.Provider, target *targetsv1alpha1.Target, bs *boostState) error {
	st := bs.st
	if box.GetStatus().GetPhase() != envv1alpha1.BoxPhase_BOX_PHASE_READY && box.GetStatus().GetPhase() != envv1alpha1.BoxPhase_BOX_PHASE_STOPPED {
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_PENDING, "WaitingForBox", "its box is not ready")
		return nil
	}
	img, ok, err := s.liveGolden(ctx, key.Project, box.GetStatus().GetImageGolden())
	if err != nil {
		return err
	}
	if !ok || goldenPhase(img) != envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY {
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_PENDING, "WaitingForBox", "its box's image golden is not ready")
		return nil
	}
	boxKey := controller.Key{Project: key.Project, Name: box.GetMetadata().GetName()}
	// Its box's disk, its own cpu and memory.
	res := proto.Clone(b.GetSpec().GetResources()).(*envv1alpha1.Resources)
	res.Disk = box.GetSpec().GetResources().GetDisk()
	is, warm, err := s.boxInstanceSpec(ctx, boxKey, box, p, target, img, res)
	if err != nil {
		bs.cond(BoostReady, false, "ImageUnresolved", "reading its image's environment, retried: "+err.Error())
		return err
	}
	name, alias := boostInstanceName(key.Name), boostAlias(box.GetMetadata().GetName())
	is.Alias, is.RunState = alias, envv1alpha1.RunState_RUN_STATE_RUNNING
	_, err = s.backend(registry.Instance).Create(s.system(ctx), &envv1alpha1.Instance{
		Metadata: &metav1.ObjectMeta{Project: key.Project, Name: name, OwnerReferences: []*metav1.OwnerReference{ownedBy(registry.Boost, b)}},
		Spec:     is,
	})
	if err != nil && connect.CodeOf(err) != connect.CodeAlreadyExists {
		return err
	}
	st.Instance, st.Alias, st.ImageGolden, st.WarmGolden = name, alias, img.GetMetadata().GetName(), warm
	bs.cond(BoostMaterialized, false, "Creating", "")
	bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_STARTING, "Starting", "its instance is being made")
	return nil
}

// boostJob runs one of the boost's instance steps as a job, as a box's
// are, saying msg while it runs.
func (s *Server) boostJob(ctx context.Context, key controller.Key, b *envv1alpha1.Boost, in *envv1alpha1.Instance, bs *boostState, step, msg string, fn func(context.Context, provider.Instance) error) error {
	job := "boost-" + step + "/" + key.String()
	ref := newStepRef("boost", key.Project, key.Name, step, in.GetMetadata().GetUid()+"/"+runOf(in), b.GetMetadata().GetGeneration())
	g, err := s.stepGate(ctx, job, ref)
	switch {
	case err != nil:
		return err
	case g.Waiting != nil:
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_STARTING, "Starting", g.Waiting.Error())
		return fmt.Errorf("boost %s: %w: %w", key, controller.ErrWaiting, g.Waiting)
	case g.Failed != "":
		bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_FAILED, "StepFailed", step+": "+g.Failed)
		return nil
	}
	bs.not(envv1alpha1.BoostPhase_BOOST_PHASE_STARTING, "Starting", msg)
	if g.Running {
		return nil
	}
	inst := provider.Instance{Project: key.Project, Name: in.GetMetadata().GetName()}
	s.startJob(b.GetSpec().GetTarget(), job, s.boostCtrl, key, func(ctx context.Context) error {
		if err := fn(ctx, inst); err != nil {
			return err
		}
		return s.stepPassed(ctx, ref)
	})
	return nil
}

// retireBoost deletes a boost, in a job: its processes' exits are read
// and kept first (keepBoostExits), and it goes only if nothing started on
// it meanwhile. The queue of its target is then admitted again, as its
// room is free.
func (s *Server) retireBoost(key controller.Key, b *envv1alpha1.Boost, why string) {
	target, uid := b.GetSpec().GetTarget(), b.GetMetadata().GetUid()
	boxKey := controller.Key{Project: key.Project, Name: b.GetSpec().GetBox()}
	s.startJob(target, "boost-retire/"+key.String(), s.boostCtrl, key, func(ctx context.Context) error {
		read := s.clock.Now()
		running, err := s.keepBoostExits(ctx, boxKey, b)
		if err != nil {
			return err
		}
		if running {
			return nil
		}
		tq := s.boosts.of(target)
		tq.mu.Lock()
		defer tq.mu.Unlock()
		if tq.busy(boxKey, s.clock.Now()) || tq.used[boxKey].After(read) {
			return nil
		}
		cur, ok, err := getTyped[*envv1alpha1.Boost](ctx, s, registry.Boost, resource.Key{Project: key.Project, Name: key.Name})
		if err != nil || !ok || cur.GetMetadata().GetUid() != uid {
			return err
		}
		rv := cur.GetMetadata().GetResourceVersion()
		if _, err := s.backend(registry.Boost).Delete(s.system(ctx), resource.Key{Project: key.Project, Name: key.Name}, &rv); err != nil && !isNotFound(err) {
			return err
		}
		delete(tq.used, boxKey)
		s.log.Info("deleted a boost", "project", key.Project, "box", boxKey.Name, "target", target, "why", why)
		if err := changeRecord(ctx, s, boxProcessesKey(boxKey), func(r *boxProcesses) bool { return r.boostGone(uid) }); err != nil {
			s.log.Error("marking a deleted boost's processes failed", "project", key.Project, "box", boxKey.Name, "error", err)
		}
		if s.boxCtrl != nil {
			s.boxCtrl.Enqueue(boxKey)
		}
		t, live, err := s.liveTarget(ctx, target)
		if err != nil || !live {
			return err
		}
		return s.admitBoosts(ctx, t, tq)
	})
}
