// Package apiserver serves the tower API: every resource kind through the
// generated Connect handlers, backed by one generic implementation over the
// store, plus the background work that keeps the store consistent (garbage
// collection, compaction). Its machinery is hangar's, copied to be extracted
// into a shared module later; see docs/decisions/engine-from-hangar.md.
package apiserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	"github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1/iamv1alpha1connect"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/apiserver/servicegen"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/auth/session"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/secrets"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// Config configures a Server. Zero durations and counts take defaults.
type Config struct {
	Store  store.Store
	Clock  clockwork.Clock
	Logger *slog.Logger
	// BookmarkInterval is how often an idle watch sends a bookmark.
	BookmarkInterval time.Duration
	// EventRetention is how many revisions of history watches can resume
	// from.
	EventRetention int64
	// CompactInterval is how often history beyond the retention is dropped.
	CompactInterval time.Duration
	// SweepInterval is how often garbage collection rechecks every object,
	// catching anything it missed while following deletes.
	SweepInterval time.Duration
	// Secrets seals and opens Secrets' data; nil is no key-encryption key,
	// and writing a Secret fails (docs/decisions/secrets.md). Call
	// PrepareSecrets before serving.
	Secrets *secrets.Keyring
	// Credentials keeps the service tokens' secrets the apiserver issues.
	// Nil issues none, and ServiceTokenIssuerService fails.
	Credentials store.Credentials
}

// Server is the API server. Create it with New, start its background work
// with Run, and serve Handler on each listener.
type Server struct {
	cfg      Config
	store    store.Store
	clock    clockwork.Clock
	log      *slog.Logger
	backends map[string]*backend
	// sessions issues and checks service tokens' secrets, over
	// Config.Credentials; nil without them.
	sessions *session.Manager
	// tokenSecrets serializes issuing each service token's secrets, so its
	// status names the secret that works. A delete does not take it; see
	// issueTokenSecret. Keyed by the token's name.
	tokenSecrets keyLocks
	// tokenUses queues service tokens' uses to record in their status.
	tokenUses chan tokenUse
	// gc is how far garbage collection has come.
	gc gcProgress
}

// New returns a server over cfg.Store.
func New(cfg Config) *Server {
	if cfg.Clock == nil {
		cfg.Clock = clockwork.NewRealClock()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.BookmarkInterval == 0 {
		cfg.BookmarkInterval = 30 * time.Second
	}
	if cfg.EventRetention == 0 {
		cfg.EventRetention = 10000
	}
	if cfg.CompactInterval == 0 {
		cfg.CompactInterval = 10 * time.Minute
	}
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = 5 * time.Minute
	}
	s := &Server{
		cfg:       cfg,
		store:     txStore{cfg.Store},
		clock:     cfg.Clock,
		log:       cfg.Logger,
		backends:  map[string]*backend{},
		tokenUses: make(chan tokenUse, 256),
	}
	if cfg.Credentials != nil {
		s.sessions = s.newSessions(cfg.Credentials)
	}
	for _, r := range registry.All {
		s.backends[r.Kind] = &backend{s: s, r: r, hooks: hooksFor(r)}
	}
	return s
}

// ControllerStatus reports what the garbage collector is doing, as a
// controller's status.
func (s *Server) ControllerStatus() []controller.Status {
	return []controller.Status{s.gc.status()}
}

func (s *Server) backend(r registry.Resource) *backend { return s.backends[r.Kind] }

// Handler serves the API, authenticating every call with chain.
func (s *Server) Handler(chain auth.Chain) http.Handler {
	mux := http.NewServeMux()
	servicegen.Mount(mux, func(r registry.Resource) resource.Backend { return s.backend(r) },
		s.interceptors(chain))
	mux.Handle(iamv1alpha1connect.NewServiceTokenIssuerServiceHandler(tokenIssuer{s}, s.interceptors(chain)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// interceptors authenticate a listener's Connect calls with chain, and
// recheck its streams on the server's clock.
func (s *Server) interceptors(chain auth.Chain) connect.HandlerOption {
	return connect.WithInterceptors(chain.InterceptorOn(s.clock, s.log))
}

// ServiceTokens authenticates service tokens' secrets; ok is false without
// Config.Credentials.
func (s *Server) ServiceTokens() (a auth.Authenticator, ok bool) {
	if s.sessions == nil {
		return nil, false
	}
	return session.Bearer{M: s.sessions}, true
}

// newSessions is the credentials manager over creds.
func (s *Server) newSessions(creds store.Credentials) *session.Manager {
	return session.New(creds, session.Options{Now: s.clock.Now, Log: s.log, ServiceUsed: s.serviceTokenUsed, ServiceLive: s.serviceTokenLive})
}

// Run does the server's background work until ctx is done: it collects
// garbage, compacts history, deletes expired credentials and records service
// tokens' uses.
func (s *Server) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.runGC(gctx) })
	g.Go(func() error { return s.runCompaction(gctx) })
	if s.sessions != nil {
		g.Go(func() error { return s.sessions.Run(gctx) })
		g.Go(func() error { return s.recordTokenUses(gctx) })
	}
	// Background work ends when the caller's ctx does; only a failure before
	// that is an error.
	if err := g.Wait(); ctx.Err() == nil {
		return err
	}
	return nil
}

func (s *Server) system(ctx context.Context) context.Context {
	return auth.WithPrincipal(ctx, auth.SystemPrincipal)
}

func (s *Server) runCompaction(ctx context.Context) error {
	ticker := s.clock.NewTicker(s.cfg.CompactInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.Chan():
		}
		rev, err := s.store.Revision(ctx)
		if err != nil {
			return err
		}
		if rev > s.cfg.EventRetention {
			if err := s.store.Compact(ctx, rev-s.cfg.EventRetention); err != nil && !errors.Is(err, context.Canceled) {
				s.log.Error("compaction failed", "error", err)
			}
		}
	}
}

// get reads an object as stored, or nil if there is none.
func (s *Server) get(ctx context.Context, r registry.Resource, key resource.Key) (proto.Message, error) {
	b := s.backend(r)
	o, err := s.store.Get(ctx, b.storeKey(key))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return b.decode(o)
}
