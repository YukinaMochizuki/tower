package apiserver

import (
	"context"
	"fmt"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// SetBeforeIdleStop runs fn between a box's idle stop being decided and its
// being written, for tests of what happens between.
func (s *Server) SetBeforeIdleStop(fn func(project, box string)) {
	hook := func(k controller.Key) { fn(k.Project, k.Name) }
	s.beforeIdleStop.Store(&hook)
}

// SetBeforeBoostRetire runs fn between an idle boost's exits being read
// and its deletion being decided, for tests of what happens between.
func (s *Server) SetBeforeBoostRetire(fn func(project, box string)) {
	hook := func(k controller.Key) { fn(k.Project, k.Name) }
	s.beforeBoostRetire.Store(&hook)
}

// SetBoostSync sets how long a boosted start waits for its sync, a
// negative wait none, and the most one process's changes bring back,
// for tests.
func (s *Server) SetBoostSync(wait time.Duration, backMax int64) {
	s.syncWait.Store(int64(wait))
	s.backMax = backMax
}

// SetWakeRoomWait sets how long Wake waits for a box waiting for room on
// its node, for tests.
func (s *Server) SetWakeRoomWait(wait time.Duration) { s.wakeRoomWait.Store(int64(wait)) }

// SetNoSettleJob keeps the boost controller from settling ended
// processes itself, for tests.
func (s *Server) SetNoSettleJob() { s.noSettleJob.Store(true) }

// SetBeforeBoostStart runs fn as a boosted start, holding the box's sync
// lock, is about to start its process, for tests of what happens then.
func (s *Server) SetBeforeBoostStart(fn func(project, box string)) {
	hook := func(k controller.Key) { fn(k.Project, k.Name) }
	s.beforeBoostStart.Store(&hook)
}

// BoostSyncsWaiting is how many syncs of the box wait for its sync lock.
func (s *Server) BoostSyncsWaiting(target, project, box string) int {
	tq := s.boosts.of(target)
	tq.mu.Lock()
	defer tq.mu.Unlock()
	return tq.syncWaiting[controller.Key{Project: project, Name: box}]
}

// InUse reports whether a call into the box is under way.
func (s *Server) InUse(project, box string) bool {
	_, _, busy := s.useOf(controller.Key{Project: project, Name: box})
	return busy
}

// RecordExits records the exits ps of a box as a call that read them does,
// with that call's ctx.
func (s *Server) RecordExits(ctx context.Context, project, box string, ps []provider.Process) error {
	b, ok, err := getTyped[*envv1alpha1.Box](context.Background(), s, registry.Box, resource.Key{Project: project, Name: box})
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("box %s not found", box)
	}
	e := &environment{kind: "box", key: resource.Key{Project: project, Name: box}, box: b, ref: provider.Instance{Project: project, Name: b.GetStatus().GetInstance()}}
	res := s.instanceResources(ctx, e)
	listed := make([]listedProcess, 0, len(ps))
	for _, p := range ps {
		listed = append(listed, listedProcess{Process: p, at: ranOn{Kind: ranOnBox, CPU: res.GetCpu(), Memory: res.GetMemory()}})
	}
	s.exitsListed(ctx, e, listed, true)
	return nil
}

// SettleBoostProcess settles the box's boost process id, on its boost's
// instance, as reading its exit does; ErrWriteBackLater if its box is not
// ready for its changes yet.
func (s *Server) SettleBoostProcess(ctx context.Context, target, project, box, instance, id string) error {
	p, err := s.providerFor(target)
	if err != nil {
		return err
	}
	_, err = s.settleBoostProcess(ctx, p, controller.Key{Project: project, Name: box}, provider.Instance{Project: project, Name: instance}, id)
	return err
}

// ErrWriteBackLater is errWriteBackLater, for tests.
var ErrWriteBackLater = errWriteBackLater

// SetDeferWriteBack defers the write-back of each boost process defer
// says, as for a box not ready, until it says otherwise.
func (s *Server) SetDeferWriteBack(deferred func(id string) bool) { s.deferWriteBack.Store(&deferred) }
