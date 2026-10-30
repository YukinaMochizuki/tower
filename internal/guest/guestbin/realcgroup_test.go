package guestbin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest/cgroup"
	"github.com/YukinaMochizuki/tower/internal/guest/proclog"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// The test binary stands in for tower-guest, and for the commands it
// runs, as the environment says: startSupervised runs os.Executable()
// supervise DIR, and a process's command is the binary again.
const (
	// mainEnv runs the binary as tower-guest, with its arguments.
	mainEnv = "TOWER_GUESTBIN_TEST_MAIN"
	// helperEnv runs the binary as a helper command: helperSleep,
	// helperAlloc, helperHold, helperWork, helperWho or helperAttack.
	helperEnv = "TOWER_GUESTBIN_TEST_HELPER"
	// allocEnv is how many bytes helperAlloc and helperHold touch.
	allocEnv = "TOWER_GUESTBIN_TEST_ALLOC"
	// releaseEnv is the file whose making ends helperHold.
	releaseEnv = "TOWER_GUESTBIN_TEST_RELEASE"
	// realEnv set to 1 runs TestRealCgroup, on a guest it may change.
	realEnv = "TOWER_REAL_CGROUP"
)

const (
	helperSleep  = "sleep"
	helperAlloc  = "alloc"
	helperHold   = "hold"
	helperWork   = "work"
	helperWho    = "who"
	helperAttack = "attack"
)

func TestMain(m *testing.M) {
	// A helper first: a command inherits its supervisor's environment.
	if h := os.Getenv(helperEnv); h != "" {
		os.Exit(helper(h))
	}
	if os.Getenv(mainEnv) == "1" {
		os.Exit(Main(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// sink keeps what the helpers allocate reachable.
var sink []byte

func helper(name string) int {
	switch name {
	case helperSleep:
		time.Sleep(time.Hour) //nolint:forbidigo // the helper is a long-running command, a service's stand-in, not a wait
		return 0
	case helperAlloc:
		n, err := strconv.ParseInt(os.Getenv(allocEnv), 10, 64)
		if err != nil {
			return 2
		}
		touch(n)
		return 0
	case helperHold:
		// A child in the background, left running as it ends, then
		// memory held until the release file is there.
		self, err := os.Executable()
		if err != nil {
			return 2
		}
		child := exec.Command(self)
		child.Env = append(os.Environ(), helperEnv+"="+helperSleep)
		if err := child.Start(); err != nil {
			return 2
		}
		fmt.Printf("child %d\n", child.Process.Pid)
		n, err := strconv.ParseInt(os.Getenv(allocEnv), 10, 64)
		if err != nil {
			return 2
		}
		touch(n)
		fmt.Println("held")
		for {
			if _, err := os.Stat(os.Getenv(releaseEnv)); err == nil {
				return 0
			}
			time.Sleep(20 * time.Millisecond) //nolint:forbidigo // the helper polls a file it is told to wait for, as a command would
		}
	case helperWork:
		touch(16 << 20)
		deadline := time.Now().Add(300 * time.Millisecond)
		x := uint64(1)
		for time.Now().Before(deadline) {
			for i := range 1 << 16 {
				x = x*6364136223846793005 + uint64(i)
			}
		}
		fmt.Println(x)
		return 0
	case helperWho:
		return who()
	case helperAttack:
		return attack()
	}
	return 2
}

// touch allocates n bytes and writes every page, so each is charged.
func touch(n int64) {
	sink = make([]byte, n)
	for i := int64(0); i < n; i += 4096 {
		sink[i] = 1
	}
}

// realLimit is /tower's memory.max in TestRealCgroup: what tower gives a
// box of 256Mi, half of it.
const realLimit = 128 << 20

// TestRealCgroup runs processes through supervise against the real
// cgroup2 root, as a hangar guest does: each in a cgroup of its own under
// /tower, held to one limit together; past it, one is OOM-killed whole and
// the rest of the guest runs on; a normal one reports its usage; what a
// process leaves running, and a cgroup whose supervisor died, go back to
// the supervisor's cgroup; and every cgroup made is gone after. It changes
// the guest's /tower, so it runs only with TOWER_REAL_CGROUP=1, as root,
// where the root is writable, and fails on a guest whose /tower is in use.
func TestRealCgroup(t *testing.T) {
	if os.Getenv(realEnv) != "1" {
		t.Skipf("%s=1 is not set: this test makes cgroups under %s", realEnv, cgroup.Root)
	}
	if os.Geteuid() != 0 {
		t.Skip("not root")
	}
	if !cgroup.Available(cgroup.Root) || syscall.Access(cgroup.Root, 2 /* W_OK */) != nil {
		t.Skipf("%s is no writable cgroup2 mount", cgroup.Root)
	}
	b, err := os.ReadFile(filepath.Join(cgroup.Root, "cgroup.subtree_control"))
	if err != nil || !strings.Contains(" "+strings.TrimSpace(string(b))+" ", " memory ") {
		t.Fatalf("the root enables no memory controller for its children: %q, %v", b, err)
	}
	if b, err := os.ReadFile("/proc/swaps"); err == nil && len(strings.Split(strings.TrimSpace(string(b)), "\n")) > 1 {
		t.Logf("the guest has swap: a process past memory.max may swap rather than be killed")
	}
	tower := filepath.Join(cgroup.Root, cgroup.Parent)
	madeTower := false
	switch entries, err := os.ReadDir(tower); {
	case os.IsNotExist(err):
		madeTower = true
	case err != nil:
		t.Fatal(err)
	default:
		for _, e := range entries {
			if e.IsDir() {
				t.Fatalf("%s holds %s: run this on a guest no tower process runs in", tower, e.Name())
			}
		}
		if b, err := os.ReadFile(filepath.Join(tower, "memory.max")); err == nil {
			t.Cleanup(func() { _ = os.WriteFile(filepath.Join(tower, "memory.max"), b, 0o644) })
		}
	}
	selfLine, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the test runs in %s", strings.TrimSpace(string(selfLine)))
	t.Setenv(mainEnv, "1")
	root := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	var pids []int
	t.Cleanup(func() {
		// What a failed test left running goes, so its cgroups can.
		entries, _ := os.ReadDir(tower)
		for _, e := range entries {
			for _, id := range ids {
				if strings.HasPrefix(e.Name(), id+"-") {
					_ = os.WriteFile(filepath.Join(tower, e.Name(), "cgroup.kill"), []byte("1"), 0o644)
				}
			}
		}
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	start := func(env ...string) string {
		t.Helper()
		e := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			e[k] = v
		}
		id, err := startSupervised(Header{Root: root, Command: []string{self}, Env: e, LogCap: 1 << 20, Keep: 100, Measure: true, MemoryMax: realLimit})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		return id
	}
	pidOf := func(id string) int {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, id, proclog.PIDFile))
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.Atoi(string(b))
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, n)
		return n
	}
	cgroupOf := func(pid int) string {
		t.Helper()
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	alive := func(pid int) bool { return syscall.Kill(pid, 0) == nil }
	wait := func(id string) *proclog.Exit {
		t.Helper()
		var exit *proclog.Exit
		testutil.EventuallyWithin(t, 2*time.Minute, "process "+id+" ended", func() bool {
			in, err := proclog.Read(root, id)
			if err != nil {
				t.Fatal(err)
			}
			exit = in.Exit
			return exit != nil
		})
		return exit
	}
	logOf := func(id string) string {
		t.Helper()
		var out strings.Builder
		if _, err := proclog.ReadLog(context.Background(), root, id, 0, false, func(c proclog.Chunk) error {
			out.Write(c.Data)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	childOf := func(id string) int {
		t.Helper()
		var pid int
		testutil.EventuallyWithin(t, time.Minute, "process "+id+" said its child", func() bool {
			_, err := fmt.Sscanf(logOf(id), "child %d", &pid)
			return err == nil
		})
		pids = append(pids, pid)
		return pid
	}

	// (1) A process runs in a cgroup of its own under /tower, which holds
	// the limit; the process's own has none, and is killed whole on OOM.
	sibling := start(helperEnv + "=" + helperSleep)
	line := cgroupOf(pidOf(sibling))
	if !strings.HasPrefix(line, "0::/"+cgroup.Parent+"/"+sibling+"-") {
		t.Fatalf("the process runs in %q, want one of its own under /%s", line, cgroup.Parent)
	}
	group := filepath.Join(cgroup.Root, strings.TrimPrefix(line, "0::/"))
	for file, want := range map[string]string{
		filepath.Join(tower, "memory.max"):       strconv.Itoa(realLimit),
		filepath.Join(group, "memory.max"):       "max",
		filepath.Join(group, "memory.oom.group"): "1",
	} {
		if b, err := os.ReadFile(file); err != nil || strings.TrimSpace(string(b)) != want {
			t.Errorf("%s is %q, %v; want %s", file, b, err, want)
		}
	}

	// (2) One past the limit is OOM-killed, and says so.
	hog := start(helperEnv+"="+helperAlloc, allocEnv+"="+strconv.Itoa(4*realLimit))
	exit := wait(hog)
	t.Logf("the allocator, under a limit of %d bytes, ended %+v", realLimit, exit)
	u := exit.Usage
	switch {
	case u == nil:
		t.Fatal("the allocator's exit has no usage")
	case u.OOMKilled == nil || !*u.OOMKilled:
		t.Errorf("the allocator's exit says oom_killed %v", u.OOMKilled)
	case exit.Signal != int(syscall.SIGKILL):
		t.Errorf("the allocator ended by signal %d, not SIGKILL", exit.Signal)
	}
	if u != nil && (u.MemoryPeak == nil || *u.MemoryPeak < realLimit*3/4 || *u.MemoryPeak > realLimit) {
		t.Errorf("the allocator's memory peak is %v, want near %d", u.MemoryPeak, realLimit)
	}

	// (3) Its sibling runs on.
	if in, err := proclog.Read(root, sibling); err != nil || in.Exit != nil {
		t.Errorf("the sibling ended with the allocator: %+v, %v", in.Exit, err)
	}
	if !alive(pidOf(sibling)) {
		t.Error("the sibling's process is gone")
	}
	if err := syscall.Kill(-pidOf(sibling), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if e := wait(sibling); e.Signal != int(syscall.SIGTERM) || e.Usage == nil {
		t.Errorf("the sibling sent TERM ended %+v", e)
	}

	// (4) A normal process reports what it used.
	worker := start(helperEnv + "=" + helperWork)
	exit = wait(worker)
	t.Logf("the worker ended %+v", exit)
	u = exit.Usage
	switch {
	case exit.Code != 0:
		t.Errorf("the worker exited %d", exit.Code)
	case u == nil:
		t.Fatal("the worker's exit has no usage")
	case u.MemoryPeak == nil || *u.MemoryPeak <= 0:
		t.Errorf("the worker's memory peak is %v", u.MemoryPeak)
	case u.CPUUsage == nil || *u.CPUUsage <= 0:
		t.Errorf("the worker's CPU usage is %v", u.CPUUsage)
	case u.Wall <= 0:
		t.Errorf("the worker's wall time is %v", u.Wall)
	case u.OOMKilled == nil || *u.OOMKilled:
		t.Errorf("the worker's exit says oom_killed %v", u.OOMKilled)
	}
	if u != nil && u.CPUWait == nil {
		t.Logf("the kernel offers no cpu.pressure: cpu_wait unset")
	}

	// (6) Two processes, each under the limit and together over it: one is
	// killed whole, its child with it, and a process outside tower's path
	// runs on. The other, released, ends, leaving its child running, which
	// goes back to the supervisor's cgroup.
	outside := exec.Command(self)
	outside.Env = append(os.Environ(), helperEnv+"="+helperSleep)
	if err := outside.Start(); err != nil {
		t.Fatal(err)
	}
	pids = append(pids, outside.Process.Pid)
	t.Logf("a process outside tower's path runs in %s", cgroupOf(outside.Process.Pid))
	release := filepath.Join(t.TempDir(), "release")
	hold := func() string {
		return start(helperEnv+"="+helperHold, allocEnv+"="+strconv.Itoa(realLimit*6/10), releaseEnv+"="+release)
	}
	first := hold()
	testutil.EventuallyWithin(t, time.Minute, "the first holder's memory held", func() bool { return strings.Contains(logOf(first), "held") })
	second := hold()
	children := map[string]int{first: childOf(first), second: childOf(second)}
	var killed, survivor string
	testutil.EventuallyWithin(t, 2*time.Minute, "one holder ended", func() bool {
		for _, id := range []string{first, second} {
			if in, err := proclog.Read(root, id); err == nil && in.Exit != nil {
				killed = id
			}
		}
		return killed != ""
	})
	survivor = first
	if killed == first {
		survivor = second
	}
	exit = wait(killed)
	t.Logf("the holder killed ended %+v", exit)
	if exit.Usage == nil || exit.Usage.OOMKilled == nil || !*exit.Usage.OOMKilled || exit.Signal != int(syscall.SIGKILL) {
		t.Errorf("the holder that ended says %+v, want OOM-killed", exit)
	}
	testutil.Eventually(t, "the killed holder's child killed with it", func() bool { return !alive(children[killed]) })
	if !alive(outside.Process.Pid) {
		t.Error("a process outside tower's path was killed")
	}
	testutil.EventuallyWithin(t, time.Minute, "the surviving holder's memory held", func() bool { return strings.Contains(logOf(survivor), "held") })
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	exit = wait(survivor)
	if exit.Code != 0 || exit.Usage == nil || exit.Usage.OOMKilled == nil || *exit.Usage.OOMKilled {
		t.Errorf("the surviving holder ended %+v", exit)
	}
	left := children[survivor]
	testutil.Eventually(t, "the survivor's child moved to the supervisor's cgroup", func() bool {
		return alive(left) && cgroupOf(left) == strings.TrimSpace(string(selfLine))
	})
	_ = syscall.Kill(left, syscall.SIGKILL)
	_ = syscall.Kill(outside.Process.Pid, syscall.SIGKILL)
	_ = outside.Wait()

	// (7) A cgroup whose supervisor died is swept as the next process
	// starts, what runs in it moved to the supervisor's cgroup.
	orphan := start(helperEnv + "=" + helperSleep)
	orphanPID := pidOf(orphan)
	orphanGroup := strings.TrimPrefix(cgroupOf(orphanPID), "0::/")
	_, supervisor, _ := strings.Cut(filepath.Base(orphanGroup), "-")
	spid, err := strconv.Atoi(supervisor)
	if err != nil {
		t.Fatalf("the orphan's cgroup %s names no supervisor", orphanGroup)
	}
	if err := syscall.Kill(spid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the orphan's supervisor gone", func() bool {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", spid))
		return err != nil || !strings.Contains(string(b), orphan)
	})
	wait(start(helperEnv + "=" + helperWork))
	if _, err := os.Stat(filepath.Join(cgroup.Root, orphanGroup)); !os.IsNotExist(err) {
		t.Errorf("the orphan's cgroup %s was not swept: %v", orphanGroup, err)
	}
	if !alive(orphanPID) || cgroupOf(orphanPID) != strings.TrimSpace(string(selfLine)) {
		t.Errorf("the orphan, alive %v, runs in %s after the sweep", alive(orphanPID), cgroupOf(orphanPID))
	}
	_ = syscall.Kill(orphanPID, syscall.SIGKILL)

	// (5) Every cgroup made is gone: each is removed as its exit is
	// written, so a moment after; /tower too, if the test made it.
	testutil.Eventually(t, "the test's cgroups removed", func() bool {
		entries, _ := os.ReadDir(tower)
		for _, e := range entries {
			for _, id := range ids {
				if strings.HasPrefix(e.Name(), id+"-") {
					return false
				}
			}
		}
		return true
	})
	if madeTower {
		if err := syscall.Rmdir(tower); err != nil {
			t.Errorf("removing %s, which the test made: %v", tower, err)
		}
	}
}
