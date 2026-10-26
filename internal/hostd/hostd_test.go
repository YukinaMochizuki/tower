package hostd_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/hostd"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/fake"
	"github.com/YukinaMochizuki/tower/internal/hostd/hostdtest"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
	"github.com/YukinaMochizuki/tower/internal/ticket"
)

const target = hostdtest.Target

func container(network string) *envv1alpha1.InstanceSpec {
	return &envv1alpha1.InstanceSpec{
		Target: target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, Image: hostdtest.Image,
		Network: network, Alias: "box",
	}
}

// TestAPullHoldsUpNoOtherInstance: an image pulled slowly keeps its own
// instance pending, pulling, while another instance is made meanwhile:
// pulls run apart from the host lock. Once its pull ends, it runs.
func TestAPullHoldsUpNoOtherInstance(t *testing.T) {
	const slow = "registry.tower.internal/web/slow@sha256:5105"
	w := hostdtest.New(t, hostdtest.Options{Images: []string{slow}})
	release := w.Fake.HoldPulls(slow)
	defer release()
	held := container("")
	held.Image = slow
	w.Apply(t, "held", held)
	testutil.Eventually(t, "the held instance pending, pulling", func() (bool, string) {
		c := conditions.Find(w.Instance(t, "held").GetStatus().GetConditions(), hostd.InstanceReady)
		return c.GetReason() == "Pulling", fmt.Sprintf("%v", c)
	})
	w.Apply(t, "other", container(""))
	w.WaitPhase(t, "other", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	if p := w.Instance(t, "held").GetStatus().GetPhase(); p != envv1alpha1.InstancePhase_INSTANCE_PHASE_PENDING {
		t.Errorf("the held instance is %v while its pull is held", p)
	}
	release()
	w.WaitPhase(t, "held", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
}

// TestConformance holds hostd's provider, over the fake host, to the
// provider contract: the seam real drivers plug into.
func TestConformance(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	providertest.Run(t, w.Harness(w.Host))
}

// guestOf is the guest serving the instance, if any.
func guestOf(t *testing.T, w *hostdtest.World, in *envv1alpha1.Instance) (driver.Guest, bool) {
	t.Helper()
	g, err := w.Fake.Drivers().Containers.Inspect(t.Context(), "tower-"+in.GetMetadata().GetUid())
	if errors.Is(err, driver.ErrNotFound) {
		return g, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return g, true
}

func deleteInstance(t *testing.T, w *hostdtest.World, name string) {
	t.Helper()
	if _, err := w.Env.AsSystem().Delete(t.Context(), registry.Instance, client.Key{Project: apiservertest.Project, Name: name}); err != nil {
		t.Fatal(err)
	}
}

// TestAHostdRestartedFindsTheHost: hostd keeps no state of its own; one
// started again on the same host reads it back and leaves what runs as it
// is.
func TestAHostdRestartedFindsTheHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.json")
	w := hostdtest.New(t, hostdtest.Options{StatePath: path})
	w.Apply(t, "kept", container("net-a"))
	before := w.WaitPhase(t, "kept", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	w.Restart(t, hostdtest.Options{StatePath: path})
	w.Env.WaitSettled(w.Host.Status)
	g, ok := guestOf(t, w, before)
	if !ok || !g.Running {
		t.Fatalf("the guest after a restart: %+v, %v", g, ok)
	}
	if !g.StartedAt.Equal(before.GetStatus().GetStartTime().AsTime()) {
		t.Errorf("the guest started again at %v; it ran since %v", g.StartedAt, before.GetStatus().GetStartTime().AsTime())
	}
}

// TestADeletedInstanceLeavesTheHost: what served an Instance goes with it,
// its network too once nothing is on it; another Instance of the name is
// made afresh.
func TestADeletedInstanceLeavesTheHost(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	w.Apply(t, "a", container("net-a"))
	w.Apply(t, "b", container("net-a"))
	a := w.WaitPhase(t, "a", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	w.WaitPhase(t, "b", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	deleteInstance(t, w, "a")
	testutil.Eventually(t, "a's guest and disk gone", func() (bool, string) {
		s := w.Fake.State()
		return len(s.Containers) == 1 && len(s.Disks) == 1, fmt.Sprintf("%+v", s)
	})
	if s := w.Fake.State(); len(s.Networks) != 1 {
		t.Errorf("a network b is on went: %+v", s)
	}
	w.Apply(t, "a", container("net-a"))
	again := w.WaitPhase(t, "a", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	if again.GetMetadata().GetUid() == a.GetMetadata().GetUid() {
		t.Fatal("the instance made again has the uid of the one deleted")
	}
	deleteInstance(t, w, "a")
	deleteInstance(t, w, "b")
	testutil.Eventually(t, "the host empty", func() (bool, string) {
		s := w.Fake.State()
		return s.Empty(), fmt.Sprintf("%+v", s)
	})
}

// denying answers GetInstance PermissionDenied while it is on, as tower
// does to a token being revoked, and counts what it answered.
type denying struct {
	next     http.RoundTripper
	on       atomic.Bool
	answered atomic.Int64
}

func (d *denying) RoundTrip(r *http.Request) (*http.Response, error) {
	if d.on.Load() && strings.HasSuffix(r.URL.Path, "/GetInstance") {
		d.answered.Add(1)
		body := `{"code":"permission_denied","message":"refused"}`
		return &http.Response{
			StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r,
		}, nil
	}
	return d.next.RoundTrip(r)
}

// TestARefusalReleasesNothing: an Instance read refused, rather than not
// found, leaves its guest and disk on the host.
func TestARefusalReleasesNothing(t *testing.T) {
	d := &denying{}
	w := hostdtest.New(t, hostdtest.Options{Transport: func(next http.RoundTripper) http.RoundTripper {
		d.next = next
		return d
	}})
	w.Apply(t, "kept", container("net-a"))
	w.WaitPhase(t, "kept", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	d.on.Store(true)
	// Read again at every resync: a few of them refused.
	testutil.Eventually(t, "instance reads refused", func() bool { return d.answered.Load() >= 5 })
	if s := w.Fake.State(); len(s.Containers) != 1 || len(s.Disks) != 1 || len(s.Networks) != 1 {
		t.Errorf("a refused read released %+v", s)
	}
	d.on.Store(false)
}

// TestTheSweepTakesWhatADownHostdMissed: an Instance deleted while hostd
// is down is swept from the host once hostd is back and the grace has
// passed; what hostd did not make stays.
func TestTheSweepTakesWhatADownHostdMissed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.json")
	w := hostdtest.New(t, hostdtest.Options{StatePath: path})
	w.Apply(t, "missed", container("net-a"))
	w.WaitPhase(t, "missed", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	// Another's, and nobody's.
	d := w.Fake.Drivers()
	for owner, name := range map[string]string{"t2": "theirs", "": "nobodys"} {
		disk, err := d.Storage.EnsureDisk(t.Context(), driver.DiskSpec{Name: name, Kind: driver.Dataset, Labels: driver.Labels{driver.LabelOwner: owner}})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Containers.Create(t.Context(), driver.ContainerSpec{GuestSpec: driver.GuestSpec{Name: name, Disk: disk.Path, Labels: disk.Labels}, Image: hostdtest.Image}); err != nil {
			t.Fatal(err)
		}
	}
	w.Stop()
	deleteInstance(t, w, "missed")

	w.Restart(t, hostdtest.Options{StatePath: path, Host: func(c *hostd.Config) { c.SweepGrace = time.Millisecond }})
	testutil.Eventually(t, "the missed instance swept", func() (bool, string) {
		s, err := fake.ReadState(path)
		if err != nil {
			t.Fatal(err)
		}
		return slices.Equal(s.Containers, []string{"nobodys", "theirs"}) && slices.Equal(s.Disks, []string{"nobodys", "theirs"}) && len(s.Networks) == 0, fmt.Sprintf("%+v", s)
	})
}

// TestTheGraceKeepsWhatIsNew: what nothing accounts for yet stays until
// it is older than the grace.
func TestTheGraceKeepsWhatIsNew(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	d := w.Fake.Drivers()
	if _, err := d.Storage.EnsureDisk(t.Context(), driver.DiskSpec{Name: "new", Kind: driver.Dataset, Labels: driver.Labels{driver.LabelOwner: target, driver.LabelUID: "gone"}}); err != nil {
		t.Fatal(err)
	}
	if err := w.Host.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if disks, _ := d.Storage.Disks(t.Context()); len(disks) != 1 {
		t.Errorf("a sweep within the grace took %+v", disks)
	}
}

// TestGoldensOnTheHost: the sweep takes a golden of this hostd's that no
// Golden names, and leaves one a Golden names and another's; tower cannot
// delete another's.
func TestGoldensOnTheHost(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{Host: func(c *hostd.Config) { c.SweepGrace = time.Millisecond }})
	d := w.Fake.Drivers()
	if _, err := w.Env.AsSystem().Apply(t.Context(), registry.Golden, &envv1alpha1.Golden{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: "named"},
		Spec: &envv1alpha1.GoldenSpec{Target: target, InputHash: "h",
			Inputs: &envv1alpha1.GoldenSpec_BoxWarm{BoxWarm: &envv1alpha1.BoxWarmInputs{Repository: "r"}}},
	}); err != nil {
		t.Fatal(err)
	}
	// tower fails it, as nothing it is built from is recorded; it is made
	// ready once that is said, or the failure would undo it.
	testutil.Eventually(t, "the golden failed", func() (bool, string) {
		o, err := w.Env.AsSystem().Get(t.Context(), registry.Golden, client.Key{Project: apiservertest.Project, Name: "named"})
		if err != nil {
			return false, err.Error()
		}
		phase := o.(*envv1alpha1.Golden).GetStatus().GetPhase()
		return phase == envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED, phase.String()
	})
	if _, err := w.Env.AsSystem().UpdateStatus(t.Context(), registry.Golden, client.Key{Project: apiservertest.Project, Name: "named"},
		&envv1alpha1.GoldenStatus{Phase: envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY, Artifact: "golden-named"}, nil); err != nil {
		t.Fatal(err)
	}
	// Captured from a disk no hostd made, which the sweep leaves.
	if _, err := d.Storage.EnsureDisk(t.Context(), driver.DiskSpec{Name: "src", Kind: driver.Dataset}); err != nil {
		t.Fatal(err)
	}
	for name, owner := range map[string]string{"golden-named": target, "golden-stale": target, "golden-theirs": "t2"} {
		if _, err := d.Storage.Capture(t.Context(), "src", name, driver.Labels{driver.LabelOwner: owner}); err != nil {
			t.Fatal(err)
		}
	}
	listed := func() []string {
		goldens, err := d.Storage.Goldens(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, g := range goldens {
			names = append(names, g.Name)
		}
		return names
	}
	testutil.Eventually(t, "the stale golden swept", func() (bool, string) {
		names := listed()
		return !slices.Contains(names, "golden-stale"), fmt.Sprint(names)
	})
	if err := w.Host.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if names := listed(); !slices.Equal(names, []string{"golden-named", "golden-theirs"}) {
		t.Errorf("the sweep left %v", names)
	}
	if err := w.Host.DeleteArtifact(t.Context(), "golden-theirs"); !errors.Is(err, provider.ErrFailed) {
		t.Errorf("deleting another hostd's golden: %v, want ErrFailed", err)
	}
	if err := w.Host.DeleteArtifact(t.Context(), "golden-named"); err != nil {
		t.Fatal(err)
	}
	if names := listed(); !slices.Equal(names, []string{"golden-theirs"}) {
		t.Errorf("after deleting its own: %v", names)
	}
}

// TestATokenOfAnotherTargetIsRefused: hostd started for a target its token
// is not of stops at once, saying so.
func TestATokenOfAnotherTargetIsRefused(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	h := hostd.New(hostd.Config{Target: "t2", Drivers: w.Fake.Drivers(), Client: w.Env.WithSecret(w.Secret)})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := h.Run(ctx); err == nil || ctx.Err() != nil || !strings.Contains(err.Error(), "ROLE_HOSTD token of that target") {
		t.Errorf("hostd on another target's token: %v", err)
	}
}

// TestAFailedListStopsTheSweep: a list the sweep cannot read removes
// nothing: it never reads as nothing being wanted.
func TestAFailedListStopsTheSweep(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{Host: func(c *hostd.Config) { c.SweepGrace = time.Millisecond }})
	d := w.Fake.Drivers()
	disk, err := d.Storage.EnsureDisk(t.Context(), driver.DiskSpec{Name: "stale", Kind: driver.Dataset, Labels: driver.Labels{driver.LabelOwner: target, driver.LabelUID: "gone"}})
	if err != nil {
		t.Fatal(err)
	}
	w.Fake.Fail("containers.List", errors.New("docker is down"))
	if err := w.Host.Sweep(t.Context()); err == nil || !strings.Contains(err.Error(), "docker is down") {
		t.Errorf("a sweep whose list failed: %v", err)
	}
	if disks, _ := d.Storage.Disks(t.Context()); !slices.ContainsFunc(disks, func(x driver.Disk) bool { return x.Name == disk.Name }) {
		t.Fatal("a sweep whose list failed removed a disk")
	}
	w.Fake.Fail("containers.List", nil)
	testutil.Eventually(t, "the stale disk swept", func() bool {
		disks, _ := d.Storage.Disks(t.Context())
		return len(disks) == 0
	})
}

// TestAnImageNothingNamesIsSwept: an image of tower's registry no
// Instance or Golden of the target names goes once it has gone unnamed for
// the grace; one an instance names stays; what the images hold is
// reported in the target's usage.
func TestAnImageNothingNamesIsSwept(t *testing.T) {
	// The harness sweeps on its own every 200 ms too, and the first sweep
	// to see the image starts its grace: a grace of 1 s outlasts the gap
	// between the pull and this test's first sweep on a slow runner, and
	// ends well within Eventually's bound.
	w := hostdtest.New(t, hostdtest.Options{Host: func(c *hostd.Config) { c.SweepGrace = time.Second }})
	w.Apply(t, "kept", container("net-a"))
	w.WaitPhase(t, "kept", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	const stale = fake.Registry + "/web/box@sha256:5a"
	w.Fake.AddImage(stale)
	d := w.Fake.Drivers()
	if err := d.Images.Pull(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	listed := func(ref string) bool {
		list, _ := d.Images.List(t.Context())
		return slices.ContainsFunc(list, func(i driver.HostImage) bool { return slices.Contains(i.Refs, ref) })
	}
	// Seen unnamed, then kept for the grace.
	if err := w.Host.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !listed(stale) {
		t.Fatal("an image was swept before its grace")
	}
	testutil.Eventually(t, "the image nothing names swept", func() (bool, string) {
		if err := w.Host.Sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
		return !listed(stale), ""
	})
	if !listed(hostdtest.Image) {
		t.Error("the image an instance names was swept")
	}
	if err := w.Host.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	o, err := w.Env.AsSystem().Get(t.Context(), registry.Target, client.Key{Name: target})
	if err != nil {
		t.Fatal(err)
	}
	if got := o.(*targetsv1alpha1.Target).GetStatus().GetUsage().GetImages(); got == "" || got == "0" {
		t.Errorf("the target's images hold %q", got)
	}
}

// TestWhatTheHostDoesIsReported: a guest that does not start, a full pool
// and a golden not ready are reported; a guest the host lost is GONE.
func TestWhatTheHostDoesIsReported(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	ready := func(name string) *metav1.Condition {
		return conditions.Find(w.Instance(t, name).GetStatus().GetConditions(), hostd.InstanceReady)
	}

	w.Apply(t, "a", container(""))
	a := w.WaitPhase(t, "a", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	w.Fake.Lose("container", "tower-"+a.GetMetadata().GetUid())
	w.WaitPhase(t, "a", envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE)

	w.Apply(t, "b", container(""))
	b := w.WaitPhase(t, "b", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	w.Fake.FailStart("tower-"+b.GetMetadata().GetUid(), "no kernel")
	setRunState(t, w, "b", envv1alpha1.RunState_RUN_STATE_STOPPED)
	w.WaitPhase(t, "b", envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED)
	setRunState(t, w, "b", envv1alpha1.RunState_RUN_STATE_RUNNING)
	w.WaitPhase(t, "b", envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED)
	if c := ready("b"); c.GetReason() != "StartFailed" || !strings.Contains(c.GetMessage(), "no kernel") {
		t.Errorf("a guest that does not start: %v", c)
	}
	w.Fake.FailStart("tower-"+b.GetMetadata().GetUid(), "")
	w.WaitPhase(t, "b", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)

	none := uint64(0)
	w.Fake.SetFree(&none)
	w.Apply(t, "c", container(""))
	w.WaitPhase(t, "c", envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED)
	if c := ready("c"); c.GetReason() != "NoSpace" {
		t.Errorf("an instance on a full pool: %v", c)
	}
	w.Fake.SetFree(nil)

	spec := container("")
	spec.DiskGolden = "warm"
	w.Apply(t, "d", spec)
	w.WaitPhase(t, "d", envv1alpha1.InstancePhase_INSTANCE_PHASE_PENDING)
	if c := ready("d"); c.GetReason() != "WaitingForGolden" {
		t.Errorf("an instance whose golden is not ready: %v", c)
	}
}

// setRunState sets an instance's run_state, as tower's controllers do.
func setRunState(t *testing.T, w *hostdtest.World, name string, rs envv1alpha1.RunState) {
	t.Helper()
	testutil.Eventually(t, "the run_state set", func() (bool, string) {
		in := proto.Clone(w.Instance(t, name)).(*envv1alpha1.Instance)
		in.Spec.RunState = rs
		_, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, in)
		if err != nil && connect.CodeOf(err) != connect.CodeAborted {
			t.Fatal(err)
		}
		return err == nil, fmt.Sprint(err)
	})
}

// TestTheTargetIsReported: the target's capacity, its facts and Reporting.
func TestTheTargetIsReported(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	w.Fake.SetHealth("DEGRADED")
	testutil.Eventually(t, "the target reported DEGRADED", func() (bool, string) {
		o, err := w.Env.AsSystem().Get(t.Context(), registry.Target, client.Key{Name: target})
		if err != nil {
			return false, err.Error()
		}
		st := o.(*targetsv1alpha1.Target).GetStatus()
		facts := map[string]string{}
		for _, f := range st.GetFacts() {
			facts[f.GetName()] = f.GetValue()
		}
		ok := facts["zfs health"] == "DEGRADED" && facts["hostd"] == hostd.Version && facts["container driver"] != "" &&
			st.GetCapacity().GetDiskTotal() != "" && st.GetCapacity().GetCpus() > 0 && st.GetLastReportTime() != nil &&
			conditions.IsTrue(st.GetConditions(), hostd.TargetReporting)
		return ok, st.String()
	})
}

// TestUsedDiskIsReadBack: what an instance writes is reported as its
// used_disk at the next read back, with no change to its Instance.
func TestUsedDiskIsReadBack(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	w.Apply(t, "u", container(""))
	if st := w.WaitPhase(t, "u", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING).GetStatus(); st.GetUsedDisk() != "0Gi" {
		t.Fatalf("a fresh instance uses %s", st.GetUsedDisk())
	}
	ref := provider.Instance{Project: apiservertest.Project, Name: "u"}
	if err := w.Host.WriteFile(t.Context(), ref, "/workspace/big", 0o644, 0, strings.NewReader(strings.Repeat("x", 3<<20))); err != nil {
		t.Fatal(err)
	}
	// Quantities of disk are whole Gi, rounded up.
	testutil.Eventually(t, "used_disk read back", func() (bool, string) {
		st := w.Instance(t, "u").GetStatus()
		return st.GetUsedDisk() == "1Gi", st.String()
	})
}

// TestDirectStreams: a stream reaches the instance's port on a ticket for
// it alone; one expired, tampered with, or for another target, instance
// or port is refused.
func TestDirectStreams(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	w.Apply(t, "s", container(""))
	w.WaitPhase(t, "s", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	ref := provider.Instance{Project: apiservertest.Project, Name: "s"}
	where, err := w.Host.Ticket(t.Context(), ref, 8080)
	if err != nil {
		t.Fatal(err)
	}
	claims := ticket.Claims{Target: target, Project: ref.Project, Instance: ref.Name, Port: 8080, Expires: time.Now().Add(time.Minute)}
	issue := func(c ticket.Claims) string {
		tk, err := w.Signer.Issue(c)
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	conn, err := hostd.DialStream(t.Context(), where.URL, issue(claims))
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, "hello\n"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 6)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "hello\n" {
		t.Errorf("the stream echoed %q, %v", got, err)
	}
	_ = conn.Close()

	refused := map[string]string{"no ticket": ""}
	expired := claims
	expired.Expires = time.Now().Add(-time.Second)
	refused["expired"] = issue(expired)
	other := claims
	other.Target = "t2"
	refused["another target's"] = issue(other)
	other = claims
	other.Port = 22
	refused["another port's"] = issue(other)
	other = claims
	other.Instance = "other"
	refused["another instance's"] = issue(other)
	tk := issue(claims)
	// A character inside the signature: the last one's low bits base64
	// ignores.
	i, swap := len(tk)-10, byte('A')
	if tk[i] == swap {
		swap = 'B'
	}
	refused["tampered with"] = tk[:i] + string(swap) + tk[i+1:]
	for what, tk := range refused {
		_, err := hostd.DialStream(t.Context(), where.URL, tk)
		var se *hostd.StreamError
		if !errors.As(err, &se) || (se.Status != http.StatusForbidden && se.Status != http.StatusUnauthorized) {
			t.Errorf("a stream on a ticket %s: %v, want it refused", what, err)
		}
	}
}

// TestOpenStreamsAreReported: streams_open counts the direct streams open
// to an instance, as they open and close.
func TestOpenStreamsAreReported(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	w.Apply(t, "st", container(""))
	w.WaitPhase(t, "st", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	ref := provider.Instance{Project: apiservertest.Project, Name: "st"}
	where, err := w.Host.Ticket(t.Context(), ref, 8080)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := w.Signer.Issue(ticket.Claims{Target: target, Project: ref.Project, Instance: ref.Name, Port: 8080, Expires: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	reported := func(n int32) {
		t.Helper()
		testutil.Eventually(t, fmt.Sprintf("%d streams reported open", n), func() (bool, string) {
			st := w.Instance(t, "st").GetStatus()
			return st.GetStreamsOpen() == n, st.String()
		})
	}
	var conns []io.Closer
	for range 2 {
		conn, err := hostd.DialStream(t.Context(), where.URL, tk)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
	}
	reported(2)
	for _, c := range conns {
		_ = c.Close()
	}
	reported(0)
}

// TestExecEndsWithTheCall: a command runs for as long as its call.
func TestExecEndsWithTheCall(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	w.Apply(t, "e", container(""))
	w.WaitPhase(t, "e", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := w.Host.Exec(ctx, provider.ExecRequest{Instance: provider.Instance{Project: apiservertest.Project, Name: "e"}, Command: []string{"sleep"}})
		done <- err
	}()
	testutil.Eventually(t, "the command running", func() bool { return len(w.Fake.Execs()) > 0 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("a command whose call ended: %v", err)
	}
}
