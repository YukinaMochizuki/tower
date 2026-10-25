package drivertest

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// RunProcesses runs the suite of the container driver's processes.
func RunProcesses(t *testing.T, h Harness) { RunGuestProcesses(t, h, h.containers()) }

// RunVMProcesses runs the suite of the VM driver's processes.
func RunVMProcesses(t *testing.T, h Harness) { RunGuestProcesses(t, h, h.vms()) }

// RunGuestProcesses runs the suite of a guest driver's processes.
func RunGuestProcesses(t *testing.T, h Harness, k GuestKind) {
	tests := []struct {
		name string
		fn   func(*testing.T, Harness, GuestKind)
	}{
		{"OutputAndExit", testProcOutput},
		{"FollowUntilSignalled", testProcFollow},
		{"EnvironmentIsTheProcesss", testProcEnv},
		{"LimitsOfLogAndKept", testProcLimits},
		{"StoppingEndsThem", testProcStop},
		{"OutliveTheirCall", testProcOutlivesItsCall},
		{"SignalReachesTheGroup", testProcGroup},
		{"ReadBackAfterReopen", testProcReopen},
		{"NoEnvironmentKept", testNoEnvKept},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, h, k) })
	}
}

// processes is the guest kind's driver as the driver of its processes.
func processes(t *testing.T, k GuestKind) driver.Processes {
	t.Helper()
	p, ok := k.Driver.(driver.Processes)
	if !ok {
		t.Fatalf("%T runs no processes", k.Driver)
	}
	return p
}

// defaults are a process's limits where a test does not try them.
func spec(command ...string) driver.ProcessSpec {
	return driver.ProcessSpec{Command: command, LogCap: 1 << 20, Keep: 100}
}

// readLog reads a process's whole log, without following it.
func readLog(t *testing.T, p driver.Processes, guest, id string, from int64) (string, []driver.LogChunk, driver.LogEnd) {
	t.Helper()
	var text strings.Builder
	var chunks []driver.LogChunk
	end, err := p.ReadProcessLog(t.Context(), guest, id, from, false, func(c driver.LogChunk) error {
		text.Write(c.Data)
		chunks = append(chunks, c)
		return nil
	})
	mustNot(t, "reading a process's log", err)
	return text.String(), chunks, end
}

func startProc(t *testing.T, p driver.Processes, guest string, s driver.ProcessSpec) string {
	t.Helper()
	id, err := p.StartProcess(t.Context(), guest, s)
	mustNot(t, "starting "+strings.Join(s.Command, " "), err)
	if id == "" {
		t.Fatal("a process started with no id")
	}
	return id
}

// waitExit follows a process's log until it ends.
func waitExit(t *testing.T, p driver.Processes, guest, id string) driver.LogEnd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	end, err := p.ReadProcessLog(ctx, guest, id, 0, true, func(driver.LogChunk) error { return nil })
	mustNot(t, "following a process", err)
	return end
}

func testProcOutput(t *testing.T, h Harness, k GuestKind) {
	c := processes(t, k)
	g := k.guestWithDisk(t, h, "out")
	s := spec("emit", "out:one", "err:two", "out:three")
	s.Name = "emitter"
	id := startProc(t, c, g, s)
	if end := waitExit(t, c, g, id); end.Exit == nil || end.Exit.Code != 0 || end.Exit.Signal != 0 {
		t.Errorf("the process ended %+v", end.Exit)
	}
	text, chunks, end := readLog(t, c, g, id, 0)
	if text != "one\ntwo\nthree\n" || len(chunks) < 3 || chunks[0].Stderr || !chunks[1].Stderr {
		t.Errorf("the log: %q as %+v", text, chunks)
	}
	if end.HeldFrom != 0 || end.Next != int64(len(text)) {
		t.Errorf("the log ends %+v, want from 0 to %d", end, len(text))
	}
	if rest, chunks, _ := readLog(t, c, g, id, 5); rest != "wo\nthree\n" || chunks[0].Offset != 5 {
		t.Errorf("from offset 5: %q at %+v", rest, chunks)
	}
	if rest, _, at := readLog(t, c, g, id, end.Next); rest != "" || at.Next != end.Next {
		t.Errorf("from its end: %q, %+v", rest, at)
	}
	failed := startProc(t, c, g, spec("exit", "3"))
	if end := waitExit(t, c, g, failed); end.Exit == nil || end.Exit.Code != 3 {
		t.Errorf("exit 3 ended %+v", end.Exit)
	}
	list, err := c.ListProcesses(t.Context(), g)
	mustNot(t, "listing processes", err)
	if len(list) != 2 || list[0].ID != id || list[0].Name != "emitter" || list[1].ID != failed || list[0].Started.IsZero() {
		t.Errorf("listed %+v", list)
	}
	_, err = c.ReadProcessLog(t.Context(), g, "no-such-process", 0, false, func(driver.LogChunk) error { return nil })
	is(t, "a process not there", err, driver.ErrNotFound)
}

func testProcFollow(t *testing.T, h Harness, k GuestKind) {
	c := processes(t, k)
	g := k.guestWithDisk(t, h, "follow")
	id := startProc(t, c, g, spec("serve", "listening"))
	if list, _ := c.ListProcesses(t.Context(), g); len(list) != 1 || list[0].Exit != nil {
		t.Errorf("a running process listed %+v", list)
	}
	seen := make(chan string, 1)
	type followed struct {
		end driver.LogEnd
		err error
	}
	done := make(chan followed, 1)
	go func() {
		end, err := c.ReadProcessLog(t.Context(), g, id, 0, true, func(ch driver.LogChunk) error {
			select {
			case seen <- string(ch.Data):
			default:
			}
			return nil
		})
		done <- followed{end, err}
	}()
	select {
	case text := <-seen:
		if text != "listening\n" {
			t.Errorf("a follow of a running process read %q", text)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a follow read nothing of a running process")
	}
	mustNot(t, "signalling", c.SignalProcess(t.Context(), g, id, 15))
	select {
	case f := <-done:
		if end := f.end; f.err != nil || end.Exit == nil || end.Exit.Signal != 15 || end.Exit.Code != 128+15 {
			t.Errorf("a follow of a process sent TERM ended %+v", end.Exit)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a follow did not end with its process")
	}
	mustNot(t, "signalling a process that ended", c.SignalProcess(t.Context(), g, id, 9))
	_, err := c.StartProcess(t.Context(), name(t, "absent"), spec("true"))
	is(t, "a process in a guest not there", err, driver.ErrNotFound)
}

func testProcEnv(t *testing.T, h Harness, k GuestKind) {
	c := processes(t, k)
	g := k.guestWithDisk(t, h, "env")
	s := spec("env")
	s.Env = map[string]string{"DRIVERTEST_PROC": "given"}
	id := startProc(t, c, g, s)
	waitExit(t, c, g, id)
	if text, _, _ := readLog(t, c, g, id, 0); !strings.Contains(text, "DRIVERTEST_PROC=given\n") {
		t.Errorf("the environment given a process is not its own: %q", text)
	}
	other := startProc(t, c, g, spec("env"))
	waitExit(t, c, g, other)
	if text, _, _ := readLog(t, c, g, other, 0); strings.Contains(text, "DRIVERTEST_PROC") {
		t.Errorf("one process's environment reached the next: %q", text)
	}
}

func testProcLimits(t *testing.T, h Harness, k GuestKind) {
	c := processes(t, k)
	g := k.guestWithDisk(t, h, "limits")
	s := spec("emit", "out:0123456789", "out:abcdefghij", "out:ABCDEFGHIJ")
	s.LogCap, s.Keep = 16, 2
	id := startProc(t, c, g, s)
	waitExit(t, c, g, id)
	text, chunks, end := readLog(t, c, g, id, 0)
	if end.HeldFrom != 17 || end.Next != 33 || text != "ghij\nABCDEFGHIJ\n" || chunks[0].Offset != 17 {
		t.Errorf("a log over its cap: %q at %+v, ends %+v", text, chunks, end)
	}
	running := startProc(t, c, g, spec("sleep", "3600"))
	for range 3 {
		q := spec("true")
		q.Keep = 2
		waitExit(t, c, g, startProc(t, c, g, q))
	}
	list, err := c.ListProcesses(t.Context(), g)
	mustNot(t, "listing processes", err)
	finished := 0
	for _, p := range list {
		if p.Exit != nil {
			finished++
		}
		if p.ID == id {
			t.Errorf("the oldest finished process is still kept: %+v", list)
		}
	}
	if finished != 2 || len(list) != 3 || list[0].ID != running {
		t.Errorf("with 2 finished kept and one running, listed %+v", list)
	}
}

// logNext is where a process's log ends now.
func logNext(t *testing.T, p driver.Processes, guest, id string) int64 {
	t.Helper()
	_, _, end := readLog(t, p, guest, id, 0)
	return end.Next
}

func testProcOutlivesItsCall(t *testing.T, h Harness, k GuestKind) {
	c := processes(t, k)
	g := k.guestWithDisk(t, h, "outlive")
	ctx, cancel := context.WithCancel(t.Context())
	id, err := c.StartProcess(ctx, g, spec("tick"))
	cancel()
	mustNot(t, "starting tick", err)
	first := logNext(t, c, g, id)
	deadline := time.Now().Add(30 * time.Second)
	for logNext(t, c, g, id) <= first {
		if time.Now().After(deadline) {
			t.Fatal("a process whose starting call ended writes nothing more")
		}
		<-time.After(5 * time.Millisecond)
	}
	if list, _ := c.ListProcesses(t.Context(), g); len(list) != 1 || list[0].Exit != nil {
		t.Errorf("a process whose starting call ended: %+v, want it running", list)
	}
	mustNot(t, "signalling", c.SignalProcess(t.Context(), g, id, 9))
}

func testProcGroup(t *testing.T, h Harness, k GuestKind) {
	c := processes(t, k)
	g := k.guestWithDisk(t, h, "group")
	id := startProc(t, c, g, spec("spawn"))
	if _, out := run(t, k.Driver, g, nil, "children"); out != "1\n" {
		t.Fatalf("a process that spawned a child: %q children alive", out)
	}
	mustNot(t, "signalling", c.SignalProcess(t.Context(), g, id, 15))
	waitExit(t, c, g, id)
	if _, out := run(t, k.Driver, g, nil, "children"); out != "0\n" {
		t.Errorf("a signal to the process left %q children alive; it reaches its group", out)
	}
}

// reopened is the guest kind's driver of a hostd opened again.
func reopened(t *testing.T, h Harness, k GuestKind) driver.Processes {
	d := h.Reopen(t)
	if k.DiskKind == driver.Volume {
		return d.VMs
	}
	return d.Containers
}

func testProcReopen(t *testing.T, h Harness, k GuestKind) {
	if h.Reopen == nil {
		t.Skip("the harness cannot open the drivers again")
	}
	c := processes(t, k)
	g := k.guestWithDisk(t, h, "reopen")
	done := startProc(t, c, g, spec("echo", "kept"))
	waitExit(t, c, g, done)
	running := startProc(t, c, g, spec("sleep", "3600"))

	again := reopened(t, h, k)
	list, err := again.ListProcesses(t.Context(), g)
	mustNot(t, "listing after a reopen", err)
	if len(list) != 2 || list[0].ID != done || list[0].Exit == nil || list[1].ID != running || list[1].Exit != nil {
		t.Fatalf("after a reopen, listed %+v", list)
	}
	if text, _, _ := readLog(t, again, g, done, 0); text != "kept\n" {
		t.Errorf("a log read after a reopen: %q", text)
	}
	mustNot(t, "signalling after a reopen", again.SignalProcess(t.Context(), g, running, 15))
	if end := waitExit(t, again, g, running); end.Exit == nil || end.Exit.Signal != 15 {
		t.Errorf("a process signalled after a reopen ended %+v", end.Exit)
	}
}

func testNoEnvKept(t *testing.T, h Harness, k GuestKind) {
	if h.HostFiles == "" {
		t.Skip("the harness says not where the host keeps its files")
	}
	c := processes(t, k)
	g := k.guestWithDisk(t, h, "noenv")
	secret := "drivertest-value-" + name(t, "")
	s := spec("sleep", "3600")
	s.Env = map[string]string{"DRIVERTEST_SECRET": secret}
	id := startProc(t, c, g, s)
	run(t, k.Driver, g, map[string]string{"DRIVERTEST_SECRET": secret}, "true")
	notOnHost(t, h, secret)
	// Nor in what the guest keeps of its processes, where it keeps them.
	for _, dir := range k.Keeps {
		if _, out := run(t, k.Driver, g, nil, "grep", "-rl", secret, dir); out != "" {
			t.Errorf("the guest's %s holds what must never be kept: %s", dir, out)
		}
	}
	mustNot(t, "signalling", c.SignalProcess(t.Context(), g, id, 15))
}

// notOnHost fails the test if any file where the host keeps its files
// holds s.
func notOnHost(t *testing.T, h Harness, s string) {
	t.Helper()
	err := filepath.WalkDir(h.HostFiles, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			// Sockets and links hold nothing kept.
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(s)) {
			t.Errorf("%s holds what must never be kept", path)
		}
		return nil
	})
	mustNot(t, "searching the host's files", err)
}

// testProcStop: a guest stopping ends its processes. A container's are
// read from the host while it is stopped; a VM's are the guest's own, read
// once it runs again.
func testProcStop(t *testing.T, h Harness, k GuestKind) {
	c := processes(t, k)
	g := k.guestWithDisk(t, h, "stop")
	id := startProc(t, c, g, spec("sleep", "3600"))
	mustNot(t, "stopping the guest", k.Driver.Stop(t.Context(), g))
	_, err := c.StartProcess(t.Context(), g, spec("true"))
	if !errors.Is(err, driver.ErrNotRunning) {
		t.Errorf("a process in a stopped guest: %v, want ErrNotRunning", err)
	}
	if k.DiskKind == driver.Volume {
		mustNot(t, "starting the guest again", k.Driver.Start(t.Context(), g))
	}
	list, err := c.ListProcesses(t.Context(), g)
	mustNot(t, "listing processes", err)
	if len(list) != 1 || list[0].ID != id || list[0].Exit == nil {
		t.Errorf("after its guest stopped, its process: %+v; want it ended", list)
	}
}
