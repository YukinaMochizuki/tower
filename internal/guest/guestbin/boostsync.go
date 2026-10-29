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
	// Bundle says head.bundle holds the commits of HEAD the box's remote
	// refs do not reach.
	Bundle bool `json:"bundle"`
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
		return failed(stderr, 2, errors.New("a boost subcommand is boost-pack, boost-apply, boost-job or boost-clean")), 0
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
// stdout; its stderr is in the error.
func gitRun(dir string, env []string, in io.Reader, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append([]string{"GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}, env...)...)
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

// snapshot writes dir's work tree as a git tree, tracked files and those
// untracked that git does not ignore, through an index of its own (index
// in the boost directory, seeded from seed if it is not there), never the
// checkout's: its stat cache makes the next snapshot quick.
func snapshot(dir, index, seed string) (string, error) {
	if _, err := os.Stat(index); errors.Is(err, fs.ErrNotExist) && seed != "" {
		if b, err := os.ReadFile(seed); err == nil {
			if err := os.WriteFile(index, b, 0o644); err != nil {
				return "", err
			}
		}
	}
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := gitRun(dir, env, nil, "add", "-A"); err != nil {
		return "", err
	}
	return gitLine(dir, env, "write-tree")
}

// boxSnapshot is snapshot in a box, whose own index is copied each time,
// so its stat cache is the user's own, up to date.
func boxSnapshot(dir, gitDir, boostDir string) (string, error) {
	index := filepath.Join(boostDir, "box-"+strconv.FormatInt(time.Now().UnixNano(), 36)+".index")
	defer func() { _ = os.Remove(index) }()
	return snapshot(dir, index, filepath.Join(gitDir, "index"))
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
// staging directory; --max, the most delta.tar may hold, in bytes.
func boostPack(args []string) (*Manifest, error) {
	fl := flag.NewFlagSet("boost-pack", flag.ContinueOnError)
	dir := fl.String("dir", "/workspace", "")
	base := fl.String("base", "", "")
	out := fl.String("out", "", "")
	limit := fl.Int64("max", 2<<30, "")
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
	tree, err := boxSnapshot(*dir, gitDir, boostDir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Tree: tree}
	m.Head, _ = gitLine(*dir, nil, "rev-parse", "--verify", "-q", "HEAD")
	m.Branch, _ = gitLine(*dir, nil, "symbolic-ref", "-q", "--short", "HEAD")
	var es []entry
	if *base != "" && gitHas(*dir, *base+"^{tree}") {
		if m.Base, err = gitLine(*dir, nil, "rev-parse", *base+"^{tree}"); err != nil {
			return nil, err
		}
		es, err = diffTrees(*dir, m.Base, tree)
	} else {
		m.Full = true
		es, err = listTree(*dir, tree)
	}
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return nil, err
	}
	var send []entry
	var deleted []string
	for _, e := range es {
		switch {
		case e.mode == gitlinkMode && e.status == "D":
			m.Gitlinks = append(m.Gitlinks, Gitlink{Path: e.path})
		case e.mode == gitlinkMode:
			m.Gitlinks = append(m.Gitlinks, Gitlink{Path: e.path, Commit: e.blob})
		case e.status == "D" && e.oldMode == gitlinkMode:
			m.Gitlinks = append(m.Gitlinks, Gitlink{Path: e.path})
		case e.status == "D":
			deleted = append(deleted, e.path)
		default:
			send = append(send, e)
		}
	}
	sizes, err := blobSizes(*dir, send)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, n := range sizes {
		total += n
	}
	if total > *limit {
		return nil, codedError{ExitTooLarge, fmt.Errorf("what changed holds %d bytes, past the %d a sync sends; the largest: %s",
			total, *limit, largest(send, sizes, 5))}
	}
	if err := writeDelta(*dir, filepath.Join(*out, "delta.tar"), send); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(*out, "deleted"), []byte(strings.Join(deleted, "\x00")), 0o644); err != nil {
		return nil, err
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
		// The commits of HEAD no remote ref reaches: local ones, which the
		// boost's mirror lacks.
		n, err := gitLine(*dir, nil, "rev-list", "--count", "HEAD", "--not", "--remotes")
		if err != nil {
			return nil, err
		}
		if n != "0" {
			if _, err := gitRun(*dir, nil, nil, "bundle", "create", "-q", filepath.Join(*out, "head.bundle"), "HEAD", "--not", "--remotes"); err != nil {
				return nil, err
			}
			m.Bundle = true
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return m, os.WriteFile(filepath.Join(*out, "manifest.json"), b, 0o644)
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
}

// boostApply: --dir, the boost's checkout; --staging, what boost-pack
// made, copied here; --shared, a job still runs on the boost, so what it
// wrote is left as it is and the tree is not checked. Without --shared,
// what the boost's own jobs changed since the base is put back first, and
// the tree made must be the one sent.
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
	index := filepath.Join(boostDir, "snapshot.index")
	a := &Applied{Tree: m.Tree, Shared: *shared}
	if !*shared {
		if a.Restored, err = restore(*dir, index, &m, *staging); err != nil {
			return nil, err
		}
	}
	// The delta: deletions first, so a directory a file replaces is empty.
	del, err := os.ReadFile(filepath.Join(*staging, "deleted"))
	if err != nil {
		return nil, err
	}
	for _, p := range splitNUL(del) {
		if err := removeIn(*dir, p); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(filepath.Join(*staging, "delta.tar"))
	if err != nil {
		return nil, err
	}
	err = tarx.Extract(f, *dir)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	if m.Bundle {
		if _, err := gitRun(*dir, nil, nil, "fetch", "-q", filepath.Join(*staging, "head.bundle"), "HEAD"); err != nil {
			return nil, err
		}
	}
	if m.Head != "" {
		if !gitHas(*dir, m.Head+"^{commit}") {
			return nil, fmt.Errorf("HEAD's commit %s is not here", m.Head)
		}
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
		if _, err := gitRun(*dir, nil, nil, "read-tree", m.Head); err != nil {
			return nil, err
		}
	}
	if *shared {
		// What a running job wrote stays, so the work tree is not the
		// tree sent: the tree is made from the base and what was sent,
		// which stores it here for the next send, and checks the send.
		got, err := buildTree(*dir, &m, *staging)
		if err != nil {
			return nil, err
		}
		if got != m.Tree {
			return nil, codedError{ExitTreeMismatch, fmt.Errorf("the tree sent makes %s, not the %s it should", got, m.Tree)}
		}
		return a, nil
	}
	got, err := snapshot(*dir, index, "")
	if err != nil {
		return nil, err
	}
	if got != m.Tree {
		return nil, codedError{ExitTreeMismatch, fmt.Errorf("the tree made is %s, not the %s sent", got, m.Tree)}
	}
	return a, nil
}

// buildTree makes the tree sent from the base and the staging
// directory's delta, in an index of its own, writing its objects here.
func buildTree(dir string, m *Manifest, staging string) (string, error) {
	f, err := os.CreateTemp("", "tower-boost-index-*")
	if err != nil {
		return "", err
	}
	index := f.Name()
	_ = f.Close()
	_ = os.Remove(index)
	defer func() { _ = os.Remove(index) }()
	env := []string{"GIT_INDEX_FILE=" + index}
	if m.Full {
		_, err = gitRun(dir, env, nil, "read-tree", "--empty")
	} else {
		_, err = gitRun(dir, env, nil, "read-tree", m.Base)
	}
	if err != nil {
		return "", err
	}
	del, err := os.ReadFile(filepath.Join(staging, "deleted"))
	if err != nil {
		return "", err
	}
	// Mode 0 removes a path; the id's length is the repository's hash's.
	removed := "0 " + strings.Repeat("0", len(m.Tree)) + "\t"
	var info strings.Builder
	for _, p := range splitNUL(del) {
		info.WriteString(removed + p + "\x00")
	}
	for _, g := range m.Gitlinks {
		if g.Commit == "" {
			info.WriteString(removed + g.Path + "\x00")
		} else {
			info.WriteString(gitlinkMode + " " + g.Commit + "\t" + g.Path + "\x00")
		}
	}
	tf, err := os.Open(filepath.Join(staging, "delta.tar"))
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
	if _, err := gitRun(dir, env, strings.NewReader(info.String()), "update-index", "-z", "--index-info"); err != nil {
		return "", err
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
// whole tree (no base) removes every path not in it. Gitignored paths
// are left as they are.
func restore(dir, index string, m *Manifest, staging string) (int, error) {
	now, err := snapshot(dir, index, "")
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
		for _, g := range m.Gitlinks {
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
		if e.mode == gitlinkMode {
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
		// is there goes, as a file may stand where a directory was.
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
	full := filepath.Join(dir, filepath.FromSlash(p))
	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// A directory where a file was: what it holds is gone already, or
		// is untracked and stays.
		if info, serr := os.Lstat(full); serr != nil || !info.IsDir() {
			return err
		}
		return nil
	}
	for d := filepath.Dir(full); d != filepath.Clean(dir) && strings.HasPrefix(d, filepath.Clean(dir)+string(filepath.Separator)) && removedEmpty(d); d = filepath.Dir(d) {
	}
	return nil
}

// removedEmpty removes the directory d if it is empty, saying whether it
// did.
func removedEmpty(d string) bool { return os.Remove(d) == nil }

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
	now, err := snapshot(dir, filepath.Join(boostDir, "snapshot.index"), "")
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
