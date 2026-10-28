package apiserver_test

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/devcontainer"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/fake"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// openBoxes makes n ready boxes of repo, named after it.
func (w *world) openBoxes(repo string, n int) {
	w.t.Helper()
	for i := range n {
		w.readyBox(fmt.Sprintf("%s-%d", repo, i), func(b *envv1alpha1.Box) { b.Spec.Checkouts[0].Repository = repo })
	}
}

// records counts tower's internal records of kind.
func (w *world) records(kind string) int {
	w.t.Helper()
	objs, _, err := w.store.List(w.t.Context(), store.ListOptions{Kind: kind})
	if err != nil {
		w.t.Fatal(err)
	}
	return len(objs)
}

// goldensEverMadeFor are the warm goldens of repo the store's history
// holds, gone or not.
func (w *world) goldensEverMadeFor(repo string) []string {
	w.t.Helper()
	rev, err := w.store.Revision(w.t.Context())
	if err != nil {
		w.t.Fatal(err)
	}
	var names []string
	// Every kind's events, so the watch reaches the store's revision.
	err = w.store.Watch(w.t.Context(), store.WatchOptions{SinceRevision: 0}, func(ev store.Event) error {
		var g envv1alpha1.Golden
		if ev.Object.Kind == registry.Golden.Kind && proto.Unmarshal(ev.Object.Data, &g) == nil && g.GetMetadata().GetLabels()["tower.hangarlab.cloud/warm-for"] == repo &&
			!slices.Contains(names, ev.Object.Name) {
			names = append(names, ev.Object.Name)
		}
		if ev.Revision >= rev {
			return errDone
		}
		return nil
	})
	if !errors.Is(err, errDone) {
		w.t.Fatal(err)
	}
	return names
}

// TestAFailedGoldenIsBuiltAgainAfterABackoff: a failed golden something
// asks for is built again once its backoff passes: ten minutes, then
// twice as long after each failure in a row, which a restart keeps. The
// box waiting says why and when; a success ends the backoff.
func TestAFailedGoldenIsBuiltAgainAfterABackoff(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.fake.FailBuilds(fmt.Errorf("step 3/7: npm ci exited 1: %w", provider.ErrFailed))
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	waitSays := func(want ...string) *envv1alpha1.Box {
		t.Helper()
		var b *envv1alpha1.Box
		testutil.Eventually(t, fmt.Sprintf("the box failed, saying %q", want), func() (bool, string) {
			b = w.box("dev")
			msg := condition(b.GetStatus().GetConditions(), "Ready").GetMessage()
			for _, s := range want {
				if !strings.Contains(msg, s) {
					return false, b.GetStatus().String()
				}
			}
			return b.GetStatus().GetPhase() == envv1alpha1.BoxPhase_BOX_PHASE_FAILED, b.GetStatus().String()
		})
		return b
	}
	at := func(d time.Duration) string { return w.clock.Now().Add(d).UTC().Format(time.RFC3339) }
	b := waitSays("npm ci exited 1", "built again at "+at(apiserver.GoldenRetryFirst))
	first, _ := w.golden(b.GetStatus().GetImageGolden())
	uid := func() string {
		g, ok := w.golden(first.GetMetadata().GetName())
		if !ok {
			return ""
		}
		return g.GetMetadata().GetUid()
	}
	w.advance(apiserver.GoldenRetryFirst - time.Minute)
	if got := uid(); got != first.GetMetadata().GetUid() {
		t.Fatalf("the golden was built again before its backoff: uid %q, was %q", got, first.GetMetadata().GetUid())
	}
	// The backoff outlives a restart.
	w.restart()
	w.advance(2 * time.Minute)
	waitSays("npm ci exited 1", "built again at "+at(2*apiserver.GoldenRetryFirst))
	second := uid()
	if second == first.GetMetadata().GetUid() || second == "" {
		t.Fatalf("the golden was not built again after its backoff: uid %q", second)
	}
	w.fake.FailBuilds(nil)
	w.advance(2*apiserver.GoldenRetryFirst - 2*time.Minute)
	if got := uid(); got != second {
		t.Fatalf("the second backoff did not double: built again at uid %q", got)
	}
	w.advance(2 * time.Minute)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	w.waitSettled()
	if n := w.records("GoldenBackoff"); n != 0 {
		t.Errorf("%d backoffs kept after the golden built", n)
	}
}

// TestAWarmGoldenInUseIsKeptUntilUnused: a warm golden that falls out of
// the budget while a box is cloned from it is kept, artifact and all, and
// counted against the budget, until the box goes; no new box is cloned
// from it meanwhile.
func TestAWarmGoldenInUseIsKeptUntilUnused(t *testing.T) {
	t.Parallel()
	testutil.Quarantine(t, "#19")
	// Room for one warm golden of 10Gi, and not for another on an
	// estimate (5Gi) beside it.
	w := newWorld(t, worldOption{settings: &targetsv1alpha1.TargetSettings{WarmBudget: "14Gi"}})
	w.fake.CaptureSize(func(provider.Instance) uint64 { return 10 << 30 })
	w.mustApply(registry.Repository, repository("a"))
	w.mustApply(registry.Repository, repository("b"))
	w.openBoxes("a", 3)
	w.advance(5 * time.Minute)
	var warmA string
	testutil.Eventually(t, "a warm golden for a", func() (bool, string) {
		got := w.warmOf()
		if len(got["a"]) == 1 {
			warmA = got["a"][0]
		}
		return len(got["a"]) == 1, fmt.Sprint(got)
	})
	user := w.readyBox("user", func(b *envv1alpha1.Box) { b.Spec.Checkouts[0].Repository = "a" })
	if user.GetStatus().GetWarmGolden() != warmA {
		t.Fatalf("the box was not cloned from a's warm golden: %v", user.GetStatus())
	}
	g, _ := w.golden(warmA)
	artifact := g.GetStatus().GetArtifact()

	// b, used far more, takes the place once a's minimum hold is over.
	w.openBoxes("b", 9)
	w.advance(apiserver.DefaultWarmMinHold)
	w.advance(time.Minute)
	testutil.Eventually(t, "a's warm golden let go", func() (bool, string) {
		g, ok := w.golden(warmA)
		return !ok || g.GetMetadata().GetAnnotations()["tower.hangarlab.cloud/warm-dropped"] != "", fmt.Sprint(g.GetMetadata())
	})
	w.waitSettled()
	if _, ok := w.golden(warmA); !ok {
		t.Fatal("a warm golden a box is cloned from was deleted")
	}
	if !slices.Contains(w.fake.Artifacts(), artifact) {
		t.Errorf("the artifact of a warm golden a box is cloned from was deleted: %v", w.fake.Artifacts())
	}
	if made := w.goldensEverMadeFor("b"); len(made) != 0 {
		t.Errorf("b was given room the kept golden still holds: %v", made)
	}
	fresh := w.readyBox("fresh", func(b *envv1alpha1.Box) { b.Spec.Checkouts[0].Repository = "a" })
	if wg := fresh.GetStatus().GetWarmGolden(); wg != "" {
		t.Errorf("a new box was cloned from a warm golden let go: %q", wg)
	}

	// Once nothing is cloned from it, it goes, and b gets its place.
	if _, err := w.asSystem().generic.Delete(t.Context(), registry.Box, client.Key{Project: "web", Name: "user"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a's golden gone, b warm", func() (bool, string) {
		_, kept := w.golden(warmA)
		got := w.warmOf()
		return !kept && !slices.Contains(w.fake.Artifacts(), artifact) && len(got["b"]) == 1, fmt.Sprint(kept, got, w.fake.Artifacts())
	})
}

// TestAWarmSizeOutlivesARestart: a repository whose warm golden proved too
// big for the budget is not let in again on the estimate, also after tower
// restarts: its golden is not built again.
func TestAWarmSizeOutlivesARestart(t *testing.T) {
	t.Parallel()
	// Both are let in on the estimate (5Gi each); built, at 10Gi each,
	// one goes, and would fit again beside the other on the estimate.
	w := newWorld(t, worldOption{settings: &targetsv1alpha1.TargetSettings{WarmBudget: "16Gi"}})
	w.fake.CaptureSize(func(provider.Instance) uint64 { return 10 << 30 })
	w.mustApply(registry.Repository, repository("a"))
	w.mustApply(registry.Repository, repository("b"))
	w.openBoxes("a", 3)
	w.openBoxes("b", 2)
	w.advance(5 * time.Minute)
	testutil.Eventually(t, "one warm golden, for a", func() (bool, string) {
		got := w.warmOf()
		return len(got["a"]) == 1 && len(got["b"]) == 0, fmt.Sprint(got)
	})
	w.waitSettled()
	builds := func() int {
		n := 0
		for _, e := range w.fake.Execs() {
			if strings.HasPrefix(e.Instance.Name, "build-warm") {
				n++
			}
		}
		return n
	}
	before := builds()
	w.restart()
	w.advance(5 * time.Minute)
	w.advance(5 * time.Minute)
	if n := builds(); n != before {
		t.Errorf("a warm golden was built again after a restart: %d builder commands, then %d", before, n)
	}
}

// TestAWarmGoldenOfAnotherImageOrTargetIsNotCloned: a box is cloned from a
// warm golden only if it was made on the box's own image, on the box's own
// target; otherwise it takes the slow path.
func TestAWarmGoldenOfAnotherImageOrTargetIsNotCloned(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.openBoxes("api", 2)
	w.advance(5 * time.Minute)
	var warm string
	testutil.Eventually(t, "a warm golden", func() (bool, string) {
		got := w.warmOf()["api"]
		if len(got) == 1 {
			warm = got[0]
		}
		return len(got) == 1, fmt.Sprint(got)
	})
	browser := w.readyBox("browser", func(b *envv1alpha1.Box) { b.Spec.Addons = []string{"browser"} })
	if wg := browser.GetStatus().GetWarmGolden(); wg != "" {
		t.Errorf("a box of another image was cloned from warm golden %q, made on another", wg)
	}
	// A newer warm golden of the same image, ready on another target.
	g, _ := w.golden(warm)
	other := proto.Clone(g).(*envv1alpha1.Golden)
	other.Metadata = &metav1.ObjectMeta{Project: "web", Name: "warm-elsewhere", Labels: g.GetMetadata().GetLabels()}
	other.Spec.Target, other.Status = "t2", nil
	w.mustApply(registry.Target, hostdTarget("t2"))
	w.clock.Advance(time.Second)
	w.mustApply(registry.Golden, other)
	if _, err := w.asSystem().generic.UpdateStatus(t.Context(), registry.Golden, client.Key{Project: "web", Name: "warm-elsewhere"},
		&envv1alpha1.GoldenStatus{Phase: envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY, Artifact: "t2/snap-1"}, nil); err != nil && connect.CodeOf(err) != connect.CodeAborted {
		t.Fatal(err)
	}
	same := w.readyBox("same")
	if wg := same.GetStatus().GetWarmGolden(); wg != warm {
		t.Errorf("a box was cloned from %q, want its target's warm golden %q", wg, warm)
	}
}

// TestAGoldenBeingDeletedIsNotUsed: a box whose image golden is being
// deleted takes it for gone, and waits for a new one, rather than read the
// old one's state.
func TestAGoldenBeingDeletedIsNotUsed(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.fake.FailBuilds(fmt.Errorf("broken: %w", provider.ErrFailed))
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	b := w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_FAILED)
	name := b.GetStatus().GetImageGolden()
	// Held while it is deleted.
	finalizers := func(change func([]string) []string) {
		t.Helper()
		testutil.Eventually(t, "the golden's finalizers changed", func() (bool, string) {
			g, _ := w.golden(name)
			g.Metadata.Finalizers = change(g.Metadata.Finalizers)
			_, err := w.asSystem().generic.Apply(t.Context(), registry.Golden, g)
			if err != nil && connect.CodeOf(err) != connect.CodeAborted {
				t.Fatal(err)
			}
			return err == nil, fmt.Sprint(err)
		})
	}
	const hold = "test.example.com/hold"
	finalizers(func(f []string) []string { return append(f, hold) })
	if _, err := w.asSystem().generic.Delete(t.Context(), registry.Golden, client.Key{Project: "web", Name: name}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the box waiting for its golden to go", func() (bool, string) {
		b := w.box("dev")
		return b.GetStatus().GetPhase() == envv1alpha1.BoxPhase_BOX_PHASE_PENDING, b.GetStatus().String()
	})
	w.fake.FailBuilds(nil)
	if err := w.srv.RemoveFinalizer(t.Context(), registry.Golden, resource.Key{Project: "web", Name: name}, hold); err != nil {
		t.Fatal(err)
	}
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
}

// TestAGoldensBuildIsTowersOwn: what a golden is built from is kept where
// only tower reads it; the golden, which hostd reads, holds its hashes.
func TestAGoldensBuildIsTowersOwn(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	b := w.readyBox("dev")
	g, _ := w.golden(b.GetStatus().GetImageGolden())
	for k, v := range g.GetMetadata().GetAnnotations() {
		if strings.Contains(v, "mirror") || strings.Contains(v, "web/api") {
			t.Errorf("the golden's annotation %s says what it is built from: %s", k, v)
		}
	}
	if n := w.records("GoldenBuild"); n == 0 {
		t.Error("no build of a golden is kept")
	}
}

// TestAMirrorAheadOfItsRepositoryIsNotStale: commits behind are counted
// up to the head the repository's status names, which a warm golden is
// built at, not the mirror's own, which a fetch may have moved past it.
func TestAMirrorAheadOfItsRepositoryIsNotStale(t *testing.T) {
	t.Parallel()
	testutil.Quarantine(t, "#25")
	w := newWorld(t, worldOption{settings: &targetsv1alpha1.TargetSettings{
		WarmMaxCommitsBehind: 2, IdleStopAfter: durationpb.New(1000 * time.Hour),
	}})
	url := "https://git.example.com/api.git"
	w.fake.SetRemote(url, "main", nil, "c1")
	w.readyBox("dev")
	w.advance(5 * time.Minute)
	var first string
	testutil.Eventually(t, "a warm golden", func() (bool, string) {
		got := w.warmOf()["api"]
		if len(got) == 1 {
			first = got[0]
		}
		return len(got) == 1, fmt.Sprint(got)
	})
	// The repository's status at c3, two behind: fresh.
	w.fake.SetRemote(url, "main", nil, "c1", "c2", "c3")
	w.advance(10 * time.Minute)
	testutil.Eventually(t, "the mirror's head c3", func() (bool, string) {
		r, _ := w.getRepo("api")
		return r.GetStatus().GetMirrors()[0].GetHead() == "c3", fmt.Sprint(r.GetStatus().GetMirrors())
	})
	// The mirror moves on to c4 before the status says so.
	w.fake.SetRemote(url, "main", nil, "c1", "c2", "c3", "c4")
	if _, err := w.fake.Fetch(t.Context(), provider.FetchRequest{Mirror: "web/api", URL: url, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	w.readyBox("another") // the warm controller looks again
	w.waitSettled()
	if made := w.goldensEverMadeFor("api"); len(made) != 1 || made[0] != first {
		t.Errorf("a warm golden fresh against its repository's status was rebuilt: %v", made)
	}
}

// TestABuildTheTargetDidNotAnswerIsTriedAgain: a golden's build that
// failed because its target did not answer, as a hostd not yet connected
// does not, is tried again once it answers, and the box comes up.
func TestABuildTheTargetDidNotAnswerIsTriedAgain(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.fake.SetUnavailable(true)
	b := box("dev")
	b.Spec.Image = "ghcr.io/acme/dev:1"
	w.mustApply(registry.Box, b)
	testutil.Eventually(t, "the build tried and refused", func() (bool, string) {
		o, err := w.get(registry.Target, "", "t1")
		if err != nil {
			return false, err.Error()
		}
		last := o.(*targetsv1alpha1.Target).GetStatus().GetLastError()
		return strings.Contains(last, "golden/"), last
	})
	w.fake.SetUnavailable(false)
	w.advance(controllerMaxWait)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
}

// builderSpec is the spec of the builder of the warm golden of repo that
// the store's history holds last, gone or not.
func (w *world) builderSpec(repo string) *envv1alpha1.InstanceSpec {
	w.t.Helper()
	made := w.goldensEverMadeFor(repo)
	if len(made) == 0 {
		w.t.Fatalf("no warm golden of %s was made", repo)
	}
	rev, err := w.store.Revision(w.t.Context())
	if err != nil {
		w.t.Fatal(err)
	}
	var spec *envv1alpha1.InstanceSpec
	err = w.store.Watch(w.t.Context(), store.WatchOptions{SinceRevision: 0}, func(ev store.Event) error {
		var in envv1alpha1.Instance
		if ev.Object.Kind == registry.Instance.Kind && ev.Object.Name == "build-"+made[len(made)-1] && proto.Unmarshal(ev.Object.Data, &in) == nil {
			spec = in.GetSpec()
		}
		if ev.Revision >= rev {
			return errDone
		}
		return nil
	})
	if !errors.Is(err, errDone) {
		w.t.Fatal(err)
	}
	if spec == nil {
		w.t.Fatalf("no builder of %v in the history", made)
	}
	return spec
}

// excludedCaches reports whether the caches were excluded from git in the
// instance's checkout.
func excludedCaches(execs []fake.Exec, instance string) bool {
	return slices.ContainsFunc(execs, func(e fake.Exec) bool {
		return e.Instance.Name == instance && len(e.Command) == 3 && e.Command[0] == "sh" && strings.Contains(e.Command[2], "/.ws-cache/") && strings.Contains(e.Command[2], ".git/info/exclude")
	})
}

// adoptedToolDirs reports whether the instance kept the tool directories
// its image filled (apiserver's adoptToolDirs).
func adoptedToolDirs(execs []fake.Exec, instance string) bool {
	return slices.ContainsFunc(execs, func(e fake.Exec) bool {
		return e.Instance.Name == instance && len(e.Command) == 3 && e.Command[0] == "sh" && strings.Contains(e.Command[2], "MISE_DATA_DIR") && strings.Contains(e.Command[2], "'"+apiserver.CacheDir+"'")
	})
}

// warmGolden opens three boxes of repo a, and returns the warm golden it
// then gets.
func (w *world) warmGolden() *envv1alpha1.Golden {
	w.t.Helper()
	w.mustApply(registry.Repository, repository("a"))
	w.openBoxes("a", 3)
	w.advance(5 * time.Minute)
	var warm string
	testutil.Eventually(w.t, "a warm golden for a", func() (bool, string) {
		got := w.warmOf()
		if len(got["a"]) == 1 {
			warm = got["a"][0]
		}
		return warm != "", fmt.Sprint(got)
	})
	g, _ := w.golden(warm)
	return g
}

// prebuilding is a world whose warm goldens' pre-build ends with steps.
func prebuilding(steps ...devcontainer.Step) worldOption {
	return worldOption{config: func(c *apiserver.Config) {
		c.Resolver = devcontainer.Fake{Prebuild: steps}
		c.PrebuildStepTimeout = 200 * time.Millisecond
	}}
}

// TestAPreBuildStepThatRunsTooLongIsLeft: a pre-build step that runs past
// its time is ended, and the golden is captured, saying so.
func TestAPreBuildStepThatRunsTooLongIsLeft(t *testing.T) {
	t.Parallel()
	w := newWorld(t, prebuilding(devcontainer.Step{Name: "slow", Command: []string{"sleep"}}))
	g := w.warmGolden()
	if c := condition(g.GetStatus().GetConditions(), apiserver.GoldenPreBuilt); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetMessage() != "slow: did not end in 200ms" {
		t.Errorf("a golden whose pre-build step ran too long: PreBuilt %v", c)
	}
}

// TestAPreBuildStepOutOfDiskIsTriedAgain: on a hangar target, a pre-build
// step that fails for a full disk runs again, after the disk has had time
// to grow, for as long as it grows; once it no longer does, the golden is
// captured saying so.
func TestAPreBuildStepOutOfDiskIsTriedAgain(t *testing.T) {
	t.Parallel()
	for what, tc := range map[string]struct {
		fails int
		grows bool
		runs  int
		says  string
	}{
		"grows until it passes": {fails: 3, grows: true, runs: 4},
		"stops growing":         {fails: 100, grows: false, runs: 1, says: "make build: no space left on device, and its disk did not grow in 45s"},
		// From 2Gi to 30Gi by 2x is four steps: five runs after the first.
		"grows, never enough": {fails: 100, grows: true, runs: 6, says: "make build: no space left on device, 6 times as its disk grew"},
	} {
		t.Run(what, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, worldOption{hangar: true}, prebuilding(devcontainer.Step{Name: "make build", Command: []string{"make", "build"}}))
			var mu sync.Mutex
			runs, size := 0, uint64(2<<20)
			w.fake.OnExec(func(req provider.ExecRequest) (int, bool) {
				mu.Lock()
				defer mu.Unlock()
				switch req.Command[0] {
				case "df":
					_, _ = fmt.Fprintf(req.Stdout, "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/vdb %d %d 0 100%% /workspace\n", size, size)
					if tc.grows {
						size *= 2
					}
					return 0, true
				case "make":
					if runs++; runs <= tc.fails {
						_, _ = io.WriteString(req.Stdout, "write /workspace/.ws-cache/go-build/01: No space left on device\n")
						return 2, true
					}
					return 0, true
				}
				return 0, false
			})
			ran := func() int { mu.Lock(); defer mu.Unlock(); return runs }
			w.mustApply(registry.Repository, repository("a"))
			w.openBoxes("a", 3)
			// Not advance, which waits for the controllers to settle: the
			// builder's job waits on the clock.
			w.waitSettled()
			w.clock.Advance(5 * time.Minute)
			testutil.Eventually(t, "the warm golden captured", func() (bool, string) {
				if len(w.warmOf()["a"]) == 1 {
					return true, ""
				}
				w.clock.Advance(apiserver.NoSpaceRetryAfter / 3)
				return false, fmt.Sprint(ran(), w.warmOf())
			})
			g, _ := w.golden(w.warmOf()["a"][0])
			c := condition(g.GetStatus().GetConditions(), apiserver.GoldenPreBuilt)
			switch {
			case tc.says == "" && c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE:
				t.Errorf("a step that passed once its disk grew: PreBuilt %v", c)
			case tc.says != "" && (c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetMessage() != tc.says):
				t.Errorf("PreBuilt %v, want it to say %q", c, tc.says)
			}
			if n := ran(); n != tc.runs {
				t.Errorf("the step ran %d times, want %d", n, tc.runs)
			}
		})
	}
}

// TestABoxAndItsWarmBuilderGrowOnAHangarTarget: on a hangar target a box's
// disks grow from the target's starts, and a warm golden's builder's
// /workspace starts as a box's, then at what the last golden held.
func TestABoxAndItsWarmBuilderGrowOnAHangarTarget(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{hangar: true, settings: &targetsv1alpha1.TargetSettings{WarmMaxAge: durationpb.New(time.Hour), BoxRootDiskMax: "40Gi"}})
	w.fake.CaptureSize(func(provider.Instance) uint64 { return 4 << 30 })
	first := w.warmGolden()
	growth := &envv1alpha1.DiskGrowth{WorkspaceMax: "30Gi", Root: "10Gi", RootMax: "40Gi", Threshold: "80%", Step: "2x"}
	b := w.box("a-0")
	in, _ := w.instance(b.GetStatus().GetInstance())
	if s := in.GetSpec(); s.GetResources().GetDisk() != "2Gi" || !proto.Equal(s.GetDiskGrowth(), growth) {
		t.Errorf("a box on a hangar target: disk %q, growth %v; want 2Gi, %v", s.GetResources().GetDisk(), s.GetDiskGrowth(), growth)
	}
	if s := w.builderSpec("a"); s.GetResources().GetDisk() != "2Gi" || !proto.Equal(s.GetDiskGrowth(), growth) {
		t.Errorf("the first warm builder: disk %q, growth %v; want 2Gi, %v", s.GetResources().GetDisk(), s.GetDiskGrowth(), growth)
	}
	// Stale, it is built again, from what the first held (4Gi).
	w.advance(time.Hour + time.Minute)
	testutil.Eventually(t, "a second warm golden", func() (bool, string) {
		got := w.warmOf()["a"]
		return len(got) == 1 && got[0] != first.GetMetadata().GetName(), fmt.Sprint(got)
	})
	if s := w.builderSpec("a"); s.GetResources().GetDisk() != "5Gi" || !proto.Equal(s.GetDiskGrowth(), growth) {
		t.Errorf("the second warm builder: disk %q, growth %v; want 5Gi, %v", s.GetResources().GetDisk(), s.GetDiskGrowth(), growth)
	}
}

// TestAWarmGoldenIsPreBuilt: a warm golden's builder is made as a box of
// its image is, its caches in /workspace and out of git's sight, and runs
// the pre-build. A step that fails does not fail the golden: it is
// captured, READY, and its PreBuilt condition says which step and why.
func TestAWarmGoldenIsPreBuilt(t *testing.T) {
	t.Parallel()
	for what, fails := range map[string]bool{"passes": false, "fails": true} {
		t.Run(what, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
				c.Resolver = devcontainer.Fake{Prebuild: []devcontainer.Step{{Name: "make build", Command: []string{"make", "build"}}}}
			}})
			if fails {
				w.fake.FailCommands("make")
			}
			w.mustApply(registry.Repository, repository("a"))
			w.openBoxes("a", 3)
			w.advance(5 * time.Minute)
			var warm string
			testutil.Eventually(t, "a warm golden for a", func() (bool, string) {
				got := w.warmOf()
				if len(got["a"]) == 1 {
					warm = got["a"][0]
				}
				return warm != "", fmt.Sprint(got)
			})
			g, _ := w.golden(warm)
			if g.GetSpec().GetBoxWarm().GetPrebuildHash() == "" {
				t.Errorf("the warm golden's inputs %v carry no pre-build hash", g.GetSpec().GetBoxWarm())
			}
			c := condition(g.GetStatus().GetConditions(), apiserver.GoldenPreBuilt)
			switch {
			case !fails && (c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE || c.GetReason() != "PreBuilt"):
				t.Errorf("a warm golden whose pre-build passed: PreBuilt %v", c)
			case fails && (c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetReason() != "PreBuildFailed" || c.GetMessage() != "make build: exit 1"):
				t.Errorf("a warm golden whose pre-build failed: PreBuilt %v", c)
			}
			builder := "build-" + warm
			var ran []string
			for _, e := range w.fake.Execs() {
				if e.Instance.Name == builder {
					ran = append(ran, e.Command[0])
				}
			}
			if want := []string{"sh", "sh", "install-dependencies", "make"}; !slices.Equal(ran, want) {
				t.Errorf("the builder ran %q, want %q", ran, want)
			}
			if !excludedCaches(w.fake.Execs(), builder) {
				t.Errorf("the builder's checkout does not exclude the caches from git: %v", w.fake.Execs())
			}
			if !adoptedToolDirs(w.fake.Execs(), builder) {
				t.Errorf("the builder did not keep the image's tool directories: %v", w.fake.Execs())
			}
			spec := w.builderSpec("a")
			if spec.GetEnv()["GOCACHE"] != apiserver.CacheDir+"/go-build" || spec.GetResources().GetDisk() != "40Gi" || spec.GetDiskGrowth() != nil {
				t.Errorf("the builder on a hostd target: %v", spec)
			}
		})
	}
}
