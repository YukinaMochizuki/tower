// Package controller runs level-triggered reconcilers, in the apiserver over
// the store and in the node agent over the API. A Controller follows some
// kinds, turns every change into the keys it affects, and calls Reconcile for
// each until it succeeds. Reconcile is handed only a key, never the event, so
// it always works from the current state and a missed or repeated event
// cannot lead it astray. See docs/checklists/new-controller.md.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/store"
)

// Key names one object of the kind a reconciler is responsible for. Project
// is empty for global kinds.
type Key struct {
	Project string
	Name    string
}

func (k Key) String() string {
	if k.Project == "" {
		return k.Name
	}
	return k.Project + "/" + k.Name
}

// Reconciler brings the world in line with the object at key. It must be
// idempotent: it is called again after every relevant change, on every
// resync, and after any error.
type Reconciler interface {
	Reconcile(ctx context.Context, key Key) error
}

// ReconcilerFunc adapts a function to Reconciler.
type ReconcilerFunc func(ctx context.Context, key Key) error

func (f ReconcilerFunc) Reconcile(ctx context.Context, key Key) error { return f(ctx, key) }

// ErrWaiting, wrapped in the error Reconcile returns, says it waits on
// something outside its control, such as a VM booting: the key is checked
// again soon (see MaxWait), and nothing is logged as a failure.
var ErrWaiting = errors.New("waiting")

// Watch says which keys a change to one kind affects. Map is given the
// object as written (or, for a delete, as last stored) and returns the keys
// to reconcile; it may read the current state.
type Watch struct {
	Kind string
	Map  func(ctx context.Context, o store.Object) []Key
	// SkipResync maps the kind only as it changes, not on start or resync:
	// for a kind whose mapping is costly when another watch already queues
	// every key then. At least one watch of a controller does not skip.
	SkipResync bool
}

// Self maps an object of the reconciled kind to its own key.
func Self(_ context.Context, o store.Object) []Key {
	return []Key{{Project: o.Project, Name: o.Name}}
}

// Controller feeds a Reconciler. Run it with Run.
type Controller struct {
	// Name appears in logs.
	Name   string
	Source Source
	// Watches lists every kind whose changes matter; each object of every
	// watched kind is also mapped on start and on every resync.
	Watches    []Watch
	Reconciler Reconciler
	// Resync is how often every object is mapped again, catching anything a
	// reconcile could not see coming (time passing, for one). Zero means five
	// minutes.
	Resync time.Duration
	// MaxBackoff caps the delay before a failing key is retried. Zero means
	// one minute; the first retry comes after 100ms.
	MaxBackoff time.Duration
	// MaxWait caps the delay before a key that returned ErrWaiting is
	// checked again. Zero means two seconds.
	MaxWait time.Duration
	// Trigger, when it delivers, maps every object again at once, as a
	// resync does: for changes no watched kind shows, such as a VM exiting
	// on the host.
	Trigger <-chan struct{}
	Clock   clockwork.Clock
	Log     *slog.Logger

	q atomic.Pointer[queue]
}

// Status is what a controller was doing at one moment, read under one lock.
type Status struct {
	Name string
	// Queued counts the keys waiting for their turn.
	Queued int
	// InFlight counts the reconciles and the listings running, a listing
	// from the moment the watch or list before it ended.
	InFlight int
	// Followed is the store revision through which every change to a watched
	// kind has been turned into queued keys; zero until the first list.
	Followed int64
	// Retries are the keys whose last reconcile failed or waited, until one
	// succeeds, earliest due first.
	Retries []Retry
}

// Retry is a key a controller queues again at Due, or queued at Due and is
// to reconcile.
type Retry struct {
	Key Key
	Due time.Time
	// Waiting says Reconcile returned ErrWaiting rather than failing.
	Waiting bool
	// Attempts counts the reconciles in a row that failed or waited.
	Attempts int
}

// Status reports what c is doing. Before Run starts, it has followed
// nothing.
func (c *Controller) Status() Status {
	if q := c.q.Load(); q != nil {
		return q.status(c.Name)
	}
	return Status{Name: c.Name}
}

// Enqueue queues keys to be reconciled, as a watched change mapped to them
// would; before Run, or after it ends, it does nothing.
func (c *Controller) Enqueue(keys ...Key) {
	if q := c.q.Load(); q != nil {
		q.addAll(keys)
	}
}

// Run reconciles until ctx is done, and returns once every reconcile it
// started has finished. A failing source is retried, never fatal: one
// controller's trouble must not stop the other background work of the
// process running it.
func (c *Controller) Run(ctx context.Context) error {
	if !slices.ContainsFunc(c.Watches, func(w Watch) bool { return !w.SkipResync }) {
		return fmt.Errorf("controller %s lists nothing on start: every watch skips resyncs", c.Name)
	}
	if c.Clock == nil {
		c.Clock = clockwork.NewRealClock()
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Resync == 0 {
		c.Resync = 5 * time.Minute
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = time.Minute
	}
	if c.MaxWait == 0 {
		c.MaxWait = 2 * time.Second
	}
	log := c.Log.With("controller", c.Name)
	q := newQueue(c.Clock, 100*time.Millisecond, c.MaxBackoff)
	c.q.Store(q)
	var wg sync.WaitGroup
	defer func() {
		q.shutdown()
		wg.Wait()
	}()

	wg.Add(2)
	go func() {
		defer wg.Done()
		c.work(ctx, q, log)
	}()
	go func() {
		defer wg.Done()
		ticker := c.Clock.NewTicker(c.Resync)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.Chan():
			case <-c.Trigger:
			}
			if _, err := c.mapAll(ctx, q); err != nil && ctx.Err() == nil {
				log.Error("resync failed", "error", err)
			}
		}
	}()

	// relisting is set from a watch or list ending until the next list is
	// done, and counts as a listing: what it missed is not queued yet.
	relisting := false
	defer func() {
		if relisting {
			q.finish()
		}
	}()
	for {
		rev, err := c.mapAll(ctx, q)
		if err == nil {
			q.follow(rev)
			if relisting {
				q.finish()
				relisting = false
			}
			err = c.follow(ctx, q, rev)
		}
		if ctx.Err() != nil {
			return nil
		}
		if !relisting {
			q.start()
			relisting = true
		}
		switch {
		case errors.Is(err, store.ErrCompacted):
			log.Info("watch fell behind compaction; listing again")
			continue
		case err == nil:
			// The source ended the watch, as an API server may.
			log.Debug("watch ended; listing again shortly")
		default:
			log.Error("following changes failed; listing again shortly", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-c.Clock.After(time.Second):
		}
	}
}

// mapAll queues the keys of every object of every watched kind, and returns
// the earliest of the lists' revisions, so a watch from it misses nothing.
func (c *Controller) mapAll(ctx context.Context, q *queue) (int64, error) {
	q.start()
	defer q.finish()
	var since int64
	first := true
	for _, w := range c.Watches {
		if w.SkipResync {
			continue
		}
		objs, rev, err := c.Source.List(ctx, w.Kind)
		if err != nil {
			return 0, err
		}
		if first || rev < since {
			since, first = rev, false
		}
		for _, o := range objs {
			q.addAll(w.Map(ctx, o))
		}
	}
	return since, nil
}

func (c *Controller) follow(ctx context.Context, q *queue, since int64) error {
	byKind := map[string]Watch{}
	kinds := make([]string, 0, len(c.Watches))
	for _, w := range c.Watches {
		byKind[w.Kind] = w
		kinds = append(kinds, w.Kind)
	}
	return c.Source.Watch(ctx, kinds, since, func(ev store.Event) error {
		if ev.Type == store.EventProgress {
			q.follow(ev.Revision)
			return nil
		}
		if w, ok := byKind[ev.Object.Kind]; ok {
			q.addAll(w.Map(ctx, ev.Object))
		}
		return nil
	})
}

func (c *Controller) work(ctx context.Context, q *queue, log *slog.Logger) {
	for {
		key, ok := q.get(ctx)
		if !ok {
			return
		}
		err := c.Reconciler.Reconcile(ctx, key)
		switch {
		case err == nil:
			q.done(key, false)
		case ctx.Err() != nil:
			q.done(key, false)
			return
		case errors.Is(err, ErrWaiting):
			delay := q.wait(key, c.MaxWait)
			log.Debug("reconcile waiting; checking again", "key", key.String(), "in", delay, "for", err)
		default:
			delay := q.done(key, true)
			log.Warn("reconcile failed; retrying", "key", key.String(), "in", delay, "error", err)
		}
	}
}
