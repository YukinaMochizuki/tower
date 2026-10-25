package fake

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// proc is a process of a fake guest as the host keeps it, in the state
// file: never its environment. Its command runs when it starts and ends
// it at once, but sleep, which runs until it is signalled or its guest
// stops; serve ARGS..., which writes its arguments as a line and then
// runs as sleep does; tick, which writes a line every few milliseconds
// (while this Host, not one made again from its file, runs); and spawn,
// which starts a child in its group and runs as sleep does: a signal ends
// the child with it, and the guest's children command counts the children
// alive.
type proc struct {
	ID, Name string
	Command  []string
	Started  time.Time
	Exit     *driver.ProcessExit
	// Log holds the output from HeldFrom up to Next, oldest first.
	Log            []logChunk
	HeldFrom, Next int64
	LogCap         int64
	// Children are the children alive in its group.
	Children int
}

type logChunk struct {
	Stderr bool
	Offset int64
	Data   []byte
}

// write appends data to the log, dropping the oldest output past its cap.
func (p *proc) write(stderr bool, data []byte) {
	if len(data) == 0 || p.Exit != nil {
		return
	}
	p.Log = append(p.Log, logChunk{Stderr: stderr, Offset: p.Next, Data: slices.Clone(data)})
	p.Next += int64(len(data))
	for p.Next-p.HeldFrom > p.LogCap {
		first := &p.Log[0]
		drop := min(int64(len(first.Data)), p.Next-p.HeldFrom-p.LogCap)
		first.Data, first.Offset = first.Data[drop:], first.Offset+drop
		p.HeldFrom += drop
		if len(first.Data) == 0 {
			p.Log = p.Log[1:]
		}
	}
}

// changedLocked is closed, and made again, when the process writes or
// ends. The caller holds h.mu.
func (h *Host) changedLocked(key string) chan struct{} {
	if h.changed == nil {
		h.changed = map[string]chan struct{}{}
	}
	if h.changed[key] == nil {
		h.changed[key] = make(chan struct{})
	}
	return h.changed[key]
}

func (h *Host) notifyLocked(key string) {
	if ch := h.changed[key]; ch != nil {
		close(ch)
		delete(h.changed, key)
	}
}

// endProcs ends the guest's running processes by sig. The caller holds
// h.mu.
func (g guests) endProcs(gu *guest, sig syscall.Signal) {
	// endProc prunes gu.Procs as it goes.
	for _, p := range slices.Clone(gu.Procs) {
		p.Children = 0
		g.endProc(gu, p, &driver.ProcessExit{Code: 128 + int(sig), Signal: int(sig)})
	}
}

// endProc ends a running process and prunes the finished ones past what
// the guest keeps. The caller holds h.mu.
func (g guests) endProc(gu *guest, p *proc, exit *driver.ProcessExit) {
	if p.Exit != nil {
		return
	}
	p.Exit = exit
	g.h.notifyLocked(g.key(gu.Spec.Name) + "/" + p.ID)
	finished := 0
	for _, q := range gu.Procs {
		if q.Exit != nil {
			finished++
		}
	}
	gu.Procs = slices.DeleteFunc(gu.Procs, func(q *proc) bool {
		if q.Exit != nil && finished > gu.Keep {
			finished--
			return true
		}
		return false
	})
}

func (g guests) StartProcess(_ context.Context, name string, spec driver.ProcessSpec) (string, error) {
	var id string
	ticks := false
	err := g.h.change(g.op("StartProcess"), func() error {
		gu, d, err := g.running(name)
		if err != nil {
			return err
		}
		if spec.LogCap <= 0 || spec.Keep <= 0 {
			return fmt.Errorf("a process needs a log cap and a number of finished processes to keep, not %d and %d", spec.LogCap, spec.Keep)
		}
		gu.ProcN++
		gu.Keep = spec.Keep
		p := &proc{ID: "p" + strconv.Itoa(gu.ProcN), Name: spec.Name, Command: slices.Clone(spec.Command), Started: g.h.clock.Now(), LogCap: spec.LogCap}
		gu.Procs = append(gu.Procs, p)
		id = p.ID
		g.h.execs = append(g.h.execs, Exec{Guest: name, Command: slices.Clone(spec.Command), Env: maps.Clone(spec.Env), ExitCode: -1, Process: true})
		if len(spec.Command) > 0 && spec.Command[0] == "sleep" {
			return nil
		}
		if len(spec.Command) > 0 && spec.Command[0] == "serve" {
			p.write(false, []byte(strings.Join(spec.Command[1:], " ")+"\n"))
			return nil
		}
		if len(spec.Command) > 0 && spec.Command[0] == "tick" {
			ticks = true
			return nil
		}
		if len(spec.Command) > 0 && spec.Command[0] == "spawn" {
			p.Children = 1
			return nil
		}
		var out output
		code := shell{g: g, gu: gu, d: d, env: spec.Env, dir: spec.Dir, out: &out}.run(spec.Command)
		for _, c := range out.chunks {
			p.write(c.stderr, c.b)
		}
		g.endProc(gu, p, &driver.ProcessExit{Code: code})
		return nil
	})
	if err == nil && ticks {
		go g.tick(name, id)
	}
	return id, err
}

// tick writes a line to a tick process until it ends.
func (g guests) tick(name, id string) {
	t := g.h.clock.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for n := 1; ; n++ {
		<-t.Chan()
		done := true
		_ = g.h.change("", func() error {
			_, p, err := g.proc(name, id)
			if err != nil || p.Exit != nil {
				return err
			}
			done = false
			p.write(false, []byte("tick "+strconv.Itoa(n)+"\n"))
			g.h.notifyLocked(g.key(name) + "/" + id)
			return nil
		})
		if done {
			return
		}
	}
}

func (g guests) ListProcesses(_ context.Context, name string) ([]driver.ProcessInfo, error) {
	var out []driver.ProcessInfo
	err := g.h.read(g.op("ListProcesses"), func() error {
		gu, err := g.get(name)
		if err != nil {
			return err
		}
		for _, p := range gu.Procs {
			info := driver.ProcessInfo{ID: p.ID, Name: p.Name, Command: slices.Clone(p.Command), Started: p.Started}
			if p.Exit != nil {
				e := *p.Exit
				info.Exit = &e
			}
			out = append(out, info)
		}
		return nil
	})
	return out, err
}

// proc finds a process of a guest, running or not. The caller holds h.mu.
func (g guests) proc(name, id string) (*guest, *proc, error) {
	gu, err := g.get(name)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range gu.Procs {
		if p.ID == id {
			return gu, p, nil
		}
	}
	return nil, nil, fmt.Errorf("process %s of %s %s: %w", id, g.kind, name, driver.ErrNotFound)
}

func (g guests) ReadProcessLog(ctx context.Context, name, id string, from int64, follow bool, out func(driver.LogChunk) error) (driver.LogEnd, error) {
	for {
		var chunks []driver.LogChunk
		var end driver.LogEnd
		var changed chan struct{}
		err := g.h.read(g.op("ReadProcessLog"), func() error {
			_, p, err := g.proc(name, id)
			if err != nil {
				return err
			}
			for _, c := range p.Log {
				if c.Offset+int64(len(c.Data)) > from {
					skip := max(0, from-c.Offset)
					chunks = append(chunks, driver.LogChunk{Stderr: c.Stderr, Offset: c.Offset + skip, Data: slices.Clone(c.Data[skip:])})
				}
			}
			from = max(from, p.HeldFrom)
			end = driver.LogEnd{HeldFrom: p.HeldFrom, Next: max(from, p.Next)}
			if p.Exit != nil {
				e := *p.Exit
				end.Exit = &e
			}
			changed = g.h.changedLocked(g.key(name) + "/" + id)
			return nil
		})
		if err != nil {
			return driver.LogEnd{}, err
		}
		for _, c := range chunks {
			if err := out(c); err != nil {
				return driver.LogEnd{}, err
			}
		}
		from = end.Next
		if end.Exit != nil || !follow {
			return end, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return driver.LogEnd{}, ctx.Err()
		}
	}
}

func (g guests) SignalProcess(_ context.Context, name, id string, sig int) error {
	return g.h.change(g.op("SignalProcess"), func() error {
		gu, p, err := g.proc(name, id)
		if err != nil {
			return err
		}
		if p.Exit == nil {
			// The signal reaches its whole group.
			p.Children = 0
		}
		g.endProc(gu, p, &driver.ProcessExit{Code: 128 + sig, Signal: sig})
		return nil
	})
}
