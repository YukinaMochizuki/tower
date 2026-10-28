package gitmirror_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/drivertest"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/gitmirror"
)

// testRemote is the suite's remote: a bare repository with a few commits
// on main and one more on feature, and a work tree to change it from.
type testRemote struct {
	bare, work, head, featureHead string
	run                           func(dir string, args ...string) string
}

// remote makes the suite's remote.
func remote(t *testing.T) testRemote {
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
	head := run(work, "rev-parse", "HEAD")
	run(work, "checkout", "--quiet", "-b", "feature")
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "-A")
	run(work, "commit", "--quiet", "-m", "a feature")
	run(work, "push", "--quiet", bare, "feature")
	return testRemote{bare: bare, work: work, head: head, featureHead: run(work, "rev-parse", "HEAD"), run: run}
}

// TestGitInADirectory holds the driver, its mirrors kept in a directory as
// in a hangar target's system container, to the git driver's contract.
func TestGitInADirectory(t *testing.T) {
	root := t.TempDir()
	d, err := gitmirror.New(t.Context(), gitmirror.Config{Root: filepath.Join(root, "mirrors"), ScratchDir: filepath.Join(root, "scratch")})
	if err != nil {
		t.Fatal(err)
	}
	r := remote(t)
	drivertest.RunGit(t, drivertest.Harness{
		Drivers: driver.Drivers{Git: d}, Remote: r.bare, Branch: "main", RemoteHead: r.head,
		OtherBranch: "feature", OtherHead: r.featureHead,
	})
}

// TestAMirrorFollowsTheRepositorysBranches: a fetch takes every branch,
// the one followed staying its HEAD, and drops one the repository has
// dropped; a followed branch the repository lacks is not found.
func TestAMirrorFollowsTheRepositorysBranches(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	d, err := gitmirror.New(ctx, gitmirror.Config{Root: filepath.Join(root, "mirrors"), ScratchDir: filepath.Join(root, "scratch")})
	if err != nil {
		t.Fatal(err)
	}
	r := remote(t)
	branches := func() map[string]string {
		t.Helper()
		var origin bytes.Buffer
		if err := d.Origin(ctx, "web/api", &origin); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		tr := tar.NewReader(&origin)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				return got
			}
			if err != nil {
				t.Fatal(err)
			}
			if name, ok := strings.CutPrefix(hdr.Name, "refs/remotes/origin/"); ok {
				b, _ := io.ReadAll(tr)
				got[name] = strings.TrimSpace(string(b))
			}
		}
	}
	fetch := func() string {
		t.Helper()
		head, err := d.Fetch(ctx, driver.FetchSpec{Mirror: "web/api", URL: r.bare, Branch: "main"})
		if err != nil {
			t.Fatal(err)
		}
		return head
	}

	if head := fetch(); head != r.head {
		t.Errorf("fetched to %s, want main's %s", head, r.head)
	}
	if got := branches(); got["main"] != r.head || got["feature"] != r.featureHead || len(got) != 2 {
		t.Errorf("the mirror's branches: %v", got)
	}
	// Followed: a checkout of no ref is main's.
	if commit, err := d.Checkout(ctx, "web/api", "", io.Discard); err != nil || commit != r.head {
		t.Errorf("a checkout of no ref: %s, %v; want main's %s", commit, err, r.head)
	}

	// The repository drops feature and gains another.
	r.run(r.work, "push", "--quiet", r.bare, "--delete", "feature")
	r.run(r.work, "push", "--quiet", r.bare, "feature:later")
	fetch()
	if got := branches(); got["feature"] != "" || got["later"] != r.featureHead {
		t.Errorf("after the repository dropped feature and gained later: %v", got)
	}

	// The branch followed gone from the repository is a branch not found.
	if _, err := d.Fetch(ctx, driver.FetchSpec{Mirror: "web/api", URL: r.bare, Branch: "gone"}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("following a branch the repository lacks: %v, want ErrNotFound", err)
	}
}
