// Package zfsstore is hostd's Storage driver, on ZFS through the zfs
// command (docs/decisions/hostd-storage.md). Each instance's disk is a
// dataset, mounted on the host for its container's /workspace, or a sparse
// zvol, a VM's root disk, under Dataset/disks; each golden is a snapshot,
// Dataset/goldens/<name>@<name>, of a dataset of its own that Capture
// clones from the disk and promotes, so the golden holds what the disk
// held and outlives it, and a disk cloned from it is a clone of its
// snapshot. A golden destroyed while disks are cloned from it is destroyed
// as zfs destroy -d does, once its last clone goes. Every dataset and
// snapshot the driver makes carries a user property naming it and the
// labels hostd gave it, which is how it is listed and how the driver knows
// it as its own: what does not carry its own name is never changed or
// destroyed. The driver keeps nothing in memory but locks.
//
// It started as hangar's internal/node/driver/zfsstore, whose helpers
// (running zfs, reading properties, waiting for a zvol's device) it keeps.
package zfsstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/keylock"
)

// User properties on the driver's datasets and snapshots.
const (
	// propDisk is a disk's name, on its dataset or zvol.
	propDisk = "tower:disk"
	// propGolden is a golden's name, on its dataset and its snapshot.
	propGolden = "tower:golden"
	// propFrom is the golden a disk was cloned from, "-" for none.
	propFrom = "tower:from"
	// propLabels are the labels hostd gave it, as JSON.
	propLabels = "tower:labels"
)

// align is what sizes are rounded up to: a multiple of any volblocksize.
const align = 1 << 20

// Config configures the driver.
type Config struct {
	// Dataset is the parent of the driver's datasets, such as tower/hostd;
	// it must exist. The driver keeps disks in Dataset/disks, mounted under
	// MountRoot, and goldens in Dataset/goldens, never mounted, and makes
	// those if need be.
	Dataset string
	// MountRoot is where disks that are datasets are mounted, each at
	// MountRoot/<name>, such as /var/lib/tower-hostd/disks.
	MountRoot string
	// DeviceTimeout bounds waiting for a zvol's device node. Zero means 30
	// seconds.
	DeviceTimeout time.Duration
	// Log is where the driver notes what it recovered from. Nil means
	// slog.Default().
	Log *slog.Logger
}

// Store is the driver. It is safe for concurrent use.
type Store struct {
	cfg Config
	// locks holds a lock per disk and golden name, so one is made, captured
	// or destroyed once at a time.
	locks keylock.Map
}

var _ driver.Storage = (*Store)(nil)

// New checks the parent dataset and makes the driver's own datasets in it
// if need be.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DeviceTimeout == 0 {
		cfg.DeviceTimeout = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if !filepath.IsAbs(cfg.MountRoot) || filepath.Clean(cfg.MountRoot) != cfg.MountRoot {
		return nil, fmt.Errorf("the disks' mount root %q is not a clean absolute path", cfg.MountRoot)
	}
	s := &Store{cfg: cfg}
	if _, err := zfs(ctx, "list", "-H", "-o", "name", cfg.Dataset); err != nil {
		return nil, fmt.Errorf("the dataset for disks: %w", err)
	}
	// Each zvol inherits volmode=dev from these, so its device is made as
	// dev, never made as the default and then made again when its own
	// volmode=dev is set (hangar: docs/decisions/storage-zfs.md).
	for ds, mountpoint := range map[string]string{s.disks(): cfg.MountRoot, s.goldens(): "none"} {
		p, ok, err := props(ctx, ds, "volmode", "mountpoint")
		switch {
		case err != nil:
			return nil, err
		case !ok:
			_, err = zfs(ctx, "create", "-o", "mountpoint="+mountpoint, "-o", "canmount=off", "-o", "volmode=dev", ds)
		case p["volmode"] != "dev" || p["mountpoint"] != mountpoint:
			_, err = zfs(ctx, "set", "volmode=dev", "mountpoint="+mountpoint, ds)
		}
		if err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) disks() string   { return s.cfg.Dataset + "/disks" }
func (s *Store) goldens() string { return s.cfg.Dataset + "/goldens" }

// nameRE is what a disk or a golden may be named: a single ZFS component.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

// diskName returns a disk's dataset, or fails for a name that cannot be
// one.
func (s *Store) diskName(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("%q cannot name a disk", name)
	}
	return s.disks() + "/" + name, nil
}

// goldenName returns a golden's dataset and its snapshot, or fails for a
// name that cannot be one.
func (s *Store) goldenName(name string) (ds, snap string, err error) {
	if !nameRE.MatchString(name) {
		return "", "", fmt.Errorf("%q cannot name a golden", name)
	}
	ds = s.goldens() + "/" + name
	return ds, ds + "@" + name, nil
}

func device(volume string) string { return "/dev/zvol/" + volume }

// What zfs says of a dataset that does not exist, and of one it will not
// destroy while it is in use.
var (
	errNotExist = errors.New("dataset does not exist")
	errBusy     = errors.New("dataset is busy")
)

// busyGrace bounds retrying a destroy refused as busy: udev opens a zvol to
// probe it whenever a writer closes it, as a VM that stopped did.
const busyGrace = 3 * time.Second

// destroy destroys a dataset or snapshot with the flags, never
// recursively, waiting out a probe that holds it open; one held longer
// stays and fails.
func destroy(ctx context.Context, ds string, flags ...string) error {
	deadline := time.Now().Add(busyGrace)
	for delay := 20 * time.Millisecond; ; delay = min(delay*2, 500*time.Millisecond) {
		_, err := zfs(ctx, append(append([]string{"destroy"}, flags...), ds)...)
		if !errors.Is(err, errBusy) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
	}
}

// zfs runs the zfs command and returns its output.
func zfs(ctx context.Context, args ...string) ([]byte, error) {
	return run(ctx, "zfs", args...)
}

func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	// Errors are told apart by what the command says, in English.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		switch {
		// A snapshot that is not there, zfs destroy says otherwise.
		case strings.Contains(msg, "dataset does not exist"), strings.Contains(msg, "could not find any snapshots to destroy"):
			return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), errNotExist)
		// A dataset mounted in a container's mount namespace: its unmount
		// on the host is refused, or its destroy once unmounted.
		case strings.Contains(msg, "dataset is busy"), strings.Contains(msg, "target is busy"), strings.Contains(msg, "umount failed"):
			return nil, fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), msg, errBusy)
		case strings.Contains(msg, "out of space") || strings.Contains(msg, "No space left"):
			return nil, fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), msg, driver.ErrNoSpace)
		}
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

// props returns the named properties of a dataset, by name, parsed from
// zfs get -Hp; ok is false if it does not exist.
func props(ctx context.Context, ds string, names ...string) (map[string]string, bool, error) {
	out, err := zfs(ctx, "get", "-Hp", "-o", "property,value", strings.Join(names, ","), ds)
	if errors.Is(err, errNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if k, v, ok := strings.Cut(line, "\t"); ok {
			got[k] = v
		}
	}
	return got, true, nil
}

// list returns the rows of zfs list -Hp for the columns, of datasets of the
// types under parent, depth deep.
func list(ctx context.Context, types, parent string, depth int, columns ...string) ([][]string, error) {
	out, err := zfs(ctx, "list", "-Hp", "-d", strconv.Itoa(depth), "-t", types, "-o", strings.Join(columns, ","), parent)
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		row := strings.Split(line, "\t")
		if len(row) != len(columns) {
			return nil, fmt.Errorf("zfs list: %q has %d columns, want %d", line, len(row), len(columns))
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// unset is a user property's value, "" for one not set ("-").
func unset(v string) string {
	if v == "-" {
		return ""
	}
	return v
}

func encodeLabels(l driver.Labels) (string, error) {
	if l == nil {
		l = driver.Labels{}
	}
	b, err := json.Marshal(l)
	return string(b), err
}

func decodeLabels(v string) driver.Labels {
	l := driver.Labels{}
	if v = unset(v); v != "" {
		_ = json.Unmarshal([]byte(v), &l)
	}
	return l
}

func roundUp(n, to uint64) uint64 { return (n + to - 1) / to * to }

func parseUint(what, v string) (uint64, error) {
	if unset(v) == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", what, v, err)
	}
	return n, nil
}

func parseTime(v string) time.Time {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func (s *Store) Version(ctx context.Context) (string, error) {
	out, err := zfs(ctx, "version")
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return first, nil
}

// diskColumns are what Disks reads of each disk.
var diskColumns = []string{"name", "type", propDisk, propFrom, propLabels, "used", "creation", "volsize", "quota", "mountpoint"}

// diskOf is the disk a row of diskColumns is, ok false for what is not the
// driver's.
func (s *Store) diskOf(row []string) (driver.Disk, bool, error) {
	name, typ := row[0], row[1]
	disk := unset(row[2])
	if !nameRE.MatchString(disk) || s.disks()+"/"+disk != name {
		return driver.Disk{}, false, nil
	}
	d := driver.Disk{DiskSpec: driver.DiskSpec{Name: disk, From: unset(row[3]), Labels: decodeLabels(row[4])}, Created: parseTime(row[6])}
	var err error
	if d.Used, err = parseUint("used", row[5]); err != nil {
		return driver.Disk{}, false, err
	}
	switch typ {
	case "volume":
		d.Kind, d.Path = driver.Volume, device(name)
		d.Size, err = parseUint("volsize", row[7])
	case "filesystem":
		d.Kind, d.Path = driver.Dataset, row[9]
		d.Size, err = parseUint("quota", row[8])
	default:
		return driver.Disk{}, false, nil
	}
	return d, true, err
}

// disk reads one disk, ok false when the driver has none of the name.
func (s *Store) disk(ctx context.Context, name string) (driver.Disk, bool, error) {
	ds, err := s.diskName(name)
	if err != nil {
		return driver.Disk{}, false, err
	}
	rows, err := list(ctx, "filesystem,volume", ds, 0, diskColumns...)
	if errors.Is(err, errNotExist) || len(rows) == 0 {
		return driver.Disk{}, false, nil
	}
	if err != nil {
		return driver.Disk{}, false, err
	}
	return s.diskOf(rows[0])
}

func (s *Store) Disks(ctx context.Context) ([]driver.Disk, error) {
	rows, err := list(ctx, "filesystem,volume", s.disks(), 1, diskColumns...)
	if err != nil {
		return nil, err
	}
	var out []driver.Disk
	for _, row := range rows {
		d, ok, err := s.diskOf(row)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *Store) EnsureDisk(ctx context.Context, spec driver.DiskSpec) (driver.Disk, error) {
	ds, err := s.diskName(spec.Name)
	if err != nil {
		return driver.Disk{}, err
	}
	if spec.Kind != driver.Dataset && spec.Kind != driver.Volume {
		return driver.Disk{}, fmt.Errorf("disk %s is of no kind %d", spec.Name, spec.Kind)
	}
	unlock := s.locks.Lock("disk/" + spec.Name)
	defer unlock()
	if d, ok, err := s.disk(ctx, spec.Name); err != nil {
		return driver.Disk{}, err
	} else if ok {
		if d.Kind != spec.Kind || d.From != spec.From {
			return driver.Disk{}, fmt.Errorf("disk %s is there, a %v from %q: %w", spec.Name, d.Kind, d.From, driver.ErrConflict)
		}
		return d, nil
	} else if _, there, err := props(ctx, ds, "type"); err != nil {
		return driver.Disk{}, err
	} else if there {
		return driver.Disk{}, fmt.Errorf("%s is there, and not this driver's: %w", ds, driver.ErrConflict)
	}
	labels, err := encodeLabels(spec.Labels)
	if err != nil {
		return driver.Disk{}, err
	}
	from := spec.From
	if from == "" {
		from = "-"
	}
	own := []string{"-o", propDisk + "=" + spec.Name, "-o", propFrom + "=" + from, "-o", propLabels + "=" + labels}
	size := roundUp(max(spec.Size, 1), align)
	switch {
	case spec.From != "":
		g, ok, err := s.golden(ctx, spec.From)
		if err != nil {
			return driver.Disk{}, err
		}
		if !ok || g.Deferred {
			return driver.Disk{}, fmt.Errorf("golden %s: %w", spec.From, driver.ErrNotFound)
		}
		if g.Kind != spec.Kind {
			return driver.Disk{}, fmt.Errorf("golden %s holds a %v, not a %v: %w", spec.From, g.Kind, spec.Kind, driver.ErrConflict)
		}
		_, snap, _ := s.goldenName(spec.From)
		args := append([]string{"clone"}, own...)
		if spec.Kind == driver.Volume {
			args = append(args, "-o", "volmode=dev", "-o", "refreservation=none")
		} else if spec.Size > 0 {
			args = append(args, "-o", "quota="+strconv.FormatUint(size, 10))
		}
		if _, err := zfs(ctx, append(args, snap, ds)...); err != nil {
			return driver.Disk{}, err
		}
		if spec.Kind == driver.Volume {
			// A volume smaller than asked is grown, never shrunk.
			p, _, err := props(ctx, ds, "volsize")
			if err != nil {
				return driver.Disk{}, err
			}
			if have, _ := parseUint("volsize", p["volsize"]); have < size {
				if _, err := zfs(ctx, "set", "volsize="+strconv.FormatUint(size, 10), ds); err != nil {
					return driver.Disk{}, err
				}
			}
		}
	case spec.Kind == driver.Volume:
		args := append([]string{"create", "-s", "-V", strconv.FormatUint(size, 10), "-o", "volmode=dev"}, own...)
		if _, err := zfs(ctx, append(args, ds)...); err != nil {
			return driver.Disk{}, err
		}
	default:
		args := append([]string{"create"}, own...)
		if spec.Size > 0 {
			args = append(args, "-o", "quota="+strconv.FormatUint(size, 10))
		}
		if _, err := zfs(ctx, append(args, ds)...); err != nil {
			return driver.Disk{}, err
		}
	}
	if spec.Kind == driver.Volume {
		if _, err := s.waitDevice(ctx, ds); err != nil {
			return driver.Disk{}, err
		}
	}
	d, ok, err := s.disk(ctx, spec.Name)
	if err == nil && !ok {
		err = fmt.Errorf("disk %s was made and is not there", spec.Name)
	}
	return d, err
}

func (s *Store) DestroyDisk(ctx context.Context, name string) error {
	ds, err := s.diskName(name)
	if err != nil {
		return err
	}
	unlock := s.locks.Lock("disk/" + name)
	defer unlock()
	d, ok, err := s.disk(ctx, name)
	if err != nil || !ok {
		return err
	}
	err = destroy(ctx, ds)
	if errors.Is(err, errBusy) {
		if d.Kind == driver.Dataset {
			// Its unmount may have gone through before its destroy was
			// refused: it is mounted again for the guest still on it.
			if _, merr := zfs(ctx, "mount", ds); merr != nil && !strings.Contains(merr.Error(), "already mounted") {
				s.cfg.Log.Warn("mounting a disk again after its destroy was refused", "disk", name, "error", merr)
			}
		}
		return fmt.Errorf("disk %s: %w", name, driver.ErrBusy)
	}
	if err != nil && !errors.Is(err, errNotExist) {
		return err
	}
	return s.reapGoldens(ctx)
}

// goldenColumns are what Goldens reads of each golden's snapshot.
var goldenColumns = []string{"name", propGolden, propLabels, "referenced", "creation", "defer_destroy", "clones"}

// goldenOf is the golden a row of goldenColumns is, ok false for what is
// not the driver's.
func (s *Store) goldenOf(row []string, kinds map[string]driver.DiskKind) (driver.Golden, bool, error) {
	name := unset(row[1])
	if !nameRE.MatchString(name) {
		return driver.Golden{}, false, nil
	}
	ds, snap, err := s.goldenName(name)
	if err != nil {
		return driver.Golden{}, false, err
	}
	if snap != row[0] {
		return driver.Golden{}, false, nil
	}
	g := driver.Golden{Name: name, Kind: kinds[ds], Labels: decodeLabels(row[2]), Created: parseTime(row[4]), Deferred: row[5] == "on"}
	if g.Size, err = parseUint("referenced", row[3]); err != nil {
		return driver.Golden{}, false, err
	}
	// Its clones that are disks: a golden captured from a clone of it is
	// a clone too, but no disk.
	for _, c := range strings.Split(unset(row[6]), ",") {
		if strings.HasPrefix(c, s.disks()+"/") {
			g.Clones++
		}
	}
	return g, true, nil
}

// goldenKinds are the kinds of the goldens' datasets, by dataset.
func (s *Store) goldenKinds(ctx context.Context) (map[string]driver.DiskKind, error) {
	rows, err := list(ctx, "filesystem,volume", s.goldens(), 1, "name", "type")
	if err != nil {
		return nil, err
	}
	kinds := map[string]driver.DiskKind{}
	for _, row := range rows {
		switch row[1] {
		case "volume":
			kinds[row[0]] = driver.Volume
		case "filesystem":
			kinds[row[0]] = driver.Dataset
		}
	}
	return kinds, nil
}

// golden reads one golden, ok false when the driver has none of the name.
func (s *Store) golden(ctx context.Context, name string) (driver.Golden, bool, error) {
	ds, snap, err := s.goldenName(name)
	if err != nil {
		return driver.Golden{}, false, err
	}
	rows, err := list(ctx, "snapshot", snap, 0, goldenColumns...)
	if errors.Is(err, errNotExist) || len(rows) == 0 {
		return driver.Golden{}, false, nil
	}
	if err != nil {
		return driver.Golden{}, false, err
	}
	p, _, err := props(ctx, ds, "type")
	if err != nil {
		return driver.Golden{}, false, err
	}
	kind := driver.Dataset
	if p["type"] == "volume" {
		kind = driver.Volume
	}
	return s.goldenOf(rows[0], map[string]driver.DiskKind{ds: kind})
}

func (s *Store) Goldens(ctx context.Context) ([]driver.Golden, error) {
	kinds, err := s.goldenKinds(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := list(ctx, "snapshot", s.goldens(), 2, goldenColumns...)
	if err != nil {
		return nil, err
	}
	var out []driver.Golden
	for _, row := range rows {
		g, ok, err := s.goldenOf(row, kinds)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, func(a, b driver.Golden) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Capture snapshots the disk, clones the snapshot as the golden's dataset
// and promotes it: the golden's dataset then owns the snapshot, and the
// disk is a clone of it, which can go first. A capture cut short is taken
// up again from where it stopped.
func (s *Store) Capture(ctx context.Context, disk, golden string, labels driver.Labels) (driver.Golden, error) {
	src, err := s.diskName(disk)
	if err != nil {
		return driver.Golden{}, err
	}
	gds, gsnap, err := s.goldenName(golden)
	if err != nil {
		return driver.Golden{}, err
	}
	unlockDisk := s.locks.Lock("disk/" + disk)
	defer unlockDisk()
	unlock := s.locks.Lock("golden/" + golden)
	defer unlock()
	if g, ok, err := s.golden(ctx, golden); err != nil || ok {
		return g, err
	}
	enc, err := encodeLabels(labels)
	if err != nil {
		return driver.Golden{}, err
	}
	srcSnap := src + "@" + golden
	if p, there, err := props(ctx, gds, propGolden, "origin"); err != nil {
		return driver.Golden{}, err
	} else if there && unset(p[propGolden]) != golden {
		return driver.Golden{}, fmt.Errorf("%s is there, and not this driver's: %w", gds, driver.ErrConflict)
	} else if !there {
		d, ok, err := s.disk(ctx, disk)
		if err != nil {
			return driver.Golden{}, err
		}
		if !ok {
			return driver.Golden{}, fmt.Errorf("disk %s: %w", disk, driver.ErrNotFound)
		}
		if _, snapThere, err := props(ctx, srcSnap, "type"); err != nil {
			return driver.Golden{}, err
		} else if !snapThere {
			if _, err := zfs(ctx, "snapshot", "-o", propGolden+"="+golden, "-o", propLabels+"="+enc, srcSnap); err != nil {
				return driver.Golden{}, err
			}
		}
		args := []string{"clone", "-o", propGolden + "=" + golden, "-o", "readonly=on"}
		if d.Kind == driver.Volume {
			args = append(args, "-o", "volmode=dev", "-o", "refreservation=none")
		} else {
			args = append(args, "-o", "canmount=off", "-o", "mountpoint=none")
		}
		if _, err := zfs(ctx, append(args, srcSnap, gds)...); err != nil {
			return driver.Golden{}, err
		}
	}
	// Promoted once: the snapshot is then the golden's. A promote cut short
	// leaves the clone unpromoted, whose origin is still the disk's.
	if p, _, err := props(ctx, gds, "origin"); err != nil {
		return driver.Golden{}, err
	} else if p["origin"] == srcSnap {
		if _, err := zfs(ctx, "promote", gds); err != nil {
			return driver.Golden{}, err
		}
	}
	g, ok, err := s.golden(ctx, golden)
	if err == nil && !ok {
		err = fmt.Errorf("golden %s was captured and %s is not there", golden, gsnap)
	}
	return g, err
}

func (s *Store) DestroyGolden(ctx context.Context, name string) error {
	_, snap, err := s.goldenName(name)
	if err != nil {
		return err
	}
	unlock := s.locks.Lock("golden/" + name)
	g, ok, err := s.golden(ctx, name)
	if err != nil || !ok {
		unlock()
		return err
	}
	if !g.Deferred {
		// With clones it stays, deferred, until its last clone goes; with
		// none it goes now.
		if err := destroy(ctx, snap, "-d"); err != nil && !errors.Is(err, errNotExist) {
			unlock()
			return err
		}
	}
	unlock()
	return s.reapGoldens(ctx)
}

// reapGoldens destroys the datasets of goldens whose snapshot is gone, as
// a deferred one goes with its last clone, and, in turn, what their going
// lets go: a golden captured from a clone of another is one of its clones.
func (s *Store) reapGoldens(ctx context.Context) error {
	for {
		rows, err := list(ctx, "filesystem,volume", s.goldens(), 1, "name", propGolden)
		if err != nil {
			return err
		}
		snaps, err := list(ctx, "snapshot", s.goldens(), 2, "name")
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, r := range snaps {
			have[r[0]] = true
		}
		reaped := false
		for _, row := range rows {
			name := unset(row[1])
			ds, snap, err := s.goldenName(name)
			if err != nil || ds != row[0] || have[snap] {
				continue
			}
			// The golden's dataset holds snapshots of its own only while
			// it is promoted and its snapshot there; with that gone, any
			// other is one zfs keeps for a clone, and the dataset waits.
			unlock := s.locks.Lock("golden/" + name)
			err = destroy(ctx, ds)
			unlock()
			switch {
			case err == nil:
				reaped = true
			case errors.Is(err, errNotExist):
			default:
				s.cfg.Log.Debug("a golden's dataset stays", "golden", name, "error", err)
			}
		}
		if !reaped {
			return nil
		}
	}
}

func (s *Store) Pool(ctx context.Context) (driver.Pool, error) {
	pool, _, _ := strings.Cut(s.cfg.Dataset, "/")
	out, err := run(ctx, "zpool", "list", "-Hp", "-o", "name,health,size,free", pool)
	if err != nil {
		return driver.Pool{}, err
	}
	f := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(f) != 4 {
		return driver.Pool{}, fmt.Errorf("zpool list: %q", out)
	}
	size, err1 := parseUint("size", f[2])
	free, err2 := parseUint("free", f[3])
	if err := errors.Join(err1, err2); err != nil {
		return driver.Pool{}, err
	}
	return driver.Pool{Name: f[0], Health: f[1], Size: size, Free: free}, nil
}

// waitDevice waits for a zvol's /dev/zvol link to lead to the zvol itself,
// as a stale one may lead to another (hangar:
// docs/decisions/storage-zfs.md), and returns the device.
func (s *Store) waitDevice(ctx context.Context, vol string) (string, error) {
	deadline := time.Now().Add(s.cfg.DeviceTimeout)
	for {
		dev, err := filepath.EvalSymlinks(device(vol))
		if err == nil {
			var name string
			if name, err = zvolName(dev); err == nil && name == vol {
				return dev, nil
			}
			if err == nil {
				err = fmt.Errorf("it leads to %s, which is %s", dev, name)
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s did not lead to %s in %v: %w", device(vol), vol, s.cfg.DeviceTimeout, err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// blkZName is ZFS's BLKZNAME, _IOR(0x12, 125, char[256]): the dataset a
// zvol's device is, as udev's zvol_id asks it.
const blkZName = 0x8100127d

// zvolName returns the dataset the zvol device dev is.
func zvolName(dev string) (string, error) {
	f, err := os.Open(dev)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return VolumeOf(f)
}

// VolumeOf returns the dataset the open zvol device f is, by BLKZNAME: what
// the device is, whatever link led to it.
func VolumeOf(f *os.File) (string, error) {
	var buf [256]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkZName, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
		return "", fmt.Errorf("%s: BLKZNAME: %w", f.Name(), errno)
	}
	name, _, _ := bytes.Cut(buf[:], []byte{0})
	return string(name), nil
}
