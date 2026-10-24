package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// APISource follows kinds through the API, as a node agent does, across
// every project the caller can read.
type APISource struct {
	Client *client.Client
}

func (s APISource) List(ctx context.Context, kind string) ([]store.Object, int64, error) {
	r, err := resourceOf(kind)
	if err != nil {
		return nil, 0, err
	}
	items, rev, err := s.Client.ListAll(ctx, r, client.ListOptions{})
	if err != nil {
		return nil, 0, compacted(err)
	}
	objs := make([]store.Object, 0, len(items))
	for _, it := range items {
		o, err := toObject(r, it, object.Meta(it).GetResourceVersion())
		if err != nil {
			return nil, 0, err
		}
		objs = append(objs, o)
	}
	return objs, rev, nil
}

// Watch runs one API watch per kind. Events of one kind arrive in order;
// events of different kinds may interleave in any order, which a level-
// triggered controller does not mind. Progress is the lowest revision any
// kind's watch has reached, by an event or a bookmark, so it comes as often
// as the server's bookmarks do.
func (s APISource) Watch(ctx context.Context, kinds []string, since int64, fn func(store.Event) error) error {
	resources := make([]registry.Resource, 0, len(kinds))
	for _, kind := range kinds {
		r, err := resourceOf(kind)
		if err != nil {
			return err
		}
		resources = append(resources, r)
	}
	g, ctx := errgroup.WithContext(ctx)
	var mu sync.Mutex
	// reached is how far each kind's watch has come; guarded by mu, as is
	// progress, the last progress passed to fn.
	reached := make([]int64, len(resources))
	for i := range reached {
		reached[i] = since
	}
	progress := since
	for i, r := range resources {
		g.Go(func() error {
			err := s.Client.Watch(ctx, r, client.ListOptions{}, since, func(ev client.Event) error {
				var o store.Object
				if ev.Object != nil { // not a bookmark
					var err error
					if o, err = toObject(r, ev.Object, ev.Revision); err != nil {
						return err
					}
				}
				mu.Lock()
				defer mu.Unlock()
				if ev.Object != nil {
					if err := fn(store.Event{Type: eventType(ev), Object: o, Revision: ev.Revision}); err != nil {
						return err
					}
				}
				reached[i] = max(reached[i], ev.Revision)
				if low := slices.Min(reached); low > progress {
					progress = low
					return fn(store.Event{Type: store.EventProgress, Revision: low})
				}
				return nil
			})
			if err == nil {
				// The server ended the watch; the others end with it, and the
				// controller lists again.
				return errWatchEnded
			}
			return compacted(err)
		})
	}
	err := g.Wait()
	if errors.Is(err, errWatchEnded) {
		return nil
	}
	return err
}

var errWatchEnded = errors.New("watch ended")

func eventType(ev client.Event) store.EventType {
	switch ev.Type {
	case metav1.EventType_EVENT_TYPE_ADDED:
		return store.EventCreated
	case metav1.EventType_EVENT_TYPE_DELETED:
		return store.EventDeleted
	}
	return store.EventUpdated
}

func resourceOf(kind string) (registry.Resource, error) {
	for _, r := range registry.All {
		if r.Kind == kind {
			return r, nil
		}
	}
	return registry.Resource{}, fmt.Errorf("no kind %q in the registry", kind)
}

// compacted turns the API's refusal to watch from a compacted revision into
// store.ErrCompacted, which the controller answers by listing again.
func compacted(err error) error {
	if connect.CodeOf(err) == connect.CodeOutOfRange {
		return fmt.Errorf("%w: %w", store.ErrCompacted, err)
	}
	return err
}

// toObject encodes m as the store holds it, without a resource version,
// which the store keeps beside the object.
func toObject(r registry.Resource, m proto.Message, rev int64) (store.Object, error) {
	m = proto.Clone(m)
	meta := object.Meta(m)
	meta.ResourceVersion = 0
	data, err := proto.Marshal(m)
	if err != nil {
		return store.Object{}, err
	}
	return store.Object{
		Key:      store.Key{Kind: r.Kind, Project: meta.GetProject(), Name: meta.GetName()},
		Data:     data,
		Revision: rev,
	}, nil
}
