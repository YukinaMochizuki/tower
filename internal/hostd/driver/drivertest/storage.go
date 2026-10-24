package drivertest

import (
	"maps"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// RunStorage runs the storage driver's suite. Its goldens hold what a
// container wrote, so it needs the container driver too.
func RunStorage(t *testing.T, h Harness) {
	tests := []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"DisksAreMadeOnce", testDisksAreMadeOnce},
		{"GoldensOutliveTheirDisk", testGoldensOutliveTheirDisk},
		{"DiskOfAGuestIsBusy", testDiskOfAGuestIsBusy},
		{"Pool", testPool},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, h) })
	}
}

// disk makes a disk until the test ends.
func disk(t *testing.T, h Harness, spec driver.DiskSpec) driver.Disk {
	t.Helper()
	s := h.Drivers.Storage
	d, err := s.EnsureDisk(t.Context(), spec)
	mustNot(t, "making disk "+spec.Name, err)
	t.Cleanup(func() { _ = s.DestroyDisk(t.Context(), spec.Name) })
	return d
}

func listed(t *testing.T, h Harness, name string) (driver.Disk, bool) {
	t.Helper()
	disks, err := h.Drivers.Storage.Disks(t.Context())
	mustNot(t, "listing disks", err)
	for _, d := range disks {
		if d.Name == name {
			return d, true
		}
	}
	return driver.Disk{}, false
}

func testDisksAreMadeOnce(t *testing.T, h Harness) {
	s, ctx := h.Drivers.Storage, t.Context()
	spec := driver.DiskSpec{Name: name(t, "a"), Kind: driver.Dataset, Size: 1 << 30, Labels: labels(t)}
	d := disk(t, h, spec)
	if d.Path == "" || d.Name != spec.Name || d.Kind != driver.Dataset {
		t.Errorf("disk made: %s", describe(d))
	}
	again, err := s.EnsureDisk(ctx, spec)
	if err != nil || again.Path != d.Path {
		t.Errorf("making the disk again: %s, %v; want it as it is", describe(again), err)
	}
	l, ok := listed(t, h, spec.Name)
	if !ok || !maps.Equal(l.Labels, spec.Labels) {
		t.Errorf("the disk as listed: %s, %v; want its labels", describe(l), ok)
	}
	other := spec
	other.Kind = driver.Volume
	_, err = s.EnsureDisk(ctx, other)
	is(t, "a disk of the name made otherwise", err, driver.ErrConflict)
	_, err = s.EnsureDisk(ctx, driver.DiskSpec{Name: name(t, "b"), Kind: driver.Dataset, From: name(t, "never-captured"), Labels: labels(t)})
	is(t, "a disk cloned from a golden that is not there", err, driver.ErrNotFound)
	mustNot(t, "destroying the disk", s.DestroyDisk(ctx, spec.Name))
	if _, ok := listed(t, h, spec.Name); ok {
		t.Error("a disk destroyed is still listed")
	}
	mustNot(t, "destroying a disk that is not there", s.DestroyDisk(ctx, spec.Name))
}

func testGoldensOutliveTheirDisk(t *testing.T, h Harness) {
	s, c, ctx := h.Drivers.Storage, h.containers(), t.Context()
	src := disk(t, h, driver.DiskSpec{Name: name(t, "src"), Kind: driver.Dataset, Size: 1 << 30, Labels: labels(t)})
	g := c.guest(t, h, "writer", src.Path)
	mustNot(t, "writing", c.Driver.WriteFile(ctx, g, c.Workspace+"/warm.txt", 0o644, 0, strings.NewReader("installed")))
	mustNot(t, "removing the writer", c.Driver.Remove(ctx, g))

	gname := name(t, "golden")
	golden, err := s.Capture(ctx, src.Name, gname, labels(t))
	mustNot(t, "capturing", err)
	t.Cleanup(func() { _ = s.DestroyGolden(t.Context(), gname) })
	if golden.Name != gname || golden.Size == 0 || golden.Kind != driver.Dataset || !maps.Equal(golden.Labels, labels(t)) {
		t.Errorf("golden: %+v", golden)
	}
	if again, err := s.Capture(ctx, src.Name, gname, labels(t)); err != nil || again.Name != gname {
		t.Errorf("capturing again: %+v, %v; want the golden as it is", again, err)
	}
	mustNot(t, "destroying the captured disk", s.DestroyDisk(ctx, src.Name))

	clone := disk(t, h, driver.DiskSpec{Name: name(t, "clone"), Kind: driver.Dataset, Size: 1 << 30, From: gname, Labels: labels(t)})
	if clone.Used >= golden.Size {
		t.Errorf("a clone written nothing uses %d bytes, the golden holds %d: want only what it holds beyond it", clone.Used, golden.Size)
	}
	reader := c.guest(t, h, "reader", clone.Path)
	if got := readFile(t, c.Driver, reader, c.Workspace+"/warm.txt"); got != "installed" {
		t.Errorf("a clone of the golden holds %q", got)
	}
	goldens, err := s.Goldens(ctx)
	mustNot(t, "listing goldens", err)
	found := false
	for _, gd := range goldens {
		if gd.Name == gname {
			found = true
			if gd.Clones != 1 {
				t.Errorf("the golden has %d clones, want 1", gd.Clones)
			}
		}
	}
	if !found {
		t.Error("the golden is not listed")
	}
	mustNot(t, "destroying a golden with a clone", s.DestroyGolden(ctx, gname))
	if got := readFile(t, c.Driver, reader, c.Workspace+"/warm.txt"); got != "installed" {
		t.Errorf("a clone of a golden destroyed holds %q", got)
	}
	if !goldenListed(t, h, gname, func(g driver.Golden) bool { return g.Deferred }) {
		t.Error("a golden destroyed with a clone is not listed deferred")
	}
	_, err = s.EnsureDisk(ctx, driver.DiskSpec{Name: name(t, "late"), Kind: driver.Dataset, From: gname, Labels: labels(t)})
	is(t, "a clone of a golden destroyed", err, driver.ErrNotFound)
	mustNot(t, "removing the reader", c.Driver.Remove(ctx, reader))
	mustNot(t, "destroying the clone", s.DestroyDisk(ctx, clone.Name))
	if goldenListed(t, h, gname, nil) {
		t.Error("a golden destroyed is still listed once its last clone went")
	}
	mustNot(t, "destroying a golden that is not there", s.DestroyGolden(ctx, gname))
}

// goldenListed reports whether the golden is listed, and holds ok.
func goldenListed(t *testing.T, h Harness, name string, ok func(driver.Golden) bool) bool {
	t.Helper()
	goldens, err := h.Drivers.Storage.Goldens(t.Context())
	mustNot(t, "listing goldens", err)
	for _, g := range goldens {
		if g.Name == name {
			return ok == nil || ok(g)
		}
	}
	return false
}

func testDiskOfAGuestIsBusy(t *testing.T, h Harness) {
	s, c, ctx := h.Drivers.Storage, h.containers(), t.Context()
	d := disk(t, h, driver.DiskSpec{Name: name(t, "d"), Kind: driver.Dataset, Size: 1 << 30, Labels: labels(t)})
	g := c.guest(t, h, "on", d.Path)
	is(t, "destroying a guest's disk", s.DestroyDisk(ctx, d.Name), driver.ErrBusy)
	mustNot(t, "removing the guest", c.Driver.Remove(ctx, g))
	mustNot(t, "destroying the disk", s.DestroyDisk(ctx, d.Name))
}

func testPool(t *testing.T, h Harness) {
	p, err := h.Drivers.Storage.Pool(t.Context())
	mustNot(t, "reading the pool", err)
	if p.Name == "" || p.Health == "" || p.Size == 0 || p.Free > p.Size {
		t.Errorf("pool: %+v", p)
	}
	if v, err := h.Drivers.Storage.Version(t.Context()); err != nil || v == "" {
		t.Errorf("version: %q, %v", v, err)
	}
}
