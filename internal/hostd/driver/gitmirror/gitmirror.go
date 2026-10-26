// Package gitmirror is hostd's git driver (driver.Git): a bare mirror per
// repository, each on a ZFS dataset of its own under Dataset, through the
// git command (docs/decisions/repositories.md). A token reaches git only
// as the environment of the one command that fetches or pushes with it,
// as an HTTP header (http.extraHeader through GIT_CONFIG_*): no mirror, no
// checkout and no guest ever holds it. A checkout's origin is the mirror,
// by a name of tower's (tower://<mirror>), which reaches nothing from the
// guest: tower brings it up to the mirror itself (Origin). A push takes
// the objects and refs of a checkout's .git alone into a repository of
// the driver's own, never running git in what the guest wrote.
package gitmirror

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/guest/tarx"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/keylock"
)

// propMirror is a mirror's name, on its dataset.
const propMirror = "tower:mirror"

// Config configures the driver.
type Config struct {
	// Dataset is the parent of the mirrors' datasets, such as
	// tower/hostd/mirrors, made if need be; MountRoot is where they are
	// mounted.
	Dataset, MountRoot string
	// ScratchDir is a directory, root's alone, for checkouts and pushes
	// being made.
	ScratchDir string
}

// Driver is the git driver. It is safe for concurrent use.
type Driver struct {
	cfg   Config
	locks keylock.Map
}

var _ driver.Git = (*Driver)(nil)

// New makes the mirrors' dataset and the scratch directory.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	if err := os.MkdirAll(cfg.ScratchDir, 0o700); err != nil {
		return nil, err
	}
	if _, err := run(ctx, "", nil, "zfs", "list", "-H", "-o", "name", cfg.Dataset); err != nil {
		if _, err := run(ctx, "", nil, "zfs", "create", "-p", "-o", "mountpoint="+cfg.MountRoot, cfg.Dataset); err != nil {
			return nil, fmt.Errorf("the mirrors' dataset: %w", err)
		}
	}
	return &Driver{cfg: cfg}, nil
}

func (d *Driver) Version(ctx context.Context) (string, error) {
	out, err := git(ctx, "", nil, "version")
	return strings.TrimSpace(out), err
}

// mirrorName is what a mirror's name may be: repository names, such as
// web/api.
var mirrorName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)

// dataset is a mirror's dataset: its name with / as _ and a short hash,
// as a dataset's name takes one component.
func (d *Driver) dataset(mirror string) (string, error) {
	if !mirrorName.MatchString(mirror) || len(mirror) > 200 {
		return "", fmt.Errorf("%q cannot name a mirror", mirror)
	}
	return d.cfg.Dataset + "/" + strings.ReplaceAll(mirror, "/", "_") + "-" + shortHash(mirror), nil
}

func shortHash(s string) string {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h = (h ^ uint32(s[i])) * 16777619
	}
	return strconv.FormatUint(uint64(h), 36)
}

// repo is a mirror's bare repository, "" if the mirror was never made.
func (d *Driver) repo(ctx context.Context, mirror string) (string, error) {
	ds, err := d.dataset(mirror)
	if err != nil {
		return "", err
	}
	out, err := run(ctx, "", nil, "zfs", "get", "-H", "-o", "value", propMirror+",mountpoint", ds)
	if err != nil {
		return "", nil //nolint:nilerr // a dataset not there is a mirror never made
	}
	vals := strings.Split(strings.TrimSpace(out), "\n")
	if len(vals) != 2 || vals[0] != mirror {
		return "", nil
	}
	r := filepath.Join(vals[1], "repo.git")
	if _, err := os.Stat(filepath.Join(r, "HEAD")); err != nil {
		return "", nil //nolint:nilerr // a mirror not yet made whole
	}
	return r, nil
}

// mustRepo is a mirror's repository; one never fetched is ErrNotFound.
func (d *Driver) mustRepo(ctx context.Context, mirror string) (string, error) {
	r, err := d.repo(ctx, mirror)
	if err == nil && r == "" {
		err = fmt.Errorf("mirror %s: %w", mirror, driver.ErrNotFound)
	}
	return r, err
}

// tokenEnv is the environment a command reaching a repository with token
// is given: the token as an HTTP header, and no credential helper or
// prompt. Empty for no token.
func tokenEnv(token string) []string {
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0="}
	if token == "" {
		return env
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=http.extraHeader", "GIT_CONFIG_VALUE_1=Authorization: Basic " + basic}
}

func (d *Driver) Fetch(ctx context.Context, spec driver.FetchSpec) (string, error) {
	ds, err := d.dataset(spec.Mirror)
	if err != nil {
		return "", err
	}
	unlock := d.locks.Lock(spec.Mirror)
	defer unlock()
	r, err := d.repo(ctx, spec.Mirror)
	if err != nil {
		return "", err
	}
	if r == "" {
		mnt := filepath.Join(d.cfg.MountRoot, filepath.Base(ds))
		if _, err := run(ctx, "", nil, "zfs", "list", "-H", ds); err != nil {
			if _, err := run(ctx, "", nil, "zfs", "create", "-o", "mountpoint="+mnt, "-o", propMirror+"="+spec.Mirror, ds); err != nil {
				return "", err
			}
		}
		r = filepath.Join(mnt, "repo.git")
		if _, err := git(ctx, "", nil, "init", "--quiet", "--bare", r); err != nil {
			return "", err
		}
	}
	env := tokenEnv(spec.Token)
	branch := spec.Branch
	if branch == "" {
		out, err := git(ctx, r, env, "ls-remote", "--symref", "--", spec.URL, "HEAD")
		if err != nil {
			return "", redact(err, spec.Token)
		}
		for _, line := range strings.Split(out, "\n") {
			if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
				branch, _, _ = strings.Cut(rest, "\t")
			}
		}
		if branch == "" {
			return "", fmt.Errorf("%s names no default branch", spec.URL)
		}
	}
	if _, err := git(ctx, r, env, "fetch", "--quiet", "--no-tags", "--prune", "--", spec.URL, "+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
		if strings.Contains(err.Error(), "couldn't find remote ref") {
			return "", fmt.Errorf("branch %s of %s: %w", branch, spec.URL, driver.ErrNotFound)
		}
		return "", redact(err, spec.Token)
	}
	// The branch the mirror follows: its HEAD, what a checkout of no ref
	// takes.
	if _, err := git(ctx, r, nil, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
		return "", err
	}
	head, err := git(ctx, r, nil, "rev-parse", "--verify", "refs/heads/"+branch)
	return strings.TrimSpace(head), err
}

// redact takes a token out of an error, should git have quoted it, as
// itself or as the header it is sent in.
func redact(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	msg := err.Error()
	for _, s := range []string{token, base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))} {
		msg = strings.ReplaceAll(msg, s, "[token]")
	}
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

// commitOf resolves ref in the mirror: a branch, a tag or a commit; empty
// is the branch it follows.
func commitOf(ctx context.Context, r, ref string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	for _, try := range []string{"refs/heads/" + ref, "refs/tags/" + ref, ref} {
		if out, err := git(ctx, r, nil, "rev-parse", "--verify", "--quiet", try+"^{commit}"); err == nil {
			return strings.TrimSpace(out), nil
		}
	}
	return "", fmt.Errorf("ref %q: %w", ref, driver.ErrNotFound)
}

func (d *Driver) Checkout(ctx context.Context, mirror, ref string, w io.Writer) (string, error) {
	r, err := d.mustRepo(ctx, mirror)
	if err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(d.cfg.ScratchDir, "checkout-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	unlock := d.locks.Lock(mirror)
	commit, err := commitOf(ctx, r, ref)
	if err == nil {
		_, err = git(ctx, "", nil, "clone", "--quiet", "--no-hardlinks", "--no-checkout", "--", r, filepath.Join(tmp, "co"))
	}
	unlock()
	if err != nil {
		return "", err
	}
	co := filepath.Join(tmp, "co")
	if _, err := git(ctx, co, nil, "checkout", "--quiet", "--detach", commit); err != nil {
		return "", err
	}
	// On its branch, when that is what was asked.
	if b, _ := git(ctx, r, nil, "symbolic-ref", "--short", "HEAD"); ref == "" || ref == strings.TrimSpace(b) {
		branch := strings.TrimSpace(b)
		if _, err := git(ctx, co, nil, "checkout", "--quiet", "-B", branch, "--track", "origin/"+branch); err != nil {
			return "", err
		}
	}
	if _, err := git(ctx, co, nil, "remote", "set-url", "origin", "tower://"+mirror); err != nil {
		return "", err
	}
	return commit, tarx.WriteDir(w, co)
}

// Origin writes a tar that, extracted into a checkout's .git, brings its
// origin/* up to the mirror: a pack of every object of the mirror, and a
// ref of each of its branches under refs/remotes/origin.
func (d *Driver) Origin(ctx context.Context, mirror string, w io.Writer) error {
	r, err := d.mustRepo(ctx, mirror)
	if err != nil {
		return err
	}
	// Beside the repository: git writes the pack there and renames it in
	// place, which crosses no filesystem.
	tmp, err := os.MkdirTemp(filepath.Dir(r), "origin-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	unlock := d.locks.Lock(mirror)
	refs, err := git(ctx, r, nil, "for-each-ref", "--format=%(objectname) %(refname:strip=2)", "refs/heads/")
	if err == nil {
		_, err = gitIn(ctx, r, nil, strings.NewReader(""), "pack-objects", "--all", "--revs", "--quiet", filepath.Join(tmp, "pack"))
	}
	unlock()
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	packs, err := filepath.Glob(filepath.Join(tmp, "pack-*"))
	if err != nil {
		return err
	}
	for _, p := range packs {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := addFile(tw, "objects/pack/"+filepath.Base(p), b, 0o444); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(refs), "\n") {
		sha, name, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if err := addFile(tw, "refs/remotes/origin/"+name, []byte(sha+"\n"), 0o644); err != nil {
			return err
		}
	}
	return tw.Close()
}

func addFile(tw *tar.Writer, name string, data []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// Push takes the commits of a checkout's .git into the mirror and pushes
// its HEAD to the branch, with --force-with-lease on Lease: a lease the
// mirror does not know as a commit is one the branch is not at.
func (d *Driver) Push(ctx context.Context, spec driver.PushSpec, gitDir io.Reader) error {
	r, err := d.mustRepo(ctx, spec.Mirror)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(d.cfg.ScratchDir, "push-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	in := filepath.Join(tmp, "in.git")
	if _, err := git(ctx, "", nil, "init", "--quiet", "--bare", in); err != nil {
		return err
	}
	if err := takeObjects(gitDir, in); err != nil {
		return err
	}
	commit, err := git(ctx, in, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("the checkout's HEAD: %w", err)
	}
	commit = strings.TrimSpace(commit)
	if _, err := git(ctx, in, nil, "update-ref", "refs/tower/push", commit); err != nil {
		return err
	}
	unlock := d.locks.Lock(spec.Mirror)
	defer unlock()
	if _, err := git(ctx, r, nil, "fetch", "--quiet", "--no-tags", "--", in, "+refs/tower/push:refs/tower/push"); err != nil {
		return err
	}
	lease := ""
	if spec.Lease != "" {
		full, err := git(ctx, r, nil, "rev-parse", "--verify", "--quiet", spec.Lease+"^{commit}")
		if err != nil {
			return fmt.Errorf("branch %s: its lease %s is no commit the mirror has: %w", spec.Branch, spec.Lease, driver.ErrLeaseLost)
		}
		lease = strings.TrimSpace(full)
	}
	ref := "refs/heads/" + spec.Branch
	_, err = git(ctx, r, tokenEnv(spec.Token), "push", "--quiet", "--force-with-lease="+ref+":"+lease, "--", spec.URL, commit+":"+ref)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "stale info") || strings.Contains(msg, "rejected") || strings.Contains(msg, "already exists") {
			return fmt.Errorf("branch %s: %w: %w", spec.Branch, redact(err, spec.Token), driver.ErrLeaseLost)
		}
		return redact(err, spec.Token)
	}
	// The mirror holds what the repository now has.
	_, err = git(ctx, r, nil, "update-ref", ref, commit)
	return err
}

// takeObjects extracts into repo, a bare repository of the driver's, the
// objects and refs of a .git as CopyOut of it writes it (named .git/...):
// nothing else of it, its config and hooks the guest's to write, ever
// reaches the host.
func takeObjects(gitDir io.Reader, repo string) error {
	pr, pw := io.Pipe()
	go func() {
		tr := tar.NewReader(gitDir)
		tw := tar.NewWriter(pw)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				pw.CloseWithError(tw.Close())
				return
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			name, ok := strings.CutPrefix(path.Clean(hdr.Name), ".git/")
			if !ok {
				continue
			}
			// Not objects/info: its alternates and http-alternates would
			// have git read objects from wherever the guest names.
			keep := name == "HEAD" || name == "packed-refs" || strings.HasPrefix(name, "refs/") ||
				(strings.HasPrefix(name, "objects/") && name != "objects/info" && !strings.HasPrefix(name, "objects/info/"))
			if !keep || (hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir) {
				continue
			}
			hdr.Name = name
			if err := tw.WriteHeader(hdr); err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := io.Copy(tw, tr); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
	}()
	err := tarx.Extract(pr, repo)
	_ = pr.CloseWithError(errors.New("extracted"))
	return err
}

func (d *Driver) CommitsBehind(ctx context.Context, mirror, commit, branch string) (int, error) {
	r, err := d.mustRepo(ctx, mirror)
	if err != nil {
		return 0, err
	}
	ref := "HEAD"
	if branch != "" {
		ref = "refs/heads/" + branch
		if _, err := git(ctx, r, nil, "rev-parse", "--verify", "--quiet", ref); err != nil {
			return 0, fmt.Errorf("branch %s of mirror %s: %w", branch, mirror, driver.ErrNotFound)
		}
	}
	args := []string{"rev-list", "--count", ref}
	if _, err := git(ctx, r, nil, "rev-parse", "--verify", "--quiet", commit+"^{commit}"); err == nil {
		args = append(args, "^"+commit)
	}
	out, err := git(ctx, r, nil, args...)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

func (d *Driver) Archive(ctx context.Context, mirror, ref string, w io.Writer) (string, error) {
	r, err := d.mustRepo(ctx, mirror)
	if err != nil {
		return "", err
	}
	// The commit's objects stay as they are while a fetch moves its refs.
	unlock := d.locks.Lock(mirror)
	commit, err := commitOf(ctx, r, ref)
	unlock()
	if err != nil {
		return "", err
	}
	var errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "-C", r, "archive", "--format=tar", commit)
	cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=/nonexistent", "PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
	cmd.Stdout, cmd.Stderr = w, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git archive: %w: %s", err, strings.TrimSpace(errOut.String()))
	}
	return commit, nil
}

// treePath is a path of a work tree as git names it, "" for its root; one
// leaving the tree is no path.
func treePath(p string) (string, error) {
	c := path.Clean(p)
	switch {
	case c == ".":
		return "", nil
	case path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../"):
		return "", fmt.Errorf("%q leaves the work tree", p)
	}
	return c, nil
}

func (d *Driver) Read(ctx context.Context, mirror, ref string, paths []string, maxContent int64) (string, map[string]driver.MirrorEntry, error) {
	r, err := d.mustRepo(ctx, mirror)
	if err != nil {
		return "", nil, err
	}
	unlock := d.locks.Lock(mirror)
	commit, err := commitOf(ctx, r, ref)
	unlock()
	if err != nil {
		return "", nil, err
	}
	// One batch-check of <commit>:<path> lines says what each path is: its
	// object, its type and its size; a path not there is missing.
	var query strings.Builder
	for _, p := range paths {
		tp, err := treePath(p)
		if err != nil {
			return "", nil, err
		}
		if strings.ContainsAny(tp, "\n\x00") {
			return "", nil, fmt.Errorf("%q cannot name a path", p)
		}
		fmt.Fprintf(&query, "%s:%s\n", commit, tp)
	}
	checked, err := gitIn(ctx, r, nil, strings.NewReader(query.String()), "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)")
	if err != nil {
		return "", nil, err
	}
	lines := strings.Split(strings.TrimSuffix(checked, "\n"), "\n")
	if len(paths) == 0 {
		lines = nil
	}
	if len(lines) != len(paths) {
		return "", nil, fmt.Errorf("git cat-file answered %d paths of %d", len(lines), len(paths))
	}
	out := map[string]driver.MirrorEntry{}
	for i, line := range lines {
		f := strings.Fields(line)
		if len(f) != 3 || f[1] == "missing" || (f[1] != "blob" && f[1] != "tree") {
			continue // not there, or a submodule, which no work tree here holds
		}
		e := driver.MirrorEntry{Object: f[0], Dir: f[1] == "tree"}
		if !e.Dir {
			if e.Size, err = strconv.ParseInt(f[2], 10, 64); err != nil {
				return "", nil, fmt.Errorf("git cat-file gave %q a size of %q", paths[i], f[2])
			}
			if maxContent > 0 && e.Size <= maxContent {
				blob, err := git(ctx, r, nil, "cat-file", "blob", e.Object)
				if err != nil {
					return "", nil, err
				}
				e.Content = []byte(blob)
			}
		}
		out[paths[i]] = e
	}
	return commit, out, nil
}

// git runs git in dir, if any, with env added to a clean environment of
// its own: no global or system config.
func git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	return gitIn(ctx, dir, env, nil, args...)
}

func gitIn(ctx context.Context, dir string, env []string, stdin io.Reader, args ...string) (string, error) {
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	base := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=/nonexistent", "PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
	return runIn(ctx, append(base, env...), stdin, "git", args...)
}

func run(ctx context.Context, _ string, env []string, name string, args ...string) (string, error) {
	return runIn(ctx, append(os.Environ(), env...), nil, name, args...)
}

func runIn(ctx context.Context, env []string, stdin io.Reader, name string, args ...string) (string, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, stdin, &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, firstArgs(args), err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}

// firstArgs names a command by its first arguments, never its URL.
func firstArgs(args []string) string {
	var out []string
	for _, a := range args {
		if a == "--" || strings.Contains(a, "://") {
			break
		}
		out = append(out, a)
		if len(out) == 4 {
			break
		}
	}
	return strings.Join(out, " ")
}
