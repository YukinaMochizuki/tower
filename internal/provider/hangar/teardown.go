package hangar

import (
	"context"
	"errors"
	"fmt"
	"slices"

	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
)

// Teardown puts the target in teardown for good and deletes what hangar
// holds of it, as hangar lets it go; left is what hangar still holds, as
// kind/name, and it is called again until left is empty.
func (t *Target) Teardown(ctx context.Context) (left []string, err error) {
	// Under the lock makeSystem holds, so nothing it makes outlives a pass.
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tearingDown.Store(true)
	var errs []error
	del := func(kind, name string, meta *hangarmeta.ObjectMeta, wait bool, deleteFn func(context.Context, string) error) {
		left = append(left, kind+"/"+name)
		if wait || meta.GetDeleteTime() != nil {
			return
		}
		if err := deleteFn(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("deleting %s %s: %w", kind, name, err))
		}
	}

	cts, err := t.api.Containers().List(ctx, t.selector())
	if err != nil {
		return nil, err
	}
	mounted := map[string]bool{}
	mine := 0
	for _, ct := range cts {
		meta := ct.GetMetadata()
		if !t.mine(meta.GetLabels()) {
			continue
		}
		mine++
		for _, v := range ct.GetSpec().GetVolumes() {
			mounted[v.GetSource()] = true
		}
		del("container", meta.GetName(), meta, false, t.api.Containers().Delete)
		t.procs.forget(meta.GetUid())
	}
	// Whoever's container is on a network holds it.
	all, err := t.api.Containers().List(ctx, "")
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	for _, ct := range all {
		for _, i := range ct.GetSpec().GetInterfaces() {
			used[i.GetNetwork()] = true
		}
	}

	disks, err := t.api.Disks().List(ctx, t.selector())
	if err != nil {
		return nil, err
	}
	// A disk goes once no container mounts it: hangar refuses it before.
	for _, d := range disks {
		if meta := d.GetMetadata(); t.mine(meta.GetLabels()) {
			del("disk", meta.GetName(), meta, mounted[meta.GetName()], t.api.Disks().Delete)
		}
	}
	nets, err := t.api.Networks().List(ctx, t.selector())
	if err != nil {
		return nil, err
	}
	// A network goes once nothing is on it.
	for _, n := range nets {
		if meta := n.GetMetadata(); t.mine(meta.GetLabels()) {
			del("network", meta.GetName(), meta, used[meta.GetName()], t.api.Networks().Delete)
		}
	}
	// hangar keeps an image's base until the disks made from it go.
	imgs, err := t.api.CustomImages().List(ctx, t.selector())
	if err != nil {
		return nil, err
	}
	for _, img := range imgs {
		if meta := img.GetMetadata(); t.mine(meta.GetLabels()) {
			del("customimage", meta.GetName(), meta, false, t.api.CustomImages().Delete)
		}
	}
	secrets, err := t.api.Secrets().List(ctx, t.selector())
	if err != nil {
		return nil, err
	}
	// The registry login goes once no container of the target may pull.
	for _, sec := range secrets {
		if meta := sec.GetMetadata(); t.mine(meta.GetLabels()) {
			del("secret", meta.GetName(), meta, mine > 0, t.api.Secrets().Delete)
		}
	}
	slices.Sort(left)
	return left, errors.Join(errs...)
}

// tornDown says the target is in teardown (Teardown).
func (t *Target) tornDown() bool { return t.tearingDown.Load() }
