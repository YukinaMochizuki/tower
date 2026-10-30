package guestbin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// its HOME and no capability, and what it writes is nobody's; so is every
// file of its process's directory, which a shim running as nobody reads
// (as an earlier tower's does), and its exit is read as ever. It needs
// root, not cgroups: TestRealCgroupAsAUser measures one.
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
	ownedBy(t, filepath.Join(root, id), nobody)
	entries, err := os.ReadDir(filepath.Join(root, id))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		ownedBy(t, filepath.Join(root, id, e.Name()), nobody)
	}
	for _, f := range []string{proclog.MetaFile, proclog.LockFile, proclog.PIDFile, proclog.ExitFile, "log-00000000000000000000"} {
		if !slices.Contains(names, f) {
			t.Errorf("the process's directory holds %v, no %s", names, f)
		}
	}
	// A shim as nobody, an earlier tower's, lists and reads it.
	listed := asNobody(t, cmd, root, Header{Op: OpList, Root: root})
	if !strings.Contains(listed, id) || !strings.Contains(listed, `"exit"`) {
		t.Errorf("a shim as nobody lists %s", listed)
	}
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
	root = filepath.Join(dir, "processes")
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
// as nobody, and returns what it wrote; root is made readable to nobody
// for it, as a container's processes' root is its user's.
func asNobody(t *testing.T, cmd, root string, h Header) string {
	t.Helper()
	if err := os.Chown(root, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	var in, out, errOut bytes.Buffer
	if err := WriteHeader(&in, h); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&in, FrameEOF, nil); err != nil {
		t.Fatal(err)
	}
	c := newNobodyCmd(cmd)
	c.Stdin, c.Stdout, c.Stderr = &in, &out, &errOut
	if err := c.Run(); err != nil {
		t.Fatalf("the shim as nobody: %v: %s", err, errOut.String())
	}
	_, _ = io.Copy(io.Discard, &errOut)
	return out.String()
}

// newNobodyCmd is the binary at cmd run as tower-guest shim, as nobody.
func newNobodyCmd(cmd string) *exec.Cmd {
	c := exec.Command(cmd, "shim")
	c.Env = append(os.Environ(), mainEnv+"=1")
	c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
	return c
}
