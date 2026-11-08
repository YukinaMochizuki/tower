package hangar_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	hangartenancy "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/hangartest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// setResources sets an instance's resources, as tower's machine controller
// does.
func setResources(t *testing.T, w *hangartest.World, name string, r *envv1alpha1.Resources) {
	t.Helper()
	testutil.Eventually(t, "the instance's resources set", func() (bool, string) {
		in := proto.Clone(w.Instance(t, name)).(*envv1alpha1.Instance)
		in.Spec.Resources = r
		_, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, in)
		if err != nil && connect.CodeOf(err) != connect.CodeAborted {
			t.Fatal(err)
		}
		return err == nil, fmt.Sprint(err)
	})
}

// waitSize waits until the provider reports the instance at size
// cpu/memory/disk, in phase, its Resized of the generation reported with
// reason, and returns it.
func waitSize(t *testing.T, w *hangartest.World, name, size string, phase envv1alpha1.InstancePhase, reason string) *envv1alpha1.Instance {
	t.Helper()
	var in *envv1alpha1.Instance
	testutil.Eventually(t, fmt.Sprintf("instance %s at %s, Resized %s", name, size, reason), func() (bool, string) {
		in = w.Instance(t, name)
		st := in.GetStatus()
		r := st.GetResources()
		c := conditions.Find(st.GetConditions(), provider.InstanceResized)
		return r.GetCpu()+"/"+r.GetMemory()+"/"+r.GetDisk() == size && st.GetPhase() == phase &&
			c.GetReason() == reason && c.GetObservedGeneration() == in.GetMetadata().GetGeneration(), st.String()
	})
	return in
}

// TestAVMIsResizedInHangar: a VM given a new size has hangar restart it at
// it, its root Disk grown; a size the project's quota or the node has no
// room for leaves it running at the size it has, Resized False with
// hangar's reason and words, never FAILED, and the node's room once there
// applies it; a stopped VM takes its size stopped.
func TestAVMIsResizedInHangar(t *testing.T) {
	// Read back from hangar often: the node's room comes back by itself.
	w, f := newWorld(t, func(c *hangar.Config) { c.ReportEvery = 50 * time.Millisecond })
	h := w.Harness()
	h.Start(t, "vm", vmSpec("", ""))
	vm := "t-" + w.Instance(t, "vm").GetMetadata().GetUid()
	running := envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING
	// Its disk is as large as its image, past the 2Gi asked.
	waitSize(t, w, "vm", "2/4096Mi/3Gi", running, "Applied")

	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "4", Memory: "8Gi", Disk: "10Gi"})
	waitSize(t, w, "vm", "4/8192Mi/10Gi", running, "Applied")
	if n := f.Restarts(vm); n < 1 {
		t.Errorf("a VM resized was restarted %d times", n)
	}
	if d, err := f.Disks().Get(t.Context(), vm); err != nil || d.GetSpec().GetSize() != "10Gi" {
		t.Errorf("its root disk: %v, %v", d.GetSpec(), err)
	}

	memory := "10Gi"
	f.SetLimits(&hangartenancy.Limits{Memory: &memory})
	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "4", Memory: "16Gi", Disk: "10Gi"})
	in := waitSize(t, w, "vm", "4/8192Mi/10Gi", running, "QuotaExceeded")
	if c := conditions.Find(in.GetStatus().GetConditions(), provider.InstanceResized); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE ||
		!strings.Contains(c.GetMessage(), "over its limit of 10Gi") {
		t.Errorf("a resize over quota: Resized %v", c)
	}

	f.SetLimits(nil)
	f.FillNode("1Gi")
	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "4", Memory: "12Gi", Disk: "10Gi"})
	in = waitSize(t, w, "vm", "4/8192Mi/10Gi", running, "ResizeDoesNotFit")
	if c := conditions.Find(in.GetStatus().GetConditions(), provider.InstanceResized); !strings.Contains(c.GetMessage(), "has 1Gi of memory free") {
		t.Errorf("a resize the node has no room for: Resized %v", c)
	}
	f.FreeRoom()
	waitSize(t, w, "vm", "4/12288Mi/10Gi", running, "Applied")

	disk := "20Gi"
	f.SetLimits(&hangartenancy.Limits{Disk: &disk})
	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "4", Memory: "12Gi", Disk: "40Gi"})
	in = waitSize(t, w, "vm", "4/12288Mi/10Gi", running, "QuotaExceeded")
	if c := conditions.Find(in.GetStatus().GetConditions(), provider.InstanceResized); !strings.Contains(c.GetMessage(), "its disk stays 10Gi") {
		t.Errorf("a disk growth over quota: Resized %v", c)
	}
	f.SetLimits(nil)

	// A growth the node has no disk room for is held by hangar, Allocated
	// False: the VM runs on at its disk, the refusal saying why, until the
	// node has room.
	f.FillNodeDisk("5Gi")
	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "4", Memory: "12Gi", Disk: "40Gi"})
	in = waitSize(t, w, "vm", "4/12288Mi/10Gi", running, "ResizeDoesNotFit")
	if c := conditions.Find(in.GetStatus().GetConditions(), provider.InstanceResized); !strings.Contains(c.GetMessage(), "its disk stays 10Gi: node n1 has 5Gi of disk free") {
		t.Errorf("a disk growth the node has no room for: Resized %v", c)
	}
	f.FreeRoom()
	waitSize(t, w, "vm", "4/12288Mi/40Gi", running, "Applied")

	setRunState(t, w, "vm", envv1alpha1.RunState_RUN_STATE_STOPPED)
	w.WaitPhase(t, "vm", envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED)
	restarts := f.Restarts(vm)
	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "1", Memory: "2Gi", Disk: "40Gi"})
	waitSize(t, w, "vm", "1/2048Mi/40Gi", envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED, "Applied")
	if n := f.Restarts(vm); n != restarts {
		t.Errorf("a stopped VM resized was restarted %d times", n-restarts)
	}
}

// TestAGrowthNotYetGrantedIsApplying: a root disk growth hangar has taken
// but not yet granted is not refused: Resized True, Applying, the disk at
// its size; once granted, Applied at the new size.
func TestAGrowthNotYetGrantedIsApplying(t *testing.T) {
	w, f := newWorld(t, func(c *hangar.Config) { c.ReportEvery = 50 * time.Millisecond })
	h := w.Harness()
	h.Start(t, "vm", vmSpec("", ""))
	running := envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING
	waitSize(t, w, "vm", "2/4096Mi/3Gi", running, "Applied")
	f.HoldDiskGrowth()
	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "2", Memory: "4Gi", Disk: "10Gi"})
	in := waitSize(t, w, "vm", "2/4096Mi/3Gi", running, "Applying")
	if c := conditions.Find(in.GetStatus().GetConditions(), provider.InstanceResized); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("a growth not yet granted: Resized %v", c)
	}
	f.FreeRoom()
	waitSize(t, w, "vm", "2/4096Mi/10Gi", running, "Applied")
}

// TestARefusedResizeNeverFailsAVM: a stopped VM whose resize hangar refuses
// over quota is started all the same, at the size it has, the refusal still
// said; it is never FAILED.
func TestARefusedResizeNeverFailsAVM(t *testing.T) {
	w, f := newWorld(t, nil)
	h := w.Harness()
	h.Start(t, "vm", vmSpec("", ""))
	setRunState(t, w, "vm", envv1alpha1.RunState_RUN_STATE_STOPPED)
	stopped := envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED
	waitSize(t, w, "vm", "2/4096Mi/3Gi", stopped, "Applied")

	memory := "4Gi"
	f.SetLimits(&hangartenancy.Limits{Memory: &memory})
	setResources(t, w, "vm", &envv1alpha1.Resources{Cpu: "2", Memory: "8Gi", Disk: "2Gi"})
	waitSize(t, w, "vm", "2/4096Mi/3Gi", stopped, "QuotaExceeded")
	setRunState(t, w, "vm", envv1alpha1.RunState_RUN_STATE_RUNNING)
	in := waitSize(t, w, "vm", "2/4096Mi/3Gi", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING, "QuotaExceeded")
	if c := conditions.Find(in.GetStatus().GetConditions(), provider.InstanceResized); !strings.Contains(c.GetMessage(), "over its limit of 4Gi") {
		t.Errorf("a VM started with its resize refused: Resized %v", c)
	}
}
