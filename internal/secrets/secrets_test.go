package secrets_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/secrets"
)

const canary = "canary-Zq81-value"

func config(host, user, pass string) map[string][]byte {
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	return map[string][]byte{secrets.DockerConfigKey: []byte(`{"auths":{"` + host + `":{"auth":"` + auth + `"}}}`)}
}

func keyring(t *testing.T, current secrets.KEK, previous ...secrets.KEK) *secrets.Keyring {
	t.Helper()
	r, err := secrets.NewKeyring(current, previous...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSealOpensOnlyForItsObject(t *testing.T) {
	r := keyring(t, secrets.NewKEK("k1"))
	data := config("ghcr.io", "me", canary)
	obj := secrets.Object("p", "s", "uid-1")
	sealed, err := r.Seal(obj, data)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed.GetCiphertext(), []byte(canary)) || bytes.Contains(sealed.GetWrappedKey(), []byte(canary)) {
		t.Fatal("the sealed data holds the value in the clear")
	}
	got, err := r.Open(obj, sealed)
	if err != nil || !bytes.Equal(got[secrets.DockerConfigKey], data[secrets.DockerConfigKey]) {
		t.Fatalf("open: %v", err)
	}
	for _, other := range []string{secrets.Object("q", "s", "uid-1"), secrets.Object("p", "t", "uid-1"), secrets.Object("p", "s", "uid-2")} {
		if _, err := r.Open(other, sealed); !errors.Is(err, secrets.ErrUndecryptable) {
			t.Errorf("opened for %s: %v", other, err)
		}
	}
	if _, err := keyring(t, secrets.NewKEK("k1")).Open(obj, sealed); !errors.Is(err, secrets.ErrUndecryptable) {
		t.Errorf("another key of the same id opened it: %v", err)
	}
	if _, err := keyring(t, secrets.NewKEK("k2")).Open(obj, sealed); !errors.Is(err, secrets.ErrUnknownKEK) {
		t.Errorf("a keyring without its KEK: %v", err)
	}
	tampered := proto.Clone(sealed).(*secretsv1alpha1.SealedData)
	tampered.Ciphertext[0] ^= 1
	if _, err := r.Open(obj, tampered); !errors.Is(err, secrets.ErrUndecryptable) {
		t.Errorf("tampered data opened: %v", err)
	}
}

func TestHMACIsKeyedAndStable(t *testing.T) {
	k := secrets.NewKEK("k1")
	r := keyring(t, k)
	a, _ := r.HMAC(config("ghcr.io", "me", canary))
	b, _ := r.HMAC(config("ghcr.io", "me", canary))
	c, _ := r.HMAC(config("ghcr.io", "me", canary+"x"))
	other, _ := keyring(t, secrets.NewKEK("k2")).HMAC(config("ghcr.io", "me", canary))
	if !secrets.SameContent(a, b) || secrets.SameContent(a, c) || secrets.SameContent(a, other) || secrets.SameContent("", "") {
		t.Errorf("hmacs: %s %s %s %s", a, b, c, other)
	}
	if !strings.HasPrefix(a, "hmac-sha256:") {
		t.Errorf("hmac %s", a)
	}
}

func TestRewrapMovesToTheCurrentKEK(t *testing.T) {
	old, cur := secrets.NewKEK("old"), secrets.NewKEK("new")
	obj := secrets.Object("p", "s", "u")
	sealed, err := keyring(t, old).Seal(obj, config("ghcr.io", "me", canary))
	if err != nil {
		t.Fatal(err)
	}
	r := keyring(t, cur, old)
	if _, err := r.Open(obj, sealed); err != nil {
		t.Fatalf("a previous KEK did not open: %v", err)
	}
	re, ok, err := r.Rewrap(obj, sealed)
	if err != nil || !ok || re.GetKekId() != "new" {
		t.Fatalf("rewrap: %v %v %v", re, ok, err)
	}
	want, _ := r.HMAC(config("ghcr.io", "me", canary))
	if re.GetContentHmac() != want {
		t.Error("the rewrapped HMAC is not the current KEK's")
	}
	if _, err := keyring(t, cur).Open(obj, re); err != nil {
		t.Errorf("the new KEK alone does not open it: %v", err)
	}
	if _, ok, _ := r.Rewrap(obj, re); ok {
		t.Error("rewrapped what the current KEK sealed")
	}
	if _, err := secrets.NewKeyring(cur, cur); err == nil {
		t.Error("the same id as current and previous was taken")
	}
}

func TestKEKFile(t *testing.T) {
	k := secrets.NewKEK("kek-1")
	path := filepath.Join(t.TempDir(), "kek")
	if err := os.WriteFile(path, []byte(k.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := secrets.LoadKEK(path)
	if err != nil || got.ID != "kek-1" || got.String() != k.String() {
		t.Fatalf("load: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.LoadKEK(path); err == nil {
		t.Error("a KEK others can read was taken")
	}
	for _, bad := range []string{"", "kek-1", "kek-1 c2hvcnQ=", "bad/id " + base64.StdEncoding.EncodeToString(make([]byte, 32)), "a b c"} {
		if _, err := secrets.ParseKEK([]byte(bad)); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestValidateNeverQuotesAValue(t *testing.T) {
	regAuth := secretsv1alpha1.SecretType_SECRET_TYPE_REGISTRY_AUTH
	if err := secrets.Validate(regAuth, config("ghcr.io", "me", canary)); err != nil {
		t.Fatal(err)
	}
	bad := []map[string][]byte{
		nil,
		{"other": []byte(canary)},
		{secrets.DockerConfigKey: []byte(canary)},
		{secrets.DockerConfigKey: []byte(`{"auths":{"ghcr.io":{"username":"` + canary + `"}}}`)},
		{secrets.DockerConfigKey: []byte(`{"auths":{"ghcr.io":{"auth":"` + canary + `"}}}`)},
		{secrets.DockerConfigKey: []byte(`{"auths":{}}`)},
		{secrets.DockerConfigKey: []byte(`{"auths":{"ghcr.io/org":{"identitytoken":"` + canary + `"}}}`)},
		{secrets.DockerConfigKey: []byte(`{"auths":{"docker.io":{"identitytoken":"x"},"index.docker.io":{"identitytoken":"` + canary + `"}}}`)},
		{secrets.DockerConfigKey: bytes.Repeat([]byte(canary), secrets.MaxDataSize/len(canary)+1)},
		{secrets.DockerConfigKey: []byte(`{"auths":{"ghcr.io":{"password":"` + canary + `"`)},
	}
	for i, d := range bad {
		err := secrets.Validate(regAuth, d)
		if err == nil {
			t.Errorf("case %d validated", i)
			continue
		}
		if strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "Zq81") {
			t.Errorf("case %d: the error quotes the value: %v", i, err)
		}
	}
	if err := secrets.Validate(secretsv1alpha1.SecretType_SECRET_TYPE_UNSPECIFIED, config("ghcr.io", "me", "p")); err == nil {
		t.Error("an unspecified type validated")
	}
}

// TestOpaqueKeysAreEnvironmentNames: an opaque secret's keys are
// environment variable names, its data is bounded as every type's, and no
// error quotes a value.
func TestOpaqueKeysAreEnvironmentNames(t *testing.T) {
	opaque := secretsv1alpha1.SecretType_SECRET_TYPE_OPAQUE
	for _, k := range []string{"A", "_", "GITHUB_TOKEN", "a1_b2", "_X9"} {
		if err := secrets.Validate(opaque, map[string][]byte{k: []byte(canary)}); err != nil {
			t.Errorf("%q: %v", k, err)
		}
	}
	if err := secrets.Validate(opaque, map[string][]byte{}); err != nil {
		t.Errorf("no keys: %v", err)
	}
	for _, k := range []string{"", "1A", "A-B", "A.B", "A B", "A=B", "é", secrets.DockerConfigKey} {
		err := secrets.Validate(opaque, map[string][]byte{"OK": nil, k: []byte(canary)})
		if err == nil {
			t.Errorf("%q validated", k)
			continue
		}
		if strings.Contains(err.Error(), "canary") {
			t.Errorf("%q: the error quotes the value: %v", k, err)
		}
	}
	big := map[string][]byte{"A": bytes.Repeat([]byte(canary), secrets.MaxDataSize/len(canary)+1)}
	if err := secrets.Validate(opaque, big); err == nil || strings.Contains(err.Error(), "canary") {
		t.Errorf("data over %d bytes: %v", secrets.MaxDataSize, err)
	}
}

func TestLoginsByRegistryHost(t *testing.T) {
	d := map[string][]byte{secrets.DockerConfigKey: []byte(`{"auths":{
		"https://index.docker.io/v1/":{"username":"hub","password":"p1"},
		"GHCR.io":{"identitytoken":"tok"},
		"localhost:5000":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("u:pa:ss")) + `"}}}`)}
	if got := secrets.Registries(d); strings.Join(got, ",") != "ghcr.io,index.docker.io,localhost:5000" {
		t.Errorf("registries %v", got)
	}
	for host, want := range map[string]secrets.Login{
		"index.docker.io": {Username: "hub", Password: "p1"},
		"ghcr.io":         {IdentityToken: "tok"},
		"localhost:5000":  {Username: "u", Password: "pa:ss"},
	} {
		if got, ok := secrets.LoginFor(d, host); !ok || got != want {
			t.Errorf("%s: %+v %v", host, got, ok)
		}
	}
	if _, ok := secrets.LoginFor(d, "quay.io"); ok {
		t.Error("a login for a registry it does not hold")
	}
	for in, want := range map[string]string{"docker.io": "index.docker.io", "registry-1.docker.io": "index.docker.io", "http://Reg.Example:443/v2/": "reg.example:443"} {
		if got, err := secrets.RegistryHost(in); err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
}
