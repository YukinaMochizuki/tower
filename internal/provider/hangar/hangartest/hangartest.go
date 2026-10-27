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
	"strings"
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
	"github.com/YukinaMochizuki/tower/internal/guest/vmssh"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// Target is the world's hangar target.
const Target = "h1"

// BuildGuest builds tower-guest into dir and returns its path.
func BuildGuest(dir string) (string, error) { return build(dir, "tower-guest") }

// BuildSystem builds tower-system into dir and returns its path.
func BuildSystem(dir string) (string, error) { return build(dir, "tower-system") }

func build(dir, cmd string) (string, error) {
	path := filepath.Join(dir, cmd)
	b := exec.Command("go", "build", "-o", path, "github.com/YukinaMochizuki/tower/cmd/"+cmd)
	b.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := b.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building %s: %w: %s", cmd, err, out)
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
	// BaseImage is the base image the harness's VMs boot, tower's name for
	// it, which BaseImages maps to a hangar Image; empty runs no VM.
	BaseImage  string
	BaseImages map[string]string
	// Remote is a repository the system container fetches and pushes to,
	// on Branch, at RemoteHead, with a Dockerfile at RemoteDockerfile
	// building from Image; empty skips the mirrors' and builds' tests.
	Remote, Branch, RemoteHead, RemoteDockerfile string
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
	// Config is what Target was made with: another provider of it, as
	// after tower restarts, is made with it again.
	Config hangar.Config
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
	// The target's key for its VMs' root, as tower makes one for each
	// hangar target.
	key, err := vmssh.NewKeyPEM("tower")
	if err != nil {
		t.Fatal(err)
	}
	cfg := hangar.Config{
		Log:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		Target: Target, API: opts.API, Client: w.Env.AsSystem(), Guest: opts.Guest, PullSecret: opts.PullSecret,
		SSHKey: key, BaseImages: opts.BaseImages,
		StreamURL: "https://tower.example/streams/hangar/" + Target + "/", StreamPrefix: "/streams/hangar/" + Target + "/",
	}
	if opts.Configure != nil {
		opts.Configure(&cfg)
	}
	w.Config = cfg
	w.Target = hangar.New(cfg)
	if cfg.SystemImage != "" {
		// The system container made and answering before the suite asks it
		// anything, as a target's is long before a box needs it.
		testutil.EventuallyWithin(t, w.within(), "the system container answering", func() (bool, string) {
			err := w.Target.EnsureSystem(t.Context())
			return err == nil && w.Target.SystemReady(), fmt.Sprint(err)
		})
	}
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
			// The system container stays, as a target's does.
			instances := hangar.LabelTarget + "=" + Target + "," + hangar.LabelProject + "=" + apiservertest.Project
			cts, err := opts.API.Containers().List(cleanup, instances)
			vms, verr := opts.API.VirtualMachines().List(cleanup, instances)
			disks, derr := opts.API.Disks().List(cleanup, instances)
			if err == nil && verr == nil && derr == nil && len(cts) == 0 && len(vms) == 0 && len(disks) == 0 {
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
	skip := map[string]string{}
	if w.opts.Remote == "" {
		why := "the run names no repository the system container reaches"
		for _, name := range []string{"BuildImage", "ResolveImage", "ReadAndBuildAMirror", "MirrorAndCheckout", "PushNeedsItsLease"} {
			skip[name] = why
		}
	}
	return providertest.Harness{
		Provider: w.Target, Target: Target, Image: w.opts.Image, BaseImage: w.opts.BaseImage,
		Remote: w.opts.Remote, Branch: w.opts.Branch, RemoteHead: w.opts.RemoteHead, RemoteDockerfile: w.opts.RemoteDockerfile,
		Golden: func(t *testing.T, art provider.Artifact) string {
			t.Helper()
			key := client.Key{Project: apiservertest.Project, Name: "golden-" + strings.ToLower(art.Name)}
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
		Delete: func(t *testing.T, ref provider.Instance) {
			t.Helper()
			// Its own context: a test's ends before its cleanups run.
			ctx, cancel := context.WithTimeout(context.Background(), w.within()+time.Minute)
			defer cancel()
			key := client.Key{Project: ref.Project, Name: ref.Name}
			o, err := w.Env.AsSystem().Get(ctx, registry.Instance, key)
			if connect.CodeOf(err) == connect.CodeNotFound {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			uid := o.(*envv1alpha1.Instance).GetMetadata().GetUid()
			if _, err := w.Env.AsSystem().Delete(ctx, registry.Instance, key); err != nil && connect.CodeOf(err) != connect.CodeNotFound {
				t.Fatal(err)
			}
			// Its machine's room is the node's again once hangar has deleted
			// it, and its disk.
			sel := hangar.LabelTarget + "=" + Target + "," + hangar.LabelUID + "=" + uid
			testutil.EventuallyWithin(t, w.within(), "instance "+ref.Name+" gone from hangar", func() (bool, string) {
				cts, err := w.opts.API.Containers().List(ctx, sel)
				vms, verr := w.opts.API.VirtualMachines().List(ctx, sel)
				disks, derr := w.opts.API.Disks().List(ctx, sel)
				return err == nil && verr == nil && derr == nil && len(cts)+len(vms)+len(disks) == 0,
					fmt.Sprintf("%d containers, %d VMs, %d disks; %v %v %v", len(cts), len(vms), len(disks), err, verr, derr)
			})
		},
		ProcessLimits: w.Target.SetProcessLimits,
		Skip:          skip,
	}
}
