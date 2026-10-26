package gitmirror_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/drivertest"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/gitmirror"
)

// remote makes a bare repository with a few commits on main, as the
// suite's remote, and returns its path and its head.
func remote(t *testing.T) (string, string) {
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
	for i, content := range []string{"one\n", "two\n", "three\n"} {
		if err := os.WriteFile(filepath.Join(work, "go.mod"), []byte("module x\n// "+content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(work, "docker"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, "docker", "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		run(work, "add", "-A")
		run(work, "commit", "--quiet", "-m", "commit "+string(rune('a'+i)))
	}
	run(work, "push", "--quiet", bare, "main")
	return bare, run(work, "rev-parse", "HEAD")
}

// TestGitInADirectory holds the driver, its mirrors kept in a directory as
// in a hangar target's system container, to the git driver's contract.
func TestGitInADirectory(t *testing.T) {
	root := t.TempDir()
	d, err := gitmirror.New(t.Context(), gitmirror.Config{Root: filepath.Join(root, "mirrors"), ScratchDir: filepath.Join(root, "scratch")})
	if err != nil {
		t.Fatal(err)
	}
	r, head := remote(t)
	drivertest.RunGit(t, drivertest.Harness{Drivers: driver.Drivers{Git: d}, Remote: r, Branch: "main", RemoteHead: head})
}
