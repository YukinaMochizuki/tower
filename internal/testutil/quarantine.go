// Package testutil holds helpers shared by the tests of every package. It is
// imported only from tests.
package testutil

import (
	"os"
	"testing"
)

// RunQuarantined is the environment variable that, set to 1, runs the tests
// Quarantine would skip. The stress job (.github/workflows/stress.yml) sets it.
const RunQuarantined = "HANGAR_RUN_QUARANTINED"

// Quarantine skips a test known to be flaky, unless RunQuarantined is set.
// issue names the GitHub issue that tracks its root cause (for example
// "#80"); the test leaves quarantine when that issue is fixed, as
// docs/decisions/testing-strategy.md describes.
func Quarantine(t testing.TB, issue string) {
	t.Helper()
	if issue == "" {
		t.Fatal("a quarantined test names the issue that tracks it")
	}
	if os.Getenv(RunQuarantined) == "1" {
		return
	}
	t.Skipf("quarantined as flaky, tracked by %s; set %s=1 to run it", issue, RunQuarantined)
}
