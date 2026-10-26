package apiserver

import (
	"context"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/authz"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// A retry asked for runs a FAILED golden's, box's or machine's failed work
// now rather than when its backoff runs out (docs/decisions/failures.md):
// it deletes the records that count the failures, so the work's gate
// finds none, and queues the object, whose reconcile runs it as it runs
// work that never failed. A golden runs again as one whose backoff passed
// does (retryGolden): it is deleted, and made again by what asks for it.

// retryService serves RetryService.
type retryService struct{ s *Server }

// begin refuses whoever may not retry, naming only what the request
// named, and reads the object as the caller, for the answer: an object
// the caller may not read, or that is not there, is refused as a Get is.
func (h retryService) begin(ctx context.Context, r registry.Resource, key resource.Key) (authz.Caller, proto.Message, error) {
	b := h.s.backend(r)
	c, err := b.begin(ctx)
	if err != nil {
		return c, nil, err
	}
	if !authz.MayRetry(c) {
		return c, nil, denied("%s may not retry %s: only ROLE_ADMIN and ROLE_WORKSTREAM do", c, b.asked(key))
	}
	obj, err := b.Get(ctx, key)
	return c, obj, err
}

func (h retryService) RetryGolden(ctx context.Context, req *connect.Request[envv1alpha1.RetryGoldenRequest]) (*connect.Response[envv1alpha1.RetryGoldenResponse], error) {
	key := resource.Key{Project: req.Msg.GetProject(), Name: req.Msg.GetName()}
	c, obj, err := h.begin(ctx, registry.Golden, key)
	if err != nil {
		return nil, err
	}
	res := &envv1alpha1.RetryGoldenResponse{Golden: obj.(*envv1alpha1.Golden)}
	g, ok, err := h.s.liveGolden(ctx, key.Project, key.Name)
	if err != nil {
		return nil, apiError(err)
	}
	if ok && goldenPhase(g) == envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED {
		if err := h.s.retryFailedGolden(ctx, g, c); err != nil {
			return nil, apiError(err)
		}
		res.Retried = true
	}
	return connect.NewResponse(res), nil
}

func (h retryService) RetryBox(ctx context.Context, req *connect.Request[envv1alpha1.RetryBoxRequest]) (*connect.Response[envv1alpha1.RetryBoxResponse], error) {
	key := resource.Key{Project: req.Msg.GetProject(), Name: req.Msg.GetName()}
	c, obj, err := h.begin(ctx, registry.Box, key)
	if err != nil {
		return nil, err
	}
	res := &envv1alpha1.RetryBoxResponse{Box: obj.(*envv1alpha1.Box)}
	box, ok, err := getTyped[*envv1alpha1.Box](ctx, h.s, registry.Box, key)
	if err != nil {
		return nil, apiError(err)
	}
	if !ok {
		return connect.NewResponse(res), nil
	}
	if box.GetStatus().GetPhase() == envv1alpha1.BoxPhase_BOX_PHASE_FAILED {
		if err := h.s.retryFailed(ctx, c, "box", key, box.GetStatus().GetImageGolden(), h.s.boxCtrl); err != nil {
			return nil, apiError(err)
		}
		res.Retried = true
	}
	// Fallen back from its devcontainer's image, which did not build: that
	// image is built again now, for the boxes made after; the box stays on
	// the image it fell back to.
	if dev := box.GetStatus().GetDevcontainerGolden(); dev != "" && fellBack(box.GetStatus()) {
		g, live, err := h.s.liveGolden(ctx, key.Project, dev)
		if err != nil {
			return nil, apiError(err)
		}
		if live && goldenPhase(g) == envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED {
			if err := h.s.retryFailedGolden(ctx, g, c); err != nil {
				return nil, apiError(err)
			}
			res.Retried = true
		}
	}
	return connect.NewResponse(res), nil
}

func (h retryService) RetryMachine(ctx context.Context, req *connect.Request[envv1alpha1.RetryMachineRequest]) (*connect.Response[envv1alpha1.RetryMachineResponse], error) {
	key := resource.Key{Project: req.Msg.GetProject(), Name: req.Msg.GetName()}
	c, obj, err := h.begin(ctx, registry.Machine, key)
	if err != nil {
		return nil, err
	}
	res := &envv1alpha1.RetryMachineResponse{Machine: obj.(*envv1alpha1.Machine)}
	m, ok, err := getTyped[*envv1alpha1.Machine](ctx, h.s, registry.Machine, key)
	if err != nil {
		return nil, apiError(err)
	}
	if ok && m.GetStatus().GetPhase() == envv1alpha1.MachinePhase_MACHINE_PHASE_FAILED {
		if err := h.s.retryFailed(ctx, c, "machine", key, m.GetStatus().GetGolden(), h.s.machineCtrl); err != nil {
			return nil, apiError(err)
		}
		res.Retried = true
	}
	return connect.NewResponse(res), nil
}

// retryFailedGolden forgets the failures of a FAILED golden's inputs and
// queues it, so its reconcile deletes it to be built again at once. The
// FAILED goldens made on it (a warm checkout on its image), which failed
// with it, are retried too: each is built again once it is
// (retryGolden).
func (s *Server) retryFailedGolden(ctx context.Context, g *envv1alpha1.Golden, by authz.Caller) error {
	if err := s.deleteRecord(ctx, goldenBackoffKey(g)); err != nil {
		return err
	}
	meta := g.GetMetadata()
	s.log.Info("retrying a failed golden", "project", meta.GetProject(), "name", meta.GetName(), "by", by.String())
	enqueue(s.goldenCtrl, controller.Key{Project: meta.GetProject(), Name: meta.GetName()})
	goldens, err := listTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, meta.GetProject())
	if err != nil {
		return err
	}
	for _, child := range goldens {
		if child.GetSpec().GetParent() == meta.GetName() && child.GetMetadata().GetDeleteTime() == nil &&
			goldenPhase(child) == envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED {
			if err := s.retryFailedGolden(ctx, child, by); err != nil {
				return err
			}
		}
	}
	return nil
}

// retryFailed runs the failed work of a FAILED box or machine (kind, as
// its step records name it) again: its golden, if that failed, and every
// instance step whose failures are counted. It then queues the object.
func (s *Server) retryFailed(ctx context.Context, by authz.Caller, kind string, key resource.Key, golden string, ctrl *controller.Controller) error {
	if golden != "" {
		g, ok, err := s.liveGolden(ctx, key.Project, golden)
		if err != nil {
			return err
		}
		if ok && goldenPhase(g) == envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED {
			if err := s.retryFailedGolden(ctx, g, by); err != nil {
				return err
			}
		}
	}
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: stepBackoffKind, Project: key.Project})
	if err != nil {
		return err
	}
	for _, o := range objs {
		if strings.HasPrefix(o.Name, kind+"/"+key.Name+"/") {
			if err := s.deleteRecord(ctx, o.Key); err != nil {
				return err
			}
		}
	}
	s.log.Info("retrying the failed work of a "+kind, "project", key.Project, "name", key.Name, "by", by.String())
	enqueue(ctrl, controller.Key{Project: key.Project, Name: key.Name})
	return nil
}

// enqueue queues key on ctrl, if this apiserver runs its controllers.
func enqueue(ctrl *controller.Controller, key controller.Key) {
	if ctrl != nil {
		ctrl.Enqueue(key)
	}
}
