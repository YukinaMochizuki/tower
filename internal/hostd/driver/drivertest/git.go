package drivertest

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// RunGit runs the git driver's suite.
func RunGit(t *testing.T, h Harness) {
	g, ctx := h.Drivers.Git, t.Context()
	mirror := "drivertest/" + name(t, "mirror")
	// A token reads and writes the repository; none of it may reach a
	// checkout or a guest.
	token := "drivertest-token-" + name(t, "")
	head, err := g.Fetch(ctx, driver.FetchSpec{Mirror: mirror, URL: h.Remote, Token: token, Branch: h.Branch})
	mustNot(t, "fetching", err)
	if head == "" || (h.RemoteHead != "" && head != h.RemoteHead) {
		t.Errorf("fetched to %q, want %q", head, h.RemoteHead)
	}
	t.Run("CommitsBehind", func(t *testing.T) {
		if n, err := g.CommitsBehind(ctx, mirror, head, h.Branch); err != nil || n != 0 {
			t.Errorf("the head is %d commits behind its branch, %v", n, err)
		}
		_, err := g.CommitsBehind(ctx, "drivertest/never-fetched", head, h.Branch)
		is(t, "a mirror never fetched", err, driver.ErrNotFound)
	})
	var checkout bytes.Buffer
	t.Run("Checkout", func(t *testing.T) {
		commit, err := g.Checkout(ctx, mirror, "", &checkout)
		mustNot(t, "checking out", err)
		if commit != head {
			t.Errorf("checked out %q, want %q", commit, head)
		}
		got := files(t, checkout.Bytes())
		hasGit := false
		for name := range got {
			hasGit = hasGit || strings.HasPrefix(name, ".git/")
		}
		if !hasGit || len(got) < 2 {
			t.Errorf("the checkout holds %d files and .git %v; want its work tree and its .git", len(got), hasGit)
		}
		for name, content := range got {
			if strings.Contains(content, token) {
				t.Errorf("the checkout's %s holds the repository's token", name)
			}
		}
		// What a guest sees of it once extracted.
		c := h.containers()
		guest := c.guestWithDisk(t, h, "checkout")
		mustNot(t, "extracting the checkout", c.Driver.CopyIn(ctx, guest, c.Workspace, bytes.NewReader(checkout.Bytes())))
		var seen bytes.Buffer
		mustNot(t, "reading the guest's workspace", c.Driver.CopyOut(ctx, guest, c.Workspace, &seen))
		if bytes.Contains(seen.Bytes(), []byte(token)) {
			t.Error("the guest's checkout holds the repository's token")
		}
		_, err = g.Checkout(ctx, "drivertest/never-fetched", "", io.Discard)
		is(t, "checking out a mirror never fetched", err, driver.ErrNotFound)
	})
	t.Run("Origin", func(t *testing.T) {
		var origin bytes.Buffer
		mustNot(t, "reading the origin", g.Origin(ctx, mirror, &origin))
		if bytes.Contains(origin.Bytes(), []byte(token)) {
			t.Error("the origin holds the repository's token")
		}
		if len(files(t, origin.Bytes())) == 0 {
			t.Error("the origin holds nothing")
		}
		is(t, "the origin of a mirror never fetched", g.Origin(ctx, "drivertest/never-fetched", io.Discard), driver.ErrNotFound)
	})
	t.Run("ArchiveAndRead", func(t *testing.T) {
		var archive bytes.Buffer
		commit, err := g.Archive(ctx, mirror, "", &archive)
		mustNot(t, "archiving", err)
		if commit != head {
			t.Errorf("archived %q, want %q", commit, head)
		}
		tree := files(t, archive.Bytes())
		var file string
		for name := range tree {
			if strings.HasPrefix(name, ".git/") || name == ".git" {
				t.Errorf("the archive holds %s", name)
			}
			if file == "" || name < file {
				file = name
			}
		}
		if file == "" {
			t.Fatal("the archive holds no file")
		}
		if bytes.Contains(archive.Bytes(), []byte(token)) {
			t.Error("the archive holds the repository's token")
		}
		commit, read, err := g.Read(ctx, mirror, head, []string{file, "", "./" + file, "no/such/path"}, 1<<20)
		mustNot(t, "reading", err)
		f, root := read[file], read[""]
		if commit != head || f.Dir || f.Object == "" || string(f.Content) != tree[file] || f.Size != int64(len(tree[file])) {
			t.Errorf("read %s at %q as %+v, want its content %q", file, commit, f, tree[file])
		}
		if !root.Dir || root.Object == "" || root.Object == f.Object || read["./"+file].Object != f.Object {
			t.Errorf("read the root as %+v and ./%s as %+v", root, file, read["./"+file])
		}
		if _, ok := read["no/such/path"]; ok {
			t.Error("a path not there was read")
		}
		_, sized, err := g.Read(ctx, mirror, "", []string{file}, 0)
		mustNot(t, "reading without content", err)
		if e := sized[file]; e.Content != nil || e.Object != f.Object {
			t.Errorf("read without content as %+v", e)
		}
		if _, _, err := g.Read(ctx, mirror, "", []string{"../outside"}, 0); err == nil {
			t.Error("a path out of the work tree was read")
		}
		_, _, err = g.Read(ctx, mirror, "no-such-ref", []string{file}, 0)
		is(t, "reading a ref the mirror lacks", err, driver.ErrNotFound)
		_, _, err = g.Read(ctx, "drivertest/never-fetched", "", []string{file}, 0)
		is(t, "reading a mirror never fetched", err, driver.ErrNotFound)
		_, err = g.Archive(ctx, "drivertest/never-fetched", "", io.Discard)
		is(t, "archiving a mirror never fetched", err, driver.ErrNotFound)
	})
	t.Run("PushNeedsItsLease", func(t *testing.T) {
		gitDir := func() io.Reader {
			// The checkout's .git, as CopyOut of it writes it.
			keep := map[string]string{}
			for name, content := range files(t, checkout.Bytes()) {
				if strings.HasPrefix(name, ".git/") {
					keep[name] = content
				}
			}
			return tarOf(t, keep)
		}
		spec := driver.PushSpec{Mirror: mirror, URL: h.Remote, Token: token, Branch: h.Branch, Lease: "0000000"}
		is(t, "a push on a lease the branch is not at", g.Push(ctx, spec, gitDir()), driver.ErrLeaseLost)
		spec.Lease = head
		mustNot(t, "a push on its lease", g.Push(ctx, spec, gitDir()))
		var after bytes.Buffer
		_, err := g.Checkout(ctx, mirror, "", &after)
		mustNot(t, "checking out after a push", err)
		if bytes.Contains(after.Bytes(), []byte(token)) {
			t.Error("a checkout after a push holds the repository's token")
		}
	})
}
