package fake

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// A fake checkout's .git: its HEAD commit, the mirror it came from, and
// the commit its origin's branch is at.
const (
	gitHEAD   = ".git/HEAD"
	gitOrigin = ".git/origin"
	// OriginRef is the file of a checkout holding the commit its origin's
	// branch is at.
	OriginRef = ".git/refs/remotes/origin/HEAD"
)

type git struct{ h *Host }

func (g git) Version(context.Context) (string, error) { return Version, nil }

func (g git) Fetch(_ context.Context, spec driver.FetchSpec) (string, error) {
	var head string
	err := g.h.change("git.Fetch", func() error {
		r := g.h.st.Remotes[spec.URL]
		if r == nil {
			// A repository the test said nothing of has one commit.
			sum := sha256.Sum256([]byte(spec.URL))
			r = &repo{Branch: spec.Branch, Commits: []string{hex.EncodeToString(sum[:])[:12]}, Files: map[string][]byte{"README": []byte(spec.URL)}}
			if r.Branch == "" {
				r.Branch = "main"
			}
			g.h.st.Remotes[spec.URL] = r
		}
		if spec.Branch != "" && spec.Branch != r.Branch {
			return fmt.Errorf("branch %q of %s: %w", spec.Branch, spec.URL, driver.ErrNotFound)
		}
		g.h.st.Mirrors[spec.Mirror] = &repo{Branch: r.Branch, Commits: slices.Clone(r.Commits), Files: maps.Clone(r.Files)}
		head = r.head()
		return nil
	})
	return head, err
}

// mirror is a mirror fetched. The caller holds h.mu.
func (g git) mirror(name string) (*repo, error) {
	m := g.h.st.Mirrors[name]
	if m == nil {
		return nil, fmt.Errorf("mirror %s: %w", name, driver.ErrNotFound)
	}
	return m, nil
}

func (g git) Checkout(_ context.Context, mirror, ref string, w io.Writer) (string, error) {
	var files map[string][]byte
	var commit string
	err := g.h.read("git.Checkout", func() error {
		m, err := g.mirror(mirror)
		if err != nil {
			return err
		}
		commit = m.head()
		if ref != "" && ref != m.Branch {
			if !slices.Contains(m.Commits, ref) {
				return fmt.Errorf("ref %q in mirror %s: %w", ref, mirror, driver.ErrNotFound)
			}
			commit = ref
		}
		files = maps.Clone(m.Files)
		files[gitHEAD] = []byte(commit)
		files[gitOrigin] = []byte(mirror)
		files[OriginRef] = []byte(m.head())
		return nil
	})
	if err != nil {
		return "", err
	}
	return commit, writeTar(w, files)
}

func (g git) Origin(_ context.Context, mirror string, w io.Writer) error {
	var head string
	err := g.h.read("git.Origin", func() error {
		m, err := g.mirror(mirror)
		if err == nil {
			head = m.head()
		}
		return err
	})
	if err != nil {
		return err
	}
	return writeTar(w, map[string][]byte{"refs/remotes/origin/HEAD": []byte(head)})
}

func (g git) Push(_ context.Context, spec driver.PushSpec, gitDir io.Reader) error {
	b, err := io.ReadAll(gitDir)
	if err != nil {
		return err
	}
	files, err := untar(b)
	if err != nil {
		return err
	}
	commit, ok := files[gitHEAD]
	if !ok {
		return fmt.Errorf("the checkout's .git holds no HEAD")
	}
	return g.h.change("git.Push", func() error {
		if _, err := g.mirror(spec.Mirror); err != nil {
			return err
		}
		r := g.h.st.Remotes[spec.URL]
		if r == nil {
			return fmt.Errorf("repository %s: %w", spec.URL, driver.ErrNotFound)
		}
		at := ""
		if spec.Branch == r.Branch {
			at = r.head()
		}
		if at != spec.Lease {
			return fmt.Errorf("branch %s is at %q, not %q: %w", spec.Branch, at, spec.Lease, driver.ErrLeaseLost)
		}
		if spec.Branch == r.Branch {
			r.Commits = append(r.Commits, string(bytes.TrimSpace(commit)))
		}
		return nil
	})
}

func (g git) CommitsBehind(_ context.Context, mirror, commit, branch string) (int, error) {
	var n int
	err := g.h.read("git.CommitsBehind", func() error {
		m, err := g.mirror(mirror)
		if err != nil {
			return err
		}
		if branch != "" && branch != m.Branch {
			return fmt.Errorf("branch %q of mirror %s: %w", branch, mirror, driver.ErrNotFound)
		}
		i := slices.Index(m.Commits, commit)
		if i < 0 {
			n = len(m.Commits)
		} else {
			n = len(m.Commits) - 1 - i
		}
		return nil
	})
	return n, err
}
