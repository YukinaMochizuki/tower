package auth_test

import (
	"net/http"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/auth"
)

func TestChain(t *testing.T) {
	chain := auth.Chain{auth.Dev{}}
	h := http.Header{}
	if _, err := chain.Authenticate(t.Context(), h); err == nil {
		t.Error("a request with no credentials was authenticated")
	}
	h.Set(auth.DevPrincipalHeader, "serviceToken:workstream")
	ctx, err := chain.Authenticate(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := auth.FromContext(ctx); p != (auth.Principal{Type: auth.ServiceToken, ID: "workstream"}) {
		t.Errorf("principal %v", p)
	}
}

// TestDevNamesOnlyServiceTokens: dev auth takes a service token, and
// refuses the system, root on the host and what names no principal.
func TestDevNamesOnlyServiceTokens(t *testing.T) {
	p, ok, err := auth.Dev{}.Authenticate(t.Context(), http.Header{auth.DevPrincipalHeader: {"serviceToken:a"}})
	if want := (auth.Principal{Type: auth.ServiceToken, ID: "a"}); err != nil || !ok || p != want {
		t.Errorf("got %v, %t, %v; want %v", p, ok, err, want)
	}
	for _, v := range []string{"system:apiserver", "local:root@n1", "user:alice@example.com", "workstream", "serviceToken:"} {
		if p, ok, err := (auth.Dev{}).Authenticate(t.Context(), http.Header{auth.DevPrincipalHeader: {v}}); err == nil || ok {
			t.Errorf("%s: got %v, %t, %v; want a refusal", v, p, ok, err)
		}
	}
}

func TestParsePrincipal(t *testing.T) {
	for _, p := range []auth.Principal{
		{Type: auth.ServiceToken, ID: "a"}, auth.SystemPrincipal, {Type: auth.Local, ID: "root@n1"},
	} {
		got, err := auth.ParsePrincipal(p.String())
		if err != nil || got != p {
			t.Errorf("%s read back as %v, %v", p, got, err)
		}
	}
}
