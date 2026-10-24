package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// useRecordKind holds one record per box of how it was used, which
// outlives the box until it is older than every warm window counts:
// repositories' use is counted from them. It is not an API kind; nothing
// serves or watches it.
const useRecordKind = "BoxUse"

// useRecordsKeptFor is how long a box's use record outlives its last use.
const useRecordsKeptFor = 90 * 24 * time.Hour

// useRecord is what useRecordKind holds.
type useRecord struct {
	// Repository is the box's first checkout's.
	Repository string    `json:"repository"`
	Opened     time.Time `json:"opened"`
	LastUse    time.Time `json:"lastUse"`
	// Seconds the box ran, and since when it runs uncounted.
	Seconds int64     `json:"seconds"`
	Since   time.Time `json:"since,omitzero"`
}

// lastUses are boxes' uses through Touch since the last flush, the calls
// into them under way, and the repositories boxes asked to fetch.
type lastUses struct {
	mu      sync.Mutex
	touched map[controller.Key]time.Time
	// inFlight counts each box's calls under way, which keep it in use.
	inFlight map[controller.Key]int
	// epoch moves at each use of a box; an idle stop decided before it
	// moved is not written (commitIdleStop).
	epoch map[controller.Key]uint64
	fetch map[controller.Key]bool
}

// Touch records that the box was used now, as the data plane does on every
// call to it. It is kept in memory and written to the box's
// status.last_use_time at the next flush, at most once a minute, so uses
// never move the store revision on their own.
func (s *Server) Touch(project, box string) {
	s.uses.mu.Lock()
	defer s.uses.mu.Unlock()
	s.touchLocked(controller.Key{Project: project, Name: box})
}

func (s *Server) touchLocked(key controller.Key) {
	if s.uses.touched == nil {
		s.uses.touched = map[controller.Key]time.Time{}
	}
	if s.uses.epoch == nil {
		s.uses.epoch = map[controller.Key]uint64{}
	}
	s.uses.touched[key] = s.clock.Now()
	s.uses.epoch[key]++
}

// beginUse counts a call into the box under way: while one is, the box is
// never idle. The returned func ends it, which is a use of the box.
func (s *Server) beginUse(project, box string) (end func()) {
	key := controller.Key{Project: project, Name: box}
	s.uses.mu.Lock()
	if s.uses.inFlight == nil {
		s.uses.inFlight = map[controller.Key]int{}
	}
	s.uses.inFlight[key]++
	s.touchLocked(key)
	s.uses.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.uses.mu.Lock()
			defer s.uses.mu.Unlock()
			if s.uses.inFlight[key]--; s.uses.inFlight[key] <= 0 {
				delete(s.uses.inFlight, key)
			}
			s.touchLocked(key)
		})
	}
}

// useOf is the box's use not flushed yet (zero if none), its use epoch,
// and whether a call into it is under way.
func (s *Server) useOf(key controller.Key) (last time.Time, epoch uint64, busy bool) {
	s.uses.mu.Lock()
	defer s.uses.mu.Unlock()
	return s.uses.touched[key], s.uses.epoch[key], s.uses.inFlight[key] > 0
}

// commitIdleStop writes an idle stop decided at epoch, unless the box has
// been used since or a call into it is under way: then it writes nothing
// and reports false. A use waits while the stop is written, so it either
// comes before, and stops it, or after, and finds the box stopped.
func (s *Server) commitIdleStop(key controller.Key, epoch uint64, write func() error) (bool, error) {
	s.uses.mu.Lock()
	defer s.uses.mu.Unlock()
	if s.uses.epoch[key] != epoch || s.uses.inFlight[key] > 0 {
		return false, nil
	}
	return true, write()
}

// boxFailed is a FAILED box's refusal, saying why it failed; nil for any
// other.
func boxFailed(b *envv1alpha1.Box) error {
	if b.GetStatus().GetPhase() != envv1alpha1.BoxPhase_BOX_PHASE_FAILED {
		return nil
	}
	c := conditions.Find(b.GetStatus().GetConditions(), BoxReady)
	return failedPrecondition("box %q failed: %s: %s", b.GetMetadata().GetName(), c.GetReason(), c.GetMessage())
}

// runLastUse flushes uses every LastUseFlushEvery until ctx is done, and
// has every box's idleness checked again.
func (s *Server) runLastUse(ctx context.Context) error {
	ticker := s.clock.NewTicker(LastUseFlushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.Chan():
		}
		if err := s.flushUses(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("flushing boxes' uses failed", "error", err)
		}
	}
}

// flushUses writes the uses touched since the last flush to the boxes, adds
// the running boxes' time to their use records, and queues every box, so
// one idle for long enough is stopped, and its repository, whose use is
// counted again.
func (s *Server) flushUses(ctx context.Context) error {
	// A use stays in memory until the box's status holds it, so idle()
	// sees it in one or the other all along; one that failed to be written
	// stays for the next flush.
	s.uses.mu.Lock()
	touched := maps.Clone(s.uses.touched)
	s.uses.mu.Unlock()
	var errs []error
	for key, at := range touched {
		if err := s.writeLastUse(ctx, key, at); err != nil {
			errs = append(errs, err)
			continue
		}
		s.uses.mu.Lock()
		if s.uses.touched[key].Equal(at) {
			delete(s.uses.touched, key)
		}
		s.uses.mu.Unlock()
	}
	boxes, err := listTyped[*envv1alpha1.Box](ctx, s, registry.Box, "")
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, b := range boxes {
		key := controller.Key{Project: b.GetMetadata().GetProject(), Name: b.GetMetadata().GetName()}
		if err := s.countRunning(ctx, key, b.GetStatus().GetPhase() == envv1alpha1.BoxPhase_BOX_PHASE_READY); err != nil {
			errs = append(errs, err)
		}
		s.boxCtrl.Enqueue(key)
		// Its repository's use is counted again from what was just counted.
		if cs := b.GetSpec().GetCheckouts(); len(cs) > 0 {
			s.repositoryCtrl.Enqueue(controller.Key{Project: key.Project, Name: cs[0].GetRepository()})
		}
	}
	errs = append(errs, s.pruneUseRecords(ctx))
	errs = append(errs, s.recheckTimed(ctx))
	return errors.Join(errs...)
}

// recheckTimed queues what waits on time alone, which no write wakes: a
// failed golden waiting out its backoff, and a target whose provider may
// have fallen silent.
func (s *Server) recheckTimed(ctx context.Context) error {
	goldens, err := listTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, "")
	if err != nil {
		return err
	}
	for _, g := range goldens {
		if goldenPhase(g) == envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED {
			s.goldenCtrl.Enqueue(controller.Key{Project: g.GetMetadata().GetProject(), Name: g.GetMetadata().GetName()})
		}
	}
	targets, err := listTyped[*targetsv1alpha1.Target](ctx, s, registry.Target, "")
	if err != nil {
		return err
	}
	for _, t := range targets {
		s.targetCtrl.Enqueue(controller.Key{Name: t.GetMetadata().GetName()})
	}
	return nil
}

// writeLastUse moves the box's status.last_use_time to at, if later.
func (s *Server) writeLastUse(ctx context.Context, key controller.Key, at time.Time) error {
	for range maxRetries {
		b, ok, err := getTyped[*envv1alpha1.Box](ctx, s, registry.Box, resource.Key{Project: key.Project, Name: key.Name})
		if err != nil || !ok {
			return err
		}
		if t := b.GetStatus().GetLastUseTime(); t != nil && !t.AsTime().Before(at) {
			return nil
		}
		st := proto.Clone(b.GetStatus()).(*envv1alpha1.BoxStatus)
		if st == nil {
			st = &envv1alpha1.BoxStatus{}
		}
		st.LastUseTime = timestamppb.New(at)
		err = s.updateStatus(ctx, registry.Box, b, st)
		if connect.CodeOf(err) != connect.CodeAborted {
			if err == nil {
				err = s.updateUseRecord(ctx, key, func(r *useRecord) { r.LastUse = maxTime(r.LastUse, at) })
			}
			return err
		}
	}
	return fmt.Errorf("too many writers of box %s", key)
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func useRecordKey(key controller.Key) store.Key {
	return store.Key{Kind: useRecordKind, Project: key.Project, Name: key.Name}
}

// useRecords are the use records of project's boxes.
func (s *Server) useRecords(ctx context.Context, project string) ([]useRecord, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: useRecordKind, Project: project})
	if err != nil {
		return nil, err
	}
	out := make([]useRecord, 0, len(objs))
	for _, o := range objs {
		var r useRecord
		if err := json.Unmarshal(o.Data, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// boxOpened makes the box's use record as it first runs, under its first
// checkout's repository.
func (s *Server) boxOpened(ctx context.Context, key controller.Key, repository string) error {
	now := s.clock.Now()
	data, err := json.Marshal(useRecord{Repository: repository, Opened: now, LastUse: now, Since: now})
	if err != nil {
		return err
	}
	_, err = s.store.Create(ctx, useRecordKey(key), data)
	if errors.Is(err, store.ErrExists) {
		return nil
	}
	return err
}

// updateUseRecord changes the box's use record, if it has one.
func (s *Server) updateUseRecord(ctx context.Context, key controller.Key, change func(*useRecord)) error {
	for range maxRetries {
		o, err := s.store.Get(ctx, useRecordKey(key))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var r useRecord
		if err := json.Unmarshal(o.Data, &r); err != nil {
			return err
		}
		before := r
		change(&r)
		if r == before {
			return nil
		}
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err := s.store.Update(ctx, o.Key, data, o.Revision); !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return fmt.Errorf("too many writers of the use record of box %s", key)
}

// countRunning adds the time the box ran since it was last counted.
func (s *Server) countRunning(ctx context.Context, key controller.Key, running bool) error {
	now := s.clock.Now()
	return s.updateUseRecord(ctx, key, func(r *useRecord) {
		if !r.Since.IsZero() {
			r.Seconds += int64(now.Sub(r.Since) / time.Second)
		}
		r.Since = time.Time{}
		if running {
			r.Since = now
		}
	})
}

// pruneUseRecords deletes use records no window counts any more.
func (s *Server) pruneUseRecords(ctx context.Context) error {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: useRecordKind})
	if err != nil {
		return err
	}
	for _, o := range objs {
		var r useRecord
		if json.Unmarshal(o.Data, &r) == nil && s.clock.Since(r.LastUse) > useRecordsKeptFor {
			if _, _, err := s.store.Delete(ctx, o.Key, o.Revision); err != nil && !errors.Is(err, store.ErrConflict) {
				return err
			}
		}
	}
	return nil
}

// Wake starts a box tower stopped for idleness and waits until it is
// READY, as the data plane does on the box's next use; a box already
// running is used at once. A box stopped at workstream's request stays
// stopped: that is FailedPrecondition, and so is a FAILED box, at once,
// with why. It returns once the box is READY, FAILED, being deleted, or
// ctx is done.
func (s *Server) Wake(ctx context.Context, project, box string) error {
	key := controller.Key{Project: project, Name: box}
	since, err := s.store.Revision(ctx)
	if err != nil {
		return err
	}
	// Used before it is read: an idle stop not yet written then is not
	// (commitIdleStop), and one written already is read below.
	s.Touch(project, box)
	b, ok, err := getTyped[*envv1alpha1.Box](ctx, s, registry.Box, resource.Key{Project: project, Name: box})
	if err != nil {
		return err
	}
	if !ok || b.GetMetadata().GetDeleteTime() != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("box %q in project %q not found", box, project))
	}
	if b.GetSpec().GetRunState() == envv1alpha1.RunState_RUN_STATE_STOPPED {
		return failedPrecondition("box %q is stopped at workstream's request; set its run_state to wake it", box)
	}
	if err := boxFailed(b); err != nil {
		return err
	}
	if b.GetStatus().GetStoppedReason() == stoppedIdle {
		st := proto.Clone(b.GetStatus()).(*envv1alpha1.BoxStatus)
		st.StoppedReason, st.Phase = "", envv1alpha1.BoxPhase_BOX_PHASE_STARTING
		if err := s.updateStatus(ctx, registry.Box, b, st); err != nil {
			return err
		}
	}
	s.boxCtrl.Enqueue(key)
	if b.GetStatus().GetPhase() == envv1alpha1.BoxPhase_BOX_PHASE_READY && b.GetStatus().GetStoppedReason() == "" {
		return nil
	}
	done := errors.New("woken")
	err = s.store.Watch(ctx, store.WatchOptions{Kind: registry.Box.Kind, Project: project, SinceRevision: since}, func(ev store.Event) error {
		if ev.Object.Name != box {
			return nil
		}
		if ev.Type == store.EventDeleted {
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("box %q was deleted", box))
		}
		o, err := s.backend(registry.Box).decode(ev.Object)
		if err != nil {
			return err
		}
		b := o.(*envv1alpha1.Box)
		if b.GetMetadata().GetDeleteTime() != nil {
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("box %q is being deleted", box))
		}
		if err := boxFailed(b); err != nil {
			return err
		}
		if b.GetStatus().GetPhase() == envv1alpha1.BoxPhase_BOX_PHASE_READY {
			return done
		}
		return nil
	})
	if errors.Is(err, done) {
		return nil
	}
	return err
}
