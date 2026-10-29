package guestbin

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest/cgroup"
	"github.com/YukinaMochizuki/tower/internal/guest/proclog"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// The test binary stands in for tower-guest, and for the commands it
// runs, as the environment says: startSupervised runs os.Executable()
// supervise DIR, and a process's command is the binary again.
const (
	// mainEnv runs the binary as tower-guest, with its arguments.
	mainEnv = "TOWER_GUESTBIN_TEST_MAIN"
	// helperEnv runs the binary as a helper command: helperSleep,
	// helperAlloc or helperWork.
	helperEnv = "TOWER_GUESTBIN_TEST_HELPER"
	// allocEnv is how many bytes helperAlloc allocates and touches.
	allocEnv = "TOWER_GUESTBIN_TEST_ALLOC"
	// realEnv set to 1 runs TestRealCgroup, on a guest it may change.
	realEnv = "TOWER_REAL_CGROUP"
)

const (
	helperSleep = "sleep"
	helperAlloc = "alloc"
	helperWork  = "work"
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

// sink keeps what helperAlloc and helperWork allocate reachable.
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

// TestRealCgroup runs processes through supervise against the real
// cgroup2 root, as a hangar guest does: each in a cgroup of its own
// directly under the root; one past its memory.max is OOM-killed alone,
// its sibling running on; a normal one reports its usage; and every
// cgroup made is gone after. It changes the guest's cgroups, so it runs
// only with TOWER_REAL_CGROUP=1, as root, where the root is writable.
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
	t.Setenv(mainEnv, "1")
	root := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// As the hangar provider asks for a box of 256Mi.
	const instanceMemory = 256 << 20
	limit := provider.ProcessMemoryMax(instanceMemory)
	var made []string
	start := func(helper string, env map[string]string) string {
		t.Helper()
		e := map[string]string{helperEnv: helper}
		for k, v := range env {
			e[k] = v
		}
		id, err := startSupervised(Header{Root: root, Command: []string{self}, Env: e, LogCap: 1 << 20, Keep: 100, Measure: true, MemoryMax: limit})
		if err != nil {
			t.Fatal(err)
		}
		made = append(made, filepath.Join(cgroup.Root, "tower-"+id))
		return id
	}
	pid := func(id string) int {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, id, proclog.PIDFile))
		if err != nil {
			t.Fatal(err)
		}
		n, err := strconv.Atoi(string(b))
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
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
	t.Cleanup(func() {
		// What a failed test left running goes, so its cgroups can.
		for _, dir := range made {
			_ = os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644)
		}
	})

	// (1) A process runs in a cgroup of its own, directly under the root.
	sibling := start(helperSleep, nil)
	b, err = os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid(sibling)))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(string(b)), "0::/tower-"+sibling; got != want {
		t.Fatalf("the process runs in %q, want %q", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(cgroup.Root, "tower-"+sibling, "memory.max")); err != nil || strings.TrimSpace(string(b)) != strconv.FormatInt(limit, 10) {
		t.Errorf("its memory.max is %q, %v; want %d", b, err, limit)
	}

	// (2) One past its memory.max is OOM-killed, and says so.
	hog := start(helperAlloc, map[string]string{allocEnv: strconv.FormatInt(4*limit, 10)})
	exit := wait(hog)
	t.Logf("the allocator, held to %d bytes, ended %+v", limit, exit)
	u := exit.Usage
	switch {
	case u == nil:
		t.Fatal("the allocator's exit has no usage")
	case u.OOMKilled == nil || !*u.OOMKilled:
		t.Errorf("the allocator's exit says oom_killed %v", u.OOMKilled)
	case exit.Signal != int(syscall.SIGKILL):
		t.Errorf("the allocator ended by signal %d, not SIGKILL", exit.Signal)
	}
	if u != nil && (u.MemoryPeak == nil || *u.MemoryPeak < limit*3/4 || *u.MemoryPeak > limit) {
		t.Errorf("the allocator's memory peak is %v, want near %d", u.MemoryPeak, limit)
	}

	// (3) Its sibling runs on.
	if in, err := proclog.Read(root, sibling); err != nil || in.Exit != nil {
		t.Errorf("the sibling ended with the allocator: %+v, %v", in.Exit, err)
	}
	if err := syscall.Kill(pid(sibling), 0); err != nil {
		t.Errorf("the sibling's process is gone: %v", err)
	}
	if err := syscall.Kill(-pid(sibling), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if e := wait(sibling); e.Signal != int(syscall.SIGTERM) || e.Usage == nil {
		t.Errorf("the sibling sent TERM ended %+v", e)
	}

	// (4) A normal process reports what it used.
	worker := start(helperWork, nil)
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

	// (5) Every cgroup made is gone: each is removed as its exit is
	// written, so a moment after.
	for _, dir := range made {
		testutil.Eventually(t, dir+" removed", func() bool {
			_, err := os.Stat(dir)
			return os.IsNotExist(err)
		})
	}
}
