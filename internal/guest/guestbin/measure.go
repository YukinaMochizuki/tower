package guestbin

import (
	"os"
	"os/exec"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest/cgroup"
	"github.com/YukinaMochizuki/tower/internal/guest/proclog"
)

// Where a measured command's cgroup is made, and whether one can be: a
// test's directory stands in for cgroupfs.
var (
	cgroupRoot      = cgroup.Root
	cgroupAvailable = cgroup.Available
)

// measured is the cgroup a command runs in, and the supervisor's own,
// which what the command leaves running goes back to.
type measured struct {
	g    *cgroup.Group
	self string
}

// startMeasured starts newCmd's command, with h.Measure in a cgroup of its
// own under /tower (docs/decisions/process-usage.md). Where none can be
// made, or the start into it fails, the command starts unmeasured.
func startMeasured(newCmd func() *exec.Cmd, h Header, id string) (*exec.Cmd, *measured, error) {
	var m *measured
	if h.Measure && cgroupAvailable(cgroupRoot) {
		if parent, err := cgroup.EnsureParent(cgroupRoot, h.MemoryMax); err == nil {
			self, _ := cgroup.Self(cgroupRoot)
			cgroup.Sweep(parent, self)
			if g, err := cgroup.Make(parent, cgroup.Name(id, os.Getpid())); err == nil {
				m = &measured{g: g, self: self}
			}
		}
	}
	if m != nil {
		cmd := newCmd()
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = true, m.g.FD()
		if err := cmd.Start(); err == nil {
			return cmd, m, nil
		}
		m.remove()
	}
	cmd := newCmd()
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return cmd, nil, nil
}

// usage is what the command used, as its exit records it.
func (m *measured) usage(wall time.Duration) *proclog.Usage {
	u := m.g.Usage()
	return &proclog.Usage{MemoryPeak: u.MemoryPeak, OOMKilled: u.OOMKilled, CPUUsage: u.CPUUsage, CPUWait: u.CPUWait, Wall: wall}
}

// remove removes the cgroup; one that stays, holding a child that would
// not move, is swept as a later process starts.
func (m *measured) remove() { _ = m.g.Remove(m.self) }
