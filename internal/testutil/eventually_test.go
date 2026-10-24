package testutil_test

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// recorder stands in for the test a helper is given, so a test can see the
// helper fail without failing itself.
type recorder struct {
	testing.TB
	failure string
}

func (r *recorder) Helper() {}

func (r *recorder) Fatalf(format string, args ...any) {
	r.failure = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// run calls f with a recorder for t, as a test body, and returns how it
// failed, or "" if it did not.
func run(t *testing.T, f func(testing.TB)) string {
	r := &recorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f(r)
	}()
	<-done
	return r.failure
}

func TestEventuallyWaitsForTheCondition(t *testing.T) {
	var n atomic.Int32
	got := run(t, func(tb testing.TB) {
		testutil.Eventually(tb, "the third call", func() bool { return n.Add(1) >= 3 })
	})
	if got != "" || n.Load() != 3 {
		t.Errorf("failed with %q after %d calls; want success on the third", got, n.Load())
	}
}

func TestEventuallyWaitsInACleanup(t *testing.T) {
	var n atomic.Int32
	t.Run("cleanup", func(t *testing.T) {
		t.Cleanup(func() {
			testutil.Eventually(t, "the third call", func() bool { return n.Add(1) >= 3 })
		})
	})
	if n.Load() != 3 {
		t.Errorf("polled %d times in a cleanup; want 3", n.Load())
	}
}

func TestEventuallyReportsWhatItLastSaw(t *testing.T) {
	var n atomic.Int32
	got := run(t, func(tb testing.TB) {
		testutil.EventuallyWithin(tb, 50*time.Millisecond, "a miracle", func() (bool, string) {
			return false, fmt.Sprintf("call %d", n.Add(1))
		})
	})
	if want := fmt.Sprintf("a miracle; last seen: call %d", n.Load()); !strings.HasSuffix(got, want) {
		t.Errorf("failed with %q, want it to end with %q", got, want)
	}
}

func TestUnchangedFailsOnAChange(t *testing.T) {
	var n atomic.Int32
	got := run(t, func(tb testing.TB) {
		testutil.Unchanged(tb, "the disk", time.Second, func() (string, error) {
			return fmt.Sprint(min(n.Add(1), 3)), nil
		})
	})
	if want := "the disk changed: 1, then 2"; got != want {
		t.Errorf("failed with %q, want %q", got, want)
	}
	got = run(t, func(tb testing.TB) {
		testutil.Unchanged(tb, "the disk", 30*time.Millisecond, func() (string, error) { return "7", nil })
	})
	if got != "" {
		t.Errorf("an unchanging version failed: %q", got)
	}
}
