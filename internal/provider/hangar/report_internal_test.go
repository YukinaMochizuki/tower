package hangar

import (
	"testing"

	hangartenancy "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
)

// TestQuotaCPUFree: the vCPUs a project has left are its limit less what
// its running machines count, in thousandths, never below zero, and not
// known without a limit.
func TestQuotaCPUFree(t *testing.T) {
	limit := func(s string) *string { return &s }
	for _, tc := range []struct {
		limit *string
		used  string
		want  string
	}{
		{limit("16"), "10", "6000m"},
		{limit("16"), "", "16000m"},
		{limit("8"), "9500m", "0m"},
		{limit("2500m"), "1", "1500m"},
		{nil, "4", ""},
	} {
		p := &hangartenancy.Project{Status: &hangartenancy.ProjectStatus{
			Limits: &hangartenancy.Limits{Cpu: tc.limit}, Usage: &hangartenancy.Usage{Cpu: tc.used},
		}}
		c, _ := quota(p, nil)
		if c.GetCpuFree() != tc.want {
			t.Errorf("limit %v, used %q: cpu_free %q, want %q", tc.limit, tc.used, c.GetCpuFree(), tc.want)
		}
	}
}
