package apiserver_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// resize applies the machine with resources r.
func (w *world) resize(name string, r *envv1alpha1.Resources) {
	w.t.Helper()
	m := w.machine(name)
	m.Spec.Resources = r
	w.mustApply(registry.Machine, m)
}

// waitResized waits until the machine's Resized condition has reason, and
// returns the machine.
func (w *world) waitResized(name, reason string) *envv1alpha1.Machine {
	w.t.Helper()
	var m *envv1alpha1.Machine
	testutil.Eventually(w.t, fmt.Sprintf("machine %s Resized %s", name, reason), func() (bool, string) {
		m = w.machine(name)
		c := condition(m.GetStatus().GetConditions(), apiserver.MachineResized)
		return c.GetReason() == reason && c.GetObservedGeneration() == m.GetMetadata().GetGeneration(), m.GetStatus().String()
	})
	return m
}

// configures counts the configure phases run.
func (w *world) configures() int {
	n := 0
	for _, e := range w.fake.Execs() {
		if slices.Contains(e.Command, "--tags=configure") {
			n++
		}
	}
	return n
}

func sizeOf(r *envv1alpha1.Resources) string {
	return r.GetCpu() + "/" + r.GetMemory() + "/" + r.GetDisk()
}

// TestAReadyMachineIsResized: a ready machine given a new size restarts at
// it, saying Resizing until its target reports the new size; its ready
// check runs again in the new run, its configure phase does not, and it is
// READY at the new size, which its status and its instance's say.
func TestAReadyMachineIsResized(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	check := &readyCheck{command: "ready-now"}
	check.pass.Store(true)
	check.on(w.fake)
	m := machine("k3s")
	m.Spec.Blueprint.ReadyCheck = &envv1alpha1.ReadyCheck{Command: check.command}
	w.mustApply(registry.Machine, m)
	ready := w.waitMachineTicking("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	if got := sizeOf(ready.GetStatus().GetResources()); got != "2/4Gi/20Gi" {
		t.Errorf("a machine made at tower's default reports %s", got)
	}
	w.waitResized("k3s", "Resized")
	ref := provider.Instance{Project: "web", Name: ready.GetStatus().GetInstance()}
	configures, tries := w.configures(), check.tries.Load()

	// While the target reports nothing, the machine says it is resizing.
	w.fake.SetUnavailable(true)
	w.resize("k3s", &envv1alpha1.Resources{Cpu: "4", Memory: "8192Mi", Disk: "40Gi"})
	resizing := w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_CONFIGURING)
	if c := condition(resizing.GetStatus().GetConditions(), apiserver.MachineReady); c.GetReason() != apiserver.MachineResizing ||
		!strings.Contains(c.GetMessage(), "4 vCPUs, 8192Mi memory, 40Gi disk") {
		t.Errorf("a machine being resized: Ready %v", c)
	}
	w.waitResized("k3s", apiserver.MachineResizing)
	in, _ := w.instance(ref.Name)
	if got := sizeOf(in.GetSpec().GetResources()); got != "4/8192Mi/40Gi" {
		t.Errorf("its instance asks %s", got)
	}

	// The restart is a run of its own, which starts later.
	w.clock.Advance(time.Second)
	w.fake.SetUnavailable(false)
	done := w.waitMachineTicking("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	w.waitResized("k3s", "Resized")
	if got := sizeOf(w.machine("k3s").GetStatus().GetResources()); got != "4/8192Mi/40Gi" {
		t.Errorf("a resized machine reports %s", got)
	}
	if n := w.fake.Restarts(ref); n != 1 {
		t.Errorf("restarted %d times for one resize", n)
	}
	if was, now := ready.GetStatus().GetStartTime(), w.machine("k3s").GetStatus().GetStartTime(); was == nil || !now.AsTime().After(was.AsTime()) {
		t.Errorf("the machine's start_time %v after its restart, %v before", now, was)
	}
	if check.tries.Load() <= tries {
		t.Error("its ready check did not run again after the restart")
	}
	if n := w.configures(); n != configures {
		t.Errorf("its configure phase ran %d more times", n-configures)
	}
	if done.GetStatus().GetInstance() != ref.Name {
		t.Errorf("its instance %q was made again as %q", ref.Name, done.GetStatus().GetInstance())
	}

	// The same size asked again in other words changes nothing.
	w.resize("k3s", &envv1alpha1.Resources{Cpu: "4", Memory: "8Gi", Disk: "40Gi"})
	w.advance(time.Minute)
	if n := w.fake.Restarts(ref); n != 1 {
		t.Errorf("restarted %d times; the size asked again is the size it has", n)
	}
}

// TestAStoppedMachineTakesItsSizeStopped: a stopped machine given a new
// size takes it and stays stopped; started, it runs at it.
func TestAStoppedMachineTakesItsSizeStopped(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Machine, machine("k3s"))
	ready := w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	ref := provider.Instance{Project: "web", Name: ready.GetStatus().GetInstance()}
	stop := w.machine("k3s")
	stop.Spec.RunState = envv1alpha1.RunState_RUN_STATE_STOPPED
	w.mustApply(registry.Machine, stop)
	w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_STOPPED)

	w.resize("k3s", &envv1alpha1.Resources{Memory: "6Gi"})
	stopped := w.waitResized("k3s", "Resized")
	if p := stopped.GetStatus().GetPhase(); p != envv1alpha1.MachinePhase_MACHINE_PHASE_STOPPED {
		t.Errorf("a stopped machine resized is %v", p)
	}
	if got := sizeOf(stopped.GetStatus().GetResources()); got != "2/6Gi/20Gi" {
		t.Errorf("a stopped machine resized reports %s; empty fields keep what it has", got)
	}
	if n := w.fake.Restarts(ref); n != 0 {
		t.Errorf("a stopped machine was restarted %d times", n)
	}
	start := w.machine("k3s")
	start.Spec.RunState = envv1alpha1.RunState_RUN_STATE_RUNNING
	w.mustApply(registry.Machine, start)
	w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
}

// TestAMachinesDiskOnlyGrows: a smaller disk is refused with a reason, and
// the vCPUs asked with it are still applied.
func TestAMachinesDiskOnlyGrows(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Machine, machine("k3s"))
	w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	w.resize("k3s", &envv1alpha1.Resources{Cpu: "3", Disk: "10Gi"})
	m := w.waitResized("k3s", "DiskCannotShrink")
	c := condition(m.GetStatus().GetConditions(), apiserver.MachineResized)
	if c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || !strings.Contains(c.GetMessage(), "a disk only grows") {
		t.Errorf("a disk asked to shrink: Resized %v", c)
	}
	m = w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	if got := sizeOf(m.GetStatus().GetResources()); got != "3/4Gi/20Gi" {
		t.Errorf("a machine asked for 3 vCPUs and a smaller disk reports %s", got)
	}
}

// TestAResizeThatDoesNotFitKeepsTheSize: a size a hostd host has no room
// for, or one the target refuses, leaves the machine READY at the size it
// has, Resized False saying why, its instance never FAILED; once the
// target takes it, it is applied.
func TestAResizeThatDoesNotFitKeepsTheSize(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Machine, machine("k3s"))
	ready := w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	ref := provider.Instance{Project: "web", Name: ready.GetStatus().GetInstance()}
	if err := w.fake.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the host's capacity reported", func() (bool, string) {
		o, err := w.get(registry.Target, "", "t1")
		if err != nil {
			t.Fatal(err)
		}
		c := o.(*targetsv1alpha1.Target).GetStatus().GetCapacity()
		return c.GetCpus() == 16 && c.GetMemoryFree() == "48Gi", c.String()
	})

	for _, tc := range []struct {
		r    *envv1alpha1.Resources
		says string
	}{
		{&envv1alpha1.Resources{Cpu: "32"}, "it asks for 32 vCPUs; the host has 16"},
		{&envv1alpha1.Resources{Cpu: "2", Memory: "60Gi"}, "it asks for 57344Mi more memory; the host has 49152Mi free"},
	} {
		w.resize("k3s", tc.r)
		m := w.waitResized("k3s", "ResizeDoesNotFit")
		if c := condition(m.GetStatus().GetConditions(), apiserver.MachineResized); !strings.Contains(c.GetMessage(), tc.says) {
			t.Errorf("a resize to %s: Resized %v, want it to say %q", sizeOf(tc.r), c, tc.says)
		}
		if p := m.GetStatus().GetPhase(); p != envv1alpha1.MachinePhase_MACHINE_PHASE_READY {
			t.Errorf("a resize to %s that does not fit leaves the machine %v", sizeOf(tc.r), p)
		}
		if in, _ := w.instance(ref.Name); sizeOf(in.GetSpec().GetResources()) != "2/4Gi/20Gi" {
			t.Errorf("a resize that does not fit reached the instance: %v", in.GetSpec().GetResources())
		}
	}

	w.fake.RefuseResize(ref.Name, "QuotaExceeded", "the project's memory quota has 1Gi left")
	w.resize("k3s", &envv1alpha1.Resources{Cpu: "2", Memory: "8Gi"})
	m := w.waitResized("k3s", "QuotaExceeded")
	if c := condition(m.GetStatus().GetConditions(), apiserver.MachineResized); !strings.Contains(c.GetMessage(), "1Gi left; it keeps the size it has") {
		t.Errorf("a refused resize: Resized %v", c)
	}
	in, _ := w.instance(ref.Name)
	if in.GetStatus().GetPhase() != envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING || m.GetStatus().GetPhase() != envv1alpha1.MachinePhase_MACHINE_PHASE_READY {
		t.Errorf("a refused resize: instance %v, machine %v", in.GetStatus().GetPhase(), m.GetStatus().GetPhase())
	}
	if got := sizeOf(m.GetStatus().GetResources()); got != "2/4Gi/20Gi" {
		t.Errorf("a refused resize reports %s", got)
	}

	w.fake.RefuseResize(ref.Name, "", "")
	w.waitResized("k3s", "Resized")
	if got := sizeOf(w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY).GetStatus().GetResources()); got != "2/8Gi/20Gi" {
		t.Errorf("a resize the target took at last reports %s", got)
	}
}

// TestAResizeIsAppliedOnceTheHostHasRoom: a size a hostd host has no memory
// free for is not applied; once the host reports room, the machine's next
// resync applies it.
func TestAResizeIsAppliedOnceTheHostHasRoom(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Machine, machine("k3s"))
	ready := w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	ref := provider.Instance{Project: "web", Name: ready.GetStatus().GetInstance()}
	if err := w.fake.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	w.waitCapacity("48Gi")
	w.resize("k3s", &envv1alpha1.Resources{Memory: "60Gi"})
	w.waitResized("k3s", "ResizeDoesNotFit")

	w.fake.SetMemoryFree("100Gi")
	if err := w.fake.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	w.waitCapacity("100Gi")
	w.advance(5 * time.Minute)
	w.waitResized("k3s", "Resized")
	if got := sizeOf(w.machine("k3s").GetStatus().GetResources()); got != "2/60Gi/20Gi" {
		t.Errorf("a resize the host made room for reports %s", got)
	}
	if n := w.fake.Restarts(ref); n != 1 {
		t.Errorf("restarted %d times", n)
	}
}

// TestARefusalOfAnOlderSizeSaysNothingOfANewOne: the target's refusal of a
// size speaks for the generation of the instance it saw; a new size asked
// since is being resized, not refused, until the target says otherwise.
func TestARefusalOfAnOlderSizeSaysNothingOfANewOne(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Machine, machine("k3s"))
	ready := w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	w.fake.RefuseResize(ready.GetStatus().GetInstance(), "QuotaExceeded", "the project's memory quota has 1Gi left")
	w.resize("k3s", &envv1alpha1.Resources{Cpu: "4"})
	w.waitResized("k3s", "QuotaExceeded")

	// The target reports nothing of the new size yet.
	w.fake.SetUnavailable(true)
	w.resize("k3s", &envv1alpha1.Resources{Cpu: "3"})
	m := w.waitResized("k3s", apiserver.MachineResizing)
	if p := m.GetStatus().GetPhase(); p != envv1alpha1.MachinePhase_MACHINE_PHASE_CONFIGURING {
		t.Errorf("a machine being resized is %v", p)
	}
}

// waitCapacity waits until the target's status says memory_free.
func (w *world) waitCapacity(memoryFree string) {
	w.t.Helper()
	testutil.Eventually(w.t, "the host's capacity reported", func() (bool, string) {
		o, err := w.get(registry.Target, "", "t1")
		if err != nil {
			w.t.Fatal(err)
		}
		c := o.(*targetsv1alpha1.Target).GetStatus().GetCapacity()
		return c.GetMemoryFree() == memoryFree, c.String()
	})
}
