package hangar

import (
	"context"

	"google.golang.org/protobuf/types/known/timestamppb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/client"
)

// Sweep deletes from hangar what this target made that no Instance of it
// accounts for any more, once it is older than the sweep's grace: what a
// reconcile missed, such as an Instance deleted while tower was down. A
// network goes once no container of the target's is on it. It holds the
// target's lock alone, so no reconcile makes anything meanwhile.
func (t *Target) Sweep(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	items, _, err := t.c.ListAll(ctx, registry.Instance, client.ListOptions{})
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, o := range items {
		in := o.(*envv1alpha1.Instance)
		if in.GetSpec().GetTarget() == t.cfg.Target {
			live[in.GetMetadata().GetUid()] = true
		}
	}
	old := func(created *timestamppb.Timestamp) bool {
		return created != nil && t.clock.Since(created.AsTime()) >= t.cfg.SweepGrace
	}
	cts, err := t.api.Containers().List(ctx, t.selector())
	if err != nil {
		return err
	}
	used := map[string]bool{}
	held := map[string]bool{}
	for _, ct := range cts {
		meta := ct.GetMetadata()
		if !t.mine(meta.GetLabels()) {
			continue
		}
		uid := meta.GetLabels()[LabelUID]
		held[meta.GetName()] = true
		// Being deleted, it is still on its network until it is gone.
		for _, i := range ct.GetSpec().GetInterfaces() {
			used[i.GetNetwork()] = true
		}
		if live[uid] || !old(meta.GetCreateTime()) {
			continue
		}
		if meta.GetDeleteTime() == nil {
			t.log.Info("sweeping a container no instance accounts for", "container", meta.GetName(), "uid", uid)
			if err := t.api.Containers().Delete(ctx, meta.GetName()); err != nil {
				return err
			}
		}
		t.procs.forget(meta.GetUid())
	}
	disks, err := t.api.Disks().List(ctx, t.selector())
	if err != nil {
		return err
	}
	for _, d := range disks {
		meta := d.GetMetadata()
		// A disk goes once the container of its name has: hangar refuses to
		// delete one a machine has.
		if !t.mine(meta.GetLabels()) || live[meta.GetLabels()[LabelUID]] || !old(meta.GetCreateTime()) ||
			held[meta.GetName()] || meta.GetDeleteTime() != nil {
			continue
		}
		t.log.Info("sweeping a disk no instance accounts for", "disk", meta.GetName())
		if err := t.api.Disks().Delete(ctx, meta.GetName()); err != nil {
			return err
		}
	}
	nets, err := t.api.Networks().List(ctx, t.selector())
	if err != nil {
		return err
	}
	for _, n := range nets {
		meta := n.GetMetadata()
		if !t.mine(meta.GetLabels()) || used[meta.GetName()] || !old(meta.GetCreateTime()) || meta.GetDeleteTime() != nil {
			continue
		}
		t.log.Info("sweeping a network no container is on", "network", meta.GetName())
		if err := t.api.Networks().Delete(ctx, meta.GetName()); err != nil {
			return err
		}
	}
	// A registry login the target no longer pulls with goes.
	secrets, err := t.api.Secrets().List(ctx, t.selector())
	if err != nil {
		return err
	}
	for _, sec := range secrets {
		meta := sec.GetMetadata()
		if !t.mine(meta.GetLabels()) || meta.GetDeleteTime() != nil ||
			(len(t.cfg.PullSecret) > 0 && meta.GetName() == pullSecretName(t.cfg.Target)) {
			continue
		}
		t.log.Info("sweeping a registry login the target no longer pulls with", "secret", meta.GetName())
		if err := t.api.Secrets().Delete(ctx, meta.GetName()); err != nil {
			return err
		}
	}
	return nil
}
