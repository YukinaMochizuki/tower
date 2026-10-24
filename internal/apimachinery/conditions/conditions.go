// Package conditions reads and writes the condition lists every status
// carries. It keeps last_transition_time meaning what its name says: the time
// the status last changed, not the time anyone last wrote the condition.
package conditions

import (
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
)

// Find returns the condition of the given type, or nil.
func Find(list []*metav1.Condition, typ string) *metav1.Condition {
	for _, c := range list {
		if c.GetType() == typ {
			return c
		}
	}
	return nil
}

// IsTrue reports whether the condition of the given type is present and true.
func IsTrue(list []*metav1.Condition, typ string) bool {
	return Find(list, typ).GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE
}

// Set returns list with c in place of any condition of the same type. The
// transition time is now when the status changes, and carried over when it
// does not. The input list and c are not modified.
func Set(list []*metav1.Condition, c *metav1.Condition, now time.Time) []*metav1.Condition {
	c = proto.Clone(c).(*metav1.Condition)
	out := make([]*metav1.Condition, 0, len(list)+1)
	replaced := false
	for _, old := range list {
		if old.GetType() != c.GetType() {
			out = append(out, old)
			continue
		}
		replaced = true
		if old.GetStatus() == c.GetStatus() && old.GetLastTransitionTime() != nil {
			c.LastTransitionTime = old.GetLastTransitionTime()
		} else {
			c.LastTransitionTime = timestamppb.New(now)
		}
		out = append(out, c)
	}
	if !replaced {
		c.LastTransitionTime = timestamppb.New(now)
		out = append(out, c)
	}
	return out
}
