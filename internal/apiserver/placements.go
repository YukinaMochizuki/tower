package apiserver

import (
	"context"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
)

// PlacementReady says a placement's target exists and is not being
// deleted, so its project's environments can be made.
const PlacementReady = "Ready"

// reconcilePlacement reports whether the placement can be used. A
// placement whose target went anyway, as the system may delete one, stays
// and says so.
func (s *Server) reconcilePlacement(ctx context.Context, key controller.Key) error {
	pl, ok, err := getTyped[*targetsv1alpha1.Placement](ctx, s, registry.Placement, resource.Key{Name: key.Name})
	if err != nil || !ok {
		return err
	}
	ready := &metav1.Condition{Type: PlacementReady, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "TargetReady", ObservedGeneration: pl.GetMetadata().GetGeneration()}
	if _, live, err := s.liveTarget(ctx, pl.GetSpec().GetTarget()); err != nil {
		return err
	} else if !live {
		ready.Status, ready.Reason = metav1.ConditionStatus_CONDITION_STATUS_FALSE, "TargetGone"
		ready.Message = fmt.Sprintf("target %q does not exist or is being deleted", pl.GetSpec().GetTarget())
	}
	st := proto.Clone(pl.GetStatus()).(*targetsv1alpha1.PlacementStatus)
	if st == nil {
		st = &targetsv1alpha1.PlacementStatus{}
	}
	st.ObservedGeneration = pl.GetMetadata().GetGeneration()
	st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), ready, s.clock.Now())
	return s.updateStatus(ctx, registry.Placement, pl, st)
}
