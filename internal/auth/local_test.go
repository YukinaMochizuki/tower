package auth_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/auth"
)

type authResult struct {
	p   auth.Principal
	ok  bool
	err error
}

// overSocket serves one request on a unix socket whose connections connCtx
// records, authenticates it with a, and returns what a said.
func overSocket(t *testing.T, connCtx func(context.Context, net.Conn) context.Context, a auth.Authenticator, header http.Header) authResult {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan authResult, 1)
	srv := &http.Server{
		ConnContext: connCtx,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok, err := a.Authenticate(r.Context(), r.Header)
			got <- authResult{p, ok, err}
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://admin/", nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return <-got
}

func TestLocalRoot(t *testing.T) {
	me := uint32(os.Getuid())
	t.Run("the accepted uid is root on the host", func(t *testing.T) {
		r := overSocket(t, auth.AdminConnContext, auth.LocalRoot{Host: "n1", UID: me}, nil)
		if r.err != nil || !r.ok {
			t.Fatalf("got %v, %t, %v", r.p, r.ok, r.err)
		}
		if want := (auth.Principal{Type: auth.Local, ID: "root@n1"}); r.p != want {
			t.Errorf("principal %v, want %v", r.p, want)
		}
		if r.p.String() != "local:root@n1" {
			t.Errorf("principal reads %q in logs, want local:root@n1", r.p.String())
		}
	})
	t.Run("any other uid is refused", func(t *testing.T) {
		r := overSocket(t, auth.AdminConnContext, auth.LocalRoot{Host: "n1", UID: me + 1}, nil)
		if r.err == nil || r.ok {
			t.Fatalf("a uid that is not the accepted one got %v, %t, %v", r.p, r.ok, r.err)
		}
		if !strings.Contains(r.err.Error(), "is not root") {
			t.Errorf("refusal %q does not say why", r.err)
		}
	})
	t.Run("root by default", func(t *testing.T) {
		if os.Getuid() != 0 {
			t.Skip("needs root")
		}
		r := overSocket(t, auth.AdminConnContext, auth.LocalRoot{Host: "n1"}, nil)
		if r.err != nil || !r.ok {
			t.Fatalf("root got %v, %t, %v", r.p, r.ok, r.err)
		}
	})
	t.Run("another socket's peers are not seen", func(t *testing.T) {
		// A listener that records no peer for LocalRoot, as every one but
		// the admin socket, makes no one root, whoever connects.
		r := overSocket(t, nil, auth.LocalRoot{Host: "n1", UID: me}, nil)
		if r.ok || r.err != nil {
			t.Fatalf("a peer another listener took was recognized: %v, %t, %v", r.p, r.ok, r.err)
		}
	})
	t.Run("no header makes it", func(t *testing.T) {
		h := http.Header{auth.DevPrincipalHeader: {"local:root@n1"}, "Authorization": {"Bearer x"}}
		_, ok, err := auth.LocalRoot{Host: "n1", UID: me}.Authenticate(t.Context(), h)
		if ok || err != nil {
			t.Fatalf("headers without a connection were recognized: %t, %v", ok, err)
		}
	})
}

// No authenticator but LocalRoot makes a Local principal, whatever the
// request says.
func TestOnlyLocalRootMakesLocal(t *testing.T) {
	h := http.Header{auth.DevPrincipalHeader: {"local:root@n1"}}
	if p, ok, err := (auth.Dev{}).Authenticate(t.Context(), h); err == nil || ok {
		t.Fatalf("dev made %v, %t, %v; want a refusal", p, ok, err)
	}
	me := uint32(os.Getuid())
	r := overSocket(t, auth.AdminConnContext, auth.LocalRoot{Host: "n1", UID: me}, h)
	if r.p != (auth.Principal{Type: auth.Local, ID: "root@n1"}) {
		t.Fatalf("the admin socket made %v", r.p)
	}
}
