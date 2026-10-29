package guestbin

import (
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
// own named name, directly under the cgroup2 root: under the supervisor's
// own (hangar's /workload) none can be made, as a cgroup holding processes
// enables no controller for its children. A cgroup that cannot be made, or
// a start into it that fails, starts the command as it would be without
// one: measuring never keeps a process from running. The cgroup is nil
// then.
func startMeasured(newCmd func() *exec.Cmd, h Header, name string) (*exec.Cmd, *measured, error) {
	var m *measured
	if h.Measure && cgroupAvailable(cgroupRoot) {
		if g, err := cgroup.Make(cgroupRoot, "tower-"+name, h.MemoryMax); err == nil {
			self, _ := cgroup.Self(cgroupRoot)
			m = &measured{g: g, self: self}
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
// not move, costs the guest an empty directory and nothing of the
// process's.
func (m *measured) remove() { _ = m.g.Remove(m.self) }
