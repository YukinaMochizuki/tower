package hangar

import (
	"testing"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

func TestMeasure(t *testing.T) {
	withMemory := func(m string) *envv1alpha1.Instance {
		return &envv1alpha1.Instance{Spec: &envv1alpha1.InstanceSpec{Resources: &envv1alpha1.Resources{Memory: m}}}
	}
	for _, tc := range []struct {
		name    string
		p       placed
		measure bool
		max     int64
	}{
		{"a container of 4Gi", placed{in: withMemory("4Gi")}, true, 4<<30 - provider.ProcessMemoryReserve},
		{"a small container keeps half", placed{in: withMemory("1Gi")}, true, 512 << 20},
		{"a container without memory is unbounded", placed{in: withMemory("")}, true, 0},
		{"a VM is not measured", placed{in: withMemory("4Gi"), vm: true}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			measure, max := measure(tc.p)
			if measure != tc.measure || max != tc.max {
				t.Errorf("measure = %v, %d; want %v, %d", measure, max, tc.measure, tc.max)
			}
		})
	}
}
