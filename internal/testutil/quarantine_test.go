package testutil_test

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

func TestQuarantineSkipsUnlessAskedToRun(t *testing.T) {
	t.Setenv(testutil.RunQuarantined, "")
	ran := false
	t.Run("quarantined", func(t *testing.T) {
		testutil.Quarantine(t, "#1")
		ran = true
	})
	if ran {
		t.Error("a quarantined test ran without " + testutil.RunQuarantined)
	}

	t.Setenv(testutil.RunQuarantined, "1")
	t.Run("asked to run", func(t *testing.T) {
		testutil.Quarantine(t, "#1")
		ran = true
	})
	if !ran {
		t.Error("a quarantined test did not run with " + testutil.RunQuarantined + "=1")
	}
}
