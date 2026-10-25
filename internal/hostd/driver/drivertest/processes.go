package drivertest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// RunProcesses runs the suite of a container driver's processes.
func RunProcesses(t *testing.T, h Harness) {
	tests := []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"OutputAndExit", testProcOutput},
		{"FollowUntilSignalled", testProcFollow},
		{"EnvironmentIsTheProcesss", testProcEnv},
		{"LimitsOfLogAndKept", testProcLimits},
		{"StoppingEndsThem", testProcStop},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, h) })
	}
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

func testProcOutput(t *testing.T, h Harness) {
	c := h.Drivers.Containers
	g := h.containers().guestWithDisk(t, h, "out")
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

func testProcFollow(t *testing.T, h Harness) {
	c := h.Drivers.Containers
	g := h.containers().guestWithDisk(t, h, "follow")
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

func testProcEnv(t *testing.T, h Harness) {
	c := h.Drivers.Containers
	g := h.containers().guestWithDisk(t, h, "env")
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

func testProcLimits(t *testing.T, h Harness) {
	c := h.Drivers.Containers
	g := h.containers().guestWithDisk(t, h, "limits")
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

func testProcStop(t *testing.T, h Harness) {
	c := h.Drivers.Containers
	g := h.containers().guestWithDisk(t, h, "stop")
	id := startProc(t, c, g, spec("sleep", "3600"))
	mustNot(t, "stopping the guest", c.Stop(t.Context(), g))
	list, err := c.ListProcesses(t.Context(), g)
	mustNot(t, "listing processes", err)
	if len(list) != 1 || list[0].ID != id || list[0].Exit == nil {
		t.Errorf("after its guest stopped, its process: %+v", list)
	}
	_, err = c.StartProcess(t.Context(), g, spec("true"))
	if !errors.Is(err, driver.ErrNotRunning) {
		t.Errorf("a process in a stopped guest: %v, want ErrNotRunning", err)
	}
}
