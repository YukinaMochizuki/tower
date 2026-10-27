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
	vms, err := t.api.VirtualMachines().List(ctx, t.selector())
	if err != nil {
		return nil, err
	}
	for _, vm := range vms {
		meta := vm.GetMetadata()
		if !t.mine(meta.GetLabels()) {
			continue
		}
		mounted[vm.GetSpec().GetRootDisk().GetDisk()] = true
		del("virtualmachine", meta.GetName(), meta, false, t.api.VirtualMachines().Delete)
		t.procs.forget(meta.GetUid())
		t.vms.forget(meta.GetUid())
	}
	// Whoever's container or VM is on a network holds it.
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
	allVMs, err := t.api.VirtualMachines().List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, vm := range allVMs {
		for _, i := range vm.GetSpec().GetInterfaces() {
			used[i.GetNetwork()] = true
		}
	}

	// The endpoints and the service go at once; hangar keeps each until its
	// node lets go of it. The security group goes once no interface names
	// it: hangar refuses it before. A hangar without the kinds has none.
	neps, err := t.api.NetworkEndpoints().List(ctx, t.selector())
	if err != nil && !unsupported(err) {
		return nil, err
	}
	for _, ne := range neps {
		if meta := ne.GetMetadata(); t.mine(meta.GetLabels()) {
			del("networkendpoint", meta.GetName(), meta, false, t.api.NetworkEndpoints().Delete)
			t.forgetEndpoint(ne.GetSpec().GetNetwork())
		}
	}
	svcs, err := t.api.EndpointServices().List(ctx, t.selector())
	if err != nil && !unsupported(err) {
		return nil, err
	}
	for _, svc := range svcs {
		if meta := svc.GetMetadata(); t.mine(meta.GetLabels()) {
			del("endpointservice", meta.GetName(), meta, false, t.api.EndpointServices().Delete)
		}
	}
	grouped := map[string]bool{}
	for _, ct := range all {
		for _, i := range ct.GetSpec().GetInterfaces() {
			for _, g := range i.GetSecurityGroups() {
				grouped[g] = true
			}
		}
	}
	for _, vm := range allVMs {
		for _, i := range vm.GetSpec().GetInterfaces() {
			for _, g := range i.GetSecurityGroups() {
				grouped[g] = true
			}
		}
	}
	sgs, err := t.api.SecurityGroups().List(ctx, t.selector())
	if err != nil && !unsupported(err) {
		return nil, err
	}
	for _, sg := range sgs {
		if meta := sg.GetMetadata(); t.mine(meta.GetLabels()) {
			del("securitygroup", meta.GetName(), meta, grouped[meta.GetName()], t.api.SecurityGroups().Delete)
		}
	}

	disks, err := t.api.Disks().List(ctx, t.selector())
	if err != nil {
		return nil, err
	}
	// A disk goes once no container mounts it and no VM boots from it:
	// hangar refuses it before.
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
