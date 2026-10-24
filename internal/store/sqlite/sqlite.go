// Package sqlite implements store.Store on SQLite through modernc.org/sqlite,
// which needs no cgo. Writes go through one connection and are serialized
// in-process; reads use a pool of connections reading WAL snapshots.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sync"
	"sync/atomic"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite/db"
)

//go:embed migrations/*.sql
var migrations embed.FS

const (
	// subscriberBuffer is how many live events a watch may fall behind before
	// it switches to reading the events table to catch up.
	subscriberBuffer = 1024
	replayBatch      = 500
	// idleReaders is how many read connections stay open between reads.
	// Every controller reads at once; with database/sql's default of two,
	// the rest are closed as each read ends and opened again for the next,
	// and opening one runs the pragmas and parses the schema.
	idleReaders = 16
)

// Store is a store.Store backed by one SQLite database file.
type Store struct {
	// rdb serves reads. wdb has a single connection that starts every
	// transaction with a write lock: a transaction that reads before it
	// writes cannot then fail to upgrade because another process (such as a
	// backup tool) wrote in between.
	rdb, wdb *sql.DB
	q        *db.Queries
	// closing is set before the databases close, so errors from operations
	// racing Close can be reported as store.ErrClosed.
	closing atomic.Bool

	// mu serializes writes so that revision order, commit order and the order
	// events reach subscribers are the same. It also guards the fields below.
	mu        sync.Mutex
	rev       int64
	compacted int64
	subs      map[*subscriber]struct{}
	closed    bool
}

var _ store.Store = (*Store)(nil)

type subscriber struct {
	// ch is closed when the subscriber falls behind or the store closes.
	ch chan store.Event
}

// Open opens or creates the database at path and applies pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := func(txlock string) string {
		q := url.Values{}
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "busy_timeout(5000)")
		q.Add("_pragma", "synchronous(FULL)")
		q.Set("_txlock", txlock)
		return "file:" + path + "?" + q.Encode()
	}
	wdb, err := sql.Open("sqlite", dsn("immediate"))
	if err != nil {
		return nil, err
	}
	wdb.SetMaxOpenConns(1)
	rdb, err := sql.Open("sqlite", dsn("deferred"))
	if err != nil {
		_ = wdb.Close()
		return nil, err
	}
	rdb.SetMaxIdleConns(idleReaders)
	s, err := open(ctx, rdb, wdb)
	if err != nil {
		_ = rdb.Close()
		_ = wdb.Close()
		return nil, err
	}
	return s, nil
}

func open(ctx context.Context, rdb, wdb *sql.DB) (*Store, error) {
	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, wdb, dir)
	if err != nil {
		return nil, err
	}
	if _, err := provider.Up(ctx); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	s := &Store{rdb: rdb, wdb: wdb, q: db.New(rdb), subs: map[*subscriber]struct{}{}}
	if s.rev, err = s.q.GetMeta(ctx, "revision"); err != nil {
		return nil, err
	}
	if s.compacted, err = s.q.GetMeta(ctx, "compacted"); err != nil {
		return nil, err
	}
	return s, nil
}

// closedOr reports store.ErrClosed for an error caused by a concurrent Close,
// and err otherwise.
func (s *Store) closedOr(err error) error {
	if err != nil && s.closing.Load() {
		return store.ErrClosed
	}
	return err
}

func (s *Store) Get(ctx context.Context, key store.Key) (store.Object, error) {
	if s.closing.Load() {
		return store.Object{}, store.ErrClosed
	}
	row, err := s.q.GetObject(ctx, db.GetObjectParams{Kind: key.Kind, Project: key.Project, Name: key.Name})
	if errors.Is(err, sql.ErrNoRows) {
		return store.Object{}, store.ErrNotFound
	}
	if err != nil {
		return store.Object{}, s.closedOr(err)
	}
	return objectFromRow(row), nil
}

func (s *Store) List(ctx context.Context, opts store.ListOptions) ([]store.Object, int64, error) {
	if s.closing.Load() {
		return nil, 0, store.ErrClosed
	}
	objs, rev, err := s.list(ctx, opts)
	return objs, rev, s.closedOr(err)
}

func (s *Store) list(ctx context.Context, opts store.ListOptions) ([]store.Object, int64, error) {
	tx, err := s.rdb.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)
	// Both reads share the transaction's snapshot, so the revision is exactly
	// the one the objects were read at.
	rev, err := q.GetMeta(ctx, "revision")
	if err != nil {
		return nil, 0, err
	}
	objs, err := listObjects(ctx, q, opts)
	return objs, rev, err
}

func listObjects(ctx context.Context, q *db.Queries, opts store.ListOptions) ([]store.Object, error) {
	limit := int64(opts.Limit)
	if limit <= 0 {
		limit = -1
	}
	rows, err := q.ListObjects(ctx, db.ListObjectsParams{
		Kind:         opts.Kind,
		Project:      opts.Project,
		AfterProject: opts.StartAfter.Project,
		AfterName:    opts.StartAfter.Name,
		MaxRows:      limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]store.Object, 0, len(rows))
	for _, r := range rows {
		out = append(out, objectFromRow(r))
	}
	return out, nil
}

func (s *Store) Create(ctx context.Context, key store.Key, data []byte) (int64, error) {
	return s.write(ctx, func(t *txn) (int64, error) { return t.Create(ctx, key, data) })
}

func (s *Store) Update(ctx context.Context, key store.Key, data []byte, expected int64) (int64, error) {
	return s.write(ctx, func(t *txn) (int64, error) { return t.Update(ctx, key, data, expected) })
}

func (s *Store) Delete(ctx context.Context, key store.Key, expected int64) (store.Object, int64, error) {
	var last store.Object
	rev, err := s.write(ctx, func(t *txn) (int64, error) {
		var rev int64
		var err error
		last, rev, err = t.Delete(ctx, key, expected)
		return rev, err
	})
	return last, rev, err
}

// write runs one write as a transaction of its own.
func (s *Store) write(ctx context.Context, fn func(t *txn) (int64, error)) (int64, error) {
	var rev int64
	err := s.txn(ctx, func(t *txn) error {
		var err error
		rev, err = fn(t)
		return err
	})
	return rev, err
}

func (s *Store) Txn(ctx context.Context, fn func(tx store.Tx) error) error {
	return s.txn(ctx, func(t *txn) error { return fn(t) })
}

// txn runs fn in a write transaction, under s.mu from start to end, so the
// revisions it takes are the next ones, it commits in revision order, and
// its events reach live watches in order once it has committed.
func (s *Store) txn(ctx context.Context, fn func(t *txn) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return store.ErrClosed
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	t := &txn{q: s.q.WithTx(tx), rev: s.rev}
	if err := fn(t); err != nil {
		return err
	}
	if t.broken != nil {
		return t.broken
	}
	if len(t.events) == 0 {
		return nil
	}
	if err := t.q.SetMeta(ctx, db.SetMetaParams{Key: "revision", Value: t.rev}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.rev = t.rev
	for _, ev := range t.events {
		s.publish(ev)
	}
	return nil
}

// txn is a write transaction: the store.Tx of Store.Txn.
type txn struct {
	q *db.Queries
	// rev is the revision of the last write made in it, or the store's.
	rev int64
	// events are its writes' events, published once it commits.
	events []store.Event
	// broken is the failure that ended it, if one did (write).
	broken error
}

var _ store.Tx = (*txn)(nil)

func (t *txn) Get(ctx context.Context, key store.Key) (store.Object, error) {
	row, err := t.q.GetObject(ctx, getParams(key))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Object{}, store.ErrNotFound
	}
	if err != nil {
		return store.Object{}, err
	}
	return objectFromRow(row), nil
}

func (t *txn) List(ctx context.Context, opts store.ListOptions) ([]store.Object, int64, error) {
	objs, err := listObjects(ctx, t.q, opts)
	return objs, t.rev, err
}

func (t *txn) Revision(context.Context) (int64, error) { return t.rev, nil }

func (t *txn) Create(ctx context.Context, key store.Key, data []byte) (int64, error) {
	data = nonNil(data)
	return t.write(ctx, func(q *db.Queries, rev int64) (store.Event, error) {
		if _, err := q.GetObject(ctx, getParams(key)); err == nil {
			return store.Event{}, store.ErrExists
		} else if !errors.Is(err, sql.ErrNoRows) {
			return store.Event{}, err
		}
		err := q.InsertObject(ctx, db.InsertObjectParams{
			Kind: key.Kind, Project: key.Project, Name: key.Name, Revision: rev, Data: data,
		})
		return store.Event{Type: store.EventCreated, Object: store.Object{Key: key, Data: data, Revision: rev}}, err
	})
}

func (t *txn) Update(ctx context.Context, key store.Key, data []byte, expected int64) (int64, error) {
	data = nonNil(data)
	return t.write(ctx, func(q *db.Queries, rev int64) (store.Event, error) {
		if _, err := current(ctx, q, key, expected); err != nil {
			return store.Event{}, err
		}
		err := q.UpdateObject(ctx, db.UpdateObjectParams{
			Revision: rev, Data: data, Kind: key.Kind, Project: key.Project, Name: key.Name,
		})
		return store.Event{Type: store.EventUpdated, Object: store.Object{Key: key, Data: data, Revision: rev}}, err
	})
}

func (t *txn) Delete(ctx context.Context, key store.Key, expected int64) (store.Object, int64, error) {
	var last store.Object
	rev, err := t.write(ctx, func(q *db.Queries, rev int64) (store.Event, error) {
		cur, err := current(ctx, q, key, expected)
		if err != nil {
			return store.Event{}, err
		}
		last = cur
		err = q.DeleteObject(ctx, db.DeleteObjectParams{Kind: key.Kind, Project: key.Project, Name: key.Name})
		return store.Event{Type: store.EventDeleted, Object: store.Object{Key: key, Data: nonNil(cur.Data), Revision: rev}}, err
	})
	return last, rev, err
}

// current reads the object an update or delete applies to and checks the
// expected revision.
func current(ctx context.Context, q *db.Queries, key store.Key, expected int64) (store.Object, error) {
	row, err := q.GetObject(ctx, getParams(key))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Object{}, store.ErrNotFound
	}
	if err != nil {
		return store.Object{}, err
	}
	if expected != 0 && row.Revision != expected {
		return store.Object{}, store.ErrConflict
	}
	return objectFromRow(row), nil
}

// write runs one mutation as the transaction's next revision and records its
// event. A write fn refuses with a store error has written nothing; any other
// failure may follow a write, so it fails the rest of the transaction.
func (t *txn) write(ctx context.Context, fn func(q *db.Queries, rev int64) (store.Event, error)) (int64, error) {
	if t.broken != nil {
		return 0, t.broken
	}
	rev := t.rev + 1
	ev, err := fn(t.q, rev)
	if errors.Is(err, store.ErrExists) || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
		return 0, err
	}
	if err != nil {
		t.broken = err
		return 0, err
	}
	ev.Revision = rev
	if err := t.q.InsertEvent(ctx, db.InsertEventParams{
		Revision: rev, Type: int64(ev.Type),
		Kind: ev.Object.Kind, Project: ev.Object.Project, Name: ev.Object.Name, Data: ev.Object.Data,
	}); err != nil {
		t.broken = err
		return 0, err
	}
	t.rev = rev
	t.events = append(t.events, ev)
	return rev, nil
}

// publish hands an event to every live watch without blocking. A watch whose
// buffer is full loses its subscription and catches up from the events table.
// Callers hold s.mu.
func (s *Store) publish(ev store.Event) {
	for sub := range s.subs {
		select {
		case sub.ch <- ev:
		default:
			close(sub.ch)
			delete(s.subs, sub)
		}
	}
}

func (s *Store) Revision(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, store.ErrClosed
	}
	return s.rev, nil
}

func (s *Store) Compact(ctx context.Context, revision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return store.ErrClosed
	}
	if revision > s.rev {
		return fmt.Errorf("compact to %d: beyond current revision %d", revision, s.rev)
	}
	if revision <= s.compacted {
		return nil
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)
	if err := q.DeleteEventsThrough(ctx, revision); err != nil {
		return err
	}
	if err := q.SetMeta(ctx, db.SetMetaParams{Key: "compacted", Value: revision}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.compacted = revision
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.closing.Store(true)
	for sub := range s.subs {
		close(sub.ch)
		delete(s.subs, sub)
	}
	s.mu.Unlock()
	return errors.Join(s.rdb.Close(), s.wdb.Close())
}

func (s *Store) Watch(ctx context.Context, opts store.WatchOptions, send func(store.Event) error) error {
	if opts.SinceRevision < 0 {
		return fmt.Errorf("watch from revision %d: must not be negative", opts.SinceRevision)
	}
	last := opts.SinceRevision
	for {
		sub, cur, err := s.subscribe()
		if err != nil {
			return err
		}
		if last > cur {
			s.unsubscribe(sub)
			return fmt.Errorf("watch from revision %d: beyond current revision %d", last, cur)
		}
		// Every write after cur reaches sub, so replaying (last, cur] from the
		// table and then draining sub delivers each event exactly once.
		if err := s.replay(ctx, opts, last, cur, send); err != nil {
			s.unsubscribe(sub)
			return s.closedOr(err)
		}
		last = cur
		resubscribe, err := s.live(ctx, opts, sub, &last, send)
		if !resubscribe {
			s.unsubscribe(sub)
			return err
		}
	}
}

func (s *Store) subscribe() (*subscriber, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, 0, store.ErrClosed
	}
	sub := &subscriber{ch: make(chan store.Event, subscriberBuffer)}
	s.subs[sub] = struct{}{}
	return sub, s.rev, nil
}

func (s *Store) unsubscribe(sub *subscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[sub]; ok {
		delete(s.subs, sub)
		close(sub.ch)
	}
}

// replay sends the matching events in (after, through] from the events table.
// Each batch is read in its own short transaction, so a slow consumer never
// holds a snapshot open; that would keep SQLite from checkpointing the WAL.
func (s *Store) replay(ctx context.Context, opts store.WatchOptions, after, through int64, send func(store.Event) error) error {
	for after < through {
		rows, err := s.replayBatch(ctx, opts, after, through)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := send(eventFromRow(r)); err != nil {
				return err
			}
		}
		if len(rows) < replayBatch {
			return nil
		}
		after = rows[len(rows)-1].Revision
	}
	return nil
}

// replayBatch reads the next batch of events after after. The compaction mark
// is read in the same snapshot as the events: compaction only removes events
// at or below the mark, so if after is not below it, nothing in the batch was
// removed.
func (s *Store) replayBatch(ctx context.Context, opts store.WatchOptions, after, through int64) ([]db.Event, error) {
	tx, err := s.rdb.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)
	compacted, err := q.GetMeta(ctx, "compacted")
	if err != nil {
		return nil, err
	}
	if after < compacted {
		return nil, store.ErrCompacted
	}
	return q.ListEvents(ctx, db.ListEventsParams{
		After: after, Through: through, Kind: opts.Kind, Project: opts.Project, MaxRows: replayBatch,
	})
}

// live delivers events from sub until ctx ends or send fails. It reports
// resubscribe when sub fell behind, so the caller catches up from *last.
func (s *Store) live(ctx context.Context, opts store.WatchOptions, sub *subscriber, last *int64, send func(store.Event) error) (resubscribe bool, err error) {
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-opts.Progress:
			if err := send(store.Event{Type: store.EventProgress, Revision: *last}); err != nil {
				return false, err
			}
		case ev, ok := <-sub.ch:
			if !ok {
				return true, nil
			}
			*last = ev.Revision
			if matches(opts, ev.Object.Key) {
				if err := send(ev); err != nil {
					return false, err
				}
			}
		}
	}
}

func matches(opts store.WatchOptions, key store.Key) bool {
	return (opts.Kind == "" || opts.Kind == key.Kind) && (opts.Project == "" || opts.Project == key.Project)
}

// nonNil turns nil into an empty slice: the driver stores nil as NULL, an
// object whose fields are all defaults marshals to nil, and an empty blob
// reads back as nil.
func nonNil(data []byte) []byte {
	if data == nil {
		return []byte{}
	}
	return data
}

func getParams(key store.Key) db.GetObjectParams {
	return db.GetObjectParams{Kind: key.Kind, Project: key.Project, Name: key.Name}
}

func objectFromRow(r db.Object) store.Object {
	return store.Object{Key: store.Key{Kind: r.Kind, Project: r.Project, Name: r.Name}, Data: r.Data, Revision: r.Revision}
}

func eventFromRow(r db.Event) store.Event {
	return store.Event{
		Type:     store.EventType(r.Type),
		Object:   store.Object{Key: store.Key{Kind: r.Kind, Project: r.Project, Name: r.Name}, Data: r.Data, Revision: r.Revision},
		Revision: r.Revision,
	}
}
