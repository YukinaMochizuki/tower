// Package resource defines the contract between the generated per-kind
// Connect handlers and the generic machinery that implements every kind. The
// handlers only translate between typed messages and this interface.
package resource

import (
	"context"

	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
)

// Key identifies one object. Project is empty for global kinds.
type Key struct {
	Project string
	Name    string
}

// ListOptions selects the objects a List returns. An empty Project on a
// project-scoped kind means every project the caller can read.
type ListOptions struct {
	Project       string
	LabelSelector string
	PageSize      int32
	PageToken     string
}

// ListResult is one page of objects and the store revision it was read at.
type ListResult struct {
	Items         []proto.Message
	NextPageToken string
	Revision      int64
}

// WatchOptions selects the events a Watch delivers, starting after
// SinceRevision.
type WatchOptions struct {
	Project       string
	LabelSelector string
	SinceRevision int64
}

// Event is one watch event. Object is nil for bookmarks.
type Event struct {
	Type     metav1.EventType
	Object   proto.Message
	Revision int64
}

// ApplyResult is the stored object after an apply.
type ApplyResult struct {
	Object   proto.Message
	Warnings []string
	Created  bool
}

// Backend implements the operations of one resource kind. Objects passed in
// and returned are always that kind's message type.
type Backend interface {
	Get(ctx context.Context, key Key) (proto.Message, error)
	List(ctx context.Context, opts ListOptions) (ListResult, error)
	// Watch calls send for every event until ctx is done or send fails.
	Watch(ctx context.Context, opts WatchOptions, send func(Event) error) error
	Apply(ctx context.Context, obj proto.Message) (ApplyResult, error)
	// Create is Apply that refuses an existing object with AlreadyExists.
	Create(ctx context.Context, obj proto.Message) (ApplyResult, error)
	// Delete returns the object as it was when deletion was accepted.
	Delete(ctx context.Context, key Key, expectedResourceVersion *int64) (proto.Message, error)
	UpdateStatus(ctx context.Context, key Key, status proto.Message, expectedResourceVersion *int64) (proto.Message, error)
}
