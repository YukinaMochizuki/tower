package apiserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/object"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// The policy controllers. Each reconciles one kind by key, reading the
// store; slow work through a provider runs as a job (jobs), never in a
// reconcile. See hangar: docs/checklists/new-controller.md.
func (s *Server) newControllers() []*controller.Controller {
	c := func(name string, rec func(context.Context, controller.Key) error, watches ...controller.Watch) *controller.Controller {
		return &controller.Controller{
			Name: name, Source: controller.StoreSource{Store: s.store}, Clock: s.clock, Log: s.log,
			Reconciler: controller.ReconcilerFunc(againOnConflict(rec)), Watches: watches,
		}
	}
	s.placementCtrl = c("placement", s.reconcilePlacement,
		controller.Watch{Kind: registry.Placement.Kind, Map: controller.Self},
		controller.Watch{Kind: registry.Target.Kind, Map: s.placementsOnTarget, SkipResync: true},
	)
	s.repositoryCtrl = c("repository", s.reconcileRepository,
		controller.Watch{Kind: registry.Repository.Kind, Map: controller.Self},
		controller.Watch{Kind: registry.Placement.Kind, Map: s.inProjectOfPlacement(registry.Repository), SkipResync: true},
		controller.Watch{Kind: registry.Golden.Kind, Map: s.repositoryOfWarm, SkipResync: true},
	)
	s.boxCtrl = c("box", s.reconcileBox,
		controller.Watch{Kind: registry.Box.Kind, Map: controller.Self},
		controller.Watch{Kind: registry.Placement.Kind, Map: s.inProjectOfPlacement(registry.Box), SkipResync: true},
		controller.Watch{Kind: registry.Instance.Kind, Map: s.ownerOf(registry.Box), SkipResync: true},
		controller.Watch{Kind: registry.Golden.Kind, Map: s.inProject(registry.Box), SkipResync: true},
		controller.Watch{Kind: registry.Repository.Kind, Map: s.inProject(registry.Box), SkipResync: true},
	)
	s.machineCtrl = c("machine", s.reconcileMachine,
		controller.Watch{Kind: registry.Machine.Kind, Map: controller.Self},
		controller.Watch{Kind: registry.Placement.Kind, Map: s.inProjectOfPlacement(registry.Machine), SkipResync: true},
		controller.Watch{Kind: registry.Instance.Kind, Map: s.ownerOf(registry.Machine), SkipResync: true},
		controller.Watch{Kind: registry.Golden.Kind, Map: s.inProject(registry.Machine), SkipResync: true},
	)
	s.goldenCtrl = c("golden", s.reconcileGolden,
		controller.Watch{Kind: registry.Golden.Kind, Map: controller.Self},
		controller.Watch{Kind: registry.Instance.Kind, Map: s.ownerOf(registry.Golden), SkipResync: true},
		// What references a golden going lets it be pruned.
		controller.Watch{Kind: registry.Box.Kind, Map: s.inProject(registry.Golden), SkipResync: true},
		controller.Watch{Kind: registry.Machine.Kind, Map: s.inProject(registry.Golden), SkipResync: true},
	)
	s.warmCtrl = c("warm", s.reconcileWarm,
		controller.Watch{Kind: registry.Target.Kind, Map: controller.Self},
		controller.Watch{Kind: registry.Repository.Kind, Map: s.targetOfProject, SkipResync: true},
		controller.Watch{Kind: registry.Golden.Kind, Map: goldenTarget, SkipResync: true},
		controller.Watch{Kind: registry.Placement.Kind, Map: placementTarget, SkipResync: true},
	)
	s.targetCtrl = c("target", s.reconcileTargetStatus,
		controller.Watch{Kind: registry.Target.Kind, Map: controller.Self},
		controller.Watch{Kind: registry.Instance.Kind, Map: instanceTarget, SkipResync: true},
		controller.Watch{Kind: registry.Golden.Kind, Map: goldenTarget, SkipResync: true},
	)
	return []*controller.Controller{s.placementCtrl, s.repositoryCtrl, s.boxCtrl, s.machineCtrl, s.goldenCtrl, s.warmCtrl, s.targetCtrl}
}

// againOnConflict runs a reconcile again at once when a write of it lost a
// race with another writer, such as a provider reporting status, rather
// than waiting for the controller's backoff: what it read is stale, and
// reading again is all it takes.
func againOnConflict(rec func(context.Context, controller.Key) error) func(context.Context, controller.Key) error {
	return func(ctx context.Context, key controller.Key) error {
		var err error
		for range maxRetries {
			if err = rec(ctx, key); connect.CodeOf(err) != connect.CodeAborted {
				return err
			}
		}
		return err
	}
}

// jobs runs the slow work controllers start, one job per name at a time.
// A job reports through the objects it writes and then queues its key
// again; while any runs, it counts as a controller's work in flight, so
// WaitSettled waits for it. Jobs live in memory: an apiserver restarted
// mid-job finds its object where the job left it and starts it again.
type jobs struct {
	mu      sync.Mutex
	running map[string]bool
	// failed is each job's last error, until it runs again.
	failed map[string]error
	wg     sync.WaitGroup
}

// start runs fn as the job of the name unless it runs already, and
// reports whether it started it. When fn returns, its error is kept for
// lastError and then key is queued on ctrl.
func (j *jobs) start(ctx context.Context, name string, ctrl *controller.Controller, key controller.Key, fn func(ctx context.Context) error) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running == nil {
		j.running, j.failed = map[string]bool{}, map[string]error{}
	}
	if j.running[name] || ctx.Err() != nil {
		return false
	}
	j.running[name] = true
	delete(j.failed, name)
	j.wg.Add(1)
	go func() {
		defer j.wg.Done()
		err := fn(ctx)
		j.mu.Lock()
		delete(j.running, name)
		if err != nil {
			j.failed[name] = err
		}
		j.mu.Unlock()
		ctrl.Enqueue(key)
	}()
	return true
}

// lastError is the error the job of the name last ended with, and whether
// it runs now.
func (j *jobs) lastError(name string) (err error, running bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.failed[name], j.running[name]
}

// forget drops the job's last error, once its object has said it.
func (j *jobs) forget(name string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.failed, name)
}

func (j *jobs) status() controller.Status {
	j.mu.Lock()
	defer j.mu.Unlock()
	// Jobs follow no writes: the controllers that start them do.
	return controller.Status{Name: "jobs", InFlight: len(j.running), Followed: math.MaxInt64}
}

// Errors controllers report when what they need is not there yet.
var (
	errNoProvider = errors.New("no provider is configured for the target")
)

// providerFor is the provider of the target.
func (s *Server) providerFor(target string) (provider.Provider, error) {
	providers := s.cfg.Providers
	if providers == nil {
		providers = s.defaultProvider
	}
	p, err := providers(target)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errNoProvider
	}
	return p, nil
}

// getTyped reads an object of kind r as stored, or the zero T if there is
// none.
func getTyped[T proto.Message](ctx context.Context, s *Server, r registry.Resource, key resource.Key) (T, bool, error) {
	var zero T
	o, err := s.get(ctx, r, key)
	if err != nil || o == nil {
		return zero, false, err
	}
	return o.(T), true, nil
}

// listTyped lists every object of kind r in project, every project when
// it is empty.
func listTyped[T proto.Message](ctx context.Context, s *Server, r registry.Resource, project string) ([]T, error) {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: r.Kind, Project: project})
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(objs))
	for _, o := range objs {
		m, err := s.backend(r).decode(o)
		if err != nil {
			return nil, err
		}
		out = append(out, m.(T))
	}
	return out, nil
}

// updateStatus writes obj's status, read at its resource version, as the
// system, unless it is what is stored already.
func (s *Server) updateStatus(ctx context.Context, r registry.Resource, obj proto.Message, status proto.Message) error {
	if proto.Equal(object.Status(obj), status) {
		return nil
	}
	meta := object.Meta(obj)
	rv := meta.GetResourceVersion()
	_, err := s.backend(r).UpdateStatus(s.system(ctx), resource.Key{Project: meta.GetProject(), Name: meta.GetName()}, status, &rv)
	return err
}

// childName is the name of what an object of the name makes, after its
// prefix, shortened with a hash when it would not be a name.
func childName(prefix, name string) string {
	n := prefix + "-" + name
	if len(n) <= 63 {
		return n
	}
	sum := sha256.Sum256([]byte(n))
	return n[:54] + "-" + hex.EncodeToString(sum[:])[:8]
}

// ownedBy is an owner reference to obj of kind r, as its controller.
func ownedBy(r registry.Resource, obj proto.Message) *metav1.OwnerReference {
	meta := object.Meta(obj)
	return &metav1.OwnerReference{ApiVersion: r.APIVersion(), Kind: r.Kind, Name: meta.GetName(), Uid: meta.GetUid(), Controller: true}
}

// owns reports whether owner, of kind r, is one of obj's owners by uid.
func owns(r registry.Resource, owner, obj proto.Message) bool {
	for _, ref := range object.Meta(obj).GetOwnerReferences() {
		if ref.GetKind() == r.Kind && ref.GetUid() == object.Meta(owner).GetUid() {
			return true
		}
	}
	return false
}

// placedTarget is the target a project's environments are made on now: its
// Placement's, when the Placement and its Target stand. reason says why
// there is none.
func (s *Server) placedTarget(ctx context.Context, project string) (*targetsv1alpha1.Target, string, error) {
	pl, ok, err := getTyped[*targetsv1alpha1.Placement](ctx, s, registry.Placement, resource.Key{Name: project})
	if err != nil {
		return nil, "", err
	}
	if !ok || pl.GetMetadata().GetDeleteTime() != nil {
		return nil, fmt.Sprintf("project %q has no Placement", project), nil
	}
	t, ok, err := s.liveTarget(ctx, pl.GetSpec().GetTarget())
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, fmt.Sprintf("the placement's target %q is gone", pl.GetSpec().GetTarget()), nil
	}
	return t, "", nil
}

// liveTarget is the target of the name, if it exists and is not being
// deleted.
func (s *Server) liveTarget(ctx context.Context, name string) (*targetsv1alpha1.Target, bool, error) {
	t, ok, err := getTyped[*targetsv1alpha1.Target](ctx, s, registry.Target, resource.Key{Name: name})
	if err != nil || !ok || t.GetMetadata().GetDeleteTime() != nil {
		return nil, false, err
	}
	return t, true, nil
}

// Maps from a changed object to the keys to reconcile.

// ownerOf maps an object to its owners of kind r.
func (s *Server) ownerOf(r registry.Resource) func(context.Context, store.Object) []controller.Key {
	return func(_ context.Context, o store.Object) []controller.Key {
		b, ok := s.backends[o.Kind]
		if !ok {
			return nil
		}
		obj, err := b.decode(o)
		if err != nil {
			return nil
		}
		var keys []controller.Key
		for _, ref := range object.Meta(obj).GetOwnerReferences() {
			if ref.GetKind() == r.Kind {
				keys = append(keys, controller.Key{Project: o.Project, Name: ref.GetName()})
			}
		}
		return keys
	}
}

// inProject maps a change in a project to every object of kind r there.
func (s *Server) inProject(r registry.Resource) func(context.Context, store.Object) []controller.Key {
	return func(ctx context.Context, o store.Object) []controller.Key {
		return s.keysIn(ctx, r, o.Project)
	}
}

// inProjectOfPlacement maps a Placement to every object of kind r of its
// project.
func (s *Server) inProjectOfPlacement(r registry.Resource) func(context.Context, store.Object) []controller.Key {
	return func(ctx context.Context, o store.Object) []controller.Key {
		return s.keysIn(ctx, r, o.Name)
	}
}

func (s *Server) keysIn(ctx context.Context, r registry.Resource, project string) []controller.Key {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: r.Kind, Project: project})
	if err != nil {
		return nil
	}
	keys := make([]controller.Key, 0, len(objs))
	for _, obj := range objs {
		keys = append(keys, controller.Key{Project: obj.Project, Name: obj.Name})
	}
	return keys
}

func (s *Server) placementsOnTarget(ctx context.Context, o store.Object) []controller.Key {
	pls, err := listTyped[*targetsv1alpha1.Placement](ctx, s, registry.Placement, "")
	if err != nil {
		return nil
	}
	var keys []controller.Key
	for _, pl := range pls {
		if pl.GetSpec().GetTarget() == o.Name {
			keys = append(keys, controller.Key{Name: pl.GetMetadata().GetName()})
		}
	}
	return keys
}

// targetOfProject maps an object of a project to its project's target.
func (s *Server) targetOfProject(ctx context.Context, o store.Object) []controller.Key {
	pl, ok, err := getTyped[*targetsv1alpha1.Placement](ctx, s, registry.Placement, resource.Key{Name: o.Project})
	if err != nil || !ok {
		return nil
	}
	return []controller.Key{{Name: pl.GetSpec().GetTarget()}}
}

func placementTarget(_ context.Context, o store.Object) []controller.Key {
	var pl targetsv1alpha1.Placement
	if proto.Unmarshal(o.Data, &pl) != nil {
		return nil
	}
	return []controller.Key{{Name: pl.GetSpec().GetTarget()}}
}

func goldenTarget(_ context.Context, o store.Object) []controller.Key {
	var g envv1alpha1.Golden
	if proto.Unmarshal(o.Data, &g) != nil {
		return nil
	}
	return []controller.Key{{Name: g.GetSpec().GetTarget()}}
}

func instanceTarget(_ context.Context, o store.Object) []controller.Key {
	var in envv1alpha1.Instance
	if proto.Unmarshal(o.Data, &in) != nil {
		return nil
	}
	return []controller.Key{{Name: in.GetSpec().GetTarget()}}
}

// isNotFound reports whether err says an object is not there.
func isNotFound(err error) bool { return connect.CodeOf(err) == connect.CodeNotFound }
