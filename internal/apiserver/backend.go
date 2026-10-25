package apiserver

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/labels"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/object"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/authz"
	"github.com/YukinaMochizuki/tower/internal/store"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
	// maxRetries bounds how often a write without an expected version
	// retries after losing a race with another writer.
	maxRetries = 10
)

// backend implements every operation of one kind on top of the store; the
// rules it enforces are in hangar: docs/decisions/apply-semantics.md.
type backend struct {
	s     *Server
	r     registry.Resource
	hooks kindHooks
}

var _ resource.Backend = (*backend)(nil)

func (b *backend) storeKey(k resource.Key) store.Key {
	return store.Key{Kind: b.r.Kind, Project: k.Project, Name: k.Name}
}

func (b *backend) decode(o store.Object) (proto.Message, error) {
	m := b.r.Message.New().Interface()
	if err := proto.Unmarshal(o.Data, m); err != nil {
		return nil, fmt.Errorf("decode %s %s: %w", b.r.Kind, o.Name, err)
	}
	object.SetTypeMeta(m, b.r.APIVersion(), b.r.Kind)
	object.Meta(m).ResourceVersion = o.Revision
	return m, nil
}

// encode serializes an object for storage. The resource version is left out:
// the store's revision is the only source of it.
func encode(m proto.Message) ([]byte, error) {
	c := proto.Clone(m)
	object.Meta(c).ResourceVersion = 0
	return proto.Marshal(c)
}

// begin authenticates the call and returns its caller, with a service
// token's role as its ServiceToken gives it now.
func (b *backend) begin(ctx context.Context) (authz.Caller, error) {
	return b.s.begin(ctx)
}

// begin returns the caller of ctx. A service token's role is read from its
// ServiceToken at every call, so a token deleted, or made again with
// another role, counts at once; a token whose ServiceToken is gone or being
// deleted holds none.
func (s *Server) begin(ctx context.Context) (authz.Caller, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return authz.Caller{}, connect.NewError(connect.CodeUnauthenticated, auth.ErrNoCredentials)
	}
	c := authz.Caller{Principal: p}
	if p.Type == auth.ServiceToken {
		role, target, err := s.roleOf(ctx, p.ID)
		if err != nil {
			return c, apiError(err)
		}
		c.Role, c.Target = role, target
	}
	return c, nil
}

func (b *backend) allowed(p authz.Caller, verb authz.Verb) bool {
	return authz.Allowed(p, b.r, verb)
}

// authorize checks a grant. The denial names only what the caller asked
// for, never anything read from the stored object; a role decides by the
// kind alone, so it comes before anything is read.
func (b *backend) authorize(p authz.Caller, verb authz.Verb, asked string) error {
	if !b.allowed(p, verb) {
		return denied("%s may not %s %s", p, verb, asked)
	}
	return nil
}

// asked describes the object a call named, in the words of the request.
func (b *backend) asked(k resource.Key) string {
	if k.Project != "" {
		return fmt.Sprintf("%s %q in project %q", b.r.Plural, k.Name, k.Project)
	}
	return fmt.Sprintf("%s %q", b.r.Plural, k.Name)
}

func (b *backend) askedIn(project string) string {
	if project != "" {
		return fmt.Sprintf("%s in project %q", b.r.Plural, project)
	}
	return b.r.Plural
}

func (b *backend) checkKey(k resource.Key) error {
	if b.r.ProjectScoped() {
		if k.Project == "" {
			return invalid("%s are project-scoped; project is required", b.r.Plural)
		}
		if err := ValidName(k.Project); err != nil {
			return invalid("project: %v", err)
		}
	} else if k.Project != "" {
		return invalid("%s are global; project must be empty", b.r.Plural)
	}
	if err := ValidName(k.Name); err != nil {
		return invalid("name: %v", err)
	}
	return nil
}

func (b *backend) Get(ctx context.Context, key resource.Key) (proto.Message, error) {
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	_, obj, err := b.read(ctx, p, authz.Get, key)
	if err != nil {
		return nil, err
	}
	b.answer(obj)
	return obj, nil
}

// answer clears from obj, in place, what no answer holds
// (kindHooks.redact). Every object a call or event answers goes through it.
func (b *backend) answer(obj proto.Message) {
	if obj == nil {
		return
	}
	if b.hooks.redact != nil {
		b.hooks.redact(obj)
	}
}

// read fetches an object for an action and authorizes it. Its answer never
// tells a caller without the grant whether the object exists:
// authorization comes before the read.
func (b *backend) read(ctx context.Context, p authz.Caller, verb authz.Verb, key resource.Key) (store.Object, proto.Message, error) {
	if err := b.authorize(p, verb, b.asked(key)); err != nil {
		return store.Object{}, nil, err
	}
	cur, err := b.s.store.Get(ctx, b.storeKey(key))
	if errors.Is(err, store.ErrNotFound) && authz.OwnTargetOnly(p) {
		return store.Object{}, nil, b.notOwn(p, verb, key)
	}
	if err != nil {
		return store.Object{}, nil, apiError(err)
	}
	obj, err := b.decode(cur)
	if err != nil {
		return store.Object{}, nil, apiError(err)
	}
	if !b.ownTarget(p, obj) {
		return store.Object{}, nil, b.notOwn(p, verb, key)
	}
	return cur, obj, nil
}

// notOwn is what a caller held to its own target is told of an object that
// is not there or is another target's: not found, alike for both, so it
// learns nothing of other targets, and a hostd reads only NotFound as an
// object gone (internal/hostd releases on it alone).
func (b *backend) notOwn(p authz.Caller, verb authz.Verb, key resource.Key) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s cannot %s %s: not found on its target", p, verb, b.asked(key)))
}

// ownTarget reports whether p may see obj at all: whoever is not held to
// its own target may; a caller who is, only an object of that target.
func (b *backend) ownTarget(p authz.Caller, obj proto.Message) bool {
	if !authz.OwnTargetOnly(p) {
		return true
	}
	return b.hooks.targetOf != nil && p.Target != "" && b.hooks.targetOf(obj) == p.Target
}

func (b *backend) List(ctx context.Context, opts resource.ListOptions) (resource.ListResult, error) {
	p, err := b.begin(ctx)
	if err != nil {
		return resource.ListResult{}, err
	}
	sel, err := labels.Parse(opts.LabelSelector)
	if err != nil {
		return resource.ListResult{}, invalid("%v", err)
	}
	if err := b.authorize(p, authz.List, b.askedIn(opts.Project)); err != nil {
		return resource.ListResult{}, err
	}
	after, err := decodePageToken(opts.PageToken)
	if err != nil {
		return resource.ListResult{}, err
	}
	size := int(opts.PageSize)
	if size <= 0 {
		size = defaultPageSize
	}
	size = min(size, maxPageSize)
	if authz.OwnTargetOnly(p) {
		return b.listOwnTarget(ctx, p, opts.Project, sel, after, size)
	}
	objs, rev, err := b.s.store.List(ctx, store.ListOptions{Kind: b.r.Kind, Project: opts.Project, StartAfter: after, Limit: size})
	if err != nil {
		return resource.ListResult{}, apiError(err)
	}
	res := resource.ListResult{Revision: rev}
	for _, o := range objs {
		obj, err := b.decode(o)
		if err != nil {
			return resource.ListResult{}, apiError(err)
		}
		if !b.ownTarget(p, obj) || !sel.Matches(object.Meta(obj).GetLabels()) {
			continue
		}
		b.answer(obj)
		res.Items = append(res.Items, obj)
	}
	// A full page means there may be more, even if filtering emptied it.
	if len(objs) == size {
		res.NextPageToken = encodePageToken(objs[len(objs)-1].Key)
	}
	return res, nil
}

// listOwnTarget lists for a caller that sees only its own target's
// objects. It reads on past what it may not see until its page is full or
// the kind ends, so a next page token names only an object it returned:
// a token naming an object of another target would tell that the object
// exists. The revision is the first read's, which the rest are at or
// after.
func (b *backend) listOwnTarget(ctx context.Context, p authz.Caller, project string, sel labels.Selector, after store.Key, size int) (resource.ListResult, error) {
	var res resource.ListResult
	for first := true; ; first = false {
		objs, rev, err := b.s.store.List(ctx, store.ListOptions{Kind: b.r.Kind, Project: project, StartAfter: after, Limit: size})
		if err != nil {
			return resource.ListResult{}, apiError(err)
		}
		if first {
			res.Revision = rev
		}
		for _, o := range objs {
			after = o.Key
			obj, err := b.decode(o)
			if err != nil {
				return resource.ListResult{}, apiError(err)
			}
			if !b.ownTarget(p, obj) || !sel.Matches(object.Meta(obj).GetLabels()) {
				continue
			}
			b.answer(obj)
			res.Items = append(res.Items, obj)
			if len(res.Items) == size {
				res.NextPageToken = encodePageToken(o.Key)
				return res, nil
			}
		}
		if len(objs) < size {
			return res, nil
		}
	}
}

func encodePageToken(k store.Key) string {
	return base64.RawURLEncoding.EncodeToString([]byte(k.Project + "/" + k.Name))
}

func decodePageToken(tok string) (store.Key, error) {
	if tok == "" {
		return store.Key{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return store.Key{}, invalid("malformed page token")
	}
	project, name, ok := strings.Cut(string(raw), "/")
	if !ok {
		return store.Key{}, invalid("malformed page token")
	}
	return store.Key{Project: project, Name: name}, nil
}

func (b *backend) Watch(ctx context.Context, opts resource.WatchOptions, send func(resource.Event) error) error {
	p, err := b.begin(ctx)
	if err != nil {
		return err
	}
	sel, err := labels.Parse(opts.LabelSelector)
	if err != nil {
		return invalid("%v", err)
	}
	if err := b.mayWatch(p, opts.Project); err != nil {
		return err
	}
	// The watch stands only as long as its caller may still watch at all.
	auth.Recheck(ctx, func(ctx context.Context) error {
		p, err := b.begin(ctx)
		if err != nil {
			return err
		}
		return b.mayWatch(p, opts.Project)
	})
	since := opts.SinceRevision
	if since == 0 {
		if since, err = b.s.store.Revision(ctx); err != nil {
			return apiError(err)
		}
	}
	// The opening bookmark tells the client where the watch starts, and makes
	// the response begin before the first change, which may be far off.
	if err := send(resource.Event{Type: metav1.EventType_EVENT_TYPE_BOOKMARK, Revision: since}); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	progress := make(chan struct{})
	ticker := b.s.cfg.BookmarkClock.NewTicker(b.s.cfg.BookmarkInterval)
	defer ticker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.Chan():
				select {
				case progress <- struct{}{}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	err = b.s.store.Watch(ctx, store.WatchOptions{
		Kind: b.r.Kind, Project: opts.Project, SinceRevision: since, Progress: progress,
	}, func(ev store.Event) error {
		if ev.Type == store.EventProgress {
			return send(resource.Event{Type: metav1.EventType_EVENT_TYPE_BOOKMARK, Revision: ev.Revision})
		}
		obj, err := b.decode(ev.Object)
		if err != nil {
			return err
		}
		if !b.ownTarget(p, obj) || !sel.Matches(object.Meta(obj).GetLabels()) {
			return nil
		}
		b.answer(obj)
		return send(resource.Event{Type: eventType(ev.Type), Object: obj, Revision: ev.Revision})
	})
	// The caller's deadline reaches the server a little before the caller,
	// so say it ran out rather than end the stream as if it were complete.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return connect.NewError(connect.CodeDeadlineExceeded, ctx.Err())
	}
	if ctx.Err() != nil {
		return nil
	}
	return apiError(err)
}

// mayWatch authorizes a watch in project, or everywhere when it is empty.
func (b *backend) mayWatch(p authz.Caller, project string) error {
	return b.authorize(p, authz.Watch, b.askedIn(project))
}

func eventType(t store.EventType) metav1.EventType {
	switch t {
	case store.EventCreated:
		return metav1.EventType_EVENT_TYPE_ADDED
	case store.EventUpdated:
		return metav1.EventType_EVENT_TYPE_MODIFIED
	case store.EventDeleted:
		return metav1.EventType_EVENT_TYPE_DELETED
	}
	return metav1.EventType_EVENT_TYPE_UNSPECIFIED
}

func (b *backend) Apply(ctx context.Context, in proto.Message) (resource.ApplyResult, error) {
	return b.answered(ctx, in, false)
}

// answered applies in and passes the result through answer, as the caller
// is answered.
func (b *backend) answered(ctx context.Context, in proto.Message, createOnly bool) (resource.ApplyResult, error) {
	res, err := b.apply(ctx, in, createOnly)
	if err == nil {
		b.answer(res.Object)
	}
	return res, err
}

// Create writes in only if no object has its key, and fails with
// AlreadyExists otherwise, once the caller is authorized as an update would
// be. It serves every create_only Apply RPC, CreateServiceToken, and
// controllers: one that read "absent" must not overwrite an object written
// since.
func (b *backend) Create(ctx context.Context, in proto.Message) (resource.ApplyResult, error) {
	return b.answered(ctx, in, true)
}

func (b *backend) apply(ctx context.Context, in proto.Message, createOnly bool) (resource.ApplyResult, error) {
	p, err := b.begin(ctx)
	if err != nil {
		return resource.ApplyResult{}, err
	}
	in = proto.Clone(in)
	apiVersion, kind := object.TypeMeta(in)
	if apiVersion != "" && apiVersion != b.r.APIVersion() || kind != "" && kind != b.r.Kind {
		return resource.ApplyResult{}, invalid("apiVersion %q kind %q does not match %s %s", apiVersion, kind, b.r.APIVersion(), b.r.Kind)
	}
	object.SetTypeMeta(in, b.r.APIVersion(), b.r.Kind)
	meta := object.Meta(in)
	key := resource.Key{Project: meta.GetProject(), Name: meta.GetName()}
	if err := b.checkKey(key); err != nil {
		return resource.ApplyResult{}, err
	}
	// The kind decides who may write; checking it first keeps the rest of
	// the call from telling outsiders anything about the object, a
	// create-only apply's AlreadyExists included.
	if err := b.authorize(p, authz.Apply, b.asked(key)); err != nil {
		return resource.ApplyResult{}, err
	}
	expected := meta.GetResourceVersion()
	if createOnly && expected != 0 {
		return resource.ApplyResult{}, invalid("a create-only apply takes no resourceVersion")
	}

	for range maxRetries {
		cur, err := b.s.store.Get(ctx, b.storeKey(key))
		switch {
		case errors.Is(err, store.ErrNotFound):
			if expected != 0 {
				return resource.ApplyResult{}, connect.NewError(connect.CodeNotFound,
					fmt.Errorf("%s %s does not exist at resource version %d", b.r.Kind, key.Name, expected))
			}
			res, err := b.create(ctx, p, key, in)
			// Made since the read: read again, so a create-only apply is
			// told it exists, and a plain one updates it.
			if errors.Is(err, store.ErrExists) {
				continue
			}
			return res, apiError(err)
		case err != nil:
			return resource.ApplyResult{}, apiError(err)
		case createOnly:
			return resource.ApplyResult{}, b.exists(key)
		}
		res, err := b.update(ctx, p, key, cur, in, expected)
		// Without an expected version Apply is create-or-update, so an object
		// changed or deleted since it was read is read again: deleted, it is
		// created.
		if (errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound)) && expected == 0 {
			continue
		}
		return res, apiError(err)
	}
	return resource.ApplyResult{}, connect.NewError(connect.CodeAborted, errors.New("too many concurrent writers; retry"))
}

func (b *backend) exists(key resource.Key) error {
	return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s %s already exists", b.r.Kind, key.Name))
}

func (b *backend) create(ctx context.Context, p authz.Caller, key resource.Key, in proto.Message) (resource.ApplyResult, error) {
	meta := object.Meta(in)
	if err := checkMetaKeys(p, meta, nil); err != nil {
		return resource.ApplyResult{}, err
	}
	if err := b.s.admit(ctx, b, p, nil, in); err != nil {
		return resource.ApplyResult{}, err
	}
	if p.Type != auth.System {
		meta.Finalizers = nil
		meta.OwnerReferences = nil
	}
	owner, err := b.s.ownerRef(ctx, b, in)
	if err != nil {
		return resource.ApplyResult{}, err
	}
	if owner != nil && !slices.ContainsFunc(meta.OwnerReferences, func(r *metav1.OwnerReference) bool { return r.GetUid() == owner.GetUid() }) {
		meta.OwnerReferences = append(meta.OwnerReferences, owner)
	}
	if b.hooks.cascades && !slices.Contains(meta.Finalizers, dependentsFinalizer) {
		meta.Finalizers = append(meta.Finalizers, dependentsFinalizer)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return resource.ApplyResult{}, err
	}
	meta.Uid = id.String()
	meta.CreateTime = timestamppb.New(b.s.clock.Now())
	meta.Generation = 1
	meta.DeleteTime = nil
	object.SetStatus(in, nil)
	if b.hooks.seal != nil {
		if err := b.hooks.seal(b.s, nil, in); err != nil {
			return resource.ApplyResult{}, err
		}
	}
	data, err := encode(in)
	if err != nil {
		return resource.ApplyResult{}, err
	}
	rev, err := b.s.store.Create(ctx, b.storeKey(key), data)
	if err != nil {
		return resource.ApplyResult{}, err
	}
	meta.ResourceVersion = rev
	return resource.ApplyResult{Object: in, Created: true}, nil
}

func (b *backend) update(ctx context.Context, p authz.Caller, key resource.Key, cur store.Object, in proto.Message, expected int64) (resource.ApplyResult, error) {
	old, err := b.decode(cur)
	if err != nil {
		return resource.ApplyResult{}, err
	}
	if expected != 0 && expected != cur.Revision {
		return resource.ApplyResult{}, connect.NewError(connect.CodeAborted,
			fmt.Errorf("%s %s is at resource version %d, not %d", b.r.Kind, key.Name, cur.Revision, expected))
	}
	oldMeta := object.Meta(old)
	if err := checkMetaKeys(p, object.Meta(in), oldMeta); err != nil {
		return resource.ApplyResult{}, err
	}
	if oldMeta.GetDeleteTime() != nil {
		return resource.ApplyResult{}, failedPrecondition("%s %s is being deleted", b.r.Kind, key.Name)
	}
	if changed := object.ImmutableChanges(old, in); len(changed) > 0 {
		return resource.ApplyResult{}, invalid("immutable fields changed: %s", strings.Join(changed, ", "))
	}
	if err := b.s.admit(ctx, b, p, old, in); err != nil {
		return resource.ApplyResult{}, err
	}

	merged := proto.Clone(in)
	meta := object.Meta(merged)
	meta.Uid = oldMeta.GetUid()
	meta.CreateTime = oldMeta.GetCreateTime()
	meta.Generation = oldMeta.GetGeneration()
	meta.DeleteTime = nil
	meta.ResourceVersion = cur.Revision
	if p.Type != auth.System {
		meta.Finalizers = oldMeta.GetFinalizers()
		meta.OwnerReferences = oldMeta.GetOwnerReferences()
		meta.Labels = keepReserved(meta.GetLabels(), oldMeta.GetLabels())
		meta.Annotations = keepReserved(meta.GetAnnotations(), oldMeta.GetAnnotations())
	}
	object.CopyStatus(merged, old)
	if b.hooks.seal != nil {
		if err := b.hooks.seal(b.s, old, merged); err != nil {
			return resource.ApplyResult{}, err
		}
	}
	if !object.SpecEqual(old, merged) {
		meta.Generation++
	}
	if proto.Equal(old, merged) {
		return resource.ApplyResult{Object: old}, nil
	}
	data, err := encode(merged)
	if err != nil {
		return resource.ApplyResult{}, err
	}
	rev, err := b.s.store.Update(ctx, b.storeKey(key), data, cur.Revision)
	if err != nil {
		return resource.ApplyResult{}, err
	}
	meta.ResourceVersion = rev
	return resource.ApplyResult{Object: merged}, nil
}

// keepReserved returns the user's map plus the system-reserved entries of
// the stored one, which a user's apply must not drop.
func keepReserved(user, stored map[string]string) map[string]string {
	out := maps.Clone(user)
	for k, v := range stored {
		if reservedKey(k) {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

func (b *backend) Delete(ctx context.Context, key resource.Key, expected *int64) (proto.Message, error) {
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	for range maxRetries {
		cur, old, err := b.read(ctx, p, authz.Delete, key)
		if err != nil {
			return nil, err
		}
		if expected != nil && *expected != cur.Revision {
			return nil, connect.NewError(connect.CodeAborted,
				fmt.Errorf("%s %s is at resource version %d, not %d", b.r.Kind, key.Name, cur.Revision, *expected))
		}
		meta := object.Meta(old)
		if meta.GetDeleteTime() == nil && b.hooks.beforeDelete != nil {
			if err := b.hooks.beforeDelete(ctx, b.s, p, old); err != nil {
				return nil, apiError(err)
			}
		}
		if len(meta.GetFinalizers()) > 0 {
			if meta.GetDeleteTime() != nil {
				if err := b.afterDelete(ctx, key); err != nil {
					return nil, err
				}
				b.answer(old)
				return old, nil
			}
			meta.DeleteTime = timestamppb.New(b.s.clock.Now())
			data, err := encode(old)
			if err != nil {
				return nil, apiError(err)
			}
			rev, err := b.s.store.Update(ctx, b.storeKey(key), data, cur.Revision)
			if errors.Is(err, store.ErrConflict) && expected == nil {
				continue
			}
			if err != nil {
				return nil, apiError(err)
			}
			meta.ResourceVersion = rev
			if err := b.afterDelete(ctx, key); err != nil {
				return nil, err
			}
			b.answer(old)
			return old, nil
		}
		_, _, err = b.s.store.Delete(ctx, b.storeKey(key), cur.Revision)
		if errors.Is(err, store.ErrConflict) && expected == nil {
			continue
		}
		if err != nil {
			return nil, apiError(err)
		}
		if err := b.afterDelete(ctx, key); err != nil {
			return nil, err
		}
		b.answer(old)
		return old, nil
	}
	return nil, connect.NewError(connect.CodeAborted, errors.New("too many concurrent writers; retry"))
}

// afterDelete runs the kind's afterDelete hook, if it has one, once a delete
// is stored. Its failure fails the call, which the caller retries: the
// object is still there, being deleted, and the hook runs again.
func (b *backend) afterDelete(ctx context.Context, key resource.Key) error {
	if b.hooks.afterDelete == nil {
		return nil
	}
	if err := b.hooks.afterDelete(context.WithoutCancel(ctx), b.s, key); err != nil {
		return apiError(err)
	}
	return nil
}

func (b *backend) UpdateStatus(ctx context.Context, key resource.Key, status proto.Message, expected *int64) (proto.Message, error) {
	p, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := b.checkKey(key); err != nil {
		return nil, err
	}
	if status == nil || !status.ProtoReflect().IsValid() {
		return nil, invalid("status is required")
	}
	for range maxRetries {
		cur, obj, err := b.read(ctx, p, authz.UpdateStatus, key)
		if err != nil {
			return nil, err
		}
		if expected != nil && *expected != cur.Revision {
			return nil, connect.NewError(connect.CodeAborted,
				fmt.Errorf("%s %s is at resource version %d, not %d", b.r.Kind, key.Name, cur.Revision, *expected))
		}
		object.SetStatus(obj, status)
		data, err := encode(obj)
		if err != nil {
			return nil, apiError(err)
		}
		rev, err := b.s.store.Update(ctx, b.storeKey(key), data, cur.Revision)
		if errors.Is(err, store.ErrConflict) && expected == nil {
			continue
		}
		if err != nil {
			return nil, apiError(err)
		}
		object.Meta(obj).ResourceVersion = rev
		b.answer(obj)
		return obj, nil
	}
	return nil, connect.NewError(connect.CodeAborted, errors.New("too many concurrent writers; retry"))
}
