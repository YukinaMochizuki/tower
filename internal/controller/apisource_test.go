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

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/testutil"
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

func secret(name string) *secretsv1alpha1.Secret {
	return &secretsv1alpha1.Secret{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name},
		Spec: &secretsv1alpha1.SecretSpec{
			Type: secretsv1alpha1.SecretType_SECRET_TYPE_OPAQUE, Data: map[string][]byte{"TOKEN": []byte("x")},
		},
	}
}

// TestAPISourceFollowsTheAPI: a controller following kinds through the API,
// as one outside the apiserver does, reconciles what exists, what changes,
// and what goes.
func TestAPISourceFollowsTheAPI(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	root := env.As(apiservertest.Admin)
	ctx := t.Context()
	if _, err := root.Apply(ctx, registry.Secret, secret("before")); err != nil {
		t.Fatal(err)
	}

	r := &seen{keys: map[controller.Key]int{}}
	c := &controller.Controller{
		Name: "t", Source: controller.APISource{Client: env.AsLocal()}, Reconciler: r,
		Watches: []controller.Watch{
			{Kind: registry.Secret.Kind, Map: controller.Self},
			// A project changing reconciles a key of its own name.
			{Kind: registry.Project.Kind, Map: func(_ context.Context, o store.Object) []controller.Key {
				var p tenancyv1alpha1.Project
				if err := proto.Unmarshal(o.Data, &p); err != nil {
					t.Errorf("decoding a project: %v", err)
				}
				return []controller.Key{{Name: "project:" + p.GetMetadata().GetName()}}
			}},
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
	testutil.Eventually(t, "an existing secret reconciled", func() bool { return r.count(before) >= 1 })
	testutil.Eventually(t, "its project, a second kind, reconciled", func() bool {
		return r.count(controller.Key{Name: "project:" + apiservertest.Project}) >= 1
	})

	// A secret has no finalizers: making it is a single event, and so is
	// deleting it.
	after := controller.Key{Project: apiservertest.Project, Name: "after"}
	if _, err := root.Apply(ctx, registry.Secret, secret("after")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a new secret reconciled", func() bool { return r.count(after) == 1 })
	if _, err := root.Delete(ctx, registry.Secret, client.Key{Project: after.Project, Name: after.Name}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a deleted secret reconciled", func() bool { return r.count(after) == 2 })
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
		Name: "t", Source: controller.APISource{Client: env.AsLocal()}, Reconciler: r,
		Watches: []controller.Watch{
			{Kind: registry.Secret.Kind, Map: controller.Self},
			{Kind: registry.ServiceToken.Kind, Map: controller.Self},
		},
	}
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	root := env.As(apiservertest.Admin)
	res, err := root.Apply(t.Context(), registry.Secret, secret("web"))
	if err != nil {
		t.Fatal(err)
	}
	written := res.Object.(*secretsv1alpha1.Secret).GetMetadata().GetResourceVersion()
	key := controller.Key{Project: apiservertest.Project, Name: "web"}
	testutil.Eventually(t, "the secret followed", func() (bool, string) {
		st := c.Status()
		if st.Followed < written {
			return false, fmt.Sprintf("%+v, want followed to %d", st, written)
		}
		if st.Queued == 0 && st.InFlight == 0 && r.count(key) == 0 {
			t.Fatalf("followed to %d with the secret written at %d neither queued nor reconciled", st.Followed, written)
		}
		return true, ""
	})

	if _, err := root.Apply(t.Context(), registry.Project, &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "other"}}); err != nil {
		t.Fatal(err)
	}
	rev, err := env.Store.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "followed past a project", func() (bool, string) {
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
	err = controller.APISource{Client: env.AsLocal()}.Watch(ctx, []string{registry.Secret.Kind}, 1, func(store.Event) error { return nil })
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
	root := env.As(apiservertest.Admin)
	since, err := env.Store.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	res, err := root.Apply(t.Context(), registry.Secret, secret("web"))
	if err != nil {
		t.Fatal(err)
	}
	written := res.Object.(*secretsv1alpha1.Secret).GetMetadata().GetResourceVersion()
	h := holdWatches{hold: "/WatchSecrets", release: make(chan struct{}), at: written, passed: make(chan struct{}), once: &sync.Once{}}
	cl := client.New(client.UnixSocket(env.AdminSocket), client.UnixSocketURL, connect.WithInterceptors(h))

	var mu sync.Mutex
	var events []store.Event
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	go func() {
		finished <- controller.APISource{Client: cl}.Watch(ctx, []string{registry.Secret.Kind, registry.ServiceToken.Kind}, since,
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
		sawSecret := false
		for _, ev := range events {
			if ev.Type == store.EventProgress {
				p = ev.Revision
			} else if ev.Object.Name == "web" {
				sawSecret = true
			}
		}
		return p, sawSecret
	}

	select {
	case <-h.passed:
	case <-time.After(testutil.Wait):
		t.Fatal("the service tokens' watch never passed the secret's write")
	}
	if p, _ := followed(); p >= written {
		t.Errorf("progress to %d with the secret written at %d still held back", p, written)
	}
	close(h.release)
	testutil.Eventually(t, "progress past the secret", func() (bool, string) {
		p, saw := followed()
		return p >= written && saw, fmt.Sprintf("progress %d, secret seen %t", p, saw)
	})
}

// TestAWatchEndingEndsThemAll: when the server ends one kind's watch, the
// others end too, so the controller lists everything again rather than
// following some kinds and not others.
func TestAWatchEndingEndsThemAll(t *testing.T) {
	env := apiservertest.Start(t, nil)
	env.Setup()
	cl := client.New(client.UnixSocket(env.AdminSocket), client.UnixSocketURL, connect.WithInterceptors(endWatches{"/WatchSecrets"}))
	rev, err := env.Store.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = controller.APISource{Client: cl}.Watch(ctx, []string{registry.ServiceToken.Kind, registry.Secret.Kind}, rev, func(store.Event) error { return nil })
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
		finished <- controller.APISource{Client: env.AsLocal()}.Watch(ctx, []string{registry.Secret.Kind, registry.Project.Kind}, rev,
			func(ev store.Event) error {
				seen[ev.Object.Kind]++
				if seen[registry.Secret.Kind] > 0 && seen[registry.Project.Kind] > 0 {
					done.Store(true)
				}
				return nil
			})
	}()
	// Two writes, one of each kind, which the watches of both deliver at
	// once.
	if _, err := env.As(apiservertest.Admin).Apply(ctx, registry.Project, &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "other"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.As(apiservertest.Admin).Apply(ctx, registry.Secret, secret("api")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "events of both kinds", done.Load)
	cancel()
	<-finished
}
