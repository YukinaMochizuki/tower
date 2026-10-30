package guestbin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest/cgroup"
	"github.com/YukinaMochizuki/tower/internal/guest/proclog"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

func newTrue() *exec.Cmd {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func withCgroupRoot(t *testing.T, root string, available bool) {
	t.Helper()
	r, a := cgroupRoot, cgroupAvailable
	t.Cleanup(func() { cgroupRoot, cgroupAvailable = r, a })
	cgroupRoot, cgroupAvailable = root, func(string) bool { return available }
}

// TestStartMeasuredWithoutMeasure: a process not asked to be measured
// makes no cgroup.
func TestStartMeasuredWithoutMeasure(t *testing.T) {
	root := t.TempDir()
	withCgroupRoot(t, root, true)
	cmd, m, err := startMeasured(newTrue, Header{MemoryMax: 1 << 30}, "p1")
	if err != nil || m != nil {
		t.Fatalf("startMeasured = %v, %v", m, err)
	}
	_ = cmd.Wait()
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("made %v", entries)
	}
}

// TestStartMeasuredWhereNoCgroupCanBeMade: a root that is no cgroup2, or
// one that refuses a group, runs the process unmeasured.
func TestStartMeasuredWhereNoCgroupCanBeMade(t *testing.T) {
	withCgroupRoot(t, filepath.Join(t.TempDir(), "absent"), true)
	cmd, m, err := startMeasured(newTrue, Header{Measure: true, MemoryMax: 1 << 30}, "p1")
	if err != nil || m != nil {
		t.Fatalf("startMeasured = %v, %v", m, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Error(err)
	}
	withCgroupRoot(t, t.TempDir(), false)
	if _, m, err := startMeasured(newTrue, Header{Measure: true}, "p2"); err != nil || m != nil {
		t.Fatalf("startMeasured = %v, %v", m, err)
	}
}

// TestStartMeasuredWhenTheStartIntoItFails: a directory that is no cgroup
// is made, but clone3 refuses to start a process into it; the process
// runs unmeasured, and the group is given up.
func TestStartMeasuredWhenTheStartIntoItFails(t *testing.T) {
	root := t.TempDir()
	withCgroupRoot(t, root, true)
	cmd, m, err := startMeasured(newTrue, Header{Measure: true, MemoryMax: 1 << 30}, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Errorf("measured in a directory that is no cgroup")
	}
	if err := cmd.Wait(); err != nil {
		t.Error(err)
	}
	if !cmd.ProcessState.Success() {
		t.Errorf("the process ended %v", cmd.ProcessState)
	}
}

// TestStartMeasuredOfACommandNotThere fails as a start without a cgroup
// does.
func TestStartMeasuredOfACommandNotThere(t *testing.T) {
	withCgroupRoot(t, t.TempDir(), true)
	_, _, err := startMeasured(func() *exec.Cmd {
		cmd := exec.Command("/nonexistent/program")
		cmd.SysProcAttr = &syscall.SysProcAttr{}
		return cmd
	}, Header{Measure: true}, "p1")
	if err == nil {
		t.Fatal("a command not there started")
	}
}

// TestALiveSupervisorsCgroupIsNotSwept: a supervisor's command line names
// its process, as cgroup.Sweep reads it to tell a live supervisor from a
// dead one: a cgroup named for a running process and its supervisor is
// not swept as another process starts; once the supervisor is gone, it is.
// Without it, every start swept every running process's cgroup, moving it
// out of /tower, unmeasured.
func TestALiveSupervisorsCgroupIsNotSwept(t *testing.T) {
	t.Setenv(mainEnv, "1")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	id, err := startSupervised(Header{Root: root, Command: []string{self}, Env: map[string]string{helperEnv: helperSleep}, LogCap: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	pid := pidIn(t, filepath.Join(root, id))
	t.Cleanup(func() { endProcess(t, root, id, pid) })
	supervisor, _ := statOf(t, pid)
	// A parent standing in for /tower, holding the cgroup Name gives the
	// process: a directory alone, which Sweep reads by its name.
	parent := t.TempDir()
	group := filepath.Join(parent, cgroup.Name(id, supervisor))
	if err := os.Mkdir(group, 0o755); err != nil {
		t.Fatal(err)
	}
	if swept := cgroup.Sweep(parent, t.TempDir()); len(swept) != 0 {
		b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", supervisor))
		t.Fatalf("the cgroup of process %s, its supervisor %d alive (command line %q), was swept: %v", id, supervisor, b, swept)
	}
	if _, err := os.Stat(group); err != nil {
		t.Fatalf("the live process's cgroup is gone: %v", err)
	}
}

// endProcess kills a process's group and waits for its supervisor to write
// its exit, before the test's directories go: one still writing it makes
// their removal fail.
func endProcess(t *testing.T, root, id string, pid int) {
	t.Helper()
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	testutil.EventuallyWithin(t, time.Minute, "process "+id+"'s exit written", func() bool {
		in, err := proclog.Read(root, id)
		return err == nil && in.Exit != nil
	})
}
