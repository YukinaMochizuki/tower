package apiserver

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// The reasons a golden is evicted for, as its log line says.
const (
	evictOverBudget = "over budget"
	evictLowOnDisk  = "low on disk"
)

// GoldenFreedWait is how long the disk of evicted goldens is taken as freed
// while the target's reports do not show it yet: a provider deletes in its
// own time, and reads its free disk before it stamps a report.
const GoldenFreedWait = 10 * time.Minute

// goldenEvictedRecord is a target's goldenEvictedKind record: the disk its
// last evictions freed, the free disk reported when they were made, and
// when.
type goldenEvictedRecord struct {
	Freed    uint64    `json:"freed"`
	BaseFree uint64    `json:"baseFree"`
	At       time.Time `json:"at"`
}

func goldenEvictedKey(target string) store.Key {
	return store.Key{Kind: goldenEvictedKind, Name: target}
}

// unreported is what of the record's freed disk a report of free does not
// show yet, while GoldenFreedWait has not passed.
func (r goldenEvictedRecord) unreported(free uint64, now time.Time) uint64 {
	if now.Sub(r.At) >= GoldenFreedWait {
		return 0
	}
	var seen uint64
	if free > r.BaseFree {
		seen = free - r.BaseFree
	}
	if seen >= r.Freed {
		return 0
	}
	return r.Freed - seen
}

// goldenShortfalls are the targets whose goldens nothing references were
// last too few to free what their budget needed, which is logged once.
type goldenShortfalls struct {
	mu    sync.Mutex
	short map[string]bool
}

// set records whether the target fell short, and reports whether that
// changed.
func (g *goldenShortfalls) set(target string, short bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.short == nil {
		g.short = map[string]bool{}
	}
	changed := g.short[target] != short
	g.short[target] = short
	return changed
}

// reconcileGoldenBudget evicts the target's goldens other than warm ones
// that nothing references, least recently used first: over golden_budget,
// down to golden_budget_low; with less free than DiskPressure needs and
// GoldenEvictCushion, until it would have that (docs in target.proto).
func (s *Server) reconcileGoldenBudget(ctx context.Context, key controller.Key) error {
	t, live, err := s.liveTarget(ctx, key.Name)
	if err != nil || !live {
		return err
	}
	set := settingsOf(t)
	goldens, err := listTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, "")
	if err != nil {
		return err
	}
	var usage uint64
	var candidates []*envv1alpha1.Golden
	for _, g := range goldens {
		if _, warm := g.GetMetadata().GetLabels()[warmLabel]; warm || g.GetSpec().GetTarget() != key.Name || g.GetMetadata().GetDeleteTime() != nil {
			continue
		}
		switch goldenPhase(g) {
		case envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY:
			if size := goldenSize(g); size > 0 {
				usage += size
				candidates = append(candidates, g)
			}
		case envv1alpha1.GoldenPhase_GOLDEN_PHASE_BUILDING:
			n, err := s.buildingGoldenSize(ctx, g)
			if err != nil {
				return err
			}
			usage += n
		}
	}
	var need uint64
	reason := ""
	if usage > set.goldenBudget {
		need, reason = usage-set.goldenBudgetLow, evictOverBudget
	}
	var last goldenEvictedRecord
	if _, err := s.getRecord(ctx, goldenEvictedKey(key.Name), &last); err != nil {
		return err
	}
	now := s.clock.Now()
	free, reported := reportedDiskFree(t)
	if reported {
		free += last.unreported(free, now)
		line := set.warmBudget + mustBytes(DiskPressureMargin) + mustBytes(GoldenEvictCushion)
		if free < line && line-free > need {
			need, reason = line-free, evictLowOnDisk
		}
	}
	if need == 0 {
		s.goldenShortfalls.set(key.Name, false)
		return nil
	}
	slices.SortFunc(candidates, func(a, b *envv1alpha1.Golden) int {
		return cmp.Or(goldenLastUse(a).Compare(goldenLastUse(b)),
			cmp.Compare(a.GetMetadata().GetProject()+"/"+a.GetMetadata().GetName(), b.GetMetadata().GetProject()+"/"+b.GetMetadata().GetName()))
	})
	refs := map[string]goldenRefs{}
	var freed uint64
	for _, g := range candidates {
		if freed >= need {
			break
		}
		meta := g.GetMetadata()
		r, ok := refs[meta.GetProject()]
		if !ok {
			if r, err = s.goldenRefsIn(ctx, meta.GetProject()); err != nil {
				return err
			}
			refs[meta.GetProject()] = r
		}
		if r.has(g) {
			continue
		}
		rv := meta.GetResourceVersion()
		if _, err := s.backend(registry.Golden).Delete(s.system(ctx), resource.Key{Project: meta.GetProject(), Name: meta.GetName()}, &rv); err != nil {
			if isNotFound(err) {
				continue
			}
			return err
		}
		size := goldenSize(g)
		freed += size
		// Recorded at once: the next pass must not count its disk free
		// before a report shows it.
		if reported {
			reportedFree, _ := reportedDiskFree(t)
			last = goldenEvictedRecord{Freed: last.unreported(reportedFree, now) + size, BaseFree: reportedFree, At: now}
			if err := s.putRecord(ctx, goldenEvictedKey(key.Name), last); err != nil {
				return err
			}
		}
		s.log.Info("evicted a golden", "target", key.Name, "project", meta.GetProject(), "name", meta.GetName(),
			"size", g.GetStatus().GetSize(), "last_used", goldenLastUse(g).UTC().Format(time.RFC3339), "reason", reason,
			"usage", quantity.FormatDisk(usage), "budget", quantity.FormatDisk(set.goldenBudget), "budget_low", quantity.FormatDisk(set.goldenBudgetLow),
			"disk_free", t.GetStatus().GetCapacity().GetDiskFree(), "need", quantity.FormatDisk(need))
	}
	if s.goldenShortfalls.set(key.Name, freed < need) && freed < need {
		s.log.Warn("the goldens nothing references hold less than the golden budget needs freed", "target", key.Name,
			"reason", reason, "need", quantity.FormatDisk(need), "freed", quantity.FormatDisk(freed))
	}
	return nil
}

// reportedDiskFree is the target's free disk as its provider last
// reported it, and whether it has.
func reportedDiskFree(t *targetsv1alpha1.Target) (uint64, bool) {
	q := t.GetStatus().GetCapacity().GetDiskFree()
	if q == "" || t.GetStatus().GetLastReportTime() == nil {
		return 0, false
	}
	b, err := quantity.Bytes(q)
	return b, err == nil
}

// buildingGoldenSize is what a golden being built is taken to hold once
// captured: a machine's, its builder's disk, which a capture does not
// outgrow, so room is made before the build fills it; any other, none.
func (s *Server) buildingGoldenSize(ctx context.Context, g *envv1alpha1.Golden) (uint64, error) {
	if _, isMachine := g.GetSpec().GetInputs().(*envv1alpha1.GoldenSpec_Machine); !isMachine {
		return 0, nil
	}
	disk := withDefaults(envv1alpha1.InstanceKind_INSTANCE_KIND_VM, nil).GetDisk()
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: g.GetMetadata().GetProject(), Name: childName("build", g.GetMetadata().GetName())})
	if err != nil {
		return 0, err
	}
	if ok && owns(registry.Golden, g, in) && in.GetSpec().GetResources().GetDisk() != "" {
		disk = in.GetSpec().GetResources().GetDisk()
	}
	// A quantity, which an instance is checked for when made.
	return quantity.Bytes(disk)
}
