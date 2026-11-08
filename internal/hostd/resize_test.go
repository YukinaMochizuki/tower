package hostd_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/hostdtest"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// setResources sets an instance's resources, as tower's machine controller
// does.
func setResources(t *testing.T, w *hostdtest.World, name string, r *envv1alpha1.Resources) {
	t.Helper()
	testutil.Eventually(t, "the resources set", func() (bool, string) {
		in := proto.Clone(w.Instance(t, name)).(*envv1alpha1.Instance)
		in.Spec.Resources = r
		_, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, in)
		if err != nil && connect.CodeOf(err) != connect.CodeAborted {
			t.Fatal(err)
		}
		return err == nil, fmt.Sprint(err)
	})
}

// waitSize waits until hostd reports the instance at size cpu/memory/disk,
// in phase, its Resized condition of the generation reported with reason,
// and returns it.
func waitSize(t *testing.T, w *hostdtest.World, name, size string, phase envv1alpha1.InstancePhase, reason string) *envv1alpha1.Instance {
	t.Helper()
	var in *envv1alpha1.Instance
	testutil.Eventually(t, fmt.Sprintf("instance %s at %s", name, size), func() (bool, string) {
		in = w.Instance(t, name)
		st := in.GetStatus()
		r := st.GetResources()
		c := conditions.Find(st.GetConditions(), provider.InstanceResized)
		return r.GetCpu()+"/"+r.GetMemory()+"/"+r.GetDisk() == size && st.GetPhase() == phase &&
			c.GetReason() == reason && c.GetObservedGeneration() == in.GetMetadata().GetGeneration(), st.String()
	})
	return in
}

// TestAVMIsResized: a running VM given a new size restarts at it, its disk
// grown; a disk the pool has no room for stays as it is, said as NoSpace,
// while its vCPUs are still applied; a disk is never shrunk; a stopped VM
// takes its size and stays stopped.
func TestAVMIsResized(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	vms, storage := w.Fake.Drivers().VMs, w.Fake.Drivers().Storage
	w.Apply(t, "vm", &envv1alpha1.InstanceSpec{
		Target: target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_VM, Image: "ubuntu-24.04",
		Resources: &envv1alpha1.Resources{Cpu: "2", Memory: "4Gi", Disk: "20Gi"},
	})
	in := waitSize(t, w, "vm", "2/4096Mi/20Gi", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, "Applied")
	guest, disk := "tower-"+in.GetMetadata().GetUid(), "tower-"+in.GetMetadata().GetUid()
	before, err := vms.Inspect(t.Context(), guest)
	if err != nil {
		t.Fatal(err)
	}

	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "4", Memory: "8Gi", Disk: "40Gi"})
	waitSize(t, w, "vm", "4/8192Mi/40Gi", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, "Applied")
	after, err := vms.Inspect(t.Context(), guest)
	if err != nil {
		t.Fatal(err)
	}
	if after.CPUs != 4 || after.Memory != 8<<30 || !after.Running || after.Disk != before.Disk {
		t.Errorf("a VM resized is %+v, was %+v", after.GuestSpec, before.GuestSpec)
	}
	if !after.StartedAt.After(before.StartedAt) {
		t.Errorf("a VM resized was not started again: it started at %v, first at %v", after.StartedAt, before.StartedAt)
	}

	// A disk grown alone restarts the VM too: its guest sees the size only as
	// it boots.
	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "4", Memory: "8Gi", Disk: "50Gi"})
	waitSize(t, w, "vm", "4/8192Mi/50Gi", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, "Applied")
	grown, err := vms.Inspect(t.Context(), guest)
	if err != nil {
		t.Fatal(err)
	}
	if !grown.StartedAt.After(after.StartedAt) {
		t.Errorf("a VM whose disk alone grew was not started again: it started at %v, before at %v", grown.StartedAt, after.StartedAt)
	}

	w.Fake.Fail("storage.GrowDisk", fmt.Errorf("zfs set volsize: out of space: %w", driver.ErrNoSpace))
	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "2", Memory: "8Gi", Disk: "80Gi"})
	refused := waitSize(t, w, "vm", "2/8192Mi/50Gi", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, "NoSpace")
	if c := conditions.Find(refused.GetStatus().GetConditions(), provider.InstanceResized); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Errorf("a disk the pool has no room for: Resized %v", c)
	}
	w.Fake.Fail("storage.GrowDisk", nil)
	waitSize(t, w, "vm", "2/8192Mi/80Gi", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, "Applied")

	// A smaller disk asked of hostd grows nothing and shrinks nothing.
	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "2", Memory: "8Gi", Disk: "10Gi"})
	waitSize(t, w, "vm", "2/8192Mi/80Gi", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, "Applied")

	setRunState(t, w, "vm", envv1alpha1.RunState_RUN_STATE_STOPPED)
	w.WaitPhase(t, "vm", envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED)
	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "1", Memory: "2Gi", Disk: "80Gi"})
	waitSize(t, w, "vm", "1/2048Mi/80Gi", envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED, "Applied")
	disks, err := storage.Disks(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range disks {
		if d.Name == disk && d.Size != 80<<30 {
			t.Errorf("the VM's disk is %d bytes", d.Size)
		}
	}
}

// TestAResizeCutShortMakesTheVMAgain: a resize cut short once the VM's guest
// was removed, before it was made again (chvm's Resize failing there, or
// hostd stopping there), leaves its disk: hostd makes the guest again on it
// at the size asked, and the instance is never GONE, which would have tower
// delete the disk.
func TestAResizeCutShortMakesTheVMAgain(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	w.Apply(t, "vm", &envv1alpha1.InstanceSpec{
		Target: target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_VM, Image: "ubuntu-24.04",
		Resources: &envv1alpha1.Resources{Cpu: "2", Memory: "4Gi", Disk: "20Gi"},
	})
	in := waitSize(t, w, "vm", "2/4096Mi/20Gi", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, "Applied")
	ref := provider.Instance{Project: in.GetMetadata().GetProject(), Name: "vm"}
	if err := w.Host.WriteFile(t.Context(), ref, "/home/tower/kept", 0o644, 0, strings.NewReader("kept")); err != nil {
		t.Fatal(err)
	}
	var gone atomic.Bool
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			o, err := w.Env.AsSystem().Get(context.Background(), registry.Instance, client.Key{Project: ref.Project, Name: ref.Name})
			if err == nil && o.(*envv1alpha1.Instance).GetStatus().GetPhase() == envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE {
				gone.Store(true)
			}
		}
	}()

	w.Fake.CutResize("tower-" + in.GetMetadata().GetUid())
	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "4", Memory: "8Gi", Disk: "20Gi"})
	waitSize(t, w, "vm", "4/8192Mi/20Gi", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, "Applied")
	var b strings.Builder
	if err := w.Host.ReadFile(t.Context(), ref, "/home/tower/kept", &b); err != nil || b.String() != "kept" {
		t.Errorf("the VM made again reads %q, %v of what it wrote before", b.String(), err)
	}
	if gone.Load() {
		t.Error("the instance was reported GONE while its guest was made again")
	}
}
