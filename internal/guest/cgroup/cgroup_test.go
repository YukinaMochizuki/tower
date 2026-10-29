package cgroup

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A temporary directory stands in for cgroupfs here: the kernel's own
// behaviour (clone3 into the group, the OOM kill, rmdir of a group with
// its interface files) is not exercised by these tests.

func write(t *testing.T, dir, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEnsureParent(t *testing.T) {
	root := t.TempDir()
	for range 2 {
		// Made again, as a second supervisor does, it is no error.
		path, err := EnsureParent(root, 3<<30)
		if err != nil {
			t.Fatal(err)
		}
		if path != filepath.Join(root, Parent) {
			t.Errorf("path = %q", path)
		}
	}
	path := filepath.Join(root, Parent)
	if got := read(t, filepath.Join(path, "memory.max")); got != "3221225472" {
		t.Errorf("memory.max = %q, want 3221225472", got)
	}
	// A directory keeps the last write; cgroupfs adds each controller.
	if got := read(t, filepath.Join(path, "cgroup.subtree_control")); got != "+cpu" {
		t.Errorf("cgroup.subtree_control was last written %q", got)
	}
}

func TestEnsureParentWithoutALimitLeavesMemoryMax(t *testing.T) {
	root := t.TempDir()
	if _, err := EnsureParent(root, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, Parent, "memory.max")); !os.IsNotExist(err) {
		t.Errorf("memory.max written without a limit: %v", err)
	}
}

func TestEnsureParentFailsWithoutMemory(t *testing.T) {
	defer func(w func(string, string, string) error) { writeFile = w }(writeFile)
	var wrote []string
	writeFile = func(_, name, value string) error {
		wrote = append(wrote, name+"="+value)
		if value == "+memory" {
			return syscall.ENOENT
		}
		return nil
	}
	if _, err := EnsureParent(t.TempDir(), 1<<30); err == nil {
		t.Fatal("EnsureParent succeeded where memory cannot be enabled")
	}
	if strings.Join(wrote, " ") != "cgroup.subtree_control=+memory" {
		t.Errorf("wrote %v after memory failed", wrote)
	}
	if _, err := EnsureParent(filepath.Join(t.TempDir(), "absent"), 1<<30); err == nil {
		t.Fatal("EnsureParent under a root that is not there succeeded")
	}
}

func TestMakeWritesTheGroupsOOMGroup(t *testing.T) {
	root := t.TempDir()
	g, err := Make(root, "p1-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.dir.Close() }()
	path := filepath.Join(root, "p1-1")
	if got := read(t, filepath.Join(path, "memory.oom.group")); got != "1" {
		t.Errorf("memory.oom.group = %q, want 1", got)
	}
	if _, err := os.Stat(filepath.Join(path, "memory.max")); !os.IsNotExist(err) {
		t.Errorf("a process's cgroup has a memory.max of its own: %v", err)
	}
	if g.FD() <= 0 {
		t.Errorf("FD = %d", g.FD())
	}
}

func TestMakeFailsWhereNoGroupCanBeMade(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	if _, err := Make(root, "p1-1"); err == nil {
		t.Fatal("Make under a parent that is not there succeeded")
	}
	if _, err := Make(t.TempDir(), "p1-1"); err != nil {
		t.Fatal(err)
	}
}

func TestMakeRemovesTheGroupWhenAFileFails(t *testing.T) {
	root := t.TempDir()
	// An interface file the kernel does not offer fails its write.
	defer func(w func(string, string, string) error) { writeFile = w }(writeFile)
	writeFile = func(string, string, string) error { return syscall.ENOENT }
	if _, err := Make(root, "p1-1"); err == nil {
		t.Fatal("Make succeeded without memory.oom.group")
	}
	if _, err := os.Stat(filepath.Join(root, "p1-1")); !os.IsNotExist(err) {
		t.Errorf("the group was left: %v", err)
	}
}

func TestUsage(t *testing.T) {
	root := t.TempDir()
	g, err := Make(root, "p1-1")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "p1-1")
	write(t, path, "memory.peak", "3221225472\n")
	write(t, path, "memory.events", "low 0\nhigh 0\nmax 12\noom 1\noom_kill 1\noom_group_kill 1\n")
	write(t, path, "cpu.stat", "usage_usec 81234567\nuser_usec 70000000\nsystem_usec 11234567\n")
	write(t, path, "cpu.pressure", "some avg10=0.00 avg60=1.50 avg300=0.20 total=4500000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1000\n")
	u := g.Usage()
	if u.MemoryPeak == nil || *u.MemoryPeak != 3<<30 {
		t.Errorf("MemoryPeak = %v", u.MemoryPeak)
	}
	if u.OOMKilled == nil || !*u.OOMKilled {
		t.Errorf("OOMKilled = %v", u.OOMKilled)
	}
	if u.CPUUsage == nil || *u.CPUUsage != 81234567*time.Microsecond {
		t.Errorf("CPUUsage = %v", u.CPUUsage)
	}
	if u.CPUWait == nil || *u.CPUWait != 4500*time.Millisecond {
		t.Errorf("CPUWait = %v", u.CPUWait)
	}
}

func TestUsageLeavesWhatTheKernelDoesNotOfferUnset(t *testing.T) {
	root := t.TempDir()
	g, err := Make(root, "p1-1")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "p1-1")
	write(t, path, "memory.events", "oom 0\noom_kill 0\n")
	u := g.Usage()
	if u.MemoryPeak != nil || u.CPUUsage != nil || u.CPUWait != nil {
		t.Errorf("unset fields set: %+v", u)
	}
	if u.OOMKilled == nil || *u.OOMKilled {
		t.Errorf("OOMKilled = %v, want false", u.OOMKilled)
	}
}

func TestField(t *testing.T) {
	for _, tc := range []struct {
		name, text, line, key string
		want                  int64
		ok                    bool
	}{
		{"flat", "a 1\nb 2\n", "", "b", 2, true},
		{"flat absent", "a 1\n", "", "b", 0, false},
		{"flat prefix is no match", "oom_kill_x 3\noom_kill 4\n", "", "oom_kill", 4, true},
		{"flat garbage", "b x\n", "", "b", 0, false},
		{"flat negative", "b -1\n", "", "b", 0, false},
		{"nested", "some avg10=1 total=7\nfull total=9\n", "some", "total", 7, true},
		{"nested other line", "full total=9\n", "some", "total", 0, false},
		{"nested absent key", "some avg10=1\n", "some", "total", 0, false},
		{"empty", "", "", "b", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := field([]byte(tc.text), tc.line, tc.key)
			if got != tc.want || ok != tc.ok {
				t.Errorf("field = %d, %v; want %d, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// kernelMoves stands in for cgroupfs's moves: a pid written to a
// cgroup.procs leaves the one it was in, unless stuck holds it, and an
// empty group's cgroup.procs goes, as a removed group's files do.
func kernelMoves(t *testing.T, from string, stuck map[string]bool) *[]string {
	t.Helper()
	var moved []string
	orig := movePID
	t.Cleanup(func() { movePID = orig })
	movePID = func(to, pid string) error {
		if stuck[pid] {
			return syscall.EBUSY
		}
		procs := filepath.Join(from, "cgroup.procs")
		var left []string
		for _, p := range strings.Fields(read(t, procs)) {
			if p != pid {
				left = append(left, p)
			}
		}
		moved = append(moved, filepath.Base(to)+":"+pid)
		if len(left) == 0 {
			return os.Remove(procs)
		}
		return os.WriteFile(procs, []byte(strings.Join(left, "\n")), 0o644)
	}
	return &moved
}

func TestRemove(t *testing.T) {
	root := t.TempDir()
	g, err := Make(root, "p1-1")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "p1-1")
	// cgroupfs removes a group with its interface files; a directory
	// does not.
	if err := os.Remove(filepath.Join(path, "memory.oom.group")); err != nil {
		t.Fatal(err)
	}
	if err := g.Remove(filepath.Join(root, "workload")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the group is still there: %v", err)
	}
}

func TestRemoveMovesWhatIsLeftToTheSupervisorsGroup(t *testing.T) {
	root := t.TempDir()
	g, err := Make(root, "p1-1")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "p1-1")
	if err := os.Remove(filepath.Join(path, "memory.oom.group")); err != nil {
		t.Fatal(err)
	}
	write(t, path, "cgroup.procs", "123\n456\n")
	moved := kernelMoves(t, path, nil)
	if err := g.Remove(filepath.Join(root, "workload")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*moved, " "); got != "workload:123 workload:456" {
		t.Errorf("moved %s, want each left process moved once to the supervisor's group", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the group is still there: %v", err)
	}
}

func TestRemoveLeavesAGroupWhoseProcessWillNotMove(t *testing.T) {
	root := t.TempDir()
	g, err := Make(root, "p1-1")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "p1-1")
	if err := os.Remove(filepath.Join(path, "memory.oom.group")); err != nil {
		t.Fatal(err)
	}
	write(t, path, "cgroup.procs", "123\n456\n")
	moved := kernelMoves(t, path, map[string]bool{"456": true})
	if err := g.Remove(filepath.Join(root, "workload")); err == nil {
		t.Fatal("a group still holding a process was removed")
	}
	if got := strings.Join(*moved, " "); got != "workload:123" {
		t.Errorf("moved %s, want the one that moves", got)
	}
	if got := strings.TrimSpace(read(t, filepath.Join(path, "cgroup.procs"))); got != "456" {
		t.Errorf("the group holds %q, want the one that would not move", got)
	}
}

func TestSweep(t *testing.T) {
	parent, proc := t.TempDir(), t.TempDir()
	defer func(p string) { procDir = p }(procDir)
	procDir = proc
	cmdline := func(pid, line string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(proc, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(proc, pid), "cmdline", line)
	}
	cmdline("100", "/usr/local/bin/tower-guest\x00supervise\x00/var/lib/tower/processes/p1\x00")
	// Its supervisor's pid now runs another supervisor, or no program.
	cmdline("200", "/usr/local/bin/tower-guest\x00supervise\x00/var/lib/tower/processes/p9\x00")
	for _, name := range []string{"p1-100", "p2-200", "p3-300", "p4-400", "other", "p5-x"} {
		if err := os.Mkdir(filepath.Join(parent, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, parent, "memory.max", "1")
	write(t, filepath.Join(parent, "p4-400"), "cgroup.procs", "77\n")
	moved := kernelMoves(t, filepath.Join(parent, "p4-400"), nil)
	swept := Sweep(parent, filepath.Join(t.TempDir(), "workload"))
	if got := strings.Join(swept, " "); got != "p2-200 p3-300 p4-400" {
		t.Errorf("swept %s", got)
	}
	if got := strings.Join(*moved, " "); got != "workload:77" {
		t.Errorf("moved %s, want what ran on in the swept group", got)
	}
	entries, _ := os.ReadDir(parent)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if got := strings.Join(left, " "); got != "memory.max other p1-100 p5-x" {
		t.Errorf("left %s", got)
	}
}

func TestSelf(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
		ok               bool
	}{
		{"cgroup2", "0::/workload\n", "/sys/fs/cgroup/workload", true},
		{"root", "0::/\n", "/sys/fs/cgroup", true},
		{"hybrid", "12:memory:/x\n0::/workload\n", "/sys/fs/cgroup/workload", true},
		{"v1 only", "12:memory:/x\n", "", false},
		{"escapes", "0::/../x\n", "", false},
		{"relative", "0::workload\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selfFrom(Root, []byte(tc.text))
			if got != tc.want || (err == nil) != tc.ok {
				t.Errorf("selfFrom = %q, %v; want %q, ok %v", got, err, tc.want, tc.ok)
			}
		})
	}
}

func TestAvailable(t *testing.T) {
	if Available(t.TempDir()) {
		t.Error("a temporary directory is no cgroup2 mount")
	}
}
