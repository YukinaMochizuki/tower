package apiserver

import (
	"fmt"
	"math/big"

	"google.golang.org/protobuf/types/known/durationpb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
)

// hangar's shapes (hangar: docs/decisions/machine-sizes.md): 512Mi to 8Gi
// of memory per vCPU, one vCPU always allowed.
const (
	minMemoryPerCPU = 512 << 20
	maxMemoryPerCPU = 8 << 30
)

// boostSize is an amount of cpu, in thousandths of a vCPU, and memory, in
// bytes: a boost's size, what a budget holds, or what a quota has left.
type boostSize struct {
	milliCPU int64
	memory   uint64
}

func (b boostSize) String() string {
	return fmt.Sprintf("%s vCPU and %s", formatMilliCPU(b.milliCPU), quantity.FormatMemory(b.memory))
}

func formatMilliCPU(m int64) string {
	if m%1000 == 0 {
		return fmt.Sprint(m / 1000)
	}
	return fmt.Sprintf("%dm", m)
}

// resources is b as an Instance's resources say it.
func (b boostSize) resources() *envv1alpha1.Resources {
	return &envv1alpha1.Resources{Cpu: formatMilliCPU(b.milliCPU), Memory: quantity.FormatMemory(b.memory)}
}

// covers says b is at least o in both.
func (b boostSize) covers(o boostSize) bool { return b.milliCPU >= o.milliCPU && b.memory >= o.memory }

func (b boostSize) plus(o boostSize) boostSize {
	return boostSize{milliCPU: b.milliCPU + o.milliCPU, memory: b.memory + o.memory}
}

// minus is b less o, none below zero.
func (b boostSize) minus(o boostSize) boostSize {
	out := boostSize{milliCPU: max(b.milliCPU-o.milliCPU, 0)}
	if b.memory > o.memory {
		out.memory = b.memory - o.memory
	}
	return out
}

// parseMilliCPU reads a cpu quantity, such as "8" or "6500m", in
// thousandths of a vCPU, rounded down.
func parseMilliCPU(s string) (int64, error) {
	v, err := quantity.Parse(s)
	if err != nil {
		return 0, err
	}
	if v.Sign() < 0 {
		return 0, fmt.Errorf("cpu %q is negative", s)
	}
	m := new(big.Rat).Mul(v, big.NewRat(1000, 1))
	q := new(big.Int).Quo(m.Num(), m.Denom())
	if !q.IsInt64() {
		return 0, fmt.Errorf("cpu %q is too large", s)
	}
	return q.Int64(), nil
}

// parseBoostSize reads cpu, whole vCPUs, and memory, each from def when
// empty.
func parseBoostSize(cpu, memory string, def boostSize) (boostSize, error) {
	out := def
	if cpu != "" {
		n, err := quantity.CPU(cpu)
		if err != nil {
			return boostSize{}, err
		}
		out.milliCPU = int64(n) * 1000
	}
	if memory != "" {
		b, err := quantity.Bytes(memory)
		if err != nil {
			return boostSize{}, err
		}
		if b == 0 {
			return boostSize{}, fmt.Errorf("memory %q is zero", memory)
		}
		out.memory = b
	}
	return out, nil
}

// shapeError says why b is no shape hangar makes, nil if it is one.
func (b boostSize) shapeError() error {
	cpus := uint64(b.milliCPU / 1000)
	if b.milliCPU%1000 != 0 || cpus < 1 {
		return fmt.Errorf("%s: its vCPUs are whole, at least one", b)
	}
	if cpus == 1 {
		return nil
	}
	if cpus < (b.memory+maxMemoryPerCPU-1)/maxMemoryPerCPU || cpus > b.memory/minMemoryPerCPU {
		return fmt.Errorf("%s: hangar's machines have 512Mi to 8Gi of memory per vCPU", b)
	}
	return nil
}

// shaped is b held to most, then its vCPUs moved, as little as may be,
// into hangar's shapes for its memory: memory is what a job runs out of.
func (b boostSize) shaped(most boostSize) boostSize {
	b.milliCPU, b.memory = min(b.milliCPU, most.milliCPU), min(b.memory, most.memory)
	cpus := max(b.milliCPU/1000, 1)
	least := int64((b.memory + maxMemoryPerCPU - 1) / maxMemoryPerCPU)
	if cpus < least {
		cpus = least
	}
	if top := max(int64(b.memory/minMemoryPerCPU), 1); cpus > top {
		cpus = top
	}
	b.milliCPU = cpus * 1000
	return b
}

// boostSizeOf reads a Resources' cpu and memory; what it cannot read is
// zero.
func boostSizeOf(r *envv1alpha1.Resources) boostSize {
	var out boostSize
	out.milliCPU, _ = parseMilliCPU(r.GetCpu())
	out.memory, _ = quantity.Bytes(r.GetMemory())
	return out
}

// tower's boost defaults (TargetSettings).
var (
	defaultBoostResources = boostSize{milliCPU: 8000, memory: 8 << 30}
	defaultBoostMax       = boostSize{milliCPU: 8000, memory: 16 << 30}
	defaultBoostBudget    = boostSize{milliCPU: 16000, memory: 16 << 30}
)

// readBoostSetting is a BoostSize setting with def for what it leaves
// unset, and why it is not one.
func readBoostSetting(set *targetsv1alpha1.BoostSize, def boostSize) (boostSize, error) {
	return parseBoostSize(set.GetCpu(), set.GetMemory(), def)
}

// boostSettingsError says why the target's boost settings do not hold
// together, nil if they do: each size but the budget is a shape hangar
// makes, the default is within the max, and the durations are not
// negative. The budget may be below one boost: one is let in at a time.
func boostSettingsError(set *targetsv1alpha1.TargetSettings) error {
	vals := map[string]boostSize{}
	for _, f := range []struct {
		name string
		set  *targetsv1alpha1.BoostSize
		def  boostSize
	}{
		{"boost_resources", set.GetBoostResources(), defaultBoostResources},
		{"boost_max", set.GetBoostMax(), defaultBoostMax},
		{"boost_budget", set.GetBoostBudget(), defaultBoostBudget},
	} {
		v, err := readBoostSetting(f.set, f.def)
		if err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
		if f.name != "boost_budget" {
			if err := v.shapeError(); err != nil {
				return fmt.Errorf("%s is %w", f.name, err)
			}
		}
		vals[f.name] = v
	}
	if !vals["boost_max"].covers(vals["boost_resources"]) {
		return fmt.Errorf("boost_resources (%s) is more than boost_max (%s)", vals["boost_resources"], vals["boost_max"])
	}
	for name, d := range map[string]*durationpb.Duration{"boost_idle": set.GetBoostIdle(), "boost_start_timeout": set.GetBoostStartTimeout()} {
		if d != nil && (!d.IsValid() || d.AsDuration() < 0) {
			return fmt.Errorf("%s must be a duration of zero or more", name)
		}
	}
	return nil
}
