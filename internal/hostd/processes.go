package hostd

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// Processes run in containers through the container driver, which keeps
// each one's log and exit on the host; hostd keeps nothing of them. A VM's
// are not run: tower runs none in machines.

// processLimits are what a process is started with: its log's cap and how
// many finished processes its instance keeps.
type processLimits struct {
	logCap int64
	keep   int
}

// SetProcessLimits sets the cap of the next processes' logs and how many
// finished processes an instance keeps; zero leaves one as it is.
func (h *Host) SetProcessLimits(logCap int64, finished int) {
	cur := h.processLimits()
	if logCap > 0 {
		cur.logCap = logCap
	}
	if finished > 0 {
		cur.keep = finished
	}
	h.limits.Store(&cur)
}

func (h *Host) processLimits() processLimits {
	if l := h.limits.Load(); l != nil {
		return *l
	}
	return processLimits{logCap: provider.DefaultProcessLogCap, keep: provider.DefaultFinishedProcesses}
}

// processes is the container driver for an instance's processes; a VM's
// are Unimplemented.
func processes(d driver.Drivers, p placed) (driver.Processes, error) {
	if p.in.GetSpec().GetKind() == envv1alpha1.InstanceKind_INSTANCE_KIND_VM {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("instance %s/%s is a VM, which runs no processes", p.ref.Project, p.ref.Name))
	}
	return d.Containers, nil
}

// changed reads the instance again, whose processes_running may have
// moved.
func (h *Host) changed(ref provider.Instance) {
	h.ctrl.Enqueue(controller.Key{Project: ref.Project, Name: ref.Name})
}

func (h *Host) StartProcess(ctx context.Context, req provider.ProcessRequest) (string, error) {
	p, err := h.running(ctx, req.Instance)
	if err != nil {
		return "", err
	}
	procs, err := processes(h.d, p)
	if err != nil {
		return "", err
	}
	l := h.processLimits()
	id, err := procs.StartProcess(ctx, p.name, driver.ProcessSpec{
		Name: req.Name, Command: req.Command, Env: req.Env, Dir: req.Dir, LogCap: l.logCap, Keep: l.keep,
	})
	if err != nil {
		return "", providerError(err)
	}
	h.changed(req.Instance)
	return id, nil
}

// placedProcesses finds an instance whose guest is on the host, running or
// not, and the driver of its processes.
func (h *Host) placedProcesses(ctx context.Context, in provider.Instance) (placed, driver.Processes, error) {
	p, err := h.find(ctx, in)
	if err != nil {
		return p, nil, err
	}
	procs, err := processes(h.d, p)
	return p, procs, err
}

func (h *Host) AttachProcess(ctx context.Context, in provider.Instance, id string, from int64, follow bool, out func(provider.OutputChunk) error) (provider.Attached, error) {
	p, procs, err := h.placedProcesses(ctx, in)
	if err != nil {
		return provider.Attached{}, err
	}
	var outErr error
	end, err := procs.ReadProcessLog(ctx, p.name, id, from, follow, func(c driver.LogChunk) error {
		stream := provider.Stdout
		if c.Stderr {
			stream = provider.Stderr
		}
		outErr = out(provider.OutputChunk{Stream: stream, Offset: c.Offset, Data: c.Data})
		return outErr
	})
	if outErr != nil {
		// The caller's own error, as it gave it.
		return provider.Attached{}, outErr
	}
	if err != nil {
		return provider.Attached{}, providerError(err)
	}
	return provider.Attached{HeldFrom: end.HeldFrom, Next: end.Next, Exit: exitOf(end.Exit)}, nil
}

func (h *Host) SignalProcess(ctx context.Context, in provider.Instance, id string, sig provider.Signal) error {
	p, procs, err := h.placedProcesses(ctx, in)
	if err != nil {
		return err
	}
	if err := procs.SignalProcess(ctx, p.name, id, int(sig)); err != nil {
		return providerError(err)
	}
	h.changed(in)
	return nil
}

func (h *Host) ListProcesses(ctx context.Context, in provider.Instance) ([]provider.Process, error) {
	p, procs, err := h.placedProcesses(ctx, in)
	if err != nil {
		return nil, err
	}
	list, err := procs.ListProcesses(ctx, p.name)
	if err != nil {
		return nil, providerError(err)
	}
	out := make([]provider.Process, 0, len(list))
	for _, pr := range list {
		out = append(out, provider.Process{ID: pr.ID, Name: pr.Name, Command: slices.Clone(pr.Command), Started: pr.Started, Exit: exitOf(pr.Exit)})
	}
	return out, nil
}

func exitOf(e *driver.ProcessExit) *provider.ProcessExit {
	if e == nil {
		return nil
	}
	return &provider.ProcessExit{Code: e.Code, Signal: provider.Signal(e.Signal)}
}

// processesRunning counts a container's running processes; errors.Is
// driver.ErrNotFound for one not on the host.
func (h *Host) processesRunning(ctx context.Context, guest string) (int32, error) {
	list, err := h.d.Containers.ListProcesses(ctx, guest)
	if err != nil && !errors.Is(err, driver.ErrNotFound) {
		return 0, err
	}
	var n int32
	for _, p := range list {
		if p.Exit == nil {
			n++
		}
	}
	return n, nil
}
