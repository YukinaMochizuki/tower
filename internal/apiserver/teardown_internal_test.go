package apiserver

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/jonboulle/clockwork"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/devcontainer"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/fake"
)

// onADeletingTarget runs a server with no controller, so nothing tears
// down its target t1, which is being deleted: a test calls the reconciles
// as the controllers would. asked counts the calls for t1's provider.
func onADeletingTarget(t *testing.T) (s *Server, system context.Context, asked *atomic.Int32) {
	t.Helper()
	p := fake.New("t1", clockwork.NewRealClock())
	asked = &atomic.Int32{}
	s, system, admin := runningWith(t, func(c *Config) {
		c.DisableControllers = true
		c.Resolver = devcontainer.Fake{}
		c.Providers = func(string) (provider.Provider, error) {
			asked.Add(1)
			return p, nil
		}
	})
	// What a job started would queue its key on.
	s.goldenCtrl = &controller.Controller{}
	if _, err := s.backend(registry.Target).Apply(admin, &targetsv1alpha1.Target{
		Metadata: &metav1.ObjectMeta{Name: "t1"},
		Spec:     &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hostd{Hostd: &targetsv1alpha1.HostdTarget{}}},
	}); err != nil {
		t.Fatal(err)
	}
	// Its teardown finalizer holds it, being deleted.
	if _, err := s.backend(registry.Target).Delete(admin, resource.Key{Name: "t1"}, nil); err != nil {
		t.Fatal(err)
	}
	return s, system, asked
}

// TestABoxMakesNoGoldenOnATargetBeingDeleted: a box that fell back from
// its devcontainer's golden, gone since, makes no golden again on a
// target being deleted.
func TestABoxMakesNoGoldenOnATargetBeingDeleted(t *testing.T) {
	t.Parallel()
	s, system, asked := onADeletingTarget(t)
	bs := &boxState{s: s, st: &envv1alpha1.BoxStatus{
		Target: "t1", DevcontainerGolden: "img-gone",
		Conditions: []*metav1.Condition{{Type: BoxDevcontainerFellBack, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Rebuilding"}},
	}}
	box := &envv1alpha1.Box{Metadata: &metav1.ObjectMeta{Project: "web", Name: "b1"}, Spec: &envv1alpha1.BoxSpec{Image: "ubuntu:24.04"}}
	if err := s.followDevcontainerGolden(system, controller.Key{Project: "web", Name: "b1"}, box, bs); err != nil {
		t.Fatal(err)
	}
	goldens, err := listTyped[*envv1alpha1.Golden](system, s, registry.Golden, "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(goldens) > 0 {
		t.Errorf("the box made golden %s on a target being deleted", goldens[0].GetMetadata().GetName())
	}
	if n := asked.Load(); n > 0 {
		t.Errorf("the provider of a target being deleted was asked %d times", n)
	}
}

// TestNoBuildStartsOnATargetBeingDeleted: a BUILDING golden no job builds
// starts no build on a target being deleted.
func TestNoBuildStartsOnATargetBeingDeleted(t *testing.T) {
	t.Parallel()
	s, system, asked := onADeletingTarget(t)
	key := resource.Key{Project: "web", Name: "g1"}
	if _, err := s.backend(registry.Golden).Apply(system, &envv1alpha1.Golden{
		Metadata: &metav1.ObjectMeta{Project: key.Project, Name: key.Name},
		Spec: &envv1alpha1.GoldenSpec{Target: "t1", InputHash: "g1",
			Inputs: &envv1alpha1.GoldenSpec_BoxImage{BoxImage: &envv1alpha1.BoxImageInputs{BaseImageDigest: "sha256:0"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.backend(registry.Golden).UpdateStatus(system, key, &envv1alpha1.GoldenStatus{Phase: envv1alpha1.GoldenPhase_GOLDEN_PHASE_BUILDING}, nil); err != nil {
		t.Fatal(err)
	}
	// What it is built from, as ensureGolden records it.
	if err := s.putRecord(system, goldenBuildKey(key.Project, key.Name), buildSpec{Image: &devcontainer.Request{Target: "t1", Image: "ubuntu:24.04"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcileGolden(system, controller.Key{Project: key.Project, Name: key.Name}); err != nil {
		t.Fatal(err)
	}
	if _, running := s.jobs.lastError("golden/web/g1"); running {
		t.Error("a build started on a target being deleted")
	}
	if n := asked.Load(); n > 0 {
		t.Errorf("the provider of a target being deleted was asked %d times", n)
	}
}

// TestAFinalizerOfAnotherUIDStays: removing a finalizer from the object of
// a uid leaves another object of the name, as a Target made again since
// it was read, as it is.
func TestAFinalizerOfAnotherUIDStays(t *testing.T) {
	t.Parallel()
	s, system, _ := onADeletingTarget(t)
	key := resource.Key{Name: "t1"}
	if err := s.removeFinalizerOf(system, registry.Target, key, "another-uid", TargetTeardownFinalizer); !errors.Is(err, errOtherUID) {
		t.Errorf("removing the finalizer of another uid: %v", err)
	}
	if tg, ok, err := getTyped[*targetsv1alpha1.Target](system, s, registry.Target, key); err != nil || !ok || !slices.Contains(tg.GetMetadata().GetFinalizers(), TargetTeardownFinalizer) {
		t.Errorf("the target after removing another's finalizer: %v, %v, %v", tg.GetMetadata(), ok, err)
	}
}
