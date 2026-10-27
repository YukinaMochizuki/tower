package providertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// attached is what an attach gave: the output, each chunk as
// "out:text" or "err:text", and the offsets it was at.
type attached struct {
	chunks  []string
	offsets []int64
	text    string
	end     provider.Attached
}

func attach(t *testing.T, h Harness, ctx context.Context, in provider.Instance, id string, from int64, follow bool) (attached, error) {
	t.Helper()
	var a attached
	end, err := h.Provider.AttachProcess(ctx, in, id, from, follow, func(c provider.OutputChunk) error {
		stream := "out:"
		if c.Stream == provider.Stderr {
			stream = "err:"
		}
		a.chunks = append(a.chunks, stream+string(c.Data))
		a.offsets = append(a.offsets, c.Offset)
		a.text += string(c.Data)
		return nil
	})
	a.end = end
	return a, err
}

func start(t *testing.T, h Harness, in provider.Instance, name string, command ...string) string {
	t.Helper()
	id, err := h.Provider.StartProcess(t.Context(), provider.ProcessRequest{Instance: in, Command: command, Name: name})
	if err != nil {
		t.Fatalf("starting %v: %v", command, err)
	}
	if id == "" {
		t.Fatalf("starting %v gave no id", command)
	}
	return id
}

// waitEnded waits until the provider lists the process ended: one runs
// for a while on a real host, however short its command.
func waitEnded(t *testing.T, h Harness, in provider.Instance, id string) {
	t.Helper()
	testutil.Eventually(t, "process "+id+" ended", func() (bool, string) {
		list, err := h.Provider.ListProcesses(t.Context(), in)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range list {
			if p.ID == id {
				return p.Exit != nil, fmt.Sprintf("%+v", p)
			}
		}
		return false, fmt.Sprintf("not listed in %+v", list)
	})
}

func testProcessOutput(t *testing.T, h Harness) {
	in, _ := h.start(t, "proc-output", h.container("net-a", "proc-output"))
	id := start(t, h, in, "emitter", "emit", "out:one", "err:two", "out:three")
	a, err := attach(t, h, t.Context(), in, id, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if a.text != "one\ntwo\nthree\n" || a.chunks[0] != "out:one\n" || a.chunks[1] != "err:two\n" {
		t.Errorf("the output came as %q", a.chunks)
	}
	if a.end.Exit == nil || a.end.Exit.Code != 0 || a.end.Next != int64(len(a.text)) || a.end.HeldFrom != 0 {
		t.Errorf("the attach ended %+v (exit %+v), want exit 0 at %d", a.end, a.end.Exit, len(a.text))
	}
	// From an offset, only what follows it, within a chunk too.
	b, err := attach(t, h, t.Context(), in, id, 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if b.text != "wo\nthree\n" || b.offsets[0] != 5 {
		t.Errorf("from offset 5: %q at %v", b.text, b.offsets)
	}
	// From the end, nothing, and where it is.
	c, err := attach(t, h, t.Context(), in, id, a.end.Next, false)
	if err != nil || c.text != "" || c.end.Next != a.end.Next {
		t.Errorf("from the end: %q, %+v, %v", c.text, c.end, err)
	}
	if _, err := attach(t, h, t.Context(), in, "no-such-process", 0, false); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("attaching to a process not there: %v, want ErrNotFound", err)
	}
}

// errDropped stands for a client that went away mid-attach.
var errDropped = errors.New("dropped")

func testProcessReattach(t *testing.T, h Harness) {
	in, _ := h.start(t, "proc-reattach", h.container("net-a", "proc-reattach"))
	id := start(t, h, in, "", "emit", "out:a", "out:b", "out:c")
	var got string
	var at int64
	_, err := h.Provider.AttachProcess(t.Context(), in, id, 0, true, func(c provider.OutputChunk) error {
		got, at = string(c.Data), c.Offset+int64(len(c.Data))
		return errDropped
	})
	if !errors.Is(err, errDropped) {
		t.Fatalf("an attach whose reader failed: %v", err)
	}
	rest, err := attach(t, h, t.Context(), in, id, at, true)
	if err != nil {
		t.Fatal(err)
	}
	if got+rest.text != "a\nb\nc\n" {
		t.Errorf("read %q, then from %d %q: something lost or read twice", got, at, rest.text)
	}
}

func testProcessSignalAndList(t *testing.T, h Harness) {
	in, _ := h.start(t, "proc-signal", h.container("net-a", "proc-signal"))
	done := start(t, h, in, "quick", "exit", "3")
	waitEnded(t, h, in, done)
	running := start(t, h, in, "server", "sleep", "3600")
	// A follow goes on until the process ends.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	followed := make(chan attached, 1)
	go func() {
		a, _ := attach(t, h, ctx, in, running, 0, true)
		followed <- a
	}()
	list, err := h.Provider.ListProcesses(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != done || list[0].Exit == nil || list[0].Exit.Code != 3 ||
		list[1].ID != running || list[1].Exit != nil || list[1].Name != "server" || strings.Join(list[1].Command, " ") != "sleep 3600" {
		t.Errorf("listed %+v", list)
	}
	if err := h.Provider.SignalProcess(t.Context(), in, running, provider.SignalTerm); err != nil {
		t.Fatal(err)
	}
	a := <-followed
	if e := a.end.Exit; e == nil || e.Signal != provider.SignalTerm || e.Code != 128+15 {
		t.Errorf("a follow of a process sent TERM ended %+v, want its exit by TERM", a.end)
	}
	if err := h.Provider.SignalProcess(t.Context(), in, running, provider.SignalKill); err != nil {
		t.Errorf("signalling a process that ended: %v, want no error", err)
	}
}

func testProcessesDieWithTheirInstance(t *testing.T, h Harness) {
	in, _ := h.start(t, "proc-die", h.container("net-a", "proc-die"))
	id := start(t, h, in, "", "sleep", "3600")
	h.Stop(t, in)
	list, err := h.Provider.ListProcesses(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != id || list[0].Exit == nil {
		t.Errorf("after its instance stopped, the process: %+v", list)
	}
	if _, err := h.Provider.StartProcess(t.Context(), provider.ProcessRequest{Instance: in, Command: []string{"true"}}); err == nil {
		t.Error("a process started in a stopped instance")
	}
}

func testProcessLimits(t *testing.T, h Harness) {
	if h.ProcessLimits == nil {
		t.Skip("the harness cannot set the provider's process limits")
	}
	h.ProcessLimits(16, 2)
	defer h.ProcessLimits(provider.DefaultProcessLogCap, provider.DefaultFinishedProcesses)
	in, _ := h.start(t, "proc-limits", h.container("net-a", "proc-limits"))
	id := start(t, h, in, "", "emit", "out:0123456789", "out:abcdefghij", "out:ABCDEFGHIJ")
	waitEnded(t, h, in, id)
	a, err := attach(t, h, t.Context(), in, id, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	// 33 bytes written, the last 16 held.
	if a.end.HeldFrom != 17 || a.end.Next != 33 || a.text != "ghij\nABCDEFGHIJ\n" || a.offsets[0] != 17 {
		t.Errorf("a log over its cap: %q from %v, ended %+v", a.text, a.offsets, a.end)
	}
	for range 3 {
		waitEnded(t, h, in, start(t, h, in, "", "true"))
	}
	list, err := h.Provider.ListProcesses(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID == id {
		t.Errorf("with 2 finished processes kept, listed %+v", list)
	}
}
