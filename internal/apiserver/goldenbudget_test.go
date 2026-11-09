package apiserver_test

import (
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// goldenSize is what each capture holds in these tests: a machine golden's
// builder disk, tower's default VM disk, which a golden being built counts.
const goldenSize = 20 << 30

// machineOf is a machine whose blueprint, and so golden, is its own of
// that name.
func machineOf(name, blueprint string) *envv1alpha1.Machine {
	m := machine(name)
	m.Spec.Blueprint.Content = "- hosts: all # " + blueprint + "\n"
	return m
}

// readyMachineGolden makes a machine of the blueprint, waits for it to be
// READY, and returns its golden's name.
func (w *world) readyMachineGolden(name, blueprint string) string {
	w.t.Helper()
	w.mustApply(registry.Machine, machineOf(name, blueprint))
	return w.waitMachine(name, envv1alpha1.MachinePhase_MACHINE_PHASE_READY).GetStatus().GetGolden()
}

func (w *world) deleteMachine(name string) {
	w.t.Helper()
	if _, err := w.asSystem().generic.Delete(w.t.Context(), registry.Machine, client.Key{Project: "web", Name: name}); err != nil {
		w.t.Fatal(err)
	}
	w.waitSettled()
}

// goldensLeft are those of names still there, in their order.
func (w *world) goldensLeft(names ...string) []string {
	w.t.Helper()
	var left []string
	for _, n := range names {
		if _, ok := w.golden(n); ok {
			left = append(left, n)
		}
	}
	return left
}

// TestAnIdleGoldenUnderBudgetIsKept: a golden nothing uses or references
// is kept however long it idles while the target's goldens are within
// their budget and its disk is free.
func TestAnIdleGoldenUnderBudgetIsKept(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	g := w.readyMachineGolden("k3s", "k3s")
	box := w.readyBox("dev")
	w.deleteMachine("k3s")
	if _, err := w.asSystem().generic.Delete(t.Context(), registry.Box, client.Key{Project: "web", Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	if err := w.fake.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	w.advance(30 * 24 * time.Hour)
	for _, name := range []string{g, box.GetStatus().GetImageGolden()} {
		if got, ok := w.golden(name); !ok || got.GetStatus().GetPhase() != envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY {
			t.Errorf("golden %s idle for 30 days under budget: %v, want kept READY", name, got.GetStatus())
		}
	}
}

// TestGoldensOverBudgetGoLeastRecentlyUsedFirst: a target's goldens over
// golden_budget are deleted, those last made from longest ago first, until
// they hold golden_budget_low; one something references stays, however
// long ago it was made from.
func TestGoldensOverBudgetGoLeastRecentlyUsedFirst(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{settings: &targetsv1alpha1.TargetSettings{GoldenBudget: "80Gi", GoldenBudgetLow: "60Gi"}})
	w.fake.CaptureSize(func(provider.Instance) uint64 { return goldenSize })
	// a's machine stays; b, c and e go, b made from again after them.
	a := w.readyMachineGolden("ma", "a")
	w.advance(time.Hour)
	var b, c, e string
	for _, m := range []struct {
		name string
		to   *string
	}{{"b", &b}, {"c", &c}, {"e", &e}} {
		*m.to = w.readyMachineGolden("m"+m.name, m.name)
		w.deleteMachine("m" + m.name)
		w.advance(time.Hour)
	}
	if got := w.readyMachineGolden("mb2", "b"); got != b {
		t.Fatalf("a machine of b's blueprint made golden %s, want b's %s", got, b)
	}
	w.deleteMachine("mb2")
	w.advance(time.Hour)
	all := []string{a, b, c, e}
	if left := w.goldensLeft(all...); len(left) != 4 {
		t.Fatalf("at the budget, goldens %v left, want all four", left)
	}
	// d takes them over the budget: c and e go, the two used longest ago
	// that nothing references, and a, b and d hold the low mark.
	d := w.readyMachineGolden("md", "d")
	w.waitSettled()
	if left, want := w.goldensLeft(append(all, d)...), []string{a, b, d}; !slices.Equal(left, want) {
		t.Errorf("over the budget, goldens %v left, want %v (a referenced, b made from last, d in use)", left, want)
	}
	// Nothing referenced is left to evict: over the budget, a and d stay.
	if _, err := w.asSystem().generic.Apply(t.Context(), registry.Target, func() *targetsv1alpha1.Target {
		tg := hostdTarget("t1")
		tg.Spec.Settings = &targetsv1alpha1.TargetSettings{GoldenBudget: "10Gi", GoldenBudgetLow: "0"}
		return tg
	}()); err != nil {
		t.Fatal(err)
	}
	w.advance(time.Hour)
	if left, want := w.goldensLeft(a, b, d), []string{a, d}; !slices.Equal(left, want) {
		t.Errorf("far over the budget, goldens %v left, want %v: what machines are made from stays", left, want)
	}
}

// TestGoldensGoBeforeDiskPressure: a target with less free than
// DiskPressure needs and a cushion has its goldens nothing references
// deleted, the least recently used first, until it would have that free.
// What they freed counts as free until a report shows it, across a
// restart, for GoldenFreedWait at most.
func TestGoldensGoBeforeDiskPressure(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{settings: &targetsv1alpha1.TargetSettings{WarmBudget: "100Gi"}})
	w.fake.CaptureSize(func(provider.Instance) uint64 { return goldenSize })
	var goldens []string
	for _, n := range []string{"x", "y", "z"} {
		goldens = append(goldens, w.readyMachineGolden("m"+n, n))
		w.deleteMachine("m" + n)
		w.advance(time.Hour)
	}
	// 30Gi short of 100Gi + 20Gi + 10Gi: x and y go.
	free := uint64(100 << 30)
	w.fake.SetDiskFree(&free)
	if err := w.fake.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "x and y evicted", func() (bool, string) {
		left := w.goldensLeft(goldens...)
		return slices.Equal(left, goldens[2:]), fmt.Sprint(left)
	})
	// Reports that do not show what x and y freed yet, from a tower started
	// again, evict nothing more.
	w.restart()
	for range 2 {
		w.advance(time.Minute)
		if err := w.fake.Report(t.Context()); err != nil {
			t.Fatal(err)
		}
		w.waitSettled()
		if left := w.goldensLeft(goldens...); !slices.Equal(left, goldens[2:]) {
			t.Fatalf("on a report that does not show what x and y freed, goldens %v left, want z alone", left)
		}
	}
	// Past GoldenFreedWait, the reports are taken as they are: z goes.
	w.advance(apiserver.GoldenFreedWait)
	if err := w.fake.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "z evicted", func() (bool, string) {
		left := w.goldensLeft(goldens...)
		return len(left) == 0, fmt.Sprint(left)
	})
}

// TestAGoldenBeingBuiltCountsItsBuildersDisk: a machine golden being built
// counts its builder's disk against the budget, so room is made before
// its capture, smaller as it may be, lands.
func TestAGoldenBeingBuiltCountsItsBuildersDisk(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{settings: &targetsv1alpha1.TargetSettings{GoldenBudget: "50Gi", GoldenBudgetLow: "40Gi"}})
	var captures atomic.Int32
	w.fake.CaptureSize(func(provider.Instance) uint64 {
		if captures.Add(1) <= 2 {
			return goldenSize
		}
		return 5 << 30
	})
	var old []string
	for _, n := range []string{"x", "y"} {
		old = append(old, w.readyMachineGolden("m"+n, n))
		w.deleteMachine("m" + n)
		w.advance(time.Hour)
	}
	// Built, the new golden holds 45Gi with them, under the budget; being
	// built, it counts 20Gi, and x goes.
	w.readyMachineGolden("mz", "z")
	w.waitSettled()
	if left := w.goldensLeft(old...); !slices.Equal(left, old[1:]) {
		t.Errorf("goldens %v left of x and y, want y alone: the build counted its builder's disk", left)
	}
}

// TestWarmGoldensAreNotInTheGoldenBudget: a warm golden neither counts
// against the golden budget nor is evicted by it.
func TestWarmGoldensAreNotInTheGoldenBudget(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{settings: &targetsv1alpha1.TargetSettings{GoldenBudget: "25Gi", GoldenBudgetLow: "25Gi"}})
	w.fake.CaptureSize(func(provider.Instance) uint64 { return goldenSize })
	m := w.readyMachineGolden("mx", "x")
	w.deleteMachine("mx")
	w.mustApply(registry.Repository, repository("a"))
	w.openBoxes("a", 3)
	w.advance(5 * time.Minute)
	var warm string
	testutil.Eventually(t, "a warm golden for a", func() (bool, string) {
		got := w.warmOf()
		if len(got["a"]) == 1 {
			warm = got["a"][0]
		}
		return warm != "", fmt.Sprint(got)
	})
	w.advance(time.Hour)
	if left := w.goldensLeft(m, warm); len(left) != 2 {
		t.Errorf("goldens %v left of a machine's and a warm one, want both: the warm one is outside the golden budget", left)
	}
}

// TestAGoldenGoesOnceNothingReferencesIt: over the budget, a golden goes
// as soon as the last machine made from it does.
func TestAGoldenGoesOnceNothingReferencesIt(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{settings: &targetsv1alpha1.TargetSettings{GoldenBudget: "10Gi", GoldenBudgetLow: "0"}})
	w.fake.CaptureSize(func(provider.Instance) uint64 { return goldenSize })
	g := w.readyMachineGolden("mx", "x")
	w.waitSettled()
	if _, ok := w.golden(g); !ok {
		t.Fatal("a golden a machine is made from was evicted")
	}
	w.deleteMachine("mx")
	testutil.Eventually(t, "the golden evicted", func() bool {
		_, ok := w.golden(g)
		return !ok
	})
}

// TestAFailedGoldenNothingAsksForGoes: a failed golden holds no disk; one
// nothing asks for goes after FailedGoldenKeptFor.
func TestAFailedGoldenNothingAsksForGoes(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.fake.FailCaptures(provider.ErrFailed)
	w.mustApply(registry.Machine, machine("k3s"))
	m := w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_FAILED)
	g := m.GetStatus().GetGolden()
	w.deleteMachine("k3s")
	w.advance(apiserver.FailedGoldenKeptFor / 2)
	if _, ok := w.golden(g); !ok {
		t.Fatal("a failed golden went before FailedGoldenKeptFor")
	}
	w.advance(apiserver.FailedGoldenKeptFor)
	testutil.Eventually(t, "the failed golden pruned", func() bool {
		_, ok := w.golden(g)
		return !ok
	})
}
