package apiserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/controller"
)

// TargetDiskPressure says a target has less disk free than its warm budget
// and a margin need. tower's controllers own it, counts and usage of
// goldens and instances, and a hostd target's Reachable and last_error
// (reachable); the provider owns the rest of TargetStatus (capacity,
// facts, a hangar target's Reachable, Reporting, the other usage), which
// they leave as it is.
const TargetDiskPressure = "DiskPressure"

// reconcileTargetStatus counts what tower holds on the target into its
// status.
func (s *Server) reconcileTargetStatus(ctx context.Context, key controller.Key) error {
	t, live, err := s.liveTarget(ctx, key.Name)
	if err != nil || !live {
		return err
	}
	instances, err := listTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, "")
	if err != nil {
		return err
	}
	goldens, err := listTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, "")
	if err != nil {
		return err
	}
	counts := &targetsv1alpha1.Counts{Instances: map[string]int32{}}
	var instanceDisk, goldenDisk uint64
	for _, in := range instances {
		if in.GetSpec().GetTarget() != key.Name {
			continue
		}
		phase := strings.TrimPrefix(in.GetStatus().GetPhase().String(), "INSTANCE_PHASE_")
		counts.Instances[phase]++
		if b, err := quantity.Bytes(in.GetStatus().GetUsedDisk()); err == nil {
			instanceDisk += b
		}
	}
	for _, g := range goldens {
		if g.GetSpec().GetTarget() != key.Name || g.GetMetadata().GetDeleteTime() != nil {
			continue
		}
		counts.Goldens++
		if b, err := quantity.Bytes(g.GetStatus().GetSize()); err == nil {
			goldenDisk += b
		}
	}
	st := proto.Clone(t.GetStatus()).(*targetsv1alpha1.TargetStatus)
	if st == nil {
		st = &targetsv1alpha1.TargetStatus{}
	}
	st.ObservedGeneration = t.GetMetadata().GetGeneration()
	st.Counts = counts
	if st.Usage == nil {
		st.Usage = &targetsv1alpha1.Usage{}
	}
	st.Usage.Goldens, st.Usage.Instances = quantity.FormatDisk(goldenDisk), quantity.FormatDisk(instanceDisk)
	if t.GetSpec().GetHostd() != nil {
		s.reachable(key.Name, st)
	}
	if free, err := quantity.Bytes(st.GetCapacity().GetDiskFree()); err == nil && st.GetCapacity().GetDiskFree() != "" {
		need := settingsOf(t).warmBudget + mustBytes(DiskPressureMargin)
		c := &metav1.Condition{Type: TargetDiskPressure, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "EnoughFree",
			Message: fmt.Sprintf("%s free, %s needed", quantity.FormatDisk(free), quantity.FormatDisk(need))}
		if free < need {
			c.Status, c.Reason = metav1.ConditionStatus_CONDITION_STATUS_TRUE, "LowOnDisk"
		}
		st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), c, s.clock.Now())
	}
	return s.updateStatus(ctx, registry.Target, t, st)
}
