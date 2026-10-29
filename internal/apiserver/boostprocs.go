package apiserver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
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
// from it, and a boost deleted leaves its processes' exits here. It goes
// with its box. Not an API kind.
const boxProcessesKind = "BoxProcesses"

// boostProcessesKept bounds a box's boost processes kept, those of boosts
// deleted going first.
const boostProcessesKept = 100

// boostProcessPrefix starts the id of every process run on a boost; a
// box's own are its provider's, which never start so (provider ids are
// "p" and a number on the fake, a guest's on hangar).
const boostProcessPrefix = "boost-"

// boxProcesses is what boxProcessesKind holds.
type boxProcesses struct {
	// BoxUID is the box's: a box of the name made again starts afresh.
	BoxUID string         `json:"boxUid"`
	Boost  []boostProcess `json:"boost"`
}

// boostProcess is a process run on a box's boost.
type boostProcess struct {
	// ID is tower's: boostProcessPrefix, the boost's uid's end, and
	// ProviderID, its id on Instance.
	ID         string `json:"id"`
	BoostUID   string `json:"boostUid"`
	Instance   string `json:"instance"`
	ProviderID string `json:"providerId"`
	// CPU and Memory are the boost's size.
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	Name   string `json:"name,omitempty"`
	// Command is its command line cut short, as ListProcesses shows it.
	Command string    `json:"command"`
	Dir     string    `json:"dir,omitempty"`
	Started time.Time `json:"started"`
	// Exit is how it ended, once read.
	Exit *provider.ProcessExit `json:"exit,omitempty"`
	// Gone says its boost is deleted.
	Gone bool `json:"gone,omitempty"`
}

func boxProcessesKey(box controller.Key) store.Key {
	return store.Key{Kind: boxProcessesKind, Project: box.Project, Name: box.Name}
}

// boostGone marks the processes of the boost of uid as of a boost
// deleted, reporting whether any was not.
func (r *boxProcesses) boostGone(uid string) bool {
	changed := false
	for i := range r.Boost {
		if r.Boost[i].BoostUID == uid && !r.Boost[i].Gone {
			r.Boost[i].Gone, changed = true, true
		}
	}
	return changed
}

// add keeps p, dropping the oldest past boostProcessesKept, those of
// boosts deleted first.
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

// boostProcessID is tower's id of the process pid on the boost of uid: a
// boost made again has another uid, so no id is used twice. The uid's
// end is its random part (a uid starts with when it was made).
func boostProcessID(uid, pid string) string {
	short := strings.ReplaceAll(uid, "-", "")
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
	// kept says it is read from boxProcesses, its boost deleted: its
	// usage was recorded before the boost went, and its command is cut
	// short.
	kept bool
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

// liveBoost is the box's boost the process p ran on, if it is still
// there with its instance.
func (s *Server) liveBoost(ctx context.Context, e *environment, p boostProcess) (*envv1alpha1.Boost, bool, error) {
	if p.Gone {
		return nil, false, nil
	}
	b, ok, err := getTyped[*envv1alpha1.Boost](ctx, s, registry.Boost, e.key)
	if err != nil || !ok || b.GetMetadata().GetUid() != p.BoostUID || b.GetStatus().GetInstance() != p.Instance {
		return nil, false, err
	}
	return b, true, nil
}

// boxProcessesOf lists e's processes, the box's own and its boost's,
// keeping the exits of its boost's read now. With a box's only ones
// ([]provider.Process from its instance), it is them as they are.
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
	var rec boxProcesses
	if _, err := s.getRecord(ctx, boxProcessesKey(controller.Key{Project: e.key.Project, Name: e.key.Name}), &rec); err != nil || len(rec.Boost) == 0 || rec.BoxUID != e.box.GetMetadata().GetUid() {
		return out, err
	}
	live := map[string]provider.Process{}
	b, ok, err := getTyped[*envv1alpha1.Boost](ctx, s, registry.Boost, e.key)
	if err != nil {
		return nil, err
	}
	if ok && b.GetStatus().GetInstance() != "" {
		bps, err := e.p.ListProcesses(ctx, provider.Instance{Project: e.key.Project, Name: b.GetStatus().GetInstance()})
		if err != nil {
			// A boost not running yet, or going: its processes as kept.
			s.log.Warn("listing a boost's processes failed", "environment", e.String(), "error", err)
		}
		for _, p := range bps {
			live[p.ID] = p
		}
	}
	var exits []boostProcess
	for _, bp := range rec.Boost {
		at := ranOn{Kind: ranOnBoost, CPU: bp.CPU, Memory: bp.Memory}
		if p, ok := live[bp.ProviderID]; ok && !bp.Gone && b.GetMetadata().GetUid() == bp.BoostUID {
			p.ID = bp.ID
			out = append(out, listedProcess{Process: p, at: at})
			if p.Exit != nil && bp.Exit == nil {
				bp.Exit = p.Exit
				exits = append(exits, bp)
			}
			continue
		}
		if bp.Exit == nil && !bp.Gone {
			// Its boost is starting again, or its exit not listed yet.
			continue
		}
		out = append(out, listedProcess{Process: provider.Process{
			ID: bp.ID, Name: bp.Name, Command: []string{bp.Command}, Dir: bp.Dir, Started: bp.Started, Exit: bp.Exit,
		}, at: at, kept: true})
	}
	if len(exits) > 0 {
		err := changeRecord(ctx, s, boxProcessesKey(controller.Key{Project: e.key.Project, Name: e.key.Name}), func(r *boxProcesses) bool {
			changed := false
			for _, x := range exits {
				for i := range r.Boost {
					if r.Boost[i].ID == x.ID && r.Boost[i].Exit == nil {
						r.Boost[i].Exit, changed = x.Exit, true
					}
				}
			}
			return changed
		})
		if err != nil {
			s.log.Error("keeping a boost's processes' exits failed", "environment", e.String(), "error", err)
		}
	}
	return out, nil
}

// keepBoostExits reads the exits of b's processes into boxProcesses and
// records their usage, before b goes; running says one still runs, and b
// stays.
func (s *Server) keepBoostExits(ctx context.Context, boxKey controller.Key, b *envv1alpha1.Boost) (running bool, err error) {
	if b.GetStatus().GetInstance() == "" {
		return false, nil
	}
	box, ok, err := getTyped[*envv1alpha1.Box](ctx, s, registry.Box, resource.Key{Project: boxKey.Project, Name: boxKey.Name})
	if err != nil || !ok {
		return false, err
	}
	p, err := s.providerFor(b.GetSpec().GetTarget())
	if err != nil {
		return false, fmt.Errorf("%w: %w", controller.ErrWaiting, err)
	}
	e := &environment{key: resource.Key{Project: boxKey.Project, Name: boxKey.Name}, kind: "box", box: box, p: p,
		ref: provider.Instance{Project: boxKey.Project, Name: box.GetStatus().GetInstance()}}
	ps, err := p.ListProcesses(ctx, provider.Instance{Project: boxKey.Project, Name: b.GetStatus().GetInstance()})
	if err != nil {
		if b.GetStatus().GetPhase() == envv1alpha1.BoostPhase_BOOST_PHASE_READY && !errors.Is(err, provider.ErrNotFound) {
			return false, err
		}
		// One never READY, or gone from its target, has none to keep.
		return false, nil
	}
	if slices.ContainsFunc(ps, func(p provider.Process) bool { return p.Exit == nil }) {
		return true, nil
	}
	var rec boxProcesses
	if _, err := s.getRecord(ctx, boxProcessesKey(boxKey), &rec); err != nil {
		return false, err
	}
	var listed []listedProcess
	exits := map[string]*provider.ProcessExit{}
	for _, bp := range rec.Boost {
		if bp.BoostUID != b.GetMetadata().GetUid() {
			continue
		}
		i := slices.IndexFunc(ps, func(p provider.Process) bool { return p.ID == bp.ProviderID })
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
	err = changeRecord(ctx, s, boxProcessesKey(boxKey), func(r *boxProcesses) bool {
		changed := false
		for i := range r.Boost {
			if x, ok := exits[r.Boost[i].ID]; ok && r.Boost[i].Exit == nil {
				r.Boost[i].Exit, changed = x, true
			}
		}
		return changed
	})
	if err != nil {
		return false, err
	}
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
// pending at once, with a ticket, why, and about how long.
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
	ticket := func(admitted bool) *boostTicket {
		if tk == nil {
			tk = &boostTicket{id: newTicketID(), box: boxKey, workstream: e.workstream, size: size, arrived: now}
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
		s.log.Info("a boosted start pending", "caller", c.String(), "environment", e.String(), "ticket", tk.id,
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
		if have.covers(size) {
			switch b.GetStatus().GetPhase() {
			case envv1alpha1.BoostPhase_BOOST_PHASE_READY:
				if tk != nil {
					delete(tq.tickets, tk.id)
				}
				tq.starting[boxKey]++
				tq.mu.Unlock()
				locked = false
				return h.startOnBoost(ctx, c, e, m, command, b, tq, boxKey)
			case envv1alpha1.BoostPhase_BOOST_PHASE_FAILED:
				if tk != nil {
					delete(tq.tickets, tk.id)
				}
				return nil, failedPrecondition("%s's boost failed: %s; it is deleted once unused for the target's boost_idle (%s), and the next boosted start makes another",
					e, readyMsg, set.boostIdle)
			}
			ticket(true)
			wait := max(boostStartEstimate-s.clock.Since(b.GetMetadata().GetCreateTime().AsTime()), 5*time.Second)
			return pending(envv1alpha1.PendingReason_PENDING_REASON_BOOST_STARTING, "its boost is starting: "+readyMsg, 0, wait), nil
		}
		ticket(false)
		if b.GetStatus().GetProcessesRunning() > 0 || tq.starting[boxKey] > 0 {
			return pending(envv1alpha1.PendingReason_PENDING_REASON_BOOST_BUSY, fmt.Sprintf(
				"its boost (%s) is smaller than asked (%s) and runs a process: it is made again larger once its processes end", have, size), 0, 0), nil
		}
		s.retireBoost(boxKey, b, fmt.Sprintf("a larger one (%s) is asked for", size))
		return pending(envv1alpha1.PendingReason_PENDING_REASON_BOOST_STARTING, fmt.Sprintf(
			"its boost (%s) is smaller than asked (%s): it is made again larger", have, size), 0, boostStartEstimate), nil
	}
	ticket(false)
	if err := s.admitBoosts(ctx, e.target, tq); err != nil {
		return nil, apiError(err)
	}
	if tk.admitted {
		return pending(envv1alpha1.PendingReason_PENDING_REASON_BOOST_STARTING, "its boost is being made", 0, boostStartEstimate), nil
	}
	boosts, err := s.targetBoostsOf(ctx, e.target.GetMetadata().GetName())
	if err != nil {
		return nil, apiError(err)
	}
	order := tq.order()
	position := slices.Index(order, tk) + 1
	room := roomOf(e.target, boosts)
	msg := fmt.Sprintf("the target's boosts hold %s of its boost_budget of %s", room.held, room.budget)
	if room.hasCPU || room.hasMemory {
		msg += fmt.Sprintf(", and its Plan has %s left", room.quota)
	}
	msg += fmt.Sprintf("; a boost of %s waits its turn, %d of %d", size, position, len(order))
	wait, known := s.estimateWait(e.target, tq, boosts, order, tk)
	if !known {
		msg += "; it does not fit even once every boost is gone, until the Plan has more left"
	}
	return pending(envv1alpha1.PendingReason_PENDING_REASON_QUEUED_BUDGET, msg, position, wait), nil
}

// startOnBoost starts the process on the box's READY boost b, keeping
// where it runs in boxProcesses. tq counts it as starting, which ends
// here.
func (h environmentService) startOnBoost(ctx context.Context, c authz.Caller, e *environment, m *envv1alpha1.StartProcessRequest, command []string, b *envv1alpha1.Boost, tq *targetBoosts, boxKey controller.Key) (*envv1alpha1.StartProcessResponse, error) {
	s := h.s
	defer func() {
		tq.mu.Lock()
		if tq.starting[boxKey]--; tq.starting[boxKey] <= 0 {
			delete(tq.starting, boxKey)
		}
		tq.used[boxKey] = s.clock.Now()
		tq.mu.Unlock()
		if s.boostCtrl != nil {
			s.boostCtrl.Enqueue(boxKey)
		}
	}()
	size := boostSizeOf(b.GetSpec().GetResources())
	ref := provider.Instance{Project: e.key.Project, Name: b.GetStatus().GetInstance()}
	pid, err := e.p.StartProcess(ctx, provider.ProcessRequest{
		Instance: ref, Command: e.asRunAs(command, m.GetDir()), Env: e.commandEnv(m.GetEnv()), Dir: m.GetDir(), Name: m.GetName(),
		MemoryMax: processMemoryMax(int64(min(size.memory, 1<<62))),
	})
	id := ""
	if err == nil {
		id = boostProcessID(b.GetMetadata().GetUid(), pid)
	}
	// Never the command's arguments or its environment's values: either may
	// hold a secret.
	s.log.Info("process started on a boost", "caller", c.String(), "environment", e.String(), "process", id, "program", command[0],
		"env_names", slices.Sorted(maps.Keys(m.GetEnv())), "privileged", e.secretEnv != nil, "boost", size.String(), "failed", err != nil)
	if err != nil {
		return nil, providerError(e, err)
	}
	at := ranOn{Kind: ranOnBoost, CPU: b.GetSpec().GetResources().GetCpu(), Memory: b.GetSpec().GetResources().GetMemory()}
	err = changeRecord(ctx, s, boxProcessesKey(boxKey), func(r *boxProcesses) bool {
		if uid := e.box.GetMetadata().GetUid(); r.BoxUID != uid {
			*r = boxProcesses{BoxUID: uid}
		}
		r.add(boostProcess{
			ID: id, BoostUID: b.GetMetadata().GetUid(), Instance: ref.Name, ProviderID: pid, CPU: at.CPU, Memory: at.Memory,
			Name: m.GetName(), Command: commandSummary(e.givenCommand(command)), Dir: m.GetDir(), Started: s.clock.Now(),
		})
		return true
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("process %s started on %s's boost, and keeping where it runs failed: %w", id, e, err))
	}
	return &envv1alpha1.StartProcessResponse{Process: id, RanOn: at.proto()}, nil
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
