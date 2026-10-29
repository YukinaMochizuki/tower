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
// negative wait none, and the most artifacts copy back, for tests.
func (s *Server) SetBoostSync(wait time.Duration, artifactsMax int64) {
	s.syncWait, s.artifactsMax = wait, artifactsMax
}

// SetNoSettleJob keeps the boost controller from settling ended
// processes itself, for tests.
func (s *Server) SetNoSettleJob() { s.noSettleJob.Store(true) }

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
