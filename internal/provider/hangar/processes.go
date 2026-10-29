package hangar

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/guest/guestops"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// Processes run in a container or VM under tower-guest's supervisor,
// which keeps each one's log and exit in the machine (procRoot, or
// vmssh.ProcRoot) and outlives the exec or SSH session that started it:
// hangar ends an exec's own process group as the exec ends, and the
// supervisor is in a session of its own. Unlike
// hostd's, a container's processes are read in it alone, so only while it
// runs: a stopped one's list is what the provider last listed of it, its
// running processes then ended, killed (tower-guest reads them as such
// again once it runs), which a provider made anew since does not hold; and
// a stopped one's logs are read once it runs again.

// processLimits are what a process is started with: its log's cap and how
// many finished processes its instance keeps.
type processLimits struct {
	mu     sync.Mutex
	logCap int64
	keep   int
}

// SetProcessLimits sets the cap of the next processes' logs and how many
// finished processes an instance keeps; zero leaves one as it is.
func (t *Target) SetProcessLimits(logCap int64, finished int) {
	t.limits.mu.Lock()
	defer t.limits.mu.Unlock()
	if logCap > 0 {
		t.limits.logCap = logCap
	}
	if finished > 0 {
		t.limits.keep = finished
	}
}

func (t *Target) processLimits() (int64, int) {
	t.limits.mu.Lock()
	defer t.limits.mu.Unlock()
	logCap, keep := t.limits.logCap, t.limits.keep
	if logCap == 0 {
		logCap = provider.DefaultProcessLogCap
	}
	if keep == 0 {
		keep = provider.DefaultFinishedProcesses
	}
	return logCap, keep
}

// changed reads the instance again, whose processes_running may have
// moved.
func (t *Target) changed(ref provider.Instance) {
	t.ctrl.Enqueue(controller.Key{Project: ref.Project, Name: ref.Name})
}

func (t *Target) StartProcess(ctx context.Context, req provider.ProcessRequest) (string, error) {
	p, o, err := t.running(ctx, req.Instance)
	if err != nil {
		return "", err
	}
	logCap, keep := t.processLimits()
	spec := guest.ProcessSpec{Name: req.Name, Command: req.Command, Env: req.Env, Dir: req.Dir, LogCap: logCap, Keep: keep}
	spec.Measure, spec.MemoryMax = measure(p)
	id, err := o.StartProcess(ctx, spec)
	if err != nil {
		return "", guestError(err)
	}
	t.changed(req.Instance)
	return id, nil
}

func (t *Target) AttachProcess(ctx context.Context, in provider.Instance, id string, from int64, follow bool, out func(provider.OutputChunk) error) (provider.Attached, error) {
	_, o, err := t.running(ctx, in)
	if err != nil {
		return provider.Attached{}, err
	}
	var outErr error
	end, err := o.ReadProcessLog(ctx, id, from, follow, func(c guest.LogChunk) error {
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
		return provider.Attached{}, guestError(err)
	}
	return provider.Attached{HeldFrom: end.HeldFrom, Next: end.Next, Exit: exitOf(end.Exit)}, nil
}

func (t *Target) SignalProcess(ctx context.Context, in provider.Instance, id string, sig provider.Signal) error {
	_, o, err := t.running(ctx, in)
	if errors.Is(err, errNotRunning) {
		// Its processes ended with it.
		return nil
	}
	if err != nil {
		return err
	}
	if err := o.SignalProcess(ctx, id, int(sig)); err != nil {
		return guestError(err)
	}
	t.changed(in)
	return nil
}

func (t *Target) ListProcesses(ctx context.Context, in provider.Instance) ([]provider.Process, error) {
	p, o, err := t.running(ctx, in)
	if errors.Is(err, errNotRunning) {
		if list, ok := t.procs.lastListed(p.uid); ok && p.stopped {
			// It stops, or stopped: what ran ended with it.
			for i := range list {
				if list[i].Exit == nil {
					list[i].Exit = provider.Killed(provider.SignalKill)
				}
			}
			return list, nil
		}
		return nil, fmt.Errorf("%w; its processes are read once it runs", err)
	}
	if err != nil {
		return nil, err
	}
	return t.listProcesses(ctx, p, o)
}

// listProcesses lists a running container's processes, and keeps them for
// once it stopped.
func (t *Target) listProcesses(ctx context.Context, p placed, o processLister) ([]provider.Process, error) {
	list, err := o.ListProcesses(ctx)
	if err != nil {
		return nil, guestError(err)
	}
	out := make([]provider.Process, 0, len(list))
	for _, pr := range list {
		out = append(out, provider.Process{ID: pr.ID, Name: pr.Name, Command: slices.Clone(pr.Command), Dir: pr.Dir, Started: pr.Started, Exit: exitOf(pr.Exit)})
	}
	t.procs.listed(p.uid, slices.Clone(out))
	return out, nil
}

// processLister is what lists a guest's processes: guestops.Ops.
type processLister interface {
	ListProcesses(ctx context.Context) ([]guest.ProcessInfo, error)
}

func exitOf(e *guest.ProcessExit) *provider.ProcessExit {
	if e == nil {
		return nil
	}
	return &provider.ProcessExit{Code: e.Code, Signal: provider.Signal(e.Signal), Usage: (*provider.ProcessUsage)(e.Usage)}
}

// measure says whether a process of p is measured, and the memory it is
// held to: in a container, whose guest's cgroup2 tower-guest may write,
// below the instance's memory (provider.ProcessMemoryMax). A VM's are
// not: under systemd, a cgroup beside systemd's own wants a decision of
// its own (docs/decisions/process-usage.md).
func measure(p placed) (bool, int64) {
	if p.vm {
		return false, 0
	}
	memory, err := quantity.Bytes(p.in.GetSpec().GetResources().GetMemory())
	if err != nil || memory == 0 || memory > math.MaxInt64 {
		// Measured, unbounded: an instance tower made has its memory.
		return true, 0
	}
	return true, provider.ProcessMemoryMax(int64(memory))
}

// countTimeout bounds counting a machine's processes.
const countTimeout = 15 * time.Second

// processesRunning counts a running machine's running processes, for its
// status.
// It runs in a reconcile, so it writes nothing in, and is bounded:
// nothing a machine does holds up its target's other instances. A VM is
// asked over a connection the provider holds already, never one it makes:
// connecting waits for the VM's sshd, which a reconcile, holding the
// target's lock, must not. Without one, or with it not answering within
// countTimeout, its count is the last known (InstanceStatus's
// processes_running). One tower never reached (whose host key no Instance
// pins) runs nothing of tower's yet.
func (t *Target) processesRunning(ctx context.Context, ref provider.Instance, in *envv1alpha1.Instance, obj machineObj, m *hangarcompute.Machine) (int32, error) {
	ctx, cancel := context.WithTimeout(ctx, countTimeout)
	defer cancel()
	p := placed{ref: ref, in: in, name: obj.name, uid: obj.uid, vm: obj.vm}
	var o guestops.Ops
	var err error
	switch {
	case !obj.vm:
		o, err = t.opsIfInstalled(ctx, obj.name, obj.uid)
	case in.GetStatus().GetSshHostKey() == "":
		err = errNoGuest
	default:
		o, err = t.vmOps(ctx, p, m, vmDial{reconcile: true})
	}
	last := in.GetStatus().GetProcessesRunning()
	switch {
	case errors.Is(err, errNoGuest):
		return 0, nil
	case errors.Is(err, errUnreached):
		return last, nil
	case err != nil:
		return 0, err
	}
	list, err := t.listProcesses(ctx, p, o)
	if err != nil && obj.vm {
		// What runs in it is not known now; it was as last reported.
		return last, err
	}
	if err != nil {
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
