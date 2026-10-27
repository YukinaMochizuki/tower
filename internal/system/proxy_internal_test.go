package system

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// inProcess runs tower-system's calls in the test's process.
type inProcess struct{ cfg Config }

func (l inProcess) RunSystem(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return Serve(ctx, l.cfg, stdin, stdout, stderr), nil
}

// gitInvocation is a git command as a wrapper on PATH saw it: its
// arguments and its environment.
type gitInvocation struct {
	args []string
	env  []string
}

// recordGit puts a git on PATH that logs each command's arguments and
// environment before it runs the real git, and returns what it logged so
// far.
func recordGit(t *testing.T) func() []gitInvocation {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := "#!/bin/sh\n{ printf 'args'; printf ' %s' \"$@\"; echo; env; echo '--end--'; } >>" + log + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []gitInvocation {
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		var out []gitInvocation
		var cur gitInvocation
		for _, line := range strings.Split(string(b), "\n") {
			switch {
			case strings.HasPrefix(line, "args"):
				cur = gitInvocation{args: strings.Fields(strings.TrimPrefix(line, "args"))}
			case line == "--end--":
				out = append(out, cur)
			case line != "":
				cur.env = append(cur.env, line)
			}
		}
		return out
	}
}

// proxyEnv is what of env names a proxy.
func proxyEnv(env []string) []string {
	var out []string
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if strings.HasSuffix(strings.ToLower(k), "_proxy") {
			out = append(out, e)
		}
	}
	slices.Sort(out)
	return out
}

// TestGitReachesRepositoriesThroughTheProxy: with a proxy configured, the
// git commands that reach a repository, a fetch's ls-remote and fetch and
// a push, in a call and in the server alike, carry it, loopback excepted;
// with none, no command carries any.
func TestGitReachesRepositoriesThroughTheProxy(t *testing.T) {
	logged := recordGit(t)
	remote := gitRemote(t)
	const proxy = "http://127.0.0.1:1055"
	want := []string{"HTTPS_PROXY=" + proxy, "NO_PROXY=127.0.0.1,localhost,::1", "http_proxy=" + proxy, "https_proxy=" + proxy, "no_proxy=127.0.0.1,localhost,::1"}
	for _, p := range []string{proxy, ""} {
		before := len(logged())
		cfg := Config{Root: t.TempDir(), GitProxy: p}
		o := Ops{R: inProcess{cfg}}
		if _, err := o.Fetch(t.Context(), driver.FetchSpec{Mirror: "web/api", URL: remote, Token: "tok-1"}); err != nil {
			t.Fatalf("proxy %q: the fetch: %v", p, err)
		}
		var co bytes.Buffer
		if _, err := o.Checkout(t.Context(), "web/api", "", &co); err != nil {
			t.Fatalf("proxy %q: the checkout: %v", p, err)
		}
		if err := o.Push(t.Context(), driver.PushSpec{Mirror: "web/api", URL: remote, Branch: "from-call", Token: "tok-1"}, bytes.NewReader(co.Bytes())); err != nil {
			t.Fatalf("proxy %q: the call's push: %v", p, err)
		}
		s := NewServer(cfg, slog.New(slog.DiscardHandler))
		cred := strings.Repeat("p", 40)
		if err := s.grant(controlGrant{Credential: cred, Op: GrantPush, Mirror: "web/api", URL: remote, Branch: "from-serve", Token: "tok-1", Network: "net-a", TTL: 60,
			Sources: []Source{{Network: "net-a", Project: "web", Address: "100.64.0.1"}}}); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, PathPrefix+GrantPush, bytes.NewReader(co.Bytes()))
		req.RemoteAddr = "100.64.0.1:40000"
		req.Header.Set("Authorization", "Bearer "+cred)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("proxy %q: the server's push: %d %s", p, rec.Code, rec.Body.String())
		}

		reaching := map[string]int{}
		for _, inv := range logged()[before:] {
			op := ""
			switch {
			case slices.Contains(inv.args, "ls-remote"):
				op = "ls-remote"
			case slices.Contains(inv.args, "push"):
				op = "push"
			case slices.Contains(inv.args, "fetch") && slices.Contains(inv.args, remote):
				op = "fetch"
			}
			got := proxyEnv(inv.env)
			switch {
			case p == "" && len(got) > 0:
				t.Errorf("with no proxy, git %v carries %q", inv.args, got)
			case p != "" && op != "" && !slices.Equal(got, want):
				t.Errorf("with proxy %q, git %s carries %q, want %q", p, op, got, want)
			}
			if op != "" {
				reaching[op]++
			}
		}
		if reaching["ls-remote"] != 1 || reaching["fetch"] != 1 || reaching["push"] != 2 {
			t.Errorf("proxy %q: the commands reaching the repository were %v", p, reaching)
		}
	}
}

// connect is a CONNECT request as an HTTP proxy saw it: its target, its
// head as sent, and the first byte through the tunnel.
type connect struct {
	method, target string
	head           []byte
	first          byte
}

// connectProxy is an HTTP proxy on loopback that answers a CONNECT, takes
// the tunnel's first byte, and closes it; it sends each request it saw.
func connectProxy(t *testing.T) (string, <-chan connect) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	seen := make(chan connect, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				var head bytes.Buffer
				br := bufio.NewReader(io.TeeReader(c, &head))
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				got := connect{method: req.Method, target: req.Host, head: bytes.Clone(head.Bytes())}
				if req.Method == http.MethodConnect {
					if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err == nil {
						got.first, _ = br.ReadByte()
					}
				}
				seen <- got
			}()
		}
	}()
	return "http://" + ln.Addr().String(), seen
}

// TestAnHTTPSRemoteGoesThroughTheProxy: a fetch of an https repository of
// a name nothing but the proxy resolves, as a tailnet's are, reaches it
// through the proxy's CONNECT, TLS inside it; the proxy never sees the
// token.
func TestAnHTTPSRemoteGoesThroughTheProxy(t *testing.T) {
	proxy, seen := connectProxy(t)
	o := Ops{R: inProcess{Config{Root: t.TempDir(), GitProxy: proxy}}}
	const token = "secret-token-9"
	_, err := o.Fetch(t.Context(), driver.FetchSpec{Mirror: "web/api", URL: "https://repo.tailnet.invalid:9443/web/api.git", Token: token})
	if err == nil {
		t.Fatal("a fetch through a proxy that closes the tunnel succeeded")
	}
	if strings.Contains(err.Error(), "Could not resolve host") {
		t.Fatalf("git resolved the host itself: %v", err)
	}
	select {
	case c := <-seen:
		if c.method != http.MethodConnect || c.target != "repo.tailnet.invalid:9443" {
			t.Errorf("the proxy saw %s %s, want CONNECT repo.tailnet.invalid:9443", c.method, c.target)
		}
		if c.first != 0x16 {
			t.Errorf("the tunnel began with %#x, not a TLS handshake", c.first)
		}
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		if bytes.Contains(c.head, []byte(token)) || bytes.Contains(c.head, []byte(basic)) {
			t.Errorf("the proxy saw the token: %q", c.head)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the proxy saw nothing; the fetch: %v", err)
	}
}
