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
	head, err := g.Fetch(ctx, driver.FetchSpec{Mirror: mirror, URL: h.Remote, Branch: h.Branch})
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
		_, err = g.Checkout(ctx, "drivertest/never-fetched", "", io.Discard)
		is(t, "checking out a mirror never fetched", err, driver.ErrNotFound)
	})
	t.Run("Origin", func(t *testing.T) {
		var origin bytes.Buffer
		mustNot(t, "reading the origin", g.Origin(ctx, mirror, &origin))
		if len(files(t, origin.Bytes())) == 0 {
			t.Error("the origin holds nothing")
		}
		is(t, "the origin of a mirror never fetched", g.Origin(ctx, "drivertest/never-fetched", io.Discard), driver.ErrNotFound)
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
		spec := driver.PushSpec{Mirror: mirror, URL: h.Remote, Branch: h.Branch, Lease: "0000000"}
		is(t, "a push on a lease the branch is not at", g.Push(ctx, spec, gitDir()), driver.ErrLeaseLost)
		spec.Lease = head
		mustNot(t, "a push on its lease", g.Push(ctx, spec, gitDir()))
	})
}
