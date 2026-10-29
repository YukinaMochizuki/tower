// Package authz decides whether a caller may perform an action on a kind:
// a small table from a service token's role to the verbs it may use on each
// kind. It holds no view of the store; the apiserver reads the role from the
// caller's ServiceToken at every call, so a delete or a rotate counts at
// once. See docs/decisions/authentication.md.
package authz

import (
	"slices"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/auth"
)

// Verb is an action on a resource kind.
type Verb string

const (
	// Get reads one object.
	Get Verb = "get"
	// List reads objects in bulk.
	List Verb = "list"
	// Watch follows changes to objects.
	Watch Verb = "watch"
	// Apply creates or replaces an object's spec and metadata.
	Apply Verb = "apply"
	// Delete deletes an object.
	Delete Verb = "delete"
	// UpdateStatus writes an object's status, which only the apiserver
	// itself does; see hangar: docs/decisions/apply-semantics.md.
	UpdateStatus Verb = "updateStatus"
)

// Verbs lists every Verb, for tests that must see each one.
var Verbs = []Verb{Get, List, Watch, Apply, Delete, UpdateStatus}

// userVerbs are what a role may grant: every verb but UpdateStatus.
var userVerbs = []Verb{Get, List, Watch, Apply, Delete}

// readVerbs read a kind and change nothing.
var readVerbs = []Verb{Get, List, Watch}

// systemKinds are made only by tower's own controllers: a role reads them
// at most.
var systemKinds = []string{registry.Golden.Kind, registry.Instance.Kind, registry.Boost.Kind}

// grants is each role's verbs by kind. A kind or role it does not list is
// refused everything. ROLE_HOSTD's verbs hold only on its own target's
// objects (OwnTargetOnly), which the apiserver decides object by object.
var grants = map[iamv1alpha1.Role]map[string][]Verb{
	iamv1alpha1.Role_ROLE_ADMIN: adminGrants(),
	iamv1alpha1.Role_ROLE_HOSTD: {
		registry.Instance.Kind: {Get, List, Watch, UpdateStatus},
		registry.Golden.Kind:   readVerbs,
		registry.Target.Kind:   {Get, UpdateStatus},
	},
	iamv1alpha1.Role_ROLE_WORKSTREAM: {
		registry.Project.Kind:    userVerbs,
		registry.Secret.Kind:     userVerbs,
		registry.Repository.Kind: userVerbs,
		registry.Box.Kind:        userVerbs,
		registry.Machine.Kind:    userVerbs,
		// workstream shows where a project runs and what it is made of,
		// and a person there picks a project's target: it applies the
		// Placement, but never deletes one, which would leave the
		// project's environments UNPLACED (docs/decisions/placement.md).
		registry.Target.Kind:    readVerbs,
		registry.Placement.Kind: {Get, List, Watch, Apply},
		registry.Golden.Kind:    readVerbs,
		registry.Instance.Kind:  readVerbs,
		// A box's boost, which tower makes and deletes.
		registry.Boost.Kind: readVerbs,
	},
}

// adminGrants is every user verb on every kind, but only reads of the
// kinds tower's controllers alone make.
func adminGrants() map[string][]Verb {
	m := map[string][]Verb{}
	for _, r := range registry.All {
		m[r.Kind] = userVerbs
		if slices.Contains(systemKinds, r.Kind) {
			m[r.Kind] = readVerbs
		}
	}
	return m
}

// Caller is who calls, as authorization sees it: the principal, and for a
// service token the role its ServiceToken gives it, read for this call.
type Caller struct {
	auth.Principal
	// Role is a service token's; unset for any other principal, and for a
	// token whose ServiceToken is gone.
	Role iamv1alpha1.Role
	// Target is a ROLE_HOSTD token's: the one target whose objects it may
	// touch.
	Target string
}

// OwnTargetOnly reports whether what c may do on a kind holds only on the
// objects of its own target, which the caller of Allowed then checks on
// each object.
func OwnTargetOnly(c Caller) bool {
	return c.Type == auth.ServiceToken && c.Role == iamv1alpha1.Role_ROLE_HOSTD
}

// Allowed reports whether c may do verb on objects of kind r. The system
// may do everything; root on the host, what an admin token may.
func Allowed(c Caller, r registry.Resource, verb Verb) bool {
	switch c.Type {
	case auth.System:
		return true
	case auth.Local:
		return slices.Contains(grants[iamv1alpha1.Role_ROLE_ADMIN][r.Kind], verb)
	case auth.ServiceToken:
		return slices.Contains(grants[c.Role][r.Kind], verb)
	}
	return false
}

// MayUseEnvironments reports whether c may call EnvironmentService, and so
// run commands, read and write files, and push from environments:
// ROLE_WORKSTREAM, and the system. Not an admin, nor root on the host: what
// runs in an environment is judged in workstream (docs/decisions/secrets.md).
func MayUseEnvironments(c Caller) bool {
	switch c.Type {
	case auth.System:
		return true
	case auth.ServiceToken:
		return c.Role == iamv1alpha1.Role_ROLE_WORKSTREAM
	}
	return false
}

// MayRetry reports whether c may call RetryService, and so run a failed
// golden's, box's or machine's work before its backoff runs out: an admin
// token, root on the host, ROLE_WORKSTREAM (for the person it acts for),
// and the system. Not hostd, which has no say in tower's policy
// (docs/decisions/layers.md).
func MayRetry(c Caller) bool {
	switch c.Type {
	case auth.System, auth.Local:
		return true
	case auth.ServiceToken:
		return c.Role == iamv1alpha1.Role_ROLE_ADMIN || c.Role == iamv1alpha1.Role_ROLE_WORKSTREAM
	}
	return false
}

// MayOpenTunnel reports whether c may open its target's tunnel: a
// ROLE_HOSTD token, for its own target alone.
func MayOpenTunnel(c Caller) bool {
	return c.Type == auth.ServiceToken && c.Role == iamv1alpha1.Role_ROLE_HOSTD && c.Target != ""
}

// MayAbandonTargets reports whether c may let a Target being deleted go
// without its teardown done (TargetActionService.AbandonTarget): an
// admin token, root on the host, and the system.
func MayAbandonTargets(c Caller) bool {
	switch c.Type {
	case auth.System, auth.Local:
		return true
	case auth.ServiceToken:
		return c.Role == iamv1alpha1.Role_ROLE_ADMIN
	}
	return false
}

// MayIssueTokens reports whether c may call ServiceTokenIssuerService: an
// admin token, root on the host, and the system.
func MayIssueTokens(c Caller) bool {
	switch c.Type {
	case auth.System, auth.Local:
		return true
	case auth.ServiceToken:
		return c.Role == iamv1alpha1.Role_ROLE_ADMIN
	}
	return false
}
