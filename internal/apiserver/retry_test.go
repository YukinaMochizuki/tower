package apiserver_test

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// TestARetryBuildsAFailedGoldenAtOnce: a golden that failed waits out its
// backoff, and its box says when it failed and when it is built again;
// a retry asked for builds it again at once, as a first failure counts
// from then, and a retry of the box once the cause is fixed makes it
// ready without waiting.
func TestARetryBuildsAFailedGoldenAtOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.fake.FailBuilds(fmt.Errorf("step 3/7: npm ci exited 1: %w", provider.ErrFailed))
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	at := func(d time.Duration) string { return w.clock.Now().Add(d).UTC().Format(time.RFC3339) }
	b := w.waitFailedSaying("dev", "npm ci exited 1", "failed at "+at(0), "built again at "+at(apiserver.GoldenRetryFirst))
	name := b.GetStatus().GetImageGolden()
	first, _ := w.golden(name)
	if msg := condition(first.GetStatus().GetConditions(), apiserver.GoldenReady).GetMessage(); !strings.Contains(msg, "failed at "+at(0)) ||
		!strings.Contains(msg, "built again at "+at(apiserver.GoldenRetryFirst)) {
		t.Errorf("the golden's condition says %q", msg)
	}
	uid := func() string {
		g, ok := w.golden(name)
		if !ok {
			return ""
		}
		return g.GetMetadata().GetUid()
	}

	// Without a retry, nothing runs before the backoff passes.
	w.advance(time.Minute)
	if got := uid(); got != first.GetMetadata().GetUid() {
		t.Fatalf("the golden was built again before its backoff: uid %q, was %q", got, first.GetMetadata().GetUid())
	}

	res := must[envv1alpha1.RetryGoldenResponse](t)(w.as(workstream).retry.RetryGolden(t.Context(), connect.NewRequest(&envv1alpha1.RetryGoldenRequest{Project: "web", Name: name})))
	if !res.GetRetried() || res.GetGolden().GetMetadata().GetUid() != first.GetMetadata().GetUid() {
		t.Errorf("RetryGolden answered %v", res)
	}
	// Built again at once, and failed again: the first backoff, from now.
	w.waitFailedSaying("dev", "npm ci exited 1", "failed at "+at(0), "built again at "+at(apiserver.GoldenRetryFirst))
	if got := uid(); got == first.GetMetadata().GetUid() || got == "" {
		t.Fatalf("the golden was not built again after a retry: uid %q", got)
	}

	// The cause fixed, a retry of the box makes it ready now.
	w.fake.FailBuilds(nil)
	br := must[envv1alpha1.RetryBoxResponse](t)(w.as(admin).retry.RetryBox(t.Context(), connect.NewRequest(&envv1alpha1.RetryBoxRequest{Project: "web", Name: "dev"})))
	if !br.GetRetried() || br.GetBox().GetStatus().GetPhase() != envv1alpha1.BoxPhase_BOX_PHASE_FAILED {
		t.Errorf("RetryBox answered %v", br)
	}
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	w.waitSettled()
	if n := w.records("GoldenBackoff"); n != 0 {
		t.Errorf("%d backoffs kept after the golden built", n)
	}

	// A box that is not FAILED is left as it is.
	br = must[envv1alpha1.RetryBoxResponse](t)(w.as(workstream).retry.RetryBox(t.Context(), connect.NewRequest(&envv1alpha1.RetryBoxRequest{Project: "web", Name: "dev"})))
	if br.GetRetried() {
		t.Errorf("a READY box was retried: %v", br)
	}
}

// TestARetryOfAFallenBackBoxBuildsItsDevcontainerImage: a box whose
// devcontainer's image did not build falls back, READY, naming the golden
// that failed, why, when, and when it is built again. A retry of the box
// builds that golden again at once, without waiting out its backoff, and
// leaves the box on the image it fell back to; a box made after is made
// of the devcontainer's image.
func TestARetryOfAFallenBackBoxBuildsItsDevcontainerImage(t *testing.T) {
	t.Parallel()
	w := newWorld(t, realResolver())
	w.fake.SetImage("ubuntu:24.04", provider.ImageConfig{Digest: "sha256:dd"})
	w.fake.SetRemote("https://git.example.com/api.git", "main", map[string]string{
		".devcontainer.json": `{"build": {"dockerfile": "Dockerfile"}}`,
		"Dockerfile":         "FROM ubuntu:24.04\nRUN apt-get update\n",
	}, "c1")
	w.fake.FailMirrorBuilds(fmt.Errorf("step 2/2: apt-get update: Temporary failure resolving 'archive.ubuntu.com': %w", provider.ErrFailed))
	fallback := func(b *envv1alpha1.Box) { b.Spec.FallbackImage = "ubuntu:24.04" }
	at := func(d time.Duration) string { return w.clock.Now().Add(d).UTC().Format(time.RFC3339) }
	first := w.readyBox("first", fallback)
	dev := first.GetStatus().GetDevcontainerGolden()
	if dev == "" || dev == first.GetStatus().GetImageGolden() {
		t.Fatalf("a fallen-back box names devcontainer golden %q, image golden %q", dev, first.GetStatus().GetImageGolden())
	}
	c := condition(first.GetStatus().GetConditions(), apiserver.BoxDevcontainerFellBack)
	want := "its devcontainer's image did not build (golden " + dev + "): step 2/2: apt-get update: Temporary failure resolving 'archive.ubuntu.com'"
	if c.GetReason() != "BuildFailed" || !strings.HasPrefix(c.GetMessage(), want) || !strings.Contains(c.GetMessage(), "failed at "+at(0)) ||
		!strings.Contains(c.GetMessage(), "built again at "+at(apiserver.GoldenRetryFirst)) {
		t.Errorf("DevcontainerFellBack %v, want it starting %q and saying when", c, want)
	}
	failed, _ := w.golden(dev)
	if goldenPhase := failed.GetStatus().GetPhase(); goldenPhase != envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED {
		t.Fatalf("the devcontainer's golden is %v", goldenPhase)
	}

	// Without a retry, a box made before the backoff passes falls back too.
	w.advance(time.Minute)
	before := w.readyBox("before", fallback)
	if before.GetStatus().GetImageGolden() != first.GetStatus().GetImageGolden() || before.GetStatus().GetDevcontainerGolden() != dev {
		t.Errorf("a box made before the retry: %v", before.GetStatus())
	}

	// The cause fixed, a retry of the READY box builds the image now.
	w.fake.FailMirrorBuilds(nil)
	res := must[envv1alpha1.RetryBoxResponse](t)(w.as(workstream).retry.RetryBox(t.Context(), connect.NewRequest(&envv1alpha1.RetryBoxRequest{Project: "web", Name: "first"})))
	if !res.GetRetried() || res.GetBox().GetStatus().GetPhase() != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("RetryBox answered %v", res)
	}
	testutil.Eventually(t, "the devcontainer's golden built again", func() (bool, string) {
		g, ok := w.golden(dev)
		return ok && g.GetMetadata().GetUid() != failed.GetMetadata().GetUid() && g.GetStatus().GetPhase() == envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY, g.String()
	})
	testutil.Eventually(t, "the box saying its devcontainer's image is built since", func() (bool, string) {
		st := w.box("first").GetStatus()
		c := condition(st.GetConditions(), apiserver.BoxDevcontainerFellBack)
		return c.GetReason() == "BuiltSince" && strings.Contains(c.GetMessage(), dev) && st.GetDevcontainerGolden() == "", st.String()
	})
	if got := w.box("first").GetStatus().GetImageGolden(); got != first.GetStatus().GetImageGolden() {
		t.Errorf("the retried box moved to image golden %q", got)
	}

	// A box made now is made of the devcontainer's image.
	second := w.readyBox("second", fallback)
	if got := second.GetStatus().GetImageGolden(); got != dev {
		t.Errorf("a box made after the retry is made of %q, want %q", got, dev)
	}
	if c := condition(second.GetStatus().GetConditions(), apiserver.BoxDevcontainerFellBack); c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("a box made after the retry fell back: %v", c)
	}
	w.waitSettled()
	if n := w.records("GoldenBackoff"); n != 0 {
		t.Errorf("%d backoffs kept after the golden built", n)
	}

	// Nothing left to retry.
	res = must[envv1alpha1.RetryBoxResponse](t)(w.as(admin).retry.RetryBox(t.Context(), connect.NewRequest(&envv1alpha1.RetryBoxRequest{Project: "web", Name: "first"})))
	if res.GetRetried() {
		t.Errorf("a box whose devcontainer image is built was retried: %v", res)
	}
}

// TestARetryRunsAFailedStepAtOnce: a box whose checkout step failed runs
// it again at once when retried, not when its backoff passes.
func TestARetryRunsAFailedStepAtOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.fake.FailCommands("install-dependencies")
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	at := func(d time.Duration) string { return w.clock.Now().Add(d).UTC().Format(time.RFC3339) }
	b := w.waitFailedSaying("dev", "install-dependencies exited 1", "failed at "+at(0), "tried again at "+at(apiserver.StepRetryFirst))
	instance := b.GetStatus().GetInstance()
	w.advance(time.Minute)
	if n := w.runsIn(instance, "install-dependencies"); n != 1 {
		t.Fatalf("the step ran %d times before its backoff passed", n)
	}

	w.fake.FailCommands()
	res := must[envv1alpha1.RetryBoxResponse](t)(w.as(workstream).retry.RetryBox(t.Context(), connect.NewRequest(&envv1alpha1.RetryBoxRequest{Project: "web", Name: "dev"})))
	if !res.GetRetried() {
		t.Errorf("RetryBox answered %v", res)
	}
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	if n := w.runsIn(instance, "install-dependencies"); n != 2 {
		t.Errorf("the step ran %d times", n)
	}
	w.waitSettled()
	if n := w.records("StepBackoff"); n != 0 {
		t.Errorf("%d step backoffs kept after a retry passed", n)
	}
}

// TestARetryRunsAFailedConfigurePhaseAtOnce: a machine whose configure
// phase failed runs it again at once when retried.
func TestARetryRunsAFailedConfigurePhaseAtOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var mu sync.Mutex
	fail, configures := true, 0
	w.fake.OnExec(func(req provider.ExecRequest) (int, bool) {
		if !slices.Contains(req.Command, "--tags=configure") {
			return 0, false
		}
		mu.Lock()
		defer mu.Unlock()
		configures++
		if fail {
			return 2, true
		}
		return 0, true
	})
	w.mustApply(registry.Machine, machine("k3s"))
	at := func(d time.Duration) string { return w.clock.Now().Add(d).UTC().Format(time.RFC3339) }
	testutil.Eventually(t, "the machine failed, saying when it failed and runs again", func() (bool, string) {
		st := w.machine("k3s").GetStatus()
		msg := condition(st.GetConditions(), apiserver.MachineReady).GetMessage()
		return st.GetPhase() == envv1alpha1.MachinePhase_MACHINE_PHASE_FAILED && strings.Contains(msg, "failed at "+at(0)) &&
			strings.Contains(msg, "tried again at "+at(apiserver.StepRetryFirst)), st.String()
	})
	w.waitSettled()
	mu.Lock()
	fail = false
	mu.Unlock()
	res := must[envv1alpha1.RetryMachineResponse](t)(w.as(admin).retry.RetryMachine(t.Context(), connect.NewRequest(&envv1alpha1.RetryMachineRequest{Project: "web", Name: "k3s"})))
	if !res.GetRetried() {
		t.Errorf("RetryMachine answered %v", res)
	}
	w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	mu.Lock()
	defer mu.Unlock()
	if configures != 2 {
		t.Errorf("the configure phase ran %d times", configures)
	}
}

// TestWhoMayRetry: an admin, workstream and root on the host may retry;
// hostd and a caller without credentials may not, and are told nothing of
// the object; one that is not there is NOT_FOUND.
func TestWhoMayRetry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.setup()
	req := func(name string) *connect.Request[envv1alpha1.RetryBoxRequest] {
		return connect.NewRequest(&envv1alpha1.RetryBoxRequest{Project: "web", Name: name})
	}
	for who, c := range map[string]clients{"admin": e.as(admin), "workstream": e.as(workstream), "root on the host": e.local()} {
		_, err := c.retry.RetryBox(t.Context(), req("absent"))
		wantCode(t, err, connect.CodeNotFound, who+" retrying an absent box")
	}
	_, err := e.as(hostd).retry.RetryBox(t.Context(), req("absent"))
	wantCode(t, err, connect.CodePermissionDenied, "hostd retrying a box")
	_, err = e.as("").retry.RetryMachine(t.Context(), connect.NewRequest(&envv1alpha1.RetryMachineRequest{Project: "web", Name: "absent"}))
	wantCode(t, err, connect.CodeUnauthenticated, "no credentials retrying a machine")
	_, err = e.as(admin).retry.RetryGolden(t.Context(), connect.NewRequest(&envv1alpha1.RetryGoldenRequest{Name: "absent"}))
	wantCode(t, err, connect.CodeInvalidArgument, "a golden without a project")
}
