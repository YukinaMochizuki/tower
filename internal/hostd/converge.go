package hostd

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// InstanceReady is the condition an Instance's status carries: whether it
// runs, or why not.
const InstanceReady = "Ready"

// newController follows the target's Instances, and the Goldens their
// disks are cloned from, through the API. Each Instance is read back from
// the host at every resync too, which catches what the host did by itself.
func (h *Host) newController() *controller.Controller {
	return &controller.Controller{
		Name: "hostd-" + h.cfg.Target, Source: controller.APISource{Client: h.c}, Clock: h.clock, Log: h.log,
		Resync: h.cfg.ReportEvery, Reconciler: controller.ReconcilerFunc(h.reconcile),
		Watches: []controller.Watch{
			{Kind: registry.Instance.Kind, Map: controller.Self},
			// A golden made ready lets an instance cloned from it be made.
			{Kind: registry.Golden.Kind, Map: h.clonesOf, SkipResync: true},
		},
	}
}

// clonesOf are the instances whose disks are cloned from a golden.
func (h *Host) clonesOf(ctx context.Context, o store.Object) []controller.Key {
	items, _, err := h.c.ListAll(ctx, registry.Instance, client.ListOptions{Project: o.Project})
	if err != nil {
		// Their resync reads them again.
		h.log.Warn("listing the instances of a golden failed", "project", o.Project, "golden", o.Name, "error", err)
		return nil
	}
	var keys []controller.Key
	for _, m := range items {
		in := m.(*envv1alpha1.Instance)
		if in.GetSpec().GetDiskGolden() == o.Name {
			keys = append(keys, controller.Key{Project: o.Project, Name: in.GetMetadata().GetName()})
		}
	}
	return keys
}

func (h *Host) reconcile(ctx context.Context, key controller.Key) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	o, err := h.c.Get(ctx, registry.Instance, client.Key{Project: key.Project, Name: key.Name})
	if code := connect.CodeOf(err); code == connect.CodeNotFound || code == connect.CodePermissionDenied {
		return h.release(ctx, key, "")
	}
	if err != nil {
		return err
	}
	in := o.(*envv1alpha1.Instance)
	if in.GetSpec().GetTarget() != h.cfg.Target {
		return h.release(ctx, key, "")
	}
	if err := h.release(ctx, key, in.GetMetadata().GetUid()); err != nil {
		return err
	}
	st, err := h.converge(ctx, in)
	if err != nil {
		// What runs keeps running, and its status stays as it was; the
		// controller reads it all again.
		return err
	}
	return h.report(ctx, in, st)
}

// observed is what a reconcile found of an instance on the host.
type observed struct {
	phase       envv1alpha1.InstancePhase
	reason, msg string
	guest       *driver.Guest
	disk        *driver.Disk
}

func failed(reason string, err error) observed {
	return observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED, reason: reason, msg: err.Error()}
}

// converge makes the host hold the instance as its spec says, and returns
// what it holds now. Its error is one to read everything again for.
func (h *Host) converge(ctx context.Context, in *envv1alpha1.Instance) (observed, error) {
	spec, uid := in.GetSpec(), in.GetMetadata().GetUid()
	ref := in.GetMetadata()
	g := h.guests(in)
	vm := spec.GetKind() == envv1alpha1.InstanceKind_INSTANCE_KIND_VM
	name := guestName(uid)
	_, err := g.Inspect(ctx, name)
	if err != nil && !errors.Is(err, driver.ErrNotFound) {
		return observed{}, err
	}
	made := err == nil
	disk, err := h.disk(ctx, diskName(uid))
	if err != nil {
		return observed{}, err
	}
	// What hostd reported made is gone from the host: tower decides what
	// to do about it.
	switch in.GetStatus().GetPhase() {
	case envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED, envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE:
		if !made || disk == nil {
			return observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE, reason: "Gone", msg: "the instance is not on the host any more"}, nil
		}
	}
	labels := driver.Labels{driver.LabelOwner: h.cfg.Target, driver.LabelUID: uid, driver.LabelProject: ref.GetProject(), driver.LabelInstance: ref.GetName()}
	if !made {
		from := ""
		if gname := spec.GetDiskGolden(); gname != "" {
			o, err := h.c.Get(ctx, registry.Golden, client.Key{Project: ref.GetProject(), Name: gname})
			// One not there reads to hostd as one it may not read.
			if code := connect.CodeOf(err); err != nil && code != connect.CodeNotFound && code != connect.CodePermissionDenied {
				return observed{}, err
			}
			gd, _ := o.(*envv1alpha1.Golden)
			if gd.GetStatus().GetPhase() != envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY || gd.GetStatus().GetArtifact() == "" {
				return observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_PENDING, reason: "WaitingForGolden", msg: "its golden " + gname + " is not ready"}, nil
			}
			from = gd.GetStatus().GetArtifact()
		}
		network := ""
		if spec.GetNetwork() != "" {
			network = networkName(ref.GetProject(), spec.GetNetwork())
			if err := g.EnsureNetwork(ctx, network, driver.Labels{driver.LabelOwner: h.cfg.Target}); err != nil {
				return observed{}, err
			}
		}
		kind := driver.Dataset
		if vm {
			kind = driver.Volume
		}
		size, _ := quantity.Bytes(spec.GetResources().GetDisk())
		d, err := h.d.Storage.EnsureDisk(ctx, driver.DiskSpec{Name: diskName(uid), Kind: kind, Size: size, From: from, Labels: labels})
		switch {
		case errors.Is(err, driver.ErrNoSpace):
			return failed("NoSpace", err), nil
		case errors.Is(err, driver.ErrNotFound):
			return failed("GoldenNotOnHost", err), nil
		case err != nil:
			return observed{}, err
		}
		cpus, _ := quantity.CPU(spec.GetResources().GetCpu())
		memory, _ := quantity.Bytes(spec.GetResources().GetMemory())
		gs := driver.GuestSpec{Name: name, Disk: d.Path, Network: network, Alias: spec.GetAlias(), CPUs: int(cpus), Memory: memory, Labels: labels}
		if vm {
			err = h.d.VMs.Create(ctx, driver.VMSpec{GuestSpec: gs, BaseImage: spec.GetImage()})
		} else {
			if err := h.d.Images.Pull(ctx, spec.GetImage()); errors.Is(err, driver.ErrNotFound) || errors.Is(err, driver.ErrNoSpace) {
				return failed("PullFailed", err), nil
			} else if err != nil {
				return observed{}, err
			}
			err = h.d.Containers.Create(ctx, driver.ContainerSpec{GuestSpec: gs, Image: spec.GetImage()})
		}
		if errors.Is(err, driver.ErrConflict) {
			return failed("Conflict", err), nil
		}
		if err != nil {
			return observed{}, err
		}
	}
	if spec.GetRunState() == envv1alpha1.RunState_RUN_STATE_STOPPED {
		if err := g.Stop(ctx, name); err != nil {
			return observed{}, err
		}
	} else if err := g.Start(ctx, name); errors.Is(err, driver.ErrStartFailed) {
		return failed("StartFailed", err), nil
	} else if err != nil {
		return observed{}, err
	}
	// Read back: what is reported is what the host has.
	got, err := g.Inspect(ctx, name)
	if err != nil {
		return observed{}, err
	}
	if disk, err = h.disk(ctx, diskName(uid)); err != nil {
		return observed{}, err
	}
	o := observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED, reason: "Stopped", guest: &got, disk: disk}
	if got.Running {
		o.phase, o.reason = envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, "Running"
	}
	return o, nil
}

// disk is the disk of the name, nil when it is not on the host.
func (h *Host) disk(ctx context.Context, name string) (*driver.Disk, error) {
	disks, err := h.d.Storage.Disks(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range disks {
		if d.Name == name && h.mine(d.Labels) {
			return &d, nil
		}
	}
	return nil, nil
}

// report writes the instance's status as found, when it changed.
func (h *Host) report(ctx context.Context, in *envv1alpha1.Instance, o observed) error {
	st := proto.Clone(in.GetStatus()).(*envv1alpha1.InstanceStatus)
	if st == nil {
		st = &envv1alpha1.InstanceStatus{}
	}
	st.ObservedGeneration = in.GetMetadata().GetGeneration()
	st.Phase = o.phase
	st.Address, st.UsedDisk, st.StartTime = "", "", nil
	if o.guest != nil && o.guest.Running {
		st.Address, st.StartTime = o.guest.Address, timestamppb.New(o.guest.StartedAt)
	}
	if o.disk != nil {
		st.UsedDisk = quantity.FormatDisk(o.disk.Used)
	}
	ready := &metav1.Condition{Type: InstanceReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: o.reason, Message: o.msg, ObservedGeneration: st.ObservedGeneration}
	if o.phase == envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING {
		ready.Status = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	}
	st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), ready, h.clock.Now())
	if proto.Equal(st, in.GetStatus()) {
		return nil
	}
	rv := in.GetMetadata().GetResourceVersion()
	_, err := h.c.UpdateStatus(ctx, registry.Instance, client.Key{Project: in.GetMetadata().GetProject(), Name: in.GetMetadata().GetName()}, st, &rv)
	if connect.CodeOf(err) == connect.CodeAborted {
		return fmt.Errorf("instance %s/%s changed while it was reported; reading it again: %w", in.GetMetadata().GetProject(), in.GetMetadata().GetName(), err)
	}
	return err
}

// release removes from the host what this hostd made for the instance of
// key but for the uid keep (none when empty): what an earlier Instance of
// the name, or a deleted one, left. A network nothing is on any more goes
// with it.
func (h *Host) release(ctx context.Context, key controller.Key, keep string) error {
	theirs := func(l driver.Labels) bool {
		return h.mine(l) && l[driver.LabelProject] == key.Project && l[driver.LabelInstance] == key.Name && l[driver.LabelUID] != keep
	}
	var networks []string
	for _, g := range []driver.Guests{h.d.Containers, h.d.VMs} {
		guests, err := g.List(ctx)
		if err != nil {
			return err
		}
		for _, gu := range guests {
			if !theirs(gu.Labels) {
				continue
			}
			h.log.Info("removing a guest of an instance gone", "project", key.Project, "instance", key.Name, "guest", gu.Name)
			if err := g.Remove(ctx, gu.Name); err != nil {
				return err
			}
			if gu.Network != "" {
				networks = append(networks, gu.Network)
			}
		}
	}
	disks, err := h.d.Storage.Disks(ctx)
	if err != nil {
		return err
	}
	for _, d := range disks {
		if theirs(d.Labels) {
			if err := h.d.Storage.DestroyDisk(ctx, d.Name); err != nil {
				return err
			}
		}
	}
	for _, n := range networks {
		if err := h.removeNetworkIfUnused(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

// removeNetworkIfUnused removes a network of this hostd's that no guest is
// on.
func (h *Host) removeNetworkIfUnused(ctx context.Context, name string) error {
	for _, g := range []driver.Guests{h.d.Containers, h.d.VMs} {
		networks, err := g.Networks(ctx)
		if err != nil {
			return err
		}
		for _, n := range networks {
			if n.Name != name || !h.mine(n.Labels) {
				continue
			}
			if err := g.RemoveNetwork(ctx, name); err != nil && !errors.Is(err, driver.ErrBusy) {
				return err
			}
		}
	}
	return nil
}
