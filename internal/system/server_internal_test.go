package system

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitRemote is a bare repository with a commit on main.
func gitRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bare, work := filepath.Join(dir, "remote.git"), filepath.Join(dir, "work")
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", "--initial-branch=main", bare},
		{"init", "--quiet", "--initial-branch=main", work},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "--quiet", "-m", "one"}, {"push", "--quiet", bare, "main"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return bare
}

// served is a server over a mirror web/api and another project's,
// other/api, of one remote, which knows two networks: net-a of project
// web, from 100.64.0.1, and net-b of project other, from 100.64.0.3.
func served(t *testing.T) *Server {
	t.Helper()
	cfg := Config{Root: t.TempDir()}
	remote := gitRemote(t)
	for _, m := range []string{"web/api", "other/api"} {
		if code := Serve(t.Context(), cfg, framed(t, Header{Op: OpFetch, Mirror: m, URL: remote, Branch: "main"}), io.Discard, os.Stderr); code != 0 {
			t.Fatalf("fetching %s: exit %d", m, code)
		}
	}
	s := NewServer(cfg, slog.New(slog.DiscardHandler))
	if err := s.grant(controlGrant{Credential: strings.Repeat("s", 32), Op: GrantOrigin, Mirror: "web/api", Network: "net-a", TTL: 60, Sources: []Source{
		{Network: "net-a", Project: "web", Address: "100.64.0.1"}, {Network: "net-b", Project: "other", Address: "100.64.0.3"},
	}}); err != nil {
		t.Fatal(err)
	}
	return s
}

// framed is a call's stdin: its header and the end of its data.
func framed(t *testing.T, h Header) io.Reader {
	t.Helper()
	var b bytes.Buffer
	if err := writeHeader(&b, h); err != nil {
		t.Fatal(err)
	}
	b.Write([]byte{'e', 0, 0, 0, 0})
	return &b
}

// ask makes a request of the server from src with credential cred.
func ask(s *Server, method, path, src, cred string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = src + ":40000"
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func grantOf(t *testing.T, s *Server, cred, op, mirror, network string) {
	t.Helper()
	if err := s.grant(controlGrant{Credential: cred, Op: op, Mirror: mirror, Network: network, TTL: 60}); err != nil {
		t.Fatal(err)
	}
}

// entries are a tar's entries by name: each one's mode and content, but
// what two clones of a mirror differ in, the times git writes in .git's
// index and logs.
func entries(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		c, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == ".git/index" || strings.HasPrefix(h.Name, ".git/logs/") {
			c = nil
		}
		out[h.Name] = fmt.Sprintf("%o %x", h.Mode, c)
	}
}

// TestACredentialIsUsedOnce: a checkout on its credential answers the
// tar the call answers, as the same code makes it, with its commit; the
// credential presented again is refused.
func TestACredentialIsUsedOnce(t *testing.T) {
	s := served(t)
	cred := strings.Repeat("a", 43)
	grantOf(t, s, cred, GrantCheckout, "web/api", "net-a")
	rec := ask(s, http.MethodGet, PathPrefix+GrantCheckout, "100.64.0.1", cred)
	if rec.Code != http.StatusOK {
		t.Fatalf("a checkout on its credential: %d %s", rec.Code, rec.Body.String())
	}
	var call bytes.Buffer
	if code := Serve(t.Context(), s.cfg, framed(t, Header{Op: OpCheckout, Mirror: "web/api"}), &call, os.Stderr); code != 0 {
		t.Fatalf("the call's checkout: exit %d", code)
	}
	line, tarball, _ := bytes.Cut(call.Bytes(), []byte("\n"))
	if got, want := entries(t, rec.Body.Bytes()), entries(t, tarball); len(want) < 5 || !maps.Equal(got, want) {
		t.Errorf("the served tar holds %v, the call's %v", got, want)
	}
	if got := rec.Header().Get(ResultHeader); got != string(line) {
		t.Errorf("the served result %q, the call's %q", got, line)
	}
	if again := ask(s, http.MethodGet, PathPrefix+GrantCheckout, "100.64.0.1", cred); again.Code != http.StatusForbidden {
		t.Errorf("the credential presented again: %d %s, want 403", again.Code, again.Body.String())
	}
}

// TestACredentialIsRefusedButForItsOwn: a credential is refused for
// another operation, from another network's address, or for a mirror not
// of its network's project; refused, it is gone too.
func TestACredentialIsRefusedButForItsOwn(t *testing.T) {
	s := served(t)
	cases := []struct {
		name, op, mirror, network, path, src, method string
	}{
		{"another operation", GrantOrigin, "web/api", "net-a", PathPrefix + GrantCheckout, "100.64.0.1", http.MethodGet},
		{"a push for an origin", GrantOrigin, "web/api", "net-a", PathPrefix + GrantPush, "100.64.0.1", http.MethodPost},
		{"another network's address", GrantCheckout, "web/api", "net-a", PathPrefix + GrantCheckout, "100.64.0.3", http.MethodGet},
		{"the other address of its transit", GrantCheckout, "web/api", "net-a", PathPrefix + GrantCheckout, "100.64.0.0", http.MethodGet},
		{"another project's mirror", GrantCheckout, "other/api", "net-a", PathPrefix + GrantCheckout, "100.64.0.1", http.MethodGet},
		{"another project's mirror's origin", GrantOrigin, "other/api", "net-a", PathPrefix + GrantOrigin, "100.64.0.1", http.MethodGet},
		{"a push to another project's mirror", GrantPush, "other/api", "net-a", PathPrefix + GrantPush, "100.64.0.1", http.MethodPost},
		{"a network tower never named", GrantCheckout, "web/api", "net-c", PathPrefix + GrantCheckout, "100.64.0.1", http.MethodGet},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cred := strings.Repeat(string(rune('b'+i)), 40)
			grantOf(t, s, cred, c.op, c.mirror, c.network)
			if rec := ask(s, c.method, c.path, c.src, cred); rec.Code != http.StatusForbidden {
				t.Errorf("%d %s, want 403", rec.Code, rec.Body.String())
			}
			// Gone once presented, even refused: its own request now fails.
			own := map[string]string{GrantCheckout: http.MethodGet, GrantOrigin: http.MethodGet, GrantPush: http.MethodPost}[c.op]
			if rec := ask(s, own, PathPrefix+c.op, "100.64.0.1", cred); rec.Code != http.StatusForbidden {
				t.Errorf("presented again as its own: %d %s, want 403", rec.Code, rec.Body.String())
			}
		})
	}
	t.Run("no credential", func(t *testing.T) {
		if rec := ask(s, http.MethodGet, PathPrefix+GrantCheckout, "100.64.0.1", ""); rec.Code != http.StatusForbidden {
			t.Errorf("%d, want 403", rec.Code)
		}
	})
	t.Run("an expired one", func(t *testing.T) {
		cred := strings.Repeat("z", 40)
		grantOf(t, s, cred, GrantCheckout, "web/api", "net-a")
		s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
		defer func() { s.now = time.Now }()
		if rec := ask(s, http.MethodGet, PathPrefix+GrantCheckout, "100.64.0.1", cred); rec.Code != http.StatusForbidden {
			t.Errorf("%d %s, want 403", rec.Code, rec.Body.String())
		}
	})
	t.Run("one of another network's project, from that network", func(t *testing.T) {
		// net-b's own credential for a mirror of web, net-a's project.
		cred := strings.Repeat("y", 40)
		grantOf(t, s, cred, GrantCheckout, "web/api", "net-b")
		if rec := ask(s, http.MethodGet, PathPrefix+GrantCheckout, "100.64.0.3", cred); rec.Code != http.StatusForbidden {
			t.Errorf("%d %s, want 403", rec.Code, rec.Body.String())
		}
	})
}

// TestTheProbeAnswersOnlyItsNetwork: the probe answers a request from
// the network it names, and no other.
func TestTheProbeAnswersOnlyItsNetwork(t *testing.T) {
	s := served(t)
	if rec := ask(s, http.MethodGet, PathPing+"?network=net-a", "100.64.0.1", ""); rec.Code != http.StatusOK {
		t.Errorf("from its network: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range []struct{ network, src string }{{"net-a", "100.64.0.3"}, {"net-c", "100.64.0.1"}, {"", "100.64.0.1"}} {
		if rec := ask(s, http.MethodGet, PathPing+"?network="+c.network, c.src, ""); rec.Code != http.StatusForbidden {
			t.Errorf("network %q from %s: %d, want 403", c.network, c.src, rec.Code)
		}
	}
}

// TestAGrantIsRefusedThatSaysTooLittle: a grant needs a long credential,
// an operation, a mirror, a network and a bounded life.
func TestAGrantIsRefusedThatSaysTooLittle(t *testing.T) {
	s := served(t)
	good := controlGrant{Credential: strings.Repeat("k", 32), Op: GrantCheckout, Mirror: "web/api", Network: "net-a", TTL: 60}
	for name, mut := range map[string]func(*controlGrant){
		"a short credential": func(g *controlGrant) { g.Credential = "short" },
		"no operation":       func(g *controlGrant) { g.Op = "" },
		"a build":            func(g *controlGrant) { g.Op = OpBuild },
		"no mirror":          func(g *controlGrant) { g.Mirror = "" },
		"no network":         func(g *controlGrant) { g.Network = "" },
		"no life":            func(g *controlGrant) { g.TTL = 0 },
		"too long a life":    func(g *controlGrant) { g.TTL = int(MaxGrantTTL/time.Second) + 1 },
		"a bad source":       func(g *controlGrant) { g.Sources = []Source{{Network: "net-a", Project: "web", Address: "nowhere"}} },
	} {
		g := good
		mut(&g)
		if err := s.grant(g); err == nil {
			t.Errorf("%s: granted", name)
		}
	}
}

// controlRequest makes a request of the server's control handler, as a call
// does.
func controlRequest(t *testing.T, s *Server, method, path string, body any) int {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.ControlHandler().ServeHTTP(rec, httptest.NewRequest(method, path, bytes.NewReader(b)))
	return rec.Code
}

// TestARevokedCredentialIsRefused: a credential revoked before it is
// presented, as one whose transfer went another way, is refused.
func TestARevokedCredentialIsRefused(t *testing.T) {
	s := served(t)
	cred := strings.Repeat("r", 40)
	grantOf(t, s, cred, GrantCheckout, "web/api", "net-a")
	if code := controlRequest(t, s, http.MethodDelete, controlGrantPath, controlGrant{Credential: cred}); code != http.StatusNoContent {
		t.Fatalf("revoking: %d", code)
	}
	if rec := ask(s, http.MethodGet, PathPrefix+GrantCheckout, "100.64.0.1", cred); rec.Code != http.StatusForbidden {
		t.Errorf("a revoked credential: %d, want 403", rec.Code)
	}
}

// TestTheSourcesKeepWhatALiveGrantIsFor: the networks tower gives the
// server replace those it had, but for a network a live grant is for, as
// one made just before the sources were read: its credential still works
// from its address.
func TestTheSourcesKeepWhatALiveGrantIsFor(t *testing.T) {
	s := served(t)
	cred := strings.Repeat("l", 40)
	if err := s.grant(controlGrant{Credential: cred, Op: GrantCheckout, Mirror: "web/api", Network: "net-new", TTL: 60,
		Sources: []Source{{Network: "net-new", Project: "web", Address: "100.64.0.5"}}}); err != nil {
		t.Fatal(err)
	}
	if code := controlRequest(t, s, http.MethodPut, controlSourcesPath, []Source{{Network: "net-a", Project: "web", Address: "100.64.0.1"}}); code != http.StatusNoContent {
		t.Fatalf("giving the sources: %d", code)
	}
	if rec := ask(s, http.MethodGet, PathPrefix+GrantCheckout, "100.64.0.5", cred); rec.Code != http.StatusOK {
		t.Errorf("the live grant's credential after the sources: %d %s", rec.Code, rec.Body.String())
	}
	if rec := ask(s, http.MethodGet, PathPing+"?network=net-b", "100.64.0.3", ""); rec.Code != http.StatusForbidden {
		t.Errorf("a network gone from the sources, no grant for it: %d, want 403", rec.Code)
	}
}

// TestAnIdleConnectionIsClosed: the server closes a box's connection
// that waits past its idle time for its next request.
func TestAnIdleConnectionIsClosed(t *testing.T) {
	s := NewServer(Config{Root: t.TempDir(), Listen: "127.0.0.1:0", Control: filepath.Join(t.TempDir(), "c.sock")}, slog.New(slog.DiscardHandler))
	s.idle = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	addr := make(chan string, 1)
	go func() { _ = s.Run(ctx, func(a string) { addr <- a }) }()
	conn, err := net.Dial("tcp", <-addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "GET "+PathPing+" HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadByte(); !errors.Is(err, io.EOF) {
		t.Errorf("an idle connection, read after its idle time: %v, want it closed", err)
	}
}
