package controller

import (
	"context"
	"slices"

	"github.com/YukinaMochizuki/hangar/internal/store"
)

// Source is what a controller follows: the store, inside the apiserver, or
// the API, from a node. Objects come encoded as the store holds them, with
// no resource version in their metadata (it is the Object's Revision), so
// Map functions read the same fields either way.
type Source interface {
	// List returns every object of kind, and a revision from which a watch
	// misses no change made after the list.
	List(ctx context.Context, kind string) ([]store.Object, int64, error)
	// Watch calls fn for each change to an object of one of kinds after
	// since, until ctx is done or fn or the watch fails. Between changes it
	// may call fn with a store.EventProgress: every change to kinds through
	// its Revision has been passed to fn already. It returns an error
	// wrapping store.ErrCompacted when changes after since are no longer
	// kept, and may return nil when the source ends the watch; either way
	// the controller lists again.
	Watch(ctx context.Context, kinds []string, since int64, fn func(store.Event) error) error
}

// StoreSource follows a store directly.
type StoreSource struct {
	Store store.Store
}

func (s StoreSource) List(ctx context.Context, kind string) ([]store.Object, int64, error) {
	return s.Store.List(ctx, store.ListOptions{Kind: kind})
}

func (s StoreSource) Watch(ctx context.Context, kinds []string, since int64, fn func(store.Event) error) error {
	return s.Store.Watch(ctx, store.WatchOptions{SinceRevision: since}, func(ev store.Event) error {
		if slices.Contains(kinds, ev.Object.Kind) {
			if err := fn(ev); err != nil {
				return err
			}
		}
		// The watch sees every write, of any kind, in revision order.
		return fn(store.Event{Type: store.EventProgress, Revision: ev.Revision})
	})
}
