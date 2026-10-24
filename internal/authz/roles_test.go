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

// TestTheRoleTable holds every role's verbs on every kind exactly: what it
// lists is allowed, and every other verb refused.
func TestTheRoleTable(t *testing.T) {
	local := authz.Caller{Principal: auth.Principal{Type: auth.Local, ID: "root@n1"}}
	for _, tc := range []struct {
		name   string
		caller authz.Caller
		want   map[string][]authz.Verb
	}{
		{"admin", token(iamv1alpha1.Role_ROLE_ADMIN), map[string][]authz.Verb{
			registry.Project.Kind: writes, registry.Secret.Kind: writes, registry.ServiceToken.Kind: writes,
		}},
		{"root on the host", local, map[string][]authz.Verb{
			registry.Project.Kind: writes, registry.Secret.Kind: writes, registry.ServiceToken.Kind: writes,
		}},
		{"workstream", token(iamv1alpha1.Role_ROLE_WORKSTREAM), map[string][]authz.Verb{
			registry.Project.Kind: writes, registry.Secret.Kind: writes,
		}},
		{"hostd", token(iamv1alpha1.Role_ROLE_HOSTD), nil},
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
