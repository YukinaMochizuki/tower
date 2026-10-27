package apiserver_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// TestTwoMachinesOfABlueprintHaveTheirOwnNames: a machine is named on its
// workstream's network by its own name unless its blueprint names an
// alias, so two machines of one blueprint are two names.
func TestTwoMachinesOfABlueprintHaveTheirOwnNames(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Machine, machine("db-1"))
	w.mustApply(registry.Machine, machine("db-2"))
	one := w.waitMachine("db-1", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	two := w.waitMachine("db-2", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	if one.GetStatus().GetAlias() != "db-1" || two.GetStatus().GetAlias() != "db-2" {
		t.Errorf("two machines of blueprint k3s are named %q and %q, want their own names", one.GetStatus().GetAlias(), two.GetStatus().GetAlias())
	}
	for _, name := range []string{"db-1", "db-2"} {
		in, ok := w.instance(w.machine(name).GetStatus().GetInstance())
		if !ok || in.GetSpec().GetAlias() != name {
			t.Errorf("machine %s's instance is named %q", name, in.GetSpec().GetAlias())
		}
	}
}

// TestCreatesOfOneAliasAtOnceAdmitOne: machines of one workstream made at
// once under one alias: exactly one is admitted.
func TestCreatesOfOneAliasAtOnceAdmitOne(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const n = 16
	var wg sync.WaitGroup
	var admitted, refused atomic.Int32
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := machine(fmt.Sprintf("race-%d", i))
			m.Spec.Blueprint.NetAlias = "same"
			<-start
			_, err := w.asSystem().generic.Apply(t.Context(), registry.Machine, m)
			switch {
			case err == nil:
				admitted.Add(1)
			case connect.CodeOf(err) == connect.CodeFailedPrecondition:
				refused.Add(1)
			default:
				t.Errorf("a create at once: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if admitted.Load() != 1 || refused.Load() != n-1 {
		t.Errorf("%d machines made at once under one alias: %d admitted, %d refused; want one admitted", n, admitted.Load(), refused.Load())
	}
}

// TestAnAliasInUseIsRefused: a machine whose alias another machine or a
// box of its workstream answers to is refused, and a box whose name a
// machine of its workstream answers to; another workstream's are not.
func TestAnAliasInUseIsRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	first := machine("k1")
	first.Spec.Blueprint.NetAlias = "k3s"
	w.mustApply(registry.Machine, first)

	refused := func(what string, r registry.Resource, obj proto.Message) {
		t.Helper()
		_, err := w.asSystem().generic.Apply(t.Context(), r, obj)
		if connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("%s: %v, want it refused as failed precondition", what, err)
		}
	}
	dup := machine("k2")
	dup.Spec.Blueprint.NetAlias = "k3s"
	refused("a machine with another machine's net_alias", registry.Machine, dup)
	byName := machine("k1b")
	byName.Spec.Blueprint.NetAlias = "k1"
	w.mustApply(registry.Machine, byName) // k1's alias is k3s, not its name
	boxAlias := machine("k4")
	boxAlias.Spec.Blueprint.NetAlias = "dev"
	refused("a machine with a box's name as its net_alias", registry.Machine, boxAlias)
	refused("a machine named as a box, by default its alias", registry.Machine, machine("dev"))
	refused("a box named as a machine's alias", registry.Box, box("k3s", "api"))

	// Another workstream's names are its own.
	other := machine("k5")
	other.Spec.Workstream, other.Spec.Blueprint.NetAlias = "ws2", "k3s"
	w.mustApply(registry.Machine, other)
}

// TestAMachineKeepsTheAliasItsInstanceHas: a machine made before machines
// were named by their own names says the alias its instance has, its
// blueprint's, and keeps it: another machine taking it is refused.
func TestAMachineKeepsTheAliasItsInstanceHas(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{unplaced: true})
	w.mustApply(registry.Machine, machine("legacy"))
	m := w.machine("legacy")
	// Its instance as the controller made it before: named by the
	// blueprint.
	w.mustApply(registry.Instance, &envv1alpha1.Instance{
		Metadata: &metav1.ObjectMeta{Project: "web", Name: "mc-legacy", OwnerReferences: []*metav1.OwnerReference{{
			ApiVersion: registry.Machine.APIVersion(), Kind: registry.Machine.Kind, Name: "legacy", Uid: m.GetMetadata().GetUid(), Controller: true,
		}}},
		Spec: &envv1alpha1.InstanceSpec{Target: "t1", Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_VM, Image: "ubuntu-24.04", Network: "ws1", Alias: "k3s"},
	})
	w.mustApply(registry.Placement, placement("web", "t1"))
	testutil.Eventually(t, "the machine saying its instance's alias", func() (bool, string) {
		a := w.machine("legacy").GetStatus().GetAlias()
		return a == "k3s", a
	})
	taken := machine("k6")
	taken.Spec.Blueprint.NetAlias = "k3s"
	if _, err := w.asSystem().generic.Apply(t.Context(), registry.Machine, taken); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a machine taking the alias an older machine has: %v, want it refused", err)
	}
}
