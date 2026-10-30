package guestbin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/guest/cgroup"
	"github.com/YukinaMochizuki/tower/internal/guest/proclog"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

const (
	// fileEnv is the file helperWho writes.
	fileEnv = "TOWER_GUESTBIN_TEST_FILE"
	// escapeEnv set to 1 has helperWho try to leave its cgroup.
	escapeEnv = "TOWER_GUESTBIN_TEST_ESCAPE"
)

// whoAmI is what helperWho says of itself.
type whoAmI struct {
	guest.Identity
	Cgroup string `json:"cgroup"`
	CapEff string `json:"capEff"`
	// Escaped are the cgroups it moved itself into, Refused why it could
	// not move itself into each other.
	Escaped []string `json:"escaped,omitempty"`
	Refused []string `json:"refused,omitempty"`
}

// who is helperWho: it writes fileEnv's file, tries to leave its cgroup
// with escapeEnv, and says who it runs as, in which cgroup, with which
// capabilities.
func who() int {
	w := whoAmI{Identity: self()}
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		w.Cgroup = strings.TrimSpace(string(b))
	}
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "CapEff:"); ok {
				w.CapEff = strings.TrimSpace(v)
			}
		}
	}
	if f := os.Getenv(fileEnv); f != "" {
		if err := os.WriteFile(f, []byte("mine\n"), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	if os.Getenv(escapeEnv) == "1" {
		pid := []byte(fmt.Sprint(os.Getpid()))
		for _, g := range []string{cgroup.Root, filepath.Join(cgroup.Root, cgroup.Parent)} {
			if err := os.WriteFile(filepath.Join(g, "cgroup.procs"), pid, 0); err != nil {
				w.Refused = append(w.Refused, err.Error())
			} else {
				w.Escaped = append(w.Escaped, g)
			}
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(w); err != nil {
		return 1
	}
	return 0
}

// TestIdentityIsWhoTheShimRunsAs: the identity op says the shim's uid,
// gid, groups and HOME.
func TestIdentityIsWhoTheShimRunsAs(t *testing.T) {
	var in, out, errOut bytes.Buffer
	if err := WriteHeader(&in, Header{Op: OpIdentity}); err != nil {
		t.Fatal(err)
	}
	if code := shim(&in, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var got guest.Identity
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	groups, _ := os.Getgroups()
	want := guest.Identity{UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), Home: os.Getenv("HOME")}
	for _, g := range groups {
		want.Groups = append(want.Groups, uint32(g))
	}
	if got.UID != want.UID || got.GID != want.GID || !slices.Equal(got.Groups, want.Groups) || got.Home != want.Home {
		t.Errorf("the shim says it runs as %+v, want %+v", got, want)
	}
}

// TestACommandDropsOnlyToAnother: a start naming no one, or the one the
// supervisor runs as, drops to no one; one naming another drops to it.
func TestACommandDropsOnlyToAnother(t *testing.T) {
	me := self()
	reordered := me
	reordered.Groups = slices.Clone(me.Groups)
	slices.Reverse(reordered.Groups)
	reordered.Home = "/elsewhere"
	other := me
	other.UID++
	fewer := me
	fewer.Groups = nil
	for what, tc := range map[string]struct {
		as   *guest.Identity
		drop bool
	}{
		"no one":                  {nil, false},
		"itself":                  {&me, false},
		"itself, groups shuffled": {&reordered, false},
		"another uid":             {&other, true},
		"other groups":            {&fewer, len(me.Groups) > 0},
	} {
		if got := dropTo(tc.as); (got != nil) != tc.drop {
			t.Errorf("%s: drops to %+v, want a drop %v", what, got, tc.drop)
		}
	}
}

// TestACommandHasTheHomeOfWhomItRunsAs: a command dropping to a user has
// that user's HOME unless its start sets one; one dropping to no one has
// the supervisor's.
func TestACommandHasTheHomeOfWhomItRunsAs(t *testing.T) {
	t.Setenv("HOME", "/root")
	vscode := &guest.Identity{UID: 1000, GID: 1000, Home: "/home/vscode"}
	home := func(env []string) string {
		h := ""
		for _, e := range env {
			if v, ok := strings.CutPrefix(e, "HOME="); ok {
				if h != "" {
					t.Errorf("HOME twice in %q", env)
				}
				h = v
			}
		}
		return h
	}
	for what, tc := range map[string]struct {
		env  map[string]string
		as   *guest.Identity
		want string
	}{
		"dropping":                {nil, vscode, "/home/vscode"},
		"dropping, HOME given":    {map[string]string{"HOME": "/srv"}, vscode, "/srv"},
		"to no one":               {nil, nil, "/root"},
		"to one with no HOME":     {nil, &guest.Identity{UID: 1000}, "/root"},
		"dropping, other env too": {map[string]string{"A": "1"}, vscode, "/home/vscode"},
	} {
		if got := home(commandEnv(tc.env, tc.as)); got != tc.want {
			t.Errorf("%s: HOME=%q, want %q", what, got, tc.want)
		}
	}
}

// TestACommandStartsAsItsUser starts a process as nobody, its supervisor
// root: the command runs as that uid, gid and those groups alone, with
// its HOME and no capability, and what it writes is nobody's. Its
// process's directory and its files are root's, readable to nobody, who
// writes none of them: a shim as nobody, as every operation but a start
// runs, lists it and reads its log. It needs root, not cgroups:
// TestRealCgroupAsAUser measures one.
func TestACommandStartsAsItsUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("not root")
	}
	nobody := guest.Identity{UID: 65534, GID: 65534, Groups: []uint32{65534, 4242}}
	root, work, cmd := asUserWorld(t, nobody)
	nobody.Home = work
	file := filepath.Join(work, "made")
	t.Setenv(mainEnv, "1")
	id, err := startSupervised(Header{Root: root, Command: []string{cmd}, Dir: work, LogCap: 1 << 20, Keep: 10, As: &nobody,
		Env: map[string]string{helperEnv: helperWho, fileEnv: file}})
	if err != nil {
		t.Fatal(err)
	}
	w, exit := waitWho(t, root, id)
	if exit.Code != 0 {
		t.Fatalf("the command ended %+v", exit)
	}
	if w.UID != nobody.UID || w.GID != nobody.GID || !sameGroups(w.Groups, nobody.Groups) {
		t.Errorf("the command ran as %d:%d %v, want %d:%d %v", w.UID, w.GID, w.Groups, nobody.UID, nobody.GID, nobody.Groups)
	}
	if w.Home != work {
		t.Errorf("the command's HOME is %q, want %q", w.Home, work)
	}
	if strings.Trim(w.CapEff, "0") != "" {
		t.Errorf("the command kept capabilities %s", w.CapEff)
	}
	ownedBy(t, file, nobody)
	rootUser := guest.Identity{}
	dir := filepath.Join(root, id)
	ownedBy(t, dir, rootUser)
	modeIs(t, dir, 0o755)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		ownedBy(t, filepath.Join(dir, e.Name()), rootUser)
		modeIs(t, filepath.Join(dir, e.Name()), 0o644)
	}
	for _, f := range []string{proclog.MetaFile, proclog.LockFile, proclog.PIDFile, proclog.ExitFile, "log-00000000000000000000"} {
		if !slices.Contains(names, f) {
			t.Errorf("the process's directory holds %v, no %s", names, f)
		}
	}
	// As the user: listed, its exit read past its lock, its log read.
	listed := asNobody(t, cmd, Header{Op: OpList, Root: root})
	if !strings.Contains(listed, id) || !strings.Contains(listed, `"exit"`) {
		t.Errorf("a shim as nobody lists %s", listed)
	}
	if logged := asNobody(t, cmd, Header{Op: OpLog, Root: root, ID: id}); !strings.Contains(logged, "chunk") {
		t.Errorf("a shim as nobody reads the log as %s", logged)
	}
}

// TestTheUserSignalsTheCommandAlone: a command started as nobody is in a
// process group of its own, its root supervisor in none of it; a shim as
// nobody signals the group, which the user may, and the supervisor, which
// no signal of the group reaches, writes how the command ended.
func TestTheUserSignalsTheCommandAlone(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("not root")
	}
	nobody := guest.Identity{UID: 65534, GID: 65534, Groups: []uint32{65534}}
	root, work, cmd := asUserWorld(t, nobody)
	nobody.Home = work
	t.Setenv(mainEnv, "1")
	id, err := startSupervised(Header{Root: root, Command: []string{cmd}, Dir: work, LogCap: 1 << 20, Keep: 10, As: &nobody,
		Env: map[string]string{helperEnv: helperSleep}})
	if err != nil {
		t.Fatal(err)
	}
	pid := pidIn(t, filepath.Join(root, id))
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	supervisor, group := statOf(t, pid)
	_, supervisorGroup := statOf(t, supervisor)
	if group != pid || supervisorGroup == group {
		t.Errorf("the command %d is in group %d, its supervisor %d in %d: want the command alone in its own", pid, group, supervisor, supervisorGroup)
	}
	asNobody(t, cmd, Header{Op: OpSignal, Root: root, ID: id, Signal: int(syscall.SIGKILL)})
	var exit *proclog.Exit
	testutil.EventuallyWithin(t, time.Minute, "the exit written", func() bool {
		in, err := proclog.Read(root, id)
		if err != nil {
			t.Fatal(err)
		}
		exit = in.Exit
		return exit != nil
	})
	if exit.Signal != int(syscall.SIGKILL) {
		t.Errorf("the command killed by its user ended %+v", exit)
	}
	if b, err := os.ReadFile(filepath.Join(root, id, proclog.ExitFile)); err != nil || !strings.Contains(string(b), `"signal":9`) {
		t.Errorf("the supervisor wrote no exit: %q, %v", b, err)
	}
}

// TestAUserRedirectsNoWriteOfTheSupervisor: the command, as nobody, who
// owns the processes' root as a container's user owns /.tower/processes,
// tries what would have its root supervisor write elsewhere: a symlink in
// its process's directory, which it may not make, and, while the
// supervisor runs, its directory renamed and another put in its place, a
// directory of its own of symlinks to a root file, or a symlink. The root
// file is never written, and the supervisor's writes, its log and exit,
// all reach the directory it made, wherever it went: its log's next
// segment, made after the swap (its cap is small), among them, and nothing
// is made in what the user put in its place.
func TestAUserRedirectsNoWriteOfTheSupervisor(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("not root")
	}
	for _, how := range []string{attackDir, attackLink} {
		t.Run(how, func(t *testing.T) {
			nobody := guest.Identity{UID: 65534, GID: 65534, Groups: []uint32{65534}}
			root, work, cmd := asUserWorld(t, nobody)
			nobody.Home = work
			target := filepath.Join(t.TempDir(), "root-only")
			if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(mainEnv, "1")
			id, err := startSupervised(Header{Root: root, Command: []string{cmd}, Dir: work, LogCap: 32, Keep: 10, As: &nobody,
				Env: map[string]string{helperEnv: helperAttack, attackEnv: how, rootEnv: root, targetEnv: target}})
			if err != nil {
				t.Fatal(err)
			}
			moved := filepath.Join(root, "moved")
			testutil.EventuallyWithin(t, time.Minute, "the process ended in the directory it was moved to", func() bool {
				_, err := os.Stat(filepath.Join(moved, proclog.ExitFile))
				return err == nil
			})
			if b, err := os.ReadFile(target); err != nil || string(b) != "untouched" {
				t.Errorf("the root file holds %q, %v: the supervisor wrote it", b, err)
			}
			if st, err := os.Stat(target); err != nil || st.Mode().Perm() != 0o600 {
				t.Errorf("the root file is %v, %v", st, err)
			}
			exit, err := os.ReadFile(filepath.Join(moved, proclog.ExitFile))
			if err != nil || !strings.Contains(string(exit), `"code":0`) {
				t.Errorf("the exit in the moved directory is %q, %v", exit, err)
			}
			var out strings.Builder
			for _, e := range must(os.ReadDir(moved)) {
				if strings.HasPrefix(e.Name(), "log-") {
					out.Write(must(os.ReadFile(filepath.Join(moved, e.Name()))))
				}
			}
			for _, want := range []string{"refused: " + plantName, "swapped"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("the log in the moved directory holds %q, no %q", out.String(), want)
				}
			}
			if _, err := os.Lstat(filepath.Join(root, id)); err != nil {
				t.Errorf("what the user put in the directory's place is gone: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(moved, plantName)); err == nil {
				t.Errorf("the user planted %s in the supervisor's directory", plantName)
			}
			// Nothing made where the user's directory or symlink leads.
			placed := filepath.Join(root, id)
			if how == attackLink {
				placed = filepath.Dir(target)
			}
			for _, e := range must(os.ReadDir(placed)) {
				if st, err := os.Lstat(filepath.Join(placed, e.Name())); err == nil && st.Mode()&os.ModeSymlink == 0 && e.Name() != filepath.Base(target) {
					t.Errorf("the supervisor made %s in what the user put in its directory's place", e.Name())
				}
			}
			segs := 0
			for _, e := range must(os.ReadDir(moved)) {
				if strings.HasPrefix(e.Name(), "log-") {
					segs++
				}
			}
			if segs < 2 {
				t.Errorf("the log has %d segments in the moved directory: want one made after the swap", segs)
			}
		})
	}
}

// TestAForgedPIDSignalsNoOneElse: a process's pid is root's, which nobody
// cannot rewrite; one in a directory nobody put in its place, naming a
// root process's group, signals none of it from a shim as nobody, as a
// signal is sent.
func TestAForgedPIDSignalsNoOneElse(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("not root")
	}
	nobody := guest.Identity{UID: 65534, GID: 65534, Groups: []uint32{65534}}
	root, work, cmd := asUserWorld(t, nobody)
	nobody.Home = work
	t.Setenv(mainEnv, "1")
	id, err := startSupervised(Header{Root: root, Command: []string{cmd}, Dir: work, LogCap: 1 << 20, Keep: 10, As: &nobody,
		Env: map[string]string{helperEnv: helperSleep}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, id)
	pid := pidIn(t, dir)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	if _, code := asNobodyCode(t, cmd, Header{Op: OpWrite, Path: filepath.Join(dir, proclog.PIDFile)}); code == 0 {
		t.Error("nobody rewrote the pid")
	}
	if b, err := os.ReadFile(filepath.Join(dir, proclog.PIDFile)); err != nil || string(b) != strconv.Itoa(pid) {
		t.Errorf("the pid is %q, %v; want %d", b, err, pid)
	}
	// A root process group, then the user's forgery naming it.
	victim := exec.Command("sleep", "60")
	victim.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-victim.Process.Pid, syscall.SIGKILL); _ = victim.Wait() })
	if err := os.Rename(dir, filepath.Join(root, "moved")); err != nil {
		t.Fatal(err)
	}
	forged := map[string]string{
		proclog.MetaFile: `{"id":"` + id + `","command":["x"]}`,
		proclog.PIDFile:  strconv.Itoa(victim.Process.Pid),
		proclog.LockFile: "",
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dir, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	for name, content := range forged {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(f, 65534, 65534); err != nil {
			t.Fatal(err)
		}
	}
	// Its lock held, so it reads as running, as the user would hold it.
	lock, err := os.Open(filepath.Join(dir, proclog.LockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	_, code := asNobodyCode(t, cmd, Header{Op: OpSignal, Root: root, ID: id, Signal: int(syscall.SIGKILL)})
	if code == 0 {
		t.Error("the forged signal succeeded")
	}
	if err := syscall.Kill(victim.Process.Pid, 0); err != nil {
		t.Errorf("the root process is gone: %v", err)
	}
}

// What helperAttack does, as attackEnv says: put a directory of its own in
// its process's directory's place, or a symlink.
const (
	attackEnv  = "TOWER_GUESTBIN_TEST_ATTACK"
	rootEnv    = "TOWER_GUESTBIN_TEST_ROOT"
	targetEnv  = "TOWER_GUESTBIN_TEST_TARGET"
	attackDir  = "dir"
	attackLink = "link"
	// plantName is the symlink it tries to make in its own directory.
	plantName = "exit.json.tmp"
)

// attack is helperAttack: in the processes' root, rootEnv, it tries a
// symlink to targetEnv in its process's directory, the only one there;
// then renames that directory to "moved" and puts in its place a
// directory of symlinks to targetEnv under the names the supervisor
// writes, or a symlink to targetEnv's directory; and says so, on its
// stdout, which its supervisor writes to its log.
func attack() int {
	root, target := os.Getenv(rootEnv), os.Getenv(targetEnv)
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		fmt.Fprintln(os.Stderr, "the processes' root holds", len(entries), err)
		return 1
	}
	dir := filepath.Join(root, entries[0].Name())
	if err := os.Symlink(target, filepath.Join(dir, plantName)); err != nil {
		fmt.Println("refused: " + plantName)
	} else {
		fmt.Println("planted: " + plantName)
	}
	if err := os.Rename(dir, filepath.Join(root, "moved")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch os.Getenv(attackEnv) {
	case attackLink:
		err = os.Symlink(filepath.Dir(target), dir)
	default:
		err = os.Mkdir(dir, 0o755)
		for _, name := range []string{proclog.MetaFile, proclog.ExitFile, proclog.ExitFile + ".tmp", proclog.MetaFile + ".tmp", proclog.PIDFile, proclog.LockFile, "log-00000000000000000000", "log-00000000000000000001"} {
			if err == nil {
				err = os.Symlink(target, filepath.Join(dir, name))
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Past the log's cap: its supervisor makes the next segment now.
	fmt.Println("swapped " + strings.Repeat("x", 64))
	return 0
}

// TestRealCgroupAsAUser runs a measured process as nobody, its supervisor
// root, against the real cgroup2 root: it runs as nobody in a cgroup of
// its own under /tower, cannot move itself out of it, and its exit has
// its usage. It runs where TestRealCgroup does, alike.
func TestRealCgroupAsAUser(t *testing.T) {
	if os.Getenv(realEnv) != "1" {
		t.Skipf("%s=1 is not set: this test makes cgroups under %s", realEnv, cgroup.Root)
	}
	if os.Geteuid() != 0 {
		t.Skip("not root")
	}
	if !cgroup.Available(cgroup.Root) || syscall.Access(cgroup.Root, 2 /* W_OK */) != nil {
		t.Skipf("%s is no writable cgroup2 mount", cgroup.Root)
	}
	nobody := guest.Identity{UID: 65534, GID: 65534, Groups: []uint32{65534}}
	root, work, cmd := asUserWorld(t, nobody)
	nobody.Home = work
	t.Setenv(mainEnv, "1")
	id, err := startSupervised(Header{Root: root, Command: []string{cmd}, Dir: work, LogCap: 1 << 20, Keep: 10, As: &nobody,
		Measure: true, MemoryMax: realLimit, Env: map[string]string{helperEnv: helperWho, escapeEnv: "1", fileEnv: filepath.Join(work, "made")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		entries, _ := os.ReadDir(filepath.Join(cgroup.Root, cgroup.Parent))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), id+"-") {
				_ = os.WriteFile(filepath.Join(cgroup.Root, cgroup.Parent, e.Name(), "cgroup.kill"), []byte("1"), 0o644)
			}
		}
	})
	w, exit := waitWho(t, root, id)
	t.Logf("the command said %+v and ended %+v", w, exit)
	if w.UID != nobody.UID || w.GID != nobody.GID || w.Home != work {
		t.Errorf("the command ran as %+v, want %+v", w.Identity, nobody)
	}
	if !strings.HasPrefix(w.Cgroup, "0::/"+cgroup.Parent+"/"+id+"-") {
		t.Errorf("the command ran in %q, want a cgroup of its own under /%s", w.Cgroup, cgroup.Parent)
	}
	if len(w.Escaped) > 0 || len(w.Refused) != 2 {
		t.Errorf("the command moved itself into %v (refused %v): want it held in its own", w.Escaped, w.Refused)
	}
	if exit.Usage == nil || exit.Usage.MemoryPeak == nil {
		t.Errorf("the command's exit has no usage: %+v", exit)
	}
	ownedBy(t, filepath.Join(work, "made"), nobody)
}

// asUserWorld makes, for a command run as id: a processes' root, a work
// directory id owns, and a copy of the test binary it may run, in a
// directory it may reach, which a test's own and the build's are not.
func asUserWorld(t *testing.T, id guest.Identity) (root, work, cmd string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "runas-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The processes' root is the user's, as installGuest makes
	// /.tower/processes the container's user's.
	root = filepath.Join(dir, "processes")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(root, int(id.UID), int(id.GID)); err != nil {
		t.Fatal(err)
	}
	work = filepath.Join(dir, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(work, int(id.UID), int(id.GID)); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	cmd = filepath.Join(dir, "guestbin.test")
	if err := os.WriteFile(cmd, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return root, work, cmd
}

// waitWho waits for a helperWho process to end and reads what it said.
func waitWho(t *testing.T, root, id string) (whoAmI, proclog.Exit) {
	t.Helper()
	var exit *proclog.Exit
	testutil.EventuallyWithin(t, time.Minute, "process "+id+" ended", func() bool {
		in, err := proclog.Read(root, id)
		if err != nil {
			t.Fatal(err)
		}
		exit = in.Exit
		return exit != nil
	})
	var out strings.Builder
	if _, err := proclog.ReadLog(context.Background(), root, id, 0, false, func(c proclog.Chunk) error {
		out.Write(c.Data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var w whoAmI
	if err := json.Unmarshal([]byte(out.String()), &w); err != nil {
		t.Fatalf("the command said %q: %v", out.String(), err)
	}
	return w, *exit
}

// ownedBy checks path is id's.
func ownedBy(t *testing.T, path string, id guest.Identity) {
	t.Helper()
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	s := st.Sys().(*syscall.Stat_t)
	if s.Uid != id.UID || s.Gid != id.GID {
		t.Errorf("%s is %d:%d's, want %d:%d's", path, s.Uid, s.Gid, id.UID, id.GID)
	}
}

// asNobody runs one operation of the binary at cmd as tower-guest shim,
// as nobody, and returns what it wrote, failing the test unless it passed.
func asNobody(t *testing.T, cmd string, h Header) string {
	t.Helper()
	out, code := asNobodyCode(t, cmd, h)
	if code != 0 {
		t.Fatalf("the shim as nobody exited %d", code)
	}
	return out
}

// asNobodyCode runs one operation of the binary at cmd as tower-guest
// shim, as nobody, and returns what it wrote and its exit code. Its stdin
// stays open until it exits, as a call's does while it lasts: a log's
// read ends with it.
func asNobodyCode(t *testing.T, cmd string, h Header) (string, int) {
	t.Helper()
	var in, out, errOut bytes.Buffer
	if err := WriteHeader(&in, h); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&in, FrameEOF, nil); err != nil {
		t.Fatal(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pw.Close() }()
	go func() { _, _ = pw.Write(in.Bytes()) }()
	c := newNobodyCmd(cmd)
	c.Stdin, c.Stdout, c.Stderr = pr, &out, &errOut
	err = c.Run()
	_ = pr.Close()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("the shim as nobody: %v: %s", err, errOut.String())
	}
	if err != nil {
		t.Logf("the shim as nobody: %v: %s", err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), c.ProcessState.ExitCode()
}

// pidIn reads the pid of a process's directory once it is written.
func pidIn(t *testing.T, dir string) int {
	t.Helper()
	var pid int
	testutil.EventuallyWithin(t, time.Minute, "the pid written", func() bool {
		b, err := os.ReadFile(filepath.Join(dir, proclog.PIDFile))
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(string(b))
		return err == nil
	})
	return pid
}

// statOf is a process's parent and process group.
func statOf(t *testing.T, pid int) (ppid, pgrp int) {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	// After the command's name, in parentheses, which may hold spaces.
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
	ppid, _ = strconv.Atoi(f[1])
	pgrp, _ = strconv.Atoi(f[2])
	return ppid, pgrp
}

// modeIs checks path's permissions are m.
func modeIs(t *testing.T, path string, m os.FileMode) {
	t.Helper()
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != m {
		t.Errorf("%s is %o, want %o", path, st.Mode().Perm(), m)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// newNobodyCmd is the binary at cmd run as tower-guest shim, as nobody.
func newNobodyCmd(cmd string) *exec.Cmd {
	c := exec.Command(cmd, "shim")
	c.Env = append(os.Environ(), mainEnv+"=1")
	c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	return c
}
