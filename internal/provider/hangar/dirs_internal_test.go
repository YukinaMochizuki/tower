package hangar

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// TestTheUserIsGivenTheRepositoriesDirectory runs the shell installGuest
// runs as root against directories of a temporary root: an empty
// workspace and a repos directory, made if missing, are given to the
// container's user, so that tower-guest, running as that user, can make a
// box's later checkouts under /repos; a directory that holds something is
// left as it is. Ownership is checked when the test runs as root, which
// alone may give a file away.
func TestTheUserIsGivenTheRepositoriesDirectory(t *testing.T) {
	t.Parallel()
	owner, uid := "4321:4321", 4321
	if os.Getuid() != 0 {
		owner, uid = "", os.Getuid()
	}
	run := func(t *testing.T, root string) {
		t.Helper()
		o := owner
		if o == "" {
			o = "$(id -u):$(id -g)"
		}
		line := giveDirsLine(o, filepath.Join(root, "workspace"), filepath.Join(root, "repos"))
		if out, err := exec.Command("/bin/sh", "-c", "set -e; "+line).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", line, err, out)
		}
	}
	ownedBy := func(t *testing.T, dir string) int {
		t.Helper()
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		return int(st.Sys().(*syscall.Stat_t).Uid)
	}

	t.Run("empty and missing", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "workspace"), 0o755); err != nil {
			t.Fatal(err)
		}
		run(t, root)
		for _, d := range []string{"workspace", "repos"} {
			if got := ownedBy(t, filepath.Join(root, d)); got != uid {
				t.Errorf("%s is %d's, want %d's", d, got, uid)
			}
		}
	})
	t.Run("holding something", func(t *testing.T) {
		if os.Getuid() != 0 {
			t.Skip("only root can give a directory away, so nothing tells left alone from given")
		}
		root := t.TempDir()
		for _, d := range []string{"workspace/x", "repos/y"} {
			if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		run(t, root)
		for _, d := range []string{"workspace", "repos"} {
			if got := ownedBy(t, filepath.Join(root, d)); got != 0 {
				t.Errorf("%s, which holds something, was given to %d", d, got)
			}
		}
	})
}
