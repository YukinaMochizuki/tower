package fake

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

type storage struct{ h *Host }

func (s storage) Version(context.Context) (string, error) { return Version, nil }

func (s storage) EnsureDisk(_ context.Context, spec driver.DiskSpec) (driver.Disk, error) {
	var out driver.Disk
	err := s.h.change("storage.EnsureDisk", func() error {
		if d := s.h.st.Disks[spec.Name]; d != nil {
			if d.Spec.Kind != spec.Kind || d.Spec.From != spec.From {
				return fmt.Errorf("disk %s is a %v from %q: %w", spec.Name, d.Spec.Kind, d.Spec.From, driver.ErrConflict)
			}
			out = s.h.diskOf(spec.Name, d)
			return nil
		}
		d := &disk{Spec: spec, Files: map[string][]byte{}, Created: s.h.clock.Now()}
		d.Spec.Labels = maps.Clone(spec.Labels)
		if spec.From != "" {
			g := s.h.st.Goldens[spec.From]
			if g == nil {
				return fmt.Errorf("golden %s: %w", spec.From, driver.ErrNotFound)
			}
			if g.Kind != spec.Kind {
				return fmt.Errorf("golden %s is of a %v, not a %v: %w", spec.From, g.Kind, spec.Kind, driver.ErrConflict)
			}
			d.Files, d.Base = maps.Clone(g.Files), filesSize(g.Files)
		}
		if err := s.h.room(Overhead); err != nil {
			return err
		}
		s.h.st.Disks[spec.Name] = d
		out = s.h.diskOf(spec.Name, d)
		return nil
	})
	return out, err
}

// diskOf is the disk as the driver lists it. The caller holds h.mu.
func (h *Host) diskOf(name string, d *disk) driver.Disk {
	spec := d.Spec
	spec.Labels = maps.Clone(spec.Labels)
	return driver.Disk{DiskSpec: spec, Path: diskPath(name), Used: uint64(max(0, filesSize(d.Files)-d.Base)), Created: d.Created}
}

func (s storage) Disks(context.Context) ([]driver.Disk, error) {
	var out []driver.Disk
	err := s.h.read("storage.Disks", func() error {
		for _, name := range slices.Sorted(maps.Keys(s.h.st.Disks)) {
			out = append(out, s.h.diskOf(name, s.h.st.Disks[name]))
		}
		return nil
	})
	return out, err
}

func (s storage) DestroyDisk(_ context.Context, name string) error {
	return s.h.change("storage.DestroyDisk", func() error {
		for k, g := range s.h.st.Guests {
			if diskName(g.Spec.Disk) == name {
				return fmt.Errorf("disk %s is %s's: %w", name, k, driver.ErrBusy)
			}
		}
		delete(s.h.st.Disks, name)
		return nil
	})
}

func (s storage) Capture(_ context.Context, diskName, name string, labels driver.Labels) (driver.Golden, error) {
	var out driver.Golden
	err := s.h.change("storage.Capture", func() error {
		if g := s.h.st.Goldens[name]; g != nil {
			out = s.h.goldenOf(name, g)
			return nil
		}
		d := s.h.st.Disks[diskName]
		if d == nil {
			return fmt.Errorf("disk %s: %w", diskName, driver.ErrNotFound)
		}
		if err := s.h.room(Overhead + uint64(filesSize(d.Files))); err != nil {
			return err
		}
		g := &golden{Kind: d.Spec.Kind, Files: maps.Clone(d.Files), Labels: maps.Clone(labels), Created: s.h.clock.Now()}
		s.h.st.Goldens[name] = g
		out = s.h.goldenOf(name, g)
		return nil
	})
	return out, err
}

// goldenOf is the golden as the driver lists it. The caller holds h.mu.
func (h *Host) goldenOf(name string, g *golden) driver.Golden {
	clones := 0
	for _, d := range h.st.Disks {
		if d.Spec.From == name {
			clones++
		}
	}
	return driver.Golden{Name: name, Kind: g.Kind, Size: Overhead + uint64(filesSize(g.Files)), Clones: clones, Labels: maps.Clone(g.Labels), Created: g.Created}
}

func (s storage) Goldens(context.Context) ([]driver.Golden, error) {
	var out []driver.Golden
	err := s.h.read("storage.Goldens", func() error {
		for _, name := range slices.Sorted(maps.Keys(s.h.st.Goldens)) {
			out = append(out, s.h.goldenOf(name, s.h.st.Goldens[name]))
		}
		return nil
	})
	return out, err
}

func (s storage) DestroyGolden(_ context.Context, name string) error {
	return s.h.change("storage.DestroyGolden", func() error {
		g := s.h.st.Goldens[name]
		if g == nil {
			return nil
		}
		if n := s.h.goldenOf(name, g).Clones; n > 0 {
			return fmt.Errorf("golden %s has %d clones: %w", name, n, driver.ErrBusy)
		}
		delete(s.h.st.Goldens, name)
		return nil
	})
}

func (s storage) Pool(context.Context) (driver.Pool, error) {
	var out driver.Pool
	err := s.h.read("storage.Pool", func() error {
		out = driver.Pool{Name: "fake", Health: s.h.health, Size: s.h.st.PoolSize, Free: s.h.freeLocked()}
		return nil
	})
	return out, err
}
