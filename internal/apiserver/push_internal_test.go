package apiserver

import (
	"strings"
	"testing"
)

// TestAPushCommitIsAFullName: a push's commit is empty or a full object
// name, which is what the checkout's branch is compared with as it is.
func TestAPushCommitIsAFullName(t *testing.T) {
	for _, c := range []string{"", strings.Repeat("a", 40), strings.Repeat("0", 64), "0123456789abcdef0123456789abcdef01234567"} {
		if err := checkCommit(c); err != nil {
			t.Errorf("checkCommit(%q): %v, want nil", c, err)
		}
	}
	for _, c := range []string{"abc1234", strings.Repeat("A", 40), strings.Repeat("a", 41), strings.Repeat("g", 40), "HEAD", "refs/heads/main", "-" + strings.Repeat("a", 39)} {
		if err := checkCommit(c); err == nil {
			t.Errorf("checkCommit(%q) passed, want refused", c)
		}
	}
}
