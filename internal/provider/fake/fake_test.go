package fake_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/fake"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

const target = "t1"

// world is an apiserver with a hostd target t1, realized by a fake.
type world struct {
	env  *apiservertest.Env
	fake *fake.Fake
}

func newWorld(t *testing.T) world {
	t.Helper()
	env := apiservertest.Start(t, nil)
	env.Setup()
	if _, err := env.As(apiservertest.Admin).Apply(t.Context(), registry.Target, &targetsv1alpha1.Target{
		Metadata: &metav1.ObjectMeta{Name: target},
		Spec:     &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hostd{Hostd: &targetsv1alpha1.HostdTarget{}}},
	}); err != nil {
		t.Fatal(err)
	}
	hostd := env.AsToken("hostd-"+target, iamv1alpha1.Role_ROLE_HOSTD, target)
	f := fake.New(target, clockwork.NewRealClock())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, hostd) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return world{env: env, fake: f}
}

func (w world) instance(t *testing.T, name string) *envv1alpha1.Instance {
	t.Helper()
	o, err := w.env.AsSystem().Get(t.Context(), registry.Instance, client.Key{Project: apiservertest.Project, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return o.(*envv1alpha1.Instance)
}

func (w world) waitPhase(t *testing.T, name string, phase envv1alpha1.InstancePhase) *envv1alpha1.Instance {
	t.Helper()
	var in *envv1alpha1.Instance
	testutil.Eventually(t, fmt.Sprintf("instance %s %v", name, phase), func() (bool, string) {
		in = w.instance(t, name)
		return in.GetStatus().GetPhase() == phase, in.GetStatus().String()
	})
	return in
}

func (w world) harness() providertest.Harness {
	return providertest.Harness{
		Provider: w.fake, Target: target, Image: "registry.tower.internal/base@sha256:00", BaseImage: "ubuntu-24.04",
		Start: func(t *testing.T, name string, spec *envv1alpha1.InstanceSpec) (provider.Instance, *envv1alpha1.InstanceStatus) {
			t.Helper()
			if _, err := w.env.AsSystem().Apply(t.Context(), registry.Instance, &envv1alpha1.Instance{
				Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name}, Spec: spec,
			}); err != nil {
				t.Fatal(err)
			}
			in := w.waitPhase(t, name, envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
			return provider.Instance{Project: apiservertest.Project, Name: name}, in.GetStatus()
		},
		Stop: func(t *testing.T, ref provider.Instance) {
			t.Helper()
			in := proto.Clone(w.instance(t, ref.Name)).(*envv1alpha1.Instance)
			in.Spec.RunState = envv1alpha1.RunState_RUN_STATE_STOPPED
			if _, err := w.env.AsSystem().Apply(t.Context(), registry.Instance, in); err != nil {
				t.Fatal(err)
			}
			w.waitPhase(t, ref.Name, envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED)
		},
		Golden: func(t *testing.T, art provider.Artifact) string {
			t.Helper()
			key := client.Key{Project: apiservertest.Project, Name: "golden-" + fmt.Sprint(len(art.Name))}
			if _, err := w.env.AsSystem().Apply(t.Context(), registry.Golden, &envv1alpha1.Golden{
				Metadata: &metav1.ObjectMeta{Project: key.Project, Name: key.Name},
				Spec: &envv1alpha1.GoldenSpec{Target: target, InputHash: art.Name,
					Inputs: &envv1alpha1.GoldenSpec_BoxWarm{BoxWarm: &envv1alpha1.BoxWarmInputs{Repository: "r"}}},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := w.env.AsSystem().UpdateStatus(t.Context(), registry.Golden, key, &envv1alpha1.GoldenStatus{
				Phase: envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY, Artifact: art.Name,
			}, nil); err != nil {
				t.Fatal(err)
			}
			return key.Name
		},
		Remote: "https://git.example.com/conformance.git", Branch: "main", RemoteHead: "c3",
	}
}

func TestConformance(t *testing.T) {
	w := newWorld(t)
	w.fake.SetRemote("https://git.example.com/conformance.git", "main", map[string]string{"go.mod": "module x\n"}, "c1", "c2", "c3")
	providertest.Run(t, w.harness())
}

// TestFaults: each fault the fake injects shows as a provider shows it.
func TestFaults(t *testing.T) {
	w := newWorld(t)
	h := w.harness()
	ctx := t.Context()
	spec := func() *envv1alpha1.InstanceSpec {
		return &envv1alpha1.InstanceSpec{Target: target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, Image: h.Image}
	}
	apply := func(name string) {
		t.Helper()
		if _, err := w.env.AsSystem().Apply(ctx, registry.Instance, &envv1alpha1.Instance{
			Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name}, Spec: spec(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a boot that fails", func(t *testing.T) {
		w.fake.FailBoot("broken", "the kernel panicked")
		apply("broken")
		in := w.waitPhase(t, "broken", envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED)
		if c := in.GetStatus().GetConditions(); len(c) != 1 || c[0].GetReason() != "BootFailed" || c[0].GetMessage() != "the kernel panicked" {
			t.Errorf("conditions %v", c)
		}
		w.fake.FailBoot("broken", "")
		w.waitPhase(t, "broken", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	})
	t.Run("an instance that vanishes", func(t *testing.T) {
		ref, _ := h.Start(t, "lost", spec())
		w.fake.Vanish(ref)
		w.waitPhase(t, "lost", envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE)
		if _, err := w.fake.Exec(ctx, provider.ExecRequest{Instance: ref, Command: []string{"true"}}); !errors.Is(err, provider.ErrNotFound) {
			t.Errorf("a command in a vanished instance: %v", err)
		}
		// Made again, it is a new instance.
		if _, err := w.env.AsSystem().Delete(ctx, registry.Instance, client.Key{Project: ref.Project, Name: ref.Name}); err != nil {
			t.Fatal(err)
		}
		h.Start(t, "lost", spec())
	})
	t.Run("a full disk", func(t *testing.T) {
		var none uint64
		w.fake.SetDiskFree(&none)
		defer w.fake.SetDiskFree(nil)
		apply("full")
		in := w.waitPhase(t, "full", envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED)
		if in.GetStatus().GetConditions()[0].GetReason() != "NoSpace" {
			t.Errorf("conditions %v", in.GetStatus().GetConditions())
		}
		if _, err := w.fake.BuildImage(ctx, provider.BuildRequest{Context: strings.NewReader("x"), Repository: "r"}); !errors.Is(err, provider.ErrNoSpace) {
			t.Errorf("a build on a full disk: %v", err)
		}
	})
	t.Run("failing builds and captures", func(t *testing.T) {
		w.fake.FailBuilds(fmt.Errorf("step 3: %w", provider.ErrFailed))
		if _, err := w.fake.BuildImage(ctx, provider.BuildRequest{Context: strings.NewReader("x"), Repository: "r"}); !errors.Is(err, provider.ErrFailed) {
			t.Errorf("a failing build: %v", err)
		}
		w.fake.FailBuilds(nil)
		ref, _ := h.Start(t, "cap", spec())
		h.Stop(t, ref)
		w.fake.FailCaptures(provider.ErrFailed)
		if _, err := w.fake.Capture(ctx, ref); !errors.Is(err, provider.ErrFailed) {
			t.Errorf("a failing capture: %v", err)
		}
		w.fake.FailCaptures(nil)
	})
	t.Run("a target that stops answering", func(t *testing.T) {
		ref, _ := h.Start(t, "quiet", spec())
		w.fake.SetUnavailable(true)
		if _, err := w.fake.Exec(ctx, provider.ExecRequest{Instance: ref, Command: []string{"true"}}); !errors.Is(err, provider.ErrUnavailable) {
			t.Errorf("a command on a target that does not answer: %v", err)
		}
		if err := w.fake.Report(ctx); err != nil {
			t.Fatal(err)
		}
		tg, err := w.env.AsSystem().Get(ctx, registry.Target, client.Key{Name: target})
		if err != nil {
			t.Fatal(err)
		}
		if tg.(*targetsv1alpha1.Target).GetStatus().GetLastReportTime() != nil {
			t.Error("a target that does not answer reported")
		}
		w.fake.SetUnavailable(false)
		if err := w.fake.Report(ctx); err != nil {
			t.Fatal(err)
		}
		tg, _ = w.env.AsSystem().Get(ctx, registry.Target, client.Key{Name: target})
		st := tg.(*targetsv1alpha1.Target).GetStatus()
		if st.GetLastReportTime() == nil || st.GetCapacity().GetDiskTotal() == "" || len(st.GetFacts()) == 0 {
			t.Errorf("the report: %v", st)
		}
	})
	t.Run("a failing command", func(t *testing.T) {
		ref, _ := h.Start(t, "cmd", spec())
		w.fake.FailCommands("make")
		defer w.fake.FailCommands()
		if code, err := w.fake.Exec(ctx, provider.ExecRequest{Instance: ref, Command: []string{"make", "deps"}}); err != nil || code != 1 {
			t.Errorf("a failing command: %d, %v", code, err)
		}
	})
}
