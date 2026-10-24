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

// grants is each role's verbs by kind. A kind or role it does not list is
// refused everything. ROLE_HOSTD holds nothing yet: its reads come with the
// kinds it acts on.
var grants = map[iamv1alpha1.Role]map[string][]Verb{
	iamv1alpha1.Role_ROLE_ADMIN: everyKind(userVerbs),
	iamv1alpha1.Role_ROLE_WORKSTREAM: {
		registry.Project.Kind: userVerbs,
		registry.Secret.Kind:  userVerbs,
	},
}

func everyKind(verbs []Verb) map[string][]Verb {
	m := map[string][]Verb{}
	for _, r := range registry.All {
		m[r.Kind] = verbs
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
