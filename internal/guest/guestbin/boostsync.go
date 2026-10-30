package guestbin

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest/tarx"
)

// A boost's sync (docs/decisions/boost.md): tower-guest in the box writes
// its work tree as a git tree and packs what changed since a base
// (boost-pack); tower copies the pack to the boost, where tower-guest
// applies it (boost-apply); a boosted process runs under boost-job, which
// says what it changed and measures its artifacts once it ends. Every
// file name goes to git as an argument or NUL-separated input, never
// through a shell. Each subcommand writes its result as JSON on stdout.

// The exit codes of the boost subcommands beyond 0 and ExitFailed.
const (
	// ExitBaseMissing: the base boost-apply was given is not there.
	ExitBaseMissing = 3
	// ExitTreeMismatch: the tree boost-apply made is not the one sent.
	ExitTreeMismatch = 4
	// ExitTooLarge: boost-pack's delta is past its cap.
	ExitTooLarge = 5
	// ExitHeadMissing: boost-apply lacks HEAD's commit, or what its
	// bundle needs: the box is to send HEAD's whole history.
	ExitHeadMissing = 6
)

// BoostDir is where the boost subcommands keep what they make, in a
// checkout's git directory: out of its work tree, on its disk.
const BoostDir = "tower-boost"

// boostKeepFor is how long a staging directory or report left by a
// crash is kept before a later subcommand sweeps it.
const boostKeepFor = 24 * time.Hour

// changedKept bounds the changed paths a report lists.
const changedKept = 200

// Manifest is what boost-pack made and says: the work tree's tree, the
// HEAD it is on, what of it changed since base, and the files in its
// staging directory.
type Manifest struct {
	Tree string `json:"tree"`
	// Head is HEAD's commit, "" for none; Branch its branch, "" when
	// detached.
	Head   string `json:"head,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Base is the tree the delta is against, "" for a whole tree (Full).
	Base string `json:"base,omitempty"`
	Full bool   `json:"full"`
	// Files and Bytes are what delta.tar holds; Deleted the paths in
	// deleted; Paths, for a whole tree, every path of it.
	Files   int   `json:"files"`
	Bytes   int64 `json:"bytes"`
	Deleted int   `json:"deleted"`
	// Gitlinks are the submodules (and nested repositories) of the tree
	// that changed since the base, or all of them for a whole tree: their
	// checkouts are not sent, only the commit the tree names.
	Gitlinks []Gitlink `json:"gitlinks,omitempty"`
	// Bundle says head.bundle holds the commits of HEAD the box's origin
	// refs do not reach, or all of them (boost-pack --bundle-all).
	Bundle bool `json:"bundle"`
	// Links are every gitlink of the tree, which the tree made on the
	// boost is checked with, their checkouts never sent.
	Links []Gitlink `json:"links,omitempty"`
	// IndexTree is the box's index's tree, what is staged there, sent as
	// what differs from Tree (index.tar, index.deleted, IndexGitlinks);
	// "" when the index has none (a merge's conflicts).
	IndexTree     string    `json:"indexTree,omitempty"`
	IndexGitlinks []Gitlink `json:"indexGitlinks,omitempty"`
}

// Gitlink is a submodule of the tree: its path, and the commit the tree
// names, "" for one deleted.
type Gitlink struct {
	Path   string `json:"path"`
	Commit string `json:"commit,omitempty"`
}

// Changed is a path a boosted process changed: A, M, D or T (type).
type Changed struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// Artifact is a path a boosted process named to copy back, measured.
type Artifact struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Exists bool   `json:"exists"`
	Error  string `json:"error,omitempty"`
}

// Report is what boost-job writes once its command ends.
type Report struct {
	Changed   []Changed  `json:"changed"`
	Total     int        `json:"total"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// Boost runs a boost subcommand with its arguments, writing its result to
// stdout, and returns its exit code. A command boost-job runs that a
// signal ends makes it exit 128 plus the signal's number; Main raises the
// signal instead.
func Boost(args []string, stdout, stderr io.Writer) int {
	code, _ := boost(args, stdout, stderr, false)
	return code
}

// boostMain is Boost as tower-guest runs it: a command a signal ended
// ends it by the same signal, so its process's exit says the signal.
func boostMain(args []string) int {
	code, sig := boost(args, os.Stdout, os.Stderr, true)
	if sig != 0 {
		signal.Reset(sig)
		_ = syscall.Kill(os.Getpid(), sig)
		// A signal it cannot be ended by, such as one it was started
		// ignoring: its number, as a shell says it.
		time.Sleep(time.Second)
	}
	return code
}

func boost(args []string, stdout, stderr io.Writer, main bool) (int, syscall.Signal) {
	if len(args) == 0 {
		return failed(stderr, 2, errors.New("a boost subcommand is boost-pack, boost-apply, boost-job, boost-measure or boost-clean")), 0
	}
	var err error
	var out any
	switch args[0] {
	case "boost-pack":
		out, err = boostPack(args[1:])
	case "boost-apply":
		out, err = boostApply(args[1:])
	case "boost-clean":
		out, err = boostClean(args[1:])
	case "boost-measure":
		out, err = boostMeasure(args[1:])
	case "boost-job":
		return boostJob(args[1:], stdout, stderr, main)
	default:
		return failed(stderr, 2, fmt.Errorf("no boost subcommand %q", args[0])), 0
	}
	if err != nil {
		var ce codedError
		if errors.As(err, &ce) {
			return failed(stderr, ce.code, err), 0
		}
		return failed(stderr, ExitFailed, err), 0
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		return failed(stderr, ExitFailed, err), 0
	}
	return 0, 0
}

// codedError is an error a subcommand exits with a code of its own for.
type codedError struct {
	code int
	err  error
}

func (e codedError) Error() string { return e.err.Error() }
func (e codedError) Unwrap() error { return e.err }

// gitRun runs git in dir with env added, stdin from in, and returns its
// stdout; its stderr is in the error. Every path it is given is a path,
// never a pattern (GIT_LITERAL_PATHSPECS), and dir is a safe directory
// for this call alone, whoever owns it.
func gitRun(dir string, env []string, in io.Reader, args ...string) ([]byte, error) {
	safe := dir
	if abs, err := filepath.Abs(dir); err == nil {
		safe = abs
	}
	cmd := exec.Command("git", append([]string{"-c", "safe.directory=" + safe}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append([]string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_LITERAL_PATHSPECS=1"}, env...)...)
	cmd.Stdin = in
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

func gitLine(dir string, env []string, args ...string) (string, error) {
	out, err := gitRun(dir, env, nil, args...)
	return strings.TrimSpace(string(out)), err
}

// gitHas says the object named exists.
func gitHas(dir, name string) bool {
	_, err := gitRun(dir, nil, nil, "cat-file", "-e", name)
	return err == nil
}

// boostGitDir is dir's git directory, and its boost directory made.
func boostGitDir(dir string) (gitDir, boostDir string, err error) {
	gitDir, err = gitLine(dir, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", "", err
	}
	boostDir = filepath.Join(gitDir, BoostDir)
	if err := os.MkdirAll(boostDir, 0o755); err != nil {
		return "", "", err
	}
	sweep(boostDir)
	return gitDir, boostDir, nil
}

// sweep removes what a crash left in the boost directory: staging
// directories and reports older than boostKeepFor.
func sweep(boostDir string) {
	entries, err := os.ReadDir(boostDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "sync-") && !strings.HasPrefix(e.Name(), "job-") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > boostKeepFor {
			_ = os.RemoveAll(filepath.Join(boostDir, e.Name()))
		}
	}
}

// snapshot writes dir's work tree as a git tree, through an index of its
// own (index, in the boost directory), never the checkout's: the paths of
// tracked, a tree, whether or not git ignores them, as a checkout's index
// tracks a file an ignore rule matches; and untracked files git does not
// ignore. "" tracks none. The index is made tracked's paths with
// read-tree -m, which keeps the stat cache of each entry whose content is
// tracked's, so the next snapshot is quick.
func snapshot(dir, index, tracked string) (string, error) {
	env := []string{"GIT_INDEX_FILE=" + index}
	var err error
	if tracked == "" {
		_, err = gitRun(dir, env, nil, "read-tree", "--empty")
	} else if _, err = gitRun(dir, env, nil, "read-tree", "-m", tracked); err != nil {
		// An index read-tree -m cannot merge from, one a crash cut short:
		// made again, without its stat cache.
		_ = os.Remove(index)
		_, err = gitRun(dir, env, nil, "read-tree", tracked)
	}
	if err != nil {
		return "", err
	}
	return addAll(dir, index)
}

// addAll writes dir's work tree as a git tree through index: the paths
// it tracks, and untracked files git does not ignore.
func addAll(dir, index string) (string, error) {
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := gitRun(dir, env, nil, "add", "-A"); err != nil {
		return "", err
	}
	return gitLine(dir, env, "write-tree")
}

// boxSnapshot is snapshot in a box, through a copy of its own index made
// each time: what it tracks is tracked, ignored or not, and its stat
// cache is the user's own, up to date. A box without an index (one
// removed, or a checkout not made yet) tracks HEAD's paths, or none
// without a HEAD. indexTree is the index's own tree, what is staged, ""
// when it has none (a merge's conflicts, or no index).
func boxSnapshot(dir, gitDir, boostDir string) (tree, indexTree string, err error) {
	index := filepath.Join(boostDir, "box-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".index")
	defer func() { _ = os.Remove(index) }()
	b, err := os.ReadFile(filepath.Join(gitDir, "index"))
	switch {
	case err == nil:
		if err := os.WriteFile(index, b, 0o644); err != nil {
			return "", "", err
		}
		indexTree, _ = gitLine(dir, []string{"GIT_INDEX_FILE=" + index}, "write-tree")
		tree, err = addAll(dir, index)
		return tree, indexTree, err
	case errors.Is(err, fs.ErrNotExist):
		head := ""
		if gitHas(dir, "HEAD^{tree}") {
			head = "HEAD^{tree}"
		}
		tree, err = snapshot(dir, index, head)
		return tree, "", err
	default:
		return "", "", err
	}
}

// linksOf are the gitlinks of es.
func linksOf(es []entry) []Gitlink {
	var out []Gitlink
	for _, e := range es {
		if e.mode == gitlinkMode {
			out = append(out, Gitlink{Path: e.path, Commit: e.blob})
		}
	}
	return out
}

// withLinks is the tree of index with its gitlinks those of links alone,
// and nothing under them: a submodule's checkout, or a nested
// repository's, is never sent, so the boost holds its own, or none. The
// index is copied, not changed.
func withLinks(dir, index string, links []Gitlink, hashLen int) (string, error) {
	tmp := index + ".links"
	defer func() { _ = os.Remove(tmp) }()
	b, err := os.ReadFile(index)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	env := []string{"GIT_INDEX_FILE=" + tmp}
	out, err := gitRun(dir, env, nil, "ls-files", "-s", "-z")
	if err != nil {
		return "", err
	}
	keep := map[string]bool{}
	for _, l := range links {
		keep[l.Path] = true
	}
	under := func(p string) bool {
		for d := path.Dir(p); d != "." && d != "/"; d = path.Dir(d) {
			if keep[d] {
				return true
			}
		}
		return false
	}
	removed := "0 " + strings.Repeat("0", hashLen) + "\t"
	var info strings.Builder
	for _, rec := range splitNUL(out) {
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		if (strings.HasPrefix(meta, gitlinkMode+" ") && !keep[p]) || under(p) {
			info.WriteString(removed + p + "\x00")
		}
	}
	for _, l := range links {
		info.WriteString(gitlinkMode + " " + l.Commit + "\t" + l.Path + "\x00")
	}
	if info.Len() > 0 {
		if _, err := gitRun(dir, env, strings.NewReader(info.String()), "update-index", "-z", "--index-info"); err != nil {
			return "", err
		}
	}
	return gitLine(dir, env, "write-tree")
}

// entry is one path of a tree diff or listing.
type entry struct {
	status, oldMode, mode, blob, path string
}

// diffTrees lists what differs from a to b, recursively, renames as a
// delete and an add.
func diffTrees(dir, a, b string) ([]entry, error) {
	out, err := gitRun(dir, nil, nil, "diff-tree", "-r", "-z", "--no-renames", "--raw", a, b)
	if err != nil {
		return nil, err
	}
	var es []entry
	fields := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) < 5 {
			continue
		}
		es = append(es, entry{status: meta[4][:1], oldMode: meta[0], mode: meta[1], blob: meta[3], path: fields[i+1]})
	}
	return es, nil
}

// listTree lists every path of tree, as added.
func listTree(dir, tree string) ([]entry, error) {
	out, err := gitRun(dir, nil, nil, "ls-tree", "-r", "-z", "--full-tree", tree)
	if err != nil {
		return nil, err
	}
	var es []entry
	for _, rec := range strings.Split(string(out), "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) < 3 {
			continue
		}
		es = append(es, entry{status: "A", mode: f[0], blob: f[2], path: p})
	}
	return es, nil
}

const gitlinkMode = "160000"

// boostPack: --dir, the checkout; --base, the tree or commit to send what
// changed since, "" for the whole tree, or one the box lacks; --out, the
// staging directory; --max, the most delta.tar may hold, in bytes;
// --bundle-all, HEAD's whole history bundled, for a boost that lacks
// what the box's origin refs reach.
func boostPack(args []string) (*Manifest, error) {
	fl := flag.NewFlagSet("boost-pack", flag.ContinueOnError)
	dir := fl.String("dir", "/workspace", "")
	base := fl.String("base", "", "")
	out := fl.String("out", "", "")
	limit := fl.Int64("max", 2<<30, "")
	bundleAll := fl.Bool("bundle-all", false, "")
	if err := fl.Parse(args); err != nil {
		return nil, err
	}
	if *out == "" {
		return nil, errors.New("--out is required")
	}
	gitDir, boostDir, err := boostGitDir(*dir)
	if err != nil {
		return nil, err
	}
	tree, indexTree, err := boxSnapshot(*dir, gitDir, boostDir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Tree: tree}
	m.Head, _ = gitLine(*dir, nil, "rev-parse", "--verify", "-q", "HEAD")
	m.Branch, _ = gitLine(*dir, nil, "symbolic-ref", "-q", "--short", "HEAD")
	all, err := listTree(*dir, tree)
	if err != nil {
		return nil, err
	}
	m.Links = linksOf(all)
	var es []entry
	if *base != "" && gitHas(*dir, *base+"^{tree}") {
		if m.Base, err = gitLine(*dir, nil, "rev-parse", *base+"^{tree}"); err != nil {
			return nil, err
		}
		es, err = diffTrees(*dir, m.Base, tree)
	} else {
		m.Full, es = true, all
	}
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return nil, err
	}
	send, deleted := splitDelta(es, &m.Gitlinks)
	// What is staged, where it differs from the work tree.
	var staged []entry
	var stagedDeleted []string
	if indexTree != "" {
		diff, err := diffTrees(*dir, tree, indexTree)
		if err != nil {
			return nil, err
		}
		staged, stagedDeleted = splitDelta(diff, &m.IndexGitlinks)
		m.IndexTree = indexTree
	}
	sizes, err := blobSizes(*dir, append(slices.Clone(send), staged...))
	if err != nil {
		return nil, err
	}
	var total int64
	for _, n := range sizes {
		total += n
	}
	if total > *limit {
		return nil, codedError{ExitTooLarge, fmt.Errorf("what changed holds %d bytes, past the %d a sync sends; the largest: %s",
			total, *limit, largest(append(slices.Clone(send), staged...), sizes, 5))}
	}
	for _, f := range []struct {
		name    string
		es      []entry
		deleted []string
	}{{"delta", send, deleted}, {"index", staged, stagedDeleted}} {
		if err := writeDelta(*dir, filepath.Join(*out, f.name+".tar"), f.es); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(*out, f.name+".deleted"), []byte(strings.Join(f.deleted, "\x00")), 0o644); err != nil {
			return nil, err
		}
	}
	m.Files, m.Bytes, m.Deleted = len(send), total, len(deleted)
	if m.Full {
		var paths []string
		for _, e := range send {
			paths = append(paths, e.path)
		}
		if err := os.WriteFile(filepath.Join(*out, "paths"), []byte(strings.Join(paths, "\x00")), 0o644); err != nil {
			return nil, err
		}
	}
	if m.Head != "" {
		// The commits of HEAD no origin ref reaches: local ones, which the
		// boost's mirror lacks; or, asked for, all of them.
		not := []string{"--not", "--remotes=origin"}
		if *bundleAll {
			not = nil
		}
		n, err := gitLine(*dir, nil, append([]string{"rev-list", "--count", "HEAD"}, not...)...)
		if err != nil {
			return nil, err
		}
		if n != "0" {
			bundle := filepath.Join(*out, "head.bundle")
			if _, err := gitRun(*dir, nil, nil, append([]string{"bundle", "create", "-q", bundle, "HEAD"}, not...)...); err != nil {
				return nil, err
			}
			m.Bundle = true
			// The commits count with what changed toward the cap.
			info, err := os.Stat(bundle)
			if err != nil {
				return nil, err
			}
			if total+info.Size() > *limit {
				what := "the box's commits origin lacks"
				if *bundleAll {
					what = "HEAD's whole history"
				}
				return nil, codedError{ExitTooLarge, fmt.Errorf("what changed (%d bytes) and %s (%d bytes, %d commits) are past the %d a sync sends",
					total, what, info.Size(), mustAtoi(n), *limit)}
			}
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return m, os.WriteFile(filepath.Join(*out, "manifest.json"), b, 0o644)
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// splitDelta splits es into the blobs to send and the paths deleted,
// adding the gitlinks that changed, whose checkouts are not sent, to
// links.
func splitDelta(es []entry, links *[]Gitlink) (send []entry, deleted []string) {
	for _, e := range es {
		switch {
		case e.mode == gitlinkMode && e.status == "D":
			*links = append(*links, Gitlink{Path: e.path})
		case e.mode == gitlinkMode:
			*links = append(*links, Gitlink{Path: e.path, Commit: e.blob})
		case e.status == "D" && e.oldMode == gitlinkMode:
			*links = append(*links, Gitlink{Path: e.path})
		case e.status == "D":
			deleted = append(deleted, e.path)
		default:
			send = append(send, e)
		}
	}
	return send, deleted
}

// blobSizes is the size of each entry's blob, in order.
func blobSizes(dir string, es []entry) ([]int64, error) {
	if len(es) == 0 {
		return nil, nil
	}
	var in bytes.Buffer
	for _, e := range es {
		in.WriteString(e.blob + "\n")
	}
	out, err := gitRun(dir, nil, &in, "cat-file", "--batch-check=%(objectsize)")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(es) {
		return nil, fmt.Errorf("git cat-file sized %d objects of %d", len(lines), len(es))
	}
	sizes := make([]int64, len(es))
	for i, l := range lines {
		if sizes[i], err = strconv.ParseInt(l, 10, 64); err != nil {
			return nil, fmt.Errorf("git cat-file: %q", l)
		}
	}
	return sizes, nil
}

// largest names the n largest entries, with their sizes.
func largest(es []entry, sizes []int64, n int) string {
	idx := make([]int, len(es))
	for i := range idx {
		idx[i] = i
	}
	slices.SortFunc(idx, func(a, b int) int { return cmp.Compare(sizes[b], sizes[a]) })
	var parts []string
	for _, i := range idx[:min(n, len(idx))] {
		parts = append(parts, fmt.Sprintf("%s (%d bytes)", es[i].path, sizes[i]))
	}
	return strings.Join(parts, ", ")
}

// writeDelta writes a tar of es's blobs, read from the object store, not
// the work tree, so it holds the tree as written whatever changes since:
// a regular file with its mode, a symlink with its target.
func writeDelta(dir, file string, es []entry) error {
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	tw := tar.NewWriter(f)
	if len(es) > 0 {
		cmd := exec.Command("git", "cat-file", "--batch")
		cmd.Dir = dir
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		var errOut bytes.Buffer
		cmd.Stderr = &errOut
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() {
			for _, e := range es {
				_, _ = io.WriteString(stdin, e.blob+"\n")
			}
			_ = stdin.Close()
		}()
		r := bufio.NewReader(stdout)
		for _, e := range es {
			if err := writeEntry(tw, r, e); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return err
			}
		}
		if err := cmd.Wait(); err != nil {
			return fmt.Errorf("git cat-file: %w: %s", err, errOut.String())
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return f.Close()
}

// writeEntry writes one entry from git cat-file --batch's output.
func writeEntry(tw *tar.Writer, r *bufio.Reader, e entry) error {
	head, err := r.ReadString('\n')
	if err != nil {
		return err
	}
	f := strings.Fields(head)
	if len(f) != 3 || f[1] != "blob" {
		return fmt.Errorf("git cat-file answered %q for %s", strings.TrimSpace(head), e.path)
	}
	size, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil {
		return err
	}
	hdr := &tar.Header{Name: e.path, Size: size, Mode: 0o644, Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)}
	switch e.mode {
	case "100755":
		hdr.Mode = 0o755
	case "120000":
		target := make([]byte, size)
		if _, err := io.ReadFull(r, target); err != nil {
			return err
		}
		hdr.Typeflag, hdr.Linkname, hdr.Size, hdr.Mode = tar.TypeSymlink, string(target), 0, 0o777
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = r.Discard(1)
		return err
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := io.CopyN(tw, r, size); err != nil {
		return err
	}
	_, err = r.Discard(1)
	return err
}

// Applied is what boost-apply did.
type Applied struct {
	Tree string `json:"tree"`
	// Restored is how many paths the boost's own jobs had changed since
	// the base were put back first; Shared says none were, as a job still
	// runs on it.
	Restored int  `json:"restored"`
	Shared   bool `json:"shared"`
	// Staged says the box's index, what is staged there, is the boost's.
	Staged bool `json:"staged"`
}

// boostApply: --dir, the boost's checkout; --staging, what boost-pack
// made, copied here; --shared, a job still runs on the boost, so what it
// wrote is left as it is and the work tree is not checked. Without
// --shared, what the boost's own jobs changed since the base is put back
// first, and the tree made must be the one sent, but for submodules'
// checkouts, never sent. HEAD is the box's commit, on its branch, and the
// index the box's.
func boostApply(args []string) (*Applied, error) {
	fl := flag.NewFlagSet("boost-apply", flag.ContinueOnError)
	dir := fl.String("dir", "/workspace", "")
	staging := fl.String("staging", "", "")
	shared := fl.Bool("shared", false, "")
	if err := fl.Parse(args); err != nil {
		return nil, err
	}
	if *staging == "" {
		return nil, errors.New("--staging is required")
	}
	defer func() { _ = os.RemoveAll(*staging) }()
	_, boostDir, err := boostGitDir(*dir)
	if err != nil {
		return nil, err
	}
	var m Manifest
	b, err := os.ReadFile(filepath.Join(*staging, "manifest.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if !m.Full && !gitHas(*dir, m.Base+"^{tree}") {
		return nil, codedError{ExitBaseMissing, fmt.Errorf("the base tree %s is not here", m.Base)}
	}
	// HEAD's commits first: a boost that lacks them is sent all of them.
	if m.Bundle {
		if _, err := gitRun(*dir, nil, nil, "fetch", "-q", filepath.Join(*staging, "head.bundle"), "HEAD"); err != nil {
			return nil, codedError{ExitHeadMissing, err}
		}
	}
	if m.Head != "" && !gitHas(*dir, m.Head+"^{commit}") {
		return nil, codedError{ExitHeadMissing, fmt.Errorf("HEAD's commit %s is not here", m.Head)}
	}
	index := filepath.Join(boostDir, "snapshot.index")
	a := &Applied{Tree: m.Tree, Shared: *shared}
	if !*shared {
		if a.Restored, err = restore(*dir, index, &m, *staging); err != nil {
			return nil, err
		}
	}
	// The delta: deletions first, so a directory a file replaces is empty.
	del, err := os.ReadFile(filepath.Join(*staging, "delta.deleted"))
	if err != nil {
		return nil, err
	}
	for _, p := range splitNUL(del) {
		if err := removeIn(*dir, p); err != nil {
			return nil, err
		}
	}
	if err := extractDelta(*dir, filepath.Join(*staging, "delta.tar")); err != nil {
		return nil, err
	}
	// The tree sent, made from the base and what was sent, which stores
	// it here: for the next send, and as what the work tree tracks. It
	// checks the send.
	base := m.Base
	if m.Full {
		base = ""
	}
	sent, err := treeFrom(*dir, base, filepath.Join(*staging, "delta"), m.Gitlinks, len(m.Tree))
	if err != nil {
		return nil, err
	}
	if sent != m.Tree {
		return nil, codedError{ExitTreeMismatch, fmt.Errorf("the tree sent makes %s, not the %s it should", sent, m.Tree)}
	}
	// What a running job wrote stays, so with one the work tree is not
	// the tree sent, and is not checked.
	if !*shared {
		// The tree sent's paths are tracked here as in the box, a file
		// an ignore rule matches too.
		if _, err := snapshot(*dir, index, m.Tree); err != nil {
			return nil, err
		}
		got, err := withLinks(*dir, index, m.Links, len(m.Tree))
		if err != nil {
			return nil, err
		}
		if got != m.Tree {
			return nil, codedError{ExitTreeMismatch, fmt.Errorf("the tree made is %s, not the %s sent", got, m.Tree)}
		}
	}
	if m.Head != "" {
		if m.Branch != "" {
			if _, err := gitRun(*dir, nil, nil, "update-ref", "refs/heads/"+m.Branch, m.Head); err != nil {
				return nil, err
			}
			if _, err := gitRun(*dir, nil, nil, "symbolic-ref", "HEAD", "refs/heads/"+m.Branch); err != nil {
				return nil, err
			}
		} else if _, err := gitRun(*dir, nil, nil, "update-ref", "--no-deref", "HEAD", m.Head); err != nil {
			return nil, err
		}
	}
	// The index: the box's, made from the tree sent and what is staged
	// apart from it; else HEAD's.
	indexTree := m.Head
	if m.IndexTree != "" {
		got, err := treeFrom(*dir, m.Tree, filepath.Join(*staging, "index"), m.IndexGitlinks, len(m.Tree))
		if err == nil && got == m.IndexTree {
			indexTree, a.Staged = got, true
		}
	}
	if indexTree != "" {
		if _, err := gitRun(*dir, nil, nil, "read-tree", indexTree); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// extractDelta extracts a delta tar into dir. A path it writes a file or
// a symlink to that is a directory here is removed first, with what it
// holds, ignored files too: the box's tree has a file there.
func extractDelta(dir, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = f.Close()
			return err
		}
		if hdr.Typeflag != tar.TypeDir {
			if err := clearDir(dir, hdr.Name); err != nil {
				_ = f.Close()
				return err
			}
		}
	}
	_ = f.Close()
	if f, err = os.Open(file); err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return tarx.Extract(f, dir)
}

// clearDir removes the directory at p of dir, with all it holds, if one
// is there and is not a symlink; never anything outside dir, through a
// name or a link (os.Root).
func clearDir(dir, p string) error {
	p = path.Clean(p)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") || path.IsAbs(p) {
		return fmt.Errorf("%q is outside %s", p, dir)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if info, err := root.Lstat(p); err == nil && info.IsDir() {
		return root.RemoveAll(p)
	}
	return nil
}

// treeFrom makes a tree from base ("" for none) and a delta, stage.tar
// and stage.deleted, and the gitlinks that changed, in an index of its
// own, writing its objects here.
func treeFrom(dir, base, stage string, links []Gitlink, hashLen int) (string, error) {
	f, err := os.CreateTemp("", "tower-boost-index-*")
	if err != nil {
		return "", err
	}
	index := f.Name()
	_ = f.Close()
	_ = os.Remove(index)
	defer func() { _ = os.Remove(index) }()
	env := []string{"GIT_INDEX_FILE=" + index}
	if base == "" {
		_, err = gitRun(dir, env, nil, "read-tree", "--empty")
	} else {
		_, err = gitRun(dir, env, nil, "read-tree", base)
	}
	if err != nil {
		return "", err
	}
	del, err := os.ReadFile(stage + ".deleted")
	if err != nil {
		return "", err
	}
	// Mode 0 removes a path; the id's length is the repository's hash's.
	removed := "0 " + strings.Repeat("0", hashLen) + "\t"
	var info strings.Builder
	for _, p := range splitNUL(del) {
		info.WriteString(removed + p + "\x00")
	}
	for _, g := range links {
		if g.Commit == "" {
			info.WriteString(removed + g.Path + "\x00")
		} else {
			info.WriteString(gitlinkMode + " " + g.Commit + "\t" + g.Path + "\x00")
		}
	}
	tf, err := os.Open(stage + ".tar")
	if err != nil {
		return "", err
	}
	defer func() { _ = tf.Close() }()
	tr := tar.NewReader(tf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		mode, body := "100644", io.Reader(tr)
		switch {
		case hdr.Typeflag == tar.TypeSymlink:
			mode, body = "120000", strings.NewReader(hdr.Linkname)
		case hdr.Mode&0o111 != 0:
			mode = "100755"
		}
		blob, err := hashObject(dir, body)
		if err != nil {
			return "", err
		}
		info.WriteString(mode + " " + blob + "\t" + hdr.Name + "\x00")
	}
	if info.Len() > 0 {
		if _, err := gitRun(dir, env, strings.NewReader(info.String()), "update-index", "-z", "--index-info"); err != nil {
			return "", err
		}
	}
	return gitLine(dir, env, "write-tree")
}

// hashObject writes r as a blob and returns its id.
func hashObject(dir string, r io.Reader) (string, error) {
	out, err := gitRun(dir, nil, r, "hash-object", "-w", "--stdin", "--no-filters")
	return strings.TrimSpace(string(out)), err
}

// restore puts back what changed on the boost since the base: a path
// changed or deleted is checked out of the base, one added removed. A
// whole tree (no base) removes every path not in it that git does not
// ignore. Other gitignored paths are left as they are; with a base, one
// the base tracks is put back as any other.
func restore(dir, index string, m *Manifest, staging string) (int, error) {
	tracked := m.Base
	if m.Full {
		tracked = ""
	}
	now, err := snapshot(dir, index, tracked)
	if err != nil {
		return 0, err
	}
	if m.Full {
		keep := map[string]bool{}
		paths, err := os.ReadFile(filepath.Join(staging, "paths"))
		if err != nil {
			return 0, err
		}
		for _, p := range splitNUL(paths) {
			keep[p] = true
		}
		for _, g := range m.Links {
			keep[g.Path] = true
		}
		es, err := listTree(dir, now)
		if err != nil {
			return 0, err
		}
		n := 0
		for _, e := range es {
			if !keep[e.path] && e.mode != gitlinkMode {
				if err := removeIn(dir, e.path); err != nil {
					return n, err
				}
				n++
			}
		}
		return n, nil
	}
	es, err := diffTrees(dir, m.Base, now)
	if err != nil {
		return 0, err
	}
	var back []string
	n := 0
	for _, e := range es {
		if e.mode == gitlinkMode || e.oldMode == gitlinkMode {
			// A submodule's checkout is the boost's own.
			continue
		}
		n++
		if e.status == "A" {
			if err := removeIn(dir, e.path); err != nil {
				return n, err
			}
			continue
		}
		// Changed, deleted, or of another type: the base's, after what
		// is there goes, a directory where the base has a file with all it
		// holds, ignored files too.
		if err := clearDir(dir, e.path); err != nil {
			return n, err
		}
		if err := removeIn(dir, e.path); err != nil {
			return n, err
		}
		back = append(back, e.path)
	}
	if len(back) > 0 {
		tmp := index + ".restore"
		defer func() { _ = os.Remove(tmp) }()
		in := strings.NewReader(strings.Join(back, "\x00"))
		if _, err := gitRun(dir, []string{"GIT_INDEX_FILE=" + tmp}, in,
			"checkout", m.Base, "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return n, err
		}
	}
	return n, nil
}

// removeIn removes the path p of dir, if there, and the directories it
// leaves empty up to dir. p never climbs out of dir.
func removeIn(dir, p string) error {
	p = path.Clean(p)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") || path.IsAbs(p) {
		return fmt.Errorf("%q is outside %s", p, dir)
	}
	// Never anything outside dir, through a name or a link (os.Root).
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// A directory where a file was: what it holds is gone already, or
		// is untracked and stays.
		if info, serr := root.Lstat(p); serr != nil || !info.IsDir() {
			return err
		}
		return nil
	}
	for d := path.Dir(p); d != "." && removedEmpty(root, d); d = path.Dir(d) {
	}
	return nil
}

// removedEmpty removes the directory d of root if it is empty, saying
// whether it did.
func removedEmpty(root *os.Root, d string) bool { return root.Remove(d) == nil }

func splitNUL(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), "\x00")
}

// Cleaned is what boost-clean removed.
type Cleaned struct {
	Removed []string `json:"removed"`
}

// boostClean: --dir, a checkout; --name, a staging directory or report of
// its boost directory (sync-... or job-...), removed if there. Leftovers
// past boostKeepFor are swept as by every subcommand.
func boostClean(args []string) (*Cleaned, error) {
	fl := flag.NewFlagSet("boost-clean", flag.ContinueOnError)
	dir := fl.String("dir", "/workspace", "")
	var names stringList
	fl.Var(&names, "name", "")
	if err := fl.Parse(args); err != nil {
		return nil, err
	}
	_, boostDir, err := boostGitDir(*dir)
	if err != nil {
		return nil, err
	}
	c := &Cleaned{}
	for _, n := range names {
		if n == "" || strings.ContainsAny(n, "/\\") || (!strings.HasPrefix(n, "sync-") && !strings.HasPrefix(n, "job-")) {
			return nil, fmt.Errorf("%q is no name of the boost directory's", n)
		}
		p := filepath.Join(boostDir, n)
		if _, err := os.Lstat(p); err == nil {
			if err := os.RemoveAll(p); err != nil {
				return nil, err
			}
			c.Removed = append(c.Removed, n)
		}
	}
	return c, nil
}

// boostMeasure: --dir, the boost's checkout; --artifact, each path to
// measure, as boost-job does, right before tower copies it back.
func boostMeasure(args []string) ([]Artifact, error) {
	fl := flag.NewFlagSet("boost-measure", flag.ContinueOnError)
	dir := fl.String("dir", "/workspace", "")
	var artifacts stringList
	fl.Var(&artifacts, "artifact", "")
	if err := fl.Parse(args); err != nil {
		return nil, err
	}
	out := []Artifact{}
	for _, a := range artifacts {
		out = append(out, measure(*dir, a))
	}
	return out, nil
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// boostJob: --dir, the boost's checkout; --tree, the tree sent; --report,
// where to write the Report, in the boost directory; --artifact, each
// path, relative to --dir, to measure; then "--" and the command, run in
// the working directory it is given. Once the command ends, what it
// changed relative to the tree, tracked or new and not ignored, is
// written to the report, then it exits as the command did. The signals a
// process's group is sent reach the command; this waits for its end. The
// diff runs in the process's cgroup, so its peak counts in the process's.
func boostJob(args []string, stdout, stderr io.Writer, main bool) (int, syscall.Signal) {
	fl := flag.NewFlagSet("boost-job", flag.ContinueOnError)
	dir := fl.String("dir", "/workspace", "")
	tree := fl.String("tree", "", "")
	report := fl.String("report", "", "")
	// The command's working directory, which it otherwise inherits; for
	// a caller that runs this in-process, such as the fake provider.
	cwd := fl.String("cwd", "", "")
	var artifacts stringList
	fl.Var(&artifacts, "artifact", "")
	if err := fl.Parse(args); err != nil {
		return failed(stderr, 2, err), 0
	}
	command := fl.Args()
	if len(command) == 0 {
		return failed(stderr, 2, errors.New("boost-job runs a command, after --")), 0
	}
	if main {
		// The group's signals are the command's; this outlives them to
		// write the report. Caught, not ignored: an ignored signal would
		// stay ignored in the command, a caught one is its default there.
		caught := make(chan os.Signal, 8)
		signal.Notify(caught, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
		defer signal.Stop(caught)
		go func() {
			for range caught {
			}
		}()
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = *cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	if !main {
		cmd.Stdin = nil
	}
	code, sig := 0, syscall.Signal(0)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			_, _ = fmt.Fprintf(stderr, "tower-guest: %v\n", err)
			code = 127
		} else if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig, code = ws.Signal(), 128+int(ws.Signal())
		} else {
			code = ee.ExitCode()
		}
	}
	if *report != "" {
		writeReport(*dir, *tree, *report, artifacts)
	}
	return code, sig
}

// writeReport writes what the job changed and its artifacts' sizes; a
// failure is said in the report, which the process's exit then says.
func writeReport(dir, tree, file string, artifacts []string) {
	r := Report{Changed: []Changed{}}
	if err := changedSince(dir, tree, &r); err != nil {
		r.Error = err.Error()
	}
	for _, a := range artifacts {
		r.Artifacts = append(r.Artifacts, measure(dir, a))
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		_ = os.Rename(tmp, file)
	}
}

// changedSince lists, in r, what differs in dir's work tree from tree.
func changedSince(dir, tree string, r *Report) error {
	if tree == "" {
		return nil
	}
	_, boostDir, err := boostGitDir(dir)
	if err != nil {
		return err
	}
	index := filepath.Join(boostDir, "snapshot.index")
	// The synced tree's paths are tracked, a file an ignore rule matches
	// too: a job's change to one is listed.
	if _, err := snapshot(dir, index, tree); err != nil {
		return err
	}
	// Submodules' checkouts are the boost's own: as the tree sent has them.
	all, err := listTree(dir, tree)
	if err != nil {
		return err
	}
	now, err := withLinks(dir, index, linksOf(all), len(tree))
	if err != nil {
		return err
	}
	es, err := diffTrees(dir, tree, now)
	if err != nil {
		return err
	}
	r.Total = len(es)
	for _, e := range es[:min(len(es), changedKept)] {
		r.Changed = append(r.Changed, Changed{Path: e.path, Status: e.status})
	}
	return nil
}

// measure is an artifact's size: a file's, or what a directory holds; one
// that leaves dir, through a name or a link, is refused.
func measure(dir, p string) Artifact {
	a := Artifact{Path: p}
	clean := path.Clean(p)
	if p == "" || path.IsAbs(p) || clean == ".." || strings.HasPrefix(clean, "../") {
		a.Error = "not a path in " + dir
		return a
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		a.Error = err.Error()
		return a
	}
	full, err := filepath.EvalSymlinks(filepath.Join(dir, filepath.FromSlash(clean)))
	if errors.Is(err, fs.ErrNotExist) {
		a.Error = "not there"
		return a
	}
	if err != nil {
		a.Error = err.Error()
		return a
	}
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		a.Error = "it leads out of " + dir
		return a
	}
	a.Exists = true
	err = filepath.WalkDir(full, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			a.Bytes += info.Size()
		}
		return nil
	})
	if err != nil {
		a.Error = err.Error()
	}
	return a
}
