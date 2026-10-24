package conditions_test

import (
	"testing"
	"time"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/conditions"
)

const (
	condTrue  = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	condFalse = metav1.ConditionStatus_CONDITION_STATUS_FALSE
)

func TestSetKeepsTransitionTimeWhileStatusHolds(t *testing.T) {
	t0 := time.Unix(1000, 0)
	t1 := t0.Add(time.Minute)
	t2 := t1.Add(time.Minute)

	list := conditions.Set(nil, &metav1.Condition{Type: "Ready", Status: condTrue, Reason: "A"}, t0)
	list = conditions.Set(list, &metav1.Condition{Type: "Other", Status: condFalse}, t0)
	list = conditions.Set(list, &metav1.Condition{Type: "Ready", Status: condTrue, Reason: "B"}, t1)

	ready := conditions.Find(list, "Ready")
	if got := ready.GetLastTransitionTime().AsTime(); !got.Equal(t0) {
		t.Errorf("same status moved the transition time to %v", got)
	}
	if ready.GetReason() != "B" || len(list) != 2 {
		t.Errorf("not replaced in place: %v", list)
	}

	list = conditions.Set(list, &metav1.Condition{Type: "Ready", Status: condFalse}, t2)
	if got := conditions.Find(list, "Ready").GetLastTransitionTime().AsTime(); !got.Equal(t2) {
		t.Errorf("a status change kept the old transition time %v", got)
	}
	if conditions.IsTrue(list, "Ready") || conditions.IsTrue(list, "Missing") {
		t.Error("IsTrue reported a false or missing condition as true")
	}
}

func TestSetDoesNotModifyItsInputs(t *testing.T) {
	in := []*metav1.Condition{{Type: "Ready", Status: condTrue}}
	c := &metav1.Condition{Type: "Ready", Status: condFalse}
	_ = conditions.Set(in, c, time.Unix(1, 0))
	if in[0].GetStatus() != condTrue || c.GetLastTransitionTime() != nil {
		t.Error("Set modified its inputs")
	}
}
