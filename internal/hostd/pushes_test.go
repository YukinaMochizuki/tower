package hostd_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/hostd"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/hostdtest"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// errRegistryDown is a push that does not reach the registry.
var errRegistryDown = errors.New("dial tcp: connection refused")

// fastPushRetry tries a failed push again soon.
func fastPushRetry(c *hostd.Config) {
	c.PushRetryFirst, c.PushRetryMost = 20*time.Millisecond, 50*time.Millisecond
}

// build builds an image through hostd's provider, as tower does.
func build(t *testing.T, w *hostdtest.World, dockerfile string) string {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(dockerfile)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(dockerfile)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	ref, err := w.Host.BuildImage(t.Context(), provider.BuildRequest{Context: &b, Repository: "web/box"})
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// imagesPushed is the target's ImagesPushed condition, as last reported.
func imagesPushed(t *testing.T, w *hostdtest.World) *metav1.Condition {
	t.Helper()
	o, err := w.Env.AsSystem().Get(t.Context(), registry.Target, client.Key{Name: target})
	if err != nil {
		t.Fatal(err)
	}
	return conditions.Find(o.(*targetsv1alpha1.Target).GetStatus().GetConditions(), hostd.TargetImagesPushed)
}

// TestAnImageBuiltHereRunsBeforeItIsPushed: an instance of an image built
// on the host runs at once, with no pull, while its push fails; the
// failure is reported on the target, and the image is tried again until
// the registry holds it.
func TestAnImageBuiltHereRunsBeforeItIsPushed(t *testing.T) {
	t.Parallel()
	w := hostdtest.New(t, hostdtest.Options{Host: fastPushRetry})
	w.Fake.Fail("images.Push", errRegistryDown)
	ref := build(t, w, "FROM "+hostdtest.Image+"\nRUN make\n")
	// A pull of it would never end.
	release := w.Fake.HoldPulls(ref)
	defer release()

	spec := container("")
	spec.Image = ref
	w.Apply(t, "built", spec)
	w.WaitPhase(t, "built", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	if w.Fake.InRegistry(ref) {
		t.Fatal("the registry holds an image whose every push failed")
	}
	testutil.Eventually(t, "the failed push reported", func() (bool, string) {
		c := imagesPushed(t, w)
		return c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE && c.GetReason() == "PushFailed" &&
			strings.Contains(c.GetMessage(), ref) && strings.Contains(c.GetMessage(), errRegistryDown.Error()), fmt.Sprint(c)
	})
	if p := w.Instance(t, "built").GetStatus().GetPhase(); p != envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING {
		t.Errorf("the instance is %v while its image's push fails", p)
	}

	w.Fake.Fail("images.Push", nil)
	testutil.Eventually(t, "the image pushed", func() (bool, string) {
		c := imagesPushed(t, w)
		return w.Fake.InRegistry(ref) && conditions.IsTrue([]*metav1.Condition{c}, hostd.TargetImagesPushed), fmt.Sprint(c)
	})
}

// TestAnImageNotPushedIsNotSwept: an image built on the host that nothing
// names stays while it is not pushed, past the sweep's grace; once pushed,
// it goes as any other.
func TestAnImageNotPushedIsNotSwept(t *testing.T) {
	t.Parallel()
	w := hostdtest.New(t, hostdtest.Options{Host: func(c *hostd.Config) {
		fastPushRetry(c)
		c.SweepGrace = 20 * time.Millisecond
	}})
	w.Fake.Fail("images.Push", errRegistryDown)
	ref := build(t, w, "FROM "+hostdtest.Image+"\nRUN unnamed\n")
	listed := func() bool {
		list, err := w.Fake.Drivers().Images.List(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(list, func(i driver.HostImage) bool { return slices.Contains(i.Refs, ref) })
	}
	for range 3 {
		if err := w.Host.Sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond) //nolint:forbidigo // the grace passing is what is tested: that the image stays after it
	}
	if !listed() {
		t.Fatal("an image not pushed was swept")
	}
	w.Fake.Fail("images.Push", nil)
	testutil.Eventually(t, "the image, pushed, swept", func() (bool, string) {
		if err := w.Host.Sweep(t.Context()); err != nil {
			t.Fatal(err)
		}
		return w.Fake.InRegistry(ref) && !listed(), ""
	})
}

// TestAHostdRestartedPushesWhatWasLeft: what a hostd built and did not
// push, another started on the host pushes.
func TestAHostdRestartedPushesWhatWasLeft(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "host.json")
	w := hostdtest.New(t, hostdtest.Options{StatePath: path, Host: fastPushRetry})
	w.Fake.Fail("images.Push", errRegistryDown)
	ref := build(t, w, "FROM "+hostdtest.Image+"\nRUN left\n")
	w.Restart(t, hostdtest.Options{StatePath: path, Host: fastPushRetry})
	testutil.Eventually(t, "the image pushed by the hostd started again", func() (bool, string) {
		return w.Fake.InRegistry(ref), ""
	})
}
