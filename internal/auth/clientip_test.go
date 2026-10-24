package auth_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/client"
)

// echoClientIP answers with the client address the request carries.
var echoClientIP = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	ip, ok := auth.ClientIP(r.Context())
	if !ok {
		_, _ = io.WriteString(w, "none")
		return
	}
	_, _ = io.WriteString(w, ip.String()+" header="+r.Header.Get(auth.ClientIPHeader))
})

func get(t *testing.T, hc *http.Client, url string, h http.Header) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = h
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func TestTrustClientIPFromTheEdgeAlone(t *testing.T) {
	serve := func(t *testing.T, uids []uint32) string {
		path := filepath.Join(t.TempDir(), "public.sock")
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: auth.TrustClientIPFrom(uids, echoClientIP), ConnContext: auth.ProxyConnContext}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
		return path
	}
	me := uint32(os.Getuid())
	t.Run("the edge's uid", func(t *testing.T) {
		hc := client.UnixSocket(serve(t, []uint32{me}))
		code, body := get(t, hc, client.UnixSocketURL+"/", http.Header{auth.ClientIPHeader: {"::ffff:192.0.2.7"}})
		// Believed, unmapped, and not passed on.
		if code != http.StatusOK || body != "192.0.2.7 header=" {
			t.Errorf("got %d %q", code, body)
		}
		if code, body := get(t, hc, client.UnixSocketURL+"/", nil); code != http.StatusBadRequest {
			t.Errorf("no address: %d %q; want 400", code, body)
		}
		if code, body := get(t, hc, client.UnixSocketURL+"/", http.Header{auth.ClientIPHeader: {"192.0.2.7, 10.0.0.1"}}); code != http.StatusBadRequest {
			t.Errorf("a list: %d %q; want 400", code, body)
		}
	})
	t.Run("another uid", func(t *testing.T) {
		hc := client.UnixSocket(serve(t, []uint32{me + 1}))
		if code, body := get(t, hc, client.UnixSocketURL+"/", http.Header{auth.ClientIPHeader: {"192.0.2.7"}}); code != http.StatusForbidden {
			t.Errorf("got %d %q; want 403", code, body)
		}
	})
	t.Run("a listener that records no peers", func(t *testing.T) {
		srv := httptest.NewServer(auth.TrustClientIPFrom([]uint32{me}, echoClientIP))
		defer srv.Close()
		if code, body := get(t, srv.Client(), srv.URL, http.Header{auth.ClientIPHeader: {"192.0.2.7"}}); code != http.StatusForbidden {
			t.Errorf("got %d %q; want 403", code, body)
		}
	})
}

func TestClientIPFromConnIgnoresTheHeader(t *testing.T) {
	srv := httptest.NewServer(auth.ClientIPFromConn(echoClientIP))
	defer srv.Close()
	code, body := get(t, srv.Client(), srv.URL, http.Header{auth.ClientIPHeader: {"192.0.2.7"}})
	if code != http.StatusOK || body != "127.0.0.1 header=" {
		t.Errorf("got %d %q; want the connection's address and no header", code, body)
	}
}
