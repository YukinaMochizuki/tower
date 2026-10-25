package apiserver

import (
	"context"
	"errors"
	"slices"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/object"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// gcProgress is how far garbage collection has come, which
// ControllerStatus reports as a controller's.
type gcProgress struct {
	mu sync.Mutex
	// sweeps counts the sweeps running.
	sweeps int
	// followed is the revision through which every write has been handled.
	followed int64
}

func (g *gcProgress) follow(rev int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.followed = max(g.followed, rev)
}

func (g *gcProgress) sweeping(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweeps += n
}

func (g *gcProgress) status() controller.Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	return controller.Status{Name: "garbage collector", InFlight: g.sweeps, Followed: g.followed}
}

// runGC deletes what owner references say should go. It follows writes:
//   - an object marked for deletion that cascades has everything it owns
//     deleted, and loses its finalizer once nothing is left;
//   - an object that is gone takes its dependents with it, and lets an owner
//     that was waiting on it finish.
//
// A sweep over everything runs on start and then periodically, and catches
// whatever following writes missed. Failures are logged, never fatal: the next
// sweep retries. Dependents are found through s.ownerIdx, which the start
// builds and the writes followed keep up to date.
func (s *Server) runGC(ctx context.Context) error {
	ticker := s.clock.NewTicker(s.cfg.SweepInterval)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.Chan():
				s.sweep(ctx)
			}
		}
	}()
	for {
		s.ownerIdx.invalidate()
		rev, err := s.store.Revision(ctx)
		if err != nil {
			return err
		}
		if err := s.indexOwners(ctx, rev); err != nil && ctx.Err() == nil {
			s.log.Error("garbage collection could not index owner references; it scans the store instead", "error", err)
		}
		s.sweep(ctx)
		s.gc.follow(rev)
		err = s.store.Watch(ctx, store.WatchOptions{SinceRevision: rev}, func(ev store.Event) error {
			defer s.gc.follow(ev.Revision)
			b, ok := s.backends[ev.Object.Kind]
			if !ok {
				return nil
			}
			obj, err := b.decode(ev.Object)
			if err != nil {
				s.log.Error("garbage collection could not decode an object", "kind", ev.Object.Kind, "name", ev.Object.Name, "error", err)
				return nil
			}
			// The index is updated first, so what follows finds a deleted
			// object no longer among its owners' dependents.
			newOwner := s.ownerIdx.apply(ev, ownerUIDs(obj))
			key := resource.Key{Project: ev.Object.Project, Name: ev.Object.Name}
			if ev.Type == store.EventDeleted {
				s.deleteDependents(ctx, object.Meta(obj).GetUid())
				s.finishOwners(ctx, key.Project, obj)
				return nil
			}
			// An owner reference the system set itself is not checked on
			// create, and its owner may have gone in the meantime; so may that
			// of one added later, whose owner's delete found nothing to take.
			if ev.Type == store.EventCreated || newOwner {
				s.collectIfOrphaned(ctx, b, key, obj)
			}
			if s.isCascading(b, obj) {
				s.cascade(ctx, b, key, obj)
			}
			return nil
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !errors.Is(err, store.ErrCompacted) {
			return err
		}
	}
}

// indexOwners builds s.ownerIdx from the store, listed once rev was read, so
// that following the writes after rev keeps it up to date. A write the
// listing already shows is applied again, which leaves it as it was.
func (s *Server) indexOwners(ctx context.Context, rev int64) error {
	m := newOwnerMap()
	err := s.eachObject(ctx, func(b *backend, key resource.Key, obj proto.Message) error {
		m.set(b.storeKey(key), ownerUIDs(obj))
		return nil
	})
	if err != nil {
		return err
	}
	s.ownerIdx.install(rev, m)
	return nil
}

func (s *Server) isCascading(b *backend, obj proto.Message) bool {
	meta := object.Meta(obj)
	return b.hooks.cascades && meta.GetDeleteTime() != nil && slices.Contains(meta.GetFinalizers(), dependentsFinalizer)
}

// sweep rechecks every object: orphans are deleted, and cascades that stalled
// are pushed on.
func (s *Server) sweep(ctx context.Context) {
	s.gc.sweeping(1)
	defer s.gc.sweeping(-1)
	err := s.eachObject(ctx, func(b *backend, key resource.Key, obj proto.Message) error {
		if s.isCascading(b, obj) {
			s.cascade(ctx, b, key, obj)
		}
		gone, err := s.orphaned(ctx, key, obj)
		if err != nil {
			return err
		}
		if gone {
			s.collect(ctx, b, key)
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		s.log.Error("garbage collection sweep failed", "error", err)
	}
}

// eachObject calls fn for every stored object of every kind.
func (s *Server) eachObject(ctx context.Context, fn func(b *backend, key resource.Key, obj proto.Message) error) error {
	for _, r := range registry.All {
		b := s.backend(r)
		objs, _, err := s.store.List(ctx, store.ListOptions{Kind: r.Kind})
		if err != nil {
			return err
		}
		for _, o := range objs {
			obj, err := b.decode(o)
			if err != nil {
				return err
			}
			if err := fn(b, resource.Key{Project: o.Project, Name: o.Name}, obj); err != nil {
				return err
			}
		}
	}
	return nil
}

// dependents calls fn for every object that may be owned by the uid, which
// fn reads and checks before it acts. Those s.ownerIdx holds are as of the
// writes followed so far; with complete, so are those written since, which
// a cascade needs before it concludes that nothing is left: a dependent
// written since is in the store, and its owner must not go before it.
func (s *Server) dependents(ctx context.Context, ownerUID string, complete bool, fn func(b *backend, key resource.Key)) error {
	keys, rev, ok := s.ownerIdx.lookup(ownerUID)
	var err error
	if ok && complete {
		keys, ok, err = s.ownedSince(ctx, ownerUID, rev, keys)
		if err != nil {
			return err
		}
	}
	if !ok {
		if keys, err = s.scanDependents(ctx, ownerUID); err != nil {
			return err
		}
	}
	for _, k := range keys {
		if b, ok := s.backends[k.Kind]; ok {
			fn(b, resource.Key{Project: k.Project, Name: k.Name})
		}
	}
	return nil
}

// errCaughtUp ends a watch that has reached the revision it was after.
var errCaughtUp = errors.New("caught up")

// ownedSince adds to keys those of the objects written after rev, through
// the store's current revision, that name the uid. It returns false if the
// store no longer holds the writes since rev.
func (s *Server) ownedSince(ctx context.Context, ownerUID string, rev int64, keys []store.Key) ([]store.Key, bool, error) {
	cur, err := s.store.Revision(ctx)
	if err != nil || cur <= rev {
		return keys, true, err
	}
	err = s.store.Watch(ctx, store.WatchOptions{SinceRevision: rev}, func(ev store.Event) error {
		b, ok := s.backends[ev.Object.Kind]
		if ok && ev.Type != store.EventDeleted && !slices.Contains(keys, ev.Object.Key) {
			obj, err := b.decode(ev.Object)
			if err != nil {
				return err
			}
			if slices.Contains(ownerUIDs(obj), ownerUID) {
				keys = append(keys, ev.Object.Key)
			}
		}
		if ev.Revision >= cur {
			return errCaughtUp
		}
		return nil
	})
	switch {
	case errors.Is(err, errCaughtUp):
		return keys, true, nil
	case errors.Is(err, store.ErrCompacted):
		return nil, false, nil
	default:
		return nil, false, err
	}
}

// scanDependents returns the keys of every object that names the uid, read
// from the whole store: what dependents does when the index cannot answer.
func (s *Server) scanDependents(ctx context.Context, ownerUID string) ([]store.Key, error) {
	var keys []store.Key
	err := s.eachObject(ctx, func(b *backend, key resource.Key, obj proto.Message) error {
		if slices.Contains(ownerUIDs(obj), ownerUID) {
			keys = append(keys, b.storeKey(key))
		}
		return nil
	})
	return keys, err
}

func (s *Server) deleteDependents(ctx context.Context, ownerUID string) {
	err := s.dependents(ctx, ownerUID, false, func(b *backend, key resource.Key) { s.collectOwned(ctx, b, key, ownerUID) })
	if err != nil && ctx.Err() == nil {
		s.log.Error("deleting dependents failed", "owner", ownerUID, "error", err)
	}
}

// cascade deletes what a cascading object owns, and removes its finalizer
// once nothing is left, which completes its deletion.
func (s *Server) cascade(ctx context.Context, b *backend, key resource.Key, obj proto.Message) {
	remaining := 0
	uid := object.Meta(obj).GetUid()
	err := s.dependents(ctx, uid, true, func(db *backend, dkey resource.Key) {
		if s.collectOwned(ctx, db, dkey, uid) {
			remaining++
		}
	})
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("cascading delete failed", "kind", b.r.Kind, "name", key.Name, "error", err)
		}
		return
	}
	if remaining > 0 {
		return
	}
	if b.hooks.beforeGone != nil {
		if err := b.hooks.beforeGone(ctx, s, key); err != nil {
			if ctx.Err() == nil {
				s.log.Error("an object being deleted cannot go yet", "kind", b.r.Kind, "project", key.Project, "name", key.Name, "error", err)
			}
			return
		}
	}
	if err := s.RemoveFinalizer(ctx, b.r, key, dependentsFinalizer); err != nil && ctx.Err() == nil {
		s.log.Error("finishing a cascading delete failed", "kind", b.r.Kind, "name", key.Name, "error", err)
	}
}

// finishOwners lets the owners of a deleted object finish their own deletion
// if it was the last thing they were waiting on.
func (s *Server) finishOwners(ctx context.Context, project string, deleted proto.Message) {
	for _, ref := range object.Meta(deleted).GetOwnerReferences() {
		r, ok := registry.Lookup(ref.GetApiVersion(), ref.GetKind())
		if !ok {
			continue
		}
		b := s.backend(r)
		key := resource.Key{Name: ref.GetName()}
		if r.ProjectScoped() {
			key.Project = ownerProject(project, ref)
		}
		o, err := s.store.Get(ctx, b.storeKey(key))
		if err != nil {
			continue
		}
		owner, err := b.decode(o)
		if err != nil || object.Meta(owner).GetUid() != ref.GetUid() {
			continue
		}
		if s.isCascading(b, owner) {
			s.cascade(ctx, b, key, owner)
		}
	}
}

// orphaned reports whether any of obj's owners is gone.
func (s *Server) orphaned(ctx context.Context, key resource.Key, obj proto.Message) (bool, error) {
	for _, ref := range object.Meta(obj).GetOwnerReferences() {
		alive, err := s.ownerAlive(ctx, key.Project, ref)
		if err != nil || !alive {
			return !alive, err
		}
	}
	return false, nil
}

func (s *Server) collectIfOrphaned(ctx context.Context, b *backend, key resource.Key, obj proto.Message) {
	gone, err := s.orphaned(ctx, key, obj)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("checking owners failed", "kind", b.r.Kind, "name", key.Name, "error", err)
		}
		return
	}
	if gone {
		// Only the object the event was about: a later one of the same name
		// has owners of its own. And only if it still names an owner that
		// is gone: a later write may have taken the reference back.
		cur, err := s.store.Get(ctx, b.storeKey(key))
		if err != nil {
			return
		}
		now, err := b.decode(cur)
		if err != nil || object.Meta(now).GetUid() != object.Meta(obj).GetUid() {
			return
		}
		if gone, err := s.orphaned(ctx, key, now); err != nil || !gone {
			return
		}
		rv := cur.Revision
		if _, err := b.Delete(s.system(ctx), key, &rv); err == nil {
			s.log.Info("collected", "kind", b.r.Kind, "project", key.Project, "name", key.Name)
		} else if c := connect.CodeOf(err); c != connect.CodeNotFound && c != connect.CodeAborted && ctx.Err() == nil {
			s.log.Error("collecting failed", "kind", b.r.Kind, "project", key.Project, "name", key.Name, "error", err)
		}
	}
}

func (s *Server) ownerAlive(ctx context.Context, project string, ref *metav1.OwnerReference) (bool, error) {
	r, ok := registry.Lookup(ref.GetApiVersion(), ref.GetKind())
	if !ok {
		return false, nil
	}
	if r.ProjectScoped() {
		project = ownerProject(project, ref)
	} else {
		project = ""
	}
	o, err := s.store.Get(ctx, store.Key{Kind: r.Kind, Project: project, Name: ref.GetName()})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	owner, err := s.backend(r).decode(o)
	if err != nil {
		return false, err
	}
	return object.Meta(owner).GetUid() == ref.GetUid(), nil
}

// ownerProject is the project of a project-scoped owner: the one its
// reference names, for a global dependent, or else the dependent's own.
func ownerProject(dependentProject string, ref *metav1.OwnerReference) string {
	if ref.GetProject() != "" {
		return ref.GetProject()
	}
	return dependentProject
}

// collect deletes one object as the system. A failure is logged rather than
// returned: the next sweep tries again.
func (s *Server) collect(ctx context.Context, b *backend, key resource.Key) {
	_, err := b.Delete(s.system(ctx), key, nil)
	switch {
	case err == nil:
		s.log.Info("collected", "kind", b.r.Kind, "project", key.Project, "name", key.Name)
	case connect.CodeOf(err) != connect.CodeNotFound && ctx.Err() == nil:
		s.log.Error("collecting failed", "kind", b.r.Kind, "project", key.Project, "name", key.Name, "error", err)
	}
}

// collectOwned deletes the object at key as the system if it names the
// uid as an owner, and reports whether it did when read, so it was there to
// wait for. The delete is conditional on what was read, so an object that
// stopped naming the uid, or another of its name, is never taken. A failure
// is logged rather than returned: the next sweep tries again.
func (s *Server) collectOwned(ctx context.Context, b *backend, key resource.Key, ownerUID string) (owned bool) {
	for range maxRetries {
		cur, err := s.store.Get(ctx, b.storeKey(key))
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
				s.log.Error("reading a dependent failed", "kind", b.r.Kind, "project", key.Project, "name", key.Name, "error", err)
			}
			return false
		}
		obj, err := b.decode(cur)
		if err != nil {
			s.log.Error("garbage collection could not decode an object", "kind", b.r.Kind, "name", key.Name, "error", err)
			return false
		}
		if !slices.Contains(ownerUIDs(obj), ownerUID) {
			return false
		}
		rv := cur.Revision
		_, err = b.Delete(s.system(ctx), key, &rv)
		switch c := connect.CodeOf(err); {
		case err == nil:
			s.log.Info("collected", "kind", b.r.Kind, "project", key.Project, "name", key.Name)
			return true
		case c == connect.CodeAborted:
			continue
		case c == connect.CodeNotFound:
			return false
		default:
			if ctx.Err() == nil {
				s.log.Error("collecting failed", "kind", b.r.Kind, "project", key.Project, "name", key.Name, "error", err)
			}
			return true
		}
	}
	return true
}

// RemoveFinalizer removes a finalizer from an object as the system, and
// deletes the object if it was being deleted and that was the last one.
func (s *Server) RemoveFinalizer(ctx context.Context, r registry.Resource, key resource.Key, finalizer string) error {
	b := s.backend(r)
	sk := b.storeKey(key)
	for range maxRetries {
		cur, err := s.store.Get(ctx, sk)
		if err != nil {
			return apiError(err)
		}
		obj, err := b.decode(cur)
		if err != nil {
			return err
		}
		meta := object.Meta(obj)
		if !slices.Contains(meta.GetFinalizers(), finalizer) {
			return nil
		}
		meta.Finalizers = slices.DeleteFunc(meta.Finalizers, func(f string) bool { return f == finalizer })
		if meta.GetDeleteTime() != nil && len(meta.GetFinalizers()) == 0 {
			_, _, err = s.store.Delete(ctx, sk, cur.Revision)
		} else {
			var data []byte
			if data, err = encode(obj); err == nil {
				_, err = s.store.Update(ctx, sk, data, cur.Revision)
			}
		}
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		return apiError(err)
	}
	return connect.NewError(connect.CodeAborted, errors.New("too many concurrent writers; retry"))
}
