package apiserver

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/secrets"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/store/sqlite"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// running starts a server and returns it with contexts acting as the
// system and as an admin token, which has made project "web".
func running(t *testing.T) (s *Server, system, admin context.Context) {
	t.Helper()
	return runningWith(t, func(*Config) {})
}

// runningWith is running with a config adjusted by configure.
func runningWith(t *testing.T, configure func(*Config)) (s *Server, system, admin context.Context) {
	t.Helper()
	return runningAt(t, filepath.Join(t.TempDir(), "tower.db"), configure)
}

// runningAt is runningWith on the database file db, which a test can read.
// The server has a KEK and keeps credentials in the store, unless configure
// says otherwise.
func runningAt(t *testing.T, db string, configure func(*Config)) (s *Server, system, admin context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := sqlite.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := secrets.NewKeyring(secrets.NewKEK("test"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Store: st, Clock: clockwork.NewFakeClock(), Secrets: keys, Credentials: st}
	configure(&cfg)
	s = New(cfg)
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = st.Close()
	})
	system = auth.WithPrincipal(ctx, auth.SystemPrincipal)
	admin = serviceToken(t, s, system, "admin", iamv1alpha1.Role_ROLE_ADMIN, "")
	if _, err := s.backend(registry.Project).Apply(admin, &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "web"}}); err != nil {
		t.Fatal(err)
	}
	return s, system, admin
}

// serviceToken makes a ServiceToken of the role, as the system, and
// returns a context acting as it.
func serviceToken(t *testing.T, s *Server, system context.Context, name string, role iamv1alpha1.Role, target string) context.Context {
	t.Helper()
	if _, err := s.backend(registry.ServiceToken).Apply(system, &iamv1alpha1.ServiceToken{
		Metadata: &metav1.ObjectMeta{Name: name},
		Spec:     &iamv1alpha1.ServiceTokenSpec{Role: role, Target: target},
	}); err != nil {
		t.Fatal(err)
	}
	return auth.WithPrincipal(context.Background(), tokenPrincipal(resource.Key{Name: name}))
}

// local is a context acting as root on the host.
var local = auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.Local, ID: "root@n1"})

// opaque is an opaque secret in project "web" holding data.
func opaque(name string, data map[string]string) *secretsv1alpha1.Secret {
	d := map[string][]byte{}
	for k, v := range data {
		d[k] = []byte(v)
	}
	return &secretsv1alpha1.Secret{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec:     &secretsv1alpha1.SecretSpec{Type: secretsv1alpha1.SecretType_SECRET_TYPE_OPAQUE, Data: d},
	}
}

// sec is an opaque secret in project "web" of one key.
func sec(name string) *secretsv1alpha1.Secret {
	return opaque(name, map[string]string{"TOKEN": "x"})
}

func TestFinalizers(t *testing.T) {
	t.Parallel()
	s, system, admin := running(t)
	b := s.backend(registry.Secret)
	key := resource.Key{Project: "web", Name: "api"}

	c := sec("api")
	c.Metadata.Finalizers = []string{"tower.hangarlab.cloud/test"}
	if _, err := b.Apply(system, c); err != nil {
		t.Fatal(err)
	}

	got, err := b.Delete(admin, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.(*secretsv1alpha1.Secret).GetMetadata().GetDeleteTime() == nil {
		t.Fatal("deleting an object with a finalizer did not mark it")
	}
	if _, err := b.Get(admin, key); err != nil {
		t.Fatalf("an object with a finalizer disappeared on delete: %v", err)
	}
	_, err = b.Apply(admin, sec("api"))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("applying an object being deleted: %v", err)
	}

	// Removing the last finalizer completes the deletion.
	if err := s.RemoveFinalizer(system, registry.Secret, key, "tower.hangarlab.cloud/test"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "api deleted", deleted(b, admin, key))
}

// deleted says whether the object at key is gone from b, as happens in the
// background once its finalizers are done.
func deleted(b *backend, ctx context.Context, key resource.Key) func() (bool, string) {
	return func() (bool, string) {
		_, err := b.Get(ctx, key)
		return connect.CodeOf(err) == connect.CodeNotFound, fmt.Sprint(err)
	}
}

func TestReservedLabelsSurviveUserApply(t *testing.T) {
	t.Parallel()
	s, system, admin := running(t)
	b := s.backend(registry.Secret)

	c := sec("api")
	c.Metadata.Labels = map[string]string{"tower.hangarlab.cloud/env": "e-1", "app": "web"}
	if _, err := b.Apply(system, c); err != nil {
		t.Fatal(err)
	}
	mine := sec("api")
	mine.Metadata.Labels = map[string]string{"app": "api"}
	res, err := b.Apply(admin, mine)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"tower.hangarlab.cloud/env": "e-1", "app": "api"}
	got := res.Object.(*secretsv1alpha1.Secret).GetMetadata().GetLabels()
	if len(got) != len(want) || got["tower.hangarlab.cloud/env"] != "e-1" || got["app"] != "api" {
		t.Errorf("labels after a user apply: %v, want %v", got, want)
	}
	// Reserved under the domain and its subdomains, not under another.
	for key, reserved := range map[string]bool{
		"tower.hangarlab.cloud/x": true, "a.tower.hangarlab.cloud/x": true,
		"hangarlab.cloud/x": false, "xtower.hangarlab.cloud/x": false, "tower/x": false,
	} {
		l := sec("labelled")
		l.Metadata.Labels = map[string]string{key: "v"}
		_, err := b.Apply(admin, l)
		if got := connect.CodeOf(err) == connect.CodeInvalidArgument; got != reserved {
			t.Errorf("a user setting %s: %v; reserved %t", key, err, reserved)
		}
	}
}

func TestReservedAnnotationsSurviveAndEchoBack(t *testing.T) {
	t.Parallel()
	s, system, admin := running(t)
	b := s.backend(registry.Project)
	p := &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "api", Annotations: map[string]string{"tower.hangarlab.cloud/env": "e-1"}}}
	if _, err := b.Apply(system, p); err != nil {
		t.Fatal(err)
	}
	// Applying without the annotation keeps it.
	res, err := b.Apply(admin, &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "api"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Object.(*tenancyv1alpha1.Project).GetMetadata().GetAnnotations()["tower.hangarlab.cloud/env"]; got != "e-1" {
		t.Errorf("a user apply dropped a system annotation: %q", got)
	}
	// Sending back what was read is fine; changing it is not.
	read, err := b.Get(admin, resource.Key{Name: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(admin, read); err != nil {
		t.Errorf("applying an object as read: %v", err)
	}
	forged := proto.Clone(read).(*tenancyv1alpha1.Project)
	forged.Metadata.Annotations["tower.hangarlab.cloud/env"] = "e-2"
	if _, err := b.Apply(admin, forged); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("changing a system annotation: %v", err)
	}
}

func TestSweepCollectsOrphans(t *testing.T) {
	t.Parallel()
	s, system, admin := running(t)
	b := s.backend(registry.Secret)

	orphan := sec("orphan")
	orphan.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.Secret.APIVersion(), Kind: "Secret", Name: "gone", Uid: "no-such-uid",
	}}
	if _, err := b.Apply(system, orphan); err != nil {
		t.Fatal(err)
	}
	// Same name as a live owner, but the uid of an earlier object by that
	// name: still an orphan.
	impostor := sec("impostor")
	impostor.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.Secret.APIVersion(), Kind: "Secret", Name: "owner", Uid: "earlier-uid",
	}}
	if _, err := b.Apply(system, impostor); err != nil {
		t.Fatal(err)
	}
	owned := sec("owned")
	if _, err := b.Apply(system, sec("owner")); err != nil {
		t.Fatal(err)
	}
	owner, err := b.Get(admin, resource.Key{Project: "web", Name: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	owned.Metadata.OwnerReferences = []*metav1.OwnerReference{{
		ApiVersion: registry.Secret.APIVersion(), Kind: "Secret", Name: "owner",
		Uid: owner.(*secretsv1alpha1.Secret).GetMetadata().GetUid(),
	}}
	if _, err := b.Apply(system, owned); err != nil {
		t.Fatal(err)
	}

	s.sweep(system)
	testutil.Eventually(t, "orphan collected", deleted(b, admin, resource.Key{Project: "web", Name: "orphan"}))
	testutil.Eventually(t, "impostor collected", deleted(b, admin, resource.Key{Project: "web", Name: "impostor"}))
	if _, err := b.Get(admin, resource.Key{Project: "web", Name: "owned"}); err != nil {
		t.Errorf("the sweep collected an object whose owner exists: %v", err)
	}

	// Deleting the owner collects what it owns.
	if _, err := b.Delete(admin, resource.Key{Project: "web", Name: "owner"}, nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "deleting the owner collects its dependent", deleted(b, admin, resource.Key{Project: "web", Name: "owned"}))
}

func TestStoredBytesCarryNoResourceVersion(t *testing.T) {
	t.Parallel()
	s, system, _ := running(t)
	if _, err := s.backend(registry.Secret).Apply(system, sec("api")); err != nil {
		t.Fatal(err)
	}
	o, err := s.store.Get(system, store.Key{Kind: "Secret", Project: "web", Name: "api"})
	if err != nil {
		t.Fatal(err)
	}
	var c secretsv1alpha1.Secret
	if err := proto.Unmarshal(o.Data, &c); err != nil {
		t.Fatal(err)
	}
	if c.GetMetadata().GetResourceVersion() != 0 {
		t.Errorf("stored bytes carry resource version %d; the store revision is the only source", c.GetMetadata().GetResourceVersion())
	}
}
