package hangar_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarnetwork "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	hangarsecrets "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	hangartenancy "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/object"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/fake"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/hangartest"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// towerGuest and towerSystem are tower-guest and tower-system, built once
// for the package's tests.
var towerGuest, towerSystem string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tower-guest-")
	if err != nil {
		panic(err)
	}
	if towerGuest, err = hangartest.BuildGuest(dir); err != nil {
		panic(err)
	}
	if towerSystem, err = hangartest.BuildSystem(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// newWorld is a world over a fake hangar, whose containers have the
// testbed's programs; setup, if given, sets the fake up before the target
// first asks it anything.
func newWorld(t *testing.T, configure func(*hangar.Config), setup ...func(*fake.Fake)) (*hangartest.World, *fake.Fake) {
	t.Helper()
	guest, err := os.ReadFile(towerGuest)
	if err != nil {
		t.Fatal(err)
	}
	f := fake.New(towerGuest, t.TempDir(), clockwork.NewRealClock())
	if f.Bin, err = filepath.Abs("../../../test/testbed/guestbin"); err != nil {
		t.Fatal(err)
	}
	f.System = towerSystem
	f.AddImage("ubuntu-24-04", "3Gi")
	t.Cleanup(f.Close)
	for _, fn := range setup {
		fn(f)
	}
	const image = "registry.tower.internal/base@sha256:00"
	remote, head := gitRemote(t, map[string]string{"go.mod": "module x\n", "docker/Dockerfile": "FROM " + image + "\nCOPY go.mod /go.mod\n"})
	w := hangartest.New(t, hangartest.Options{
		API: f, Project: fake.Project, Guest: guest, Image: image,
		BaseImage: "ubuntu-24.04", BaseImages: map[string]string{"ubuntu-24.04": "ubuntu-24-04"},
		Remote: remote, Branch: "main", RemoteHead: head, RemoteDockerfile: "docker/Dockerfile",
		Configure: func(c *hangar.Config) {
			c.SystemImage = "registry.tower.internal/tower/system@sha256:01"
			// The registry by two names: the builder's and hangar's.
			c.Registry, c.RegistryHost = "registry.tower.internal", "registry.example.com"
			if configure != nil {
				configure(c)
			}
		},
	})
	return w, f
}

// TestConformance holds the provider over a fake hangar to the contract
// every provider is. The mirrors' transfers go over the endpoint, none
// through tower.
func TestConformance(t *testing.T) {
	w, _ := newWorld(t, nil)
	providertest.Run(t, w.Harness())
	if m := w.Target.MirrorTransfers(); m.Relayed != 0 || m.RelayedBytes != 0 || m.Direct == 0 {
		t.Errorf("the mirrors' transfers: %+v, want every one over the endpoint", m)
	}
}

// TestConformanceOverTheRelay holds the provider over a hangar without
// endpoints (hangar#224) to the mirrors' part of the contract: each
// transfer is relayed through tower, and the target says why.
func TestConformanceOverTheRelay(t *testing.T) {
	w, f := newWorld(t, nil, func(f *fake.Fake) { f.WithoutEndpoints() })
	// Said as soon as the system container is made, before any network.
	if err := w.Target.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v := fact(t, w, "mirror transport"); v != "relay: hangar has no EndpointService (hangar#224)" {
		t.Errorf("the target's mirror transport, before any network: %q", v)
	}
	h := w.Harness()
	for _, name := range providertest.Names() {
		if name != "MirrorAndCheckout" && name != "PushNeedsItsLease" {
			h.Skip[name] = "the relay carries the mirrors' transfers alone"
		}
	}
	providertest.Run(t, h)
	if m := w.Target.MirrorTransfers(); m.Direct != 0 || m.Relayed == 0 || m.RelayedBytes == 0 || !strings.Contains(m.LastRelay, "hangar#224") {
		t.Errorf("the mirrors' transfers: %+v, want every one relayed, as hangar has no endpoints", m)
	}
	if n := len(f.EndpointServiceNames()) + len(f.NetworkEndpointNames()) + len(f.SecurityGroupNames()); n != 0 {
		t.Errorf("%d endpoint objects made on a hangar without them", n)
	}
	if err := w.Target.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v := fact(t, w, "mirror transport"); v != "relay: hangar has no EndpointService (hangar#224)" {
		t.Errorf("the target's mirror transport: %q", v)
	}
}

// fact is the value of the target's fact of the name, as last reported.
func fact(t *testing.T, w *hangartest.World, name string) string {
	t.Helper()
	o, err := w.Env.AsSystem().Get(t.Context(), registry.Target, client.Key{Name: hangartest.Target})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range o.(*targetsv1alpha1.Target).GetStatus().GetFacts() {
		if f.GetName() == name {
			return f.GetValue()
		}
	}
	return ""
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
	nets, err := f.Networks().List(t.Context(), hangar.LabelNetwork+"=ws1")
	if err != nil {
		t.Fatal(err)
	}
	if !strSet(f.ContainerNames()).has(name) || !strSet(f.DiskNames()).has(name) || len(nets) != 1 {
		t.Fatalf("hangar holds containers %v, disks %v, networks %v", f.ContainerNames(), f.DiskNames(), f.NetworkNames())
	}
	network := nets[0].GetMetadata().GetName()
	ct, err := f.Containers().Get(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	spec := ct.GetSpec()
	if len(spec.GetVolumes()) != 1 || spec.GetVolumes()[0].GetSource() != name || spec.GetVolumes()[0].GetTarget() != hangar.WorkspaceDir {
		t.Errorf("the container's volumes: %v", spec.GetVolumes())
	}
	if len(spec.GetInterfaces()) != 1 || spec.GetInterfaces()[0].GetNetwork() != network {
		t.Errorf("the container's interfaces: %v, networks %v", spec.GetInterfaces(), f.NetworkNames())
	}
	if len(spec.GetImagePullSecrets()) != 1 || f.Secret(spec.GetImagePullSecrets()[0]).GetSpec().GetType() != hangarsecrets.SecretType_SECRET_TYPE_REGISTRY_AUTH {
		t.Errorf("the container pulls with %v, secrets %v", spec.GetImagePullSecrets(), f.SecretNames())
	}

	if _, err := w.Env.AsSystem().Delete(t.Context(), registry.Instance, client.Key{Project: ref.Project, Name: ref.Name}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the instance's container and disk gone", func() (bool, string) {
		return !strSet(f.ContainerNames()).has(name) && !strSet(f.DiskNames()).has(name), fmt.Sprint(f.ContainerNames(), f.DiskNames())
	})
	neps, err := f.NetworkEndpoints().List(t.Context(), hangar.LabelHangarNetwork+"="+network)
	if err != nil || len(neps) != 1 || neps[0].GetSpec().GetService() != systemName(f) {
		t.Fatalf("network %s's endpoints: %v, %v; want one, to the system container", network, neps, err)
	}
	if err := w.Target.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := f.NetworkNames(); strSet(n).has(network) {
		t.Errorf("network %s outlived every container on it", network)
	}
	if n := f.NetworkEndpointNames(); strSet(n).has(neps[0].GetMetadata().GetName()) {
		t.Errorf("network %s's endpoint outlived it", network)
	}
	if !strSet(f.ContainerNames()).has(systemName(f)) {
		t.Errorf("the sweep took the system container: %v", f.ContainerNames())
	}
}

// TestAContainersDisksGrowAsItsInstanceSays: an instance with disk_growth
// is a Disk that grows from resources.disk by threshold and step, and a
// Container whose root disk grows from its start by root_threshold and
// root_step, each max at least its start; one written before the root had
// its own grows both as threshold and step say; one without has disks that
// do not grow, the root disk hangar's default.
func TestAContainersDisksGrowAsItsInstanceSays(t *testing.T) {
	w, f := newWorld(t, nil)
	h := w.Harness()
	grows := container("ws1")
	grows.Resources = &envv1alpha1.Resources{Cpu: "2", Memory: "4Gi", Disk: "40Gi"}
	grows.DiskGrowth = &envv1alpha1.DiskGrowth{
		WorkspaceMax: "30Gi", Root: "5Gi", RootMax: "30Gi", Threshold: "80%", Step: "4Gi", RootThreshold: "90%", RootStep: "2Gi",
	}
	h.Start(t, "grows", grows)
	before := container("ws3")
	before.Resources = &envv1alpha1.Resources{Cpu: "2", Memory: "4Gi", Disk: "40Gi"}
	before.DiskGrowth = &envv1alpha1.DiskGrowth{WorkspaceMax: "30Gi", Root: "5Gi", RootMax: "30Gi", Threshold: "80%", Step: "2x"}
	h.Start(t, "before", before)
	h.Start(t, "fixed", container("ws2"))
	for name, want := range map[string]struct {
		disk      string
		grow      *hangarstorage.DiskGrow
		resources *hangarcompute.Resources
	}{
		"grows": {
			disk: "40Gi", grow: &hangarstorage.DiskGrow{Max: "40Gi", Threshold: "80%", Step: "4Gi"},
			resources: &hangarcompute.Resources{Cpu: "2", Memory: "4Gi", Disk: "5Gi", DiskGrow: &hangarcompute.DiskGrow{Max: "30Gi", Threshold: "90%", Step: "2Gi"}},
		},
		// As it was before root_threshold and root_step were.
		"before": {
			disk: "40Gi", grow: &hangarstorage.DiskGrow{Max: "40Gi", Threshold: "80%", Step: "2x"},
			resources: &hangarcompute.Resources{Cpu: "2", Memory: "4Gi", Disk: "5Gi", DiskGrow: &hangarcompute.DiskGrow{Max: "30Gi", Threshold: "80%", Step: "2x"}},
		},
		"fixed": {disk: hangar.DefaultDisk},
	} {
		obj := "t-" + w.Instance(t, name).GetMetadata().GetUid()
		d, err := f.Disks().Get(t.Context(), obj)
		if err != nil {
			t.Fatal(err)
		}
		if d.GetSpec().GetSize() != want.disk || !proto.Equal(d.GetSpec().GetGrow(), want.grow) {
			t.Errorf("%s: its disk %v, want %s growing as %v", name, d.GetSpec(), want.disk, want.grow)
		}
		ct, err := f.Containers().Get(t.Context(), obj)
		if err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(ct.GetSpec().GetResources(), want.resources) {
			t.Errorf("%s: its container's resources %v, want %v", name, ct.GetSpec().GetResources(), want.resources)
		}
		// Read back as it was applied, it is not applied again.
		if err := w.Target.Converge(t.Context(), w.Instance(t, name)); err != nil {
			t.Fatal(err)
		}
		again, err := f.Containers().Get(t.Context(), obj)
		if err != nil {
			t.Fatal(err)
		}
		if g := again.GetMetadata().GetGeneration(); g != ct.GetMetadata().GetGeneration() {
			t.Errorf("%s: its container was applied again on a converge, generation %d to %d", name, ct.GetMetadata().GetGeneration(), g)
		}
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
		// A network endpoint whose network is gone.
		if _, err := f.NetworkEndpoints().Apply(ctx, &hangarnetwork.NetworkEndpoint{Metadata: &hangarmeta.ObjectMeta{Name: "ep-" + name, Labels: l},
			Spec: &hangarnetwork.NetworkEndpointSpec{Network: "w-gone", Service: systemName(f)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Target.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strSet(f.DiskNames()); got.has("t-stray") || !got.has("t-theirs") || !got.has("users") {
		t.Errorf("after the sweep, disks %v", got)
	}
	if got := strSet(f.NetworkEndpointNames()); got.has("ep-t-stray") || !got.has("ep-t-theirs") || !got.has("ep-users") {
		t.Errorf("after the sweep, network endpoints %v", got)
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
// room from its quota while hangar answers (what the system container's
// 4Gi disk leaves of 100Gi), and that it does not once it stops.
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
	if !conditions.IsTrue(st.GetConditions(), hangar.TargetReachable) || st.GetCapacity().GetDiskTotal() != "100Gi" || st.GetCapacity().GetDiskFree() != "96Gi" {
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

// TestAnInstanceWaitsForRoomOnItsNode: a stopped instance started again on
// a node with too little memory free (hangar's StartDoesNotFit), and a new
// one no node has the memory for (Unschedulable, naming it), are PENDING,
// their Ready WaitingForRoom with hangar's words, and run once there is
// room; a cordoned node is no wait for room.
func TestAnInstanceWaitsForRoomOnItsNode(t *testing.T) {
	w, f := newWorld(t, nil)
	h := w.Harness()
	ref, _ := h.Start(t, "box", container("ws1"))
	h.Stop(t, ref)
	f.FillNode("512Mi")
	setRunState(t, w, "box", envv1alpha1.RunState_RUN_STATE_RUNNING)
	waitReady(t, w, "box", provider.ReasonWaitingForRoom, "node n1 has 512Mi of memory free for this machine, not 1Gi; it starts once there is room")
	if _, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, &envv1alpha1.Instance{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: "new"}, Spec: container("ws2"),
	}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, w, "new", provider.ReasonWaitingForRoom, "no ready node has 1Gi of memory free")
	f.FreeRoom()
	w.WaitPhase(t, "box", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	w.WaitPhase(t, "new", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)

	// What only looks like it, of an older generation or not for room, is
	// not a wait for room.
	for _, tt := range []struct {
		name       string
		reason     string
		msg        string
		generation int64
	}{
		{"older", "StartDoesNotFit", "node n1 has 0Mi of memory free for this machine, not 1Gi; it starts once there is room", 1},
		{"cordoned", "Unschedulable", "every ready node is cordoned", 2},
	} {
		ct := &hangarcompute.Container{
			Metadata: &hangarmeta.ObjectMeta{Generation: 2},
			Spec:     &hangarcompute.ContainerSpec{RunState: hangarcompute.ContainerRunState_CONTAINER_RUN_STATE_RUNNING},
			Status: &hangarcompute.ContainerStatus{Conditions: []*hangarmeta.Condition{{
				Type: "Ready", Status: hangarmeta.ConditionStatus_CONDITION_STATUS_FALSE, Reason: tt.reason, Message: tt.msg, ObservedGeneration: tt.generation,
			}}},
		}
		if hangar.WaitsForRoom(ct) {
			t.Errorf("%s: %s %q is taken for a wait for room", tt.name, tt.reason, tt.msg)
		}
	}
}

// setRunState sets the instance's run_state.
func setRunState(t *testing.T, w *hangartest.World, name string, rs envv1alpha1.RunState) {
	t.Helper()
	testutil.Eventually(t, "the instance's run_state set "+rs.String(), func() (bool, string) {
		in := proto.Clone(w.Instance(t, name)).(*envv1alpha1.Instance)
		in.Spec.RunState = rs
		_, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, in)
		if err != nil && connect.CodeOf(err) != connect.CodeAborted {
			t.Fatal(err)
		}
		return err == nil, fmt.Sprint(err)
	})
}

// waitReady waits until the instance is PENDING, its Ready of reason and
// message.
func waitReady(t *testing.T, w *hangartest.World, name, reason, msg string) {
	t.Helper()
	testutil.Eventually(t, fmt.Sprintf("instance %s %s", name, reason), func() (bool, string) {
		in := w.Instance(t, name)
		c := conditions.Find(in.GetStatus().GetConditions(), hangar.InstanceReady)
		return in.GetStatus().GetPhase() == envv1alpha1.InstancePhase_INSTANCE_PHASE_PENDING && c.GetReason() == reason && c.GetMessage() == msg, in.GetStatus().String()
	})
}

// TestAnInstanceWaitsOnlyForTheAliasOfAnEarlierOneOfItsName: an Instance
// made again under a name whose earlier one hangar is still deleting
// waits only for its alias, which hangar holds until the earlier one is
// gone, saying so, and runs then; the earlier one's objects go once hangar
// lets them.
func TestAnInstanceWaitsOnlyForTheAliasOfAnEarlierOneOfItsName(t *testing.T) {
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
	// Its objects are named apart, but its alias is the earlier one's,
	// which hangar holds until the earlier container is gone: it waits for
	// it, saying so, and runs once it is.
	if _, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, &envv1alpha1.Instance{
		Metadata: &metav1.ObjectMeta{Project: ref.Project, Name: ref.Name}, Spec: container("ws1"),
	}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the new instance waiting for its alias", func() (bool, string) {
		in := w.Instance(t, "box")
		c := conditions.Find(in.GetStatus().GetConditions(), hangar.InstanceReady)
		return in.GetStatus().GetPhase() == envv1alpha1.InstancePhase_INSTANCE_PHASE_PENDING && c.GetReason() == "AliasHeld", in.GetStatus().String()
	})
	f.ReleaseDeletes()
	w.WaitPhase(t, "box", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
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

// TestACommandAsRootRunsAsRootInAContainer: a command asked to run as
// root runs tower-guest as root in a container; any other as the
// container's user.
func TestACommandAsRootRunsAsRootInAContainer(t *testing.T) {
	w, f := newWorld(t, nil)
	ref, _ := w.Harness().Start(t, "box", container("ws1"))
	for _, asRoot := range []bool{false, true} {
		arg := fmt.Sprint(asRoot)
		if _, err := w.Target.Exec(t.Context(), provider.ExecRequest{Instance: ref, Command: []string{"true", arg}, AsRoot: asRoot}); err != nil {
			t.Fatal(err)
		}
		want := ""
		if asRoot {
			want = "0"
		}
		i := slices.IndexFunc(f.ExecsAs(), func(e fake.ExecAs) bool { return slices.Equal(e.Command, []string{"true", arg}) })
		if i < 0 || f.ExecsAs()[i].User != want {
			t.Errorf("as root %v: hangar ran %v; want the shim as user %q", asRoot, f.ExecsAs(), want)
		}
	}
}

// gitRemote makes a bare repository of files, two commits on main, and
// returns its path and its head.
func gitRemote(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	bare, work := filepath.Join(dir, "remote.git"), filepath.Join(dir, "work")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(dir, "init", "--quiet", "--bare", "--initial-branch=main", bare)
	run(dir, "init", "--quiet", "--initial-branch=main", work)
	run(work, "commit", "--quiet", "--allow-empty", "-m", "first")
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run(work, "add", "-A")
	run(work, "commit", "--quiet", "-m", "second")
	run(work, "push", "--quiet", bare, "main")
	return bare, run(work, "rev-parse", "HEAD")
}

// systemName is the fake's system container, the one named tower-sys-*.
func systemName(f *fake.Fake) string {
	for _, n := range f.ContainerNames() {
		if strings.HasPrefix(n, "tower-sys-") {
			return n
		}
	}
	return "(none)"
}

// TestWhatTheTargetBuildsIsNamedAsHangarPullsIt: an image the system
// container pushes to the registry by its own name comes back by the name
// hangar pulls it by, and reads back by that name; the system container's
// own image is named so too.
func TestWhatTheTargetBuildsIsNamedAsHangarPullsIt(t *testing.T) {
	w, f := newWorld(t, nil)
	ctx := t.Context()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	df := "FROM registry.example.com/base@sha256:00\nUSER 7\n"
	_ = tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(df))})
	_, _ = tw.Write([]byte(df))
	_ = tw.Close()
	image, err := w.Target.BuildImage(ctx, provider.BuildRequest{Context: &b, Repository: "web/box"})
	if err != nil || !strings.HasPrefix(image, "registry.example.com/web/box@sha256:") {
		t.Fatalf("built %q, %v; want it by registry.example.com", image, err)
	}
	if cfg, err := w.Target.ResolveImage(ctx, image); err != nil || cfg.User != "7" {
		t.Errorf("resolved %s as %+v, %v", image, cfg, err)
	}
	ct, err := f.Containers().Get(ctx, systemName(f))
	if err != nil || ct.GetSpec().GetImage() != "registry.example.com/tower/system@sha256:01" {
		t.Errorf("the system container runs %q, %v", ct.GetSpec().GetImage(), err)
	}
}

// TestTheSweepKeepsWhatAGoldenNeeds: a golden's image is kept while a
// Golden names it, or while the instance it is captured from is there,
// and goes once neither holds.
func TestTheSweepKeepsWhatAGoldenNeeds(t *testing.T) {
	w, f := newWorld(t, func(c *hangar.Config) { c.SweepGrace = time.Nanosecond })
	ctx := t.Context()
	h := w.Harness()
	src, _ := h.Start(t, "src", container("ws1"))
	h.Stop(t, src)
	art, err := w.Target.Capture(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	// Captured, its Golden not yet written: its instance holds it.
	if err := w.Target.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CustomImages().Get(ctx, art.Name); err != nil {
		t.Fatalf("the sweep took an image under capture: %v", err)
	}
	h.Golden(t, art)
	if _, err := w.Env.AsSystem().Delete(ctx, registry.Instance, client.Key{Project: src.Project, Name: src.Name}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the source's container gone", func() (bool, string) {
		return !strSet(f.ContainerNames()).has("t-" + strings.TrimPrefix(art.Name, "g-")), fmt.Sprint(f.ContainerNames())
	})
	if err := w.Target.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := f.CustomImages().Get(ctx, art.Name); err != nil {
		t.Fatalf("the sweep took an image a Golden names: %v", err)
	}
	goldens, _, err := w.Env.AsSystem().ListAll(ctx, registry.Golden, client.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range goldens {
		_, _ = w.Env.AsSystem().Delete(ctx, registry.Golden, client.Key{Project: object.Meta(g).GetProject(), Name: object.Meta(g).GetName()})
	}
	testutil.Eventually(t, "the image swept once nothing names it", func() (bool, string) {
		if err := w.Target.Sweep(ctx); err != nil {
			return false, err.Error()
		}
		_, err := f.CustomImages().Get(ctx, art.Name)
		return errors.Is(err, hangar.ErrNotFound), fmt.Sprint(err)
	})
	if !strSet(f.DiskNames()).has(systemName(f)) || !strSet(f.NetworkNames()).has(systemName(f)) {
		t.Errorf("the sweep took the system container's disk or network: %v %v", f.DiskNames(), f.NetworkNames())
	}
}
