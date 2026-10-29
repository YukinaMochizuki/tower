package apiserver

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/authz"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// boxProcessesKind holds, per box, which of its processes ran on its
// boost, on which instance and by which id there, and, once read, how
// each ended: a process call on the box finds where the process runs
// from it, and a boost gone leaves its processes' exits here. It holds no
// command text, only the coarse key of each command
// (docs/decisions/process-usage.md). It goes with its box. Not an API
// kind.
const boxProcessesKind = "BoxProcesses"

// boostProcessesKept bounds a box's boost processes kept, those of
// instances gone going first.
const boostProcessesKept = 100

// boostProcessPrefix starts the id of every process run on a boost; a
// box's own are its provider's, which never start so (provider ids are
// "p" and a number on the fake, a guest's on hangar).
const boostProcessPrefix = "boost-"

// boxProcesses is what boxProcessesKind holds.
type boxProcesses struct {
	// BoxUID is the box's: a box of the name made again starts afresh.
	BoxUID string `json:"boxUid"`
	// Repository is the box's first checkout's, which its processes'
	// usage is kept under once the box is gone.
	Repository string         `json:"repository,omitempty"`
	Boost      []boostProcess `json:"boost"`
}

// boostProcess is a process run on a box's boost.
type boostProcess struct {
	// ID is tower's: boostProcessPrefix, the end of the uid of the
	// instance it runs on, and ProviderID, its id there.
	ID          string `json:"id"`
	BoostUID    string `json:"boostUid"`
	Instance    string `json:"instance"`
	InstanceUID string `json:"instanceUid"`
	ProviderID  string `json:"providerId"`
	// CPU and Memory are the boost's size.
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	Name   string `json:"name,omitempty"`
	// Coarse is its command's coarse key (coarseCommand), all of its
	// command it is shown by once its boost is gone.
	Coarse  string    `json:"coarse,omitempty"`
	Dir     string    `json:"dir,omitempty"`
	Started time.Time `json:"started"`
	// Exit is how it ended, once read.
	Exit *provider.ProcessExit `json:"exit,omitempty"`
	// Gone says its instance is gone: with no Exit, it ended unseen.
	Gone bool `json:"gone,omitempty"`
	// RunID names its report on the boost (reportPath); Artifacts are the
	// paths it named; Sync is the sync before it started.
	RunID     string     `json:"runId,omitempty"`
	Artifacts []string   `json:"artifacts,omitempty"`
	Sync      *syncStats `json:"sync,omitempty"`
	// Run is what it changed and copied back, once Settled.
	Run     *runResult `json:"run,omitempty"`
	Settled bool       `json:"settled,omitempty"`
}

func boxProcessesKey(box controller.Key) store.Key {
	return store.Key{Kind: boxProcessesKind, Project: box.Project, Name: box.Name}
}

// gone marks the processes match picks as gone, reporting whether any
// was not.
func (r *boxProcesses) gone(match func(boostProcess) bool) bool {
	changed := false
	for i := range r.Boost {
		if match(r.Boost[i]) && !r.Boost[i].Gone {
			r.Boost[i].Gone, changed = true, true
		}
	}
	return changed
}

// boostGone marks the processes of the boost of uid gone.
func (r *boxProcesses) boostGone(uid string) bool {
	return r.gone(func(p boostProcess) bool { return p.BoostUID == uid })
}

// instanceGone marks the processes of the instance of uid gone.
func (r *boxProcesses) instanceGone(uid string) bool {
	return r.gone(func(p boostProcess) bool { return p.InstanceUID == uid })
}

// add keeps p, dropping the oldest past boostProcessesKept, those gone
// first.
func (r *boxProcesses) add(p boostProcess) {
	r.Boost = append(r.Boost, p)
	for len(r.Boost) > boostProcessesKept {
		i := slices.IndexFunc(r.Boost, func(p boostProcess) bool { return p.Gone })
		if i < 0 {
			i = 0
		}
		r.Boost = slices.Delete(r.Boost, i, i+1)
	}
}

func (r *boxProcesses) find(id string) (boostProcess, bool) {
	i := slices.IndexFunc(r.Boost, func(p boostProcess) bool { return p.ID == id })
	if i < 0 {
		return boostProcess{}, false
	}
	return r.Boost[i], true
}

// keepExits sets the exits of the processes exits names, by tower's id,
// that have none, reporting whether any had none.
func (r *boxProcesses) keepExits(exits map[string]*provider.ProcessExit) bool {
	changed := false
	for i := range r.Boost {
		if x, ok := exits[r.Boost[i].ID]; ok && x != nil && r.Boost[i].Exit == nil {
			r.Boost[i].Exit, changed = x, true
		}
	}
	return changed
}

// boostProcessID is tower's id of the process pid on the instance of
// uid: a boost's instance made again, or another boost, has another uid,
// so no id is used twice. The uid's end is its random part (a uid starts
// with when it was made).
func boostProcessID(instanceUID, pid string) string {
	short := strings.ReplaceAll(instanceUID, "-", "")
	if len(short) > 12 {
		short = short[len(short)-12:]
	}
	return boostProcessPrefix + short + "-" + pid
}

// listedProcess is a process of a box's, in the box or on its boost, as
// tower names it, and where it ran.
type listedProcess struct {
	provider.Process
	at ranOn
	// kept says it is read from boxProcesses, its instance gone: its
	// usage was recorded before it went, if it could be, and Command is
	// its coarse key alone.
	kept bool
	// lost says it ended unseen: its instance went before its exit was
	// read.
	lost bool
	// run is a boost process's sync, and what it changed and copied back.
	run *envv1alpha1.BoostRun
}

// running says the process runs.
func (p listedProcess) running() bool { return p.Exit == nil && !p.lost }

// lostExitCode is a lost exit's code: not 0, so a reader that does not
// look at lost takes it as failed.
const lostExitCode = 255

// exit is how the API says the process ended; nil while it runs.
func (p listedProcess) exit() *envv1alpha1.ProcessExit {
	var x *envv1alpha1.ProcessExit
	switch {
	case p.lost:
		x = &envv1alpha1.ProcessExit{Code: lostExitCode, Lost: true}
	case p.Exit != nil:
		x = processExit(p.Exit)
	default:
		return nil
	}
	x.Boost = p.run
	return x
}

// proto is a as the API says it; nil for none, as a machine's process.
func (a ranOn) proto() *envv1alpha1.RanOn {
	if a.Kind == "" {
		return nil
	}
	kind := envv1alpha1.RanOnKind_RAN_ON_KIND_BOX
	if a.Kind == ranOnBoost {
		kind = envv1alpha1.RanOnKind_RAN_ON_KIND_BOOST
	}
	return &envv1alpha1.RanOn{Kind: kind, Resources: &envv1alpha1.Resources{Cpu: a.CPU, Memory: a.Memory}}
}

// liveInstanceOf says whether the instance the boost process p ran on is
// still there, the same one, not gone.
func (s *Server) liveInstanceOf(ctx context.Context, project string, p boostProcess) (bool, error) {
	if p.Gone {
		return false, nil
	}
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: project, Name: p.Instance})
	if err != nil || !ok {
		return false, err
	}
	return in.GetMetadata().GetUid() == p.InstanceUID && in.GetStatus().GetPhase() != envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE, nil
}

// boxProcessesOf lists e's processes, the box's own and its boost's,
// keeping the exits of its boost's read now. A boost process whose
// instance is gone is listed as kept: its exit, or lost.
func (s *Server) boxProcessesOf(ctx context.Context, e *environment) ([]listedProcess, error) {
	ps, err := e.p.ListProcesses(ctx, e.ref)
	if err != nil {
		return nil, err
	}
	at := ranOn{}
	if e.box != nil {
		res := s.instanceResources(ctx, e)
		at = ranOn{Kind: ranOnBox, CPU: res.GetCpu(), Memory: res.GetMemory()}
	}
	out := make([]listedProcess, 0, len(ps))
	for _, p := range ps {
		out = append(out, listedProcess{Process: p, at: at})
	}
	if e.box == nil {
		return out, nil
	}
	boxKey := controller.Key{Project: e.key.Project, Name: e.key.Name}
	var rec boxProcesses
	if _, err := s.getRecord(ctx, boxProcessesKey(boxKey), &rec); err != nil || len(rec.Boost) == 0 || rec.BoxUID != e.box.GetMetadata().GetUid() {
		return out, err
	}
	// The boost's instance now, and what runs on it.
	live := map[string]provider.Process{}
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: e.key.Project, Name: boostInstanceName(e.key.Name)})
	if err != nil {
		return nil, err
	}
	listed := false
	if ok && in.GetStatus().GetPhase() != envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE {
		bps, err := e.p.ListProcesses(ctx, provider.Instance{Project: e.key.Project, Name: in.GetMetadata().GetName()})
		if err != nil {
			// A boost not running yet, or going: its processes as kept.
			s.log.Warn("listing a boost's processes failed", "environment", e.String(), "error", err)
		}
		listed = err == nil
		for _, p := range bps {
			live[p.ID] = p
		}
	}
	exits := map[string]*provider.ProcessExit{}
	var settle []int
	for _, bp := range rec.Boost {
		at := ranOn{Kind: ranOnBoost, CPU: bp.CPU, Memory: bp.Memory}
		onIt := ok && !bp.Gone && bp.InstanceUID == in.GetMetadata().GetUid()
		if p, found := live[bp.ProviderID]; onIt && found {
			p.ID = bp.ID
			if p.Exit != nil && bp.Exit == nil {
				exits[bp.ID] = p.Exit
			}
			if p.Exit != nil && !bp.Settled {
				settle = append(settle, len(out))
			}
			out = append(out, listedProcess{Process: p, at: at, run: boostRunOf(bp)})
			continue
		}
		if onIt && bp.Exit == nil && !listed {
			// Its instance did not answer: running, as far as is known.
			out = append(out, listedProcess{Process: provider.Process{ID: bp.ID, Name: bp.Name, Command: []string{bp.Coarse}, Dir: bp.Dir, Started: bp.Started}, at: at, kept: true, run: boostRunOf(bp)})
			continue
		}
		// Its instance gone, or the instance no longer keeps it: as kept,
		// its exit or lost.
		out = append(out, listedProcess{Process: provider.Process{
			ID: bp.ID, Name: bp.Name, Command: []string{bp.Coarse}, Dir: bp.Dir, Started: bp.Started, Exit: bp.Exit,
		}, at: at, kept: true, lost: bp.Exit == nil, run: boostRunOf(bp)})
	}
	if len(exits) > 0 {
		if err := changeRecord(ctx, s, boxProcessesKey(boxKey), func(r *boxProcesses) bool { return r.keepExits(exits) }); err != nil {
			s.log.Error("keeping a boost's processes' exits failed", "environment", e.String(), "error", err)
		}
	}
	// What the ended ones changed and copied back, before they are said
	// to have ended.
	for _, i := range settle {
		bp, err := s.settleBoostProcess(ctx, e.p, boxKey, provider.Instance{Project: e.key.Project, Name: in.GetMetadata().GetName()}, out[i].ID)
		if err != nil {
			s.log.Error("settling a boost process failed", "environment", e.String(), "process", out[i].ID, "error", err)
			continue
		}
		out[i].run = boostRunOf(bp)
	}
	return out, nil
}

// keepBoostExits reads the exits of b's processes into boxProcesses and
// records their usage. Unless forced, running says one still runs, and
// nothing is kept, and a listing that fails is an error, so a READY
// boost goes only on one that shows nothing running; forced, as before
// b's instance goes whatever runs, it keeps what ended and leaves the
// rest to be marked lost, and an
// instance that does not answer keeps nothing. Its box gone, the usage is
// kept under the repository boxProcesses names.
func (s *Server) keepBoostExits(ctx context.Context, boxKey controller.Key, b *envv1alpha1.Boost, force bool) (running bool, err error) {
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: boxKey.Project, Name: boostInstanceName(b.GetMetadata().GetName())})
	if err != nil || !ok || !owns(registry.Boost, b, in) {
		return false, err
	}
	var rec boxProcesses
	if _, err := s.getRecord(ctx, boxProcessesKey(boxKey), &rec); err != nil {
		return false, err
	}
	mine := slices.ContainsFunc(rec.Boost, func(p boostProcess) bool { return p.InstanceUID == in.GetMetadata().GetUid() && p.Exit == nil })
	if !mine && force {
		return false, nil
	}
	p, err := s.providerFor(b.GetSpec().GetTarget())
	if err != nil {
		if force {
			return false, nil
		}
		return false, fmt.Errorf("%w: %w", controller.ErrWaiting, err)
	}
	ps, err := p.ListProcesses(ctx, provider.Instance{Project: boxKey.Project, Name: in.GetMetadata().GetName()})
	switch {
	case err == nil:
	case force:
		// Going whatever: none to keep.
		return false, nil
	case b.GetStatus().GetPhase() == envv1alpha1.BoostPhase_BOOST_PHASE_PENDING || b.GetStatus().GetPhase() == envv1alpha1.BoostPhase_BOOST_PHASE_STARTING:
		// Never READY: nothing ran on it.
		return false, nil
	default:
		// Not known to run nothing, even not found: it stays, and a later
		// reconcile tries again.
		return false, fmt.Errorf("listing its processes before it goes for idleness: %w", err)
	}
	if !force && slices.ContainsFunc(ps, func(p provider.Process) bool { return p.Exit == nil }) {
		return true, nil
	}
	var listed []listedProcess
	exits := map[string]*provider.ProcessExit{}
	for _, bp := range rec.Boost {
		if bp.InstanceUID != in.GetMetadata().GetUid() {
			continue
		}
		i := slices.IndexFunc(ps, func(p provider.Process) bool { return p.ID == bp.ProviderID && p.Exit != nil })
		if i < 0 {
			continue
		}
		p := ps[i]
		p.ID, exits[bp.ID] = bp.ID, p.Exit
		listed = append(listed, listedProcess{Process: p, at: ranOn{Kind: ranOnBoost, CPU: bp.CPU, Memory: bp.Memory}})
	}
	if len(listed) == 0 {
		return false, nil
	}
	if err := changeRecord(ctx, s, boxProcessesKey(boxKey), func(r *boxProcesses) bool { return r.keepExits(exits) }); err != nil {
		return false, err
	}
	// Their reports and artifacts, before the boost goes.
	for _, l := range listed {
		if _, err := s.settleBoostProcess(ctx, p, boxKey, provider.Instance{Project: boxKey.Project, Name: in.GetMetadata().GetName()}, l.ID); err != nil {
			s.log.Error("settling a boost process failed", "box", boxKey.String(), "process", l.ID, "error", err)
		}
	}
	box, ok, err := getTyped[*envv1alpha1.Box](ctx, s, registry.Box, resource.Key{Project: boxKey.Project, Name: boxKey.Name})
	if err != nil {
		return false, err
	}
	if !ok || box.GetMetadata().GetUid() != rec.BoxUID {
		// The box is gone: what its usage is kept under, from the record.
		box = &envv1alpha1.Box{
			Metadata: &metav1.ObjectMeta{Project: boxKey.Project, Name: boxKey.Name, Uid: rec.BoxUID},
			Spec:     &envv1alpha1.BoxSpec{Checkouts: []*envv1alpha1.Checkout{{Repository: rec.Repository}}},
		}
	}
	e := &environment{key: resource.Key{Project: boxKey.Project, Name: boxKey.Name}, kind: "box", box: box, p: p}
	// The boost's alone: the box's own are counted at their next read.
	s.exitsListed(ctx, e, listed, false)
	return false, nil
}

// commandSummary is a command line as ListProcesses shows it, cut short.
func commandSummary(command []string) string {
	cmd := strings.Join(command, " ")
	if len(cmd) > commandSummaryLen {
		cmd = cmd[:commandSummaryLen] + "..."
	}
	return cmd
}

// startBoosted answers a StartProcess that asks for a boost, or asks
// again with a ticket: started on the box's boost once it is READY, else
// pending at once, with a ticket, why, and about how long. Asking again
// with a ticket still pending changes nothing of its place; it admits
// the target's queue, as any start does.
func (h environmentService) startBoosted(ctx context.Context, c authz.Caller, e *environment, m *envv1alpha1.StartProcessRequest, command []string) (*envv1alpha1.StartProcessResponse, error) {
	s := h.s
	if e.box == nil {
		return nil, invalid("a boost is a box's: %s has none", e)
	}
	if e.target.GetSpec().GetHangar() == nil {
		return nil, failedPrecondition("%s runs on a hostd target, which has no boosts: its box's memory is a limit, raised as it runs", e)
	}
	set := settingsOf(e.target)
	boxKey := controller.Key{Project: e.key.Project, Name: e.key.Name}
	tq := s.boosts.of(e.target.GetMetadata().GetName())
	tq.mu.Lock()
	locked := true
	defer func() {
		if locked {
			tq.mu.Unlock()
		}
	}()
	now := s.clock.Now()
	tq.prune(now)
	var tk *boostTicket
	if id := m.GetPending(); id != "" {
		if t, ok := tq.tickets[id]; ok && t.box == boxKey {
			tk = t
		}
		if m.GetCancel() {
			if tk != nil {
				delete(tq.tickets, tk.id)
				s.log.Info("a pending boosted start given up", "caller", c.String(), "environment", e.String(), "ticket", tk.id)
				if err := s.admitBoosts(ctx, e.target, tq); err != nil {
					return nil, apiError(err)
				}
			}
			return &envv1alpha1.StartProcessResponse{}, nil
		}
	} else if m.GetCancel() {
		return nil, invalid("cancel gives up a pending start: name its ticket in pending")
	}
	size := set.boostResources
	if tk != nil {
		size, tk.seen = tk.size, now
	} else if m.GetBoost() != nil {
		var err error
		if size, err = boostAsk(m.GetBoost(), set); err != nil {
			return nil, err
		}
	}
	v, err := s.boostsOnTargetNow(ctx, e.target)
	if err != nil {
		return nil, apiError(err)
	}
	// One that cannot fit even with no boost held is not queued.
	if empty := emptyRoomOf(e.target, v); !empty.fits(size) {
		if tk != nil {
			delete(tq.tickets, tk.id)
		}
		return nil, failedPrecondition("a boost of %s does not fit %s's target %q even with no boost on it: its Plan has %s left",
			size, e, e.target.GetMetadata().GetName(), empty.quota)
	}
	ticket := func(admitted bool) *boostTicket {
		if tk == nil {
			tq.arrivals++
			tk = &boostTicket{id: newTicketID(), box: boxKey, workstream: e.workstream, size: size, arrived: now, arrival: tq.arrivals}
			tq.tickets[tk.id] = tk
		}
		tk.seen, tk.admitted = now, tk.admitted || admitted
		return tk
	}
	pending := func(reason envv1alpha1.PendingReason, msg string, position int, wait time.Duration) *envv1alpha1.StartProcessResponse {
		p := &envv1alpha1.PendingStart{Ticket: tk.id, Reason: reason, Message: msg, Position: int32(position)}
		if wait > 0 {
			p.EstimatedWait = durationpb.New(wait.Round(time.Second))
		}
		s.log.Debug("a boosted start pending", "caller", c.String(), "environment", e.String(), "ticket", tk.id,
			"reason", reason.String(), "size", size.String(), "position", position, "wait", wait)
		return &envv1alpha1.StartProcessResponse{Pending: p}
	}

	b, ok, err := getTyped[*envv1alpha1.Boost](ctx, s, registry.Boost, e.key)
	if err != nil {
		return nil, apiError(err)
	}
	if ok {
		have := boostSizeOf(b.GetSpec().GetResources())
		readyMsg := conditions.Find(b.GetStatus().GetConditions(), BoostReady).GetMessage()
		switch {
		case b.GetMetadata().GetDeleteTime() != nil:
			ticket(false)
			return pending(envv1alpha1.PendingReason_PENDING_REASON_BOOST_GOING, "its boost is being deleted, its processes' exits kept first; ask again: another is made once it is gone", 0, 0), nil
		case b.GetStatus().GetPhase() == envv1alpha1.BoostPhase_BOOST_PHASE_FAILED:
			// Its room is free already; this start replaces it.
			ticket(false)
			// One held back by its backoff is tried again at the next ask.
			_ = s.retireBoost(boxKey, b, boostRetire{why: "a boosted start replaces it", force: true})
			return pending(envv1alpha1.PendingReason_PENDING_REASON_BOOST_STARTING, "its failed boost is replaced: "+readyMsg, 0, boostStartEstimate), nil
		case have.covers(size) && b.GetStatus().GetPhase() == envv1alpha1.BoostPhase_BOOST_PHASE_READY:
			return h.syncAndStart(ctx, c, e, m, command, b, tq, boxKey, ticket(true), &locked, pending)
		case have.covers(size):
			ticket(true)
			wait := max(boostStartEstimate-s.clock.Since(b.GetMetadata().GetCreateTime().AsTime()), 5*time.Second)
			return pending(envv1alpha1.PendingReason_PENDING_REASON_BOOST_STARTING, "its boost is starting: "+readyMsg, 0, wait), nil
		}
		ticket(false)
		if b.GetStatus().GetProcessesRunning() > 0 || tq.starting[boxKey] > 0 {
			return pending(envv1alpha1.PendingReason_PENDING_REASON_BOOST_BUSY, fmt.Sprintf(
				"its boost (%s) is smaller than asked (%s) and runs a process: it is made again larger once its processes end", have, size), 0, 0), nil
		}
		_ = s.retireBoost(boxKey, b, boostRetire{why: fmt.Sprintf("a larger one (%s) is asked for", size)})
		return pending(envv1alpha1.PendingReason_PENDING_REASON_BOOST_STARTING, fmt.Sprintf(
			"its boost (%s) is smaller than asked (%s): it is made again larger", have, size), 0, boostStartEstimate), nil
	}
	ticket(false)
	if err := s.admitBoosts(ctx, e.target, tq); err != nil {
		return nil, apiError(err)
	}
	if _, ok := tq.tickets[tk.id]; !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s is gone", e))
	}
	if tk.admitted {
		return pending(envv1alpha1.PendingReason_PENDING_REASON_BOOST_STARTING, "its boost is being made", 0, boostStartEstimate), nil
	}
	if v, err = s.boostsOnTargetNow(ctx, e.target); err != nil {
		return nil, apiError(err)
	}
	order := tq.order()
	position := slices.Index(order, tk) + 1
	room := roomOf(e.target, v)
	msg := fmt.Sprintf("the target's boosts hold %s of its boost_budget of %s", room.held, room.budget)
	if room.hasCPU || room.hasMemory {
		msg += fmt.Sprintf(", and its Plan has %s left", room.quota)
	}
	msg += fmt.Sprintf("; a boost of %s waits its turn, %d of %d", size, position, len(order))
	return pending(envv1alpha1.PendingReason_PENDING_REASON_QUEUED_BUDGET, msg, position, s.estimateWait(e.target, tq, v, order, tk)), nil
}

// syncAndStart syncs the box's work tree to its READY boost b, as a job
// for tk, then starts the process; a sync not done within boostSyncWait
// answers pending SYNCING, and the start asked again with tk starts from
// it once done. Held tq.mu, which it lets go (*locked).
func (h environmentService) syncAndStart(ctx context.Context, c authz.Caller, e *environment, m *envv1alpha1.StartProcessRequest, command []string,
	b *envv1alpha1.Boost, tq *targetBoosts, boxKey controller.Key, tk *boostTicket, locked *bool,
	pending func(envv1alpha1.PendingReason, string, int, time.Duration) *envv1alpha1.StartProcessResponse,
) (*envv1alpha1.StartProcessResponse, error) {
	s := h.s
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: e.key.Project, Name: boostInstanceName(e.key.Name)})
	if err != nil {
		return nil, apiError(err)
	}
	if !ok || !owns(registry.Boost, b, in) {
		return pending(envv1alpha1.PendingReason_PENDING_REASON_BOOST_STARTING, "its boost's instance is being made again", 0, boostStartEstimate), nil
	}
	if tk.sync != nil && tk.sync.instance != in.GetMetadata().GetUid() {
		tk.sync = nil
	}
	if tk.sync == nil {
		// Another process of the box on the boost, running or starting:
		// what it wrote stays.
		shared := b.GetStatus().GetProcessesRunning() > 0 || tq.starting[boxKey] > 0
		tk.sync = s.startSync(e, b, in, shared, tq, boxKey)
	}
	run := tk.sync
	tq.mu.Unlock()
	*locked = false
	if !waitSync(ctx, run, s.syncWaitFor()) {
		return pending(envv1alpha1.PendingReason_PENDING_REASON_SYNCING, "the box's work tree is being synced to its boost", 0, s.syncWaitFor()), nil
	}
	tq.mu.Lock()
	delete(tq.tickets, tk.id)
	if run.err != nil {
		tq.mu.Unlock()
		return nil, syncError(e, run.err)
	}
	tq.starting[boxKey]++
	tq.mu.Unlock()
	return h.startOnBoost(ctx, c, e, m, command, b, tq, boxKey, run.stats)
}

// startOnBoost starts the process on the box's READY boost b, keeping
// where it runs in boxProcesses; if that cannot be kept, the process is
// killed, as no call could reach it. tq counts it as starting, which ends
// here.
func (h environmentService) startOnBoost(ctx context.Context, c authz.Caller, e *environment, m *envv1alpha1.StartProcessRequest, command []string, b *envv1alpha1.Boost, tq *targetBoosts, boxKey controller.Key, synced *syncStats) (*envv1alpha1.StartProcessResponse, error) {
	s := h.s
	defer func() {
		tq.mu.Lock()
		if tq.starting[boxKey]--; tq.starting[boxKey] <= 0 {
			delete(tq.starting, boxKey)
		}
		tq.used[boxKey] = s.clock.Now()
		tq.starts[boxKey]++
		tq.mu.Unlock()
		if s.boostCtrl != nil {
			s.boostCtrl.Enqueue(boxKey)
		}
	}()
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: e.key.Project, Name: boostInstanceName(e.key.Name)})
	if err != nil {
		return nil, apiError(err)
	}
	if !ok || !owns(registry.Boost, b, in) {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("%s's boost has no instance now; ask again", e))
	}
	size := boostSizeOf(b.GetSpec().GetResources())
	ref := provider.Instance{Project: e.key.Project, Name: in.GetMetadata().GetName()}
	// Under boost-job, which says what it changed relative to the tree
	// synced and measures its artifacts once it ends.
	runID, tree := randomID(), ""
	if synced != nil {
		tree = synced.Tree
	}
	wrapped := boostJobCommand(tree, reportPath(runID), m.GetArtifacts(), e.asRunAs(command, m.GetDir()))
	pid, err := e.p.StartProcess(ctx, provider.ProcessRequest{
		Instance: ref, Command: wrapped, Env: e.commandEnv(m.GetEnv()), Dir: m.GetDir(), Name: m.GetName(),
		MemoryMax: processMemoryMax(int64(min(size.memory, 1<<62))),
	})
	id := ""
	if err == nil {
		id = boostProcessID(in.GetMetadata().GetUid(), pid)
	}
	// Never the command's arguments or its environment's values: either may
	// hold a secret.
	s.log.Info("process started on a boost", "caller", c.String(), "environment", e.String(), "process", id, "program", command[0],
		"env_names", slices.Sorted(maps.Keys(m.GetEnv())), "privileged", e.secretEnv != nil, "boost", size.String(), "failed", err != nil)
	if err != nil {
		return nil, providerError(e, err)
	}
	at := ranOn{Kind: ranOnBoost, CPU: b.GetSpec().GetResources().GetCpu(), Memory: b.GetSpec().GetResources().GetMemory()}
	var repository string
	if cs := e.box.GetSpec().GetCheckouts(); len(cs) > 0 {
		repository = cs[0].GetRepository()
	}
	entry := boostProcess{
		ID: id, BoostUID: b.GetMetadata().GetUid(), Instance: ref.Name, InstanceUID: in.GetMetadata().GetUid(), ProviderID: pid,
		CPU: at.CPU, Memory: at.Memory, Name: m.GetName(), Coarse: coarseCommand(commandText(e.givenCommand(command))), Dir: m.GetDir(),
		Started: s.clock.Now(), RunID: runID, Artifacts: m.GetArtifacts(), Sync: synced,
	}
	err = changeRecord(ctx, s, boxProcessesKey(boxKey), func(r *boxProcesses) bool {
		if uid := e.box.GetMetadata().GetUid(); r.BoxUID != uid {
			*r = boxProcesses{BoxUID: uid}
		}
		r.Repository = repository
		r.add(entry)
		return true
	})
	if err != nil {
		if kerr := e.p.SignalProcess(context.WithoutCancel(ctx), ref, pid, provider.SignalKill); kerr != nil {
			s.log.Error("killing a boost process tower could not keep failed", "environment", e.String(), "process", id, "error", kerr)
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("keeping where %s's process runs failed, and it was killed: %w", e, err))
	}
	return &envv1alpha1.StartProcessResponse{Process: id, RanOn: at.proto(), Boost: boostRunOf(entry)}, nil
}

// boostProcessOf is the box's process of id run on its boost; NotFound
// if there is none.
func (s *Server) boostProcessOf(ctx context.Context, e *environment, id string) (boostProcess, error) {
	var rec boxProcesses
	if e.box != nil {
		if _, err := s.getRecord(ctx, boxProcessesKey(controller.Key{Project: e.key.Project, Name: e.key.Name}), &rec); err != nil {
			return boostProcess{}, apiError(err)
		}
	}
	p, ok := rec.find(id)
	if !ok || rec.BoxUID != e.box.GetMetadata().GetUid() {
		return boostProcess{}, connect.NewError(connect.CodeNotFound, fmt.Errorf("%s has no process %q", e, id))
	}
	return p, nil
}
