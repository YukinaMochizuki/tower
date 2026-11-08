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
	"github.com/YukinaMochizuki/tower/internal/provider"
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
	// NotFound alone says the Instance is gone. Any other error, a refusal
	// included (a token being revoked, say), says nothing of it, so the host
	// is left as it is; what is truly gone, the sweep takes after its grace.
	if connect.CodeOf(err) == connect.CodeNotFound {
		return h.release(ctx, key, "")
	}
	if err != nil {
		return err
	}
	in := o.(*envv1alpha1.Instance)
	if in.GetSpec().GetTarget() != h.cfg.Target {
		// tower shows no other target's; one shown is left to the sweep.
		return nil
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
	// processes are the container's running processes.
	processes int32
	// resizeRefused is why the host keeps a VM's disk at the size it has,
	// with its words; "" when it applied the size asked.
	resizeRefused, resizeMsg string
	// tailnet is how the join of a running container that joins a tailnet
	// stands, nil for any other.
	tailnet *driver.TailnetState
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
	cur, err := g.Inspect(ctx, name)
	if err != nil && !errors.Is(err, driver.ErrNotFound) {
		return observed{}, err
	}
	made := err == nil
	disk, err := h.disk(ctx, diskName(uid))
	if err != nil {
		return observed{}, err
	}
	// What hostd reported made is gone from the host: tower decides what to
	// do about it. A VM whose guest alone went, as a resize cut short does,
	// is made again on its disk (docs/decisions/machine-resize.md).
	switch in.GetStatus().GetPhase() {
	case envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED, envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE:
		if disk == nil || (!made && !vm) {
			return observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE, reason: "Gone", msg: "the instance is not on the host any more"}, nil
		}
	}
	labels := driver.Labels{driver.LabelOwner: h.cfg.Target, driver.LabelUID: uid, driver.LabelProject: ref.GetProject(), driver.LabelInstance: ref.GetName()}
	if !made {
		from := ""
		if gname := spec.GetDiskGolden(); gname != "" && disk == nil {
			o, err := h.c.Get(ctx, registry.Golden, client.Key{Project: ref.GetProject(), Name: gname})
			if err != nil && connect.CodeOf(err) != connect.CodeNotFound {
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
		// A disk already there is the one the guest is made on, whatever
		// became of the golden it was cloned from.
		d := disk
		if d == nil {
			size, _ := quantity.Bytes(spec.GetResources().GetDisk())
			made, err := h.d.Storage.EnsureDisk(ctx, driver.DiskSpec{Name: diskName(uid), Kind: kind, Size: size, From: from, Labels: labels})
			switch {
			case errors.Is(err, driver.ErrNoSpace):
				return failed("NoSpace", err), nil
			case errors.Is(err, driver.ErrNotFound):
				return failed("GoldenNotOnHost", err), nil
			case err != nil:
				return observed{}, err
			}
			d = &made
		}
		cpus, _ := quantity.CPU(spec.GetResources().GetCpu())
		memory, _ := quantity.Bytes(spec.GetResources().GetMemory())
		gs := driver.GuestSpec{Name: name, Disk: d.Path, Network: network, Alias: spec.GetAlias(), CPUs: int(cpus), Memory: memory, Labels: labels}
		if vm {
			err = h.d.VMs.Create(ctx, driver.VMSpec{GuestSpec: gs, BaseImage: spec.GetImage()})
		} else {
			var image string
			image, err = h.image(ctx, spec.GetImage(), controller.Key{Project: ref.GetProject(), Name: ref.GetName()})
			switch {
			case errors.Is(err, driver.ErrNotFound) || errors.Is(err, driver.ErrNoSpace):
				return failed("PullFailed", err), nil
			case err != nil:
				return observed{}, err
			case image == "":
				return observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_PENDING, reason: "Pulling", msg: "its image " + spec.GetImage() + " is being pulled"}, nil
			}
			err = h.d.Containers.Create(ctx, driver.ContainerSpec{GuestSpec: gs, Image: image, Env: spec.GetEnv(), Tailnet: containerTailnet(spec)})
			if errors.Is(err, driver.ErrStartFailed) {
				// A host without tailscale for a box that joins a tailnet.
				return failed("CannotCreate", err), nil
			}
			if errors.Is(err, driver.ErrNotFound) {
				// Gone from the host since it was pulled: pulled again.
				h.pulls.forget(spec.GetImage())
				return observed{}, err
			}
		}
		if errors.Is(err, driver.ErrConflict) {
			return failed("Conflict", err), nil
		}
		if err != nil {
			return observed{}, err
		}
	}
	// A tailnet added, removed or changed since the container was made
	// (docs/decisions/tailnet.md).
	if made && !vm && !sameTailnet(cur.Tailnet, containerTailnet(spec)) {
		err := h.d.Containers.SetTailnet(ctx, name, containerTailnet(spec))
		if errors.Is(err, driver.ErrStartFailed) {
			return failed("CannotCreate", err), nil
		}
		if err != nil {
			return observed{}, err
		}
	}
	var refused, refusedMsg string
	if made && vm && disk != nil {
		if refused, refusedMsg, err = h.resize(ctx, spec.GetResources(), name, disk); err != nil {
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
	o := observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED, reason: "Stopped", guest: &got, disk: disk, resizeRefused: refused, resizeMsg: refusedMsg}
	if got.Running {
		o.phase, o.reason = envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, "Running"
		if !vm {
			if o.processes, err = h.processesRunning(ctx, name); err != nil {
				return observed{}, err
			}
			if spec.GetTailnet() != nil {
				st, err := h.d.Containers.Tailnet(ctx, name)
				if err != nil {
					return observed{}, err
				}
				o.tailnet = &st
			}
		}
	}
	return o, nil
}

// containerTailnet is the tailnet a container instance's spec joins, nil
// for none.
func containerTailnet(spec *envv1alpha1.InstanceSpec) *driver.ContainerTailnet {
	t := spec.GetTailnet()
	if t == nil {
		return nil
	}
	return &driver.ContainerTailnet{Hostname: t.GetHostname(), AcceptRoutes: t.GetAcceptRoutes()}
}

func sameTailnet(a, b *driver.ContainerTailnet) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// resize gives a made VM the size r asks, stopping one running for it; the
// converge that follows starts it again. Its order and what a full pool does
// are in docs/decisions/machine-resize.md.
func (h *Host) resize(ctx context.Context, r *envv1alpha1.Resources, name string, disk *driver.Disk) (refused, msg string, err error) {
	got, err := h.d.VMs.Inspect(ctx, name)
	if err != nil {
		return "", "", err
	}
	cpus, memory, size := got.CPUs, got.Memory, disk.Size
	if c, err := quantity.CPU(r.GetCpu()); err == nil {
		cpus = int(c)
	}
	if m, err := quantity.Bytes(r.GetMemory()); err == nil && m > 0 {
		memory = m
	}
	if s, err := quantity.Bytes(r.GetDisk()); err == nil {
		size = max(size, s)
	}
	grown := false
	if size > disk.Size {
		d, err := h.d.Storage.GrowDisk(ctx, disk.Name, size)
		switch {
		case errors.Is(err, driver.ErrNoSpace):
			refused, msg = "NoSpace", err.Error()
		case err != nil:
			return "", "", err
		default:
			grown = d.Size > disk.Size
		}
	}
	resized := cpus != got.CPUs || memory != got.Memory
	if !grown && !resized {
		return refused, msg, nil
	}
	h.log.Info("resizing a VM", "guest", name, "cpus", cpus, "memory", quantity.FormatMemory(memory), "disk", quantity.FormatDisk(size), "restart", got.Running)
	if got.Running {
		if err := h.d.VMs.Stop(ctx, name); err != nil {
			return "", "", err
		}
	}
	if resized {
		if err := h.d.VMs.Resize(ctx, name, cpus, memory); err != nil {
			return "", "", err
		}
	}
	return refused, msg, nil
}

// image is the name a container of ref is made by on the host, at once
// for an image the host holds, built on it or pulled; else "" while it is
// pulled, apart from the host lock, which a reconcile holds, key being
// reconciled again when the pull ends. An image built on the host is never
// pulled, whether or not it is pushed yet.
func (h *Host) image(ctx context.Context, ref string, key controller.Key) (string, error) {
	local, err := h.d.Images.Local(ctx, ref)
	if !errors.Is(err, driver.ErrNotFound) {
		return local, err
	}
	pulled, err := h.pulls.get(ref, key)
	if err != nil || !pulled {
		return "", err
	}
	// Pulled, and gone from the host since: pulled again.
	h.pulls.forget(ref)
	return "", fmt.Errorf("image %s left the host after it was pulled", ref)
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
	st.Address, st.UsedDisk, st.StartTime, st.ProcessesRunning, st.StreamsOpen = "", "", nil, 0, 0
	if o.guest != nil && o.guest.Running {
		st.Address, st.StartTime, st.ProcessesRunning = o.guest.Address, timestamppb.New(o.guest.StartedAt), o.processes
		st.StreamsOpen = h.streamsOpen(provider.Instance{Project: in.GetMetadata().GetProject(), Name: in.GetMetadata().GetName()})
	}
	if o.disk != nil {
		st.UsedDisk = quantity.FormatDisk(o.disk.Used)
	}
	if in.GetSpec().GetKind() == envv1alpha1.InstanceKind_INSTANCE_KIND_VM && o.guest != nil && o.disk != nil {
		st.Resources = &envv1alpha1.Resources{Cpu: quantity.FormatCPU(int32(o.guest.CPUs)), Memory: quantity.FormatMemory(o.guest.Memory), Disk: quantity.FormatDisk(o.disk.Size)}
		resized := &metav1.Condition{Type: provider.InstanceResized, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Applied", ObservedGeneration: st.ObservedGeneration}
		if o.resizeRefused != "" {
			resized.Status, resized.Reason, resized.Message = metav1.ConditionStatus_CONDITION_STATUS_FALSE, o.resizeRefused, o.resizeMsg
		}
		st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), resized, h.clock.Now())
	}
	if in.GetSpec().GetTailnet() == nil {
		// It goes with the spec's tailnet.
		st.Conditions = slices.DeleteFunc(slices.Clone(st.GetConditions()), func(c *metav1.Condition) bool { return c.GetType() == provider.InstanceTailnetJoined })
	}
	if t := o.tailnet; t != nil {
		joined := &metav1.Condition{Type: provider.InstanceTailnetJoined, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: t.Reason, Message: t.Message, ObservedGeneration: st.ObservedGeneration}
		if t.Reason == provider.TailnetJoined {
			joined.Status = metav1.ConditionStatus_CONDITION_STATUS_TRUE
		}
		st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), joined, h.clock.Now())
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
