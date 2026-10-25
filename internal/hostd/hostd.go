// Package hostd makes a hostd target's Instances real on its host and runs
// what tower asks there, deciding nothing (docs/decisions/layers.md). It
// follows hangar's node agent (hangar: docs/decisions/node-agent.md): it
// keeps no state of its own, but follows its target's Instances and
// Goldens through the API with its ROLE_HOSTD token, converges the host to
// them through its drivers, reads the host back each time and reports
// what it found; a sweep removes what nothing of its target accounts for.
// Host is also the target's provider.Provider, which cmd/hostd serves
// tower on the tunnel it opens, and serves direct streams on tickets
// tower signs.
package hostd

import (
	"cmp"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"golang.org/x/sync/errgroup"

	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// Version is hostd's, as it reports it.
var Version = "dev"

// Defaults of a Config.
const (
	DefaultReportEvery = 30 * time.Second
	DefaultSweepEvery  = time.Minute
	// DefaultSweepGrace is how old what nothing accounts for must be
	// before the sweep removes it: a golden captured whose Golden does not
	// name it yet, an instance's disk made as its Instance was listed.
	DefaultSweepGrace = 10 * time.Minute
)

// Config is a host's.
type Config struct {
	// Target is the Target the host provides, its token's.
	Target  string
	Drivers driver.Drivers
	// Client acts as the target's ROLE_HOSTD token.
	Client *client.Client
	// TicketKey is tower's public key, which direct streams' tickets are
	// verified with; nil refuses every stream.
	TicketKey ed25519.PublicKey
	// StreamURL is where clients reach Streams, such as
	// http://host.tailnet:7443; empty refuses tickets.
	StreamURL string
	// ReportEvery is how often the Target's status is reported and every
	// Instance read back; SweepEvery how often the sweep runs, SweepGrace
	// its grace. Zero is each one's default.
	ReportEvery, SweepEvery, SweepGrace time.Duration
	Clock                               clockwork.Clock
	Log                                 *slog.Logger
}

// Host is one host running hostd. Make it with New and run it with Run.
type Host struct {
	cfg   Config
	d     driver.Drivers
	c     *client.Client
	clock clockwork.Clock
	log   *slog.Logger
	// mu is the host lock: reconciles and sweeps take turns under it, so
	// a sweep never removes what a reconcile is making.
	mu   sync.Mutex
	ctrl *controller.Controller
	// limits are SetProcessLimits'; nil is provider's defaults.
	limits atomic.Pointer[processLimits]
}

var _ provider.Provider = (*Host)(nil)

// New returns a host of cfg.
func New(cfg Config) *Host {
	cfg.ReportEvery = cmp.Or(cfg.ReportEvery, DefaultReportEvery)
	cfg.SweepEvery = cmp.Or(cfg.SweepEvery, DefaultSweepEvery)
	cfg.SweepGrace = cmp.Or(cfg.SweepGrace, DefaultSweepGrace)
	h := &Host{cfg: cfg, d: cfg.Drivers, c: cfg.Client, clock: cfg.Clock, log: cfg.Log}
	if h.clock == nil {
		h.clock = clockwork.NewRealClock()
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	h.ctrl = h.newController()
	return h
}

// Run converges the host, reports and sweeps until ctx is done. It first
// checks that its token is its target's, which is fatal otherwise.
func (h *Host) Run(ctx context.Context) error {
	if err := h.checkTarget(ctx); err != nil || ctx.Err() != nil {
		return err
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return h.ctrl.Run(ctx) })
	g.Go(func() error { h.every(ctx, h.cfg.ReportEvery, "report", h.Report); return nil })
	g.Go(func() error { h.every(ctx, h.cfg.SweepEvery, "sweep", h.Sweep); return nil })
	return g.Wait()
}

// checkTarget reads the Target, which only the target's own token may.
// One that does not answer is waited for: the apiserver may start after.
// A ctx done while it waits is no error.
func (h *Host) checkTarget(ctx context.Context) error {
	wait := 200 * time.Millisecond
	for ctx.Err() == nil {
		_, err := h.getTarget(ctx)
		switch code := connect.CodeOf(err); {
		case err == nil:
			return nil
		case code == connect.CodeNotFound || code == connect.CodePermissionDenied || code == connect.CodeUnauthenticated:
			return fmt.Errorf("target %q: %w; hostd's token must be a ROLE_HOSTD token of that target", h.cfg.Target, err)
		}
		if ctx.Err() == nil {
			h.log.Warn("tower does not answer yet", "error", err, "retry_in", wait)
		}
		select {
		case <-ctx.Done():
		case <-h.clock.After(wait):
		}
		wait = min(2*wait, 5*time.Second)
	}
	return nil
}

// every runs fn now and then every d until ctx is done.
func (h *Host) every(ctx context.Context, d time.Duration, what string, fn func(context.Context) error) {
	t := h.clock.NewTicker(d)
	defer t.Stop()
	for {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			h.log.Error(what+" failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.Chan():
		}
	}
}

// Status is what the host's controller is doing, for WaitSettled.
func (h *Host) Status() []controller.Status { return []controller.Status{h.ctrl.Status()} }

// Names of what hostd makes, after the uid of the Instance it serves, so a
// later Instance of the same name never takes an earlier one's.
func guestName(uid string) string  { return "tower-" + uid }
func diskName(uid string) string   { return "tower-" + uid }
func goldenName(uid string) string { return "golden-" + uid }

// networkName is a workstream network's on the host: workstreams are
// named within their project.
func networkName(project, network string) string { return "tower-" + project + "-" + network }

// mine reports whether what carries labels is this hostd's.
func (h *Host) mine(labels driver.Labels) bool { return labels[driver.LabelOwner] == h.cfg.Target }

// providerError is what a driver's error is to tower.
func providerError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, driver.ErrNotFound):
		return fmt.Errorf("%w: %w", err, provider.ErrNotFound)
	case errors.Is(err, driver.ErrNoSpace):
		return fmt.Errorf("%w: %w", err, provider.ErrNoSpace)
	case errors.Is(err, driver.ErrLeaseLost):
		return fmt.Errorf("%w: %w", err, provider.ErrLeaseLost)
	case errors.Is(err, fs.ErrNotExist):
		return err
	case errors.Is(err, driver.ErrNotRunning), errors.Is(err, driver.ErrBadOffset), errors.Is(err, driver.ErrBusy),
		errors.Is(err, driver.ErrStartFailed), errors.Is(err, driver.ErrConflict):
		return fmt.Errorf("%w: %w", err, provider.ErrFailed)
	}
	return err
}
