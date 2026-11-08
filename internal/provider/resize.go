package provider

import (
	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
)

// InstanceResized is the condition a provider sets on a VM Instance: False,
// in the target's words, while the target keeps the VM at the size it has;
// True otherwise (docs/decisions/machine-resize.md).
const InstanceResized = "Resized"

// SameResources says a and b ask for the same vCPUs, memory and disk, by
// value: "4Gi" is "4096Mi". An empty field is the same only as an empty
// one.
func SameResources(a, b *envv1alpha1.Resources) bool {
	return sameCPU(a.GetCpu(), b.GetCpu()) && sameBytes(a.GetMemory(), b.GetMemory()) && sameBytes(a.GetDisk(), b.GetDisk())
}

// SizeApplied says have, a VM's size as applied, is what want asks: the same
// vCPUs and memory, a disk at least as large (one from a larger image or
// golden is); a field want leaves empty asks nothing.
func SizeApplied(want, have *envv1alpha1.Resources) bool {
	if c := want.GetCpu(); c != "" && !sameCPU(c, have.GetCpu()) {
		return false
	}
	if m := want.GetMemory(); m != "" && !sameBytes(m, have.GetMemory()) {
		return false
	}
	if d := want.GetDisk(); d != "" {
		w, werr := quantity.Bytes(d)
		h, herr := quantity.Bytes(have.GetDisk())
		if werr != nil || herr != nil || h < w {
			return false
		}
	}
	return true
}

func sameCPU(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	x, xerr := quantity.Parse(a)
	y, yerr := quantity.Parse(b)
	if xerr != nil || yerr != nil {
		return a == b
	}
	return x.Cmp(y) == 0
}

func sameBytes(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	x, xerr := quantity.Bytes(a)
	y, yerr := quantity.Bytes(b)
	if xerr != nil || yerr != nil {
		return a == b
	}
	return x == y
}
