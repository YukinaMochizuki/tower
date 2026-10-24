// Package store is the object store every resource goes through: opaque
// object bytes keyed by kind, project and name, versioned by a single global
// revision, with a watch that can resume from any revision not yet compacted.
// It knows nothing about protobuf or resource semantics. See
// docs/decisions/sqlite-store.md.
package store

import (
	"context"
	"errors"
)

var (
	// ErrNotFound means no object has the key.
	ErrNotFound = errors.New("store: object not found")
	// ErrExists means Create found an object with the key already.
	ErrExists = errors.New("store: object already exists")
	// ErrConflict means the object's revision did not match the expected one.
	ErrConflict = errors.New("store: revision conflict")
	// ErrCompacted means a watch needed history that compaction removed; the
	// caller must list again and watch from the list's revision.
	ErrCompacted = errors.New("store: revision compacted")
	// ErrClosed is returned by every operation on a closed store.
	ErrClosed = errors.New("store: closed")
)

// Key identifies an object. Project is empty for global kinds.
type Key struct {
	Kind    string
	Project string
	Name    string
}

// Object is a stored object. Revision is the store revision of its last write
// (for a deleted object in an event, the revision of the delete).
type Object struct {
	Key
	Data     []byte
	Revision int64
}

// EventType says what happened to an object.
type EventType int

const (
	// EventCreated reports a new object.
	EventCreated EventType = iota + 1
	// EventUpdated reports a changed object.
	EventUpdated
	// EventDeleted reports a removed object, carrying its last data.
	EventDeleted
	// EventProgress carries no object: every event matching the watch up to
	// Revision has been delivered.
	EventProgress
)

// Event is one change seen by a watch. For EventDeleted, Object holds the
// object's last data.
type Event struct {
	Type     EventType
	Object   Object
	Revision int64
}

// ListOptions selects objects of one kind, ordered by project and name. Kind
// is required. An empty Project matches every project. A zero Limit means no
// limit.
type ListOptions struct {
	Kind       string
	Project    string
	StartAfter Key
	Limit      int
}

// WatchOptions selects the events a watch delivers. An empty Kind matches
// every kind and an empty Project every project. The watch delivers events
// after SinceRevision: 0 replays all history that was never compacted, and
// the current Revision delivers only writes still to come.
type WatchOptions struct {
	Kind          string
	Project       string
	SinceRevision int64
	// Progress asks the watch to deliver an EventProgress, once per receive.
	Progress <-chan struct{}
}

// Tx is what a transaction (Store.Txn) reads and writes through. Its reads
// see what the store holds together with the transaction's own writes; its
// writes take consecutive revisions and are seen by no one else until the
// transaction commits.
type Tx interface {
	Get(ctx context.Context, key Key) (Object, error)
	List(ctx context.Context, opts ListOptions) ([]Object, int64, error)
	Create(ctx context.Context, key Key, data []byte) (int64, error)
	Update(ctx context.Context, key Key, data []byte, expected int64) (int64, error)
	Delete(ctx context.Context, key Key, expected int64) (Object, int64, error)
	// Revision is the revision of the transaction's last write, or the
	// store's when it has written nothing yet.
	Revision(ctx context.Context) (int64, error)
}

// Store is implemented by each storage backend; storetest holds the contract
// every implementation must pass.
//
// Every successful write increments the revision by exactly one, and writes
// commit in revision order. Watchers rely on both: a gap in the revisions they
// see would mean a lost event.
type Store interface {
	// Get returns the object with the key.
	Get(ctx context.Context, key Key) (Object, error)
	// List returns matching objects and the revision they were read at.
	List(ctx context.Context, opts ListOptions) ([]Object, int64, error)
	// Create stores a new object and returns its revision.
	Create(ctx context.Context, key Key, data []byte) (int64, error)
	// Update replaces an existing object. A nonzero expected revision must
	// match the object's current revision.
	Update(ctx context.Context, key Key, data []byte, expected int64) (int64, error)
	// Delete removes an object and returns its last state and the revision of
	// the delete. A nonzero expected revision must match.
	Delete(ctx context.Context, key Key, expected int64) (Object, int64, error)
	// Watch calls send for each matching event, in revision order, until ctx
	// is done or send returns an error. It never skips or repeats an event:
	// a watch that falls so far behind that compaction removes what it still
	// needs ends with ErrCompacted, at the start or at any later point.
	Watch(ctx context.Context, opts WatchOptions, send func(Event) error) error
	// Revision returns the current store revision, once any open
	// transaction has ended.
	Revision(ctx context.Context) (int64, error)
	// Txn runs fn in one transaction: fn's writes through tx all commit if
	// it returns nil, and none otherwise. A write refused with ErrExists,
	// ErrNotFound or ErrConflict changes nothing and the transaction goes
	// on. Every other write, and Revision, waits for it, so fn uses tx,
	// never the Store; reads through the Store do not wait, and do not see
	// the transaction's writes until it commits.
	Txn(ctx context.Context, fn func(tx Tx) error) error
	// Compact discards events at and below the revision, so a watch can no
	// longer start below it.
	Compact(ctx context.Context, revision int64) error
	// Close releases the store. Running watches end with ErrClosed, possibly
	// after delivering events they had already read.
	Close() error
}
