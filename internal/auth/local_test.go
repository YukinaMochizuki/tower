package auth_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/auth"
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
	t.Run("the node socket's peers are not seen", func(t *testing.T) {
		// Root's node agent on the node socket is not root on the admin one.
		r := overSocket(t, auth.ConnContext, auth.LocalRoot{Host: "n1", UID: me}, nil)
		if r.ok || r.err != nil {
			t.Fatalf("a peer the node listener recorded was recognized: %v, %t, %v", r.p, r.ok, r.err)
		}
	})
	t.Run("the admin socket's peers are not a node", func(t *testing.T) {
		r := overSocket(t, auth.AdminConnContext, auth.PeerCred{NodeUIDs: []uint32{me}, NodeName: "n1"}, nil)
		if r.ok || r.err != nil {
			t.Fatalf("a peer the admin listener recorded was a node: %v, %t, %v", r.p, r.ok, r.err)
		}
	})
	t.Run("no header makes it", func(t *testing.T) {
		h := http.Header{auth.DevUserHeader: {"root@n1"}, "Authorization": {"Bearer x"}}
		_, ok, err := auth.LocalRoot{Host: "n1", UID: me}.Authenticate(t.Context(), h)
		if ok || err != nil {
			t.Fatalf("headers without a connection were recognized: %t, %v", ok, err)
		}
	})
}

// No authenticator but LocalRoot makes a Local principal, whatever the
// request says.
func TestOnlyLocalRootMakesLocal(t *testing.T) {
	h := http.Header{auth.DevUserHeader: {"local:root@n1"}}
	p, ok, err := auth.Dev{}.Authenticate(t.Context(), h)
	if err != nil || !ok || p.Type != auth.User {
		t.Fatalf("dev made %v, %t, %v; want a user", p, ok, err)
	}
	me := uint32(os.Getuid())
	r := overSocket(t, auth.ConnContext, auth.PeerCred{NodeUIDs: []uint32{me}, NodeName: "n1"}, h)
	if r.p.Type != auth.Node {
		t.Fatalf("the node socket made %v", r.p)
	}
}

func TestDevRefusesCloudflareTraffic(t *testing.T) {
	for _, h := range []string{"Cf-Connecting-Ip", "CDN-Loop", "cf-ray", "Cf-Warp-Tag-Id"} {
		t.Run(h, func(t *testing.T) {
			header := http.Header{}
			header.Set(auth.DevUserHeader, "root@example.com")
			header.Set(h, "x")
			_, err := auth.Chain{auth.Dev{}}.Authenticate(t.Context(), header)
			if err == nil {
				t.Fatal("dev auth took a request that came through Cloudflare")
			}
			if !strings.Contains(err.Error(), "came through Cloudflare") {
				t.Errorf("refusal %q does not say why", err)
			}
		})
	}
	t.Run("even without a dev user", func(t *testing.T) {
		_, ok, err := auth.Dev{}.Authenticate(t.Context(), http.Header{"Cdn-Loop": {"cloudflare"}})
		if ok || err == nil {
			t.Fatalf("got %t, %v; want a refusal", ok, err)
		}
	})
}
