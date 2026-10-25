package hostd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// Sweep removes from the host what this hostd made that no Instance or
// Golden of its target accounts for any more and is older than the grace,
// as a delete missed while hostd was down or disconnected leaves: guests,
// their disks, goldens, then networks no guest is on; and the images of
// tower's registry it pulled that no Instance or Golden of its target
// names, once they have gone unnamed for the grace (an image's own time is
// its build's, the epoch: hostd counts from when it first saw it unnamed,
// afresh after a restart). An image a container is made of stays. A list it could not
// read, from the API or from a driver, stops it: a failed list must never
// read as nothing being wanted. What another hostd made, or anything not
// made by one, it never touches.
func (h *Host) Sweep(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	instances, _, err := h.c.ListAll(ctx, registry.Instance, client.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing instances: %w", err)
	}
	goldens, _, err := h.c.ListAll(ctx, registry.Golden, client.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing goldens: %w", err)
	}
	uids, artifacts := map[string]bool{}, map[string]bool{}
	for _, m := range instances {
		in := m.(*envv1alpha1.Instance)
		if in.GetSpec().GetTarget() == h.cfg.Target {
			uids[in.GetMetadata().GetUid()] = true
			artifacts[in.GetSpec().GetImage()] = true
		}
	}
	for _, m := range goldens {
		g := m.(*envv1alpha1.Golden)
		if a := g.GetStatus().GetArtifact(); a != "" && g.GetSpec().GetTarget() == h.cfg.Target {
			artifacts[a] = true
		}
	}
	old := func(created time.Time) bool { return h.clock.Since(created) >= h.cfg.SweepGrace }
	gone := func(l driver.Labels, created time.Time) bool {
		return h.mine(l) && !uids[l[driver.LabelUID]] && old(created)
	}

	// Everything is listed before anything is removed.
	kinds := []driver.Guests{h.d.Containers, h.d.VMs}
	var guests [][]driver.Guest
	var networks [][]driver.Network
	for _, g := range kinds {
		list, err := g.List(ctx)
		if err != nil {
			return fmt.Errorf("listing guests: %w", err)
		}
		nets, err := g.Networks(ctx)
		if err != nil {
			return fmt.Errorf("listing networks: %w", err)
		}
		guests, networks = append(guests, list), append(networks, nets)
	}
	disks, err := h.d.Storage.Disks(ctx)
	if err != nil {
		return fmt.Errorf("listing disks: %w", err)
	}
	stored, err := h.d.Storage.Goldens(ctx)
	if err != nil {
		return fmt.Errorf("listing goldens on the host: %w", err)
	}
	images, err := h.d.Images.List(ctx)
	if err != nil {
		return fmt.Errorf("listing images: %w", err)
	}

	var errs []error
	for i, g := range kinds {
		for _, gu := range guests[i] {
			if gone(gu.Labels, gu.Created) {
				h.log.Info("sweeping a guest no instance accounts for", "guest", gu.Name, "instance", gu.Labels[driver.LabelProject]+"/"+gu.Labels[driver.LabelInstance])
				errs = append(errs, g.Remove(ctx, gu.Name))
			}
		}
	}
	for _, d := range disks {
		if gone(d.Labels, d.Created) {
			h.log.Info("sweeping a disk no instance accounts for", "disk", d.Name)
			errs = append(errs, h.d.Storage.DestroyDisk(ctx, d.Name))
		}
	}
	for _, g := range stored {
		if !h.mine(g.Labels) || artifacts[g.Name] || g.Deferred || !old(g.Created) {
			continue
		}
		h.log.Info("sweeping a golden no Golden names", "golden", g.Name, "clones", g.Clones)
		errs = append(errs, h.d.Storage.DestroyGolden(ctx, g.Name))
	}
	errs = append(errs, h.sweepImages(ctx, images, artifacts))
	for i, g := range kinds {
		for _, n := range networks[i] {
			// One a guest is still on is busy, and stays.
			if h.mine(n.Labels) && old(n.Created) {
				if err := g.RemoveNetwork(ctx, n.Name); err != nil && !errors.Is(err, driver.ErrBusy) {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// sweepImages removes the images no Instance or Golden names, once they
// have gone unnamed for the grace. The caller holds h.mu.
func (h *Host) sweepImages(ctx context.Context, images []driver.HostImage, named map[string]bool) error {
	now := h.clock.Now()
	seen := map[string]bool{}
	var errs []error
	for _, im := range images {
		seen[im.ID] = true
		if slices.ContainsFunc(im.Refs, func(r string) bool { return named[r] }) {
			delete(h.unnamedImages, im.ID)
			continue
		}
		since, ok := h.unnamedImages[im.ID]
		if !ok {
			h.unnamedImages[im.ID] = now
			continue
		}
		if now.Sub(since) < h.cfg.SweepGrace {
			continue
		}
		h.log.Info("sweeping an image nothing names", "image", im.ID, "refs", im.Refs)
		removed := true
		for _, r := range im.Refs {
			if err := h.d.Images.Remove(ctx, r); errors.Is(err, driver.ErrBusy) {
				removed = false
			} else if err != nil {
				errs = append(errs, err)
				removed = false
			}
		}
		if removed {
			delete(h.unnamedImages, im.ID)
			for _, r := range im.Refs {
				h.pulls.forget(r)
			}
		}
	}
	for id := range h.unnamedImages {
		if !seen[id] {
			delete(h.unnamedImages, id)
		}
	}
	return errors.Join(errs...)
}
