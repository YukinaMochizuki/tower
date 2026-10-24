package auth_test

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/auth"
)

func TestChain(t *testing.T) {
	chain := auth.Chain{auth.Dev{}}
	h := http.Header{}
	if _, err := chain.Authenticate(t.Context(), h); err == nil {
		t.Error("a request with no credentials was authenticated")
	}
	h.Set(auth.DevUserHeader, "Bob@Example.com")
	ctx, err := chain.Authenticate(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := auth.FromContext(ctx); p != (auth.Principal{Type: auth.User, ID: "bob@example.com"}) {
		t.Errorf("principal %v", p)
	}
}

func TestPeerCred(t *testing.T) {
	me := uint32(os.Getuid())
	for _, tt := range []struct {
		name     string
		uids     []uint32
		nodeName string
		ok       bool
		wantErr  bool
	}{
		{name: "allowed uid", uids: []uint32{me}, nodeName: "n1", ok: true},
		{name: "other uid", uids: []uint32{me + 1}, nodeName: "n1", wantErr: true},
		{name: "no node name", uids: []uint32{me}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sock := filepath.Join(t.TempDir(), "s.sock")
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				p   auth.Principal
				ok  bool
				err error
			}
			got := make(chan result, 1)
			srv := &http.Server{
				ConnContext: auth.ConnContext,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					p, ok, err := auth.PeerCred{NodeUIDs: tt.uids, NodeName: tt.nodeName}.Authenticate(r.Context(), r.Header)
					got <- result{p, ok, err}
				}),
			}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			client := &http.Client{Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sock)
				},
			}}
			resp, err := client.Get("http://node/")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			r := <-got
			if (r.err != nil) != tt.wantErr || r.ok != tt.ok {
				t.Fatalf("got %v, %t, %v", r.p, r.ok, r.err)
			}
			if tt.ok && r.p != (auth.Principal{Type: auth.Node, ID: tt.nodeName}) {
				t.Errorf("principal %v", r.p)
			}
		})
	}
}

func TestPeerCredIgnoresTCP(t *testing.T) {
	_, ok, err := auth.PeerCred{NodeUIDs: []uint32{0}}.Authenticate(t.Context(), http.Header{})
	if ok || err != nil {
		t.Errorf("a request without peer credentials was recognized: %t, %v", ok, err)
	}
}
