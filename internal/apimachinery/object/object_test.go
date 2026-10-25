package object_test

import (
	"slices"
	"testing"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/object"
)

func TestImmutableChanges(t *testing.T) {
	hostd := iamv1alpha1.Role_ROLE_HOSTD
	token := func(target, description string) *iamv1alpha1.ServiceToken {
		return &iamv1alpha1.ServiceToken{Spec: &iamv1alpha1.ServiceTokenSpec{Role: hostd, Target: target, Description: description}}
	}
	if got := object.ImmutableChanges(token("t1", "a"), token("t1", "b")); len(got) != 0 {
		t.Errorf("a mutable change reported %v", got)
	}
	if got := object.ImmutableChanges(token("t1", "a"), token("t2", "a")); !slices.Equal(got, []string{"spec.target"}) {
		t.Errorf("changing the target reported %v", got)
	}
	if got := object.ImmutableChanges(token("t1", "a"), &iamv1alpha1.ServiceToken{}); !slices.Equal(got, []string{"spec.role", "spec.target"}) {
		t.Errorf("clearing the whole spec reported %v", got)
	}

	typed := func(t secretsv1alpha1.SecretType) *secretsv1alpha1.Secret {
		s := &secretsv1alpha1.Secret{Spec: &secretsv1alpha1.SecretSpec{Data: map[string][]byte{"A": []byte("x")}}}
		s.Spec.Type = t
		return s
	}
	opaque, regAuth := secretsv1alpha1.SecretType_SECRET_TYPE_OPAQUE, secretsv1alpha1.SecretType_SECRET_TYPE_REGISTRY_AUTH
	if got := object.ImmutableChanges(typed(opaque), typed(regAuth)); !slices.Equal(got, []string{"spec.type"}) {
		t.Errorf("changing a secret's type reported %v", got)
	}
	if got := object.ImmutableChanges(typed(opaque), typed(secretsv1alpha1.SecretType_SECRET_TYPE_UNSPECIFIED)); !slices.Equal(got, []string{"spec.type"}) {
		t.Errorf("clearing a secret's type reported %v", got)
	}
	if got := object.ImmutableChanges(typed(opaque), typed(opaque)); len(got) != 0 {
		t.Errorf("an unchanged secret reported %v", got)
	}
}
