package guestops_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"slices"
	"sync"
	"syscall"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
)

// counted runs tower-guest shim here, as local does, keeping the ops it
// ran.
type counted struct {
	mu  *sync.Mutex
	ops *[]string
}

func (c counted) RunShim(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	pr, pw := io.Pipe()
	in := bufio.NewReader(stdin)
	typ, data, err := guestbin.ReadFrame(in)
	if err != nil || typ != guestbin.FrameHeader {
		return 0, err
	}
	var h struct{ Op string }
	_ = json.Unmarshal(data, &h)
	c.mu.Lock()
	*c.ops = append(*c.ops, h.Op)
	c.mu.Unlock()
	go func() {
		err := guestbin.WriteFrame(pw, typ, data)
		if err == nil {
			_, err = io.Copy(pw, in)
		}
		pw.CloseWithError(err)
	}()
	return local{}.RunShim(ctx, pr, stdout, stderr)
}

// TestOnlyAStartRunsOnStartR: an Ops with a runner of its starts runs
// starts on it alone, naming its user; a process's signal, list and log
// run on R, as every other operation does, so a root StartR reads and
// signals nothing the user could have written.
func TestOnlyAStartRunsOnStartR(t *testing.T) {
	var mu sync.Mutex
	var onR, onStart []string
	o := ops(t)
	o.R, o.StartR = counted{&mu, &onR}, counted{&mu, &onStart}
	id := startProc(t, o, guest.ProcessSpec{Command: []string{"sleep", "60"}, LogCap: 1 << 20})
	if _, err := o.ListProcesses(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := o.ReadProcessLog(t.Context(), id, 0, false, func(guest.LogChunk) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := o.SignalProcess(t.Context(), id, int(syscall.SIGKILL)); err != nil {
		t.Fatal(err)
	}
	// Its exit written before the test's directory goes.
	if _, end := follow(t, o, id); end.Exit == nil {
		t.Fatalf("the process signalled ended %+v", end)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(onStart, []string{guestbin.OpStart}) {
		t.Errorf("StartR ran %v, want the start alone", onStart)
	}
	for _, op := range []string{guestbin.OpList, guestbin.OpLog, guestbin.OpSignal} {
		if !slices.Contains(onR, op) {
			t.Errorf("R ran %v, no %s", onR, op)
		}
	}
	if slices.Contains(onR, guestbin.OpStart) {
		t.Errorf("R ran a start: %v", onR)
	}
}
