package controller

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
)

// queue hands out keys in the order they were first added, each at most once
// while it waits. One worker takes them, so a key is never reconciled twice at
// once; a key added while it is being reconciled waits for the next turn. A
// key whose reconcile failed comes back after a delay that doubles with each
// failure in a row, and one that waits after a delay that doubles with each
// wait in a row. A wait is not a failure: it ends a run of failures, as a
// failure ends a run of waits, so a key that waited long fails first after
// the shortest delay. A key has at most one such return pending: the latest
// reconcile decides it, and one that succeeds cancels it.
//
// It also keeps what Status reports, so one lock covers all of it.
type queue struct {
	clock     clockwork.Clock
	base, max time.Duration
	mu        sync.Mutex
	order     []Key
	queued    map[Key]bool
	failures  map[Key]int
	waits     map[Key]int
	wake      chan struct{}
	closed    bool
	// busy counts the reconciles and listings running.
	busy int
	// followed is the revision through which every change to a watched kind
	// has been added.
	followed int64
	// retries are the keys whose last reconcile failed or waited: a delayed
	// add, or one made whose reconcile has not ended.
	retries map[Key]pending
	next    int
}

// pending is a delayed add; id tells it from the one that replaced it.
type pending struct {
	Retry
	timer clockwork.Timer
	id    int
}

func newQueue(clock clockwork.Clock, base, max time.Duration) *queue {
	return &queue{
		clock: clock, base: base, max: max,
		queued: map[Key]bool{}, failures: map[Key]int{}, waits: map[Key]int{}, wake: make(chan struct{}, 1),
		retries: map[Key]pending{},
	}
}

func (q *queue) addAll(keys []Key) {
	for _, k := range keys {
		q.add(k)
	}
}

func (q *queue) add(k Key) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.addLocked(k)
}

func (q *queue) addLocked(k Key) {
	if q.closed || q.queued[k] {
		return
	}
	q.queued[k] = true
	q.order = append(q.order, k)
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// get blocks until a key is ready or ctx is done. The key counts as busy
// until done or wait.
func (q *queue) get(ctx context.Context) (Key, bool) {
	for {
		q.mu.Lock()
		if len(q.order) > 0 && !q.closed {
			k := q.order[0]
			q.order = q.order[1:]
			delete(q.queued, k)
			q.busy++
			q.mu.Unlock()
			return k, true
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return Key{}, false
		case <-q.wake:
		}
	}
}

// done records how a reconcile of k ended. After a failure it schedules a
// retry and returns the delay; after a success it forgets past failures
// and waits.
func (q *queue) done(k Key, failed bool) time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.busy--
	delete(q.waits, k)
	if !failed {
		delete(q.failures, k)
		q.cancel(k)
		return 0
	}
	return q.again(k, q.failures, q.max, false)
}

// wait schedules k again after a reconcile that waits, up to limit later,
// and returns the delay.
func (q *queue) wait(k Key, limit time.Duration) time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.busy--
	delete(q.failures, k)
	return q.again(k, q.waits, limit, true)
}

// again schedules k after a delay that doubles with each count in a row in
// counts, up to limit, and returns the delay. q.mu is held.
func (q *queue) again(k Key, counts map[Key]int, limit time.Duration, waiting bool) time.Duration {
	n := counts[k]
	counts[k] = n + 1
	delay := q.base << min(n, 30)
	if delay > limit || delay <= 0 {
		delay = limit
	}
	attempts := q.retries[k].Attempts + 1
	q.cancel(k)
	id := q.next
	q.next++
	timer := q.clock.AfterFunc(delay, func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		if p, ok := q.retries[k]; !ok || p.id != id {
			return // cancelled while it fired
		}
		// The retry stays listed until the reconcile it brings ends, which
		// replaces it or, succeeding, cancels it.
		q.addLocked(k)
	})
	q.retries[k] = pending{Retry: Retry{Key: k, Due: q.clock.Now().Add(delay), Waiting: waiting, Attempts: attempts}, timer: timer, id: id}
	return delay
}

// cancel drops k's pending add, if any. q.mu is held.
func (q *queue) cancel(k Key) {
	if p, ok := q.retries[k]; ok {
		p.timer.Stop()
		delete(q.retries, k)
	}
}

// start and finish bracket a listing.
func (q *queue) start() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.busy++
}

func (q *queue) finish() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.busy--
}

// follow records that every change to a watched kind through rev has been
// added.
func (q *queue) follow(rev int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.followed = max(q.followed, rev)
}

func (q *queue) status(name string) Status {
	q.mu.Lock()
	defer q.mu.Unlock()
	s := Status{Name: name, Queued: len(q.order), InFlight: q.busy, Followed: q.followed}
	for _, p := range q.retries {
		s.Retries = append(s.Retries, p.Retry)
	}
	slices.SortFunc(s.Retries, func(a, b Retry) int { return a.Due.Compare(b.Due) })
	return s
}

func (q *queue) shutdown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
}
