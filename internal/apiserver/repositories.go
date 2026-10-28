package apiserver

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// RepositoryReachable says the last fetch of a repository succeeded.
const RepositoryReachable = "Reachable"

// mirrorName is a repository's mirror on its target.
func mirrorName(project, repo string) string { return project + "/" + repo }

// repositoryOfWarm maps a warm golden to the repository it is held for.
func (s *Server) repositoryOfWarm(_ context.Context, o store.Object) []controller.Key {
	var g envv1alpha1.Golden
	if proto.Unmarshal(o.Data, &g) != nil {
		return nil
	}
	if repo := g.GetSpec().GetBoxWarm().GetRepository(); repo != "" {
		return []controller.Key{{Project: o.Project, Name: repo}}
	}
	return nil
}

// reconcileRepository keeps the repository's mirror on its project's
// target fresh, fetching it every MirrorFetchEvery and when a box asks
// (fetchNow), and writes how much it is used.
func (s *Server) reconcileRepository(ctx context.Context, key controller.Key) error {
	repo, ok, err := getTyped[*envv1alpha1.Repository](ctx, s, registry.Repository, resource.Key{Project: key.Project, Name: key.Name})
	if err != nil || !ok || repo.GetMetadata().GetDeleteTime() != nil {
		return err
	}
	st := proto.Clone(repo.GetStatus()).(*envv1alpha1.RepositoryStatus)
	if st == nil {
		st = &envv1alpha1.RepositoryStatus{}
	}
	st.ObservedGeneration = repo.GetMetadata().GetGeneration()
	target, _, err := s.placedTarget(ctx, key.Project)
	if err != nil {
		return err
	}
	var waitErr error
	if target != nil {
		set := settingsOf(target)
		if st.Use, err = s.repositoryUse(ctx, key, set.warmWindow); err != nil {
			return err
		}
		if st.Use.Warm, err = s.holdsWarm(ctx, key); err != nil {
			return err
		}
		waitErr = s.fetchMirror(ctx, key, repo, target.GetMetadata().GetName(), st)
	}
	if err := s.updateStatus(ctx, registry.Repository, repo, st); err != nil {
		return err
	}
	return waitErr
}

// fetchMirror starts a fetch of the repository's mirror on target when it
// has none or it is stale, and says in st what the last fetch came to.
func (s *Server) fetchMirror(ctx context.Context, key controller.Key, repo *envv1alpha1.Repository, target string, st *envv1alpha1.RepositoryStatus) error {
	job := "fetch/" + key.String() + "/" + target
	jerr, running := s.jobs.lastError(job)
	if running {
		return nil
	}
	reachable := conditions.Find(st.GetConditions(), RepositoryReachable)
	if jerr != nil {
		s.jobs.forget(job)
		st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), &metav1.Condition{
			Type: RepositoryReachable, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "FetchFailed", Message: jerr.Error(),
		}, s.clock.Now())
		return fmt.Errorf("fetching repository %s: %w: %w", key, controller.ErrWaiting, jerr)
	}
	i := slices.IndexFunc(st.GetMirrors(), func(m *envv1alpha1.Mirror) bool { return m.GetTarget() == target })
	fresh := i >= 0 && s.clock.Since(st.GetMirrors()[i].GetFetchTime().AsTime()) < MirrorFetchEvery &&
		!s.fetchAsked(key) && reachable != nil
	if fresh {
		return nil
	}
	p, err := s.providerFor(target)
	if err != nil {
		return fmt.Errorf("repository %s: %w: %w", key, controller.ErrWaiting, err)
	}
	s.fetchDone(key)
	s.startJob(target, job, s.repositoryCtrl, key, func(ctx context.Context) error {
		n := s.fetches.started(key)
		defer func() {
			s.fetches.finished(key, n)
			if s.warmCtrl != nil {
				s.warmCtrl.Enqueue(controller.Key{Name: target})
			}
		}()
		token := ""
		if c := repo.GetSpec().GetCredentialSecret(); c != "" {
			data, err := s.OpenSecret(s.system(ctx), resource.Key{Project: key.Project, Name: c})
			if err != nil {
				return err
			}
			token = string(data["token"])
		}
		head, err := p.Fetch(ctx, provider.FetchRequest{
			Mirror: mirrorName(key.Project, key.Name), URL: repo.GetSpec().GetUrl(), Token: token, Branch: repo.GetSpec().GetDefaultBranch(),
		})
		if err != nil {
			return err
		}
		return s.recordFetch(ctx, key, target, head)
	})
	return nil
}

// recordFetch writes a fetch's head into the repository's status.
func (s *Server) recordFetch(ctx context.Context, key controller.Key, target, head string) error {
	for range maxRetries {
		repo, ok, err := getTyped[*envv1alpha1.Repository](ctx, s, registry.Repository, resource.Key{Project: key.Project, Name: key.Name})
		if err != nil || !ok {
			return err
		}
		st := proto.Clone(repo.GetStatus()).(*envv1alpha1.RepositoryStatus)
		if st == nil {
			st = &envv1alpha1.RepositoryStatus{}
		}
		mirror := &envv1alpha1.Mirror{Target: target, Head: head, FetchTime: timestamppb.New(s.clock.Now())}
		if i := slices.IndexFunc(st.GetMirrors(), func(m *envv1alpha1.Mirror) bool { return m.GetTarget() == target }); i >= 0 {
			st.Mirrors[i] = mirror
		} else {
			st.Mirrors = append(st.Mirrors, mirror)
		}
		st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), &metav1.Condition{
			Type: RepositoryReachable, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Fetched",
		}, s.clock.Now())
		err = s.updateStatus(ctx, registry.Repository, repo, st)
		if connect.CodeOf(err) != connect.CodeAborted {
			return err
		}
	}
	return fmt.Errorf("too many writers of repository %s", key)
}

// mirrorHead is the commit the repository's mirror on target was last
// fetched at, "" if it has not been.
func mirrorHead(repo *envv1alpha1.Repository, target string) string {
	for _, m := range repo.GetStatus().GetMirrors() {
		if m.GetTarget() == target {
			return m.GetHead()
		}
	}
	return ""
}

// fetchAsked reports whether a box asked for the repository's mirror to be
// fetched; fetchNow asks.
func (s *Server) fetchAsked(key controller.Key) bool {
	s.uses.mu.Lock()
	defer s.uses.mu.Unlock()
	return s.uses.fetch[key]
}

func (s *Server) fetchNow(key controller.Key) {
	s.uses.mu.Lock()
	if s.uses.fetch == nil {
		s.uses.fetch = map[controller.Key]bool{}
	}
	s.uses.fetch[key] = true
	s.uses.mu.Unlock()
	if s.repositoryCtrl != nil {
		s.repositoryCtrl.Enqueue(key)
	}
}

func (s *Server) fetchDone(key controller.Key) {
	s.uses.mu.Lock()
	defer s.uses.mu.Unlock()
	delete(s.uses.fetch, key)
}

// fetchSeq numbers each repository's fetches as they start, so a fetch
// started after some moment is told apart from one already under way
// then, under a clock that need not move.
type fetchSeq struct {
	mu sync.Mutex
	// last is the number of the last fetch started, done of the last one
	// finished, however it ended.
	last, done map[controller.Key]int
	// asked is, per repository whose warm golden waits on a fetch to be
	// built, last when it asked.
	asked map[controller.Key]int
}

func (f *fetchSeq) init() {
	if f.last == nil {
		f.last, f.done, f.asked = map[controller.Key]int{}, map[controller.Key]int{}, map[controller.Key]int{}
	}
}

func (f *fetchSeq) started(key controller.Key) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	f.last[key]++
	return f.last[key]
}

func (f *fetchSeq) finished(key controller.Key, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done[key] = max(f.done[key], n)
}

// fetchedToBuild reports whether the repository's mirror was fetched since
// a warm golden of it was first wanted, asking for a fetch the first time,
// so the golden is built from the branch as it is, not as the last
// periodic fetch saw it. A fetch that failed counts: the golden is built
// from what the mirror has.
func (s *Server) fetchedToBuild(key controller.Key) bool {
	f := &s.fetches
	f.mu.Lock()
	f.init()
	at, asked := f.asked[key]
	if !asked {
		f.asked[key] = f.last[key]
	}
	fetched := asked && f.done[key] > at
	f.mu.Unlock()
	if !asked {
		s.fetchNow(key)
	}
	return fetched
}

// builtFromFetch forgets fetchedToBuild's ask once no warm golden of the
// repository waits to be built.
func (s *Server) builtFromFetch(key controller.Key) {
	s.fetches.mu.Lock()
	defer s.fetches.mu.Unlock()
	delete(s.fetches.asked, key)
}

// holdsWarm reports whether the repository holds a ready warm golden.
func (s *Server) holdsWarm(ctx context.Context, key controller.Key) (bool, error) {
	goldens, err := listTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, key.Project)
	if err != nil {
		return false, err
	}
	for _, g := range goldens {
		if g.GetMetadata().GetLabels()[warmLabel] == key.Name && goldenPhase(g) == envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY && g.GetMetadata().GetDeleteTime() == nil {
			return true, nil
		}
	}
	return false, nil
}

// repositoryUse is the repository's use over window, from the use records
// of the boxes that checked it out first.
func (s *Server) repositoryUse(ctx context.Context, key controller.Key, window time.Duration) (*envv1alpha1.RepositoryUse, error) {
	records, err := s.useRecords(ctx, key.Project)
	if err != nil {
		return nil, err
	}
	use := &envv1alpha1.RepositoryUse{}
	from := s.clock.Now().Add(-window)
	var last time.Time
	for _, r := range records {
		if r.Repository != key.Name || r.LastUse.Before(from) {
			continue
		}
		if !r.Opened.Before(from) {
			use.BoxesOpened++
		}
		use.BoxSeconds += r.Seconds
		if r.LastUse.After(last) {
			last = r.LastUse
		}
	}
	if !last.IsZero() {
		use.LastUseTime = timestamppb.New(last)
	}
	return use, nil
}
