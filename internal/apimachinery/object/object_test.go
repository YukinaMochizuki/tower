package object_test

import (
	"slices"
	"testing"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
)

func TestImmutableChanges(t *testing.T) {
	project := func(tenant, display string) *tenancyv1alpha1.Project {
		return &tenancyv1alpha1.Project{Spec: &tenancyv1alpha1.ProjectSpec{Tenant: tenant, DisplayName: display}}
	}
	if got := object.ImmutableChanges(project("acme", "a"), project("acme", "b")); len(got) != 0 {
		t.Errorf("a mutable change reported %v", got)
	}
	if got := object.ImmutableChanges(project("acme", "a"), project("other", "a")); !slices.Equal(got, []string{"spec.tenant"}) {
		t.Errorf("changing the tenant reported %v", got)
	}
	if got := object.ImmutableChanges(project("acme", "a"), &tenancyv1alpha1.Project{}); !slices.Equal(got, []string{"spec.tenant"}) {
		t.Errorf("clearing the whole spec reported %v", got)
	}

	scoped := func(tenant string) *iamv1alpha1.RoleBinding {
		rb := &iamv1alpha1.RoleBinding{Spec: &iamv1alpha1.RoleBindingSpec{Role: "tenant.owner"}}
		if tenant != "" {
			rb.Spec.Scope = &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Tenant{Tenant: tenant}}
		}
		return rb
	}
	if got := object.ImmutableChanges(scoped("acme"), scoped("other")); !slices.Equal(got, []string{"spec.scope"}) {
		t.Errorf("moving a binding reported %v", got)
	}
	if got := object.ImmutableChanges(scoped("acme"), scoped("")); !slices.Equal(got, []string{"spec.scope"}) {
		t.Errorf("clearing a binding's scope reported %v", got)
	}
	if got := object.ImmutableChanges(scoped("acme"), scoped("acme")); len(got) != 0 {
		t.Errorf("an unchanged binding reported %v", got)
	}
}
