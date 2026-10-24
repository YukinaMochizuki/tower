package apiserver

import (
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
)

// tower's defaults: what a Target's unset settings, and an environment's
// unset resources, are. They are tower's policy, not workstream's, and are
// written down here alone.
const (
	// DefaultIdleStopAfter is how long a box goes unused before it is
	// stopped.
	DefaultIdleStopAfter = 2 * time.Hour
	// DefaultWarmBudget is how much disk warm goldens may hold on a target.
	DefaultWarmBudget = "100Gi"
	// DefaultWarmWindow is how far back a repository's use counts.
	DefaultWarmWindow = 14 * 24 * time.Hour
	// DefaultWarmMaxAge and DefaultWarmMaxCommitsBehind are how stale a
	// warm golden may get before it is rebuilt.
	DefaultWarmMaxAge           = 7 * 24 * time.Hour
	DefaultWarmMaxCommitsBehind = 200
	// DefaultWarmEstimate is what a warm golden not built yet is taken to
	// hold, until its first one says.
	DefaultWarmEstimate = "5Gi"
	// DefaultWarmMinHold is how long a repository that took a warm golden
	// keeps its place, whatever the ranking says, so use that shifts for a
	// day does not rebuild goldens back and forth.
	DefaultWarmMinHold = 24 * time.Hour
	// WarmChallengeMargin is by how much a repository's score must beat a
	// holder's to take its place: 0.2 is by 20%.
	WarmChallengeMargin = 0.2
	// DefaultReportInterval is how often a provider reports its target's
	// status; one silent for ReportsMissed of them is not Reporting.
	DefaultReportInterval = time.Minute
	ReportsMissed         = 3
	// DiskPressureMargin is how much more than the warm budget a target
	// must have free not to be under DiskPressure.
	DiskPressureMargin = "20Gi"
	// DefaultGoldenPruneAfter is how long a golden nothing was made from,
	// and nothing references, is kept.
	DefaultGoldenPruneAfter = 7 * 24 * time.Hour
	// MirrorFetchEvery is how often a repository's mirror is fetched again.
	MirrorFetchEvery = 10 * time.Minute
	// LastUseFlushEvery is how often Touch's uses are written to the boxes.
	LastUseFlushEvery = time.Minute
)

// defaultResources are an environment's, by kind, for each field left
// empty.
var defaultResources = map[envv1alpha1.InstanceKind]*envv1alpha1.Resources{
	envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER: {Cpu: "2", Memory: "4Gi", Disk: "40Gi"},
	envv1alpha1.InstanceKind_INSTANCE_KIND_VM:        {Cpu: "2", Memory: "4Gi", Disk: "40Gi"},
}

// withDefaults is r with each empty field tower's default for kind.
func withDefaults(kind envv1alpha1.InstanceKind, r *envv1alpha1.Resources) *envv1alpha1.Resources {
	d := defaultResources[kind]
	out := &envv1alpha1.Resources{Cpu: r.GetCpu(), Memory: r.GetMemory(), Disk: r.GetDisk()}
	if out.Cpu == "" {
		out.Cpu = d.GetCpu()
	}
	if out.Memory == "" {
		out.Memory = d.GetMemory()
	}
	if out.Disk == "" {
		out.Disk = d.GetDisk()
	}
	return out
}

// settings are a target's policy settings with tower's defaults in place
// of those unset.
type settings struct {
	idleStopAfter        time.Duration
	warmBudget           uint64
	warmWindow           time.Duration
	warmMaxAge           time.Duration
	warmMaxCommitsBehind int
	warmMinHold          time.Duration
}

func settingsOf(t *targetsv1alpha1.Target) settings {
	set := t.GetSpec().GetSettings()
	s := settings{
		idleStopAfter: DefaultIdleStopAfter, warmBudget: mustBytes(DefaultWarmBudget), warmWindow: DefaultWarmWindow,
		warmMaxAge: DefaultWarmMaxAge, warmMaxCommitsBehind: DefaultWarmMaxCommitsBehind, warmMinHold: DefaultWarmMinHold,
	}
	if d := set.GetIdleStopAfter(); d != nil && d.AsDuration() > 0 {
		s.idleStopAfter = d.AsDuration()
	}
	if b, err := quantity.Bytes(set.GetWarmBudget()); err == nil && set.GetWarmBudget() != "" {
		s.warmBudget = b
	}
	if d := set.GetWarmWindow(); d != nil && d.AsDuration() > 0 {
		s.warmWindow = d.AsDuration()
	}
	if d := set.GetWarmMaxAge(); d != nil && d.AsDuration() > 0 {
		s.warmMaxAge = d.AsDuration()
	}
	if n := set.GetWarmMaxCommitsBehind(); n > 0 {
		s.warmMaxCommitsBehind = int(n)
	}
	return s
}

// mustBytes reads a quantity written in this file.
func mustBytes(q string) uint64 {
	b, err := quantity.Bytes(q)
	if err != nil {
		panic(err)
	}
	return b
}
