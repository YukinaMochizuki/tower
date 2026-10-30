package guestbin

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A boost's sync (docs/decisions/boost.md): tower-guest walks the box's
// /workspace and the boost's, and sends the boost what differs
// (boost-manifest, boost-pack, boost-apply), with git's state; a boosted
// process runs under boost-job, which says what it changed once it ends;
// boost-back packs that on the boost, boost-writeback applies it in the
// box, the box winning. Every file name goes to git as an argument or
// NUL-separated input, never through a shell. Each subcommand writes its
// result as JSON on stdout.

// The exit codes of the boost subcommands beyond 0 and ExitFailed.
const (
	// ExitTooLarge: what boost-pack must send of git's state, staged
	// content and HEAD's commits, is past its cap.
	ExitTooLarge = 5
	// ExitHeadMissing: boost-apply lacks HEAD's commit, or what its
	// bundle needs: the box is to send HEAD's whole history.
	ExitHeadMissing = 6
	// ExitNoSince: the box's manifest a shared sync or a write-back
	// compares with is not kept.
	ExitNoSince = 7
)

// BoostDir is where the boost subcommands keep what they make, in a
// checkout's git directory: out of its work tree, on its disk.
const BoostDir = "tower-boost"

// boostKeepFor is how long a staging directory or report left by a
// crash is kept before a later subcommand sweeps it.
const boostKeepFor = 24 * time.Hour

// changedKept bounds the changed paths a report lists.
const changedKept = 200

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
		return failed(stderr, 2, errors.New("a boost subcommand is boost-baseline, boost-manifest, boost-pack, boost-apply, boost-job, boost-back, boost-writeback or boost-clean")), 0
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
	case "boost-manifest":
		out, err = boostManifest(args[1:])
	case "boost-baseline":
		out, err = boostBaseline(args[1:])
	case "boost-back":
		out, err = boostBack(args[1:])
	case "boost-writeback":
		out, err = boostWriteback(args[1:])
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
// directories, reports and sent manifests older than boostKeepFor.
func sweep(boostDir string) {
	entries, err := os.ReadDir(boostDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "sync-") && !strings.HasPrefix(e.Name(), "job-") && !strings.HasPrefix(e.Name(), "sent-") {
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

func splitNUL(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), "\x00")
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }
