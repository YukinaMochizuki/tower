package chvm

// UserRange, checkFree, readLines and check are hangar's
// (internal/node/driver/chvm/users.go at
// cfb94f5e32b405b212b21099a95b3413d159ba1a), its range's name hostd's;
// hostd keeps each VM's id in the VM's directory, where hangar keeps it in
// its launch spec.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// UserRange is a range of ids, uids and the gids of the same numbers:
// First, and Count of them in all.
type UserRange struct{ First, Count uint32 }

// DefaultVMUsers are the ids the VMs' Cloud Hypervisor runs as unless
// Config.VMUsers says otherwise. No user or group of the host may have one.
var DefaultVMUsers = UserRange{First: 200000, Count: 65536}

func (r UserRange) has(id uint32) bool { return id >= r.First && id-r.First < r.Count }

// vmUsersName is the name the VMs' ids may be kept under, in /etc/subuid
// and /etc/subgid, so no user is given them as its own.
const vmUsersName = "tower-vms"

// checkFree refuses a range a user or group in etc has an id in, or that a
// subordinate range in etc overlaps but the VMs' own: a VM could reach
// what is that user's, or a container's.
func (r UserRange) checkFree(etc string) error {
	for _, f := range []struct {
		name   string
		fields []int // the fields holding ids
	}{{"passwd", []int{2, 3}}, {"group", []int{2}}} {
		lines, err := readLines(filepath.Join(etc, f.name))
		if err != nil {
			return err
		}
		for _, l := range lines {
			parts := strings.Split(l, ":")
			for _, i := range f.fields {
				if i >= len(parts) {
					continue
				}
				if id, err := strconv.ParseUint(parts[i], 10, 32); err == nil && r.has(uint32(id)) {
					return fmt.Errorf("%s %s has id %d, one of the VMs' (%d+%d)", f.name, parts[0], id, r.First, r.Count)
				}
			}
		}
	}
	for _, name := range []string{"subuid", "subgid"} {
		lines, err := readLines(filepath.Join(etc, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, l := range lines {
			parts := strings.Split(l, ":")
			if len(parts) != 3 {
				continue
			}
			start, err1 := strconv.ParseUint(parts[1], 10, 32)
			count, err2 := strconv.ParseUint(parts[2], 10, 32)
			if err1 != nil || err2 != nil || parts[0] == vmUsersName {
				continue
			}
			if start < uint64(r.First)+uint64(r.Count) && uint64(r.First) < start+count {
				return fmt.Errorf("%s gives %s %d+%d, which overlaps the VMs' ids (%d+%d)", name, parts[0], start, count, r.First, r.Count)
			}
		}
	}
	return nil
}

// readLines is a file's lines but comments and blank ones.
func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out, nil
}

func (r UserRange) check() error {
	// Below 1000 are the host's own users; past 2^31-1 ids read as
	// negative to much of userspace.
	if r.Count == 0 || r.First < 1000 || uint64(r.First)+uint64(r.Count) > 1<<31 {
		return fmt.Errorf("VM users %d+%d: not a range of ids from 1000 to 2^31", r.First, r.Count)
	}
	return nil
}

// vmUserFile holds a VM's id, in its directory.
const vmUserFile = "vm-user"

// vmUser is the id a VM's Cloud Hypervisor runs as: the one its directory
// holds, else the lowest of the range no other VM's directory holds, which
// it is given. The caller holds d.mu.
func (d *Driver) vmUser(name string) (uint32, error) {
	if b, err := os.ReadFile(filepath.Join(d.dir(name), vmUserFile)); err == nil {
		if id, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32); err == nil && d.cfg.VMUsers.has(uint32(id)) {
			return uint32(id), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	entries, err := os.ReadDir(filepath.Join(d.cfg.StateDir, "vms"))
	if err != nil {
		return 0, err
	}
	held := map[uint32]bool{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(d.cfg.StateDir, "vms", e.Name(), vmUserFile))
		if err != nil || e.Name() == name {
			continue
		}
		if id, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32); err == nil {
			held[uint32(id)] = true
		}
	}
	for i := range d.cfg.VMUsers.Count {
		id := d.cfg.VMUsers.First + i
		if held[id] {
			continue
		}
		if err := os.MkdirAll(d.dir(name), 0o700); err != nil {
			return 0, err
		}
		return id, writeAtomic(filepath.Join(d.dir(name), vmUserFile), []byte(strconv.FormatUint(uint64(id), 10)+"\n"), 0o600)
	}
	return 0, fmt.Errorf("all %d VM users are taken", d.cfg.VMUsers.Count)
}
