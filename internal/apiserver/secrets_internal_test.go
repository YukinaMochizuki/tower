package apiserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/secrets"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// secretCanary is a password no answer, event, log line or error may hold.
const secretCanary = "canary-7Hq2-pass"

func dockerConfig(host, user, pass string) []byte {
	return []byte(`{"auths":{"` + host + `":{"username":"` + user + `","password":"` + pass + `"}}}`)
}

func registrySecret(name string, cfg []byte) *secretsv1alpha1.Secret {
	return &secretsv1alpha1.Secret{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
		Spec: &secretsv1alpha1.SecretSpec{
			Type: secretsv1alpha1.SecretType_SECRET_TYPE_REGISTRY_AUTH,
			Data: map[string][]byte{secrets.DockerConfigKey: cfg},
		},
	}
}

// lockedBuffer collects the apiserver's log.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func keyring(t *testing.T, current secrets.KEK, previous ...secrets.KEK) *secrets.Keyring {
	t.Helper()
	k, err := secrets.NewKeyring(current, previous...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// withSecrets configures a keyring and logs into log.
func withSecrets(k *secrets.Keyring, log *lockedBuffer) func(*Config) {
	return func(c *Config) {
		c.Secrets = k
		if log != nil {
			c.Logger = slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug}))
		}
	}
}

// noValues fails if an answered secret holds its values, their sealed
// form, or the canary anywhere.
func noValues(t *testing.T, what string, msgs ...proto.Message) {
	t.Helper()
	for _, m := range msgs {
		if sec, ok := m.(*secretsv1alpha1.Secret); ok && (sec.GetSpec().GetData() != nil || sec.GetSpec().GetSealed() != nil) {
			t.Errorf("%s: answered the data or its sealed form: %v", what, sec.GetSpec())
		}
		b, err := protojson.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(secretCanary)) {
			t.Errorf("%s: answered the value: %s", what, b)
		}
	}
}

// TestSecretValuesAreNeverAnswered: no answer, event, log line or error
// holds a secret's value, nor its sealed form; status says what can be
// said without them.
func TestSecretValuesAreNeverAnswered(t *testing.T) {
	log := &lockedBuffer{}
	s, system, user := runningWith(t, withSecrets(keyring(t, secrets.NewKEK("k1")), log))
	b := s.backend(registry.Secret)
	key := resource.Key{Project: "web", Name: "ghcr"}

	since, err := s.store.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []resource.Event
	watchCtx, stopWatch := context.WithCancel(user)
	watched := make(chan error, 1)
	go func() {
		watched <- b.Watch(watchCtx, resource.WatchOptions{Project: "web", SinceRevision: since}, func(ev resource.Event) error {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, ev)
			return nil
		})
	}()

	res, err := b.Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", secretCanary)))
	if err != nil {
		t.Fatal(err)
	}
	noValues(t, "apply", res.Object)
	got, err := b.Get(user, key)
	if err != nil {
		t.Fatal(err)
	}
	noValues(t, "get", got)
	st := got.(*secretsv1alpha1.Secret).GetStatus()
	if !slices.Equal(st.GetKeys(), []string{secrets.DockerConfigKey}) || !slices.Equal(st.GetRegistries(), []string{"ghcr.io"}) ||
		!strings.HasPrefix(st.GetContentHmac(), "hmac-sha256:") {
		t.Errorf("status %v", st)
	}
	for _, project := range []string{"web", ""} {
		list, err := b.List(user, resource.ListOptions{Project: project})
		if err != nil || len(list.Items) != 1 {
			t.Fatalf("list in %q: %v %v", project, list.Items, err)
		}
		noValues(t, "list", list.Items...)
	}
	status, err := b.UpdateStatus(system, key, proto.Clone(st), nil)
	if err != nil {
		t.Fatal(err)
	}
	noValues(t, "status update", status)

	// The same values again are no change; others are.
	rv := secretMeta(status).GetResourceVersion()
	res, err = b.Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", secretCanary)))
	if err != nil {
		t.Fatal(err)
	}
	noValues(t, "apply again", res.Object)
	if m := secretMeta(res.Object); m.GetGeneration() != 1 || m.GetResourceVersion() != rv {
		t.Errorf("the same values again: generation %d, version %d, want 1 and %d", m.GetGeneration(), m.GetResourceVersion(), rv)
	}
	res, err = b.Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", secretCanary+"-2")))
	if err != nil {
		t.Fatal(err)
	}
	noValues(t, "apply new values", res.Object)
	if secretMeta(res.Object).GetGeneration() != 2 {
		t.Errorf("new values: generation %d, want 2", secretMeta(res.Object).GetGeneration())
	}
	again, _ := b.Get(user, key)
	if again.(*secretsv1alpha1.Secret).GetStatus().GetContentHmac() == st.GetContentHmac() {
		t.Error("the content HMAC did not change with the values")
	}

	// Errors name what is wrong, never the value.
	bad := registrySecret("bad", []byte(`{"auths":{"ghcr.io":{"password":"`+secretCanary+`"`))
	if _, err := b.Apply(user, bad); connect.CodeOf(err) != connect.CodeInvalidArgument || strings.Contains(err.Error(), secretCanary) {
		t.Errorf("a bad config: %v", err)
	}
	bad = registrySecret("bad", dockerConfig("ghcr.io", "", secretCanary))
	if _, err := b.Apply(user, bad); connect.CodeOf(err) != connect.CodeInvalidArgument || strings.Contains(err.Error(), secretCanary) {
		t.Errorf("a login without a username: %v", err)
	}
	// An opaque secret's values are as write-only.
	res, err = b.Apply(user, opaque("env", map[string]string{"GITHUB_TOKEN": secretCanary}))
	if err != nil {
		t.Fatal(err)
	}
	noValues(t, "apply an opaque secret", res.Object)
	if keys := secretStatus(res.Object).GetKeys(); !slices.Equal(keys, []string{"GITHUB_TOKEN"}) {
		t.Errorf("an opaque secret's keys: %v", keys)
	}
	if _, err := b.Apply(user, opaque("env2", map[string]string{"NOT-A-NAME": secretCanary})); connect.CodeOf(err) != connect.CodeInvalidArgument || strings.Contains(err.Error(), secretCanary) {
		t.Errorf("an opaque key that is no environment name: %v", err)
	}

	for _, k := range []resource.Key{key, {Project: "web", Name: "env"}} {
		deleted, err := b.Delete(user, k, nil)
		if err != nil {
			t.Fatal(err)
		}
		noValues(t, "delete", deleted)
	}
	testutil.Eventually(t, "the watch saw the deletes", func() bool {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, ev := range events {
			if ev.Type == metav1.EventType_EVENT_TYPE_DELETED {
				n++
			}
		}
		return n == 2
	})
	stopWatch()
	<-watched
	mu.Lock()
	for _, ev := range events {
		if ev.Object != nil {
			noValues(t, "watch "+ev.Type.String(), ev.Object)
		}
	}
	mu.Unlock()
	if strings.Contains(log.String(), secretCanary) {
		t.Errorf("the log holds the value:\n%s", log.String())
	}
}

func secretMeta(m proto.Message) *metav1.ObjectMeta {
	return m.(*secretsv1alpha1.Secret).GetMetadata()
}

func secretStatus(m proto.Message) *secretsv1alpha1.SecretStatus {
	return m.(*secretsv1alpha1.Secret).GetStatus()
}

// TestSecretDataAtRestIsCiphertext: the database's files, its history
// included, hold no value of a secret, nor its Docker config at all.
func TestSecretDataAtRestIsCiphertext(t *testing.T) {
	dir := t.TempDir()
	s, _, user := runningAt(t, filepath.Join(dir, "tower.db"), withSecrets(keyring(t, secrets.NewKEK("k1")), nil))
	b := s.backend(registry.Secret)
	for i := range 3 {
		// Each write leaves an event too.
		if _, err := b.Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", secretCanary+string(rune('a'+i))))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.Delete(user, resource.Key{Project: "web", Name: "ghcr"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", secretCanary))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Apply(user, opaque("env", map[string]string{"GITHUB_TOKEN": secretCanary + "-opaque"})); err != nil {
		t.Fatal(err)
	}
	stored, err := s.store.Get(t.Context(), s.backend(registry.Secret).storeKey(resource.Key{Project: "web", Name: "ghcr"}))
	if err != nil {
		t.Fatal(err)
	}
	var sec secretsv1alpha1.Secret
	if err := proto.Unmarshal(stored.Data, &sec); err != nil {
		t.Fatal(err)
	}
	if sec.GetSpec().GetData() != nil || len(sec.GetSpec().GetSealed().GetCiphertext()) == 0 {
		t.Fatalf("stored spec: data %v, sealed %v", sec.GetSpec().GetData(), sec.GetSpec().GetSealed())
	}
	files, err := filepath.Glob(filepath.Join(dir, "tower.db*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("database files: %v %v", files, err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{secretCanary, `"auths"`, base64.StdEncoding.EncodeToString([]byte(secretCanary))} {
			if bytes.Contains(raw, []byte(needle)) {
				t.Errorf("%s holds %q in the clear", filepath.Base(f), needle)
			}
		}
	}
}

// TestSecretAppliesAreChecked: an apply carries the data, every time, never
// the sealed form, and holds to its type.
func TestSecretAppliesAreChecked(t *testing.T) {
	s, _, user := runningWith(t, withSecrets(keyring(t, secrets.NewKEK("k1")), nil))
	b := s.backend(registry.Secret)
	if _, err := b.Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", "p"))); err != nil {
		t.Fatal(err)
	}
	read, err := b.Get(user, resource.Key{Project: "web", Name: "ghcr"})
	if err != nil {
		t.Fatal(err)
	}
	// What get answers, applied back, has no data.
	if _, err := b.Apply(user, read); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "write-only") {
		t.Errorf("applying what get answered: %v, want refused as write-only", err)
	}
	sealed := registrySecret("ghcr", dockerConfig("ghcr.io", "me", "p"))
	sealed.Spec.Sealed = &secretsv1alpha1.SealedData{KekId: "k1"}
	sealedNew := registrySecret("fresh", dockerConfig("ghcr.io", "me", "p"))
	sealedNew.Spec.Sealed = &secretsv1alpha1.SealedData{KekId: "k1"}
	wrongType := registrySecret("other", dockerConfig("ghcr.io", "me", "p"))
	wrongType.Spec.Type = secretsv1alpha1.SecretType_SECRET_TYPE_UNSPECIFIED
	wrongKey := registrySecret("other", nil)
	wrongKey.Spec.Data = map[string][]byte{"config.json": dockerConfig("ghcr.io", "me", "p")}
	large := registrySecret("other", dockerConfig("ghcr.io", "me", strings.Repeat("p", secrets.MaxDataSize)))
	largeOpaque := opaque("other", map[string]string{"A": strings.Repeat("p", secrets.MaxDataSize)})
	for what, sec := range map[string]*secretsv1alpha1.Secret{
		"the sealed form": sealed, "the sealed form on create": sealedNew, "no type": wrongType, "another key": wrongKey,
		"too much data": large, "too much opaque data": largeOpaque,
	} {
		if _, err := b.Apply(user, sec); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want invalid", what, err)
		}
	}
}

// TestSecretsNeedAKEK: without a key-encryption key, writing a secret fails
// and says why; with none stored, the apiserver starts all the same.
func TestSecretsNeedAKEK(t *testing.T) {
	s, _, user := runningWith(t, func(c *Config) { c.Secrets = nil })
	if err := s.PrepareSecrets(t.Context()); err != nil {
		t.Errorf("an empty store without a KEK: %v", err)
	}
	_, err := s.backend(registry.Secret).Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", "p")))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "key-encryption key") {
		t.Errorf("writing a secret without a KEK: %v", err)
	}
}

// TestTheKEKIsCheckedAtStart: an apiserver over stored secrets refuses to
// start without the KEK they were sealed under, or with a wrong one, and
// seals them again under a new one given the old.
func TestTheKEKIsCheckedAtStart(t *testing.T) {
	k1 := secrets.NewKEK("k1")
	s, system, user := runningWith(t, withSecrets(keyring(t, k1), nil))
	if _, err := s.backend(registry.Secret).Apply(user, registrySecret("ghcr", dockerConfig("ghcr.io", "me", secretCanary))); err != nil {
		t.Fatal(err)
	}
	restart := func(k *secrets.Keyring) *Server {
		return New(Config{Store: s.store, Secrets: k, Clock: s.clock})
	}
	for what, k := range map[string]*secrets.Keyring{
		"no KEK":                     nil,
		"another KEK of the same id": keyring(t, secrets.NewKEK("k1")),
		"a new KEK without the old":  keyring(t, secrets.NewKEK("k2")),
		"a new KEK with a wrong old": keyring(t, secrets.NewKEK("k2"), secrets.NewKEK("k1")),
	} {
		if err := restart(k).PrepareSecrets(t.Context()); err == nil {
			t.Errorf("started with %s", what)
		}
	}
	if err := restart(keyring(t, k1)).PrepareSecrets(t.Context()); err != nil {
		t.Errorf("started again with the same KEK: %v", err)
	}
	k2 := secrets.NewKEK("k2")
	if err := restart(keyring(t, k2, k1)).PrepareSecrets(t.Context()); err != nil {
		t.Fatalf("rotating to a new KEK: %v", err)
	}
	after := restart(keyring(t, k2))
	if err := after.PrepareSecrets(t.Context()); err != nil {
		t.Fatalf("the new KEK alone, once rotated: %v", err)
	}
	obj, err := after.get(system, registry.Secret, resource.Key{Project: "web", Name: "ghcr"})
	if err != nil {
		t.Fatal(err)
	}
	sec := obj.(*secretsv1alpha1.Secret)
	if sec.GetSpec().GetSealed().GetKekId() != "k2" || sec.GetMetadata().GetGeneration() != 1 {
		t.Errorf("after rotating: KEK %s, generation %d", sec.GetSpec().GetSealed().GetKekId(), sec.GetMetadata().GetGeneration())
	}
	if sec.GetStatus().GetContentHmac() != sec.GetSpec().GetSealed().GetContentHmac() {
		t.Errorf("after rotating, the status's HMAC %q is not the sealed data's %q", sec.GetStatus().GetContentHmac(), sec.GetSpec().GetSealed().GetContentHmac())
	}
	data, err := keyring(t, k2).Open(secretObject(sec), sec.GetSpec().GetSealed())
	if err != nil || !bytes.Contains(data[secrets.DockerConfigKey], []byte(secretCanary)) {
		t.Errorf("the rotated secret does not open to its data: %v", err)
	}
}

// TestASecretsStatusIsAtItsGeneration: every apply leaves a secret's
// status observed at the generation it is stored at, and Ready, so a client
// waiting for observed == generation sees it at once.
func TestASecretsStatusIsAtItsGeneration(t *testing.T) {
	s, _, user := running(t)
	b := s.backend(registry.Secret)
	check := func(what string, m proto.Message, gen int64) *metav1.Condition {
		t.Helper()
		sec := m.(*secretsv1alpha1.Secret)
		st := sec.GetStatus()
		ready := conditions.Find(st.GetConditions(), SecretReady)
		if sec.GetMetadata().GetGeneration() != gen || st.GetObservedGeneration() != gen ||
			!conditions.IsTrue(st.GetConditions(), SecretReady) || ready.GetObservedGeneration() != gen || ready.GetLastTransitionTime() == nil {
			t.Errorf("%s: generation %d, status %v; want both at %d, and Ready there", what, sec.GetMetadata().GetGeneration(), st, gen)
		}
		return ready
	}
	created, err := b.Apply(user, opaque("env", map[string]string{"A": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	first := check("created", created.Object, 1)
	same, err := b.Apply(user, opaque("env", map[string]string{"A": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	check("the same values again", same.Object, 1)
	if secretMeta(same.Object).GetResourceVersion() != secretMeta(created.Object).GetResourceVersion() {
		t.Error("the same values again wrote")
	}
	changed, err := b.Apply(user, opaque("env", map[string]string{"A": "2"}))
	if err != nil {
		t.Fatal(err)
	}
	ready := check("new values", changed.Object, 2)
	if !ready.GetLastTransitionTime().AsTime().Equal(first.GetLastTransitionTime().AsTime()) {
		t.Errorf("Ready moved from %v to %v, though it stayed true", first.GetLastTransitionTime(), ready.GetLastTransitionTime())
	}
	labelled := opaque("env", map[string]string{"A": "2"})
	labelled.Metadata.Labels = map[string]string{"tier": "db"}
	relabelled, err := b.Apply(user, labelled)
	if err != nil {
		t.Fatal(err)
	}
	check("a label change", relabelled.Object, 2)
	got, err := b.Get(user, resource.Key{Project: "web", Name: "env"})
	if err != nil {
		t.Fatal(err)
	}
	check("read back", got, 2)
}

// TestOpenSecretIsTheSystemsAlone: the apiserver's own code opens a
// secret's values; no other caller, root on the host and admin tokens
// included, gets them, and a secret gone or being deleted opens to nothing.
func TestOpenSecretIsTheSystemsAlone(t *testing.T) {
	s, system, user := running(t)
	b := s.backend(registry.Secret)
	if _, err := b.Apply(user, opaque("env", map[string]string{"GITHUB_TOKEN": secretCanary})); err != nil {
		t.Fatal(err)
	}
	key := resource.Key{Project: "web", Name: "env"}
	data, err := s.OpenSecret(system, key)
	if err != nil || string(data["GITHUB_TOKEN"]) != secretCanary {
		t.Fatalf("the system opening a secret: %v, %v", data, err)
	}
	workstream := serviceToken(t, s, system, "ws", iamv1alpha1.Role_ROLE_WORKSTREAM, "")
	for what, ctx := range map[string]context.Context{"an admin token": user, "root on the host": local, "a workstream token": workstream, "no one": context.Background()} {
		if data, err := s.OpenSecret(ctx, key); connect.CodeOf(err) != connect.CodePermissionDenied || data != nil {
			t.Errorf("%s opening a secret: %v, %v", what, data, err)
		}
	}
	if _, err := s.OpenSecret(system, resource.Key{Project: "web", Name: "absent"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("opening a secret that is not there: %v", err)
	}
	held := opaque("held", map[string]string{"A": "x"})
	held.Metadata.Finalizers = []string{"tower.hangarlab.cloud/test"}
	if _, err := b.Apply(system, held); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Delete(user, resource.Key{Project: "web", Name: "held"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenSecret(system, resource.Key{Project: "web", Name: "held"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("opening a secret being deleted: %v", err)
	}
}
