package apiserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/devcontainer"
	"github.com/YukinaMochizuki/tower/internal/store"
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

// dropped reports whether the warm controller let the golden go while
// something still referenced it.
func dropped(g *envv1alpha1.Golden) bool {
	return g.GetMetadata().GetAnnotations()[warmDroppedAnnotation] != ""
}

// heldSince is when the repository took its place in the budget: its
// oldest warm golden not dropped, zero if it holds none.
func (c warmCandidate) heldSince() time.Time {
	var since time.Time
	for _, g := range c.goldens {
		if dropped(g) {
			continue
		}
		if t := g.GetMetadata().GetCreateTime().AsTime(); since.IsZero() || t.Before(since) {
			since = t
		}
	}
	return since
}

// score is what the repository ranks by: the seconds its boxes ran in its
// window.
func (c warmCandidate) score() float64 {
	return float64(c.repo.GetStatus().GetUse().GetBoxSeconds())
}

// newestReady is the repository's newest ready warm golden not dropped,
// nil if none.
func (c warmCandidate) newestReady() *envv1alpha1.Golden {
	var best *envv1alpha1.Golden
	for _, g := range c.goldens {
		if goldenPhase(g) == envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY && !dropped(g) &&
			(best == nil || g.GetMetadata().GetCreateTime().AsTime().After(best.GetMetadata().GetCreateTime().AsTime())) {
			best = g
		}
	}
	return best
}

// warmSize is what the repository's warm golden holds: its newest ready
// one's, or the last one tower saw, kept as an internal record so a
// restart forgets it not, or DefaultWarmEstimate before any was built. The
// last size seen keeps one that fell out of the budget out, rather than
// taken back in on an estimate and dropped again once built.
func (s *Server) warmSize(ctx context.Context, c warmCandidate) (uint64, error) {
	key := store.Key{Kind: warmSizeKind, Project: c.project, Name: c.name()}
	if g := c.newestReady(); g != nil {
		if b, err := quantity.Bytes(g.GetStatus().GetSize()); err == nil && b > 0 {
			return b, s.putRecord(ctx, key, warmSizeRecord{Bytes: b})
		}
	}
	var r warmSizeRecord
	if found, err := s.getRecord(ctx, key, &r); err != nil || found {
		return r.Bytes, err
	}
	return mustBytes(DefaultWarmEstimate), nil
}

// goldenSize is what a golden holds on its target, 0 if it does not say.
func goldenSize(g *envv1alpha1.Golden) uint64 {
	b, err := quantity.Bytes(g.GetStatus().GetSize())
	if err != nil {
		return 0
	}
	return b
}

// reconcileWarm keeps the target's warm goldens: the repositories of the
// projects placed on it, ranked by use, get one from the top down while
// their sizes fit the target's warm_budget; one that falls out loses its,
// one never used gets none, and one older than warm_max_age or more than
// warm_max_commits_behind behind its branch is rebuilt. A repository that
// holds one keeps its place for WarmMinHold, and after that against a
// challenger whose score beats its own by less than WarmChallengeMargin. A
// golden let go while something references it is kept, and counted
// against the budget, until nothing does.
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
	rankWarm(candidates, s.clock.Now(), set.warmMinHold)
	sizes := make([]uint64, len(candidates))
	for i, c := range candidates {
		if sizes[i], err = s.warmSize(ctx, c); err != nil {
			return err
		}
	}
	// What was let go but is still referenced holds disk all the same, and
	// so will a golden of a repository that falls out while something
	// references it: the budget is shared out once that is counted, so no
	// repository is let in on room such a golden still holds.
	var dropping uint64
	for _, c := range candidates {
		for _, g := range c.goldens {
			if dropped(g) {
				dropping += goldenSize(g)
			}
		}
	}
	kept := dropping
	var in []bool
	for range len(candidates) + 1 {
		in = s.shareWarmBudget(candidates, sizes, kept, set.warmBudget)
		more := dropping
		for i, c := range candidates {
			if in[i] {
				continue
			}
			n, err := s.referencedSize(ctx, c.goldens)
			if err != nil {
				return err
			}
			more += n
		}
		if more == kept {
			break
		}
		kept = more
	}
	var waiting error
	for i, c := range candidates {
		if in[i] {
			if err := s.keepWarm(ctx, t, set, c); err != nil && waiting == nil {
				waiting = err
			}
			continue
		}
		if err := s.dropWarm(ctx, c.goldens); err != nil {
			return err
		}
	}
	return waiting
}

// shareWarmBudget says which of the ranked candidates hold a warm golden:
// from the top down while their sizes fit in what budget leaves past
// held. The ranking is kept: a smaller repository further down does not
// take a place before a bigger one above it.
func (s *Server) shareWarmBudget(candidates []warmCandidate, sizes []uint64, held, budget uint64) []bool {
	in := make([]bool, len(candidates))
	fits := true
	for i, c := range candidates {
		if fits && c.used() && held+sizes[i] <= budget {
			held += sizes[i]
			in[i] = true
			continue
		}
		if c.used() {
			fits = false
		}
	}
	return in
}

// referencedSize is what the ready warm goldens not yet dropped among
// goldens hold that something references: what letting them go keeps.
func (s *Server) referencedSize(ctx context.Context, goldens []*envv1alpha1.Golden) (uint64, error) {
	var n uint64
	for _, g := range goldens {
		if dropped(g) || goldenPhase(g) != envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY {
			continue
		}
		referenced, err := s.goldenReferenced(ctx, g)
		if err != nil {
			return 0, err
		}
		if referenced {
			n += goldenSize(g)
		}
	}
	return n, nil
}

// rankWarm orders the candidates for the budget. A holder within its
// minimum hold ranks first, the longest held first, so one let in on an
// estimate that proves too big is the one that goes again; after it, a
// holder's score is raised by the margin a challenger must beat.
func rankWarm(candidates []warmCandidate, now time.Time, minHold time.Duration) {
	protected := func(c warmCandidate) bool {
		since := c.heldSince()
		return !since.IsZero() && now.Sub(since) < minHold
	}
	rank := func(c warmCandidate) float64 {
		if !c.heldSince().IsZero() {
			return c.score() * (1 + WarmChallengeMargin)
		}
		return c.score()
	}
	slices.SortStableFunc(candidates, func(a, b warmCandidate) int {
		ua, ub := a.repo.GetStatus().GetUse(), b.repo.GetStatus().GetUse()
		pa, pb := protected(a), protected(b)
		older := 0
		if pa && pb {
			older = a.heldSince().Compare(b.heldSince())
		}
		return cmp.Or(
			compareBool(pb, pa),
			older,
			cmp.Compare(rank(b), rank(a)),
			cmp.Compare(ub.GetBoxesOpened(), ua.GetBoxesOpened()),
			ub.GetLastUseTime().AsTime().Compare(ua.GetLastUseTime().AsTime()),
			cmp.Compare(a.project+"/"+a.name(), b.project+"/"+b.name()),
		)
	})
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
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

// dropWarm lets warm goldens go: one nothing references is deleted, its
// artifact with it; one something references is marked dropped and kept
// until nothing does; one still building is left until it is built.
func (s *Server) dropWarm(ctx context.Context, goldens []*envv1alpha1.Golden) error {
	for _, g := range goldens {
		meta := g.GetMetadata()
		if phase := goldenPhase(g); phase != envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY && phase != envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED {
			continue
		}
		referenced, err := s.goldenReferenced(ctx, g)
		if err != nil {
			return err
		}
		if referenced {
			if dropped(g) {
				continue
			}
			s.log.Info("letting a warm golden go once nothing references it", "project", meta.GetProject(), "name", meta.GetName(), "repository", meta.GetLabels()[warmLabel])
			if err := s.annotateGolden(ctx, g, warmDroppedAnnotation, "true"); err != nil {
				return err
			}
			continue
		}
		s.log.Info("dropping a warm golden", "project", meta.GetProject(), "name", meta.GetName(), "repository", meta.GetLabels()[warmLabel])
		rv := meta.GetResourceVersion()
		if _, err := s.backend(registry.Golden).Delete(s.system(ctx), resource.Key{Project: meta.GetProject(), Name: meta.GetName()}, &rv); err != nil && !isNotFound(err) {
			return err
		}
	}
	return nil
}

// annotateGolden sets one of tower's annotations on the golden as read,
// conditional on it not having changed.
func (s *Server) annotateGolden(ctx context.Context, g *envv1alpha1.Golden, k, v string) error {
	g = proto.Clone(g).(*envv1alpha1.Golden)
	if g.Metadata.Annotations == nil {
		g.Metadata.Annotations = map[string]string{}
	}
	g.Metadata.Annotations[k] = v
	_, err := s.backend(registry.Golden).Apply(s.system(ctx), g)
	return err
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
		// Counted between the golden's commit and the head this reconcile
		// builds, not the mirror's own head, which a fetch may have moved
		// past the repository's status.
		branch := c.repo.GetSpec().GetDefaultBranch()
		fromReady, err := p.CommitsBehind(ctx, mirror, ready.GetSpec().GetBoxWarm().GetCommit(), branch)
		if err != nil {
			return fmt.Errorf("warm goldens of %s: %w: %w", target, controller.ErrWaiting, err)
		}
		fromHead, err := p.CommitsBehind(ctx, mirror, head, branch)
		if err != nil {
			return fmt.Errorf("warm goldens of %s: %w: %w", target, controller.ErrWaiting, err)
		}
		stale = fromReady-fromHead > set.warmMaxCommitsBehind
	}
	want := ready
	if stale {
		p, err := s.providerFor(target)
		if err != nil {
			return fmt.Errorf("warm goldens of %s: %w: %w", target, controller.ErrWaiting, err)
		}
		parent, _, err := s.boxImageGolden(ctx, p, c.project, target, &envv1alpha1.BoxSpec{Checkouts: []*envv1alpha1.Checkout{{Repository: c.name()}}})
		if errors.Is(err, devcontainer.ErrUnresolvable) {
			// Its boxes fail, saying why; it has no warm golden.
			return nil
		}
		if err != nil || parent == "" {
			return err
		}
		w, err := s.cfg.Resolver.Warm(ctx, p, mirror, head)
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
