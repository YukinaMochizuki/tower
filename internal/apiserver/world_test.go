package apiserver_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/fake"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// pruneAfter is the golden prune delay of a world.
const pruneAfter = time.Hour

// controllerMaxWait is how long a controller waits before it checks again
// a key waiting on something outside it (controller.Controller.MaxWait).
const controllerMaxWait = 2 * time.Second

// world is an env with target t1, a hostd target its fake provider
// realizes, and project web placed on it unless unplaced is asked for.
type world struct {
	*env
	fake *fake.Fake
	// secret is t1's ROLE_HOSTD token's.
	secret   string
	stopFake func()
}

type worldOption struct {
	settings *targetsv1alpha1.TargetSettings
	unplaced bool
	config   envOption
}

func newWorld(t *testing.T, opts ...worldOption) *world {
	t.Helper()
	w := &world{}
	var o worldOption
	for _, opt := range opts {
		if opt.settings != nil {
			o.settings = opt.settings
		}
		o.unplaced = o.unplaced || opt.unplaced
		if opt.config != nil {
			o.config = opt.config
		}
	}
	clock := clockwork.NewFakeClock()
	w.fake = fake.New("t1", clock)
	cfg := []envOption{func(c *apiserver.Config) {
		c.Clock = clock
		c.BookmarkClock, c.BookmarkInterval = clockwork.NewRealClock(), 10*time.Millisecond
		c.GoldenPruneAfter = pruneAfter
		c.Providers = func(target string) (provider.Provider, error) {
			if target == "t1" {
				return w.fake, nil
			}
			return nil, nil
		}
	}}
	if o.config != nil {
		cfg = append(cfg, o.config)
	}
	w.env = newEnv(t, cfg...)
	w.setup()
	tg := hostdTarget("t1")
	tg.Spec.Settings = o.settings
	w.mustApply(registry.Target, tg)
	if !o.unplaced {
		w.mustApply(registry.Placement, placement("web", "t1"))
	}
	w.secret = must[iamv1alpha1.CreateServiceTokenResponse](t)(w.local().issuer.CreateServiceToken(t.Context(), createToken("hostd-t1", iamv1alpha1.Role_ROLE_HOSTD, "t1"))).GetSecret()
	w.startFake()
	w.others = append(w.others, w.fake.Status)
	return w
}

// startFake runs the fake against the apiserver serving now.
func (w *world) startFake() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.fake.Run(ctx, w.withToken(w.secret).generic) }()
	var once sync.Once
	w.stopFake = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	w.t.Cleanup(w.stopFake)
}

// restart restarts tower; the target, the fake, goes on as it was.
func (w *world) restart() {
	w.t.Helper()
	w.stopFake()
	w.env.restart()
	w.startFake()
}

func (w *world) mustApply(r registry.Resource, obj proto.Message) {
	w.t.Helper()
	if _, err := w.asSystem().generic.Apply(w.t.Context(), r, obj); err != nil {
		w.t.Fatalf("applying %s: %v", r.Kind, err)
	}
}

func (w *world) get(r registry.Resource, project, name string) (proto.Message, error) {
	return w.asSystem().generic.Get(w.t.Context(), r, client.Key{Project: project, Name: name})
}

func (w *world) box(name string) *envv1alpha1.Box {
	w.t.Helper()
	o, err := w.get(registry.Box, "web", name)
	if err != nil {
		w.t.Fatal(err)
	}
	return o.(*envv1alpha1.Box)
}

func (w *world) machine(name string) *envv1alpha1.Machine {
	w.t.Helper()
	o, err := w.get(registry.Machine, "web", name)
	if err != nil {
		w.t.Fatal(err)
	}
	return o.(*envv1alpha1.Machine)
}

func (w *world) golden(name string) (*envv1alpha1.Golden, bool) {
	w.t.Helper()
	o, err := w.get(registry.Golden, "web", name)
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, false
	}
	if err != nil {
		w.t.Fatal(err)
	}
	return o.(*envv1alpha1.Golden), true
}

func (w *world) instance(name string) (*envv1alpha1.Instance, bool) {
	w.t.Helper()
	o, err := w.get(registry.Instance, "web", name)
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil, false
	}
	if err != nil {
		w.t.Fatal(err)
	}
	return o.(*envv1alpha1.Instance), true
}

// waitBox waits until the box is in phase, and returns it.
func (w *world) waitBox(name string, phase envv1alpha1.BoxPhase) *envv1alpha1.Box {
	w.t.Helper()
	var b *envv1alpha1.Box
	testutil.Eventually(w.t, fmt.Sprintf("box %s %v", name, phase), func() (bool, string) {
		b = w.box(name)
		return b.GetStatus().GetPhase() == phase, b.GetStatus().String()
	})
	return b
}

func (w *world) waitMachine(name string, phase envv1alpha1.MachinePhase) *envv1alpha1.Machine {
	w.t.Helper()
	var m *envv1alpha1.Machine
	testutil.Eventually(w.t, fmt.Sprintf("machine %s %v", name, phase), func() (bool, string) {
		m = w.machine(name)
		return m.GetStatus().GetPhase() == phase, m.GetStatus().String()
	})
	return m
}

// boxes returns the phases a box goes through from now, in order, until
// stop is called.
func (w *world) phases(name string) (stop func() []envv1alpha1.BoxPhase) {
	ctx, cancel := context.WithCancel(w.t.Context())
	rev, err := w.store.Revision(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	ch := make(chan []envv1alpha1.BoxPhase, 1)
	go func() {
		var seen []envv1alpha1.BoxPhase
		_ = w.asSystem().generic.Watch(ctx, registry.Box, client.ListOptions{Project: "web"}, rev, func(ev client.Event) error {
			if b, ok := ev.Object.(*envv1alpha1.Box); ok && b.GetMetadata().GetName() == name {
				if p := b.GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_UNSPECIFIED && (len(seen) == 0 || seen[len(seen)-1] != p) {
					seen = append(seen, p)
				}
			}
			return nil
		})
		ch <- seen
	}()
	return func() []envv1alpha1.BoxPhase {
		cancel()
		return <-ch
	}
}

// settled waits for the controllers, then advances the clock by d and
// waits again, as time passing on the target does.
func (w *world) advance(d time.Duration) {
	w.t.Helper()
	w.waitSettled()
	w.clock.Advance(d)
	w.waitSettled()
}

func idleAfter(d time.Duration) worldOption {
	return worldOption{settings: &targetsv1alpha1.TargetSettings{IdleStopAfter: durationpb.New(d)}}
}

// condition is the condition of the type in list, nil if none.
func condition(list []*metav1.Condition, typ string) *metav1.Condition {
	for _, c := range list {
		if c.GetType() == typ {
			return c
		}
	}
	return nil
}
