package guestbin_test

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
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
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func exists(file string) bool {
	_, err := os.Lstat(file)
	return err == nil
}

// boostRun runs a boost subcommand and returns its exit code, stdout and
// stderr.
func boostRun(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := guestbin.Boost(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// pair is a box and its boost: a clone of origin at its first commit, and
// a copy of it, as a box and its boost are both made of the box's warm
// golden. last is the box's manifest last sent.
type pair struct {
	origin, box, boost string
	first              string
	last               string
	n                  int
}

func newPair(t *testing.T) *pair {
	t.Helper()
	root := t.TempDir()
	p := &pair{origin: filepath.Join(root, "origin.git"), box: filepath.Join(root, "box"), boost: filepath.Join(root, "boost")}
	seed := filepath.Join(root, "seed")
	git(t, root, "init", "-q", "-b", "main", seed)
	write(t, filepath.Join(seed, "a.txt"), "a\n", 0o644)
	write(t, filepath.Join(seed, "b.txt"), "b\n", 0o644)
	write(t, filepath.Join(seed, "d", "c.txt"), "c\n", 0o644)
	write(t, filepath.Join(seed, "run.sh"), "#!/bin/sh\n", 0o755)
	write(t, filepath.Join(seed, ".gitignore"), "*.log\n/.ws-cache/\n/bin/\nnode_modules/\n", 0o644)
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "first")
	git(t, root, "clone", "-q", "--bare", seed, p.origin)
	git(t, root, "clone", "-q", p.origin, p.box)
	// Ignored files the golden holds: dependencies installed.
	write(t, filepath.Join(p.box, "node_modules", "dep", "index.js"), "v1\n", 0o644)
	// The box's baseline, as tower takes it once its repository is checked
	// out from its golden, of which the boost is made too.
	if code, _, stderr := boostRun("boost-baseline", "--dir", p.box); code != 0 {
		t.Fatalf("boost-baseline: %s", stderr)
	}
	if out, err := exec.Command("cp", "-a", p.box, p.boost).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v: %s", err, out)
	}
	p.first = git(t, p.box, "rev-parse", "HEAD")
	return p
}

func (p *pair) boostDir(dir string) string { return filepath.Join(dir, ".git", guestbin.BoostDir) }

func (p *pair) id() string {
	p.n++
	return fmt.Sprintf("%d%d", time.Now().UnixNano(), p.n)
}

// copyDir copies a staging directory from one instance to the other, as
// tower does through Provider.Copy.
func copyDir(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-a", from, to).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v: %s", err, out)
	}
	_ = os.RemoveAll(from)
}

// pack packs the box, as tower does: not shared, against the boost's
// manifest, taken on the boost and copied to the box; shared, against the
// box's manifest last sent. It returns the staging directory on the boost
// and the manifest, failing the test unless the pack exits 0.
func (p *pair) pack(t *testing.T, shared bool, args ...string) (string, guestbin.Manifest) {
	t.Helper()
	staging, m, code, stderr := p.tryPack(t, shared, args...)
	if code != 0 {
		t.Fatalf("boost-pack exited %d: %s", code, stderr)
	}
	return staging, m
}

func (p *pair) tryPack(t *testing.T, shared bool, args ...string) (string, guestbin.Manifest, int, string) {
	t.Helper()
	id := "sync-" + p.id()
	out := filepath.Join(p.boostDir(p.box), id)
	pack := []string{"boost-pack", "--dir", p.box, "--out", out}
	if shared {
		pack = append(pack, "--shared")
	} else {
		mine := filepath.Join(p.boostDir(p.boost), id, "boost.manifest")
		if code, _, stderr := boostRun("boost-manifest", "--dir", p.boost, "--out", mine); code != 0 {
			t.Fatalf("boost-manifest: %s", stderr)
		}
		copyDir(t, filepath.Dir(mine), out)
		pack = append(pack, "--boost", filepath.Join(out, "boost.manifest"))
	}
	if p.last != "" {
		pack = append(pack, "--since", p.last)
	}
	code, stdout, stderr := boostRun(append(pack, args...)...)
	var m guestbin.Manifest
	if code == 0 {
		if err := json.Unmarshal([]byte(stdout), &m); err != nil {
			t.Fatal(err)
		}
	}
	staging := filepath.Join(p.boostDir(p.boost), id)
	_ = os.RemoveAll(staging)
	if code == 0 {
		copyDir(t, out, staging)
	}
	return staging, m, code, stderr
}

func (p *pair) apply(t *testing.T, staging string, shared bool) (guestbin.Applied, int, string) {
	t.Helper()
	args := []string{"boost-apply", "--dir", p.boost, "--staging", staging}
	if shared {
		args = append(args, "--shared")
	}
	code, out, stderr := boostRun(args...)
	var a guestbin.Applied
	if code == 0 {
		if err := json.Unmarshal([]byte(out), &a); err != nil {
			t.Fatal(err)
		}
	}
	if exists(staging) {
		t.Errorf("boost-apply left its staging directory")
	}
	return a, code, stderr
}

// send packs and applies, as a sync does, failing the test unless both
// exit 0; the box's manifest sent is its last.
func (p *pair) send(t *testing.T, shared bool, args ...string) (guestbin.Manifest, guestbin.Applied) {
	t.Helper()
	staging, m := p.pack(t, shared, args...)
	a, code, stderr := p.apply(t, staging, shared)
	if code != 0 {
		t.Fatalf("boost-apply exited %d: %s", code, stderr)
	}
	p.last = m.Sent
	return m, a
}

// job runs line under boost-job on the boost and returns its report's
// path and the report.
func (p *pair) job(t *testing.T, line string) (string, guestbin.Report) {
	t.Helper()
	report := filepath.Join(p.boostDir(p.boost), "job-"+p.id()+".json")
	code, _, stderr := boostRun("boost-job", "--dir", p.boost, "--report", report, "--cwd", p.boost, "--", "sh", "-c", line)
	if code != 0 {
		t.Fatalf("boost-job exited %d: %s", code, stderr)
	}
	var r guestbin.Report
	if err := json.Unmarshal([]byte(read(t, report)), &r); err != nil {
		t.Fatal(err)
	}
	return report, r
}

// back brings a job's changes back to the box, as a settle does: packed on
// the boost, copied, written back in the box against the box as the job's
// sync sent it.
func (p *pair) back(t *testing.T, report, sent string, args ...string) (guestbin.Back, guestbin.WroteBack) {
	t.Helper()
	id := "sync-back-" + p.id()
	out := filepath.Join(p.boostDir(p.boost), id)
	code, stdout, stderr := boostRun(append([]string{"boost-back", "--dir", p.boost, "--report", report, "--out", out}, args...)...)
	if code != 0 {
		t.Fatalf("boost-back exited %d: %s", code, stderr)
	}
	var b guestbin.Back
	if err := json.Unmarshal([]byte(stdout), &b); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(p.boostDir(p.box), id)
	copyDir(t, out, staging)
	wb := []string{"boost-writeback", "--dir", p.box, "--staging", staging, "--sent", sent}
	if p.last != "" {
		wb = append(wb, "--last", p.last)
	}
	code, stdout, stderr = boostRun(wb...)
	if code != 0 {
		t.Fatalf("boost-writeback exited %d: %s", code, stderr)
	}
	var w guestbin.WroteBack
	if err := json.Unmarshal([]byte(stdout), &w); err != nil {
		t.Fatal(err)
	}
	if exists(staging) {
		t.Error("boost-writeback left its staging directory")
	}
	return b, w
}

func mode(t *testing.T, file string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func mtime(t *testing.T, file string) time.Time {
	t.Helper()
	info, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func changedOf(r guestbin.Report) map[string]string {
	out := map[string]string{}
	for _, c := range r.Changed {
		out[c.Path] = c.Status
	}
	return out
}

// TestTheBoxsFilesReachTheBoost: what differs between the box's files and
// the boost's reaches the boost, ignored files too: a change, a new file,
// a deletion, an executable, a symlink, a dependency the box updated;
// never .git or .ws-cache. Their mtimes are kept, so the next send sends
// nothing.
func TestTheBoxsFilesReachTheBoost(t *testing.T) {
	p := newPair(t)
	write(t, filepath.Join(p.box, "a.txt"), "a, edited\n", 0o644)
	write(t, filepath.Join(p.box, "new.txt"), "untracked\n", 0o644)
	write(t, filepath.Join(p.box, "bin", "app"), "#!/bin/sh\necho app\n", 0o755)
	write(t, filepath.Join(p.box, "node_modules", "dep", "index.js"), "v2\n", 0o644)
	write(t, filepath.Join(p.box, "build.log"), "ignored\n", 0o644)
	write(t, filepath.Join(p.box, ".ws-cache", "go", "mod"), "the box's cache\n", 0o644)
	if err := os.Remove(filepath.Join(p.box, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/python3", filepath.Join(p.box, "python")); err != nil {
		t.Fatal(err)
	}
	// Its mode alone changed.
	if err := os.Chmod(filepath.Join(p.box, "d", "c.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	m, a := p.send(t, false)
	if m.Files != 6 || m.Deleted != 1 || a.Files != 6 || a.Deleted != 1 || len(a.Mismatch) != 0 {
		t.Errorf("sent %+v, applied %+v; want 6 files (a.txt, new.txt, bin/app, node_modules' index.js, build.log, d/c.txt's mode; the link is no file) and b.txt deleted", m, a)
	}
	if mode(t, filepath.Join(p.boost, "d", "c.txt")).Perm() != 0o755 {
		t.Errorf("d/c.txt, made executable in the box: %v on the boost", mode(t, filepath.Join(p.boost, "d", "c.txt")))
	}
	for f, want := range map[string]string{"a.txt": "a, edited\n", "new.txt": "untracked\n", "bin/app": "#!/bin/sh\necho app\n",
		"node_modules/dep/index.js": "v2\n", "build.log": "ignored\n"} {
		if got := read(t, filepath.Join(p.boost, f)); got != want {
			t.Errorf("%s on the boost: %q, want %q", f, got, want)
		}
		if got, want := mtime(t, filepath.Join(p.boost, f)), mtime(t, filepath.Join(p.box, f)); !got.Equal(want) {
			t.Errorf("%s on the boost has mtime %v, the box's %v", f, got, want)
		}
	}
	if mode(t, filepath.Join(p.boost, "bin", "app")).Perm() != 0o755 {
		t.Errorf("bin/app on the boost: %v, want executable", mode(t, filepath.Join(p.boost, "bin", "app")))
	}
	if exists(filepath.Join(p.boost, "b.txt")) {
		t.Error("b.txt the box deleted is on the boost")
	}
	if l, err := os.Readlink(filepath.Join(p.boost, "python")); err != nil || l != "/usr/bin/python3" {
		t.Errorf("a link out of /workspace on the boost: %q, %v; want it verbatim", l, err)
	}
	if exists(filepath.Join(p.boost, ".ws-cache", "go", "mod")) {
		t.Error(".ws-cache reached the boost")
	}
	if status, box := git(t, p.boost, "status", "--porcelain"), git(t, p.box, "status", "--porcelain"); status != box {
		t.Errorf("git status on the boost:\n%s\nin the box:\n%s", status, box)
	}
	m, _ = p.send(t, false)
	if m.Files != 0 || m.Deleted != 0 {
		t.Errorf("a second send with nothing changed: %+v", m)
	}
}

// TestTheBoostIsMadeTheBoxs: what only the boost changed, a file it added
// or changed, ignored or not, is undone by a send no job runs beside; a
// shared send sends the box's changes since its last alone, and leaves
// what the boost wrote.
func TestTheBoostIsMadeTheBoxs(t *testing.T) {
	p := newPair(t)
	p.send(t, false)
	write(t, filepath.Join(p.boost, "a.txt"), "the boost's\n", 0o644)
	write(t, filepath.Join(p.boost, "gen.go"), "package gen\n", 0o644)
	write(t, filepath.Join(p.boost, "out.log"), "the boost's log\n", 0o644)
	write(t, filepath.Join(p.box, "b.txt"), "the box's b\n", 0o644)

	m, _ := p.send(t, true)
	if m.Files != 1 || m.Deleted != 0 || !m.Shared {
		t.Errorf("a shared send: %+v, want b.txt alone", m)
	}
	if read(t, filepath.Join(p.boost, "a.txt")) != "the boost's\n" || !exists(filepath.Join(p.boost, "gen.go")) {
		t.Error("a shared send undid what a running job wrote")
	}
	if read(t, filepath.Join(p.boost, "b.txt")) != "the box's b\n" {
		t.Error("a shared send did not send the box's change")
	}

	m, _ = p.send(t, false)
	if m.Files != 1 || m.Deleted != 2 {
		t.Errorf("a send no job runs beside: %+v, want a.txt sent, gen.go and out.log deleted", m)
	}
	if read(t, filepath.Join(p.boost, "a.txt")) != "a\n" || exists(filepath.Join(p.boost, "gen.go")) || exists(filepath.Join(p.boost, "out.log")) {
		t.Error("what the boost alone changed is still there")
	}
}

// TestASharedSendNeedsTheLast: a shared send is of what the box changed
// since its last; without it, it is refused with its code.
func TestASharedSendNeedsTheLast(t *testing.T) {
	p := newPair(t)
	if _, _, code, _ := p.tryPack(t, true); code != guestbin.ExitNoSince {
		t.Errorf("a shared send with no last: exit %d, want %d", code, guestbin.ExitNoSince)
	}
	p.last = "sent-gone"
	if _, _, code, _ := p.tryPack(t, true); code != guestbin.ExitNoSince {
		t.Errorf("a shared send whose last is not kept: exit %d, want %d", code, guestbin.ExitNoSince)
	}
}

// TestAJobsChangesComeBack: what a job adds, changes or deletes, ignored
// files too, reaches the box with its mode and mtime: an executable built,
// a generated tracked file, a deletion, a link; .ws-cache stays the
// boost's. The next send sends nothing back.
func TestAJobsChangesComeBack(t *testing.T) {
	p := newPair(t)
	m, _ := p.send(t, false)
	report, r := p.job(t, `mkdir -p bin && printf '#!/bin/sh\necho built\n' > bin/app && chmod +x bin/app &&
		echo generated > a.txt && rm b.txt && ln -s ../a.txt d/link && mkdir -p .ws-cache && echo c > .ws-cache/x &&
		rm -r node_modules/dep && echo z > build.log`)
	got := changedOf(r)
	want := map[string]string{"bin": "A", "bin/app": "A", "a.txt": "M", "b.txt": "D", "d/link": "A", "node_modules/dep": "D",
		"node_modules/dep/index.js": "D", "build.log": "A"}
	if r.Version != guestbin.ReportVersion || r.Total != len(want) || r.Error != "" {
		t.Errorf("the job's report: %+v, want %v", r, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("the report says %s %q, want %q", k, got[k], v)
		}
	}
	b, w := p.back(t, report, m.Sent)
	if b.Files != 3 || b.Deleted != 3 || w.Files != 3 || w.Deleted != 3 || len(w.Conflicts) != 0 {
		t.Errorf("brought back %+v, written %+v; want bin/app, a.txt, build.log and three deletions", b, w)
	}
	if read(t, filepath.Join(p.box, "bin", "app")) != "#!/bin/sh\necho built\n" || mode(t, filepath.Join(p.box, "bin", "app")).Perm()&0o111 == 0 {
		t.Errorf("bin/app in the box: %q, %v", read(t, filepath.Join(p.box, "bin", "app")), mode(t, filepath.Join(p.box, "bin", "app")))
	}
	if out, err := exec.Command(filepath.Join(p.box, "bin", "app")).Output(); err != nil || string(out) != "built\n" {
		t.Errorf("bin/app run in the box: %q, %v", out, err)
	}
	if read(t, filepath.Join(p.box, "a.txt")) != "generated\n" || exists(filepath.Join(p.box, "b.txt")) || exists(filepath.Join(p.box, "node_modules", "dep")) {
		t.Error("the job's change, or its deletions, did not reach the box")
	}
	if l, err := os.Readlink(filepath.Join(p.box, "d", "link")); err != nil || l != "../a.txt" {
		t.Errorf("d/link in the box: %q, %v", l, err)
	}
	if exists(filepath.Join(p.box, ".ws-cache", "x")) {
		t.Error(".ws-cache came back")
	}
	if got, want := mtime(t, filepath.Join(p.box, "a.txt")), mtime(t, filepath.Join(p.boost, "a.txt")); !got.Equal(want) {
		t.Errorf("a.txt brought back has mtime %v, the boost's %v", got, want)
	}
	// The box's last manifest holds what was written back: a shared send,
	// of what the box changed since, sends nothing.
	if m, _ := p.send(t, true); m.Files != 0 || m.Deleted != 0 {
		t.Errorf("a shared send after the write-back: %+v, want nothing", m)
	}
	m, _ = p.send(t, false)
	if m.Files != 0 || m.Deleted != 0 {
		t.Errorf("a send after the write-back: %+v, want nothing", m)
	}
	if !exists(filepath.Join(p.boost, ".ws-cache", "x")) {
		t.Error("the boost's .ws-cache went")
	}
}

// TestTheBoxWinsAConflict: a path the box changed after the job's sync
// began, or one under a directory the box made something else, stays the
// box's, and is listed; the rest comes back.
func TestTheBoxWinsAConflict(t *testing.T) {
	p := newPair(t)
	m, _ := p.send(t, false)
	report, _ := p.job(t, `echo job > a.txt && echo job > b.txt && echo job > d/c.txt && echo job > new.txt`)
	write(t, filepath.Join(p.box, "a.txt"), "the box's, meanwhile\n", 0o644)
	write(t, filepath.Join(p.box, "new.txt"), "the box's too\n", 0o644)
	if err := os.RemoveAll(filepath.Join(p.box, "d")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.box, "d"), "a file now\n", 0o644)
	_, w := p.back(t, report, m.Sent)
	slices.Sort(w.Conflicts)
	if !slices.Equal(w.Conflicts, []string{"a.txt", "d/c.txt", "new.txt"}) || w.Files != 1 {
		t.Errorf("written back %+v, want b.txt, and a.txt, d/c.txt and new.txt in conflict", w)
	}
	if read(t, filepath.Join(p.box, "a.txt")) != "the box's, meanwhile\n" || read(t, filepath.Join(p.box, "new.txt")) != "the box's too\n" ||
		read(t, filepath.Join(p.box, "d")) != "a file now\n" {
		t.Error("the box's change was overwritten")
	}
	if read(t, filepath.Join(p.box, "b.txt")) != "job\n" {
		t.Error("b.txt, in no conflict, did not come back")
	}
}

// TestNothingIsWrittenThroughALink: a write-back never goes through a
// link the box has where the job had a directory, in the checkout or out
// of it; a tar naming a path under a link makes a directory there rather
// than follow it.
func TestNothingIsWrittenThroughALink(t *testing.T) {
	p := newPair(t)
	outside := t.TempDir()
	m, _ := p.send(t, false)
	report, _ := p.job(t, `echo job > d/c.txt && echo job > d/new.txt`)
	if err := os.RemoveAll(filepath.Join(p.box, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(p.box, "d")); err != nil {
		t.Fatal(err)
	}
	_, w := p.back(t, report, m.Sent)
	if w.Files != 0 || len(w.Conflicts) != 2 {
		t.Errorf("written back %+v, want both under the link in conflict", w)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("written outside the checkout, through a link: %v", entries)
	}

	// A tar that names a link, then a path under it.
	staging := filepath.Join(p.boostDir(p.boost), "sync-crafted")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "esc", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777})
	_ = tw.WriteHeader(&tar.Header{Name: "esc/x", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("x\n"))
	_ = tw.Close()
	write(t, filepath.Join(staging, "files.tar"), buf.String(), 0o644)
	write(t, filepath.Join(staging, "files.deleted"), "", 0o644)
	write(t, filepath.Join(staging, "manifest.json"), `{"tree":"","sent":"sent-x"}`, 0o644)
	if _, code, stderr := p.apply(t, staging, true); code != 0 {
		t.Fatalf("boost-apply: %d %s", code, stderr)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("boost-apply wrote through a link: %v", entries)
	}
	if read(t, filepath.Join(p.boost, "esc", "x")) != "x\n" {
		t.Error("esc/x was not made in the checkout")
	}
}

// TestWhatIsPastTheCapStays: a file past what one move takes is not
// moved, either way, and is said; the move does not fail.
func TestWhatIsPastTheCapStays(t *testing.T) {
	p := newPair(t)
	m, _ := p.send(t, false)
	write(t, filepath.Join(p.box, "big.bin"), strings.Repeat("x", 4096), 0o644)
	write(t, filepath.Join(p.box, "small.txt"), "small\n", 0o644)
	m2, a := p.send(t, false, "--max", "1024")
	if len(m2.Over) != 1 || m2.Over[0] != (guestbin.OverCap{Path: "big.bin", Bytes: 4096}) || m2.Files != 1 || len(a.Mismatch) != 0 {
		t.Errorf("a send past its cap: %+v, applied %+v; want small.txt sent, big.bin over", m2, a)
	}
	if exists(filepath.Join(p.boost, "big.bin")) || !exists(filepath.Join(p.boost, "small.txt")) {
		t.Error("the cap did not hold")
	}
	report, _ := p.job(t, `head -c 4096 /dev/zero > out.bin && echo r > r.txt`)
	b, w := p.back(t, report, m2.Sent, "--max", "1024")
	if len(b.Over) != 1 || b.Over[0].Path != "out.bin" || b.Files != 1 || w.Files != 1 {
		t.Errorf("brought back %+v, %+v; want r.txt, out.bin over", b, w)
	}
	if exists(filepath.Join(p.box, "out.bin")) || read(t, filepath.Join(p.box, "r.txt")) != "r\n" {
		t.Error("the cap back did not hold")
	}
	_ = m
}

// TestContentTheRecordsMissIsSentAgain: a tracked file whose content on
// the boost is not the box's, though its size and mtime are, is found by
// the boost's check against the box's tree, and sent when asked.
func TestContentTheRecordsMissIsSentAgain(t *testing.T) {
	p := newPair(t)
	p.send(t, false)
	f := filepath.Join(p.boost, "a.txt")
	at := mtime(t, f)
	write(t, f, "B\n", 0o644)
	if err := os.Chtimes(f, at, at); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(p.box, "b.txt"), "b, edited\n", 0o644)
	_, a := p.send(t, false)
	if !slices.Equal(a.Mismatch, []string{"a.txt"}) {
		t.Fatalf("the boost's check: %+v, want a.txt", a)
	}
	_, a = p.send(t, false, "--force", "a.txt")
	if len(a.Mismatch) != 0 || read(t, f) != "a\n" {
		t.Errorf("sent again: %+v, a.txt %q", a, read(t, f))
	}
}

// TestAChangeThatKeepsItsMtimeIsSent: a file the box rewrote to its size
// and mtime since its last send is sent: its inode or ctime moved.
func TestAChangeThatKeepsItsMtimeIsSent(t *testing.T) {
	p := newPair(t)
	p.send(t, false)
	f := filepath.Join(p.box, "node_modules", "dep", "index.js")
	at := mtime(t, f)
	write(t, f, "v9\n", 0o644)
	if err := os.Chtimes(f, at, at); err != nil {
		t.Fatal(err)
	}
	m, _ := p.send(t, false)
	if m.Files != 1 || read(t, filepath.Join(p.boost, "node_modules", "dep", "index.js")) != "v9\n" {
		t.Errorf("a change that kept its size and mtime: %+v", m)
	}
}

// TestGitChangesOnTheBoostAreSaid: a job's commit, or its git add, moves
// the boost's HEAD or index, which never come back: the report says so.
func TestGitChangesOnTheBoostAreSaid(t *testing.T) {
	p := newPair(t)
	p.send(t, false)
	_, r := p.job(t, `echo x > a.txt && git -c user.name=t -c user.email=t@example.com commit -q -am job`)
	if !r.HeadMoved || !r.IndexChanged {
		t.Errorf("a job that committed: %+v, want HEAD moved and the index changed", r)
	}
	_, r = p.job(t, `echo y > b.txt && git add b.txt`)
	if r.HeadMoved || !r.IndexChanged {
		t.Errorf("a job that staged: %+v, want the index changed alone", r)
	}
	_, r = p.job(t, `echo z > b.txt`)
	if r.HeadMoved || r.IndexChanged {
		t.Errorf("a job that left git alone: %+v", r)
	}
}

// TestAJobExitsAsItsCommandDid: boost-job exits as its command did, by a
// signal too, and its report is written either way.
func TestAJobExitsAsItsCommandDid(t *testing.T) {
	p := newPair(t)
	report := filepath.Join(p.boostDir(p.boost), "job-exit.json")
	if code, _, _ := boostRun("boost-job", "--dir", p.boost, "--report", report, "--cwd", p.boost, "--", "sh", "-c", "echo x > x.txt; exit 3"); code != 3 {
		t.Errorf("exit %d, want 3", code)
	}
	var r guestbin.Report
	if err := json.Unmarshal([]byte(read(t, report)), &r); err != nil || changedOf(r)["x.txt"] != "A" {
		t.Errorf("the report of a command that failed: %+v, %v", r, err)
	}
	if code, _, _ := boostRun("boost-job", "--dir", p.boost, "--", "sh", "-c", "kill -TERM $$"); code != 128+15 {
		t.Errorf("a command a signal ended: exit %d, want 143", code)
	}
}

// TestTwoJobsAtOnceKeepEachOthersWrites: a job's sync beside another job
// leaves what that one writes; each one's changes come back.
func TestTwoJobsAtOnceKeepEachOthersWrites(t *testing.T) {
	p := newPair(t)
	m1, _ := p.send(t, false)
	// The first job is running: what it wrote so far.
	write(t, filepath.Join(p.boost, "one.out"), "one\n", 0o644)
	write(t, filepath.Join(p.box, "a.txt"), "the box's, for two\n", 0o644)
	m2, _ := p.send(t, true)
	if !exists(filepath.Join(p.boost, "one.out")) || read(t, filepath.Join(p.boost, "a.txt")) != "the box's, for two\n" {
		t.Fatal("the shared send undid the first job's write, or did not send the box's")
	}
	report2, _ := p.job(t, `echo two > two.out`)
	_, w2 := p.back(t, report2, m2.Sent)
	if w2.Files != 1 || read(t, filepath.Join(p.box, "two.out")) != "two\n" {
		t.Errorf("the second job's write-back: %+v", w2)
	}
	_ = m1
}

// TestLocalCommitsReachTheBoost: the box's HEAD, a commit its remote refs
// do not reach, is the boost's HEAD, on the same branch.
func TestLocalCommitsReachTheBoost(t *testing.T) {
	p := newPair(t)
	git(t, p.box, "checkout", "-q", "-b", "topic")
	write(t, filepath.Join(p.box, "a.txt"), "committed\n", 0o644)
	git(t, p.box, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-am", "local")
	head := git(t, p.box, "rev-parse", "HEAD")
	m, _ := p.send(t, false)
	if !m.Bundle {
		t.Fatalf("%+v, want a bundle", m)
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
	staging, m := p.pack(t, false)
	if !m.Bundle {
		t.Fatal("no bundle for a local commit")
	}
	if _, code, stderr := p.apply(t, staging, false); code != guestbin.ExitHeadMissing {
		t.Fatalf("a bundle the boost lacks the prerequisites of: exit %d, want %d: %s", code, guestbin.ExitHeadMissing, stderr)
	}
	staging, _ = p.pack(t, false, "--bundle-all")
	if _, code, stderr := p.apply(t, staging, false); code != 0 {
		t.Fatalf("with HEAD's whole history: exit %d: %s", code, stderr)
	}
	if got, want := git(t, p.boost, "rev-parse", "HEAD"), git(t, p.box, "rev-parse", "HEAD"); got != want {
		t.Errorf("the boost's HEAD %s, the box's %s", got, want)
	}
}

// TestGitsStatePastTheCapIsRefused: HEAD's commits a sync bundles go
// whole or not at all: past the cap, the sync is refused.
func TestGitsStatePastTheCapIsRefused(t *testing.T) {
	p := newPair(t)
	write(t, filepath.Join(p.box, "big.bin"), rand.Text()+rand.Text()+rand.Text()+rand.Text()+rand.Text()+rand.Text()+rand.Text()+rand.Text(), 0o644)
	git(t, p.box, "add", "big.bin")
	git(t, p.box, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "big")
	git(t, p.box, "rm", "-q", "big.bin")
	git(t, p.box, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "no big")
	if _, _, code, stderr := p.tryPack(t, false, "--max", "200", "--bundle-all"); code != guestbin.ExitTooLarge {
		t.Errorf("a bundle past the cap: exit %d, %q", code, stderr)
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
	_, a := p.send(t, false)
	if !a.Staged {
		t.Errorf("%+v, want the box's index", a)
	}
	for _, args := range [][]string{{"status", "--porcelain"}, {"diff", "--cached", "--stat"}, {"diff", "--stat"}} {
		if box, boost := git(t, p.box, args...), git(t, p.boost, args...); box != boost {
			t.Errorf("git %s:\nin the box:\n%s\non the boost:\n%s", strings.Join(args, " "), box, boost)
		}
	}
}

// TestPathsAreNeverPatterns: names git would read as pathspec magic or
// globs move as the paths they are.
func TestPathsAreNeverPatterns(t *testing.T) {
	p := newPair(t)
	names := []string{":x", "a[1].txt", "a1.txt", "*"}
	for _, n := range names {
		write(t, filepath.Join(p.box, n), "box "+n+"\n", 0o644)
	}
	git(t, p.box, "add", "-A")
	git(t, p.box, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "odd names")
	if _, a := p.send(t, false); len(a.Mismatch) != 0 {
		t.Fatalf("%+v", a)
	}
	for _, n := range names {
		write(t, filepath.Join(p.boost, n), "job\n", 0o644)
	}
	p.send(t, false)
	for _, n := range names {
		if got := read(t, filepath.Join(p.boost, n)); got != "box "+n+"\n" {
			t.Errorf("%s on the boost: %q", n, got)
		}
	}
}

// TestSubmodulesKeepTheirOwnGit: a submodule's files move as any, its git
// state never: the boost's tree checks against the box's with its own
// gitlinks.
func TestSubmodulesKeepTheirOwnGit(t *testing.T) {
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
	p := &pair{origin: filepath.Join(root, "origin.git"), box: filepath.Join(root, "box"), boost: filepath.Join(root, "boost")}
	git(t, root, "clone", "-q", "--bare", seed, p.origin)
	git(t, root, "clone", "-q", p.origin, p.box)
	if out, err := exec.Command("cp", "-a", p.box, p.boost).CombinedOutput(); err != nil {
		t.Fatalf("cp: %v: %s", err, out)
	}
	git(t, p.box, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	write(t, filepath.Join(p.box, "a.txt"), "box\n", 0o644)
	m, a := p.send(t, false)
	if len(m.Links) != 1 || len(a.Mismatch) != 0 {
		t.Errorf("sent %+v, applied %+v", m, a)
	}
	if read(t, filepath.Join(p.boost, "sub", "s.txt")) != "in the submodule\n" || exists(filepath.Join(p.boost, "sub", ".git")) {
		t.Error("the submodule's files did not reach the boost, or its git file did")
	}
}

// TestATrackedFileAnIgnoreRuleMatchesIsChecked: the boost's check tracks
// what the box tracks, a file an ignore rule matches too.
func TestATrackedFileAnIgnoreRuleMatchesIsChecked(t *testing.T) {
	p := newPair(t)
	write(t, filepath.Join(p.box, ".gitignore"), "*.log\n/.ws-cache/\n*.toml\n", 0o644)
	write(t, filepath.Join(p.box, ".devcontainer", "mise.toml"), "[tools]\n", 0o644)
	git(t, p.box, "add", ".gitignore")
	git(t, p.box, "add", "-f", ".devcontainer/mise.toml")
	write(t, filepath.Join(p.box, "local.toml"), "ignored, untracked\n", 0o644)
	m, a := p.send(t, false)
	if len(a.Mismatch) != 0 {
		t.Errorf("sent %+v, applied %+v", m, a)
	}
	if git(t, p.boost, "status", "--porcelain") != git(t, p.box, "status", "--porcelain") {
		t.Error("git status differs")
	}
}

// TestBoostLeftoversAreSwept: boost-clean removes what it is named, the
// sent manifests it is not told to keep, and every subcommand sweeps what
// a crash left past a day.
func TestBoostLeftoversAreSwept(t *testing.T) {
	p := newPair(t)
	m1, _ := p.send(t, false)
	m2, _ := p.send(t, false)
	dir := p.boostDir(p.box)
	old, fresh := filepath.Join(dir, "sync-old"), filepath.Join(dir, "job-fresh.json")
	write(t, filepath.Join(old, "files.tar"), "x", 0o644)
	write(t, fresh, "{}", 0o644)
	day := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(old, day, day); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := boostRun("boost-clean", "--dir", p.box, "--prune-sent", "--keep", m2.Sent); code != 0 {
		t.Fatal(stderr)
	}
	if exists(old) || !exists(fresh) {
		t.Error("the sweep took the fresh report, or left the old staging")
	}
	if exists(filepath.Join(dir, m1.Sent+".manifest")) || !exists(filepath.Join(dir, m2.Sent+".manifest")) {
		t.Error("prune-sent did not keep the last sent alone")
	}
	if code, _, _ := boostRun("boost-clean", "--dir", p.box, "--name", "../../a.txt"); code == 0 {
		t.Error("boost-clean removed a name outside its directory")
	}
}

// TestADirectoryTheBoxAddedToStays: a directory the job removed, which
// the box put a file in since, stays with the box's file.
func TestADirectoryTheBoxAddedToStays(t *testing.T) {
	p := newPair(t)
	m, _ := p.send(t, false)
	report, _ := p.job(t, `rm -r d`)
	write(t, filepath.Join(p.box, "d", "mine.txt"), "the box's\n", 0o644)
	_, w := p.back(t, report, m.Sent)
	if read(t, filepath.Join(p.box, "d", "mine.txt")) != "the box's\n" || exists(filepath.Join(p.box, "d", "c.txt")) {
		t.Errorf("written back %+v: d/mine.txt %q, d/c.txt there %v", w, read(t, filepath.Join(p.box, "d", "mine.txt")), exists(filepath.Join(p.box, "d", "c.txt")))
	}
	if !slices.Contains(w.Conflicts, "d") {
		t.Errorf("conflicts %v, want d", w.Conflicts)
	}
}

// TestNothingOutsideIsDeleted: a path the box deleted, which on the boost
// is under a link out of the checkout, deletes nothing out there.
func TestNothingOutsideIsDeleted(t *testing.T) {
	p := newPair(t)
	p.send(t, false)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "c.txt"), "outside\n", 0o644)
	if err := os.RemoveAll(filepath.Join(p.boost, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(p.boost, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(p.box, "d", "c.txt")); err != nil {
		t.Fatal(err)
	}
	p.send(t, true)
	if read(t, filepath.Join(outside, "c.txt")) != "outside\n" {
		t.Error("a file outside the checkout was deleted through a link")
	}
}

// TestADeletionOfWhatTheBoxChangedStays: a file the job deleted, which
// the box changed after the job's sync began, stays the box's.
func TestADeletionOfWhatTheBoxChangedStays(t *testing.T) {
	p := newPair(t)
	m, _ := p.send(t, false)
	report, _ := p.job(t, `rm a.txt b.txt`)
	write(t, filepath.Join(p.box, "a.txt"), "the box's, meanwhile\n", 0o644)
	_, w := p.back(t, report, m.Sent)
	if read(t, filepath.Join(p.box, "a.txt")) != "the box's, meanwhile\n" || exists(filepath.Join(p.box, "b.txt")) {
		t.Errorf("written back %+v: a.txt %q, b.txt there %v", w, read(t, filepath.Join(p.box, "a.txt")), exists(filepath.Join(p.box, "b.txt")))
	}
	if !slices.Equal(w.Conflicts, []string{"a.txt"}) || w.Deleted != 1 {
		t.Errorf("written back %+v, want b.txt deleted, a.txt in conflict", w)
	}
}

// TestAFirstSyncSendsWhatTheBoxTouched: a file the box rewrote to the same
// size and mtime since its baseline, as npm's fixed mtimes do, reaches a
// boost the box never synced to; one untouched since is not sent.
func TestAFirstSyncSendsWhatTheBoxTouched(t *testing.T) {
	p := newPair(t)
	f := filepath.Join(p.box, "node_modules", "dep", "index.js")
	at := mtime(t, f)
	write(t, f, "v9\n", 0o644)
	if err := os.Chtimes(f, at, at); err != nil {
		t.Fatal(err)
	}
	m, _ := p.send(t, false)
	if m.Files != 1 || read(t, filepath.Join(p.boost, "node_modules", "dep", "index.js")) != "v9\n" {
		t.Errorf("a first sync after the box rewrote index.js to its size and mtime: %+v, the boost's %q", m,
			read(t, filepath.Join(p.boost, "node_modules", "dep", "index.js")))
	}
}

// TestABoxWithoutABaselineSendsAllItHas: a box without a baseline, made
// before tower took one, sends every ignored file to a boost it never
// synced to; those of its git tree are checked against it there.
func TestABoxWithoutABaselineSendsAllItHas(t *testing.T) {
	p := newPair(t)
	if err := os.Remove(filepath.Join(p.boostDir(p.box), "baseline.manifest")); err != nil {
		t.Fatal(err)
	}
	m, a := p.send(t, false)
	// node_modules' index.js, ignored; not a.txt, b.txt, d/c.txt, run.sh,
	// .gitignore, whose content the boost's check finds the box's.
	if m.Files != 1 || len(a.Mismatch) != 0 {
		t.Errorf("a first sync without a baseline: %+v, %+v; want every ignored file", m, a)
	}
	if m, _ := p.send(t, false); m.Files != 0 {
		t.Errorf("the next: %+v, want nothing", m)
	}
}

// TestAWriteBackKeepsTheBoxsEditThatKeptItsRecords: a box edit during the
// job that kept its size and mtime is not overwritten by the job's.
func TestAWriteBackKeepsTheBoxsEditThatKeptItsRecords(t *testing.T) {
	p := newPair(t)
	m, _ := p.send(t, false)
	report, _ := p.job(t, `echo J > a.txt`)
	f := filepath.Join(p.box, "a.txt")
	at := mtime(t, f)
	write(t, f, "B\n", 0o644)
	if err := os.Chtimes(f, at, at); err != nil {
		t.Fatal(err)
	}
	_, w := p.back(t, report, m.Sent)
	if read(t, f) != "B\n" || !slices.Equal(w.Conflicts, []string{"a.txt"}) {
		t.Errorf("written back %+v: a.txt %q, want the box's, in conflict", w, read(t, f))
	}
}

// TestNoPathOutsideTheSyncIsWrittenBack: changes naming a path in .git, or
// one climbing out, are refused, on the boost and in the box.
func TestNoPathOutsideTheSyncIsWrittenBack(t *testing.T) {
	p := newPair(t)
	m, _ := p.send(t, false)
	write(t, filepath.Join(p.boost, ".git", "hooks", "x"), "#!/bin/sh\n", 0o755)
	for _, bad := range []string{".git/hooks/x", "../x", "sub/.git/config"} {
		report := filepath.Join(p.boostDir(p.boost), "job-bad.json")
		write(t, report, "{}", 0o644)
		write(t, strings.TrimSuffix(report, ".json")+".changes", "A\x00"+bad+"\x00", 0o644)
		out := filepath.Join(p.boostDir(p.boost), "sync-back-bad")
		if code, _, _ := boostRun("boost-back", "--dir", p.boost, "--report", report, "--out", out); code == 0 {
			t.Errorf("boost-back of changes naming %q exited 0", bad)
		}
		_ = os.RemoveAll(out)
		// A tar naming it, as a boost-back unchecked would pack it.
		staging := filepath.Join(p.boostDir(p.box), "sync-back-crafted")
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: bad, Typeflag: tar.TypeReg, Mode: 0o755, Size: 3})
		_, _ = tw.Write([]byte("x!\n"))
		_ = tw.Close()
		write(t, filepath.Join(staging, "files.tar"), buf.String(), 0o644)
		write(t, filepath.Join(staging, "files.deleted"), "", 0o644)
		if code, _, _ := boostRun("boost-writeback", "--dir", p.box, "--staging", staging, "--sent", m.Sent); code == 0 {
			t.Errorf("boost-writeback of a tar naming %q exited 0", bad)
		}
	}
	if exists(filepath.Join(p.box, ".git", "hooks", "x")) || exists(filepath.Join(filepath.Dir(p.box), "x")) || exists(filepath.Join(p.box, "sub", ".git")) {
		t.Error("a path outside the sync was written in the box")
	}
}

// TestWhatAnotherJobWroteBackIsNotMovedAgain: a path two jobs at once both
// changed, written back by the first, is the box's already for the
// second: neither moved by it nor a conflict.
func TestWhatAnotherJobWroteBackIsNotMovedAgain(t *testing.T) {
	p := newPair(t)
	m, _ := p.send(t, false)
	// The second job's walk sees the first's write, made while it ran.
	report2, _ := p.job(t, `echo one > shared.out && echo two > two.out`)
	report1 := filepath.Join(p.boostDir(p.boost), "job-first.json")
	write(t, report1, "{}", 0o644)
	write(t, strings.TrimSuffix(report1, ".json")+".changes", "A\x00shared.out\x00", 0o644)
	if _, w1 := p.back(t, report1, m.Sent); w1.Files != 1 {
		t.Fatalf("the first's write-back: %+v", w1)
	}
	_, w2 := p.back(t, report2, m.Sent)
	if w2.Files != 1 || len(w2.Conflicts) != 0 || !slices.Equal(w2.Unchanged, []string{"shared.out"}) {
		t.Errorf("the second's write-back: %+v, want two.out written, shared.out unchanged, no conflict", w2)
	}
}

// TestTowerGuestRunsEveryBoostSubcommand: tower-guest itself, as tower
// runs it, takes each boost subcommand tower runs, not only those the
// in-process tests call through Boost.
func TestTowerGuestRunsEveryBoostSubcommand(t *testing.T) {
	p := newPair(t)
	out := filepath.Join(t.TempDir(), "m")
	if code := guestbin.Main([]string{"boost-manifest", "--dir", p.box, "--out", out}); code != 0 || !exists(out) {
		t.Errorf("tower-guest boost-manifest: exit %d", code)
	}
	if code := guestbin.Main([]string{"boost-baseline", "--dir", p.box}); code != 0 {
		t.Errorf("tower-guest boost-baseline: exit %d", code)
	}
	// Missing their arguments, each is refused by itself, as a command
	// it has, not as one it has not (2).
	for _, c := range []string{"boost-pack", "boost-apply", "boost-back", "boost-writeback", "boost-clean"} {
		if code := guestbin.Main([]string{c, "--dir", p.box}); code == 2 {
			t.Errorf("tower-guest %s: exit 2, a command it has not", c)
		}
	}
	if code := guestbin.Main([]string{"boost-nope"}); code != 2 {
		t.Errorf("tower-guest boost-nope: exit %d, want 2", code)
	}
}

// TestASyncsOwnTempFilesNeverTravel: a file a sync was writing beside its
// path, left by a crash, is never walked, so never sent.
func TestASyncsOwnTempFilesNeverTravel(t *testing.T) {
	p := newPair(t)
	write(t, filepath.Join(p.box, "d", ".tower-boost-abc-c.txt"), "half written\n", 0o600)
	m, _ := p.send(t, false)
	if m.Files != 0 || exists(filepath.Join(p.boost, "d", ".tower-boost-abc-c.txt")) {
		t.Errorf("a sync's leftover was sent: %+v", m)
	}
}

// TestAFirstSyncLeavesTrackedFilesToTheCheck: on a first sync, a file of
// the box's git tree the box touched is not sent for being touched: one
// whose content is the same stays, one whose content differs under the
// same records is found by the boost's check, and sent again.
func TestAFirstSyncLeavesTrackedFilesToTheCheck(t *testing.T) {
	p := newPair(t)
	// Written whole beside it and renamed on, as an editor saves: a new
	// inode, which git's own stat check sees, as it may not a rewrite in
	// place within the second it last looked.
	rewrite := func(name, text string) {
		f := filepath.Join(p.box, name)
		at := mtime(t, f)
		tmp := f + ".new"
		write(t, tmp, text, mode(t, f).Perm())
		if err := os.Chtimes(tmp, at, at); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, f); err != nil {
			t.Fatal(err)
		}
	}
	rewrite("b.txt", "b\n") // the same content, touched
	rewrite("a.txt", "Z\n") // other content, the same size and mtime
	m, a := p.send(t, false)
	if m.Files != 0 || !slices.Equal(a.Mismatch, []string{"a.txt"}) {
		t.Fatalf("a first sync after touching tracked files: %+v, %+v; want none sent, a.txt found by the check", m, a)
	}
	m, a = p.send(t, false, "--force", "a.txt")
	if m.Files != 1 || len(a.Mismatch) != 0 || read(t, filepath.Join(p.boost, "a.txt")) != "Z\n" {
		t.Errorf("a.txt sent again: %+v, %+v, the boost's %q", m, a, read(t, filepath.Join(p.boost, "a.txt")))
	}
}
