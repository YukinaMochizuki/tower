package apiserver

import (
	"testing"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
)

// TestATargetsKeyIsOnlyItsOwn: the ssh key tower made for a Target is
// deleted by that Target's uid alone: an earlier Target of the name, let
// go late, leaves a later one's key, as does the sweep of keys of Targets
// gone; and a later Target takes no earlier one's key.
func TestATargetsKeyIsOnlyItsOwn(t *testing.T) {
	t.Parallel()
	s, system, admin := runningWith(t, func(c *Config) { c.DisableControllers = true })
	if _, err := s.backend(registry.Project).Apply(admin, &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: systemProject}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Target).Apply(admin, &targetsv1alpha1.Target{
		Metadata: &metav1.ObjectMeta{Name: "h1"},
		Spec: &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hangar{Hangar: &targetsv1alpha1.HangarTarget{
			Endpoint: "https://hangar.example", Project: "p", CredentialSecret: "tok",
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	tg, _, err := getTyped[*targetsv1alpha1.Target](system, s, registry.Target, resource.Key{Name: "h1"})
	if err != nil {
		t.Fatal(err)
	}
	uid := tg.GetMetadata().GetUid()
	keyOf := func() (*secretsv1alpha1.Secret, bool) {
		sec, ok, err := getTyped[*secretsv1alpha1.Secret](system, s, registry.Secret, resource.Key{Project: systemProject, Name: hangarSSHKeySecret("h1")})
		if err != nil {
			t.Fatal(err)
		}
		return sec, ok
	}

	// A key left by an earlier Target of the name is not this one's.
	if _, err := s.backend(registry.Secret).Create(system, &secretsv1alpha1.Secret{
		Metadata: &metav1.ObjectMeta{Project: systemProject, Name: hangarSSHKeySecret("h1"), Annotations: map[string]string{targetUIDAnnotation: "earlier"}},
		Spec:     &secretsv1alpha1.SecretSpec{Type: secretsv1alpha1.SecretType_SECRET_TYPE_OPAQUE, Data: map[string][]byte{hangarSSHKeyData: []byte("earlier key")}},
	}); err != nil {
		t.Fatal(err)
	}
	key, err := s.hangarSSHKey(system, "h1", uid, true)
	if err != nil {
		t.Fatal(err)
	}
	if string(key) == "earlier key" {
		t.Fatal("a Target took an earlier Target's key")
	}
	if sec, ok := keyOf(); !ok || sec.GetMetadata().GetAnnotations()[targetUIDAnnotation] != uid {
		t.Fatalf("the key made for the Target: %v", sec.GetMetadata())
	}

	// The earlier Target let go late, and the sweep of gone Targets' keys,
	// leave it.
	for _, by := range []string{"earlier", ""} {
		if err := s.deleteHangarSSHKey(system, "h1", by); err != nil {
			t.Fatal(err)
		}
		if _, ok := keyOf(); !ok {
			t.Fatalf("deleting by uid %q took the Target's key", by)
		}
	}
	if err := s.deleteHangarSSHKey(system, "h1", uid); err != nil {
		t.Fatal(err)
	}
	if _, ok := keyOf(); ok {
		t.Error("the Target's key stayed as it was deleted by its uid")
	}
}

// TestANonHangarTargetKeepsNoKey: a key made for a Target of the name that
// is no hangar target, which has no use for it, goes as the Target is
// reconciled; so does one an earlier Target of the name left.
func TestANonHangarTargetKeepsNoKey(t *testing.T) {
	t.Parallel()
	s, system, admin := runningWith(t, func(c *Config) { c.DisableControllers = true })
	if _, err := s.backend(registry.Project).Apply(admin, &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: systemProject}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Target).Apply(admin, &targetsv1alpha1.Target{
		Metadata: &metav1.ObjectMeta{Name: "t1"},
		Spec:     &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hostd{Hostd: &targetsv1alpha1.HostdTarget{}}},
	}); err != nil {
		t.Fatal(err)
	}
	tg, _, err := getTyped[*targetsv1alpha1.Target](system, s, registry.Target, resource.Key{Name: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, made := range []string{tg.GetMetadata().GetUid(), "earlier"} {
		if _, err := s.backend(registry.Secret).Create(system, &secretsv1alpha1.Secret{
			Metadata: &metav1.ObjectMeta{Project: systemProject, Name: hangarSSHKeySecret("t1"), Annotations: map[string]string{targetUIDAnnotation: made}},
			Spec:     &secretsv1alpha1.SecretSpec{Type: secretsv1alpha1.SecretType_SECRET_TYPE_OPAQUE, Data: map[string][]byte{hangarSSHKeyData: []byte("key")}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.reconcileHangarTarget(system, controller.Key{Name: "t1"}); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := getTyped[*secretsv1alpha1.Secret](system, s, registry.Secret, resource.Key{Project: systemProject, Name: hangarSSHKeySecret("t1")}); err != nil || ok {
			t.Errorf("a key made for target uid %q stayed on a hostd target of the name: %v", made, err)
		}
	}
}
