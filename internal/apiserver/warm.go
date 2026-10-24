package apiserver

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
)

// warmCandidate is a repository of a project placed on the target, with
// its warm goldens.
type warmCandidate struct {
	project string
	repo    *envv1alpha1.Repository
	goldens []*envv1alpha1.Golden
}

func (c warmCandidate) name() string { return c.repo.GetMetadata().GetName() }

// used reports whether the repository was used within its window at all;
// one never used gets no warm golden.
func (c warmCandidate) used() bool {
	u := c.repo.GetStatus().GetUse()
	return u.GetBoxesOpened() > 0 || u.GetBoxSeconds() > 0
}

// newestReady is the repository's newest ready warm golden, nil if none.
func (c warmCandidate) newestReady() *envv1alpha1.Golden {
	var best *envv1alpha1.Golden
	for _, g := range c.goldens {
		if goldenPhase(g) == envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY &&
			(best == nil || g.GetMetadata().GetCreateTime().AsTime().After(best.GetMetadata().GetCreateTime().AsTime())) {
			best = g
		}
	}
	return best
}

// size is what the repository's warm golden holds: its newest ready one's,
// or the last one tower saw, or DefaultWarmEstimate before any was built.
// The last size seen keeps one that fell out of the budget out, rather
// than taken back in on an estimate and dropped again once built.
func (s *Server) warmSize(c warmCandidate) uint64 {
	key := controller.Key{Project: c.project, Name: c.name()}
	s.warmSizesMu.Lock()
	defer s.warmSizesMu.Unlock()
	if g := c.newestReady(); g != nil {
		if b, err := quantity.Bytes(g.GetStatus().GetSize()); err == nil && b > 0 {
			if s.warmSizes == nil {
				s.warmSizes = map[controller.Key]uint64{}
			}
			s.warmSizes[key] = b
			return b
		}
	}
	if b, ok := s.warmSizes[key]; ok {
		return b
	}
	return mustBytes(DefaultWarmEstimate)
}

// reconcileWarm keeps the target's warm goldens: the repositories of the
// projects placed on it, ranked by use, get one from the top down while
// their sizes fit the target's warm_budget; one that falls out loses its,
// one never used gets none, and one older than warm_max_age or more than
// warm_max_commits_behind behind its branch is rebuilt.
func (s *Server) reconcileWarm(ctx context.Context, key controller.Key) error {
	t, live, err := s.liveTarget(ctx, key.Name)
	if err != nil || !live {
		return err
	}
	set := settingsOf(t)
	candidates, err := s.warmCandidates(ctx, t)
	if err != nil {
		return err
	}
	slices.SortFunc(candidates, func(a, b warmCandidate) int {
		ua, ub := a.repo.GetStatus().GetUse(), b.repo.GetStatus().GetUse()
		return cmp.Or(
			cmp.Compare(ub.GetBoxSeconds(), ua.GetBoxSeconds()),
			cmp.Compare(ub.GetBoxesOpened(), ua.GetBoxesOpened()),
			ub.GetLastUseTime().AsTime().Compare(ua.GetLastUseTime().AsTime()),
			cmp.Compare(a.project+"/"+a.name(), b.project+"/"+b.name()),
		)
	})
	var held uint64
	fits := true
	var waiting error
	for _, c := range candidates {
		if size := s.warmSize(c); fits && c.used() && held+size <= set.warmBudget {
			held += size
			if err := s.keepWarm(ctx, t, set, c); err != nil {
				if waiting == nil {
					waiting = err
				}
			}
			continue
		}
		// The ranking is kept: a smaller repository further down does not
		// take a place before a bigger one above it.
		if c.used() {
			fits = false
		}
		if err := s.dropWarm(ctx, c.goldens); err != nil {
			return err
		}
	}
	return waiting
}

func (s *Server) warmCandidates(ctx context.Context, t *targetsv1alpha1.Target) ([]warmCandidate, error) {
	placements, err := listTyped[*targetsv1alpha1.Placement](ctx, s, registry.Placement, "")
	if err != nil {
		return nil, err
	}
	var out []warmCandidate
	for _, pl := range placements {
		if pl.GetSpec().GetTarget() != t.GetMetadata().GetName() || pl.GetMetadata().GetDeleteTime() != nil {
			continue
		}
		project := pl.GetMetadata().GetName()
		repos, err := listTyped[*envv1alpha1.Repository](ctx, s, registry.Repository, project)
		if err != nil {
			return nil, err
		}
		goldens, err := listTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, project)
		if err != nil {
			return nil, err
		}
		for _, r := range repos {
			if r.GetMetadata().GetDeleteTime() != nil {
				continue
			}
			c := warmCandidate{project: project, repo: r}
			for _, g := range goldens {
				if g.GetMetadata().GetLabels()[warmLabel] == r.GetMetadata().GetName() && g.GetMetadata().GetDeleteTime() == nil &&
					g.GetSpec().GetTarget() == t.GetMetadata().GetName() {
					c.goldens = append(c.goldens, g)
				}
			}
			out = append(out, c)
		}
	}
	return out, nil
}

// dropWarm deletes warm goldens; their artifacts go with them.
func (s *Server) dropWarm(ctx context.Context, goldens []*envv1alpha1.Golden) error {
	for _, g := range goldens {
		meta := g.GetMetadata()
		s.log.Info("dropping a warm golden", "project", meta.GetProject(), "name", meta.GetName(), "repository", meta.GetLabels()[warmLabel])
		if _, err := s.backend(registry.Golden).Delete(s.system(ctx), resource.Key{Project: meta.GetProject(), Name: meta.GetName()}, nil); err != nil && !isNotFound(err) {
			return err
		}
	}
	return nil
}

// keepWarm gives the repository a warm golden of its branch's head, unless
// a fresh one is ready, and drops the older ones once it is.
func (s *Server) keepWarm(ctx context.Context, t *targetsv1alpha1.Target, set settings, c warmCandidate) error {
	target := t.GetMetadata().GetName()
	key := controller.Key{Project: c.project, Name: c.name()}
	head := mirrorHead(c.repo, target)
	if head == "" {
		s.fetchNow(key)
		return nil
	}
	mirror := mirrorName(c.project, c.name())
	ready := c.newestReady()
	stale := ready == nil
	// A rebuild for age alone, of the same commit, is a new golden all the
	// same: it is told apart by when its predecessor expired, which every
	// reconcile reads the same.
	epoch := ""
	if ready != nil {
		expired := ready.GetMetadata().GetCreateTime().AsTime().Add(set.warmMaxAge)
		if s.clock.Now().After(expired) {
			stale, epoch = true, expired.UTC().Format(time.RFC3339)
		}
	}
	if ready != nil && !stale && ready.GetSpec().GetBoxWarm().GetCommit() != head {
		p, err := s.providerFor(target)
		if err != nil {
			return fmt.Errorf("warm goldens of %s: %w: %w", target, controller.ErrWaiting, err)
		}
		behind, err := p.CommitsBehind(ctx, mirror, ready.GetSpec().GetBoxWarm().GetCommit(), c.repo.GetSpec().GetDefaultBranch())
		if err != nil {
			return fmt.Errorf("warm goldens of %s: %w: %w", target, controller.ErrWaiting, err)
		}
		stale = behind > set.warmMaxCommitsBehind
	}
	want := ready
	if stale {
		parent, _, err := s.boxImageGolden(ctx, c.project, target, &envv1alpha1.BoxSpec{Checkouts: []*envv1alpha1.Checkout{{Repository: c.name()}}})
		if err != nil || parent == "" {
			return err
		}
		w, err := s.cfg.Resolver.Warm(ctx, target, mirror, head)
		if err != nil {
			return err
		}
		inputs := &envv1alpha1.BoxWarmInputs{Repository: c.name(), Commit: head, ToolchainHash: w.ToolchainHash, LockfilesHash: w.LockfilesHash}
		hash := inputHash("box-warm", target, parent, c.name(), head, w.ToolchainHash, w.LockfilesHash, epoch)
		want, err = s.ensureGolden(ctx, c.project, goldenName("warm", hash), &envv1alpha1.GoldenSpec{
			Target: target, InputHash: hash, Parent: parent, Inputs: &envv1alpha1.GoldenSpec_BoxWarm{BoxWarm: inputs},
		}, buildSpec{Mirror: mirror, Branch: c.repo.GetSpec().GetDefaultBranch()}, map[string]string{warmLabel: c.name()})
		if err != nil {
			return err
		}
		if goldenPhase(want) != envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY {
			return nil // the older one serves until it is
		}
	}
	var older []*envv1alpha1.Golden
	for _, g := range c.goldens {
		if g.GetMetadata().GetName() != want.GetMetadata().GetName() {
			older = append(older, g)
		}
	}
	return s.dropWarm(ctx, older)
}
