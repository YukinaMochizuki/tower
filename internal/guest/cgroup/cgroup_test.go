package cgroup

import (
	"os"
	"path/filepath"
	"strings"
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

func TestMakeWritesTheGroupsBounds(t *testing.T) {
	root := t.TempDir()
	g, err := Make(root, "tower-p1", 3<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.dir.Close() }()
	path := filepath.Join(root, "tower-p1")
	if got := read(t, filepath.Join(path, "memory.oom.group")); got != "1" {
		t.Errorf("memory.oom.group = %q, want 1", got)
	}
	if got := read(t, filepath.Join(path, "memory.max")); got != "3221225472" {
		t.Errorf("memory.max = %q, want 3221225472", got)
	}
	if g.FD() <= 0 {
		t.Errorf("FD = %d", g.FD())
	}
}

func TestMakeWithoutALimitLeavesMemoryMax(t *testing.T) {
	root := t.TempDir()
	g, err := Make(root, "tower-p1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.dir.Close() }()
	if _, err := os.Stat(filepath.Join(root, "tower-p1", "memory.max")); !os.IsNotExist(err) {
		t.Errorf("memory.max written without a limit: %v", err)
	}
}

func TestMakeFailsWhereNoGroupCanBeMade(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	if _, err := Make(root, "tower-p1", 1<<30); err == nil {
		t.Fatal("Make under a root that is not there succeeded")
	}
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(ro, 0o700) }()
	if os.Geteuid() != 0 {
		if _, err := Make(ro, "tower-p1", 1<<30); err == nil {
			t.Fatal("Make under a read-only root succeeded")
		}
	}
}

func TestMakeRemovesTheGroupWhenAFileFails(t *testing.T) {
	root := t.TempDir()
	// An interface file the kernel does not offer fails its write.
	defer func(w func(string, string, string) error) { writeFile = w }(writeFile)
	writeFile = func(_, name, _ string) error {
		if name == "memory.max" {
			return os.ErrNotExist
		}
		return nil
	}
	if _, err := Make(root, "tower-p1", 1<<30); err == nil {
		t.Fatal("Make succeeded without memory.max")
	}
	if _, err := os.Stat(filepath.Join(root, "tower-p1")); !os.IsNotExist(err) {
		t.Errorf("the group was left: %v", err)
	}
	if _, err := Make(root, "tower-p1", 0); err != nil {
		t.Fatalf("Make without a limit, which needs no memory.max: %v", err)
	}
}

func TestUsage(t *testing.T) {
	root := t.TempDir()
	g, err := Make(root, "tower-p1", 0)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tower-p1")
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
	g, err := Make(root, "tower-p1", 0)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tower-p1")
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

func TestRemove(t *testing.T) {
	root := t.TempDir()
	g, err := Make(root, "tower-p1", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tower-p1")
	// cgroupfs removes a group with its interface files; a directory
	// does not.
	for _, f := range []string{"memory.oom.group", "memory.max"} {
		if err := os.Remove(filepath.Join(path, f)); err != nil {
			t.Fatal(err)
		}
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
	g, err := Make(root, "tower-p1", 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tower-p1")
	to := filepath.Join(root, "workload")
	if err := os.Mkdir(to, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, to, "cgroup.procs", "")
	write(t, path, "cgroup.procs", "123\n456\n")
	// A directory's cgroup.procs does not empty as the kernel's does, so
	// the group stays, and its pids were moved each time it was read.
	if err := g.Remove(to); err == nil {
		t.Fatal("Remove of a group still holding files succeeded")
	}
	if got, want := read(t, filepath.Join(to, "cgroup.procs")), strings.Repeat("123456", moveTries); got != want {
		t.Errorf("moved %q, want %q", got, want)
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
