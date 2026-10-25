// Package hostdtest runs an apiserver with a hostd target realized by a
// real hostd over a fake host, and the providertest harness over it, for
// hostd's tests and for tower's provider of a hostd target.
package hostdtest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/hostd"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/fake"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
	"github.com/YukinaMochizuki/tower/internal/ticket"
)

// Target is the world's hostd target.
const Target = "t1"

// Remote is a repository the fake host holds, on branch main at c3.
const Remote = "https://git.example.com/conformance.git"

// Image is a container image the fake registry holds.
const Image = fake.Registry + "/base@sha256:00"

// Options adjust a world.
type Options struct {
	// Configure adjusts the apiserver's config.
	Configure func(*apiserver.Config)
	// Host adjusts hostd's config before it runs.
	Host func(*hostd.Config)
	// StatePath keeps the fake host in a file.
	StatePath string
}

// World is an apiserver with the hostd target Target, realized by Host
// over Fake.
type World struct {
	Env  *apiservertest.Env
	Fake *fake.Host
	Host *hostd.Host
	// Secret is the target's ROLE_HOSTD token's; Signer signs tickets
	// Host takes.
	Secret string
	Signer *ticket.Signer
	// Streams serves Host's direct streams.
	Streams string
	// Stop stops hostd.
	Stop func()
}

// New starts a world until the test ends.
func New(t *testing.T, opts Options) *World {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w := &World{Signer: ticket.NewSigner(key)}
	w.Env = apiservertest.Start(t, func(c *apiserver.Config) {
		c.Tickets = w.Signer
		if opts.Configure != nil {
			opts.Configure(c)
		}
	})
	w.Env.Setup()
	if _, err := w.Env.As(apiservertest.Admin).Apply(t.Context(), registry.Target, &targetsv1alpha1.Target{
		Metadata: &metav1.ObjectMeta{Name: Target},
		Spec:     &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hostd{Hostd: &targetsv1alpha1.HostdTarget{}}},
	}); err != nil {
		t.Fatal(err)
	}
	w.Secret = w.Env.CreateToken("hostd-"+Target, iamv1alpha1.Role_ROLE_HOSTD, Target)
	w.start(t, opts)
	return w
}

// start makes the fake host, from its state file if there is one, and runs
// a hostd on it until the test ends or Stop.
func (w *World) start(t *testing.T, opts Options) {
	t.Helper()
	var err error
	if w.Fake, err = fake.New(fake.Options{Path: opts.StatePath, Images: []string{Image}}); err != nil {
		t.Fatal(err)
	}
	w.Fake.SetRemote(Remote, "main", map[string]string{"go.mod": "module x\n"}, "c1", "c2", "c3")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w.Streams = "http://" + ln.Addr().String()
	cfg := hostd.Config{
		Target: Target, Drivers: w.Fake.Drivers(), Client: w.Env.WithSecret(w.Secret),
		TicketKey: w.Signer.PublicKey(), StreamURL: w.Streams,
		ReportEvery: 200 * time.Millisecond, SweepEvery: 200 * time.Millisecond, SweepGrace: time.Hour,
	}
	if opts.Host != nil {
		opts.Host(&cfg)
	}
	w.Host = hostd.New(cfg)
	srv := &http.Server{Handler: w.Host.Streams(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Host.Run(ctx) }()
	stopped := false
	w.Stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("hostd: %v", err)
		}
		_ = srv.Close()
	}
	t.Cleanup(w.Stop)
}

// Restart stops hostd and starts another on the same host, read back from
// its state file, with the same tower.
func (w *World) Restart(t *testing.T, opts Options) {
	t.Helper()
	w.Stop()
	w.start(t, opts)
}

// Tunnel opens the host's tunnel to the apiserver until the test ends,
// and returns what stops it.
func (w *World) Tunnel(t *testing.T) (stop func()) {
	t.Helper()
	agent, err := hostd.Agent(w.Env.URL, w.Secret, w.Host.Handler(), nil)
	if err != nil {
		t.Fatal(err)
	}
	agent.MinBackoff, agent.MaxBackoff = 10*time.Millisecond, 50*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.Run(ctx)
	}()
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	return stop
}

// Instance reads an instance of the world's project.
func (w *World) Instance(t *testing.T, name string) *envv1alpha1.Instance {
	t.Helper()
	o, err := w.Env.AsSystem().Get(t.Context(), registry.Instance, client.Key{Project: apiservertest.Project, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return o.(*envv1alpha1.Instance)
}

// WaitPhase waits until hostd reports the instance in phase.
func (w *World) WaitPhase(t *testing.T, name string, phase envv1alpha1.InstancePhase) *envv1alpha1.Instance {
	t.Helper()
	var in *envv1alpha1.Instance
	testutil.Eventually(t, fmt.Sprintf("instance %s %v", name, phase), func() (bool, string) {
		in = w.Instance(t, name)
		return in.GetStatus().GetPhase() == phase, in.GetStatus().String()
	})
	return in
}

// Apply makes or changes an Instance of the world's project as tower's
// controllers do.
func (w *World) Apply(t *testing.T, name string, spec *envv1alpha1.InstanceSpec) {
	t.Helper()
	if _, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, &envv1alpha1.Instance{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name}, Spec: spec,
	}); err != nil {
		t.Fatal(err)
	}
}

// Harness is the providertest harness of the world, through p, which
// carries hostd's work some way or is hostd itself.
func (w *World) Harness(p provider.Provider) providertest.Harness {
	return providertest.Harness{
		Provider: p, Target: Target, Image: Image, BaseImage: "ubuntu-24.04",
		// hostd starts its processes with them: through a tunnel too.
		ProcessLimits: func(logCap int64, finished int) { w.Host.SetProcessLimits(logCap, finished) },
		Start: func(t *testing.T, name string, spec *envv1alpha1.InstanceSpec) (provider.Instance, *envv1alpha1.InstanceStatus) {
			t.Helper()
			w.Apply(t, name, spec)
			in := w.WaitPhase(t, name, envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
			return provider.Instance{Project: apiservertest.Project, Name: name}, in.GetStatus()
		},
		Stop: func(t *testing.T, ref provider.Instance) {
			t.Helper()
			// hostd writes the instance's status as it goes, which can move
			// it between the read and the apply.
			testutil.Eventually(t, "the instance's run_state set STOPPED", func() (bool, string) {
				in := proto.Clone(w.Instance(t, ref.Name)).(*envv1alpha1.Instance)
				in.Spec.RunState = envv1alpha1.RunState_RUN_STATE_STOPPED
				_, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, in)
				if err != nil && connect.CodeOf(err) != connect.CodeAborted {
					t.Fatal(err)
				}
				return err == nil, fmt.Sprint(err)
			})
			w.WaitPhase(t, ref.Name, envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED)
		},
		Golden: func(t *testing.T, art provider.Artifact) string {
			t.Helper()
			key := client.Key{Project: apiservertest.Project, Name: "golden-" + fmt.Sprint(len(art.Name))}
			if _, err := w.Env.AsSystem().Apply(t.Context(), registry.Golden, &envv1alpha1.Golden{
				Metadata: &metav1.ObjectMeta{Project: key.Project, Name: key.Name},
				Spec: &envv1alpha1.GoldenSpec{Target: Target, InputHash: art.Name,
					Inputs: &envv1alpha1.GoldenSpec_BoxWarm{BoxWarm: &envv1alpha1.BoxWarmInputs{Repository: "r"}}},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Env.AsSystem().UpdateStatus(t.Context(), registry.Golden, key, &envv1alpha1.GoldenStatus{
				Phase: envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY, Artifact: art.Name,
			}, nil); err != nil {
				t.Fatal(err)
			}
			return key.Name
		},
		Remote: Remote, Branch: "main", RemoteHead: "c3",
	}
}
