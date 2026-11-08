package providertest

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// ResizeThrough is a Harness.Resize for a harness whose Instances, in
// project, are applied and read through c: it sets the instance's resources
// as tower's machine controller does, then waits, up to within, until its
// provider has answered that generation, by its condition Resized, in the
// phase the instance had, and returns its status.
func ResizeThrough(c *client.Client, project string, within time.Duration) func(t *testing.T, in provider.Instance, r *envv1alpha1.Resources) *envv1alpha1.InstanceStatus {
	return func(t *testing.T, ref provider.Instance, r *envv1alpha1.Resources) *envv1alpha1.InstanceStatus {
		t.Helper()
		key := client.Key{Project: project, Name: ref.Name}
		get := func() *envv1alpha1.Instance {
			o, err := c.Get(t.Context(), registry.Instance, key)
			if err != nil {
				t.Fatal(err)
			}
			return o.(*envv1alpha1.Instance)
		}
		var generation int64
		var phase envv1alpha1.InstancePhase
		// The provider writes the instance's status as it goes, which can move
		// it between the read and the apply.
		testutil.EventuallyWithin(t, within, "the instance's resources set", func() (bool, string) {
			in := proto.Clone(get()).(*envv1alpha1.Instance)
			phase = in.GetStatus().GetPhase()
			in.Spec.Resources = r
			res, err := c.Apply(t.Context(), registry.Instance, in)
			if err != nil && connect.CodeOf(err) != connect.CodeAborted {
				t.Fatal(err)
			}
			if err == nil {
				generation = res.Object.(*envv1alpha1.Instance).GetMetadata().GetGeneration()
			}
			return err == nil, fmt.Sprint(err)
		})
		var st *envv1alpha1.InstanceStatus
		testutil.EventuallyWithin(t, within, "the provider's answer to the new size", func() (bool, string) {
			st = get().GetStatus()
			c := conditions.Find(st.GetConditions(), provider.InstanceResized)
			answered := c.GetObservedGeneration() >= generation &&
				(c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_FALSE || provider.SizeApplied(r, st.GetResources()))
			return answered && st.GetPhase() == phase, st.String()
		})
		return st
	}
}

// testVMResize: a VM given a new size runs at it, as its status says, with
// what its disk held; a smaller disk asked after shrinks nothing.
func testVMResize(t *testing.T, h Harness) {
	if h.BaseImage == "" {
		t.Skip("the harness names no base image")
	}
	if h.Resize == nil {
		t.Skip("the harness resizes no instance")
	}
	ctx := t.Context()
	spec := h.vm("resize")
	vm, st := h.start(t, "resize", spec)
	if !provider.SizeApplied(spec.GetResources(), st.GetResources()) {
		// A provider reports the size it applied once it has: read it again.
		st = h.Resize(t, vm, spec.GetResources())
	}
	if !provider.SizeApplied(spec.GetResources(), st.GetResources()) {
		t.Fatalf("a VM made at %v reports %v", spec.GetResources(), st.GetResources())
	}
	if err := h.Provider.WriteFile(ctx, vm, "/var/tmp/conformance/kept", 0o644, 0, strings.NewReader("kept")); err != nil {
		t.Fatal(err)
	}

	bigger := &envv1alpha1.Resources{Cpu: "2", Memory: "2Gi", Disk: "4Gi"}
	st = h.Resize(t, vm, bigger)
	if c := conditions.Find(st.GetConditions(), provider.InstanceResized); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE || !provider.SizeApplied(bigger, st.GetResources()) {
		t.Fatalf("a VM resized to %v: Resized %v, size %v", bigger, c, st.GetResources())
	}
	var got bytes.Buffer
	if err := h.Provider.ReadFile(ctx, vm, "/var/tmp/conformance/kept", &got); err != nil || got.String() != "kept" {
		t.Errorf("a VM resized reads %q, %v of what it wrote before", got.String(), err)
	}

	smaller := &envv1alpha1.Resources{Cpu: "2", Memory: "2Gi", Disk: "3Gi"}
	st = h.Resize(t, vm, smaller)
	if !provider.SizeApplied(bigger, st.GetResources()) {
		t.Errorf("a VM asked for a smaller disk reports %v; a disk never shrinks", st.GetResources())
	}
	h.release(t, vm)
}
