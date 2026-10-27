package system

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/netutil"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// The server: what boxes reach over the target's EndpointService.
const (
	// ServePort is the port it serves boxes on.
	ServePort = 7480
	// DefaultControl is its control socket, which the calls reach it by.
	DefaultControl = "/run/tower-system/control.sock"
	// ResultHeader carries what a served operation answers, a Result as
	// JSON, as a call's first line does.
	ResultHeader = "Tower-Result"
	// PathPing is the probe a box makes before it presents a credential,
	// naming its network: it answers only a box whose requests come from
	// that network's address. PathPrefix and a grant's operation are the
	// operation's own path.
	PathPing   = "/v1/ping"
	PathPrefix = "/v1/"
	// MaxGrantTTL bounds how long a credential holds.
	MaxGrantTTL = 10 * time.Minute
	// MaxConnections bounds the boxes' connections open at once; one more
	// waits for one to close.
	MaxConnections = 64
	// ReadTimeout bounds reading a request, a push's tar included;
	// IdleTimeout how long a connection waits for its next request.
	ReadTimeout = time.Hour
	IdleTimeout = time.Minute
)

// grant is what a credential lets one request do: the operation, on the
// mirror at the ref, or for a push the repository's URL, branch and lease
// with the token, from the network's address alone, until expires.
type grant struct {
	op, mirror, ref, url, branch, lease, token, network string
	expires                                             time.Time
}

// Server serves boxes the mirrors' tars over HTTP, each request on a
// single-use credential the calls register (OpGrant), and refuses one
// that does not come from the address of the network its credential is
// for, or names a mirror not of that network's project. It keeps no
// secret but its grants', in memory, until each is presented or expires.
type Server struct {
	cfg Config
	log *slog.Logger
	now func() time.Time

	// idle is how long a box's connection waits for its next request.
	idle time.Duration

	mu      sync.Mutex
	grants  map[[sha256.Size]byte]grant
	sources map[string]Source
}

// NewServer returns a server of cfg's mirrors, which logs to log.
func NewServer(cfg Config, log *slog.Logger) *Server {
	return &Server{cfg: cfg, log: log, now: time.Now, idle: IdleTimeout, grants: map[[sha256.Size]byte]grant{}, sources: map[string]Source{}}
}

// Run serves boxes on cfg.Listen (":7480" when empty) and the calls on
// cfg.Control, until ctx ends. listening hears the boxes' address once
// both listen.
func (s *Server) Run(ctx context.Context, listening func(addr string)) error {
	addr := s.cfg.Listen
	if addr == "" {
		addr = ":" + strconv.Itoa(ServePort)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	control := s.cfg.control()
	if err := os.MkdirAll(filepath.Dir(control), 0o700); err != nil {
		_ = ln.Close()
		return err
	}
	_ = os.Remove(control)
	cln, err := net.Listen("unix", control)
	if err != nil {
		_ = ln.Close()
		return err
	}
	// root's alone: a grant carries a repository's token.
	if err := os.Chmod(control, 0o600); err != nil {
		_ = ln.Close()
		_ = cln.Close()
		return err
	}
	boxes := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: ReadTimeout, IdleTimeout: s.idle}
	calls := &http.Server{Handler: s.ControlHandler(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- boxes.Serve(netutil.LimitListener(ln, MaxConnections)) }()
	go func() { errs <- calls.Serve(cln) }()
	if listening != nil {
		listening(ln.Addr().String())
	}
	sweep := time.NewTicker(time.Minute)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = boxes.Close()
			_ = calls.Close()
			return nil
		case err := <-errs:
			_ = boxes.Close()
			_ = calls.Close()
			return err
		case <-sweep.C:
			s.dropExpired()
		}
	}
}

// dropExpired forgets the grants past their expiry.
func (s *Server) dropExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, g := range s.grants {
		if now.After(g.expires) {
			delete(s.grants, k)
		}
	}
}

// Handler serves boxes: the probe, and the granted operations.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathPing, s.ping)
	mux.HandleFunc("GET "+PathPrefix+GrantCheckout, s.checkout)
	mux.HandleFunc("GET "+PathPrefix+GrantOrigin, s.origin)
	mux.HandleFunc("POST "+PathPrefix+GrantPush, s.push)
	return mux
}

// source is where a request comes from.
func source(r *http.Request) (netip.Addr, error) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, err
	}
	return ap.Addr().Unmap(), nil
}

// fromNetwork says why a request from src is not network's, nil if it is.
func (s *Server) fromNetwork(src netip.Addr, network string) (Source, error) {
	s.mu.Lock()
	n, ok := s.sources[network]
	s.mu.Unlock()
	if !ok {
		return Source{}, fmt.Errorf("network %q is not one tower named", network)
	}
	want, err := netip.ParseAddr(n.Address)
	if err != nil || want != src {
		return Source{}, fmt.Errorf("the request comes from %s, not from network %s's address %s", src, network, n.Address)
	}
	return n, nil
}

func (s *Server) ping(w http.ResponseWriter, r *http.Request) {
	src, err := source(r)
	if err == nil {
		_, err = s.fromNetwork(src, r.URL.Query().Get("network"))
	}
	if err != nil {
		// Who asks may be anyone the network lets in: it is told nothing
		// of the networks.
		http.Error(w, "refused", http.StatusForbidden)
		return
	}
	_, _ = io.WriteString(w, "tower-system\n")
}

// errRefused is a request its credential does not let do what it asks.
var errRefused = errors.New("refused")

// take takes the request's credential, which no later request can
// present, and returns its grant if it lets the request do op: from its
// network's address, on a mirror of that network's project, before it
// expires.
func (s *Server) take(r *http.Request, op string) (grant, error) {
	cred, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || cred == "" {
		return grant{}, fmt.Errorf("no credential: %w", errRefused)
	}
	key := sha256.Sum256([]byte(cred))
	s.mu.Lock()
	g, ok := s.grants[key]
	// Single use: a credential presented once is gone, whatever comes of
	// the request.
	delete(s.grants, key)
	s.mu.Unlock()
	if !ok {
		return grant{}, fmt.Errorf("the credential is not one, or was used: %w", errRefused)
	}
	if s.now().After(g.expires) {
		return grant{}, fmt.Errorf("the credential expired: %w", errRefused)
	}
	if g.op != op {
		return grant{}, fmt.Errorf("the credential is for a %s, not a %s: %w", g.op, op, errRefused)
	}
	src, err := source(r)
	if err != nil {
		return grant{}, fmt.Errorf("%w: %w", err, errRefused)
	}
	n, err := s.fromNetwork(src, g.network)
	if err != nil {
		return grant{}, fmt.Errorf("%w: %w", err, errRefused)
	}
	if !strings.HasPrefix(g.mirror, n.Project+"/") {
		return grant{}, fmt.Errorf("mirror %s is not project %s's: %w", g.mirror, n.Project, errRefused)
	}
	return g, nil
}

// fail answers a request that failed, with its message, the token taken
// out should git have quoted it.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, g grant, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, errRefused):
		code = http.StatusForbidden
	case errors.Is(err, driver.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, driver.ErrLeaseLost):
		code = http.StatusConflict
	case errors.Is(err, driver.ErrNoSpace):
		code = http.StatusInsufficientStorage
	}
	msg := err.Error()
	if g.token != "" {
		msg = strings.ReplaceAll(msg, g.token, "[token]")
	}
	s.log.Info("refused or failed", "path", r.URL.Path, "network", g.network, "mirror", g.mirror, "status", code, "error", msg)
	http.Error(w, msg, code)
}

// answer writes res as the result header.
func answer(w http.ResponseWriter, res Result) {
	b, _ := json.Marshal(res)
	w.Header().Set(ResultHeader, string(b))
}

// sendTar answers a tar a scratch file holds, its length said, so a box
// tells one cut short.
func (s *Server) sendTar(w http.ResponseWriter, r *http.Request, g grant, res Result, f *os.File) {
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		s.fail(w, r, g, err)
		return
	}
	answer(w, res)
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	n, err := io.Copy(w, f)
	s.log.Info("served", "path", r.URL.Path, "network", g.network, "mirror", g.mirror, "bytes", n, "error", err)
}

func (s *Server) checkout(w http.ResponseWriter, r *http.Request) {
	g, err := s.take(r, GrantCheckout)
	if err != nil {
		s.fail(w, r, g, err)
		return
	}
	commit, f, err := checkoutFile(r.Context(), s.cfg, g.mirror, g.ref)
	if err != nil {
		s.fail(w, r, g, err)
		return
	}
	s.sendTar(w, r, g, Result{Commit: commit}, f)
}

func (s *Server) origin(w http.ResponseWriter, r *http.Request) {
	g, err := s.take(r, GrantOrigin)
	if err != nil {
		s.fail(w, r, g, err)
		return
	}
	f, err := originFile(r.Context(), s.cfg, g.mirror)
	if err != nil {
		s.fail(w, r, g, err)
		return
	}
	s.sendTar(w, r, g, Result{}, f)
}

func (s *Server) push(w http.ResponseWriter, r *http.Request) {
	g, err := s.take(r, GrantPush)
	if err != nil {
		s.fail(w, r, g, err)
		return
	}
	git, err := mirrors(r.Context(), s.cfg)
	if err == nil {
		err = git.Push(r.Context(), driver.PushSpec{Mirror: g.mirror, URL: g.url, Token: g.token, Branch: g.branch, Lease: g.lease}, r.Body)
	}
	if err != nil {
		s.fail(w, r, g, err)
		return
	}
	s.log.Info("pushed", "network", g.network, "mirror", g.mirror, "branch", g.branch)
	answer(w, Result{})
	w.WriteHeader(http.StatusOK)
}

// controlGrant is a grant as a call registers it.
type controlGrant struct {
	Credential string   `json:"credential"`
	Op         string   `json:"op"`
	Mirror     string   `json:"mirror"`
	Ref        string   `json:"ref,omitempty"`
	URL        string   `json:"url,omitempty"`
	Branch     string   `json:"branch,omitempty"`
	Lease      string   `json:"lease,omitempty"`
	Token      string   `json:"token,omitempty"`
	Network    string   `json:"network"`
	TTL        int      `json:"ttl"`
	Sources    []Source `json:"sources,omitempty"`
}

// The control socket's paths.
const (
	controlGrantPath   = "/grant"
	controlSourcesPath = "/sources"
	controlStatusPath  = "/status"
)

// ControlHandler serves the calls: a grant registered, the networks'
// sources given, and whether the server answers.
func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+controlGrantPath, func(w http.ResponseWriter, r *http.Request) {
		var c controlGrant
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.grant(c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT "+controlSourcesPath, func(w http.ResponseWriter, r *http.Request) {
		var srcs []Source
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&srcs); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m := map[string]Source{}
		for _, src := range srcs {
			if err := validSource(src); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			m[src.Network] = src
		}
		s.mu.Lock()
		// A network a live grant is for keeps its source: a grant made
		// as the sources were read for this call would be refused.
		now := s.now()
		for _, g := range s.grants {
			if _, ok := m[g.network]; !ok && !now.After(g.expires) {
				if src, ok := s.sources[g.network]; ok {
					m[g.network] = src
				}
			}
		}
		s.sources = m
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE "+controlGrantPath, func(w http.ResponseWriter, r *http.Request) {
		var c controlGrant
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&c); err != nil || c.Credential == "" {
			http.Error(w, "a revocation names its credential", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		delete(s.grants, sha256.Sum256([]byte(c.Credential)))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET "+controlStatusPath, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func validSource(src Source) error {
	if src.Network == "" || src.Project == "" {
		return errors.New("a source needs its network and project")
	}
	if _, err := netip.ParseAddr(src.Address); err != nil {
		return fmt.Errorf("network %s's address: %w", src.Network, err)
	}
	return nil
}

// grant registers c, and the sources it carries.
func (s *Server) grant(c controlGrant) error {
	switch c.Op {
	case GrantCheckout, GrantOrigin, GrantPush:
	default:
		return fmt.Errorf("no operation %q to grant", c.Op)
	}
	if len(c.Credential) < 32 || c.Mirror == "" || c.Network == "" {
		return errors.New("a grant needs a credential of 32 characters or more, a mirror and a network")
	}
	ttl := time.Duration(c.TTL) * time.Second
	if ttl <= 0 || ttl > MaxGrantTTL {
		return fmt.Errorf("a grant holds for 1s to %s, not %s", MaxGrantTTL, ttl)
	}
	for _, src := range c.Sources {
		if err := validSource(src); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, src := range c.Sources {
		s.sources[src.Network] = src
	}
	s.grants[sha256.Sum256([]byte(c.Credential))] = grant{
		op: c.Op, mirror: c.Mirror, ref: c.Ref, url: c.URL, branch: c.Branch, lease: c.Lease, token: c.Token,
		network: c.Network, expires: s.now().Add(ttl),
	}
	return nil
}

// controlClient is an HTTP client of the control socket.
func controlClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

// control sends a call's request to the server.
func control(ctx context.Context, cfg Config, method, path string, body any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://tower-system"+path, r)
	if err != nil {
		return err
	}
	c := controlClient(cfg.control())
	defer c.CloseIdleConnections()
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("the server: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("the server: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
