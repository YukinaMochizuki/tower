package hangar

import "testing"

func TestMeasures(t *testing.T) {
	if !measures(placed{}) {
		t.Error("a container's processes are not measured")
	}
	if measures(placed{vm: true}) {
		t.Error("a VM's processes are measured")
	}
}
