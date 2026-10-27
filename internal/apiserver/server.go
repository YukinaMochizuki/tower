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
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	"github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1/environmentsv1alpha1connect"
	"github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1/iamv1alpha1connect"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1/targetsv1alpha1connect"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/apiserver/servicegen"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/auth/session"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/devcontainer"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hostd"
	"github.com/YukinaMochizuki/tower/internal/secrets"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/ticket"
	"github.com/YukinaMochizuki/tower/internal/tunnel"
)

// Config configures a Server. Zero durations and counts take defaults.
type Config struct {
	Store  store.Store
	Clock  clockwork.Clock
	Logger *slog.Logger
	// BookmarkInterval is how often an idle watch sends a bookmark, on
	// BookmarkClock; nil is Clock. A test that moves Clock by hand runs
	// bookmarks on real time, so a client following the API, such as a
	// provider, comes up to date by itself.
	BookmarkInterval time.Duration
	BookmarkClock    clockwork.Clock
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
	// Providers returns the provider of a target, by name. An error leaves
	// the target's environments waiting. Nil is tower's own: a hostd
	// target's is its host over the tunnel it opens; a hangar target's one
	// tower runs (hangar.go).
	Providers func(target string) (provider.Provider, error)
	// TunnelLiveness is the hostd tunnels'; zero is
	// tunnel.DefaultLiveness.
	TunnelLiveness tunnel.Liveness
	// HostdCalls caps the calls open to hostd hosts; zero is hostd's
	// defaults.
	HostdCalls struct{ PerInstance, PerTarget int }
	// Tickets signs the tickets of direct streams; nil issues none, and
	// IssueTicket fails.
	Tickets *ticket.Signer
	// TicketTTL is how long a ticket lasts; zero is DefaultTicketTTL.
	TicketTTL time.Duration
	// Resolver says what boxes' images and warm checkouts are made of; nil
	// is devcontainer.Real, which reads a repository's devcontainer as
	// workstream does. Tests give devcontainer.Fake.
	Resolver devcontainer.Resolver
	// DefaultBoxImage is what a box of a repository without a devcontainer
	// is made of when it names no fallback_image; empty fails such a box.
	DefaultBoxImage string
	// GoldenPruneAfter is how long a golden nothing was made from, and
	// nothing references, is kept; zero is DefaultGoldenPruneAfter.
	GoldenPruneAfter time.Duration
	// DisableControllers leaves the policy controllers out, as tests of the
	// generic machinery do.
	DisableControllers bool
	// HangarAPI makes a hangar target's API from its endpoint, project and
	// token; nil is hangar's own (hangar.NewConnect). Tests give a fake.
	HangarAPI func(endpoint, project, token string) hangar.API
	// TowerGuest is tower-guest, statically linked, which a hangar target's
	// provider writes into its machines; empty runs nothing in them.
	TowerGuest []byte
	// SystemImage is the image of a hangar target's system container, by
	// digest (internal/system); empty makes none, and a hangar target
	// keeps no mirror and builds no image.
	SystemImage string
	// Registry is tower's registry, host[:port], as a hangar target's
	// system container pushes to it, over the tailnet.
	Registry string
	// RegistryPlainHTTP says Registry is served over plain HTTP, as one on
	// the tailnet alone may be.
	RegistryPlainHTTP bool
	// PublicURL is where clients reach this apiserver, such as
	// https://tower.example.ts.net:8080: the direct streams to hangar
	// targets' instances are served there. Empty issues no ticket for them.
	PublicURL string
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
	// admitting serializes the admission and the store write of creates
	// that kindHooks.serialize keys, as boxes' and machines' of one
	// workstream, whose aliases are checked against each other's.
	admitting keyLocks
	// tokenUses queues service tokens' uses to record in their status.
	tokenUses chan tokenUse
	// gc is how far garbage collection has come, and ownerIdx the index of
	// owner references it keeps.
	gc       gcProgress
	ownerIdx ownerIndex
	// The policy controllers, and the slow work they start.
	controllers                                   []*controller.Controller
	placementCtrl, repositoryCtrl, boxCtrl        *controller.Controller
	machineCtrl, goldenCtrl, warmCtrl, targetCtrl *controller.Controller
	jobs                                          jobs
	// jobErrs are the jobs' last failures by target, for last_error.
	jobErrs jobErrors
	// teardowns are the last teardown passes of Targets being deleted.
	teardowns teardowns
	// uses are boxes' uses through Touch, not yet written to them, and the
	// calls into them under way.
	uses lastUses
	// beforeIdleStop, if set, runs between a box's idle stop being decided
	// and its being written, for tests.
	beforeIdleStop atomic.Pointer[func(controller.Key)]
	// runCtx is Run's, which jobs run under.
	runCtx context.Context
	// tunnels are the tunnels hostd hosts opened, one per target, and
	// reach what Reachable says of each.
	tunnels    *tunnel.Hub
	reach      reachability
	hostdCalls *hostd.Calls
	// hangar are the hangar targets' providers tower runs, by hangarCtrl;
	// inproc the client they act on the API with.
	hangar     hangarTargets
	hangarCtrl *controller.Controller
	inproc     inprocClient
}

// New returns a server over cfg.Store.
func New(cfg Config) *Server {
	if cfg.Clock == nil {
		cfg.Clock = clockwork.NewRealClock()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.BookmarkClock == nil {
		cfg.BookmarkClock = cfg.Clock
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
	if cfg.Resolver == nil {
		cfg.Resolver = devcontainer.Real{}
	}
	if cfg.GoldenPruneAfter == 0 {
		cfg.GoldenPruneAfter = DefaultGoldenPruneAfter
	}
	if cfg.TicketTTL == 0 {
		cfg.TicketTTL = DefaultTicketTTL
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
	s.tunnels = &tunnel.Hub{Liveness: cfg.TunnelLiveness, Log: cfg.Logger, Changed: s.tunnelChanged}
	s.hostdCalls = &hostd.Calls{MaxPerInstance: cfg.HostdCalls.PerInstance, MaxPerTarget: cfg.HostdCalls.PerTarget}
	for _, r := range registry.All {
		s.backends[r.Kind] = &backend{s: s, r: r, hooks: hooksFor(r)}
	}
	if !cfg.DisableControllers {
		s.controllers = s.newControllers()
		if cfg.Providers == nil {
			s.hangarCtrl = s.newHangarController()
			s.controllers = append(s.controllers, s.hangarCtrl)
		}
	}
	return s
}

// ControllerStatus reports what the garbage collector, the controllers and
// their jobs are doing.
func (s *Server) ControllerStatus() []controller.Status {
	st := []controller.Status{s.gc.status(), s.jobs.status()}
	for _, c := range s.controllers {
		st = append(st, c.Status())
	}
	return append(st, s.hangarStatus()...)
}

func (s *Server) backend(r registry.Resource) *backend { return s.backends[r.Kind] }

// Handler serves the API, authenticating every call with chain. A stream
// a recheck refuses stops writing within auth.EndGrace, whether or not its
// client reads.
func (s *Server) Handler(chain auth.Chain) http.Handler {
	mux := http.NewServeMux()
	servicegen.Mount(mux, func(r registry.Resource) resource.Backend { return s.backend(r) },
		s.interceptors(chain))
	mux.Handle(iamv1alpha1connect.NewServiceTokenIssuerServiceHandler(tokenIssuer{s}, s.interceptors(chain)))
	mux.Handle(environmentsv1alpha1connect.NewRetryServiceHandler(retryService{s}, s.interceptors(chain)))
	mux.Handle(targetsv1alpha1connect.NewTargetActionServiceHandler(targetActionService{s}, s.interceptors(chain)))
	mux.Handle(environmentsv1alpha1connect.NewEnvironmentServiceHandler(environmentService{s}, s.interceptors(chain),
		connect.WithReadMaxBytes(maxEnvironmentMessage)))
	mux.HandleFunc(tunnel.Path, s.openTunnel(chain))
	// A stream's ticket is its credential, which its provider checks.
	mux.HandleFunc(HangarStreamPath, s.serveHangarStream)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return auth.CutOff(mux)
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

// Run does the server's background work until ctx is done: it runs the
// policy controllers, collects garbage, compacts history, deletes expired
// credentials and records service tokens' and boxes' uses.
func (s *Server) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
	s.runCtx = gctx
	g.Go(func() error {
		<-gctx.Done()
		s.tunnels.Close()
		return nil
	})
	g.Go(func() error { return s.runGC(gctx) })
	for _, c := range s.controllers {
		g.Go(func() error { return c.Run(gctx) })
	}
	if len(s.controllers) > 0 {
		g.Go(func() error { return s.runLastUse(gctx) })
	}
	defer s.jobs.wg.Wait()
	defer s.hangar.wg.Wait()
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
