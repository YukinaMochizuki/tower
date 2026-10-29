package guestbin

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
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
