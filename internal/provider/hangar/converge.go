package hangar

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarnetwork "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	hangarsecrets "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// InstanceReady is the condition an Instance's status carries: whether it
// runs, or why not.
const InstanceReady = "Ready"

// What a container is made with when its Instance does not say.
const (
	// NetworkCIDR is every workstream network's range. Networks are apart
	// from one another, so they may all have the same.
	NetworkCIDR = "10.88.0.0/22"
	// DefaultDisk is a /workspace Disk's size.
	DefaultDisk = "10Gi"
	// WorkspaceDir is where a container's Disk is mounted.
	WorkspaceDir = "/workspace"
	// ReposDir is where a box's checkouts after its first go, one
	// directory each (the apiserver's box controller).
	ReposDir = "/repos"
)

// keepAliveCommand is a container's process: it waits, and ends on
// SIGTERM, so a stop ends it at once. tower runs everything else through
// tower-guest, as a devcontainer's tool overrides the image's command; a
// hostd container runs the image's own entrypoint under tower-guest init,
// which a hangar container cannot, as tower-guest is written in only once
// the container runs.
var keepAliveCommand = []string{"/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 86400 & wait $!; done"}

// newController follows the target's Instances through tower's API. Each
// is read back from hangar at every resync too, which catches what hangar
// did by itself.
func (t *Target) newController() *controller.Controller {
	return &controller.Controller{
		Name: "hangar-" + t.cfg.Target, Source: controller.APISource{Client: t.c}, Clock: t.clock, Log: t.log,
		Resync: t.cfg.ReportEvery, Reconciler: controller.ReconcilerFunc(t.reconcile),
		Watches: []controller.Watch{{Kind: registry.Instance.Kind, Map: controller.Self}},
	}
}

func (t *Target) reconcile(ctx context.Context, key controller.Key) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	o, err := t.c.Get(ctx, registry.Instance, client.Key{Project: key.Project, Name: key.Name})
	// NotFound alone says the Instance is gone; any other error says
	// nothing of it, and hangar is left as it is.
	if connect.CodeOf(err) == connect.CodeNotFound {
		return t.release(ctx, key, "")
	}
	if err != nil {
		return err
	}
	in := o.(*envv1alpha1.Instance)
	if in.GetSpec().GetTarget() != t.cfg.Target {
		return nil
	}
	// What an earlier Instance of the name left goes as hangar lets it,
	// read again at each resync; it holds up nothing of this one, whose
	// objects are named apart.
	if err := t.release(ctx, key, in.GetMetadata().GetUid()); err != nil && !errors.Is(err, controller.ErrWaiting) {
		return err
	}
	ob, err := t.converge(ctx, in)
	if err != nil {
		return err
	}
	if err := t.report(ctx, in, ob); err != nil {
		return err
	}
	if ob.wait {
		return fmt.Errorf("instance %s/%s: %s: %w", key.Project, key.Name, ob.msg, controller.ErrWaiting)
	}
	return nil
}

// observed is what a reconcile found of an instance in hangar.
type observed struct {
	phase       envv1alpha1.InstancePhase
	reason, msg string
	// wait says hangar is still moving toward the spec: read it again soon.
	wait      bool
	address   string
	started   *timestamppb.Timestamp
	processes int32
}

func failed(reason string, err error) observed {
	return observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED, reason: reason, msg: err.Error()}
}

func pending(reason, msg string) observed {
	return observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_PENDING, reason: reason, msg: msg, wait: true}
}

// labels are what the provider sets on an instance's objects.
func (t *Target) labels(in *envv1alpha1.Instance) map[string]string {
	m := in.GetMetadata()
	return map[string]string{LabelTarget: t.cfg.Target, LabelUID: m.GetUid(), LabelProject: m.GetProject(), LabelInstance: m.GetName()}
}

// converge makes hangar hold the instance as its spec says, and returns
// what it holds now. Its error is one to read everything again for.
func (t *Target) converge(ctx context.Context, in *envv1alpha1.Instance) (observed, error) {
	spec := in.GetSpec()
	switch spec.GetKind() {
	case envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER:
	case envv1alpha1.InstanceKind_INSTANCE_KIND_VM:
		return t.convergeVM(ctx, in)
	default:
		return failed("Unsupported", fmt.Errorf("a hangar target runs containers and VMs, not %v", spec.GetKind())), nil
	}
	name := objectName(in.GetMetadata().GetUid())
	ct, err := t.api.Containers().Get(ctx, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return observed{}, err
	}
	disk, err := t.api.Disks().Get(ctx, name)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return observed{}, err
	}
	// What was reported made is gone from hangar: tower decides what to do
	// about it.
	switch in.GetStatus().GetPhase() {
	case envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED, envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE:
		if ct == nil || disk == nil || ct.GetMetadata().GetDeleteTime() != nil {
			return observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE, reason: "Gone", msg: "the instance is not in hangar any more"}, nil
		}
	}
	labels := t.labels(in)
	network := ""
	if spec.GetNetwork() != "" {
		network = networkName(t.cfg.Target, in.GetMetadata().GetProject(), spec.GetNetwork())
		if err := t.ensureNetwork(ctx, network, in.GetMetadata().GetProject(), spec.GetNetwork()); err != nil {
			return t.applyFailed(err)
		}
	}
	if disk == nil {
		size := spec.GetResources().GetDisk()
		if size == "" {
			size = DefaultDisk
		}
		var source *hangarstorage.DiskSource
		if g := spec.GetDiskGolden(); g != "" {
			var wait *observed
			if source, size, wait, err = t.goldenSource(ctx, in, g, size); err != nil {
				return observed{}, err
			}
			if wait != nil {
				return *wait, nil
			}
		}
		ds := &hangarstorage.DiskSpec{Size: size, Source: source}
		if g := spec.GetDiskGrowth(); g != nil {
			// One cloned from a golden starts at least at its size, which hangar
			// refuses a max under.
			ds.Grow = &hangarstorage.DiskGrow{Max: atLeast(g.GetWorkspaceMax(), size), Threshold: g.GetThreshold(), Step: g.GetStep()}
		}
		_, err := t.api.Disks().Apply(ctx, &hangarstorage.Disk{
			Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: labels},
			Spec:     ds,
		})
		if err != nil {
			return t.applyFailed(err)
		}
	}
	pull, err := t.ensurePullSecret(ctx)
	if err != nil {
		return t.applyFailed(err)
	}
	want := t.containerSpec(in, name, network, pull)
	if ct == nil || !proto.Equal(ct.GetSpec(), want) {
		if ct.GetMetadata().GetDeleteTime() != nil {
			return pending("Deleting", "an earlier container of the name is being deleted"), nil
		}
		meta := &hangarmeta.ObjectMeta{Name: name, Labels: labels}
		if ct, err = t.api.Containers().Apply(ctx, &hangarcompute.Container{Metadata: meta, Spec: want}); err != nil {
			return t.applyFailed(err)
		}
	}
	return t.observe(ctx, in, containerObj(ct))
}

// applyFailed is what an apply hangar refused says of the instance: a
// project out of quota is out of space, and one hangar refuses for its
// spec failed; anything else is read again. An alias another machine
// holds on the network is waited for: tower's admission gives an alias to
// one box or machine of a workstream, so what holds it is most likely an
// earlier instance of the same, being deleted, whose names hangar holds
// until it is gone; the provider does not know which, and says what hangar
// said.
func (t *Target) applyFailed(err error) (observed, error) {
	if connect.CodeOf(err) == connect.CodeFailedPrecondition && aliasHeld.MatchString(err.Error()) {
		return pending("AliasHeld", "waiting for hangar to free the alias, which another machine on the network holds: "+err.Error()), nil
	}
	switch connect.CodeOf(err) {
	case connect.CodeResourceExhausted:
		return failed("NoSpace", fmt.Errorf("%w: %w", err, provider.ErrNoSpace)), nil
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodePermissionDenied:
		return failed("Refused", err), nil
	}
	return observed{}, err
}

// aliasHeld matches hangar's refusal of an alias another machine holds on
// the network (hangar: internal/apiserver/aliases.go, checkAliasesFree),
// which carries no code or field of its own but its words.
var aliasHeld = regexp.MustCompile(`alias "[^"]+" is held on network "[^"]+" by `)

// containerSpec is the Container the instance is, as hangar keeps what
// its client wrote: one read back equal to it needs no apply.
func (t *Target) containerSpec(in *envv1alpha1.Instance, name, network, pull string) *hangarcompute.ContainerSpec {
	spec := in.GetSpec()
	s := &hangarcompute.ContainerSpec{
		Image:         spec.GetImage(),
		Command:       slices.Clone(keepAliveCommand),
		RestartPolicy: hangarcompute.RestartPolicy_RESTART_POLICY_ALWAYS,
		RunState:      hangarcompute.ContainerRunState_CONTAINER_RUN_STATE_RUNNING,
		Volumes:       []*hangarcompute.ContainerVolume{{Type: "volume", Source: name, Target: WorkspaceDir}},
	}
	if spec.GetRunState() == envv1alpha1.RunState_RUN_STATE_STOPPED {
		s.RunState = hangarcompute.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	}
	if r := spec.GetResources(); r.GetCpu() != "" || r.GetMemory() != "" {
		s.Resources = &hangarcompute.Resources{Cpu: r.GetCpu(), Memory: r.GetMemory()}
	}
	if g := spec.GetDiskGrowth(); g != nil && g.GetRoot() != "" {
		if s.Resources == nil {
			s.Resources = &hangarcompute.Resources{}
		}
		s.Resources.Disk = g.GetRoot()
		s.Resources.DiskGrow = &hangarcompute.DiskGrow{Max: atLeast(g.GetRootMax(), g.GetRoot()), Threshold: g.GetThreshold(), Step: g.GetStep()}
	}
	for _, k := range slices.Sorted(maps.Keys(spec.GetEnv())) {
		s.Env = append(s.Env, &hangarcompute.EnvVar{Name: k, Value: spec.GetEnv()[k]})
	}
	s.Interfaces = interfaces(network, spec.GetAlias())
	if pull != "" {
		s.ImagePullSecrets = []string{pull}
	}
	return s
}

// ensureNetwork makes a workstream's network once, with its endpoint to
// the system container's server.
func (t *Target) ensureNetwork(ctx context.Context, name, project, network string) error {
	n, err := t.api.Networks().Get(ctx, name)
	if err == nil {
		if n.GetMetadata().GetDeleteTime() != nil {
			return fmt.Errorf("network %s is being deleted; it is made again once it is gone", name)
		}
		t.ensureNetworkEndpoint(ctx, name, project)
		return nil
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err = t.api.Networks().Apply(ctx, &hangarnetwork.Network{
		Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: map[string]string{LabelTarget: t.cfg.Target, LabelProject: project, LabelNetwork: label(network)}},
		Spec:     &hangarnetwork.NetworkSpec{Ipv4Cidr: NetworkCIDR},
	})
	if err != nil {
		return err
	}
	t.ensureNetworkEndpoint(ctx, name, project)
	return nil
}

// ensurePullSecret makes the target's registry login a hangar Secret once
// and returns its name, "" with none. A login changed in tower is a new
// target's (internal/apiserver/hangar.go), which writes it again.
func (t *Target) ensurePullSecret(ctx context.Context) (string, error) {
	if len(t.cfg.PullSecret) == 0 {
		return "", nil
	}
	name := pullSecretName(t.cfg.Target)
	t.pullMu.Lock()
	defer t.pullMu.Unlock()
	if t.pullWritten {
		return name, nil
	}
	_, err := t.api.Secrets().Apply(ctx, &hangarsecrets.Secret{
		Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: map[string]string{LabelTarget: t.cfg.Target}},
		Spec: &hangarsecrets.SecretSpec{
			Type: hangarsecrets.SecretType_SECRET_TYPE_REGISTRY_AUTH,
			Data: map[string][]byte{".dockerconfigjson": t.cfg.PullSecret},
		},
	})
	if err != nil {
		return "", err
	}
	t.pullWritten = true
	return name, nil
}

// observe reads back the machine of the instance's Container or
// VirtualMachine: whether it runs, and where. A VM is RUNNING once its
// machine runs, as a container is; what is run in it waits for its sshd
// (vmOps).
func (t *Target) observe(ctx context.Context, in *envv1alpha1.Instance, obj machineObj) (observed, error) {
	stopped := obj.stopped
	m, err := t.api.Machine(ctx, obj.machine())
	ready := obj.ready
	if ready.GetReason() == "ImagePullSecretNotFound" {
		// Gone from hangar since it was written: written again.
		t.pullMu.Lock()
		t.pullWritten = false
		t.pullMu.Unlock()
	}
	if pullFailed[ready.GetReason()] {
		// hangar keeps trying it, but nothing changes it but a new image or
		// login, which is a new instance: tower decides.
		return observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED, reason: "PullFailed", msg: ready.GetMessage()}, nil
	}
	if errors.Is(err, ErrNotFound) || (err == nil && !ownedBy(m, obj.uid)) {
		if ready.GetMessage() != "" {
			return pending(ready.GetReason(), ready.GetMessage()), nil
		}
		return pending("Placing", "hangar has not placed its machine yet"), nil
	}
	if err != nil {
		return observed{}, err
	}
	switch m.GetStatus().GetPhase() {
	case hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING:
		if stopped {
			return observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, reason: "Stopping", msg: "hangar is stopping it", wait: true}, nil
		}
		o := observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, reason: "Running", started: m.GetStatus().GetBootTime()}
		o.address = obj.address(m)
		ref := provider.Instance{Project: in.GetMetadata().GetProject(), Name: in.GetMetadata().GetName()}
		n, err := t.processesRunning(ctx, ref, in, obj, m)
		if err != nil {
			// What runs is reported as running; its processes are counted at
			// the next read.
			t.log.Warn("counting an instance's processes failed", "project", ref.Project, "instance", ref.Name, "error", err)
		}
		o.processes = n
		return o, nil
	case hangarcompute.MachinePhase_MACHINE_PHASE_STOPPED:
		if !stopped {
			return pending("Starting", "hangar is starting it"), nil
		}
		t.procs.stopped(obj.uid)
		t.vms.forget(obj.uid)
		return observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED, reason: "Stopped"}, nil
	case hangarcompute.MachinePhase_MACHINE_PHASE_FAILED:
		msg := "its machine failed"
		if ready != nil && ready.GetMessage() != "" {
			msg = ready.GetMessage()
		}
		return observed{phase: envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED, reason: "MachineFailed", msg: msg}, nil
	}
	if ready != nil && ready.GetMessage() != "" {
		return pending(ready.GetReason(), ready.GetMessage()), nil
	}
	return pending("Booting", "hangar is booting its machine"), nil
}

// pullFailed are the reasons of a container's Ready that say its image
// cannot be pulled as it is asked (hangar: internal/node/images.go,
// internal/apiserver/controllers.go).
var pullFailed = map[string]bool{"ImagePullFailed": true, "ImagePullUnauthorized": true}

// addressOf is a machine's address on its first network, "" until it has
// one: hangar gives it with the network's prefix length, as 10.0.0.5/24.
func addressOf(m *hangarcompute.Machine) string {
	ifs := m.GetSpec().GetInterfaces()
	if len(ifs) == 0 {
		return ""
	}
	p, err := netip.ParsePrefix(ifs[0].GetIpv4())
	if err != nil {
		return ""
	}
	return p.Addr().String()
}

// ownedBy reports whether m is the machine of the object of uid, not an
// earlier one's of the same name.
func ownedBy(m *hangarcompute.Machine, uid string) bool {
	for _, o := range m.GetMetadata().GetOwnerReferences() {
		if o.GetUid() == uid {
			return true
		}
	}
	return false
}

func conditionOf(cs []*hangarmeta.Condition, typ string) *hangarmeta.Condition {
	for _, c := range cs {
		if c.GetType() == typ {
			return c
		}
	}
	return nil
}

// report writes the instance's status as found, when it changed.
func (t *Target) report(ctx context.Context, in *envv1alpha1.Instance, o observed) error {
	st := proto.Clone(in.GetStatus()).(*envv1alpha1.InstanceStatus)
	if st == nil {
		st = &envv1alpha1.InstanceStatus{}
	}
	st.ObservedGeneration = in.GetMetadata().GetGeneration()
	st.Phase = o.phase
	st.Address, st.UsedDisk, st.StartTime, st.ProcessesRunning, st.StreamsOpen = "", "", nil, 0, 0
	if o.phase == envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING {
		st.Address, st.StartTime, st.ProcessesRunning = o.address, o.started, o.processes
		st.StreamsOpen = t.streams.Open(provider.Instance{Project: in.GetMetadata().GetProject(), Name: in.GetMetadata().GetName()})
	}
	ready := &metav1.Condition{Type: InstanceReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: o.reason, Message: o.msg, ObservedGeneration: st.ObservedGeneration}
	if o.phase == envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING && !o.wait {
		ready.Status = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	}
	st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), ready, t.clock.Now())
	if proto.Equal(st, in.GetStatus()) {
		return nil
	}
	rv := in.GetMetadata().GetResourceVersion()
	_, err := t.c.UpdateStatus(ctx, registry.Instance, client.Key{Project: in.GetMetadata().GetProject(), Name: in.GetMetadata().GetName()}, st, &rv)
	if connect.CodeOf(err) == connect.CodeAborted {
		return fmt.Errorf("instance %s/%s changed while it was reported; reading it again: %w", in.GetMetadata().GetProject(), in.GetMetadata().GetName(), err)
	}
	return err
}

// release deletes from hangar what this target made for the instance of
// key but for the uid keep (none when empty): what an earlier Instance of
// the name, or a deleted one, left. A disk goes once its container or VM
// has: hangar refuses to delete a disk a machine has, so until then the
// key is read again. Networks go with the sweep.
func (t *Target) release(ctx context.Context, key controller.Key, keep string) error {
	sel := t.selector(LabelProject+"="+key.Project, LabelInstance+"="+key.Name)
	cts, err := t.api.Containers().List(ctx, sel)
	if err != nil {
		return err
	}
	held := map[string]bool{}
	for _, ct := range cts {
		uid := ct.GetMetadata().GetLabels()[LabelUID]
		if uid == keep || !t.mine(ct.GetMetadata().GetLabels()) {
			continue
		}
		held[uid] = true
		if ct.GetMetadata().GetDeleteTime() == nil {
			t.log.Info("deleting a container of an instance gone", "project", key.Project, "instance", key.Name, "container", ct.GetMetadata().GetName())
			if err := t.api.Containers().Delete(ctx, ct.GetMetadata().GetName()); err != nil {
				return err
			}
		}
		t.procs.forget(ct.GetMetadata().GetUid())
	}
	vms, err := t.api.VirtualMachines().List(ctx, sel)
	if err != nil {
		return err
	}
	for _, vm := range vms {
		uid := vm.GetMetadata().GetLabels()[LabelUID]
		if uid == keep || !t.mine(vm.GetMetadata().GetLabels()) {
			continue
		}
		held[uid] = true
		if vm.GetMetadata().GetDeleteTime() == nil {
			t.log.Info("deleting a VM of an instance gone", "project", key.Project, "instance", key.Name, "vm", vm.GetMetadata().GetName())
			if err := t.api.VirtualMachines().Delete(ctx, vm.GetMetadata().GetName()); err != nil {
				return err
			}
		}
		t.procs.forget(vm.GetMetadata().GetUid())
		t.vms.forget(vm.GetMetadata().GetUid())
	}
	disks, err := t.api.Disks().List(ctx, sel)
	if err != nil {
		return err
	}
	var waiting []string
	for _, d := range disks {
		uid := d.GetMetadata().GetLabels()[LabelUID]
		if uid == keep || !t.mine(d.GetMetadata().GetLabels()) || d.GetMetadata().GetDeleteTime() != nil {
			continue
		}
		if held[uid] {
			waiting = append(waiting, d.GetMetadata().GetName())
			continue
		}
		if err := t.api.Disks().Delete(ctx, d.GetMetadata().GetName()); err != nil {
			return err
		}
	}
	if len(waiting) > 0 {
		return fmt.Errorf("disks %v wait for their machines to go: %w", waiting, controller.ErrWaiting)
	}
	return nil
}
