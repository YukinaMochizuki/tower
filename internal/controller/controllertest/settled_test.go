package controllertest

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// Each test below holds something back until WaitSettled has polled a few
// times, from inside its revision reads, and then checks the work it held
// back is done: a WaitSettled that returned without waiting for it fails.

func openStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// run runs a controller of the A objects over src.
func run(t *testing.T, src controller.Source, r controller.Reconciler, clock clockwork.Clock) *controller.Controller {
	t.Helper()
	c := &controller.Controller{
		Name: "a", Source: src, Reconciler: r, Clock: clock,
		Watches: []controller.Watch{{Kind: "A", Map: controller.Self}},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	return c
}

func statusOf(c *controller.Controller) Statuses {
	return func() []controller.Status { return []controller.Status{c.Status()} }
}

// afterPolls reads st's revision, and calls then once WaitSettled has read
// it n times, and on every read after.
func afterPolls(st store.Store, n int, then func()) Revision {
	var reads atomic.Int64
	return func(ctx context.Context) (int64, error) {
		rev, err := st.Revision(ctx)
		if reads.Add(1) >= int64(n) {
			then()
		}
		return rev, err
	}
}

// heldSource is a StoreSource that holds back the event of one object
// until released, before the controller sees it.
type heldSource struct {
	controller.StoreSource
	name    string
	release chan struct{}
}

func (h heldSource) Watch(ctx context.Context, kinds []string, since int64, fn func(store.Event) error) error {
	return h.StoreSource.Watch(ctx, kinds, since, func(ev store.Event) error {
		if ev.Object.Name == h.name {
			select {
			case <-h.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return fn(ev)
	})
}

type reconciler func(ctx context.Context, k controller.Key) error

func (r reconciler) Reconcile(ctx context.Context, k controller.Key) error { return r(ctx, k) }

// TestAWriteNotYetQueuedIsNotSettled: a write the store holds but the
// controller has not yet turned into a key leaves it unsettled, though its
// queue is empty and nothing runs.
func TestAWriteNotYetQueuedIsNotSettled(t *testing.T) {
	st := openStore(t)
	held := heldSource{StoreSource: controller.StoreSource{Store: st}, name: "x", release: make(chan struct{})}
	var reconciled atomic.Bool
	c := run(t, held, reconciler(func(_ context.Context, k controller.Key) error {
		if k.Name == "x" {
			reconciled.Store(true)
		}
		return nil
	}), clockwork.NewRealClock())
	// So that x comes through the watch, not the first list.
	if _, err := st.Create(t.Context(), store.Key{Kind: "A", Name: "first"}, nil); err != nil {
		t.Fatal(err)
	}
	WaitSettled(t, st.Revision, statusOf(c))

	if _, err := st.Create(t.Context(), store.Key{Kind: "A", Name: "x"}, nil); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	WaitSettled(t, afterPolls(st, 3, func() { once.Do(func() { close(held.release) }) }), statusOf(c))
	if !reconciled.Load() {
		t.Error("settled before the write was reconciled")
	}
}

// TestAReconcileInFlightIsNotSettled: a reconcile still running leaves the
// controller unsettled.
func TestAReconcileInFlightIsNotSettled(t *testing.T) {
	st := openStore(t)
	release := make(chan struct{})
	var finished atomic.Bool
	c := run(t, controller.StoreSource{Store: st}, reconciler(func(ctx context.Context, _ controller.Key) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		finished.Store(true)
		return nil
	}), clockwork.NewRealClock())
	if _, err := st.Create(t.Context(), store.Key{Kind: "A", Name: "x"}, nil); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	WaitSettled(t, afterPolls(st, 3, func() { once.Do(func() { close(release) }) }), statusOf(c))
	if !finished.Load() {
		t.Error("settled with a reconcile in flight")
	}
}

// TestAPendingRetryIsNotSettled: a key whose reconcile failed leaves the
// controller unsettled until its retry has run.
func TestAPendingRetryIsNotSettled(t *testing.T) {
	st := openStore(t)
	clock := clockwork.NewFakeClock()
	var calls atomic.Int64
	c := run(t, controller.StoreSource{Store: st}, reconciler(func(context.Context, controller.Key) error {
		if calls.Add(1) == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}), clock)
	if _, err := st.Create(t.Context(), store.Key{Kind: "A", Name: "x"}, nil); err != nil {
		t.Fatal(err)
	}
	WaitSettled(t, afterPolls(st, 3, func() {
		if calls.Load() < 2 {
			clock.Advance(time.Second)
		}
	}), statusOf(c))
	if n := calls.Load(); n != 2 {
		t.Errorf("settled after %d reconciles, want the failure and its retry", n)
	}
}

// TestWaitingKeysAreReported: a key waiting on something outside the
// controller does not hold WaitSettled up, and is returned.
func TestWaitingKeysAreReported(t *testing.T) {
	st := openStore(t)
	c := run(t, controller.StoreSource{Store: st}, reconciler(func(_ context.Context, k controller.Key) error {
		if k.Name == "booting" {
			return controller.ErrWaiting
		}
		return nil
	}), clockwork.NewFakeClock())
	for _, name := range []string{"booting", "up"} {
		if _, err := st.Create(t.Context(), store.Key{Kind: "A", Name: name}, nil); err != nil {
			t.Fatal(err)
		}
	}
	got := WaitSettled(t, st.Revision, statusOf(c))
	want := []Waiting{{Controller: "a", Key: controller.Key{Name: "booting"}}}
	if !slices.Equal(got, want) {
		t.Errorf("waiting %v, want %v", got, want)
	}
}

// failOnce is a StoreSource whose first watch fails at once, and which
// counts its lists.
type failOnce struct {
	controller.StoreSource
	failed  chan struct{}
	watches atomic.Int64
	lists   atomic.Int64
}

func (f *failOnce) List(ctx context.Context, kind string) ([]store.Object, int64, error) {
	f.lists.Add(1)
	return f.StoreSource.List(ctx, kind)
}

func (f *failOnce) Watch(ctx context.Context, kinds []string, since int64, fn func(store.Event) error) error {
	if f.watches.Add(1) == 1 {
		close(f.failed)
		return errors.New("connection reset")
	}
	return f.StoreSource.Watch(ctx, kinds, since, fn)
}

// TestAWatchEndedIsNotSettled: from a watch failing until the list after
// it, what the controller missed meanwhile is not queued: it has not
// settled.
func TestAWatchEndedIsNotSettled(t *testing.T) {
	st := openStore(t)
	clock := clockwork.NewFakeClock()
	src := &failOnce{StoreSource: controller.StoreSource{Store: st}, failed: make(chan struct{})}
	c := run(t, src, reconciler(func(context.Context, controller.Key) error { return nil }), clock)
	<-src.failed
	testutil.Eventually(t, "listing again counted as a listing", func() (bool, string) {
		s := c.Status()
		return s.InFlight > 0, fmt.Sprintf("%+v", s)
	})
	lists := src.lists.Load()
	// The relist waits a second of the fake clock, which WaitSettled's
	// third read on moves.
	WaitSettled(t, afterPolls(st, 3, func() { clock.Advance(time.Second) }), statusOf(c))
	if src.lists.Load() == lists {
		t.Error("settled before listing again after the watch failed")
	}
}

// TestAControllerStoppedWhileRelistingIsIdle: a controller stopped while it
// waits to list again leaves nothing counted in flight.
func TestAControllerStoppedWhileRelistingIsIdle(t *testing.T) {
	st := openStore(t)
	src := &failOnce{StoreSource: controller.StoreSource{Store: st}, failed: make(chan struct{})}
	c := &controller.Controller{
		Name: "a", Source: src, Clock: clockwork.NewFakeClock(),
		Reconciler: reconciler(func(context.Context, controller.Key) error { return nil }),
		Watches:    []controller.Watch{{Kind: "A", Map: controller.Self}},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	<-src.failed
	testutil.Eventually(t, "listing again counted as a listing", func() bool { return c.Status().InFlight > 0 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if s := c.Status(); s.InFlight != 0 {
		t.Errorf("stopped, it still counts %d in flight", s.InFlight)
	}
}

// TestTheRevisionMustHoldAcrossTwoPolls: polls that each find everything
// idle and caught up do not settle while the revision moves between them.
func TestTheRevisionMustHoldAcrossTwoPolls(t *testing.T) {
	var reads atomic.Int64
	revision := func(context.Context) (int64, error) { return min(reads.Add(1), 5), nil }
	idle := func() []controller.Status { return []controller.Status{{Name: "a", Followed: 100}} }
	WaitSettled(t, revision, idle)
	if n := reads.Load(); n != 8 {
		t.Errorf("settled after %d reads, want 8: the first two polls at revision 5", n)
	}
}

// TestARevisionMovingDuringAPollIsNotSettled: a write while a poll reads
// the statuses leaves it unsettled, though the polls' first reads agree.
func TestARevisionMovingDuringAPollIsNotSettled(t *testing.T) {
	var reads atomic.Int64
	// 5, 6, 5, 6, then 7 for good.
	revision := func(context.Context) (int64, error) {
		if n := reads.Add(1); n <= 4 {
			return 4 + 2 - n%2, nil
		}
		return 7, nil
	}
	idle := func() []controller.Status { return []controller.Status{{Name: "a", Followed: 100}} }
	WaitSettled(t, revision, idle)
	if n := reads.Load(); n != 8 {
		t.Errorf("settled after %d reads, want 8: two polls at revision 7", n)
	}
}
