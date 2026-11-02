package apiserver

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
)

const scanValue = "s3cr3t/Value+With=Odd&Bytes"

// scan writes in through a scanner for scanValue in pieces of n bytes,
// returning what it passed on and why it stopped.
func scan(t *testing.T, in string, n int, limit int64) (string, *scanStop) {
	t.Helper()
	patterns, short := secretPatterns([]namedSecret{{name: "$V", value: []byte(scanValue)}})
	if len(short) > 0 {
		t.Fatalf("too short: %v", short)
	}
	var out bytes.Buffer
	s := newScanner(patterns, limit, &out)
	for b := []byte(in); len(b) > 0; {
		k := min(n, len(b))
		if _, err := s.Write(b[:k]); err != nil {
			return out.String(), s.stop
		}
		b = b[k:]
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	return out.String(), s.stop
}

// TestTheScanFindsAValueSplitAcrossWrites: a value, or an encoding of it,
// split across any two writes is found before a byte of it is passed on;
// a stream without one passes whole.
func TestTheScanFindsAValueSplitAcrossWrites(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("prefix " + scanValue + " suffix"))
	var wrapped strings.Builder
	for i := 0; i < len(b64); i += 16 {
		wrapped.WriteString(b64[i:min(i+16, len(b64))] + "\r\n")
	}
	for _, tt := range []struct {
		name, in, form string
	}{
		{"raw", strings.Repeat("a", 5000) + scanValue + strings.Repeat("b", 5000), "raw"},
		{"wrapped base64", strings.Repeat("a", 3000) + "\n" + wrapped.String(), "base64"},
	} {
		for _, n := range []int{1, 7, 4096, 1 << 20} {
			out, stop := scan(t, tt.in, n, 0)
			if stop == nil || stop.pattern == nil || stop.pattern.name != "$V" || stop.pattern.form != tt.form {
				t.Errorf("%s in writes of %d: stopped %+v", tt.name, n, stop)
				continue
			}
			if tt.form == "raw" && strings.Contains(out, scanValue[:len(scanValue)-1]) {
				t.Errorf("%s in writes of %d: passed on most of the value", tt.name, n)
			}
			if tt.form == "base64" && strings.Contains(strings.ReplaceAll(strings.ReplaceAll(out, "\n", ""), "\r", ""), b64[:20]) {
				t.Errorf("%s in writes of %d: passed on the encoding's start", tt.name, n)
			}
		}
	}
	clean := strings.Repeat("line of output\n", 2000) + strings.Repeat("\n", 300) + "end"
	for _, n := range []int{1, 13, 1 << 20} {
		if out, stop := scan(t, clean, n, 0); stop != nil || out != clean {
			t.Errorf("a clean stream in writes of %d: stopped %+v, passed %d of %d bytes", n, stop, len(out), len(clean))
		}
	}
	if _, stop := scan(t, clean, 100, 1000); stop == nil || stop.pattern != nil || stop.max != 1000 {
		t.Errorf("a stream past its max: stopped %+v", stop)
	}
}

// TestEachFormOfAValueIsFound: a value written into a longer text in
// each form, at each byte alignment of an encoding, is found.
func TestEachFormOfAValueIsFound(t *testing.T) {
	escaped, _ := json.Marshal(scanValue)
	var uni strings.Builder
	for _, r := range scanValue {
		if 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' {
			uni.WriteRune(r)
		} else {
			uni.WriteString(`\u00` + strings.ToUpper(hex.EncodeToString([]byte{byte(r)})))
		}
	}
	ins := map[string]string{
		"json":           `{"v":` + string(escaped) + `}`,
		"json, \\/":      `{"v":"` + strings.ReplaceAll(scanValue, "/", `\/`) + `"}`,
		"json, \\u":      `{"v":"` + uni.String() + `"}`,
		"url":            "a=b&v=s3cr3t%2fValue%2bWith%3dOdd%26Bytes",
		"hex":            "0x" + strings.ToUpper(hex.EncodeToString([]byte(scanValue))),
		"base64url, raw": base64.RawURLEncoding.EncodeToString([]byte(scanValue)),
	}
	for pad := range 6 {
		ins["base64 after "+strings.Repeat(".", pad)] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat(".", pad) + scanValue + "!"))
		ins["base64url after "+strings.Repeat(".", pad)] = base64.URLEncoding.EncodeToString([]byte(strings.Repeat("?", pad) + scanValue))
	}
	for name, in := range ins {
		if _, stop := scan(t, in, 5, 0); stop == nil || stop.pattern == nil {
			t.Errorf("%s: %q not found", name, in)
		}
	}
}

// The credential files TestNeedleTiers scans for, and text that holds
// only their structure.
const (
	tierToken = "eyJhbGciOiJSUzI1NiIsImtpZCI6IjEyMyJ9.eyJzdWIiOiJjaSJ9.Sig_abc-XYZ0123456789"
	tierCA    = "LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUJkekNDQVIyZ0F3SUJBZ0lCQURBS0JnZ3Foa2pPUFFRREFqQWpNU0V3SHdZRFZRUUREQmhyTTNNdGMyVnk="
	tierKube  = `apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority-data: ` + tierCA + `
    server: https://kube1.example.com:6443
  name: prod
contexts:
- context:
    cluster: prod
    namespace: ci-runners-2024
    user: ci
  name: prod
current-context: prod
preferences: {}
users:
- name: ci
  user:
    client-certificate: /var/lib/kubelet/pki/kubelet-client-2024-01-01.pem
    token: ` + tierToken + "\n"
	// Split so GitHub push protection does not take it for a real token.
	tierNetrcPassword = "ghp_" + "R4nd0mT0kenValue9988AbCdEf"
	tierNetrc         = "machine github.com\nlogin ci-bot-2024\npassword " + tierNetrcPassword + "\nmachine registry.npmjs.org login ci password npm_9fA8bC7dE6fG5hI4jK3l\n"
	tierStructure     = `apiVersion: v1
kind: Config
    server: https://kube1.example.com:6443
    namespace: ci-runners-2024
    client-certificate: /var/lib/kubelet/pki/kubelet-client-2024-01-01.pem
current-context: prod
machine github.com
login ci-bot-2024
-----BEGIN OPENSSH PRIVATE KEY-----
-----END OPENSSH PRIVATE KEY-----
`
)

// pemKey is a real RSA key as PEM, its body lines, and its body joined.
func pemKey(t *testing.T) (key string, lines []string, body string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	key = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	for l := range strings.Lines(key) {
		if l = strings.TrimSpace(l); !strings.HasPrefix(l, "-----") {
			lines = append(lines, l)
		}
	}
	return key, lines, base64.StdEncoding.EncodeToString(der)
}

// TestNeedleTiers: a secret's value whole, trimmed, and a PEM key's body
// are strong; a credential-shaped line or field of a value of several is
// weak; structure (keys, hosts, paths, names, PEM markers) is neither. A
// value under 8 bytes trimmed cannot be scanned for.
func TestNeedleTiers(t *testing.T) {
	key, keyLines, keyBody := pemKey(t)
	patterns, short := secretPatterns([]namedSecret{
		{name: "$PIN", value: []byte("4711")},
		{name: "$PADDED", value: []byte("  abc  \n\n\n")},
		{name: "/kube", value: []byte(tierKube)},
		{name: "/netrc", value: []byte(tierNetrc)},
		{name: "/key", value: []byte(key)},
		{name: "$TOKEN", value: []byte(tierToken)},
	})
	if !slices.Equal(short, []string{"$PIN", "$PADDED"}) {
		t.Errorf("too short: %v", short)
	}
	strong, weak := map[string]string{}, map[string]string{}
	for _, p := range patterns {
		if p.form != "raw" {
			continue
		}
		if p.strong {
			strong[string(p.b)] = p.name
		} else {
			weak[string(p.b)] = p.name
		}
	}
	for value, name := range map[string]string{
		tierKube: "/kube", strings.TrimSpace(tierKube): "/kube", tierNetrc: "/netrc", key: "/key", keyBody: "/key", tierToken: "$TOKEN",
	} {
		if strong[value] != name {
			t.Errorf("%s's %.20q... is not strong", name, value)
		}
	}
	for value, name := range map[string]string{
		strings.TrimRight(tierCA, "="): "/kube", tierNetrcPassword: "/netrc", "npm_9fA8bC7dE6fG5hI4jK3l": "/netrc", keyLines[0]: "/key", keyLines[5]: "/key",
	} {
		if weak[value] != name {
			t.Errorf("%s's %q is not weak", name, value)
		}
	}
	// A piece that is a whole secret too is strong.
	if _, ok := weak[tierToken]; ok || strong[tierToken] != "$TOKEN" {
		t.Errorf("the token, a whole secret and a piece of another, is weak")
	}
	var out bytes.Buffer
	s := newScanner(patterns, 0, &out)
	if _, err := s.Write([]byte(tierStructure)); err != nil || s.Flush() != nil || s.suspect != nil {
		t.Errorf("structure alone: %v, suspect %+v", err, s.suspect)
	}
	for _, p := range []string{"apiVersion", "kube1.example.com", "//kube1.example.com", "ci-runners-2024", "/var/lib/kubelet/pki/kubelet-client-2024-01-01.pem", "github.com", "ci-bot-2024", "registry.npmjs.org"} {
		if credentialShaped([]byte(p)) {
			t.Errorf("%q is credential-shaped", p)
		}
	}
}

// TestAWeakHitWaitsForAStrongOne: after a piece of a secret, the scan
// passes nothing more on but reads on, and the whole secret after it
// decides; without it the piece does.
func TestAWeakHitWaitsForAStrongOne(t *testing.T) {
	patterns, _ := secretPatterns([]namedSecret{{name: "/netrc", value: []byte(tierNetrc)}})
	for _, tt := range []struct {
		in     string
		strong bool
	}{
		{strings.Repeat("x", 1000) + tierNetrcPassword + strings.Repeat("y", 9000) + tierNetrc, true},
		{strings.Repeat("x", 1000) + tierNetrcPassword + strings.Repeat("y", 9000), false},
	} {
		var out bytes.Buffer
		s := newScanner(patterns, 0, &out)
		for b := []byte(tt.in); len(b) > 0; b = b[min(len(b), 100):] {
			if _, err := s.Write(b[:min(len(b), 100)]); err != nil {
				break
			}
		}
		switch {
		case tt.strong && (s.stop == nil || s.stop.pattern == nil || !s.stop.pattern.strong):
			t.Errorf("the whole secret after its piece: stop %+v", s.stop)
		case !tt.strong && (s.stop != nil || s.suspect == nil || s.Flush() == nil):
			t.Errorf("a piece alone: stop %+v, suspect %+v", s.stop, s.suspect)
		}
		if strings.Contains(out.String(), tierNetrcPassword) || out.Len() > 1000 {
			t.Errorf("passed on %d bytes, past the piece", out.Len())
		}
	}
}

// TestTheWriteCrossingTheCapIsScannedFirst: a secret in the write that
// passes max_bytes, before the limit, is found, and decides over the
// size; past the limit it is not read.
func TestTheWriteCrossingTheCapIsScannedFirst(t *testing.T) {
	in := strings.Repeat("a", 100) + scanValue + strings.Repeat("b", 5000)
	limit := int64(100 + len(scanValue) + 10)
	if _, stop := scan(t, in, 1<<20, limit); stop == nil || stop.pattern == nil {
		t.Errorf("a secret inside the crossing write: stopped %+v", stop)
	}
	if _, stop := scan(t, in, 1<<20, 100+int64(len(scanValue))/2); stop == nil || stop.pattern != nil {
		t.Errorf("a secret past the limit: stopped %+v; want the size", stop)
	}
	// A weak piece within the limit decides over the size too.
	patterns, _ := secretPatterns([]namedSecret{{name: "/netrc", value: []byte(tierNetrc)}})
	s := newScanner(patterns, 200, io.Discard)
	_, _ = s.Write([]byte("x " + tierNetrcPassword + strings.Repeat("y", 500)))
	if r := s.refusal(); r.GetReason().String() != "COPY_REFUSAL_REASON_SUSPECT" {
		t.Errorf("a piece inside the crossing write: %v", r)
	}
}

// TestMoreCredentialShapes: tokens of lower case and digits and UUIDs
// are pieces worth looking for; names and dates of the same characters
// are not.
func TestMoreCredentialShapes(t *testing.T) {
	for p, want := range map[string]bool{
		// Split so GitHub push protection does not take it for a real token.
		"xoxb-" + "1234567890-0987654321-a1b2c3d4e5f6g7h8i9j0": true,
		"550e8400-e29b-41d4-a716-446655440000":                 true,
		"q8w7e6r5t4y3u2i1o9p0a8s7d6f5":                         true,
		"kubelet-client-2024-01-01":                            false,
		"ci-runners-2024-eu-west-1":                            false,
		"ip-10-0-1-23-eu-west-1":                               false,
		"node1.cluster2.example.com":                           false,
		"aaaaaaaaaaaaaaaaaaaa1111111111":                       false,
	} {
		if got := credentialShaped([]byte(p)); got != want {
			t.Errorf("%q credential-shaped: %v, want %v", p, got, want)
		}
	}
}

// TestAPEMKeyInJSONIsFound: a service account's JSON, its private_key a
// PEM key with escaped line breaks, gives the key's body as a strong
// needle and its lines as weak ones.
func TestAPEMKeyInJSONIsFound(t *testing.T) {
	key, lines, body := pemKey(t)
	escaped, _ := json.Marshal(key)
	sa := `{"type":"service_account","client_id":"550e8400-e29b-41d4-a716-446655440000","private_key":` + string(escaped) + "}\n"
	patterns, _ := secretPatterns([]namedSecret{{name: "/sa.json", value: []byte(sa)}})
	var bodyStrong, lineWeak, idWeak bool
	for _, p := range patterns {
		bodyStrong = bodyStrong || p.form == "raw" && p.strong && string(p.b) == body
		lineWeak = lineWeak || p.form == "raw" && !p.strong && string(p.b) == lines[3]
		idWeak = idWeak || p.form == "raw" && !p.strong && string(p.b) == "550e8400-e29b-41d4-a716-446655440000"
	}
	if !bodyStrong || !lineWeak || !idWeak {
		t.Errorf("the key's body strong %v, a line weak %v, the client id weak %v", bodyStrong, lineWeak, idWeak)
	}
	// The key alone, out of its JSON, is found whole.
	if _, stop := scanFor(t, patterns, key); stop == nil || stop.pattern == nil {
		t.Errorf("the key itself: stopped %+v", stop)
	}
}

// TestPartlyEscapedURLForms: a value in a URL's path or userinfo, as Go
// escapes it there, is found.
func TestPartlyEscapedURLForms(t *testing.T) {
	const v = "p@ss:w/rd+Value=Odd&9!"
	patterns, _ := secretPatterns([]namedSecret{{name: "$PW", value: []byte(v)}})
	for _, in := range []string{
		"https://example.com/x/" + url.PathEscape(v) + "/y",
		(&url.URL{Scheme: "https", Host: "example.com", User: url.UserPassword("ci", v)}).String(),
		(&url.URL{Scheme: "https", Host: "example.com", User: url.User(v)}).String(),
	} {
		if _, stop := scanFor(t, patterns, in); stop == nil || stop.pattern == nil || stop.pattern.form != "url" {
			t.Errorf("%q: stopped %+v", in, stop)
		}
	}
}

// scanFor writes in through a scanner for patterns, as scan does.
func scanFor(t *testing.T, patterns []scanPattern, in string) (string, *scanStop) {
	t.Helper()
	var out bytes.Buffer
	s := newScanner(patterns, 0, &out)
	if _, err := s.Write([]byte(in)); err != nil {
		return out.String(), s.stop
	}
	_ = s.Flush()
	return out.String(), s.stop
}

// TestHistoryIsBounded: a secret keeps the 16 newest values it held, for
// 90 days each, and its history then starts where what it dropped ended.
func TestHistoryIsBounded(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	var superseded []*secretsv1alpha1.SupersededData
	for i := range 20 {
		superseded = append(superseded, &secretsv1alpha1.SupersededData{SupersededTime: timestamppb.New(now.Add(-time.Duration(i) * time.Hour))})
	}
	kept, since := pruneHistory(superseded, nil, now)
	if len(kept) != maxSuperseded || !since.AsTime().Equal(now.Add(-16*time.Hour)) {
		t.Errorf("past 16 values: kept %d, since %v", len(kept), since.AsTime())
	}
	old := []*secretsv1alpha1.SupersededData{
		{SupersededTime: timestamppb.New(now.Add(-24 * time.Hour))},
		{SupersededTime: timestamppb.New(now.Add(-91 * 24 * time.Hour))},
	}
	kept, since = pruneHistory(old, nil, now)
	if len(kept) != 1 || !since.AsTime().Equal(now.Add(-91*24*time.Hour)) {
		t.Errorf("past 90 days: kept %d, since %v", len(kept), since.AsTime())
	}
	if kept, since := pruneHistory(old[:1], nil, now); len(kept) != 1 || since != nil {
		t.Errorf("within bounds: kept %d, since %v", len(kept), since)
	}
}
