package fake

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/provider"
)

// workspace is the fake's real /workspace of each instance: a directory
// under root, a git checkout of a real repository each mirror has, which
// tower-guest's boost subcommands, run in-process by guest, work on.
type workspaces struct {
	root  string
	guest func(args []string, stdout, stderr io.Writer) int
	// real is the real commit of each mirror's fake commit, fake the
	// fake commit of each real one.
	real, fake map[string]string
}

// RealWorkspaces makes each instance's /workspace from now on a real
// directory under root: Checkout clones a real repository of the mirror
// into it, UpdateOrigin fetches it, ReadFile reads it, Copy copies it, and
// Exec and StartProcess run tower-guest's boost subcommands
// (provider.GuestBinary) on it in-process by guest. Checkout answers the
// real commit, which the fake's mirrors know as the fake one.
func (f *Fake) RealWorkspaces(root string, guest func(args []string, stdout, stderr io.Writer) int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ws = &workspaces{root: root, guest: guest, real: map[string]string{}, fake: map[string]string{}}
}

// WorkspaceDir is the real directory of an instance's path p under
// /workspace, "" without RealWorkspaces or for a path outside it.
func (f *Fake) WorkspaceDir(in provider.Instance, p string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ws.path(in, p)
}

func (w *workspaces) path(in provider.Instance, p string) string {
	if w == nil {
		return ""
	}
	p = path.Clean(p)
	if p != "/workspace" && !strings.HasPrefix(p, "/workspace/") {
		return ""
	}
	return filepath.Join(w.root, in.Project, in.Name, filepath.FromSlash(p))
}

// gitIn runs git in dir as the fake's own user.
func gitIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=fake", "GIT_AUTHOR_EMAIL=fake@example.com", "GIT_COMMITTER_NAME=fake",
		"GIT_COMMITTER_EMAIL=fake@example.com", "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// realCommit is the real commit of the mirror's fake commit, made of the
// mirror's files when first asked for. The caller holds f.mu.
func (w *workspaces) realCommit(mirror string, m *repo, commit string) (string, string, error) {
	repoDir := filepath.Join(w.root, "mirrors", mirror)
	if c, ok := w.real[mirror+"@"+commit]; ok {
		return repoDir, c, nil
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		if _, err := gitIn(w.root, "init", "-q", "-b", "main", repoDir); err != nil {
			return "", "", err
		}
	}
	entries, _ := os.ReadDir(repoDir)
	for _, e := range entries {
		if e.Name() != ".git" {
			_ = os.RemoveAll(filepath.Join(repoDir, e.Name()))
		}
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for n := range m.files {
			if !yield(n) {
				return
			}
		}
	}) {
		file := filepath.Join(repoDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return "", "", err
		}
		if err := os.WriteFile(file, m.files[name], 0o644); err != nil {
			return "", "", err
		}
	}
	if _, err := gitIn(repoDir, "add", "-A"); err != nil {
		return "", "", err
	}
	if _, err := gitIn(repoDir, "commit", "-q", "--allow-empty", "-m", commit); err != nil {
		return "", "", err
	}
	c, err := gitIn(repoDir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	w.real[mirror+"@"+commit], w.fake[c] = c, commit
	return repoDir, c, nil
}

// checkout clones the mirror's real repository at commit into the
// instance's dir. The caller holds f.mu.
func (w *workspaces) checkout(in provider.Instance, mirror string, m *repo, commit, dir string) (string, error) {
	repoDir, real, err := w.realCommit(mirror, m, commit)
	if err != nil {
		return "", err
	}
	to := w.path(in, dir)
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(to, ".git")); err != nil {
		if _, err := gitIn(filepath.Dir(to), "clone", "-q", repoDir, to); err != nil {
			return "", err
		}
	} else if _, err := gitIn(to, "fetch", "-q", "origin"); err != nil {
		return "", err
	}
	if _, err := gitIn(to, "checkout", "-q", real); err != nil {
		return "", err
	}
	return real, nil
}

// guestArgs is a tower-guest command's arguments with each /workspace
// path the instance's real one.
func (w *workspaces) guestArgs(in provider.Instance, command []string) ([]string, bool) {
	if w == nil || len(command) < 2 || command[0] != provider.GuestBinary || !strings.HasPrefix(command[1], "boost-") {
		return nil, false
	}
	args := slices.Clone(command[1:])
	for i, a := range args {
		if p := w.path(in, a); p != "" {
			args[i] = p
		}
	}
	return args, true
}

// plainGuest is what tower-guest's boost subcommands answer without
// RealWorkspaces: a sync that sends nothing, of a tree the fake names;
// boost-job, in StartProcess, runs its command as if alone.
func plainGuest(w *workspaces, command []string) (string, bool) {
	if w != nil || len(command) < 2 || command[0] != provider.GuestBinary {
		return "", false
	}
	switch command[1] {
	case "boost-pack":
		return `{"tree":"fake-tree","full":true}` + "\n", true
	case "boost-apply":
		return `{"tree":"fake-tree"}` + "\n", true
	case "boost-clean":
		return "{}\n", true
	}
	return "", false
}

// withoutBoostJob is command without the boost-job it runs under, if it
// does: its flags come in pairs up to "--".
func withoutBoostJob(command []string) []string {
	if len(command) < 2 || command[0] != provider.GuestBinary || command[1] != "boost-job" {
		return command
	}
	i := 2
	for i+1 < len(command) && command[i] != "--" {
		i += 2
	}
	if i < len(command) && command[i] == "--" {
		return command[i+1:]
	}
	return command
}

// copyPath copies a real path to another, as Copy does: fromPath's tree
// at toPath, with modes and symlinks.
func copyPath(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	src := from
	if info, err := os.Lstat(from); err != nil {
		return err
	} else if info.IsDir() {
		if err := os.MkdirAll(to, 0o755); err != nil {
			return err
		}
		src = from + "/."
	}
	var errOut bytes.Buffer
	cmd := exec.Command("cp", "-a", src, to)
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cp: %w: %s", err, errOut.String())
	}
	return nil
}
