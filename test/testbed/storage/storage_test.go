//go:build testbed

// Package storage holds the real-host tests of the storage driver
// (internal/hostd/driver/zfsstore) on the testbed's pool, tower, beyond
// the drivers' conformance suite (test/testbed/drivers): what the host
// itself sees of its datasets, zvols and snapshots, and decoys the driver
// must leave alone (docs/runbook/testbed.md).
package storage

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/zfsstore"
)

const (
	dataset   = "tower/testbed-storage"
	mountRoot = "/var/tmp/tower-testbed-storage"
)

// zfs runs the zfs command, failing the test if it fails.
func zfs(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("zfs", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("zfs %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newStore makes the driver on a dataset of the test's own, removed as the
// test ends, with decoys in it no driver made.
func newStore(t *testing.T) *zfsstore.Store {
	t.Helper()
	ctx := t.Context()
	_ = exec.Command("zfs", "destroy", "-R", dataset).Run()
	zfs(t, "create", "-o", "mountpoint=none", dataset)
	t.Cleanup(func() {
		if out, err := exec.Command("zfs", "destroy", "-R", dataset).CombinedOutput(); err != nil {
			t.Errorf("removing %s: %v: %s", dataset, err, out)
		}
		_ = os.RemoveAll(mountRoot)
	})
	s, err := zfsstore.New(ctx, zfsstore.Config{Dataset: dataset, MountRoot: mountRoot})
	if err != nil {
		t.Fatal(err)
	}
	// Decoys: a dataset and a zvol among the disks, and a dataset with a
	// snapshot among the goldens, none of them carrying the driver's own
	// properties.
	zfs(t, "create", dataset+"/disks/decoy")
	zfs(t, "create", "-s", "-V", "16M", dataset+"/disks/decoyvol")
	zfs(t, "create", "-o", "mountpoint=none", dataset+"/goldens/decoy")
	zfs(t, "snapshot", dataset+"/goldens/decoy@decoy")
	return s
}

// decoysStand fails the test unless every decoy is still there.
func decoysStand(t *testing.T) {
	t.Helper()
	for _, ds := range []string{"disks/decoy", "disks/decoyvol", "goldens/decoy", "goldens/decoy@decoy"} {
		if err := exec.Command("zfs", "list", "-t", "all", dataset+"/"+ds).Run(); err != nil {
			t.Errorf("the decoy %s is gone", ds)
		}
	}
}

var labels = driver.Labels{driver.LabelOwner: "testbed", driver.LabelInstance: "storage"}

// TestAGoldenOnTheHost: a dataset written on the host, captured, holds
// what it held in a clone of it after the disk went; the golden is a
// promoted dataset of its own; destroyed with a clone, it is deferred,
// and its dataset goes with its last clone; decoys are never listed or
// touched.
func TestAGoldenOnTheHost(t *testing.T) {
	s, ctx := newStore(t), t.Context()
	src, err := s.EnsureDisk(ctx, driver.DiskSpec{Name: "src", Kind: driver.Dataset, Size: 1 << 30, Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	if src.Path != filepath.Join(mountRoot, "src") {
		t.Errorf("the disk is at %s", src.Path)
	}
	if err := os.WriteFile(filepath.Join(src.Path, "warm.txt"), []byte("installed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if q := zfs(t, "get", "-Hp", "-o", "value", "quota", dataset+"/disks/src"); q != "1073741824" {
		t.Errorf("the disk's quota is %s", q)
	}
	g, err := s.Capture(ctx, "src", "g1", labels)
	if err != nil {
		t.Fatal(err)
	}
	if g.Kind != driver.Dataset || g.Size == 0 {
		t.Errorf("golden %+v", g)
	}
	if origin := zfs(t, "get", "-H", "-o", "value", "origin", dataset+"/disks/src"); origin != dataset+"/goldens/g1@g1" {
		t.Errorf("the captured disk's origin is %s; want the golden's snapshot, promoted", origin)
	}
	if err := s.DestroyDisk(ctx, "src"); err != nil {
		t.Fatal(err)
	}
	clone, err := s.EnsureDisk(ctx, driver.DiskSpec{Name: "clone", Kind: driver.Dataset, Size: 1 << 30, From: "g1", Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(clone.Path, "warm.txt")); err != nil || string(b) != "installed" {
		t.Errorf("the clone holds %q, %v", b, err)
	}
	disks, err := s.Disks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range disks {
		if strings.HasPrefix(d.Name, "decoy") {
			t.Errorf("a decoy is listed: %+v", d)
		}
	}
	if err := s.DestroyGolden(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	if d := zfs(t, "get", "-H", "-o", "value", "defer_destroy", dataset+"/goldens/g1@g1"); d != "on" {
		t.Errorf("a golden destroyed with a clone has defer_destroy %s", d)
	}
	if _, err := s.EnsureDisk(ctx, driver.DiskSpec{Name: "late", Kind: driver.Dataset, From: "g1", Labels: labels}); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a clone of a deferred golden: %v", err)
	}
	if err := s.DestroyDisk(ctx, "clone"); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("zfs", "list", dataset+"/goldens/g1").Run(); err == nil {
		t.Error("a golden's dataset stays after its last clone went")
	}
	goldens, err := s.Goldens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(goldens) != 0 {
		t.Errorf("goldens listed: %+v", goldens)
	}
	decoysStand(t)
	// The decoys are not the driver's to destroy.
	if err := s.DestroyDisk(ctx, "decoy"); err != nil {
		t.Errorf("destroying a decoy: %v", err)
	}
	if err := s.DestroyGolden(ctx, "decoy"); err != nil {
		t.Errorf("destroying a decoy golden: %v", err)
	}
	if _, err := s.EnsureDisk(ctx, driver.DiskSpec{Name: "decoy", Kind: driver.Dataset, Labels: labels}); !errors.Is(err, driver.ErrConflict) {
		t.Errorf("making a disk at a decoy's name: %v, want ErrConflict", err)
	}
	decoysStand(t)
}

// TestAVolumeOnTheHost: a VM's disk is a sparse zvol, its device the zvol
// itself; one cloned from a golden is grown to its size; one open is
// busy.
func TestAVolumeOnTheHost(t *testing.T) {
	s, ctx := newStore(t), t.Context()
	v, err := s.EnsureDisk(ctx, driver.DiskSpec{Name: "vm", Kind: driver.Volume, Size: 64 << 20, Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(v.Path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if name, err := zfsstore.VolumeOf(f); err != nil || name != dataset+"/disks/vm" {
		t.Errorf("the device is %s, %v", name, err)
	}
	if _, err := f.WriteAt([]byte("bootsector"), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Sync()
	if err := s.DestroyDisk(ctx, "vm"); !errors.Is(err, driver.ErrBusy) {
		t.Errorf("destroying an open zvol: %v, want ErrBusy", err)
	}
	_ = f.Close()
	if _, err := s.Capture(ctx, "vm", "gv", labels); err != nil {
		t.Fatal(err)
	}
	big, err := s.EnsureDisk(ctx, driver.DiskSpec{Name: "big", Kind: driver.Volume, Size: 128 << 20, From: "gv", Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	if big.Size != 128<<20 {
		t.Errorf("a clone asked 128 MiB has %d bytes", big.Size)
	}
	b := make([]byte, 10)
	g, err := os.Open(big.Path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = g.ReadAt(b, 0)
	_ = g.Close()
	if string(b) != "bootsector" {
		t.Errorf("the clone reads %q", b)
	}
	pool, err := s.Pool(ctx)
	if err != nil || pool.Name != "tower" || pool.Health != "ONLINE" {
		t.Errorf("pool %+v, %v", pool, err)
	}
	for _, n := range []string{"big", "vm"} {
		if err := s.DestroyDisk(context.WithoutCancel(ctx), n); err != nil {
			t.Error(err)
		}
	}
	if err := s.DestroyGolden(ctx, "gv"); err != nil {
		t.Error(err)
	}
	decoysStand(t)
}
