package apiserver

import (
	"testing"

	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
)

// TestGoldenBudgetSettings: unset, the golden budget is tower's; a high
// mark set alone brings its low one to three quarters of it.
func TestGoldenBudgetSettings(t *testing.T) {
	t.Parallel()
	const gi = 1 << 30
	for _, c := range []struct {
		set       *targetsv1alpha1.TargetSettings
		high, low uint64
	}{
		{nil, 200 * gi, 150 * gi},
		{&targetsv1alpha1.TargetSettings{GoldenBudget: "400Gi"}, 400 * gi, 300 * gi},
		{&targetsv1alpha1.TargetSettings{GoldenBudgetLow: "100Gi"}, 200 * gi, 100 * gi},
		{&targetsv1alpha1.TargetSettings{GoldenBudget: "80Gi", GoldenBudgetLow: "80Gi"}, 80 * gi, 80 * gi},
	} {
		s := settingsOf(&targetsv1alpha1.Target{Spec: &targetsv1alpha1.TargetSpec{Settings: c.set}})
		if s.goldenBudget != c.high || s.goldenBudgetLow != c.low {
			t.Errorf("settings %v: golden budget %d/%d, want %d/%d", c.set, s.goldenBudget, s.goldenBudgetLow, c.high, c.low)
		}
	}
}
