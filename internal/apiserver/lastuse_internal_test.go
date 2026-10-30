package apiserver

import (
	"context"
	"errors"
	"testing"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// TestAWakesBoundForRoomEndsWithTheWait: a bound stopped as the box stops
// waiting for room, or as the Wake ends, does nothing when its timer fires
// all the same, as one already firing as it is stopped does; a bound
// started again fires alone.
func TestAWakesBoundForRoomEndsWithTheWait(t *testing.T) {
	box := func(phase envv1alpha1.BoxPhase, reason string) *envv1alpha1.Box {
		return &envv1alpha1.Box{Status: &envv1alpha1.BoxStatus{Phase: phase, Conditions: []*metav1.Condition{{
			Type: BoxReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: "waiting for room on its node: no room",
		}}}}
	}
	waiting := box(envv1alpha1.BoxPhase_BOX_PHASE_STARTING, provider.ReasonWaitingForRoom)
	starting := box(envv1alpha1.BoxPhase_BOX_PHASE_STARTING, "Starting")
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	r := &roomWait{box: "dev", wait: time.Hour, stop: cancel}
	r.see(waiting)
	r.see(starting)
	r.fire(1)
	if r.answered() != nil || ctx.Err() != nil {
		t.Fatalf("a bound stopped as the box started fired: %v", r.answered())
	}
	r.see(waiting)
	r.fire(1)
	if r.answered() != nil {
		t.Fatalf("the first bound fired after a second started: %v", r.answered())
	}
	r.fire(2)
	if r.answered() == nil || !errors.Is(context.Cause(ctx), r.answered()) {
		t.Fatalf("the bound started again did not stop the Wake: %v, %v", r.answered(), context.Cause(ctx))
	}
	ended := &roomWait{box: "dev", wait: time.Hour, stop: cancel}
	ended.see(waiting)
	ended.end()
	ended.fire(1)
	if ended.answered() != nil {
		t.Errorf("a bound fired after its Wake ended: %v", ended.answered())
	}
}
