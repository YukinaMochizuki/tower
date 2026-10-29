package apiserver

import (
	"cmp"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // warm_off_peak's time zone, which the host need not have

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
	DefaultWarmMaxCommitsBehind = 20
	// DefaultWarmSettle is how long a branch head's pre-build inputs must
	// hold before its warm golden is rebuilt for them.
	DefaultWarmSettle = 30 * time.Minute
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
	// DefaultBoxWorkspaceDisk and DefaultBoxRootDisk are what a box's
	// /workspace and root disks start at on a target that grows them, and
	// the Max ones the most they grow to. hangar starts a root disk at
	// its image's base when that is larger (hangar#234), so the root's
	// start can sit below any image's size.
	DefaultBoxWorkspaceDisk    = "2Gi"
	DefaultBoxWorkspaceDiskMax = "30Gi"
	DefaultBoxRootDisk         = "2Gi"
	DefaultBoxRootDiskMax      = "30Gi"
	// DefaultBoxWorkspaceDiskGrowThreshold is how full a box's /workspace
	// disk is when it grows, and DefaultBoxWorkspaceDiskGrowStep how much
	// it grows by; the Root ones the same for its root disk. The root
	// starts at its image's base, mostly full, so it grows later and by
	// less: a factor would double the image's share of the quota.
	DefaultBoxWorkspaceDiskGrowThreshold = "80%"
	DefaultBoxWorkspaceDiskGrowStep      = "4Gi"
	DefaultBoxRootDiskGrowThreshold      = "90%"
	DefaultBoxRootDiskGrowStep           = "2Gi"
	// DefaultBoostIdle is how long a boost nothing runs on is kept; the
	// boost sizes' defaults are in boostsize.go.
	DefaultBoostIdle = 10 * time.Minute
	// DefaultBoostStartTimeout is how long a boost may take to be READY: a
	// devcontainer's four setup steps (onCreate, updateContent, postCreate,
	// postStart) at setupStepTimeout each, and 3 minutes for its instance's
	// start and the box's checkouts.
	DefaultBoostStartTimeout = 4*setupStepTimeout + 3*time.Minute
)

// DefaultHangarBoxMemory is a box's memory on a hangar target, where a
// box's memory is held from the tenant's quota in full while it idles
// (measured 2026-10-29: 240-420M used of 4Gi). Elsewhere a box has
// defaultResources' container memory.
const DefaultHangarBoxMemory = "2Gi"

// defaultResources are an environment's, by kind, for each field left
// empty.
var defaultResources = map[envv1alpha1.InstanceKind]*envv1alpha1.Resources{
	envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER: {Cpu: "2", Memory: "4Gi", Disk: "40Gi"},
	envv1alpha1.InstanceKind_INSTANCE_KIND_VM:        {Cpu: "2", Memory: "4Gi", Disk: "20Gi"},
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
	warmSettle           time.Duration
	// warmOffPeak is when a warm golden is rebuilt for staleness alone,
	// nil at any time.
	warmOffPeak *dailyWindow
	// The start and max of a box's /workspace and root disks, as
	// quantities.
	workspaceDisk, workspaceDiskMax, rootDisk, rootDiskMax string
	// How full each disk is when it grows, and by how much.
	workspaceGrowThreshold, workspaceGrowStep, rootGrowThreshold, rootGrowStep string
	// A boost's default size, the largest one, what all of the target's
	// may hold, and how long one nothing runs on is kept.
	boostResources, boostMax, boostBudget boostSize
	boostIdle, boostStartTimeout          time.Duration
}

func settingsOf(t *targetsv1alpha1.Target) settings {
	set := t.GetSpec().GetSettings()
	s := settings{
		idleStopAfter: DefaultIdleStopAfter, warmBudget: mustBytes(DefaultWarmBudget), warmWindow: DefaultWarmWindow,
		warmMaxAge: DefaultWarmMaxAge, warmMaxCommitsBehind: DefaultWarmMaxCommitsBehind, warmMinHold: DefaultWarmMinHold,
		warmSettle: DefaultWarmSettle, workspaceDisk: DefaultBoxWorkspaceDisk, workspaceDiskMax: DefaultBoxWorkspaceDiskMax,
		rootDisk: DefaultBoxRootDisk, rootDiskMax: DefaultBoxRootDiskMax,
		workspaceGrowThreshold: DefaultBoxWorkspaceDiskGrowThreshold, workspaceGrowStep: DefaultBoxWorkspaceDiskGrowStep,
		rootGrowThreshold: DefaultBoxRootDiskGrowThreshold, rootGrowStep: DefaultBoxRootDiskGrowStep,
		boostResources: defaultBoostResources, boostMax: defaultBoostMax, boostBudget: defaultBoostBudget,
		boostIdle: DefaultBoostIdle, boostStartTimeout: DefaultBoostStartTimeout,
	}
	// Read as a whole, as validateTarget checks them: one setting that
	// does not hold with the others leaves all of them tower's.
	if boostSettingsError(set) == nil {
		s.boostResources, _ = readBoostSetting(set.GetBoostResources(), defaultBoostResources)
		s.boostMax, _ = readBoostSetting(set.GetBoostMax(), defaultBoostMax)
		s.boostBudget, _ = readBoostSetting(set.GetBoostBudget(), defaultBoostBudget)
		if d := set.GetBoostIdle(); d != nil && d.AsDuration() > 0 {
			s.boostIdle = d.AsDuration()
		}
		if d := set.GetBoostStartTimeout(); d != nil && d.AsDuration() > 0 {
			s.boostStartTimeout = d.AsDuration()
		}
	}
	for _, q := range []struct {
		to  *string
		set string
	}{
		{&s.workspaceDisk, set.GetBoxWorkspaceDisk()}, {&s.workspaceDiskMax, set.GetBoxWorkspaceDiskMax()},
		{&s.rootDisk, set.GetBoxRootDisk()}, {&s.rootDiskMax, set.GetBoxRootDiskMax()},
	} {
		if _, err := quantity.Bytes(q.set); err == nil && q.set != "" {
			*q.to = q.set
		}
	}
	for _, g := range []struct {
		to    *string
		set   string
		valid func(string) error
	}{
		{&s.workspaceGrowThreshold, set.GetBoxWorkspaceDiskGrowThreshold(), validGrowThreshold},
		{&s.workspaceGrowStep, set.GetBoxWorkspaceDiskGrowStep(), validGrowStep},
		{&s.rootGrowThreshold, set.GetBoxRootDiskGrowThreshold(), validGrowThreshold},
		{&s.rootGrowStep, set.GetBoxRootDiskGrowStep(), validGrowStep},
	} {
		if g.set != "" && g.valid(g.set) == nil {
			*g.to = g.set
		}
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
	if d := set.GetWarmSettle(); d != nil && d.AsDuration() > 0 {
		s.warmSettle = d.AsDuration()
	}
	if w := set.GetWarmOffPeak(); w != nil {
		if dw, err := parseDailyWindow(w); err == nil {
			s.warmOffPeak = &dw
		}
	}
	return s
}

// dailyWindow is a TimeWindow read: minutes into the day in loc.
type dailyWindow struct {
	start, end int
	loc        *time.Location
}

func parseDailyWindow(w *targetsv1alpha1.TimeWindow) (dailyWindow, error) {
	minutes := func(field, v string) (int, error) {
		t, err := time.Parse("15:04", v)
		if err != nil {
			return 0, fmt.Errorf("%s %q is no time of day HH:MM", field, v)
		}
		return t.Hour()*60 + t.Minute(), nil
	}
	start, err := minutes("start", w.GetStart())
	if err != nil {
		return dailyWindow{}, err
	}
	end, err := minutes("end", w.GetEnd())
	if err != nil {
		return dailyWindow{}, err
	}
	if start == end {
		return dailyWindow{}, fmt.Errorf("start and end are both %s", w.GetStart())
	}
	loc, err := time.LoadLocation(w.GetTimeZone())
	if err != nil || w.GetTimeZone() == "Local" {
		return dailyWindow{}, fmt.Errorf("time_zone %q is no IANA time zone", w.GetTimeZone())
	}
	return dailyWindow{start: start, end: end, loc: loc}, nil
}

// contains reports whether t falls in the window; a nil window is always.
func (w *dailyWindow) contains(t time.Time) bool {
	if w == nil {
		return true
	}
	t = t.In(w.loc)
	m := t.Hour()*60 + t.Minute()
	if w.start < w.end {
		return w.start <= m && m < w.end
	}
	return m >= w.start || m < w.end
}

// boxDisks are the resources and disk growth of a box's container, or a
// warm golden's builder, on target t, from r, the box's own: on a target
// that grows disks, /workspace starts at r's disk, else at the setting,
// and both disks grow as the settings say; on any other, they are r with
// tower's defaults, and do not grow.
func boxDisks(t *targetsv1alpha1.Target, r *envv1alpha1.Resources) (*envv1alpha1.Resources, *envv1alpha1.DiskGrowth) {
	kind := envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER
	if t.GetSpec().GetHangar() == nil {
		return withDefaults(kind, r), nil
	}
	set := settingsOf(t)
	r = &envv1alpha1.Resources{Cpu: r.GetCpu(), Memory: cmp.Or(r.GetMemory(), DefaultHangarBoxMemory), Disk: cmp.Or(r.GetDisk(), set.workspaceDisk)}
	return withDefaults(kind, r), &envv1alpha1.DiskGrowth{
		WorkspaceMax: set.workspaceDiskMax, Root: set.rootDisk, RootMax: set.rootDiskMax,
		Threshold: set.workspaceGrowThreshold, Step: set.workspaceGrowStep,
		RootThreshold: set.rootGrowThreshold, RootStep: set.rootGrowStep,
	}
}

// The bounds of a disk's growth hangar accepts (its ParseDiskGrow).
const (
	minGrowThreshold = 50
	maxGrowThreshold = 95
	maxGrowFactor    = 16
	minGrowStep      = 1 << 30
)

// validGrowThreshold says why t is no threshold hangar accepts: a whole
// percentage from 50% to 95%.
func validGrowThreshold(t string) error {
	n, err := strconv.ParseUint(strings.TrimSuffix(t, "%"), 10, 8)
	if !strings.HasSuffix(t, "%") || err != nil || n < minGrowThreshold || n > maxGrowThreshold {
		return fmt.Errorf("%q is not a whole percentage from %d%% to %d%%", t, minGrowThreshold, maxGrowThreshold)
	}
	return nil
}

// validGrowStep says why s is no step hangar accepts: a factor over 1x
// and at most 16x, or a quantity of at least 1Gi.
func validGrowStep(s string) error {
	if f, isFactor := strings.CutSuffix(s, "x"); isFactor {
		r, ok := new(big.Rat).SetString(f)
		if !ok || strings.ContainsAny(f, "/eE") || r.Cmp(big.NewRat(1, 1)) <= 0 || r.Cmp(big.NewRat(maxGrowFactor, 1)) > 0 {
			return fmt.Errorf("%q is not a factor over 1x and at most %dx", s, maxGrowFactor)
		}
		return nil
	}
	b, err := quantity.Bytes(s)
	if err != nil {
		return fmt.Errorf("%q is neither a factor such as 2x nor a quantity: %w", s, err)
	}
	if b < minGrowStep {
		return fmt.Errorf("%s is less than 1Gi", s)
	}
	return nil
}

// growSteps is how many times a disk of start bytes grows by step, as
// hangar grows one, before it reaches maxSize: each step in whole Gi
// rounded up, maxSize in whole Gi rounded down. An empty step is hangar's
// 2x; one hangar would refuse, none.
func growSteps(start, maxSize uint64, step string) int {
	step = cmp.Or(step, "2x")
	if validGrowStep(step) != nil {
		return 0
	}
	var factor *big.Rat
	var add uint64
	if f, isFactor := strings.CutSuffix(step, "x"); isFactor {
		factor, _ = new(big.Rat).SetString(f)
	} else {
		add, _ = quantity.Bytes(step)
	}
	const gi = 1 << 30
	limit := maxSize / gi * gi
	n := 0
	for size := start; size < limit; n++ {
		if factor != nil {
			v := new(big.Rat).Mul(new(big.Rat).SetInt(new(big.Int).SetUint64(size)), factor)
			q, m := new(big.Int).QuoRem(v.Num(), v.Denom(), new(big.Int))
			if m.Sign() != 0 {
				q.Add(q, big.NewInt(1))
			}
			if !q.IsUint64() {
				return n + 1
			}
			size = q.Uint64()
		} else {
			size += add
		}
		size = (size + gi - 1) / gi * gi
	}
	return n
}

// builderDisks are boxDisks for a warm golden's builder on target t:
// /workspace starts a quarter over last, what the repository's last warm
// golden there held, to sit under the grow threshold; with none, as a
// box's, so the golden is as large as its build needed. It has the
// container default's memory, not DefaultHangarBoxMemory: it runs the
// pre-build, the build a box idles after, and lives only for it.
func builderDisks(t *targetsv1alpha1.Target, last uint64) (*envv1alpha1.Resources, *envv1alpha1.DiskGrowth) {
	if t.GetSpec().GetHangar() == nil {
		return boxDisks(t, nil)
	}
	set := settingsOf(t)
	start := set.workspaceDisk
	if last > 0 {
		gi := (last + last/4 + 1<<30 - 1) >> 30
		start = fmt.Sprintf("%dGi", min(max(gi, mustBytes(set.workspaceDisk)>>30), mustBytes(set.workspaceDiskMax)>>30))
	}
	return boxDisks(t, &envv1alpha1.Resources{Disk: start, Memory: defaultResources[envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER].GetMemory()})
}

// mustBytes reads a quantity written in this file.
func mustBytes(q string) uint64 {
	b, err := quantity.Bytes(q)
	if err != nil {
		panic(err)
	}
	return b
}
