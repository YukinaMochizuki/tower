package controller_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"google.golang.org/protobuf/proto"

	computev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/hangar/internal/client"
	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// seen records the keys a controller reconciled.
type seen struct {
	mu   sync.Mutex
	keys map[controller.Key]int
}

func (s *seen) Reconcile(_ context.Context, k controller.Key) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[k]++
	return nil
}

func (s *seen) count(k controller.Key) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys[k]
}

func container(name string) *computev1alpha1.Container {
	return &computev1alpha1.Container{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name},
		Spec:     &computev1alpha1.ContainerSpec{Image: "nginx"},
	}
}

// TestAPISourceFollowsTheAPI: a controller following kinds through the API,
// as the node agent, reconciles what exists, what changes, and what goes.
func TestAPISourceFollowsTheAPI(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	root := env.As(apiservertest.Root)
	ctx := t.Context()
	if _, err := root.Apply(ctx, registry.Container, container("before")); err != nil {
		t.Fatal(err)
	}

	r := &seen{keys: map[controller.Key]int{}}
	c := &controller.Controller{
		Name: "t", Source: controller.APISource{Client: env.AsNode()}, Reconciler: r,
		Watches: []controller.Watch{
			{Kind: registry.Container.Kind, Map: controller.Self},
			// A machine changing reconciles the container of the same name.
			{Kind: registry.Machine.Kind, Map: func(_ context.Context, o store.Object) []controller.Key {
				var m computev1alpha1.Machine
				if err := proto.Unmarshal(o.Data, &m); err != nil {
					t.Errorf("decoding a machine: %v", err)
				}
				return []controller.Key{{Project: o.Project, Name: "machine:" + m.GetMetadata().GetName()}}
			}},
			{Kind: registry.RoleBinding.Kind, Map: controller.Self},
		},
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	before := controller.Key{Project: apiservertest.Project, Name: "before"}
	testutil.Eventually(t, "an existing container reconciled", func() bool { return r.count(before) >= 1 })
	testutil.Eventually(t, "its machine, a second kind, reconciled", func() bool {
		return r.count(controller.Key{Project: apiservertest.Project, Name: "machine:ct-before"}) >= 1
	})

	after := controller.Key{Project: apiservertest.Project, Name: "after"}
	if _, err := root.Apply(ctx, registry.Container, container("after")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a new container reconciled", func() bool { return r.count(after) >= 1 })

	// A role binding has no finalizers: deleting it is a single event.
	binding := controller.Key{Name: "member-bob"}
	if _, err := root.Apply(ctx, registry.RoleBinding, &iamv1alpha1.RoleBinding{
		Metadata: &metav1.ObjectMeta{Name: binding.Name},
		Spec: &iamv1alpha1.RoleBindingSpec{
			Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: "bob@example.com"}},
			Role:    "platform.member",
			Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a new role binding reconciled", func() bool { return r.count(binding) == 1 })
	if _, err := root.Delete(ctx, registry.RoleBinding, client.Key{Name: binding.Name}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a deleted role binding reconciled", func() bool { return r.count(binding) == 2 })
}

// TestAPISourceFollowsPastOtherKinds: a controller following kinds through
// the API comes, by the bookmarks of each, to the store's revision though
// the last writes are to kinds it does not watch; and when it says it has
// followed a write of a kind it does, that write's key is queued already.
func TestAPISourceFollowsPastOtherKinds(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	r := &seen{keys: map[controller.Key]int{}}
	c := &controller.Controller{
		Name: "t", Source: controller.APISource{Client: env.AsNode()}, Reconciler: r,
		Watches: []controller.Watch{
			{Kind: registry.Container.Kind, Map: controller.Self},
			{Kind: registry.Disk.Kind, Map: controller.Self},
		},
	}
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	root := env.As(apiservertest.Root)
	res, err := root.Apply(t.Context(), registry.Container, container("web"))
	if err != nil {
		t.Fatal(err)
	}
	written := res.Object.(*computev1alpha1.Container).GetMetadata().GetResourceVersion()
	key := controller.Key{Project: apiservertest.Project, Name: "web"}
	testutil.Eventually(t, "the container followed", func() (bool, string) {
		st := c.Status()
		if st.Followed < written {
			return false, fmt.Sprintf("%+v, want followed to %d", st, written)
		}
		if st.Queued == 0 && st.InFlight == 0 && r.count(key) == 0 {
			t.Fatalf("followed to %d with the container written at %d neither queued nor reconciled", st.Followed, written)
		}
		return true, ""
	})

	if _, err := root.Apply(t.Context(), registry.Tenant, &tenancyv1alpha1.Tenant{Metadata: &metav1.ObjectMeta{Name: "other"}}); err != nil {
		t.Fatal(err)
	}
	rev, err := env.Store.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "followed past a tenant", func() (bool, string) {
		st := c.Status()
		return st.Followed >= rev, fmt.Sprintf("%+v, want followed to %d", st, rev)
	})
}

// TestAPISourceReportsCompaction: watching from history the apiserver no
// longer keeps fails as the store does, so the controller lists again.
func TestAPISourceReportsCompaction(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	ctx := t.Context()
	rev, err := env.Store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Store.Compact(ctx, rev); err != nil {
		t.Fatal(err)
	}
	err = controller.APISource{Client: env.AsNode()}.Watch(ctx, []string{registry.Container.Kind}, 1, func(store.Event) error { return nil })
	if !errors.Is(err, store.ErrCompacted) {
		t.Errorf("watching from a compacted revision: %v", err)
	}
}

// endWatches makes every watch of one procedure end at once, as an API
// server ending the stream would.
type endWatches struct{ procedure string }

func (e endWatches) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc { return next }

func (e endWatches) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (e endWatches) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if strings.HasSuffix(spec.Procedure, e.procedure) {
			return ended{conn}
		}
		return conn
	}
}

type ended struct{ connect.StreamingClientConn }

func (ended) Receive(any) error { return io.EOF }

// holdWatches holds back every message of one procedure's watches until
// release is closed, and closes passed once a message of another's at or
// after revision at has been handled.
type holdWatches struct {
	hold    string
	release chan struct{}
	at      int64
	passed  chan struct{}
	once    *sync.Once
}

func (h holdWatches) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc { return next }

func (h holdWatches) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func (h holdWatches) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		return &heldConn{StreamingClientConn: next(ctx, spec), h: h, held: strings.HasSuffix(spec.Procedure, h.hold), ctx: ctx}
	}
}

type heldConn struct {
	connect.StreamingClientConn
	h    holdWatches
	held bool
	ctx  context.Context
	// last is the revision of the last message received.
	last int64
}

func (c *heldConn) Receive(msg any) error {
	if c.held {
		select {
		case <-c.h.release:
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
	} else if c.last >= c.h.at {
		// Asked for the next, the watch is done with the last.
		c.h.once.Do(func() { close(c.h.passed) })
	}
	err := c.StreamingClientConn.Receive(msg)
	if m, ok := msg.(proto.Message); ok && err == nil {
		r := m.ProtoReflect()
		c.last = r.Get(r.Descriptor().Fields().ByName("revision")).Int()
	}
	return err
}

// TestAPISourceProgressWaitsForTheSlowestKind: one kind's watch ahead of
// another's does not make progress claim what the other has yet to deliver.
func TestAPISourceProgressWaitsForTheSlowestKind(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	root := env.As(apiservertest.Root)
	since, err := env.Store.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	res, err := root.Apply(t.Context(), registry.Container, container("web"))
	if err != nil {
		t.Fatal(err)
	}
	written := res.Object.(*computev1alpha1.Container).GetMetadata().GetResourceVersion()
	h := holdWatches{hold: "/WatchContainers", release: make(chan struct{}), at: written, passed: make(chan struct{}), once: &sync.Once{}}
	cl := client.New(client.UnixSocket(env.Socket), client.UnixSocketURL, connect.WithInterceptors(h))

	var mu sync.Mutex
	var events []store.Event
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	go func() {
		finished <- controller.APISource{Client: cl}.Watch(ctx, []string{registry.Container.Kind, registry.Disk.Kind}, since,
			func(ev store.Event) error {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, ev)
				return nil
			})
	}()
	defer func() {
		cancel()
		<-finished
	}()
	followed := func() (int64, bool) {
		mu.Lock()
		defer mu.Unlock()
		var p int64
		sawContainer := false
		for _, ev := range events {
			if ev.Type == store.EventProgress {
				p = ev.Revision
			} else if ev.Object.Name == "web" {
				sawContainer = true
			}
		}
		return p, sawContainer
	}

	select {
	case <-h.passed:
	case <-time.After(testutil.Wait):
		t.Fatal("the disks' watch never passed the container's write")
	}
	if p, _ := followed(); p >= written {
		t.Errorf("progress to %d with the container written at %d still held back", p, written)
	}
	close(h.release)
	testutil.Eventually(t, "progress past the container", func() (bool, string) {
		p, saw := followed()
		return p >= written && saw, fmt.Sprintf("progress %d, container seen %t", p, saw)
	})
}

// TestAWatchEndingEndsThemAll: when the server ends one kind's watch, the
// others end too, so the controller lists everything again rather than
// following some kinds and not others.
func TestAWatchEndingEndsThemAll(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	cl := client.New(client.UnixSocket(env.Socket), client.UnixSocketURL, connect.WithInterceptors(endWatches{"/WatchContainers"}))
	rev, err := env.Store.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = controller.APISource{Client: cl}.Watch(ctx, []string{registry.Machine.Kind, registry.Container.Kind}, rev, func(store.Event) error { return nil })
	if err != nil {
		t.Errorf("a watch the server ended: %v, want nil once every kind's watch ends", err)
	}
}

// TestAPISourceCallsFnOneAtATime: events of different kinds never reach fn
// at the same time, as with a store's single watch.
func TestAPISourceCallsFnOneAtATime(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	rev, err := env.Store.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	seen := map[string]int{} // unsynchronised on purpose: -race checks fn's calls
	var done atomic.Bool
	finished := make(chan error, 1)
	go func() {
		finished <- controller.APISource{Client: env.AsNode()}.Watch(ctx, []string{registry.Container.Kind, registry.Machine.Kind}, rev,
			func(ev store.Event) error {
				seen[ev.Object.Kind]++
				if seen[registry.Container.Kind] > 0 && seen[registry.Machine.Kind] > 0 {
					done.Store(true)
				}
				return nil
			})
	}()
	if _, err := env.As(apiservertest.Root).Apply(ctx, registry.Container, container("api")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "events of both kinds", done.Load)
	cancel()
	<-finished
}
