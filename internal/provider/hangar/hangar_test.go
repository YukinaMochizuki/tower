package hangar_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"

	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarsecrets "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	hangartenancy "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/fake"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/hangartest"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// towerGuest is tower-guest, built once for the package's tests.
var towerGuest string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tower-guest-")
	if err != nil {
		panic(err)
	}
	if towerGuest, err = hangartest.BuildGuest(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// newWorld is a world over a fake hangar, whose containers have the
// testbed's programs.
func newWorld(t *testing.T, configure func(*hangar.Config)) (*hangartest.World, *fake.Fake) {
	t.Helper()
	guest, err := os.ReadFile(towerGuest)
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(towerGuest, t.TempDir(), clockwork.NewRealClock())
	if f.Bin, err = filepath.Abs("../../../test/testbed/guestbin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	w := hangartest.New(t, hangartest.Options{
		API: f, Project: fake.Project, Guest: guest, Image: "registry.tower.internal/base@sha256:00", Configure: configure,
	})
	return w, f
}

// TestConformance holds the provider over a fake hangar to the contract
// every provider is.
func TestConformance(t *testing.T) {
	w, _ := newWorld(t, nil)
	providertest.Run(t, w.Harness())
}

func container(network string) *envv1alpha1.InstanceSpec {
	return &envv1alpha1.InstanceSpec{
		Target: hangartest.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER,
		Image: "registry.tower.internal/base@sha256:00", Network: network, Alias: "box",
	}
}

func (s strSet) has(name string) bool { return slices.Contains(s, name) }

type strSet []string

// TestWhatAnInstanceIsInHangar: a container instance is a Container on its
// workstream's Network, with a Disk at /workspace and the target's pull
// secret; its Container and Disk go with it, and its Network with the
// sweep once nothing is on it.
func TestWhatAnInstanceIsInHangar(t *testing.T) {
	w, f := newWorld(t, func(c *hangar.Config) {
		c.PullSecret = []byte(`{"auths":{"registry.tower.internal":{"auth":"cmVhZDpvbmx5"}}}`)
		c.SweepGrace = time.Nanosecond
	})
	h := w.Harness()
	ref, _ := h.Start(t, "box", container("ws1"))
	uid := w.Instance(t, "box").GetMetadata().GetUid()
	name := "t-" + uid
	if !strSet(f.ContainerNames()).has(name) || !strSet(f.DiskNames()).has(name) || len(f.NetworkNames()) != 1 {
		t.Fatalf("hangar holds containers %v, disks %v, networks %v", f.ContainerNames(), f.DiskNames(), f.NetworkNames())
	}
	ct, err := f.Containers().Get(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	spec := ct.GetSpec()
	if len(spec.GetVolumes()) != 1 || spec.GetVolumes()[0].GetSource() != name || spec.GetVolumes()[0].GetTarget() != hangar.WorkspaceDir {
		t.Errorf("the container's volumes: %v", spec.GetVolumes())
	}
	if len(spec.GetInterfaces()) != 1 || spec.GetInterfaces()[0].GetNetwork() != f.NetworkNames()[0] {
		t.Errorf("the container's interfaces: %v, networks %v", spec.GetInterfaces(), f.NetworkNames())
	}
	if len(spec.GetImagePullSecrets()) != 1 || f.Secret(spec.GetImagePullSecrets()[0]).GetSpec().GetType() != hangarsecrets.SecretType_SECRET_TYPE_REGISTRY_AUTH {
		t.Errorf("the container pulls with %v, secrets %v", spec.GetImagePullSecrets(), f.SecretNames())
	}

	if _, err := w.Env.AsSystem().Delete(t.Context(), registry.Instance, client.Key{Project: ref.Project, Name: ref.Name}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the instance's container and disk gone", func() (bool, string) {
		return len(f.ContainerNames()) == 0 && len(f.DiskNames()) == 0, fmt.Sprint(f.ContainerNames(), f.DiskNames())
	})
	if err := w.Target.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := f.NetworkNames(); len(n) != 0 {
		t.Errorf("networks %v outlived every container on them", n)
	}
}

// TestTheSweepTakesWhatNoInstanceAccountsFor: what the target made for an
// Instance hangar still holds once it is gone, as when tower was down as
// it went, is deleted by the sweep; what another target made, or anyone
// else, is left alone.
func TestTheSweepTakesWhatNoInstanceAccountsFor(t *testing.T) {
	w, f := newWorld(t, func(c *hangar.Config) { c.SweepGrace = time.Nanosecond })
	ctx := t.Context()
	labels := func(target, uid string) map[string]string {
		return map[string]string{hangar.LabelTarget: target, hangar.LabelUID: uid, hangar.LabelProject: "web", hangar.LabelInstance: "gone"}
	}
	for name, l := range map[string]map[string]string{"t-stray": labels(hangartest.Target, "stray"), "t-theirs": labels("other", "x"), "users": nil} {
		if _, err := f.Disks().Apply(ctx, &hangarstorage.Disk{Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: l}, Spec: &hangarstorage.DiskSpec{Size: "1Gi"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Target.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strSet(f.DiskNames()); got.has("t-stray") || !got.has("t-theirs") || !got.has("users") {
		t.Errorf("after the sweep, disks %v", got)
	}
}

// TestAProjectOutOfQuotaFailsTheInstance: hangar refusing a container for
// its project's quota fails its instance, out of space.
func TestAProjectOutOfQuotaFailsTheInstance(t *testing.T) {
	w, f := newWorld(t, nil)
	none := int32(0)
	f.SetLimits(&hangartenancy.Limits{Machines: &none})
	if _, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, &envv1alpha1.Instance{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: "box"}, Spec: container("ws1"),
	}); err != nil {
		t.Fatal(err)
	}
	in := w.WaitPhase(t, "box", envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED)
	if c := conditions.Find(in.GetStatus().GetConditions(), hangar.InstanceReady); c.GetReason() != "NoSpace" {
		t.Errorf("the instance failed with %v", c)
	}
}

// TestTheTargetSaysWhetherHangarAnswers: the report says the project's
// room from its quota while hangar answers, and that it does not once it
// stops.
func TestTheTargetSaysWhetherHangarAnswers(t *testing.T) {
	w, f := newWorld(t, nil)
	disk := "100Gi"
	f.SetLimits(&hangartenancy.Limits{Disk: &disk})
	ctx := t.Context()
	target := func() *targetsv1alpha1.Target {
		o, err := w.Env.AsSystem().Get(ctx, registry.Target, client.Key{Name: hangartest.Target})
		if err != nil {
			t.Fatal(err)
		}
		return o.(*targetsv1alpha1.Target)
	}
	if err := w.Target.Report(ctx); err != nil {
		t.Fatal(err)
	}
	st := target().GetStatus()
	if !conditions.IsTrue(st.GetConditions(), hangar.TargetReachable) || st.GetCapacity().GetDiskTotal() != "100Gi" || st.GetCapacity().GetDiskFree() != "100Gi" {
		t.Errorf("the target's status while hangar answers: %v", st)
	}
	f.SetDown(connect.NewError(connect.CodeUnavailable, errors.New("hangar is down")))
	if err := w.Target.Report(ctx); err != nil {
		t.Fatal(err)
	}
	if c := conditions.Find(target().GetStatus().GetConditions(), hangar.TargetReachable); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Errorf("Reachable while hangar is down: %v", c)
	}
	f.SetDown(nil)
}

// TestAnImageThatCannotBePulledFailsTheInstance: hangar failing to pull a
// container's image fails its instance, which no retry changes.
func TestAnImageThatCannotBePulledFailsTheInstance(t *testing.T) {
	w, f := newWorld(t, nil)
	spec := container("ws1")
	spec.Image = "registry.tower.internal/missing@sha256:01"
	f.FailPull(spec.Image)
	if _, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, &envv1alpha1.Instance{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: "box"}, Spec: spec,
	}); err != nil {
		t.Fatal(err)
	}
	in := w.WaitPhase(t, "box", envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED)
	if c := conditions.Find(in.GetStatus().GetConditions(), hangar.InstanceReady); c.GetReason() != "PullFailed" || c.GetMessage() == "" {
		t.Errorf("the instance failed with %v", c)
	}
}

// TestAnInstanceDoesNotWaitForAnEarlierOneOfItsName: an Instance made
// again under a name whose earlier one hangar is still deleting runs at
// once; the earlier one's objects go once hangar lets them.
func TestAnInstanceDoesNotWaitForAnEarlierOneOfItsName(t *testing.T) {
	w, f := newWorld(t, func(c *hangar.Config) { c.SweepGrace = time.Nanosecond })
	h := w.Harness()
	ref, _ := h.Start(t, "box", container("ws1"))
	old := "t-" + w.Instance(t, "box").GetMetadata().GetUid()
	f.HoldDeletes()
	if _, err := w.Env.AsSystem().Delete(t.Context(), registry.Instance, client.Key{Project: ref.Project, Name: ref.Name}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the earlier container being deleted", func() (bool, string) {
		ct, err := f.Containers().Get(t.Context(), old)
		return err == nil && ct.GetMetadata().GetDeleteTime() != nil, fmt.Sprint(err)
	})
	h.Start(t, "box", container("ws1"))
	f.ReleaseDeletes()
	testutil.Eventually(t, "the earlier disk gone", func() (bool, string) {
		_ = w.Target.Sweep(t.Context())
		return !strSet(f.DiskNames()).has(old), fmt.Sprint(f.DiskNames())
	})
}

// TestFirstUsesAtOnceWriteTowerGuestInOnce: commands run at once in a
// container no tower-guest is in yet all run, and it is written in once.
func TestFirstUsesAtOnceWriteTowerGuestInOnce(t *testing.T) {
	w, f := newWorld(t, nil)
	ref, _ := w.Harness().Start(t, "box", container("ws1"))
	before := f.Installs()
	errs := make(chan error, 4)
	for range 4 {
		go func() {
			_, err := w.Target.Exec(t.Context(), provider.ExecRequest{Instance: ref, Command: []string{"true"}})
			errs <- err
		}()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if n := f.Installs() - before; n > 1 {
		t.Errorf("tower-guest written in %d times", n)
	}
}
