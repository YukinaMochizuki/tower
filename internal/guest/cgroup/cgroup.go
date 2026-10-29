// Package cgroup is the cgroup tower-guest runs one process's command in,
// on a guest whose cgroup2 it may write (hangar's containers): made under
// the cgroup2 root, bounded in memory so an OOM kills that command's group
// alone, read for its usage once the command ends, and removed. It is no
// general cgroup library: it writes and reads what that needs and nothing
// else. docs/decisions/process-usage.md says why the root and why hangar's
// containers alone.
package cgroup

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Root is where a guest mounts cgroup2.
const Root = "/sys/fs/cgroup"

// cgroup2Magic is cgroup2's statfs f_type (CGROUP2_SUPER_MAGIC).
const cgroup2Magic = 0x63677270

// Available reports whether root is a cgroup2 mount. On a cgroup v1 guest
// /sys/fs/cgroup is a tmpfs, where Make would make a plain directory.
func Available(root string) bool {
	var st syscall.Statfs_t
	return syscall.Statfs(root, &st) == nil && st.Type == cgroup2Magic
}

// Group is a cgroup made for one command.
type Group struct {
	path string
	dir  *os.File
}

// Make makes the cgroup name under root, which kills its processes
// together on an OOM (memory.oom.group) and, with memoryMax above 0, holds
// them to memoryMax bytes. A file the kernel does not offer, as when the
// memory controller is not enabled for root's children, fails it, and
// what it made is removed.
func Make(root, name string, memoryMax int64) (*Group, error) {
	path := filepath.Join(root, name)
	if err := os.Mkdir(path, 0o755); err != nil {
		return nil, err
	}
	err := writeFile(path, "memory.oom.group", "1")
	if err == nil && memoryMax > 0 {
		err = writeFile(path, "memory.max", strconv.FormatInt(memoryMax, 10))
	}
	var dir *os.File
	if err == nil {
		dir, err = os.Open(path)
	}
	if err != nil {
		_ = syscall.Rmdir(path)
		return nil, fmt.Errorf("cgroup %s: %w", name, err)
	}
	return &Group{path: path, dir: dir}, nil
}

// writeFile writes an interface file of a cgroup, as a shell's > does. On
// cgroupfs, O_CREAT opens an interface file that is there and fails on one
// that is not.
var writeFile = func(dir, name, value string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(value), 0o644)
}

// FD is the cgroup's directory, for SysProcAttr.CgroupFD: a command
// started into it (clone3's CLONE_INTO_CGROUP) is in it from its first
// instruction, so no child of it escapes before a move.
func (g *Group) FD() int { return int(g.dir.Fd()) }

// Usage is what a cgroup's processes used. A field is nil when the kernel
// does not offer the file it is read from: memory.peak came in Linux 5.19,
// and cpu.pressure needs PSI.
type Usage struct {
	MemoryPeak *int64
	OOMKilled  *bool
	CPUUsage   *time.Duration
	CPUWait    *time.Duration
}

// Usage reads what the cgroup's processes used, those gone included.
func (g *Group) Usage() Usage {
	var u Usage
	if b, err := os.ReadFile(filepath.Join(g.path, "memory.peak")); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			u.MemoryPeak = &n
		}
	}
	if b, err := os.ReadFile(filepath.Join(g.path, "memory.events")); err == nil {
		if n, ok := field(b, "", "oom_kill"); ok {
			killed := n > 0
			u.OOMKilled = &killed
		}
	}
	if b, err := os.ReadFile(filepath.Join(g.path, "cpu.stat")); err == nil {
		if n, ok := field(b, "", "usage_usec"); ok {
			d := time.Duration(n) * time.Microsecond
			u.CPUUsage = &d
		}
	}
	if b, err := os.ReadFile(filepath.Join(g.path, "cpu.pressure")); err == nil {
		if n, ok := field(b, "some", "total"); ok {
			d := time.Duration(n) * time.Microsecond
			u.CPUWait = &d
		}
	}
	return u
}

// field reads a counter of a cgroup file: the value of key on the line
// "key value" (flat keyed, as memory.events and cpu.stat are), or, with
// line set, of key=value on the line starting with line (nested keyed, as
// cpu.pressure is).
func field(b []byte, line, key string) (int64, bool) {
	s := bufio.NewScanner(bytes.NewReader(b))
	for s.Scan() {
		words := strings.Fields(s.Text())
		if len(words) == 0 {
			continue
		}
		var v string
		switch {
		case line == "" && len(words) == 2 && words[0] == key:
			v = words[1]
		case line != "" && words[0] == line:
			for _, w := range words[1:] {
				if k, val, ok := strings.Cut(w, "="); ok && k == key {
					v = val
				}
			}
		}
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// moveTries bounds the moves of what is left in a cgroup: a process can
// fork as the others are moved.
const moveTries = 3

// Remove removes the cgroup. What the command left running in it, a
// child it put in the background, is moved first to the cgroup to (the
// supervisor's own, where it would have run without this one) and goes on;
// with to empty, or a child forking faster than it is moved, the cgroup
// stays.
func (g *Group) Remove(to string) error {
	_ = g.dir.Close()
	for range moveTries {
		b, err := os.ReadFile(filepath.Join(g.path, "cgroup.procs"))
		if err != nil || len(bytes.TrimSpace(b)) == 0 || to == "" {
			break
		}
		for _, pid := range strings.Fields(string(b)) {
			// One pid a write, as cgroup.procs takes them; one gone since
			// is no error of the move's.
			_ = appendFile(filepath.Join(to, "cgroup.procs"), pid)
		}
	}
	if err := syscall.Rmdir(g.path); err != nil {
		return fmt.Errorf("removing cgroup %s: %w", filepath.Base(g.path), err)
	}
	return nil
}

func appendFile(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(value)
	return errors.Join(err, f.Close())
}

// Self is the cgroup the calling process is in, as a path under root, from
// /proc/self/cgroup's cgroup2 line ("0::/workload").
func Self(root string) (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	return selfFrom(root, b)
}

func selfFrom(root string, b []byte) (string, error) {
	for line := range strings.SplitSeq(string(b), "\n") {
		if rel, ok := strings.CutPrefix(line, "0::"); ok {
			if !strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
				break
			}
			return filepath.Join(root, rel), nil
		}
	}
	return "", errors.New("cgroup: no cgroup2 line in /proc/self/cgroup")
}
