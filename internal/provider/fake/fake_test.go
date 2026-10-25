package fake_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/fake"
	"github.com/YukinaMochizuki/tower/internal/provider/fake/faketest"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

const target = faketest.Target

// world is an apiserver with a hostd target t1, realized by a fake.
type world struct {
	faketest.World
	env  *apiservertest.Env
	fake *fake.Fake
}

func newWorld(t *testing.T) world {
	t.Helper()
	w := faketest.New(t, nil)
	return world{World: w, env: w.Env, fake: w.Fake}
}

func (w world) waitPhase(t *testing.T, name string, phase envv1alpha1.InstancePhase) *envv1alpha1.Instance {
	t.Helper()
	return w.WaitPhase(t, name, phase)
}

func (w world) harness() providertest.Harness { return w.Harness(w.fake) }

func TestConformance(t *testing.T) {
	providertest.Run(t, newWorld(t).harness())
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

// TestAFollowWaitsForItsProcess: an attach that follows a running process
// waits, and ends with its exit once it ends.
func TestAFollowWaitsForItsProcess(t *testing.T) {
	w := newWorld(t)
	ref, _ := w.harness().Start(t, "follow", &envv1alpha1.InstanceSpec{Target: target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, Image: "img"})
	id, err := w.fake.StartProcess(t.Context(), provider.ProcessRequest{Instance: ref, Command: []string{"sleep"}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan provider.Attached, 1)
	go func() {
		at, _ := w.fake.AttachProcess(t.Context(), ref, id, 0, true, func(provider.OutputChunk) error { return nil })
		done <- at
	}()
	testutil.Eventually(t, "the attach following", func() bool { return w.fake.Following() == 1 })
	if err := w.fake.WriteProcess(ref, id, provider.Stdout, "listening\n"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the attach following again past the output", func() bool { return w.fake.Following() == 1 })
	if err := w.fake.EndProcess(ref, id, 7); err != nil {
		t.Fatal(err)
	}
	if at := <-done; at.Exit == nil || at.Exit.Code != 7 || at.Next != int64(len("listening\n")) {
		t.Errorf("the follow ended %+v", at)
	}
}
