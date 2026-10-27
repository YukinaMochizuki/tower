package apiserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/authz"
	"github.com/YukinaMochizuki/tower/internal/controller"
)

// TargetTeardownFinalizer holds a Target being deleted until what tower
// made on its target is gone: its Goldens, with their artifacts, and on a
// hangar target every object its provider made there
// (docs/decisions/placement.md).
const TargetTeardownFinalizer = registry.Domain + "/teardown"

// teardownEvery is how often a provider's teardown that left something on
// its target passes again: hangar deletes in its own time.
const teardownEvery = 2 * time.Second

// tearer is a provider that makes more on its target than Goldens'
// artifacts, and deletes it when its Target goes: hangar's.
type tearer interface {
	Teardown(ctx context.Context) (left []string, err error)
}

// teardowns are the last pass of each teardown under way, by target.
type teardowns struct {
	mu   sync.Mutex
	last map[string]teardownPass
}

// teardownPass is what a pass of a provider's teardown found left.
type teardownPass struct {
	// uid is the Target's: a later Target of the name starts afresh.
	uid  string
	at   time.Time
	left []string
	err  error
	// listed says a pass, this one or one before, listed the target: left
	// is all there is, not what a pass that failed early saw.
	listed bool
}

func (td *teardowns) get(target, uid string) (teardownPass, bool) {
	td.mu.Lock()
	defer td.mu.Unlock()
	p, ok := td.last[target]
	return p, ok && p.uid == uid
}

// set keeps a pass; one that failed before it listed anything keeps what
// the pass before it saw.
func (td *teardowns) set(target string, p teardownPass) {
	td.mu.Lock()
	defer td.mu.Unlock()
	if td.last == nil {
		td.last = map[string]teardownPass{}
	}
	p.listed = p.err == nil || p.left != nil
	if prev, ok := td.last[target]; ok && prev.uid == p.uid {
		if !p.listed {
			p.left = prev.left
		}
		p.listed = p.listed || prev.listed
	}
	td.last[target] = p
}

func (td *teardowns) forget(target string) {
	td.mu.Lock()
	defer td.mu.Unlock()
	delete(td.last, target)
}

// ensureTeardownFinalizer gives a Target made before its kind carried
// TargetTeardownFinalizer the finalizer.
func (s *Server) ensureTeardownFinalizer(ctx context.Context, t *targetsv1alpha1.Target) error {
	meta := t.GetMetadata()
	if slices.Contains(meta.GetFinalizers(), TargetTeardownFinalizer) {
		return nil
	}
	return s.addFinalizer(ctx, registry.Target, resource.Key{Name: meta.GetName()}, meta.GetUid(), TargetTeardownFinalizer)
}

// reconcileTeardown tears down a Target being deleted: it deletes the
// Goldens made on it, whose artifacts the golden controller deletes
// through the provider, then has a provider that makes more delete it, and
// lets the Target go once nothing is left. Meanwhile the Target's status
// says what is left, and last_error what keeps it.
func (s *Server) reconcileTeardown(ctx context.Context, t *targetsv1alpha1.Target) error {
	meta := t.GetMetadata()
	name, uid := meta.GetName(), meta.GetUid()
	if !slices.Contains(meta.GetFinalizers(), TargetTeardownFinalizer) {
		return nil
	}
	goldens, err := listTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, "")
	if err != nil {
		return err
	}
	var remaining []string
	for _, g := range goldens {
		gm := g.GetMetadata()
		if g.GetSpec().GetTarget() != name {
			continue
		}
		remaining = append(remaining, "golden "+gm.GetProject()+"/"+gm.GetName())
		if gm.GetDeleteTime() != nil {
			continue
		}
		rv := gm.GetResourceVersion()
		_, err := s.backend(registry.Golden).Delete(s.system(ctx), resource.Key{Project: gm.GetProject(), Name: gm.GetName()}, &rv)
		switch c := connect.CodeOf(err); {
		case err == nil:
			s.log.Info("deleting a golden of a target being deleted", "target", name, "project", gm.GetProject(), "golden", gm.GetName())
		case c != connect.CodeNotFound && c != connect.CodeAborted:
			return err
		}
	}
	// What tower made on a hostd target is its Goldens' artifacts alone,
	// which it knows; on a hangar target, what a pass listed.
	last, ran := s.teardowns.get(name, uid)
	known := t.GetSpec().GetHangar() == nil || t.GetStatus().GetRemainingKnown() || (ran && last.listed)
	if len(remaining) > 0 {
		// Each golden going queues the target again (goldenTarget).
		return s.teardownStatus(ctx, t, remaining, known, s.lastJobError(name))
	}
	var td tearer
	if t.GetSpec().GetHangar() != nil {
		p, err := s.providerFor(name)
		if err != nil {
			if serr := s.teardownStatus(ctx, t, last.left, known, err.Error()); serr != nil {
				return serr
			}
			// A provider tower runs queues the target as it starts (hangar.go).
			return fmt.Errorf("tearing down target %s: %w: %w", name, controller.ErrWaiting, err)
		}
		td, _ = p.(tearer)
	}
	if td == nil {
		return s.letTargetGo(ctx, name, uid)
	}
	job := "teardown/" + name
	if _, running := s.jobs.lastError(job); running {
		return nil
	}
	s.jobs.forget(job)
	if ran && last.err == nil && len(last.left) == 0 {
		return s.letTargetGo(ctx, name, uid)
	}
	if ran {
		if err := s.teardownStatus(ctx, t, last.left, known, s.lastJobError(name)); err != nil {
			return err
		}
		if s.clock.Since(last.at) < teardownEvery {
			return fmt.Errorf("tearing down target %s: %d left: %w", name, len(last.left), controller.ErrWaiting)
		}
	}
	s.startJob(name, job, s.targetCtrl, controller.Key{Name: name}, func(ctx context.Context) error {
		left, err := td.Teardown(ctx)
		s.teardowns.set(name, teardownPass{uid: uid, at: s.clock.Now(), left: left, err: err})
		if err != nil {
			return fmt.Errorf("tearing down: %w", err)
		}
		return nil
	})
	return nil
}

// letTargetGo removes a torn down Target's finalizer, which lets it go,
// then deletes the ssh key tower made for that Target's VMs, none of
// which is left.
func (s *Server) letTargetGo(ctx context.Context, name, uid string) error {
	err := s.removeFinalizerOf(ctx, registry.Target, resource.Key{Name: name}, uid, TargetTeardownFinalizer)
	if errors.Is(err, errOtherUID) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.deleteHangarSSHKey(ctx, name, uid); err != nil {
		// The Target is gone; the hangar target controller deletes it
		// as it hears so (reconcileHangarTarget).
		s.log.Warn("deleting a gone target's ssh key failed", "target", name, "error", err)
	}
	s.teardowns.forget(name)
	s.log.Info("tore down a target: nothing tower made is left on it", "target", name)
	return nil
}

// teardownStatus writes what a teardown found left into the Target's
// status, whether that is all there is, and what keeps it as last_error.
func (s *Server) teardownStatus(ctx context.Context, t *targetsv1alpha1.Target, remaining []string, known bool, lastErr string) error {
	st, _ := proto.Clone(t.GetStatus()).(*targetsv1alpha1.TargetStatus)
	if st == nil {
		st = &targetsv1alpha1.TargetStatus{}
	}
	st.Remaining, st.RemainingKnown, st.LastError = remaining, known, lastErr
	return s.updateStatus(ctx, registry.Target, t, st)
}

// targetActionService serves TargetActionService.
type targetActionService struct{ s *Server }

// AbandonTarget removes a Target's teardown finalizer without its
// teardown done, saying, to the caller and in the log, what it last found
// left on the target.
func (h targetActionService) AbandonTarget(ctx context.Context, req *connect.Request[targetsv1alpha1.AbandonTargetRequest]) (*connect.Response[targetsv1alpha1.AbandonTargetResponse], error) {
	b := h.s.backend(registry.Target)
	key := resource.Key{Name: req.Msg.GetName()}
	c, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	if !authz.MayAbandonTargets(c) {
		return nil, denied("%s may not abandon %s: only ROLE_ADMIN and root on the host do", c, b.asked(key))
	}
	obj, err := b.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	t := obj.(*targetsv1alpha1.Target)
	if t.GetMetadata().GetDeleteTime() == nil {
		return nil, failedPrecondition("target %q is not being deleted: delete it, and abandon it only if its teardown cannot finish", key.Name)
	}
	unconfirmed, known := t.GetStatus().GetRemaining(), t.GetStatus().GetRemainingKnown()
	// Only the Target read: one made again since is not abandoned.
	err = h.s.removeFinalizerOf(ctx, registry.Target, key, t.GetMetadata().GetUid(), TargetTeardownFinalizer)
	if errors.Is(err, errOtherUID) {
		return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("target %q was made again since it was read; nothing was abandoned", key.Name))
	}
	if err != nil {
		return nil, err
	}
	h.s.teardowns.forget(key.Name)
	if err := h.s.deleteHangarSSHKey(ctx, key.Name, t.GetMetadata().GetUid()); err != nil {
		h.s.log.Warn("deleting an abandoned target's ssh key failed", "target", key.Name, "error", err)
	}
	h.s.log.Warn("abandoned a target's teardown: what it last found left is not confirmed gone",
		"target", key.Name, "by", c.String(), "unconfirmed", unconfirmed, "listed", known, "last_error", t.GetStatus().GetLastError())
	return connect.NewResponse(&targetsv1alpha1.AbandonTargetResponse{Target: t, Unconfirmed: unconfirmed, UnconfirmedKnown: known}), nil
}
