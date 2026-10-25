package launcher

// From hangar's internal/node/driver/chvm/caps.go at
// cfb94f5e32b405b212b21099a95b3413d159ba1a, unchanged but for its package:
// the launcher gives up root's capabilities once Cloud Hypervisor runs, as
// hangar's does.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// launcherKeeps are the capabilities the launcher keeps once Cloud
// Hypervisor runs: to reach its socket, the VM user's (CAP_DAC_OVERRIDE),
// and to signal it (CAP_KILL).
var launcherKeeps = []uintptr{unix.CAP_DAC_OVERRIDE, unix.CAP_KILL}

func capMask(caps []uintptr) uint64 {
	var m uint64
	for _, c := range caps {
		m |= 1 << c
	}
	return m
}

// dropCaps leaves the launcher, every thread of it, with keep alone in its
// bounding, permitted and effective sets, and checks that it does. The sets
// are each thread's: hence AllThreadsSyscall, which a CGO_ENABLED=0 build has.
func dropCaps(keep []uintptr) error {
	mask := capMask(keep)
	for c := uintptr(0); ; c++ {
		// Read first: past the last capability the kernel has is EINVAL,
		// which AllThreadsSyscall must not see on one thread only.
		if _, err := unix.PrctlRetInt(unix.PR_CAPBSET_READ, c, 0, 0, 0); errors.Is(err, unix.EINVAL) {
			break
		}
		if mask&(1<<c) != 0 {
			continue
		}
		if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, unix.PR_CAPBSET_DROP, c, 0); e != 0 {
			return fmt.Errorf("dropping capability %d from the bounding set: %w", c, e)
		}
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{
		{Effective: uint32(mask), Permitted: uint32(mask)},
		{Effective: uint32(mask >> 32), Permitted: uint32(mask >> 32)},
	}
	if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return fmt.Errorf("capset: %w", e)
	}
	return checkCaps(mask)
}

// checkCaps checks every thread of the process has mask, and nothing else,
// as its bounding, permitted and effective sets, and no inheritable or
// ambient capability.
func checkCaps(mask uint64) error {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return err
	}
	want := map[string]uint64{"CapBnd": mask, "CapPrm": mask, "CapEff": mask, "CapInh": 0, "CapAmb": 0}
	for _, t := range tasks {
		b, err := os.ReadFile(filepath.Join("/proc/self/task", t.Name(), "status"))
		if errors.Is(err, os.ErrNotExist) {
			continue // the thread has exited
		}
		if err != nil {
			return err
		}
		seen := 0
		for _, line := range strings.Split(string(b), "\n") {
			key, value, _ := strings.Cut(line, ":")
			w, ok := want[key]
			if !ok {
				continue
			}
			seen++
			got, err := strconv.ParseUint(strings.TrimSpace(value), 16, 64)
			if err != nil || got != w {
				return fmt.Errorf("thread %s: %s is %s, want %016x", t.Name(), key, strings.TrimSpace(value), w)
			}
		}
		if seen != len(want) {
			return fmt.Errorf("thread %s: no capability lines in its status", t.Name())
		}
	}
	return nil
}
