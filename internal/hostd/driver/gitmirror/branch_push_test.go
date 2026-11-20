package gitmirror_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/gitmirror"
)

// gitDirTar is a work tree's .git as CopyOut of it writes it (named .git/...).
func gitDirTar(t *testing.T, work string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	root := filepath.Join(work, ".git")
	err := filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(work, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// TestAPushSendsTheNamedBranchNotHEAD: a push names a branch, and the
// commit that branch is at in the checkout is what reaches the
// repository, wherever the checkout's HEAD is (another branch checked
// out, or the branch only in a worktree).
func TestAPushSendsTheNamedBranchNotHEAD(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	d, err := gitmirror.New(ctx, gitmirror.Config{Root: filepath.Join(root, "mirrors"), ScratchDir: filepath.Join(root, "scratch")})
	if err != nil {
		t.Fatal(err)
	}
	r := remote(t)
	if _, err := d.Fetch(ctx, driver.FetchSpec{Mirror: "web/api", URL: r.bare, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	// docs: one commit on main, never checked out in the work tree itself.
	r.run(r.work, "checkout", "--quiet", "-b", "docs", "main")
	if err := os.WriteFile(filepath.Join(r.work, "DOCS.md"), []byte("docs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.run(r.work, "add", "-A")
	r.run(r.work, "commit", "--quiet", "-m", "docs")
	docs := r.run(r.work, "rev-parse", "HEAD")
	// HEAD goes back to feature, as another session's work would leave it.
	r.run(r.work, "checkout", "--quiet", "feature")

	spec := driver.PushSpec{Mirror: "web/api", URL: r.bare, Branch: "docs", Lease: ""}

	// Pinned to a commit the branch is not at (HEAD's): refused, and the
	// repository has no docs.
	spec.Commit = r.featureHead
	if err := d.Push(ctx, spec, gitDirTar(t, r.work)); err == nil {
		t.Error("a push pinned to a commit the branch is not at went")
	}
	if out, err := exec.Command("git", "-C", r.bare, "rev-parse", "--verify", "--quiet", "refs/heads/docs").Output(); err == nil {
		t.Errorf("a refused push left docs at %s", out)
	}

	// A branch the checkout lacks is not found, whatever HEAD is.
	missing := spec
	missing.Branch, missing.Commit = "nowhere", ""
	if err := d.Push(ctx, missing, gitDirTar(t, r.work)); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a push of a branch the checkout lacks: %v, want ErrNotFound", err)
	}

	// Unpinned, and pinned to its own commit: docs' commit lands, not HEAD's.
	// The second push is onto the first, so its lease is docs itself.
	for i, pin := range []string{"", docs} {
		spec.Commit = pin
		if i > 0 {
			spec.Lease = docs
		}
		if err := d.Push(ctx, spec, gitDirTar(t, r.work)); err != nil {
			t.Fatalf("pinned to %q: %v", pin, err)
		}
		if got := r.run(r.bare, "rev-parse", "refs/heads/docs"); got != docs {
			t.Errorf("pinned to %q: the repository's docs is at %s, want docs' own %s (feature, HEAD, is %s)", pin, got, docs, r.featureHead)
		}
	}
}
