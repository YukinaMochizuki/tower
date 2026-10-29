package guestbin_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
)

// git runs git in dir, failing the test on an error, and returns its
// output trimmed.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, file, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// boostRun runs a boost subcommand and returns its exit code, stdout and
// stderr.
func boostRun(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := guestbin.Boost(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// pair is a box and its boost, both clones of origin at its first commit.
type pair struct {
	origin, box, boost string
	first              string
}

func newPair(t *testing.T) pair {
	t.Helper()
	root := t.TempDir()
	p := pair{origin: filepath.Join(root, "origin.git"), box: filepath.Join(root, "box"), boost: filepath.Join(root, "boost")}
	seed := filepath.Join(root, "seed")
	git(t, root, "init", "-q", "-b", "main", seed)
	write(t, filepath.Join(seed, "a.txt"), "a\n", 0o644)
	write(t, filepath.Join(seed, "b.txt"), "b\n", 0o644)
	write(t, filepath.Join(seed, "d", "c.txt"), "c\n", 0o644)
	write(t, filepath.Join(seed, "run.sh"), "#!/bin/sh\n", 0o644)
	write(t, filepath.Join(seed, ".gitignore"), "*.log\n/.ws-cache/\n", 0o644)
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "first")
	git(t, root, "clone", "-q", "--bare", seed, p.origin)
	git(t, root, "clone", "-q", p.origin, p.box)
	git(t, root, "clone", "-q", p.origin, p.boost)
	p.first = git(t, p.box, "rev-parse", "HEAD")
	return p
}

// send packs the box against base and applies it on the boost, as tower
// does through Copy, and returns the manifest.
func (p pair) send(t *testing.T, base string, applyArgs ...string) (guestbin.Manifest, int, string) {
	t.Helper()
	id := "sync-" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	out := filepath.Join(p.box, ".git", guestbin.BoostDir, id)
	code, stdout, stderr := boostRun("boost-pack", "--dir", p.box, "--base", base, "--out", out)
	if code != 0 {
		t.Fatalf("boost-pack exited %d: %s", code, stderr)
	}
	var m guestbin.Manifest
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(p.boost, ".git", guestbin.BoostDir, id)
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("cp", "-a", out, staging).CombinedOutput(); err != nil {
		t.Fatalf("copying the staging directory: %v: %s", err, b)
	}
	if code, _, stderr := boostRun("boost-clean", "--dir", p.box, "--name", id); code != 0 {
		t.Fatalf("boost-clean: %s", stderr)
	}
	code, _, stderr = boostRun(append([]string{"boost-apply", "--dir", p.boost, "--staging", staging}, applyArgs...)...)
	if _, err := os.Stat(staging); err == nil {
		t.Errorf("boost-apply exited %d and left its staging directory", code)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("boost-clean left the box's staging directory")
	}
	return m, code, stderr
}

// TestATreeDiffSendsWhatChanged: the box's work tree, tracked and
// untracked not ignored, reaches the boost as only what changed since the
// base: an add, a change, a delete, a rename, a mode, a symlink, an
// untracked file; never a gitignored one; and the box's index is not
// touched.
func TestATreeDiffSendsWhatChanged(t *testing.T) {
	p := newPair(t)
	write(t, filepath.Join(p.box, "a.txt"), "a, changed\n", 0o644)
	if err := os.Remove(filepath.Join(p.box, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(p.box, "d"), filepath.Join(p.box, "e")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(p.box, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(p.box, "l")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.box, "new.txt"), "untracked\n", 0o644)
	write(t, filepath.Join(p.box, "build.log"), "ignored\n", 0o644)
	write(t, filepath.Join(p.box, ".ws-cache", "x"), "cache\n", 0o644)
	index := read(t, filepath.Join(p.box, ".git", "index"))

	m, code, stderr := p.send(t, p.first)
	if code != 0 {
		t.Fatalf("boost-apply exited %d: %s", code, stderr)
	}
	if read(t, filepath.Join(p.box, ".git", "index")) != index {
		t.Error("boost-pack changed the box's own index")
	}
	if m.Full || m.Files != 5 || m.Deleted != 2 {
		t.Errorf("manifest %+v, want a delta of 5 files sent (a, e/c, run.sh, l, new) and 2 deleted (b, d/c)", m)
	}
	if got := read(t, filepath.Join(p.boost, "a.txt")); got != "a, changed\n" {
		t.Errorf("a.txt on the boost: %q", got)
	}
	for _, gone := range []string{"b.txt", "d", "build.log", ".ws-cache"} {
		if _, err := os.Lstat(filepath.Join(p.boost, gone)); err == nil {
			t.Errorf("%s is on the boost", gone)
		}
	}
	if got := read(t, filepath.Join(p.boost, "e", "c.txt")); got != "c\n" {
		t.Errorf("the renamed e/c.txt on the boost: %q", got)
	}
	if info, err := os.Stat(filepath.Join(p.boost, "run.sh")); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("run.sh on the boost: %v, %v; want mode 0755", info, err)
	}
	if target, err := os.Readlink(filepath.Join(p.boost, "l")); err != nil || target != "a.txt" {
		t.Errorf("the symlink l on the boost: %q, %v", target, err)
	}
	if got := read(t, filepath.Join(p.boost, "new.txt")); got != "untracked\n" {
		t.Errorf("new.txt on the boost: %q", got)
	}
	if git(t, p.boost, "rev-parse", "HEAD") != p.first {
		t.Error("the boost's HEAD is not the box's")
	}
	if status := git(t, p.boost, "status", "--porcelain"); !strings.Contains(status, "a.txt") || !strings.Contains(status, "new.txt") {
		t.Errorf("git status on the boost does not show the box's changes: %q", status)
	}
}

// TestTheBoostsOwnWritesArePutBack: what a job wrote on the boost since
// the last sync is put back before the next, unless a job still runs on
// it (--shared), when it stays and the tree is not checked.
func TestTheBoostsOwnWritesArePutBack(t *testing.T) {
	p := newPair(t)
	write(t, filepath.Join(p.box, "a.txt"), "box 1\n", 0o644)
	m, code, stderr := p.send(t, p.first)
	if code != 0 {
		t.Fatal(stderr)
	}
	// A job on the boost rewrites a tracked file and adds one.
	write(t, filepath.Join(p.boost, "b.txt"), "generated\n", 0o644)
	write(t, filepath.Join(p.boost, "gen.go"), "package gen\n", 0o644)
	write(t, filepath.Join(p.box, "a.txt"), "box 2\n", 0o644)

	shared, code, stderr := p.send(t, m.Tree, "--shared")
	if code != 0 || shared.Files != 1 {
		t.Fatalf("a shared send: exit %d, %+v: %s", code, shared, stderr)
	}
	if read(t, filepath.Join(p.boost, "b.txt")) != "generated\n" || read(t, filepath.Join(p.boost, "a.txt")) != "box 2\n" {
		t.Error("a shared send put back what a running job wrote, or did not apply the box's change")
	}

	write(t, filepath.Join(p.box, "a.txt"), "box 3\n", 0o644)
	if _, code, stderr := p.send(t, shared.Tree); code != 0 {
		t.Fatalf("a send after the job: %d %s", code, stderr)
	}
	if got := read(t, filepath.Join(p.boost, "b.txt")); got != "b\n" {
		t.Errorf("b.txt the job rewrote is %q after the next send, want the box's", got)
	}
	if _, err := os.Stat(filepath.Join(p.boost, "gen.go")); err == nil {
		t.Error("gen.go the job added is still on the boost")
	}
	if read(t, filepath.Join(p.boost, "a.txt")) != "box 3\n" {
		t.Error("the box's latest change did not reach the boost")
	}
}

// TestAWholeTreeIsSentWithoutABase: with no base, or one the box lacks,
// the whole tree is sent, and the boost holds exactly it; one the boost
// lacks is said apart, so tower sends the whole tree.
func TestAWholeTreeIsSentWithoutABase(t *testing.T) {
	p := newPair(t)
	write(t, filepath.Join(p.box, "a.txt"), "box\n", 0o644)
	write(t, filepath.Join(p.boost, "stray.txt"), "only on the boost\n", 0o644)
	m, code, stderr := p.send(t, "0123456789abcdef0123456789abcdef01234567")
	if code != 0 || !m.Full || m.Files != 5 {
		t.Fatalf("a send against a base the box lacks: exit %d, %+v: %s", code, m, stderr)
	}
	if _, err := os.Stat(filepath.Join(p.boost, "stray.txt")); err == nil {
		t.Error("a whole tree sent left a path not in it")
	}
	if read(t, filepath.Join(p.boost, "a.txt")) != "box\n" {
		t.Error("a whole tree sent did not write the box's a.txt")
	}

	// A base the box has and the boost lacks.
	write(t, filepath.Join(p.box, "x.txt"), "x\n", 0o644)
	git(t, p.box, "add", "x.txt")
	git(t, p.box, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "local")
	local := git(t, p.box, "rev-parse", "HEAD")
	write(t, filepath.Join(p.box, "a.txt"), "box 2\n", 0o644)
	if _, code, _ := p.send(t, local); code != guestbin.ExitBaseMissing {
		t.Errorf("a base the boost lacks: exit %d, want %d", code, guestbin.ExitBaseMissing)
	}
}

// TestLocalCommitsReachTheBoost: the box's HEAD, a commit its remote refs
// do not reach, is the boost's HEAD, on the same branch.
func TestLocalCommitsReachTheBoost(t *testing.T) {
	p := newPair(t)
	git(t, p.box, "checkout", "-q", "-b", "topic")
	write(t, filepath.Join(p.box, "a.txt"), "committed\n", 0o644)
	git(t, p.box, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-am", "local")
	head := git(t, p.box, "rev-parse", "HEAD")
	m, code, stderr := p.send(t, p.first)
	if code != 0 || !m.Bundle {
		t.Fatalf("exit %d, %+v: %s", code, m, stderr)
	}
	if got := git(t, p.boost, "rev-parse", "HEAD"); got != head {
		t.Errorf("the boost's HEAD is %s, want the box's %s", got, head)
	}
	if got := git(t, p.boost, "symbolic-ref", "--short", "HEAD"); got != "topic" {
		t.Errorf("the boost is on %q, want topic", got)
	}
	if status := git(t, p.boost, "status", "--porcelain"); status != "" {
		t.Errorf("the boost's status, clean in the box: %q", status)
	}
}

// TestADeltaPastItsCapIsRefused: a delta past --max is refused, naming
// the largest paths.
func TestADeltaPastItsCapIsRefused(t *testing.T) {
	p := newPair(t)
	write(t, filepath.Join(p.box, "big.bin"), strings.Repeat("x", 4096), 0o644)
	code, _, stderr := boostRun("boost-pack", "--dir", p.box, "--base", p.first, "--out", filepath.Join(t.TempDir(), "out"), "--max", "1024")
	if code != guestbin.ExitTooLarge || !strings.Contains(stderr, "big.bin (4096 bytes)") {
		t.Errorf("a delta past its cap: exit %d, %q", code, stderr)
	}
}

// TestABoostJobReports: boost-job runs its command, exits as it did, and
// reports what it changed relative to the tree sent, and its artifacts'
// sizes, refusing one that climbs out of the checkout by its name, or
// leads out of it through a symlink.
func TestABoostJobReports(t *testing.T) {
	p := newPair(t)
	m, code, stderr := p.send(t, p.first)
	if code != 0 {
		t.Fatal(stderr)
	}
	report := filepath.Join(p.boost, ".git", guestbin.BoostDir, "job-1.json")
	code, _, stderr = boostRun("boost-job", "--dir", p.boost, "--tree", m.Tree, "--report", report,
		"--artifact", "out", "--artifact", "missing", "--artifact", "../escape", "--artifact", "esc", "--",
		"sh", "-c", `cd "$1" && echo y >> a.txt && echo g > gen.txt && mkdir out && echo 123456 > out/r && echo z > z.log && ln -s "$2" esc.log && ln -s "$2" esc && exit 3`,
		"sh", p.boost, t.TempDir())
	if code != 3 {
		t.Fatalf("boost-job exited %d, want its command's 3: %s", code, stderr)
	}
	var r guestbin.Report
	if err := json.Unmarshal([]byte(read(t, report)), &r); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range r.Changed {
		got[c.Path] = c.Status
	}
	if r.Total != 4 || got["a.txt"] != "M" || got["gen.txt"] != "A" || got["out/r"] != "A" || got["esc"] != "A" {
		t.Errorf("changed %+v (total %d), want a.txt M, gen.txt and out/r A, not the ignored z.log", r.Changed, r.Total)
	}
	arts := map[string]guestbin.Artifact{}
	for _, a := range r.Artifacts {
		arts[a.Path] = a
	}
	if a := arts["out"]; !a.Exists || a.Bytes != 7 || a.Error != "" {
		t.Errorf("artifact out: %+v", a)
	}
	if a := arts["missing"]; a.Exists || a.Error == "" {
		t.Errorf("artifact missing: %+v", a)
	}
	if a := arts["../escape"]; a.Exists || a.Error == "" {
		t.Errorf("artifact ../escape: %+v", a)
	}
	if a := arts["esc"]; a.Exists || !strings.Contains(a.Error, "leads out") {
		t.Errorf("artifact esc, a symlink out of the checkout: %+v", a)
	}
	// Measured again, as tower does right before it copies them back.
	code, out, stderr := boostRun("boost-measure", "--dir", p.boost, "--artifact", "out", "--artifact", "esc")
	var again []guestbin.Artifact
	if code != 0 || json.Unmarshal([]byte(out), &again) != nil || len(again) != 2 || again[0].Bytes != 7 || again[1].Exists {
		t.Errorf("boost-measure: exit %d, %s %s", code, out, stderr)
	}
	code, _, _ = boostRun("boost-job", "--dir", p.boost, "--", "sh", "-c", "kill -TERM $$")
	if code != 128+15 {
		t.Errorf("a command a signal ended: exit %d, want 143", code)
	}
}

// TestBoostLeftoversAreSwept: boost-clean removes what it is named, and
// every subcommand sweeps what a crash left past a day.
func TestBoostLeftoversAreSwept(t *testing.T) {
	p := newPair(t)
	dir := filepath.Join(p.box, ".git", guestbin.BoostDir)
	old, fresh := filepath.Join(dir, "sync-old"), filepath.Join(dir, "job-fresh.json")
	write(t, filepath.Join(old, "delta.tar"), "x", 0o644)
	write(t, fresh, "{}", 0o644)
	day := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(old, day, day); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := boostRun("boost-clean", "--dir", p.box); code != 0 {
		t.Fatal(stderr)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("a staging directory a day old was not swept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a fresh report was swept")
	}
	if code, out, _ := boostRun("boost-clean", "--dir", p.box, "--name", "job-fresh.json"); code != 0 || !strings.Contains(out, "job-fresh.json") {
		t.Errorf("boost-clean of a report: %d %s", code, out)
	}
	if code, _, _ := boostRun("boost-clean", "--dir", p.box, "--name", "../../a.txt"); code == 0 {
		t.Error("boost-clean removed a name outside its directory")
	}
	entries, _ := os.ReadDir(dir)
	if names := slices.Collect(func(yield func(string) bool) {
		for _, e := range entries {
			if !yield(e.Name()) {
				return
			}
		}
	}); slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, "sync-") || strings.HasPrefix(n, "job-") }) {
		t.Errorf("left in the boost directory: %v", names)
	}
}

// pack packs the box against base, copies the staging directory to the
// boost, and returns where it is there and the manifest, without applying.
func (p pair) pack(t *testing.T, base string, packArgs ...string) (string, guestbin.Manifest) {
	t.Helper()
	id := "sync-" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	out := filepath.Join(p.box, ".git", guestbin.BoostDir, id)
	code, stdout, stderr := boostRun(append([]string{"boost-pack", "--dir", p.box, "--base", base, "--out", out}, packArgs...)...)
	if code != 0 {
		t.Fatalf("boost-pack exited %d: %s", code, stderr)
	}
	var m guestbin.Manifest
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(p.boost, ".git", guestbin.BoostDir, id)
	if err := os.MkdirAll(filepath.Dir(staging), 0o755); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.Command("cp", "-a", out, staging).CombinedOutput(); err != nil {
		t.Fatalf("copying the staging directory: %v: %s", err, b)
	}
	_ = os.RemoveAll(out)
	return staging, m
}

func (p pair) apply(staging string, args ...string) (int, string) {
	code, _, stderr := boostRun(append([]string{"boost-apply", "--dir", p.boost, "--staging", staging}, args...)...)
	return code, stderr
}

// TestSubmodulesAreTheBoostsOwn: a committed submodule checked out in the
// box alone, and an untracked nested repository git add -A takes as one,
// do not keep the tree made on the boost from being the one sent: their
// checkouts are never sent; and the next send is a delta.
func TestSubmodulesAreTheBoostsOwn(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub-origin")
	git(t, root, "init", "-q", "-b", "main", sub)
	write(t, filepath.Join(sub, "s.txt"), "in the submodule\n", 0o644)
	git(t, sub, "add", "-A")
	git(t, sub, "commit", "-q", "-m", "sub")
	seed := filepath.Join(root, "seed")
	git(t, root, "init", "-q", "-b", "main", seed)
	write(t, filepath.Join(seed, "a.txt"), "a\n", 0o644)
	git(t, seed, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	git(t, seed, "commit", "-q", "-m", "first")
	origin, box, boost := filepath.Join(root, "origin.git"), filepath.Join(root, "box"), filepath.Join(root, "boost")
	git(t, root, "clone", "-q", "--bare", seed, origin)
	git(t, root, "clone", "-q", origin, box)
	git(t, root, "clone", "-q", origin, boost)
	git(t, box, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	p := pair{origin: origin, box: box, boost: boost, first: git(t, box, "rev-parse", "HEAD")}
	// An untracked nested repository, with a commit.
	nested := filepath.Join(box, "nested")
	git(t, box, "init", "-q", "-b", "main", nested)
	write(t, filepath.Join(nested, "n.txt"), "nested\n", 0o644)
	git(t, nested, "add", "-A")
	git(t, nested, "commit", "-q", "-m", "nested")
	write(t, filepath.Join(box, "a.txt"), "box\n", 0o644)

	m, code, stderr := p.send(t, p.first)
	if code != 0 {
		t.Fatalf("a send with a submodule and a nested repository: exit %d: %s", code, stderr)
	}
	if len(m.Links) != 2 {
		t.Errorf("the tree's gitlinks: %+v, want sub and nested", m.Links)
	}
	if read(t, filepath.Join(boost, "a.txt")) != "box\n" {
		t.Error("the box's a.txt is not on the boost")
	}
	if _, err := os.Stat(filepath.Join(boost, "nested", "n.txt")); err == nil {
		t.Error("a nested repository's checkout was sent")
	}
	write(t, filepath.Join(box, "a.txt"), "box 2\n", 0o644)
	m2, code, stderr := p.send(t, m.Tree)
	if code != 0 || m2.Full || m2.Files != 1 {
		t.Errorf("the next send: exit %d, %+v: %s", code, m2, stderr)
	}
	// A job's report does not list the submodules it never saw.
	report := filepath.Join(boost, ".git", guestbin.BoostDir, "job-sub.json")
	if code, _, stderr := boostRun("boost-job", "--dir", boost, "--tree", m2.Tree, "--report", report, "--", "true"); code != 0 {
		t.Fatal(stderr)
	}
	var r guestbin.Report
	if err := json.Unmarshal([]byte(read(t, report)), &r); err != nil || r.Total != 0 || r.Error != "" {
		t.Errorf("a job that changed nothing reports %+v (%v)", r, err)
	}
}

// TestPathsAreNeverPatterns: names git would read as pathspec magic or
// globs are restored as the paths they are.
func TestPathsAreNeverPatterns(t *testing.T) {
	p := newPair(t)
	names := []string{":x", "a[1].txt", "a1.txt", "*"}
	for _, n := range names {
		write(t, filepath.Join(p.box, n), "box "+n+"\n", 0o644)
	}
	git(t, p.box, "add", "-A")
	git(t, p.box, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "odd names")
	m, code, stderr := p.send(t, p.first)
	if code != 0 {
		t.Fatal(stderr)
	}
	// A job on the boost rewrites them all; the box changes one more file.
	for _, n := range names {
		write(t, filepath.Join(p.boost, n), "job\n", 0o644)
	}
	write(t, filepath.Join(p.box, "b.txt"), "box b\n", 0o644)
	if _, code, stderr := p.send(t, m.Tree); code != 0 {
		t.Fatalf("a send restoring %v: exit %d: %s", names, code, stderr)
	}
	for _, n := range names {
		if got := read(t, filepath.Join(p.boost, n)); got != "box "+n+"\n" {
			t.Errorf("%s on the boost: %q", n, got)
		}
	}
}

// TestATreeThatIsNotTheOneSentIsRefused: apply's result is checked: a
// delta that makes another tree exits with the mismatch's code.
func TestATreeThatIsNotTheOneSentIsRefused(t *testing.T) {
	p := newPair(t)
	write(t, filepath.Join(p.box, "a.txt"), "box\n", 0o644)
	staging, _ := p.pack(t, p.first)
	// A path the delta writes that the box has not.
	f, err := os.OpenFile(filepath.Join(staging, "delta.tar"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(-1024, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: "extra.txt", Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("x\n"))
	_ = tw.Close()
	_ = f.Close()
	if code, stderr := p.apply(staging); code != guestbin.ExitTreeMismatch {
		t.Errorf("a delta that makes another tree: exit %d, want %d: %s", code, guestbin.ExitTreeMismatch, stderr)
	}
}

// TestTheStagedStateIsTheBoxs: what is staged in the box is staged on the
// boost: git status and git diff there say what they say in the box.
func TestTheStagedStateIsTheBoxs(t *testing.T) {
	p := newPair(t)
	write(t, filepath.Join(p.box, "a.txt"), "staged\n", 0o644)
	git(t, p.box, "add", "a.txt")
	write(t, filepath.Join(p.box, "a.txt"), "staged, then changed\n", 0o644)
	write(t, filepath.Join(p.box, "s.txt"), "new, staged\n", 0o644)
	git(t, p.box, "add", "s.txt")
	git(t, p.box, "rm", "-q", "--cached", "b.txt")
	if _, code, stderr := p.send(t, p.first); code != 0 {
		t.Fatal(stderr)
	}
	for _, args := range [][]string{{"status", "--porcelain"}, {"diff", "--cached", "--stat"}, {"diff", "--stat"}} {
		if box, boost := git(t, p.box, args...), git(t, p.boost, args...); box != boost {
			t.Errorf("git %s:\nin the box:\n%s\non the boost:\n%s", strings.Join(args, " "), box, boost)
		}
	}
}

// TestADirectoryBecomingAFileGoesWithItsIgnoredFiles: a path that is a
// directory on the boost, holding ignored files, and a file in the box,
// is the box's file: what the directory held goes.
func TestADirectoryBecomingAFileGoesWithItsIgnoredFiles(t *testing.T) {
	p := newPair(t)
	write(t, filepath.Join(p.boost, "d", "build.log"), "ignored\n", 0o644)
	if err := os.RemoveAll(filepath.Join(p.box, "d")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.box, "d"), "now a file\n", 0o644)
	if _, code, stderr := p.send(t, p.first); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := read(t, filepath.Join(p.boost, "d")); got != "now a file\n" {
		t.Errorf("d on the boost: %q", got)
	}
}

// TestABoostLackingOriginsCommitsGetsHEADsHistory: a box on a commit of
// origin the boost has not fetched sends a bundle whose prerequisites
// the boost lacks: apply says so with its code, and a pack with all of
// HEAD's history applies.
func TestABoostLackingOriginsCommitsGetsHEADsHistory(t *testing.T) {
	p := newPair(t)
	other := filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-q", p.origin, other)
	write(t, filepath.Join(other, "o.txt"), "origin moved\n", 0o644)
	git(t, other, "add", "-A")
	git(t, other, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "moved")
	git(t, other, "push", "-q", "origin", "main")
	git(t, p.box, "pull", "-q", "--ff-only")
	write(t, filepath.Join(p.box, "a.txt"), "local\n", 0o644)
	git(t, p.box, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-am", "local")
	staging, m := p.pack(t, p.first)
	if !m.Bundle {
		t.Fatal("no bundle for a local commit")
	}
	if code, stderr := p.apply(staging); code != guestbin.ExitHeadMissing {
		t.Fatalf("a bundle the boost lacks the prerequisites of: exit %d, want %d: %s", code, guestbin.ExitHeadMissing, stderr)
	}
	staging, _ = p.pack(t, p.first, "--bundle-all")
	if code, stderr := p.apply(staging); code != 0 {
		t.Fatalf("with HEAD's whole history: exit %d: %s", code, stderr)
	}
	if got, want := git(t, p.boost, "rev-parse", "HEAD"), git(t, p.box, "rev-parse", "HEAD"); got != want {
		t.Errorf("the boost's HEAD %s, the box's %s", got, want)
	}
}
