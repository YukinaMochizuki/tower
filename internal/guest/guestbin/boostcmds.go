package guestbin

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
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
	"strings"
	"syscall"
)

// Manifest is what boost-pack made and says: the box's work tree as a git
// tree, the HEAD it is on and what is staged, for git's state on the
// boost; and the files that differ, sent as files.
type Manifest struct {
	Tree string `json:"tree"`
	// Head is HEAD's commit, "" for none; Branch its branch, "" when
	// detached.
	Head   string `json:"head,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Links are every gitlink of the tree, which the tree made on the
	// boost is checked with, their git state never sent.
	Links []Gitlink `json:"links,omitempty"`
	// Bundle says head.bundle holds the commits of HEAD the box's origin
	// refs do not reach, or all of them (boost-pack --bundle-all).
	Bundle bool `json:"bundle"`
	// IndexTree is the box's index's tree, what is staged there, sent as
	// what differs from Tree (index.tar, index.deleted, IndexGitlinks);
	// "" when the index has none (a merge's conflicts).
	IndexTree     string    `json:"indexTree,omitempty"`
	IndexGitlinks []Gitlink `json:"indexGitlinks,omitempty"`
	// Files and Bytes are what files.tar holds, Bytes with what is staged;
	// Deleted the paths of files.deleted; Over the files not sent, past
	// the cap.
	Files   int       `json:"files"`
	Bytes   int64     `json:"bytes"`
	Deleted int       `json:"deleted"`
	Over    []OverCap `json:"over,omitempty"`
	// Shared says only what the box changed since its last sync was sent,
	// a job of the box running on the boost.
	Shared bool `json:"shared,omitempty"`
	// Sent names the box's manifest as sent, kept in its boost directory
	// (sent-<id>.manifest): what a later sync, and the write-back of the
	// job this sync is for, compare the box with.
	Sent string `json:"sent"`
}

// Gitlink is a submodule of the tree: its path, and the commit the tree
// names, "" for one deleted.
type Gitlink struct {
	Path   string `json:"path"`
	Commit string `json:"commit,omitempty"`
}

// Changed is a path a boosted process changed: A, M or D.
type Changed struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// Report is what boost-job writes once its command ends: what the command
// changed in the checkout, ignored files too, and whether it moved git's
// HEAD or index, which never come back.
type Report struct {
	// Version is 2; a report without one is an earlier boost-job's.
	Version      int       `json:"version"`
	Changed      []Changed `json:"changed"`
	Total        int       `json:"total"`
	HeadMoved    bool      `json:"headMoved,omitempty"`
	IndexChanged bool      `json:"indexChanged,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// ReportVersion is the version of the Report boost-job writes.
const ReportVersion = 2

// Applied is what boost-apply did.
type Applied struct {
	Tree    string `json:"tree"`
	Shared  bool   `json:"shared"`
	Files   int    `json:"files"`
	Deleted int    `json:"deleted"`
	// Staged says the box's index, what is staged there, is the boost's.
	Staged bool `json:"staged"`
	// Mismatch are the tracked paths whose content the boost holds is not
	// the box's, though their manifests' records are the same: to send
	// again, whatever their records say.
	Mismatch []string `json:"mismatch,omitempty"`
}

// Back is what boost-back packed of a job's changes, to bring back.
type Back struct {
	Files   int       `json:"files"`
	Bytes   int64     `json:"bytes"`
	Deleted int       `json:"deleted"`
	Over    []OverCap `json:"over,omitempty"`
	// Moved are the first paths packed, Total how many.
	Moved []Moved `json:"moved"`
	Total int     `json:"total"`
}

// Moved is a path brought back: A, M or D, and its bytes.
type Moved struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Bytes  int64  `json:"bytes,omitempty"`
}

// WroteBack is what boost-writeback did in the box.
type WroteBack struct {
	Files   int   `json:"files"`
	Bytes   int64 `json:"bytes"`
	Deleted int   `json:"deleted"`
	// Conflicts are the paths the box changed since the job's sync began,
	// left as the box has them; Unchanged those the box already had as the
	// job left them, written back by another job, or as the box made them.
	Conflicts []string `json:"conflicts,omitempty"`
	Unchanged []string `json:"unchanged,omitempty"`
}

// boostPack: --dir, the box's checkout; --out, the staging directory;
// --max, the most it sends, in bytes; --boost, the boost's manifest
// (boost-manifest), copied here; --since, the box's manifest last sent
// (a Sent name); --shared, a job of the box runs on the boost; --force,
// a path sent whatever the manifests say, repeated; --bundle-all, HEAD's
// whole history bundled. Not shared, what differs from the boost's
// manifest is sent, and what the boost has that the box has not is
// deleted there; shared, only what the box changed since --since, so
// what the running job writes stays.
func boostPack(args []string) (*Manifest, error) {
	fl := flag.NewFlagSet("boost-pack", flag.ContinueOnError)
	dir := fl.String("dir", "/workspace", "")
	out := fl.String("out", "", "")
	limit := fl.Int64("max", 2<<30, "")
	theirs := fl.String("boost", "", "")
	since := fl.String("since", "", "")
	shared := fl.Bool("shared", false, "")
	bundleAll := fl.Bool("bundle-all", false, "")
	var force stringList
	fl.Var(&force, "force", "")
	if err := fl.Parse(args); err != nil {
		return nil, err
	}
	if *out == "" {
		return nil, errors.New("--out is required")
	}
	if !*shared && *theirs == "" {
		return nil, errors.New("--boost is required, but --shared")
	}
	gitDir, boostDir, err := boostGitDir(*dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return nil, err
	}
	m, gitBytes, inTree, err := packGit(*dir, gitDir, boostDir, *out, *bundleAll)
	if err != nil {
		return nil, err
	}
	m.Shared = *shared
	if gitBytes > *limit {
		return nil, codedError{ExitTooLarge, fmt.Errorf("what is staged and HEAD's commits the boost lacks hold %d bytes, past the %d a sync sends", gitBytes, *limit)}
	}
	now, err := walk(*dir)
	if err != nil {
		return nil, err
	}
	var last []fileRec
	haveLast := false
	if *since != "" {
		if err := checkSentName(*since); err != nil {
			return nil, err
		}
		last, err = readManifest(filepath.Join(boostDir, *since+".manifest"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		haveLast = err == nil
		if !haveLast {
			last = nil
			if *shared {
				return nil, codedError{ExitNoSince, fmt.Errorf("the box's last sync, %s, is not kept", *since)}
			}
		}
	} else if *shared {
		return nil, codedError{ExitNoSince, errors.New("a shared sync sends what changed since the last: --since is required")}
	}
	var changes []fileChange
	if *shared {
		changes = diffRecs(last, now, fileRec.sameHere)
	} else {
		boost, err := readManifest(*theirs)
		if err != nil {
			return nil, err
		}
		changes = diffRecs(boost, now, fileRec.sameAs)
		// A file the box changed since its last sync to this boost though
		// its size and mtime did not, and one the boost holds other content
		// of: sent whatever the records say. A boost the box has not synced
		// to holds its warm golden's files, and its own setup's: every
		// ignored file the box touched since its baseline is sent, untouched
		// ones compared by their records alone. A file of the box's git tree
		// (tracked, or untracked and not ignored) is not: the boost checks
		// its content against that tree (checkTree), and is sent it again.
		nowIdx := indexRecs(now)
		more := map[string]bool{}
		touched, err := touchedSince(boostDir, now, last, haveLast)
		if err != nil {
			return nil, err
		}
		for _, p := range touched {
			if haveLast || !inTree[p] {
				more[p] = true
			}
		}
		for _, p := range force {
			if _, ok := nowIdx[p]; ok {
				more[p] = true
			}
		}
		for _, c := range changes {
			delete(more, c.Rec.Path)
		}
		for p := range more {
			changes = append(changes, fileChange{'M', nowIdx[p]})
		}
		slices.SortFunc(changes, func(a, b fileChange) int { return strings.Compare(a.Rec.Path, b.Rec.Path) })
	}
	var sends []fileRec
	var deleted []string
	for _, c := range changes {
		if c.Status == 'D' {
			deleted = append(deleted, c.Rec.Path)
		} else {
			sends = append(sends, c.Rec)
		}
	}
	root, err := os.OpenRoot(*dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	pk, err := writeFiles(root, filepath.Join(*out, "files.tar"), sends, *limit-gitBytes)
	if err != nil {
		return nil, err
	}
	if err := writeNUL(filepath.Join(*out, "files.deleted"), deleted); err != nil {
		return nil, err
	}
	m.Files, m.Bytes, m.Deleted, m.Over = pk.files, pk.bytes+gitBytes, len(deleted), pk.over
	// The box as sent: what was sent as it was when it was, the rest as
	// it was walked.
	sentIdx := indexRecs(pk.sent)
	for i, r := range now {
		if s, ok := sentIdx[r.Path]; ok {
			now[i] = s
		}
	}
	m.Sent = "sent-" + strings.TrimPrefix(filepath.Base(*out), "sync-")
	if err := writeManifest(filepath.Join(boostDir, m.Sent+".manifest"), now); err != nil {
		return nil, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return m, os.WriteFile(filepath.Join(*out, "manifest.json"), b, 0o644)
}

// baselineName is the box's manifest as its /workspace was once its
// repositories were checked out from its warm golden, before its setup
// steps ran (boost-baseline): what a boost made of that golden holds, but
// for its own setup's writes.
const baselineName = "baseline.manifest"

// touchedSince are the files and links of now the box touched since it
// last synced to the boost (last, when haveLast), or else since its
// baseline: their inode or ctime moved, or they were not there. With
// neither, every one is.
func touchedSince(boostDir string, now, last []fileRec, haveLast bool) ([]string, error) {
	base := last
	if !haveLast {
		var err error
		base, err = readManifest(filepath.Join(boostDir, baselineName))
		if errors.Is(err, fs.ErrNotExist) {
			base = nil
		} else if err != nil {
			return nil, err
		}
	}
	idx := indexRecs(base)
	var out []string
	for _, r := range now {
		if r.Type == 'd' {
			continue
		}
		if b, ok := idx[r.Path]; haveLast && !ok {
			// New since the last sync: the records' diff sends it.
			continue
		} else if !ok || !b.sameHere(r) {
			out = append(out, r.Path)
		}
	}
	return out, nil
}

// boostBaseline: --dir, a box's checkout: its manifest as it is, kept in
// its boost directory as its baseline (baselineName), which a sync to a
// boost it has not synced to compares the box with.
func boostBaseline(args []string) (map[string]int, error) {
	fl := flag.NewFlagSet("boost-baseline", flag.ContinueOnError)
	dir := fl.String("dir", "/workspace", "")
	if err := fl.Parse(args); err != nil {
		return nil, err
	}
	_, boostDir, err := boostGitDir(*dir)
	if err != nil {
		return nil, err
	}
	recs, err := walk(*dir)
	if err != nil {
		return nil, err
	}
	return map[string]int{"paths": len(recs)}, writeManifest(filepath.Join(boostDir, baselineName), recs)
}

// checkSentName refuses a name that is no sent manifest's.
func checkSentName(n string) error {
	if !strings.HasPrefix(n, "sent-") || strings.ContainsAny(n, "/\\") || n == "sent-" {
		return fmt.Errorf("%q is no sent manifest's name", n)
	}
	return nil
}

// packGit packs git's state of the box into out: its work tree's tree's
// trees, for the boost's check (trees.pack); what is staged apart from
// it (index.tar, index.deleted); and HEAD's commits no origin ref
// reaches, or all of them (head.bundle). bytes is what they hold; inTree
// the paths of the work tree's tree, which the boost's check covers.
func packGit(dir, gitDir, boostDir, out string, bundleAll bool) (*Manifest, int64, map[string]bool, error) {
	tree, indexTree, err := boxSnapshot(dir, gitDir, boostDir)
	if err != nil {
		return nil, 0, nil, err
	}
	m := &Manifest{Tree: tree}
	m.Head, _ = gitLine(dir, nil, "rev-parse", "--verify", "-q", "HEAD")
	m.Branch, _ = gitLine(dir, nil, "symbolic-ref", "-q", "--short", "HEAD")
	all, err := listTree(dir, tree)
	if err != nil {
		return nil, 0, nil, err
	}
	m.Links = linksOf(all)
	inTree := make(map[string]bool, len(all))
	for _, e := range all {
		inTree[e.path] = true
	}
	if err := writeTrees(dir, tree, filepath.Join(out, "trees.pack")); err != nil {
		return nil, 0, nil, err
	}
	var staged []entry
	var stagedDeleted []string
	if indexTree != "" {
		diff, err := diffTrees(dir, tree, indexTree)
		if err != nil {
			return nil, 0, nil, err
		}
		staged, stagedDeleted = splitDelta(diff, &m.IndexGitlinks)
		m.IndexTree = indexTree
	}
	sizes, err := blobSizes(dir, staged)
	if err != nil {
		return nil, 0, nil, err
	}
	var total int64
	for _, n := range sizes {
		total += n
	}
	if err := writeDelta(dir, filepath.Join(out, "index.tar"), staged); err != nil {
		return nil, 0, nil, err
	}
	if err := writeNUL(filepath.Join(out, "index.deleted"), stagedDeleted); err != nil {
		return nil, 0, nil, err
	}
	if m.Head != "" {
		not := []string{"--not", "--remotes=origin"}
		if bundleAll {
			not = nil
		}
		n, err := gitLine(dir, nil, append([]string{"rev-list", "--count", "HEAD"}, not...)...)
		if err != nil {
			return nil, 0, nil, err
		}
		if n != "0" {
			bundle := filepath.Join(out, "head.bundle")
			if _, err := gitRun(dir, nil, nil, append([]string{"bundle", "create", "-q", bundle, "HEAD"}, not...)...); err != nil {
				return nil, 0, nil, err
			}
			m.Bundle = true
			info, err := os.Stat(bundle)
			if err != nil {
				return nil, 0, nil, err
			}
			total += info.Size()
		}
	}
	return m, total, inTree, nil
}

// writeTrees packs tree and every tree under it, no blob: what the boost
// reads the tree's paths from, to check its own work tree against.
func writeTrees(dir, tree, file string) error {
	out, err := gitRun(dir, nil, nil, "ls-tree", "-r", "-t", "-z", "--full-tree", tree)
	if err != nil {
		return err
	}
	ids := []string{tree}
	for _, rec := range splitNUL(out) {
		meta, _, ok := strings.Cut(rec, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 && f[1] == "tree" {
			ids = append(ids, f[2])
		}
	}
	pack, err := gitRun(dir, nil, strings.NewReader(strings.Join(ids, "\n")+"\n"), "pack-objects", "-q", "--stdout")
	if err != nil {
		return err
	}
	return os.WriteFile(file, pack, 0o644)
}

// boostApply: --dir, the boost's checkout; --staging, what boost-pack
// made, copied here; --shared, a job of the box still runs on the boost,
// so the work tree is not checked. The deletions, then the files, each
// confined to the checkout; then, not shared, the work tree is checked
// against the box's tree, a tracked path whose content differs said in
// Mismatch. HEAD is the box's commit, on its branch, and the index the
// box's.
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
	// HEAD's commits first: a boost that lacks them is sent all of them.
	if m.Bundle {
		if _, err := gitRun(*dir, nil, nil, "fetch", "-q", filepath.Join(*staging, "head.bundle"), "HEAD"); err != nil {
			return nil, codedError{ExitHeadMissing, err}
		}
	}
	if m.Head != "" && !gitHas(*dir, m.Head+"^{commit}") {
		return nil, codedError{ExitHeadMissing, fmt.Errorf("HEAD's commit %s is not here", m.Head)}
	}
	a := &Applied{Tree: m.Tree, Shared: *shared}
	root, err := os.OpenRoot(*dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	deleted, err := readNUL(filepath.Join(*staging, "files.deleted"))
	if err != nil {
		return nil, err
	}
	for _, p := range deepestFirst(deleted) {
		if err := checkPath(p); err != nil {
			return nil, err
		}
		if under, err := underLink(root, p); err != nil || under {
			// What is under a link is not the checkout's to delete.
			if err != nil {
				return nil, err
			}
			continue
		}
		if err := root.RemoveAll(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		a.Deleted++
	}
	if a.Files, err = applyTar(root, filepath.Join(*staging, "files.tar")); err != nil {
		return nil, err
	}
	if !*shared {
		if a.Mismatch, err = checkTree(*dir, boostDir, *staging, &m); err != nil {
			return nil, err
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

// underLink says a directory of p is a link, or not a directory: p is
// nothing of the checkout's there.
func underLink(root *os.Root, p string) (bool, error) {
	for d := path.Dir(p); d != "."; d = path.Dir(d) {
		info, err := root.Lstat(d)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return true, nil
		}
	}
	return false, nil
}

// applyTar applies each entry of the tar file in root, in order.
func applyTar(root *os.Root, file string) (int, error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	n := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if err := applyEntry(root, hdr, tr); err != nil {
			return n, err
		}
		if hdr.Typeflag == tar.TypeReg {
			n++
		}
	}
}

// checkTree checks the work tree, files made the box's, against the box's
// tree, as git sees it: the tracked paths whose content is not the box's,
// though their records were the same, but those over the cap, which were
// not sent, and those never synced.
func checkTree(dir, boostDir, staging string, m *Manifest) ([]string, error) {
	pack, err := os.Open(filepath.Join(staging, "trees.pack"))
	if err != nil {
		return nil, err
	}
	_, err = gitRun(dir, nil, pack, "unpack-objects", "-q")
	_ = pack.Close()
	if err != nil {
		return nil, err
	}
	index := filepath.Join(boostDir, "snapshot.index")
	// The tree sent's paths are tracked here as in the box, a file an
	// ignore rule matches too.
	if _, err := snapshot(dir, index, m.Tree); err != nil {
		return nil, err
	}
	got, err := withLinks(dir, index, m.Links, len(m.Tree))
	if err != nil || got == m.Tree {
		return nil, err
	}
	out, err := gitRun(dir, nil, nil, "diff-tree", "-r", "-z", "--name-only", "--no-renames", m.Tree, got)
	if err != nil {
		return nil, err
	}
	over := map[string]bool{}
	for _, o := range m.Over {
		over[o.Path] = true
	}
	var mismatch []string
	for _, p := range splitNUL(out) {
		// Each instance's own .ws-cache, which git may not ignore, is never
		// synced, so never the box's.
		if p != "" && !over[p] && !excluded(p) {
			mismatch = append(mismatch, p)
		}
	}
	return mismatch, nil
}

// boostManifest: --dir, a checkout; --out, where to write its manifest.
func boostManifest(args []string) (map[string]int, error) {
	fl := flag.NewFlagSet("boost-manifest", flag.ContinueOnError)
	dir := fl.String("dir", "/workspace", "")
	out := fl.String("out", "", "")
	if err := fl.Parse(args); err != nil {
		return nil, err
	}
	if *out == "" {
		return nil, errors.New("--out is required")
	}
	if _, _, err := boostGitDir(*dir); err != nil {
		return nil, err
	}
	recs, err := walk(*dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return nil, err
	}
	return map[string]int{"paths": len(recs)}, writeManifest(*out, recs)
}

// boostJob: --dir, the boost's checkout; --report, where to write the
// Report, in the boost directory (job-<id>.json); then "--" and the
// command, run in the working directory it is given. The checkout's
// manifest is taken before the command starts and once it ends: what
// differs, ignored files too, is the job's changes, kept beside the
// report (job-<id>.changes) for boost-back, and said in it. The signals
// a process's group is sent reach the command; this waits for its end.
// Both walks run in the process's cgroup, so they count in its peak.
func boostJob(args []string, stdout, stderr io.Writer, main bool) (int, syscall.Signal) {
	fl := flag.NewFlagSet("boost-job", flag.ContinueOnError)
	dir := fl.String("dir", "/workspace", "")
	report := fl.String("report", "", "")
	// The command's working directory, which it otherwise inherits; for
	// a caller that runs this in-process, such as the fake provider.
	cwd := fl.String("cwd", "", "")
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
	var start []fileRec
	var startErr error
	head0, index0 := "", ""
	if *report != "" {
		_ = os.MkdirAll(filepath.Dir(*report), 0o755)
		start, startErr = walk(*dir)
		head0, index0 = gitState(*dir)
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
		writeReport(*dir, *report, start, startErr, head0, index0)
	}
	return code, sig
}

// gitState is the checkout's HEAD and a hash of its index, "" for none.
func gitState(dir string) (head, index string) {
	head, _ = gitLine(dir, nil, "rev-parse", "-q", "--verify", "HEAD")
	if gitDir, err := gitLine(dir, nil, "rev-parse", "--absolute-git-dir"); err == nil {
		if b, err := os.ReadFile(filepath.Join(gitDir, "index")); err == nil {
			sum := sha256.Sum256(b)
			index = hex.EncodeToString(sum[:])
		}
	}
	return head, index
}

// changesPath is where boost-job keeps a job's changes, beside its report.
func changesPath(report string) string {
	return strings.TrimSuffix(report, ".json") + ".changes"
}

// writeReport writes what the job changed since start, and keeps the
// changes for boost-back; a failure is said in the report.
func writeReport(dir, file string, start []fileRec, startErr error, head0, index0 string) {
	r := Report{Version: ReportVersion, Changed: []Changed{}}
	var changes []fileChange
	end, err := walk(dir)
	switch {
	case startErr != nil:
		r.Error = "its files before it started could not be read: " + startErr.Error()
	case err != nil:
		r.Error = "its files once it ended could not be read: " + err.Error()
	default:
		changes = diffRecs(start, end, fileRec.sameHere)
	}
	var list []string
	for _, c := range changes {
		list = append(list, string(c.Status), c.Rec.Path)
	}
	if err := writeNUL(changesPath(file), list); err != nil && r.Error == "" {
		r.Error = "its changes could not be kept: " + err.Error()
	}
	r.Total = len(changes)
	for _, c := range changes[:min(len(changes), changedKept)] {
		r.Changed = append(r.Changed, Changed{Path: c.Rec.Path, Status: string(c.Status)})
	}
	head1, index1 := gitState(dir)
	r.HeadMoved, r.IndexChanged = head1 != head0, index1 != index0
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		_ = os.Rename(tmp, file)
	}
}

// boostBack: --dir, the boost's checkout; --report, a job's report, its
// changes beside it; --out, the staging directory; --max, the most it
// brings back. What the job added or changed is packed as it is now, in
// path order, a file past the cap left out and said over; what it
// deleted, or what is gone since, listed deleted.
func boostBack(args []string) (*Back, error) {
	fl := flag.NewFlagSet("boost-back", flag.ContinueOnError)
	dir := fl.String("dir", "/workspace", "")
	report := fl.String("report", "", "")
	out := fl.String("out", "", "")
	limit := fl.Int64("max", 2<<30, "")
	if err := fl.Parse(args); err != nil {
		return nil, err
	}
	if *report == "" || *out == "" {
		return nil, errors.New("--report and --out are required")
	}
	if _, _, err := boostGitDir(*dir); err != nil {
		return nil, err
	}
	list, err := readNUL(changesPath(*report))
	if err != nil {
		return nil, err
	}
	if len(list)%2 != 0 {
		return nil, errors.New("a job's changes are cut short")
	}
	root, err := os.OpenRoot(*dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var sends []fileRec
	var deleted []string
	status := map[string]string{}
	for i := 0; i < len(list); i += 2 {
		st, p := list[i], list[i+1]
		if err := checkPath(p); err != nil {
			return nil, err
		}
		r, ok, err := lstatRec(root, p)
		if err != nil {
			return nil, err
		}
		if st == "D" || !ok {
			deleted = append(deleted, p)
			status[p] = "D"
			continue
		}
		sends = append(sends, r)
		status[p] = st
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return nil, err
	}
	pk, err := writeFiles(root, filepath.Join(*out, "files.tar"), sends, *limit)
	if err != nil {
		return nil, err
	}
	if err := writeNUL(filepath.Join(*out, "files.deleted"), deleted); err != nil {
		return nil, err
	}
	b := &Back{Files: pk.files, Bytes: pk.bytes, Deleted: len(deleted), Over: pk.over, Moved: []Moved{}}
	var moved []Moved
	for _, r := range pk.sent {
		// A directory made goes with what it holds, which is listed.
		if r.Type != 'd' {
			moved = append(moved, Moved{Path: r.Path, Status: status[r.Path], Bytes: r.Size})
		}
	}
	for _, p := range deleted {
		moved = append(moved, Moved{Path: p, Status: "D"})
	}
	slices.SortFunc(moved, func(a, b Moved) int { return strings.Compare(a.Path, b.Path) })
	b.Total = len(moved)
	b.Moved = append(b.Moved, moved[:min(len(moved), changedKept)]...)
	return b, nil
}

// boostWriteback: --dir, the box's checkout; --staging, what boost-back
// packed, copied here; --sent, the box's manifest as the job's sync sent
// it; --last, the box's manifest last sent, kept up to date with what is
// written back. Each path the box changed since the job's sync began, or
// whose directory it did, is a conflict: the box's stays. The rest is
// applied, deletions first, confined to the checkout, never through a
// link.
func boostWriteback(args []string) (*WroteBack, error) {
	fl := flag.NewFlagSet("boost-writeback", flag.ContinueOnError)
	dir := fl.String("dir", "/workspace", "")
	staging := fl.String("staging", "", "")
	sent := fl.String("sent", "", "")
	last := fl.String("last", "", "")
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
	if err := checkSentName(*sent); err != nil {
		return nil, err
	}
	base, err := readManifest(filepath.Join(boostDir, *sent+".manifest"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, codedError{ExitNoSince, fmt.Errorf("the box's files as its sync sent them, %s, are not kept: nothing is brought back", *sent)}
	}
	if err != nil {
		return nil, err
	}
	was := indexRecs(base)
	root, err := os.OpenRoot(*dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	w := &writer{root: root, was: was, made: map[string]bool{}, wrote: map[string]bool{}, gone: map[string]bool{}}
	deleted, err := readNUL(filepath.Join(*staging, "files.deleted"))
	if err != nil {
		return nil, err
	}
	out := &WroteBack{}
	for _, p := range deepestFirst(deleted) {
		if err := checkPath(p); err != nil {
			return nil, err
		}
		ok, err := w.remove(p)
		if err != nil {
			return nil, err
		}
		if ok {
			out.Deleted++
		}
	}
	f, err := os.Open(filepath.Join(*staging, "files.tar"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		ok, err := w.apply(hdr, tr)
		if err != nil {
			return nil, err
		}
		if ok && hdr.Typeflag == tar.TypeReg {
			out.Files++
			out.Bytes += hdr.Size
		}
	}
	out.Conflicts, out.Unchanged = w.conflicts, w.unchanged
	if *last != "" {
		if err := checkSentName(*last); err != nil {
			return nil, err
		}
		if err := w.keepLast(filepath.Join(boostDir, *last+".manifest")); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// writer writes a job's changes back in the box, the box winning.
type writer struct {
	root *os.Root
	// was is the box as the job's sync sent it.
	was map[string]fileRec
	// made are the directories this write-back made or kept; skipped
	// those it left the box's, and all under them.
	made, wrote, gone map[string]bool
	skipped           []string
	conflicts         []string
	unchanged         []string
}

// changed says the box changed p since the job's sync began: it is there
// and was not, or was and is not, or is not as it was. A directory that
// is one still is the same.
func (w *writer) changed(p string) (bool, error) {
	now, ok, err := lstatRec(w.root, p)
	if err != nil {
		return false, err
	}
	was, had := w.was[p]
	switch {
	case ok != had:
		return true, nil
	case !ok:
		return false, nil
	case was.Type == 'd' && now.Type == 'd':
		return false, nil
	}
	return !was.sameHere(now), nil
}

// under says p is under a path left the box's.
func (w *writer) under(p string) bool {
	for _, d := range w.skipped {
		if strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// parentsHold says every directory of p is one in the box, or was not
// there when the job's sync began and is not now: one the box made a file
// or a link, or removed, is the box's.
func (w *writer) parentsHold(p string) (bool, error) {
	var dirs []string
	for d := path.Dir(p); d != "."; d = path.Dir(d) {
		dirs = append(dirs, d)
	}
	for _, d := range slices.Backward(dirs) {
		if w.made[d] {
			continue
		}
		info, err := w.root.Lstat(d)
		switch {
		case err == nil && info.IsDir():
			continue
		case err == nil:
			return false, nil
		case !errors.Is(err, fs.ErrNotExist):
			return false, err
		}
		if _, had := w.was[d]; had {
			return false, nil
		}
		return true, nil
	}
	return true, nil
}

func (w *writer) conflict(p string) {
	w.conflicts = append(w.conflicts, p)
}

// remove deletes p as the job did, unless the box changed it; a directory
// only once empty: what the box put in it since stays.
func (w *writer) remove(p string) (bool, error) {
	hold, err := w.parentsHold(p)
	if err != nil {
		return false, err
	}
	if w.under(p) || !hold {
		w.conflict(p)
		return false, nil
	}
	if _, ok, err := lstatRec(w.root, p); err != nil {
		return false, err
	} else if !ok {
		// Gone already, as the job left it.
		w.unchanged = append(w.unchanged, p)
		return false, nil
	}
	changed, err := w.changed(p)
	if err != nil {
		return false, err
	}
	if changed {
		w.conflict(p)
		w.skipped = append(w.skipped, p)
		return false, nil
	}
	err = w.root.Remove(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST):
		w.conflict(p)
		return false, nil
	case err != nil:
		return false, err
	}
	w.gone[p] = true
	return true, nil
}

// apply writes one entry back, unless the box changed its path, or one of
// its directories, since the job's sync began.
func (w *writer) apply(hdr *tar.Header, body io.Reader) (bool, error) {
	p := strings.TrimSuffix(hdr.Name, "/")
	if err := checkPath(p); err != nil {
		return false, err
	}
	skip := func() (bool, error) {
		w.conflict(p)
		if hdr.Typeflag == tar.TypeDir {
			w.skipped = append(w.skipped, p)
		}
		return false, nil
	}
	if w.under(p) {
		return skip()
	}
	// Its directories first: one the box made a link is never gone
	// through.
	hold, err := w.parentsHold(p)
	if err != nil {
		return false, err
	}
	if !hold {
		return skip()
	}
	if now, ok, err := lstatRec(w.root, p); err != nil {
		return false, err
	} else if ok && sameAsEntry(now, hdr) {
		// The box has it as the job left it: written back by another job
		// that wrote it too, or made so in the box.
		w.unchanged = append(w.unchanged, p)
		if hdr.Typeflag == tar.TypeDir {
			w.made[p] = true
		}
		return false, nil
	}
	changed, err := w.changed(p)
	if err != nil {
		return false, err
	}
	if changed {
		return skip()
	}
	if info, err := w.root.Lstat(p); err == nil && info.IsDir() && hdr.Typeflag != tar.TypeDir {
		// A directory the job made a file or a link: only if the box put
		// nothing in it since.
		if err := w.root.Remove(p); err != nil {
			return skip()
		}
	}
	if err := applyEntry(w.root, hdr, body); err != nil {
		return false, err
	}
	if hdr.Typeflag == tar.TypeDir {
		w.made[p] = true
	}
	w.wrote[p] = true
	return true, nil
}

// sameAsEntry says r is what the tar entry hdr makes: its type and mode,
// a file's size and mtime, a link's target.
func sameAsEntry(r fileRec, hdr *tar.Header) bool {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return r.Type == 'd' && r.Mode == fs.FileMode(hdr.Mode).Perm()
	case tar.TypeSymlink:
		return r.Type == 'l' && r.Link == hdr.Linkname
	case tar.TypeReg:
		return r.Type == 'f' && r.Mode == fs.FileMode(hdr.Mode).Perm() && r.Size == hdr.Size && r.MTime == hdr.ModTime.UnixNano()
	}
	return false
}

// keepLast makes the box's manifest last sent hold what was written back
// as it is now, and not what was deleted, so a later sync compares the
// box's own changes since, not the job's.
func (w *writer) keepLast(file string) error {
	recs, err := readManifest(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	idx := indexRecs(recs)
	for p := range w.gone {
		delete(idx, p)
	}
	for p := range w.wrote {
		r, ok, err := lstatRec(w.root, p)
		if err != nil {
			return err
		}
		if ok {
			idx[p] = r
		}
	}
	recs = recs[:0]
	for _, r := range idx {
		recs = append(recs, r)
	}
	slices.SortFunc(recs, func(a, b fileRec) int { return strings.Compare(a.Path, b.Path) })
	return writeManifest(file, recs)
}

// Cleaned is what boost-clean removed.
type Cleaned struct {
	Removed []string `json:"removed"`
}

// boostClean: --dir, a checkout; --name, a staging directory, report or
// sent manifest of its boost directory (sync-..., job-..., sent-...),
// removed if there; --prune-sent, every sent manifest but those --keep
// names removed. Leftovers past boostKeepFor are swept as by every
// subcommand.
func boostClean(args []string) (*Cleaned, error) {
	fl := flag.NewFlagSet("boost-clean", flag.ContinueOnError)
	dir := fl.String("dir", "/workspace", "")
	var names, keep stringList
	fl.Var(&names, "name", "")
	fl.Var(&keep, "keep", "")
	prune := fl.Bool("prune-sent", false, "")
	if err := fl.Parse(args); err != nil {
		return nil, err
	}
	_, boostDir, err := boostGitDir(*dir)
	if err != nil {
		return nil, err
	}
	c := &Cleaned{Removed: []string{}}
	if *prune {
		entries, err := os.ReadDir(boostDir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			n, ok := strings.CutSuffix(e.Name(), ".manifest")
			if ok && strings.HasPrefix(n, "sent-") && !slices.Contains(keep, n) {
				names = append(names, e.Name())
			}
		}
	}
	for _, n := range names {
		if n == "" || strings.ContainsAny(n, "/\\") || n == "." || n == ".." ||
			(!strings.HasPrefix(n, "sync-") && !strings.HasPrefix(n, "job-") && !strings.HasPrefix(n, "sent-")) {
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
