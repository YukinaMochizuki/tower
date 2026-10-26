// Package hangartest runs an apiserver with a hangar target realized by the
// hangar provider over an API, and the providertest harness over it: a fake
// hangar in CI (internal/provider/hangar), a real one on a testbed
// (test/hangar).
package hangartest

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// Target is the world's hangar target.
const Target = "h1"

// BuildGuest builds tower-guest into dir and returns its path.
func BuildGuest(dir string) (string, error) {
	path := filepath.Join(dir, "tower-guest")
	build := exec.Command("go", "build", "-o", path, "github.com/YukinaMochizuki/tower/cmd/tower-guest")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building tower-guest: %w: %s", err, out)
	}
	return path, nil
}

// Options are a world's.
type Options struct {
	// API is the hangar project; Project its name.
	API     hangar.API
	Project string
	// Guest is tower-guest, statically linked.
	Guest []byte
	// PullSecret is the registry login hangar pulls with, if any.
	PullSecret []byte
	// Image is what the harness's containers run, by digest.
	Image string
	// Configure, if set, adjusts the provider's config.
	Configure func(*hangar.Config)
	// Wait bounds a wait for an instance's phase; zero is testutil's. A
	// real hangar pulls and boots in it.
	Wait time.Duration
}

// World is an apiserver with the hangar target Target, realized by Target.
type World struct {
	Env    *apiservertest.Env
	Target *hangar.Target
	opts   Options
}

// New starts a world until the test ends.
func New(t *testing.T, opts Options) *World {
	t.Helper()
	w := &World{opts: opts}
	w.Env = apiservertest.Start(t, func(c *apiserver.Config) {
		c.Providers = func(string) (provider.Provider, error) { return w.Target, nil }
	})
	w.Env.Setup()
	if _, err := w.Env.As(apiservertest.Admin).Apply(t.Context(), registry.Target, &targetsv1alpha1.Target{
		Metadata: &metav1.ObjectMeta{Name: Target},
		Spec: &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hangar{Hangar: &targetsv1alpha1.HangarTarget{
			Endpoint: "https://hangar.example", Project: opts.Project, CredentialSecret: "hangar-token",
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := hangar.Config{
		Log:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Target: Target, API: opts.API, Client: w.Env.AsSystem(), Guest: opts.Guest, PullSecret: opts.PullSecret,
		StreamURL: "https://tower.example/streams/hangar/" + Target + "/", StreamPrefix: "/streams/hangar/" + Target + "/",
	}
	if opts.Configure != nil {
		opts.Configure(&cfg)
	}
	w.Target = hangar.New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Target.Run(ctx) }()
	t.Cleanup(func() {
		// What the tests made goes from hangar with their Instances.
		cleanup, stop := context.WithCancel(context.Background())
		defer stop()
		items, _, err := w.Env.AsSystem().ListAll(cleanup, registry.Instance, client.ListOptions{})
		if err == nil {
			for _, o := range items {
				in := o.(*envv1alpha1.Instance)
				_, _ = w.Env.AsSystem().Delete(cleanup, registry.Instance, client.Key{Project: in.GetMetadata().GetProject(), Name: in.GetMetadata().GetName()})
			}
		}
		// The provider deletes their containers and disks as it hears of
		// it; on a real hangar, a delete takes until the machine is gone.
		for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); {
			cts, err := opts.API.Containers().List(cleanup, hangar.LabelTarget+"="+Target)
			disks, derr := opts.API.Disks().List(cleanup, hangar.LabelTarget+"="+Target)
			if err == nil && derr == nil && len(cts) == 0 && len(disks) == 0 {
				break
			}
			<-time.After(500 * time.Millisecond)
		}
		if err := w.Target.Sweep(cleanup); err != nil {
			t.Logf("the last sweep: %v", err)
		}
		cancel()
		<-done
	})
	return w
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

// WaitPhase waits until the provider reports the instance in phase.
func (w *World) WaitPhase(t *testing.T, name string, phase envv1alpha1.InstancePhase) *envv1alpha1.Instance {
	t.Helper()
	var in *envv1alpha1.Instance
	testutil.EventuallyWithin(t, w.within(), fmt.Sprintf("instance %s %v", name, phase), func() (bool, string) {
		in = w.Instance(t, name)
		return in.GetStatus().GetPhase() == phase, in.GetStatus().String()
	})
	return in
}

func (w *World) within() time.Duration {
	if w.opts.Wait == 0 {
		return testutil.Wait
	}
	return w.opts.Wait
}

// Harness is the providertest harness of the world.
func (w *World) Harness() providertest.Harness {
	later := "a hangar target does not do it yet"
	return providertest.Harness{
		Provider: w.Target, Target: Target, Image: w.opts.Image,
		Start: func(t *testing.T, name string, spec *envv1alpha1.InstanceSpec) (provider.Instance, *envv1alpha1.InstanceStatus) {
			t.Helper()
			if _, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, &envv1alpha1.Instance{
				Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name}, Spec: spec,
			}); err != nil {
				t.Fatal(err)
			}
			in := w.WaitPhase(t, name, envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
			return provider.Instance{Project: apiservertest.Project, Name: name}, in.GetStatus()
		},
		Stop: func(t *testing.T, ref provider.Instance) {
			t.Helper()
			testutil.EventuallyWithin(t, w.within(), "the instance's run_state set STOPPED", func() (bool, string) {
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
		ProcessLimits: w.Target.SetProcessLimits,
		Skip: map[string]string{
			"BuildImage": later, "ResolveImage": later, "ReadAndBuildAMirror": later, "CaptureAndClone": later,
			"MirrorAndCheckout": later, "PushNeedsItsLease": later,
		},
	}
}
