package apiserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/object"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/authz"
	"github.com/YukinaMochizuki/tower/internal/store"
)

var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidName checks that a name is a DNS-1123 label, the rule for every object
// name and project.
func ValidName(name string) error {
	if len(name) == 0 || len(name) > 63 || !dns1123Label.MatchString(name) {
		return fmt.Errorf("%q must be 1-63 lowercase letters, digits or '-', starting and ending with a letter or digit", name)
	}
	return nil
}

// validName reports whether a name is one ValidName takes.
func validName(name string) bool { return ValidName(name) == nil }

// reservedKey reports whether a label or annotation key is reserved for the
// system: its prefix is the API domain or a subdomain of it.
func reservedKey(key string) bool {
	prefix, _, ok := strings.Cut(key, "/")
	return ok && (prefix == registry.Domain || strings.HasSuffix(prefix, "."+registry.Domain))
}

// checkMetaKeys validates labels and annotations. A user may not set a
// reserved key, but may send back one the stored object already has with the
// same value, so reading an object and applying it again works.
func checkMetaKeys(p authz.Caller, meta, stored *metav1.ObjectMeta) error {
	for _, kind := range []struct {
		what         string
		user, stored map[string]string
	}{
		{"label", meta.GetLabels(), stored.GetLabels()},
		{"annotation", meta.GetAnnotations(), stored.GetAnnotations()},
	} {
		for k, v := range kind.user {
			if k == "" || len(k) > 317 {
				return invalid("%s key %q must be 1-317 characters", kind.what, k)
			}
			if kind.what == "label" && len(v) > 63 {
				return invalid("label %q: value longer than 63 characters", k)
			}
			if reservedKey(k) && p.Type != auth.System {
				if sv, ok := kind.stored[k]; !ok || sv != v {
					return invalid("%s %q uses a prefix reserved for the system", kind.what, k)
				}
			}
		}
	}
	return nil
}

// dependentsFinalizer keeps an object that cascades in place, marked as
// being deleted, until the garbage collector has deleted everything it
// owns.
const dependentsFinalizer = registry.Domain + "/dependents"

// kindHooks is what differs between kinds in the generic machinery.
type kindHooks struct {
	// cascades makes deleting the object delete everything it owns first.
	cascades bool
	// owner returns the object a new one belongs to, if any: deleting the
	// owner deletes it.
	owner func(ctx context.Context, s *Server, obj proto.Message) (registry.Resource, resource.Key, bool)
	// validate checks rules specific to the kind; old is nil on create.
	validate func(ctx context.Context, s *Server, p authz.Caller, old, obj proto.Message) error
	// afterDelete runs once a delete is stored, whether it marked the
	// object or removed it, and again on every later delete of an object
	// being deleted, so it must be idempotent: it ends what the object
	// stands for outside the store, as a ServiceToken's secret.
	afterDelete func(ctx context.Context, s *Server, key resource.Key) error
	// beforeGone runs when a cascading object has nothing left and is about
	// to go; its failure keeps the object, being deleted, and the garbage
	// collector tries again. It ends for good what afterDelete may have
	// failed to.
	beforeGone func(ctx context.Context, s *Server, key resource.Key) error
	// seal, if set, turns what a caller wrote into what is stored, once
	// admission has passed it and the object has its uid; old is nil on
	// create. A stored object equal to old is not written.
	seal func(s *Server, old, obj proto.Message) error
	// redact, if set, clears in place what no answer may hold, whoever
	// asks: a Secret's values. It is applied to every object answered, by
	// every call and event.
	redact func(obj proto.Message)
	// beforeDelete may refuse a delete; it runs when the delete is asked
	// for, not again while finalizers hold the object.
	beforeDelete func(ctx context.Context, s *Server, p authz.Caller, obj proto.Message) error
	// targetOf names the Target obj belongs to: a caller held to its own
	// target (authz.OwnTargetOnly) touches only the objects of it. A kind
	// without it holds none of theirs.
	targetOf func(obj proto.Message) string
}

// ownedByProject makes every project-scoped object belong to its project.
func ownedByProject(_ context.Context, _ *Server, obj proto.Message) (registry.Resource, resource.Key, bool) {
	return registry.Project, resource.Key{Name: object.Meta(obj).GetProject()}, true
}

func hooksFor(r registry.Resource) kindHooks {
	switch r.Kind {
	case registry.Project.Kind:
		// Everything in a project is owned by it, so deleting it deletes
		// its contents first.
		return kindHooks{cascades: true}
	case registry.ServiceToken.Kind:
		// Deleting a token revokes its secret at once, and once more before
		// it goes, so no secret outlives its token. See servicetokens.go.
		return kindHooks{
			validate:    validateServiceToken,
			cascades:    true,
			afterDelete: revokeServiceToken,
			beforeGone:  revokeServiceToken,
		}
	case registry.Secret.Kind:
		// Values are write-only and stored sealed. See
		// docs/decisions/secrets.md.
		return kindHooks{
			owner:    ownedByProject,
			validate: validateSecret,
			seal:     sealSecret,
			redact:   redactSecret,
		}
	}
	if h, ok := environmentHooks(r); ok {
		return h
	}
	if r.ProjectScoped() {
		return kindHooks{owner: ownedByProject}
	}
	return kindHooks{}
}

// admit runs the checks every write of an object goes through; old is nil on
// create.
func (s *Server) admit(ctx context.Context, b *backend, p authz.Caller, old, obj proto.Message) error {
	if b.r.ProjectScoped() {
		if err := s.requireLive(ctx, registry.Project, "", object.Meta(obj).GetProject()); err != nil {
			return err
		}
	}
	if b.hooks.validate != nil {
		if err := b.hooks.validate(ctx, s, p, old, obj); err != nil {
			return err
		}
	}
	return nil
}

// ownerRef returns a reference to the object a new one belongs to, which
// must exist and not be on its way out.
func (s *Server) ownerRef(ctx context.Context, b *backend, obj proto.Message) (*metav1.OwnerReference, error) {
	if b.hooks.owner == nil {
		return nil, nil
	}
	r, key, ok := b.hooks.owner(ctx, s, obj)
	if !ok {
		return nil, nil
	}
	o, err := s.store.Get(ctx, s.backend(r).storeKey(key))
	if errors.Is(err, store.ErrNotFound) {
		return nil, failedPrecondition("%s %q does not exist", strings.ToLower(r.Kind), key.Name)
	}
	if err != nil {
		return nil, err
	}
	owner, err := s.backend(r).decode(o)
	if err != nil {
		return nil, err
	}
	meta := object.Meta(owner)
	if meta.GetDeleteTime() != nil {
		return nil, failedPrecondition("%s %q is being deleted", strings.ToLower(r.Kind), key.Name)
	}
	return &metav1.OwnerReference{
		ApiVersion: r.APIVersion(), Kind: r.Kind, Name: key.Name, Uid: meta.GetUid(), Controller: true,
	}, nil
}

// requireLive checks that an object an incoming one depends on exists and is
// not being deleted.
func (s *Server) requireLive(ctx context.Context, r registry.Resource, project, name string) error {
	o, err := s.store.Get(ctx, store.Key{Kind: r.Kind, Project: project, Name: name})
	if errors.Is(err, store.ErrNotFound) {
		return failedPrecondition("%s %q does not exist", strings.ToLower(r.Kind), name)
	}
	if err != nil {
		return err
	}
	obj, err := s.backend(r).decode(o)
	if err != nil {
		return err
	}
	if object.Meta(obj).GetDeleteTime() != nil {
		return failedPrecondition("%s %q is being deleted", strings.ToLower(r.Kind), name)
	}
	return nil
}
