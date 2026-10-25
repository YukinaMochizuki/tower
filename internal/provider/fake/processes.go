package fake

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/YukinaMochizuki/tower/internal/provider"
)

// process is one process of a fake instance: its log, in memory, and how
// it ended. The fake's commands run when the process starts and end it at
// once, but sleep, which runs until it is signalled or its instance
// stops; a test writes to a process and ends it with WriteProcess and
// EndProcess.
type process struct {
	id, name string
	command  []string
	started  time.Time
	// log holds the output from heldFrom up to next, oldest first.
	log            []provider.OutputChunk
	heldFrom, next int64
	exit           *provider.ProcessExit
	// changed is closed, and made again, at each write and at the end.
	changed chan struct{}
}

func (p *process) notify() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// write appends data to the log, dropping the oldest output past cap. The
// caller holds f.mu.
func (p *process) write(stream provider.OutputStream, data []byte, logCap int64) {
	if len(data) == 0 || p.exit != nil {
		return
	}
	p.log = append(p.log, provider.OutputChunk{Stream: stream, Offset: p.next, Data: slices.Clone(data)})
	p.next += int64(len(data))
	for p.next-p.heldFrom > logCap {
		first := &p.log[0]
		drop := min(int64(len(first.Data)), p.next-p.heldFrom-logCap)
		first.Data, first.Offset = first.Data[drop:], first.Offset+drop
		p.heldFrom += drop
		if len(first.Data) == 0 {
			p.log = p.log[1:]
		}
	}
	p.notify()
}

// end ends the process with exit, if it runs. The caller holds f.mu.
func (p *process) end(exit *provider.ProcessExit) {
	if p.exit != nil {
		return
	}
	p.exit = exit
	p.notify()
}

func (i *instance) processesRunning() int {
	n := 0
	for _, p := range i.processes {
		if p.exit == nil {
			n++
		}
	}
	return n
}

// findProcess returns the instance's process of the id; the instance may
// be stopped. The caller holds f.mu.
func (f *Fake) findProcess(ref provider.Instance, id string) (*process, error) {
	if err := f.answering(); err != nil {
		return nil, err
	}
	local := f.instances[ref]
	if local == nil || f.vanished[local.uid] {
		return nil, fmt.Errorf("instance %s/%s: %w", ref.Project, ref.Name, provider.ErrNotFound)
	}
	for _, p := range local.processes {
		if p.id == id {
			return p, nil
		}
	}
	return nil, fmt.Errorf("process %s of instance %s/%s: %w", id, ref.Project, ref.Name, provider.ErrNotFound)
}

// processWriter writes what a fake command writes to its process's log.
type processWriter struct {
	f      *Fake
	p      *process
	stream provider.OutputStream
}

func (w processWriter) Write(b []byte) (int, error) {
	w.f.mu.Lock()
	defer w.f.mu.Unlock()
	w.p.write(w.stream, b, w.f.processLogCap)
	return len(b), nil
}

func (f *Fake) StartProcess(_ context.Context, req provider.ProcessRequest) (string, error) {
	f.mu.Lock()
	local, err := f.running(req.Instance)
	if err != nil {
		f.mu.Unlock()
		return "", err
	}
	local.processN++
	p := &process{
		id: "p" + strconv.Itoa(local.processN), name: req.Name, command: slices.Clone(req.Command),
		started: f.clock.Now(), changed: make(chan struct{}),
	}
	local.processes = append(local.processes, p)
	f.pruneProcesses(local)
	f.execs = append(f.execs, Exec{Instance: req.Instance, Command: slices.Clone(req.Command), Env: maps.Clone(req.Env), ExitCode: -1, Process: true})
	cmd := command{files: maps.Clone(local.files), failing: slices.Clone(f.failCommands), onExec: f.onExec}
	f.mu.Unlock()
	f.enqueue(req.Instance)
	if len(req.Command) > 0 && req.Command[0] == "sleep" {
		return p.id, nil
	}
	code := cmd.run(provider.ExecRequest{
		Instance: req.Instance, Command: req.Command, Env: req.Env, Dir: req.Dir,
		Stdout: processWriter{f, p, provider.Stdout}, Stderr: processWriter{f, p, provider.Stderr},
	})
	f.mu.Lock()
	p.end(&provider.ProcessExit{Code: code})
	f.pruneProcesses(local)
	f.mu.Unlock()
	f.enqueue(req.Instance)
	return p.id, nil
}

// pruneProcesses drops the oldest finished processes past what an instance
// keeps. The caller holds f.mu.
func (f *Fake) pruneProcesses(local *instance) {
	finished := 0
	for _, p := range local.processes {
		if p.exit != nil {
			finished++
		}
	}
	local.processes = slices.DeleteFunc(local.processes, func(p *process) bool {
		if p.exit != nil && finished > f.finishedProcesses {
			finished--
			return true
		}
		return false
	})
}

func (f *Fake) AttachProcess(ctx context.Context, in provider.Instance, id string, from int64, follow bool, out func(provider.OutputChunk) error) (provider.Attached, error) {
	for {
		f.mu.Lock()
		p, err := f.findProcess(in, id)
		if err != nil {
			f.mu.Unlock()
			return provider.Attached{}, err
		}
		var chunks []provider.OutputChunk
		for _, c := range p.log {
			if end := c.Offset + int64(len(c.Data)); end > from {
				skip := max(0, from-c.Offset)
				chunks = append(chunks, provider.OutputChunk{Stream: c.Stream, Offset: c.Offset + skip, Data: slices.Clone(c.Data[skip:])})
			}
		}
		from = max(from, p.heldFrom)
		next, heldFrom, exit, changed := max(from, p.next), p.heldFrom, p.exit, p.changed
		f.mu.Unlock()
		for _, c := range chunks {
			if err := out(c); err != nil {
				return provider.Attached{}, err
			}
		}
		from = next
		if exit != nil || !follow {
			var e *provider.ProcessExit
			if exit != nil {
				copied := *exit
				e = &copied
			}
			return provider.Attached{HeldFrom: heldFrom, Next: next, Exit: e}, nil
		}
		f.following.Add(1)
		select {
		case <-changed:
		case <-ctx.Done():
			f.following.Add(-1)
			return provider.Attached{}, ctx.Err()
		}
		f.following.Add(-1)
	}
}

func (f *Fake) SignalProcess(_ context.Context, in provider.Instance, id string, sig provider.Signal) error {
	f.mu.Lock()
	p, err := f.findProcess(in, id)
	if err == nil {
		p.end(provider.Killed(sig))
		f.pruneProcesses(f.instances[in])
	}
	f.mu.Unlock()
	if err == nil {
		f.enqueue(in)
	}
	return err
}

func (f *Fake) ListProcesses(_ context.Context, in provider.Instance) ([]provider.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.answering(); err != nil {
		return nil, err
	}
	local := f.instances[in]
	if local == nil || f.vanished[local.uid] {
		return nil, fmt.Errorf("instance %s/%s: %w", in.Project, in.Name, provider.ErrNotFound)
	}
	out := make([]provider.Process, 0, len(local.processes))
	for _, p := range local.processes {
		pr := provider.Process{ID: p.id, Name: p.name, Command: slices.Clone(p.command), Started: p.started}
		if p.exit != nil {
			e := *p.exit
			pr.Exit = &e
		}
		out = append(out, pr)
	}
	return out, nil
}

// The process faults and controls.

// WriteProcess writes text to a running process's stdout or stderr, as the
// process would.
func (f *Fake) WriteProcess(in provider.Instance, id string, stream provider.OutputStream, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, err := f.findProcess(in, id)
	if err != nil {
		return err
	}
	p.write(stream, []byte(text), f.processLogCap)
	return nil
}

// EndProcess ends a running process with code, as one that exits or dies
// does.
func (f *Fake) EndProcess(in provider.Instance, id string, code int) error {
	f.mu.Lock()
	p, err := f.findProcess(in, id)
	if err == nil {
		p.end(&provider.ProcessExit{Code: code})
		f.pruneProcesses(f.instances[in])
	}
	f.mu.Unlock()
	if err == nil {
		f.enqueue(in)
	}
	return err
}

// SetStreams sets how many direct streams are open to an instance, as
// clients connecting and leaving on their tickets do.
func (f *Fake) SetStreams(in provider.Instance, n int) error {
	f.mu.Lock()
	local := f.instances[in]
	if local != nil {
		local.streams = n
	}
	f.mu.Unlock()
	if local == nil {
		return fmt.Errorf("instance %s/%s: %w", in.Project, in.Name, provider.ErrNotFound)
	}
	f.enqueue(in)
	return nil
}

// Following counts the attaches waiting for a process to write or end.
func (f *Fake) Following() int { return int(f.following.Load()) }

// SetProcessLimits sets the cap of a process's log and how many finished
// processes an instance keeps; zero leaves one as it is.
func (f *Fake) SetProcessLimits(logCap int64, finished int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if logCap > 0 {
		f.processLogCap = logCap
	}
	if finished > 0 {
		f.finishedProcesses = finished
	}
}
