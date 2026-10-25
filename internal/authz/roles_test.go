package authz_test

import (
	"slices"
	"testing"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/authz"
)

func token(role iamv1alpha1.Role) authz.Caller {
	return authz.Caller{Principal: auth.Principal{Type: auth.ServiceToken, ID: "t"}, Role: role}
}

// writes is every verb a role may hold: all but UpdateStatus.
var writes = []authz.Verb{authz.Get, authz.List, authz.Watch, authz.Apply, authz.Delete}

// reads change nothing.
var reads = []authz.Verb{authz.Get, authz.List, authz.Watch}

// admin is what an admin token and root on the host hold: every kind, and
// only reads of what tower's controllers alone make.
var admin = map[string][]authz.Verb{
	registry.Project.Kind: writes, registry.Secret.Kind: writes, registry.ServiceToken.Kind: writes,
	registry.Target.Kind: writes, registry.Placement.Kind: writes,
	registry.Repository.Kind: writes, registry.Box.Kind: writes, registry.Machine.Kind: writes,
	registry.Golden.Kind: reads, registry.Instance.Kind: reads,
}

// TestTheRoleTable holds every role's verbs on every kind exactly: what it
// lists is allowed, and every other verb refused.
func TestTheRoleTable(t *testing.T) {
	local := authz.Caller{Principal: auth.Principal{Type: auth.Local, ID: "root@n1"}}
	for _, tc := range []struct {
		name   string
		caller authz.Caller
		want   map[string][]authz.Verb
	}{
		{"admin", token(iamv1alpha1.Role_ROLE_ADMIN), admin},
		{"root on the host", local, admin},
		{"workstream", token(iamv1alpha1.Role_ROLE_WORKSTREAM), map[string][]authz.Verb{
			registry.Project.Kind: writes, registry.Secret.Kind: writes,
			registry.Repository.Kind: writes, registry.Box.Kind: writes, registry.Machine.Kind: writes,
			registry.Target.Kind: reads, registry.Placement.Kind: reads,
			registry.Golden.Kind: reads, registry.Instance.Kind: reads,
		}},
		// On its own target's objects alone, which the apiserver checks.
		{"hostd", token(iamv1alpha1.Role_ROLE_HOSTD), map[string][]authz.Verb{
			registry.Instance.Kind: {authz.Get, authz.List, authz.Watch, authz.UpdateStatus},
			registry.Golden.Kind:   reads,
			registry.Target.Kind:   {authz.Get, authz.UpdateStatus},
		}},
		{"a token without a role", token(iamv1alpha1.Role_ROLE_UNSPECIFIED), nil},
		{"no principal", authz.Caller{}, nil},
	} {
		for _, r := range registry.All {
			for _, v := range authz.Verbs {
				want := slices.Contains(tc.want[r.Kind], v)
				if got := authz.Allowed(tc.caller, r, v); got != want {
					t.Errorf("%s may %s %s: %t, want %t", tc.name, v, r.Plural, got, want)
				}
			}
		}
	}
}

func TestTheSystemMayDoEverything(t *testing.T) {
	sys := authz.Caller{Principal: auth.SystemPrincipal}
	for _, r := range registry.All {
		for _, v := range authz.Verbs {
			if !authz.Allowed(sys, r, v) {
				t.Errorf("the system may not %s %s", v, r.Plural)
			}
		}
	}
}

func TestWhoMayIssueTokens(t *testing.T) {
	for c, want := range map[authz.Caller]bool{
		token(iamv1alpha1.Role_ROLE_ADMIN):                               true,
		token(iamv1alpha1.Role_ROLE_WORKSTREAM):                          false,
		token(iamv1alpha1.Role_ROLE_HOSTD):                               false,
		token(iamv1alpha1.Role_ROLE_UNSPECIFIED):                         false,
		{Principal: auth.Principal{Type: auth.Local, ID: "root@n1"}}:     true,
		{Principal: auth.SystemPrincipal}:                                true,
		{Principal: auth.Principal{}, Role: iamv1alpha1.Role_ROLE_ADMIN}: false,
	} {
		if got := authz.MayIssueTokens(c); got != want {
			t.Errorf("%v (role %v) may issue tokens: %t, want %t", c.Principal, c.Role, got, want)
		}
	}
}

func TestWhoMayUseEnvironments(t *testing.T) {
	for c, want := range map[authz.Caller]bool{
		token(iamv1alpha1.Role_ROLE_WORKSTREAM):                               true,
		token(iamv1alpha1.Role_ROLE_ADMIN):                                    false,
		token(iamv1alpha1.Role_ROLE_HOSTD):                                    false,
		token(iamv1alpha1.Role_ROLE_UNSPECIFIED):                              false,
		{Principal: auth.Principal{Type: auth.Local, ID: "root@n1"}}:          false,
		{Principal: auth.SystemPrincipal}:                                     true,
		{Principal: auth.Principal{}, Role: iamv1alpha1.Role_ROLE_WORKSTREAM}: false,
	} {
		if got := authz.MayUseEnvironments(c); got != want {
			t.Errorf("%v (role %v) may use environments: %t, want %t", c.Principal, c.Role, got, want)
		}
	}
}

func TestWhoMayOpenATunnel(t *testing.T) {
	hostd := token(iamv1alpha1.Role_ROLE_HOSTD)
	hostd.Target = "t1"
	for c, want := range map[authz.Caller]bool{
		hostd:                                   true,
		token(iamv1alpha1.Role_ROLE_HOSTD):      false,
		token(iamv1alpha1.Role_ROLE_WORKSTREAM): false,
		token(iamv1alpha1.Role_ROLE_ADMIN):      false,
		{Principal: auth.Principal{Type: auth.Local, ID: "root@n1"}}: false,
		{Principal: auth.SystemPrincipal}:                            false,
	} {
		if got := authz.MayOpenTunnel(c); got != want {
			t.Errorf("%v (role %v, target %q) may open a tunnel: %t, want %t", c.Principal, c.Role, c.Target, got, want)
		}
	}
}
