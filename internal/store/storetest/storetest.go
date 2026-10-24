// Package storetest is the contract every store.Store implementation must
// pass. An implementation's tests call Run with a constructor for empty
// stores.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// Run runs the whole contract. open must return a new, empty store; Run
// closes it.
func Run(t *testing.T, open func(t *testing.T) store.Store) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s store.Store)
	}{
		{"CreateGet", testCreateGet},
		{"UpdateDelete", testUpdateDelete},
		{"EmptyData", testEmptyData},
		{"RevisionIsGlobal", testRevisionIsGlobal},
		{"List", testList},
		{"ListSnapshot", testListSnapshot},
		{"WatchReplayThenLive", testWatchReplayThenLive},
		{"WatchFromZero", testWatchFromZero},
		{"WatchFilters", testWatchFilters},
		{"WatchProgressAdvances", testWatchProgressAdvances},
		{"WatchProgressNeverAhead", testWatchProgressNeverAhead},
		{"WatchSlowConsumer", testWatchSlowConsumer},
		{"WatchStartsDuringWrites", testWatchStartsDuringWrites},
		{"ConcurrentWriters", testConcurrentWriters},
		{"TxnCommitsTogether", testTxnCommitsTogether},
		{"TxnRollsBack", testTxnRollsBack},
		{"Compaction", testCompaction},
		{"CompactionOvertakesSlowWatch", testCompactionOvertakesSlowWatch},
		{"WatchEndsOnClose", testWatchEndsOnClose},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := open(t)
			t.Cleanup(func() { _ = s.Close() })
			tt.fn(t, s)
		})
	}
}

func key(kind, project, name string) store.Key {
	return store.Key{Kind: kind, Project: project, Name: name}
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func testCreateGet(t *testing.T, s store.Store) {
	ctx := t.Context()
	k := key("Container", "p", "web")
	rev := must[int64](t)(s.Create(ctx, k, []byte("v1")))
	if rev != 1 {
		t.Errorf("first revision is %d, want 1", rev)
	}
	got := must[store.Object](t)(s.Get(ctx, k))
	if string(got.Data) != "v1" || got.Revision != 1 || got.Key != k {
		t.Errorf("Get returned %+v", got)
	}
	if _, err := s.Create(ctx, k, []byte("again")); !errors.Is(err, store.ErrExists) {
		t.Errorf("second Create: %v, want ErrExists", err)
	}
	if _, err := s.Get(ctx, key("Container", "other", "web")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get in another project: %v, want ErrNotFound", err)
	}
	if rev := must[int64](t)(s.Revision(ctx)); rev != 1 {
		t.Errorf("a failed Create moved the revision to %d", rev)
	}
}

func testUpdateDelete(t *testing.T, s store.Store) {
	ctx := t.Context()
	k := key("Tenant", "", "acme")
	must[int64](t)(s.Create(ctx, k, []byte("v1")))

	if _, err := s.Update(ctx, k, []byte("v2"), 7); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Update with a stale revision: %v, want ErrConflict", err)
	}
	if rev := must[int64](t)(s.Update(ctx, k, []byte("v2"), 1)); rev != 2 {
		t.Errorf("Update returned revision %d, want 2", rev)
	}
	if rev := must[int64](t)(s.Update(ctx, k, []byte("v3"), 0)); rev != 3 {
		t.Errorf("unconditional Update returned revision %d, want 3", rev)
	}
	if _, err := s.Update(ctx, key("Tenant", "", "missing"), nil, 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Update of a missing object: %v, want ErrNotFound", err)
	}

	if _, _, err := s.Delete(ctx, k, 2); !errors.Is(err, store.ErrConflict) {
		t.Errorf("Delete with a stale revision: %v, want ErrConflict", err)
	}
	last, rev, err := s.Delete(ctx, k, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(last.Data) != "v3" || last.Revision != 3 || rev != 4 {
		t.Errorf("Delete returned %+v at revision %d", last, rev)
	}
	if _, err := s.Get(ctx, k); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get after Delete: %v, want ErrNotFound", err)
	}
	if _, _, err := s.Delete(ctx, k, 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("second Delete: %v, want ErrNotFound", err)
	}
}

func testEmptyData(t *testing.T, s store.Store) {
	ctx := t.Context()
	k := key("Tenant", "", "empty")
	must[int64](t)(s.Create(ctx, k, nil))
	must[int64](t)(s.Update(ctx, k, []byte{}, 0))
	if _, _, err := s.Delete(ctx, k, 0); err != nil {
		t.Errorf("deleting an object with no data: %v", err)
	}
}

func testRevisionIsGlobal(t *testing.T, s store.Store) {
	ctx := t.Context()
	var revs []int64
	revs = append(revs, must[int64](t)(s.Create(ctx, key("Tenant", "", "a"), nil)))
	revs = append(revs, must[int64](t)(s.Create(ctx, key("Container", "p", "a"), nil)))
	revs = append(revs, must[int64](t)(s.Update(ctx, key("Tenant", "", "a"), nil, 0)))
	if !slices.Equal(revs, []int64{1, 2, 3}) {
		t.Errorf("revisions across kinds are %v, want 1 2 3", revs)
	}
}

func testList(t *testing.T, s store.Store) {
	ctx := t.Context()
	for _, k := range []store.Key{
		key("Container", "p2", "b"), key("Container", "p1", "b"), key("Container", "p1", "a"),
		key("Container", "p2", "a"), key("Machine", "p1", "a"),
	} {
		must[int64](t)(s.Create(ctx, k, []byte(k.Project+"/"+k.Name)))
	}
	names := func(objs []store.Object) []string {
		var out []string
		for _, o := range objs {
			out = append(out, o.Project+"/"+o.Name)
		}
		return out
	}

	all, rev, err := s.List(ctx, store.ListOptions{Kind: "Container"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"p1/a", "p1/b", "p2/a", "p2/b"}; !slices.Equal(names(all), want) {
		t.Errorf("List all = %v, want %v", names(all), want)
	}
	if rev != 5 {
		t.Errorf("List revision %d, want 5", rev)
	}

	p1 := must[[]store.Object](t)(func() ([]store.Object, error) {
		objs, _, err := s.List(ctx, store.ListOptions{Kind: "Container", Project: "p1"})
		return objs, err
	}())
	if want := []string{"p1/a", "p1/b"}; !slices.Equal(names(p1), want) {
		t.Errorf("List p1 = %v, want %v", names(p1), want)
	}

	var paged []string
	after := store.Key{}
	for range 10 {
		page, _, err := s.List(ctx, store.ListOptions{Kind: "Container", StartAfter: after, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		paged = append(paged, names(page)...)
		if len(page) < 3 {
			break
		}
		after = page[len(page)-1].Key
	}
	if want := []string{"p1/a", "p1/b", "p2/a", "p2/b"}; !slices.Equal(paged, want) {
		t.Errorf("paged List = %v, want %v", paged, want)
	}
}

// collector gathers watch events from a background watch.
type collector struct {
	mu     sync.Mutex
	events []store.Event
	done   chan error
	cancel context.CancelFunc
}

func watch(t *testing.T, s store.Store, opts store.WatchOptions) *collector {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	c := &collector{done: make(chan error, 1), cancel: cancel}
	go func() {
		c.done <- s.Watch(ctx, opts, func(ev store.Event) error {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.events = append(c.events, ev)
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-c.done
	})
	return c
}

// waitEvents waits until the collector has n events and returns them.
func (c *collector) waitEvents(t *testing.T, n int) []store.Event {
	t.Helper()
	var got []store.Event
	testutil.Eventually(t, fmt.Sprintf("%d events", n), func() (bool, string) {
		c.mu.Lock()
		got = slices.Clone(c.events)
		c.mu.Unlock()
		return len(got) >= n, fmt.Sprint(describe(got))
	})
	return got
}

func describe(evs []store.Event) []string {
	var out []string
	for _, ev := range evs {
		out = append(out, fmt.Sprintf("%d:%d:%s/%s", ev.Revision, ev.Type, ev.Object.Project, ev.Object.Name))
	}
	return out
}

func revisions(evs []store.Event) []int64 {
	var out []int64
	for _, ev := range evs {
		out = append(out, ev.Revision)
	}
	return out
}

func span(from, to int64) []int64 {
	var out []int64
	for r := from; r <= to; r++ {
		out = append(out, r)
	}
	return out
}

func testWatchReplayThenLive(t *testing.T, s store.Store) {
	ctx := t.Context()
	k := key("Container", "p", "web")
	must[int64](t)(s.Create(ctx, k, []byte("v1")))
	must[int64](t)(s.Update(ctx, k, []byte("v2"), 0))
	must[int64](t)(s.Update(ctx, k, []byte("v3"), 0))

	c := watch(t, s, store.WatchOptions{Kind: "Container", SinceRevision: 1})
	c.waitEvents(t, 2)
	must[int64](t)(s.Update(ctx, k, []byte("v4"), 0))
	if _, _, err := s.Delete(ctx, k, 0); err != nil {
		t.Fatal(err)
	}

	got := c.waitEvents(t, 4)
	if !slices.Equal(revisions(got), []int64{2, 3, 4, 5}) {
		t.Fatalf("revisions %v, want 2 3 4 5", revisions(got))
	}
	wantTypes := []store.EventType{store.EventUpdated, store.EventUpdated, store.EventUpdated, store.EventDeleted}
	for i, ev := range got {
		if ev.Type != wantTypes[i] {
			t.Errorf("event %d has type %d, want %d", i, ev.Type, wantTypes[i])
		}
	}
	if del := got[3]; string(del.Object.Data) != "v4" || del.Object.Revision != 5 {
		t.Errorf("delete event carries %+v, want the last data at the delete's revision", del.Object)
	}
}

func testWatchFromZero(t *testing.T, s store.Store) {
	ctx := t.Context()
	must[int64](t)(s.Create(ctx, key("Tenant", "", "a"), nil))
	must[int64](t)(s.Create(ctx, key("Tenant", "", "b"), nil))
	c := watch(t, s, store.WatchOptions{SinceRevision: 0})
	c.waitEvents(t, 2)
	must[int64](t)(s.Create(ctx, key("Tenant", "", "c"), nil))
	if got := c.waitEvents(t, 3); !slices.Equal(revisions(got), []int64{1, 2, 3}) {
		t.Errorf("watch from 0 saw %v, want all history then live", revisions(got))
	}
}

func testWatchFilters(t *testing.T, s store.Store) {
	ctx := t.Context()
	c, base := watchFromHere(t, s, store.WatchOptions{Kind: "Container", Project: "p1"})
	must[int64](t)(s.Create(ctx, key("Container", "p2", "x"), nil))
	must[int64](t)(s.Create(ctx, key("Machine", "p1", "x"), nil))
	must[int64](t)(s.Create(ctx, key("Container", "p1", "x"), nil))
	must[int64](t)(s.Create(ctx, key("Container", "p1", "y"), nil))
	got := c.waitEvents(t, 2)
	if !slices.Equal(revisions(got), []int64{base + 3, base + 4}) {
		t.Errorf("filtered watch delivered %v", describe(got))
	}
}

// watchFromHere starts a watch at the current revision, so every later write
// is delivered without racing the watch's subscription.
func watchFromHere(t *testing.T, s store.Store, opts store.WatchOptions) (*collector, int64) {
	t.Helper()
	rev := must[int64](t)(s.Revision(t.Context()))
	opts.SinceRevision = rev
	return watch(t, s, opts), rev
}

func testWatchProgressAdvances(t *testing.T, s store.Store) {
	ctx := t.Context()
	progress := make(chan struct{})
	c, _ := watchFromHere(t, s, store.WatchOptions{Kind: "Container", Project: "p1", Progress: progress})
	must[int64](t)(s.Create(ctx, key("Container", "p1", "x"), nil))
	c.waitEvents(t, 1)
	// Writes the watch filters out still advance the revision it reports,
	// once the watch has processed them.
	must[int64](t)(s.Create(ctx, key("Machine", "p1", "x"), nil))
	rev := must[int64](t)(s.Create(ctx, key("Container", "p2", "x"), nil))
	testutil.Eventually(t, fmt.Sprintf("progress at %d", rev), func() (bool, string) {
		progress <- struct{}{}
		got := c.waitEvents(t, 2)
		last := got[len(got)-1]
		if last.Type != store.EventProgress || last.Object.Key != (store.Key{}) || last.Object.Data != nil {
			t.Fatalf("want a bare progress event, got %v", describe(got))
		}
		if last.Revision > rev {
			t.Fatalf("progress reported %d, beyond the last write %d", last.Revision, rev)
		}
		c.mu.Lock()
		c.events = c.events[:1]
		c.mu.Unlock()
		return last.Revision == rev, fmt.Sprint(last.Revision)
	})
}

func testWatchSlowConsumer(t *testing.T, s store.Store) {
	ctx := t.Context()
	start := must[int64](t)(s.Revision(ctx))
	release := make(chan struct{})
	var mu sync.Mutex
	var got []int64
	first := true
	wctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- s.Watch(wctx, store.WatchOptions{SinceRevision: start}, func(ev store.Event) error {
			if first {
				first = false
				<-release
			}
			mu.Lock()
			defer mu.Unlock()
			got = append(got, ev.Revision)
			return nil
		})
	}()
	defer func() {
		cancel()
		<-done
	}()

	// The first event blocks the consumer while far more events than any
	// reasonable live buffer pile up behind it.
	const n = 3000
	for i := range n {
		must[int64](t)(s.Create(ctx, key("Container", "p", fmt.Sprintf("c%d", i)), nil))
	}
	close(release)

	var cur []int64
	// Thousands of events through SQLite take seconds under the race
	// detector.
	testutil.EventuallyWithin(t, 30*time.Second, "the slow consumer at the last event", func() (bool, string) {
		mu.Lock()
		cur = slices.Clone(got)
		mu.Unlock()
		return len(cur) > 0 && cur[len(cur)-1] == start+n, fmt.Sprintf("%d events", len(cur))
	})
	if !slices.Equal(cur, span(start+1, start+n)) {
		t.Fatalf("slow consumer saw %d events from %d, want every revision from %d once", len(cur), cur[0], start+1)
	}
}

func testWatchStartsDuringWrites(t *testing.T, s store.Store) {
	ctx := t.Context()
	for round := range 10 {
		stop := make(chan struct{})
		written := make(chan int64, 1)
		// writing is closed once the writer has written, or given up.
		writing := make(chan struct{})
		var once sync.Once
		go func() {
			var last int64
			defer func() { written <- last }()
			defer once.Do(func() { close(writing) })
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				rev, err := s.Create(ctx, key("Container", "p", fmt.Sprintf("r%d-%d", round, i)), nil)
				if err != nil {
					t.Error(err)
					return
				}
				once.Do(func() { close(writing) })
				last = rev
			}
		}()
		<-writing
		since := must[int64](t)(s.Revision(ctx))
		c := watch(t, s, store.WatchOptions{SinceRevision: since})
		// Writing on until the watch has delivered well past its replay
		// makes sure it also went through the handoff to live events.
		c.waitEvents(t, 200)
		close(stop)
		lastRev := <-written
		got := c.waitEvents(t, int(lastRev-since))
		if !slices.Equal(revisions(got), span(since+1, lastRev)) {
			t.Fatalf("round %d: watch from %d saw %d events, want every revision through %d once", round, since, len(got), lastRev)
		}
		c.cancel()
	}
}

func testConcurrentWriters(t *testing.T, s store.Store) {
	ctx := t.Context()
	c, base := watchFromHere(t, s, store.WatchOptions{})
	const writers, each = 8, 50
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if _, err := s.Create(ctx, key("Container", fmt.Sprintf("w%d", w), fmt.Sprintf("c%d", i)), nil); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if rev := must[int64](t)(s.Revision(ctx)); rev != base+writers*each {
		t.Errorf("revision %d after %d writes from %d", rev, writers*each, base)
	}
	got := c.waitEvents(t, writers*each)
	if !slices.Equal(revisions(got), span(base+1, base+writers*each)) {
		t.Errorf("concurrent writes were not seen once each, in order")
	}
}

// testTxnCommitsTogether: a transaction reads its own writes, which no one
// else sees until it commits; then they all are, at consecutive revisions,
// and a watch sees each once, in order. A write it refuses changes nothing
// and the transaction goes on.
func testTxnCommitsTogether(t *testing.T, s store.Store) {
	ctx := t.Context()
	must[int64](t)(s.Create(ctx, key("Tenant", "", "before"), []byte("b")))
	c, base := watchFromHere(t, s, store.WatchOptions{})
	err := s.Txn(ctx, func(tx store.Tx) error {
		if rev := must[int64](t)(tx.Create(ctx, key("Tenant", "", "a"), []byte("a1"))); rev != base+1 {
			t.Errorf("first write in the transaction at %d, want %d", rev, base+1)
		}
		if _, err := tx.Create(ctx, key("Tenant", "", "before"), nil); !errors.Is(err, store.ErrExists) {
			t.Errorf("Create of a taken key in a transaction: %v, want ErrExists", err)
		}
		must[int64](t)(tx.Update(ctx, key("Tenant", "", "a"), []byte("a2"), base+1))
		must[int64](t)(tx.Create(ctx, key("Container", "p", "b"), []byte("b1")))
		if got := must[store.Object](t)(tx.Get(ctx, key("Tenant", "", "a"))); string(got.Data) != "a2" {
			t.Errorf("the transaction read %q of its own write, want a2", got.Data)
		}
		objs, rev, err := tx.List(ctx, store.ListOptions{Kind: "Tenant"})
		if err != nil || len(objs) != 2 || rev != base+3 {
			t.Errorf("List in the transaction: %d objects at %d, %v; want 2 at %d", len(objs), rev, err, base+3)
		}
		if rev := must[int64](t)(tx.Revision(ctx)); rev != base+3 {
			t.Errorf("the transaction's revision is %d, want %d", rev, base+3)
		}
		// Outside the transaction nothing has happened yet. Get and List do
		// not wait for it, unlike writes and Revision.
		if _, err := s.Get(ctx, key("Tenant", "", "a")); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get outside the transaction before it commits: %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if rev := must[int64](t)(s.Revision(ctx)); rev != base+3 {
		t.Errorf("revision %d after the transaction, want %d", rev, base+3)
	}
	if got := must[store.Object](t)(s.Get(ctx, key("Tenant", "", "a"))); string(got.Data) != "a2" || got.Revision != base+2 {
		t.Errorf("after the commit: %+v", got)
	}
	got := c.waitEvents(t, 3)
	if !slices.Equal(revisions(got), span(base+1, base+3)) {
		t.Errorf("a watch saw %v, want the transaction's three writes once each", describe(got))
	}
	if rev := must[int64](t)(s.Create(ctx, key("Tenant", "", "after"), nil)); rev != base+4 {
		t.Errorf("the write after the transaction is at %d, want %d", rev, base+4)
	}
}

// testTxnRollsBack: a transaction that fails leaves nothing, not even a
// revision, and the store goes on as before.
func testTxnRollsBack(t *testing.T, s store.Store) {
	ctx := t.Context()
	must[int64](t)(s.Create(ctx, key("Tenant", "", "kept"), []byte("v1")))
	c, base := watchFromHere(t, s, store.WatchOptions{})
	failed := errors.New("step failed")
	err := s.Txn(ctx, func(tx store.Tx) error {
		must[int64](t)(tx.Create(ctx, key("Tenant", "", "gone"), nil))
		must[int64](t)(tx.Update(ctx, key("Tenant", "", "kept"), []byte("v2"), 0))
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("Txn returned %v, want the failure of fn", err)
	}
	if _, err := s.Get(ctx, key("Tenant", "", "gone")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an object created in a failed transaction: %v, want ErrNotFound", err)
	}
	if got := must[store.Object](t)(s.Get(ctx, key("Tenant", "", "kept"))); string(got.Data) != "v1" {
		t.Errorf("an object updated in a failed transaction reads %q, want v1", got.Data)
	}
	if rev := must[int64](t)(s.Revision(ctx)); rev != base {
		t.Errorf("a failed transaction moved the revision to %d from %d", rev, base)
	}
	if rev := must[int64](t)(s.Create(ctx, key("Tenant", "", "next"), nil)); rev != base+1 {
		t.Errorf("the write after a failed transaction is at %d, want %d", rev, base+1)
	}
	if got := c.waitEvents(t, 1); !slices.Equal(revisions(got), []int64{base + 1}) || got[0].Object.Name != "next" {
		t.Errorf("a watch saw %v, want only the write after the transaction", describe(got))
	}
}

func testCompaction(t *testing.T, s store.Store) {
	ctx := t.Context()
	for i := range 5 {
		must[int64](t)(s.Create(ctx, key("Tenant", "", fmt.Sprintf("t%d", i)), nil))
	}
	if err := s.Compact(ctx, 3); err != nil {
		t.Fatal(err)
	}
	err := s.Watch(ctx, store.WatchOptions{SinceRevision: 2}, func(store.Event) error { return nil })
	if !errors.Is(err, store.ErrCompacted) {
		t.Errorf("watch below the compaction point: %v, want ErrCompacted", err)
	}
	c := watch(t, s, store.WatchOptions{SinceRevision: 3})
	if got := c.waitEvents(t, 2); !slices.Equal(revisions(got), []int64{4, 5}) {
		t.Errorf("watch at the compaction point saw %v, want 4 5", revisions(got))
	}
	if err := s.Compact(ctx, 99); err == nil {
		t.Error("compacting beyond the current revision succeeded")
	}
	if _, err := s.Get(ctx, key("Tenant", "", "t0")); err != nil {
		t.Errorf("compaction removed an object: %v", err)
	}
}

func testWatchEndsOnClose(t *testing.T, s store.Store) {
	ctx := t.Context()
	since := must[int64](t)(s.Revision(ctx))
	done := make(chan error, 1)
	delivered := make(chan struct{}, 1)
	go func() {
		done <- s.Watch(context.Background(), store.WatchOptions{SinceRevision: since}, func(store.Event) error {
			select {
			case delivered <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	must[int64](t)(s.Create(ctx, key("Tenant", "", "a"), nil))
	select {
	case <-delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("watch never delivered an event")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrClosed) {
			t.Errorf("watch ended with %v when the store closed, want ErrClosed", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not end when the store closed")
	}
	if _, err := s.Get(ctx, key("Tenant", "", "a")); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Get after Close: %v, want ErrClosed", err)
	}
	if _, _, err := s.List(ctx, store.ListOptions{Kind: "Tenant"}); !errors.Is(err, store.ErrClosed) {
		t.Errorf("List after Close: %v, want ErrClosed", err)
	}
	if _, err := s.Create(ctx, key("Tenant", "", "b"), nil); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Create after Close: %v, want ErrClosed", err)
	}
}

// blockedWatch starts a watch whose consumer blocks on its first event until
// release is closed, and records everything it is sent.
type blockedWatch struct {
	release chan struct{}
	done    chan error
	mu      sync.Mutex
	events  []store.Event
}

func startBlockedWatch(t *testing.T, s store.Store, opts store.WatchOptions) *blockedWatch {
	t.Helper()
	w := &blockedWatch{release: make(chan struct{}), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		w.done <- s.Watch(ctx, opts, func(ev store.Event) error {
			w.mu.Lock()
			w.events = append(w.events, ev)
			first := len(w.events) == 1
			w.mu.Unlock()
			if first {
				<-w.release
			}
			return nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-w.done
	})
	return w
}

func (w *blockedWatch) snapshot() []store.Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.events)
}

func (w *blockedWatch) waitEvents(t *testing.T, what string, cond func([]store.Event) bool) []store.Event {
	t.Helper()
	var got []store.Event
	// Thousands of events through SQLite take seconds under the race
	// detector.
	testutil.EventuallyWithin(t, 30*time.Second, what, func() (bool, string) {
		got = w.snapshot()
		return cond(got), fmt.Sprintf("%d events", len(got))
	})
	return got
}

// testWatchProgressNeverAhead checks the promise behind EventProgress: every
// matching event at or below its revision was delivered before it, even when
// progress is asked for while events are still queued for a slow consumer.
func testWatchProgressNeverAhead(t *testing.T, s store.Store) {
	ctx := t.Context()
	progress := make(chan struct{})
	since := must[int64](t)(s.Revision(ctx))
	w := startBlockedWatch(t, s, store.WatchOptions{Kind: "Container", Project: "p1", SinceRevision: since, Progress: progress})

	var matching []int64
	for i := range 60 {
		matching = append(matching, must[int64](t)(s.Create(ctx, key("Container", "p1", fmt.Sprintf("c%d", i)), nil)))
		must[int64](t)(s.Create(ctx, key("Machine", "p1", fmt.Sprintf("m%d", i)), nil))
	}
	last := must[int64](t)(s.Revision(ctx))
	const asks = 20
	go func() {
		for range asks {
			select {
			case progress <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}()
	close(w.release)

	got := w.waitEvents(t, "every progress asked for and every matching event", func(evs []store.Event) bool {
		n := 0
		for _, ev := range evs {
			if ev.Type == store.EventProgress {
				n++
			}
		}
		return n == asks && len(evs) == asks+len(matching)
	})
	delivered := map[int64]bool{}
	for _, ev := range got {
		if ev.Type != store.EventProgress {
			delivered[ev.Revision] = true
			continue
		}
		if ev.Revision > last {
			t.Fatalf("progress reported %d, beyond the last write %d", ev.Revision, last)
		}
		for _, rev := range matching {
			if rev <= ev.Revision && !delivered[rev] {
				t.Fatalf("progress reported %d before delivering the event at %d", ev.Revision, rev)
			}
		}
	}
}

// testListSnapshot checks that List's revision is the one its objects were
// read at, while another goroutine keeps writing.
func testListSnapshot(t *testing.T, s store.Store) {
	ctx := t.Context()
	base := must[int64](t)(s.Revision(ctx))
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.Create(ctx, key("Container", "p", fmt.Sprintf("c%05d", i)), nil); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	defer func() {
		close(stop)
		<-finished
	}()
	for range 200 {
		objs, rev, err := s.List(ctx, store.ListOptions{Kind: "Container"})
		if err != nil {
			t.Fatal(err)
		}
		// Only creates of this kind happen, one revision each, so the snapshot
		// at rev holds exactly the objects created up to it.
		if int64(len(objs)) != rev-base {
			t.Fatalf("List at revision %d returned %d objects, want %d", rev, len(objs), rev-base)
		}
		for _, o := range objs {
			if o.Revision > rev {
				t.Fatalf("List at revision %d returned an object written at %d", rev, o.Revision)
			}
		}
	}
}

// testCompactionOvertakesSlowWatch checks that a watch that falls behind and
// is then overtaken by compaction ends with ErrCompacted instead of skipping
// the events it lost.
func testCompactionOvertakesSlowWatch(t *testing.T, s store.Store) {
	ctx := t.Context()
	since := must[int64](t)(s.Revision(ctx))
	w := startBlockedWatch(t, s, store.WatchOptions{SinceRevision: since})
	const n = 3000
	for i := range n {
		must[int64](t)(s.Create(ctx, key("Container", "p", fmt.Sprintf("c%d", i)), nil))
	}
	w.waitEvents(t, "the first event", func(evs []store.Event) bool { return len(evs) >= 1 })
	if err := s.Compact(ctx, since+n); err != nil {
		t.Fatal(err)
	}
	close(w.release)
	var err error
	select {
	case err = <-w.done:
		w.done <- err
	case <-time.After(30 * time.Second):
		t.Fatal("overtaken watch never ended")
	}
	if !errors.Is(err, store.ErrCompacted) {
		t.Fatalf("overtaken watch ended with %v, want ErrCompacted", err)
	}
	got := revisions(w.snapshot())
	if len(got) == 0 || !slices.Equal(got, span(since+1, since+int64(len(got)))) {
		t.Errorf("before ending, the watch delivered %d events that are not contiguous from %d", len(got), since+1)
	}
}
