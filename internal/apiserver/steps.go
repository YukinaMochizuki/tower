package apiserver

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// An instance step is what tower runs in an instance on its way to its box
// or machine being ready, as a job: a box's secret files and checkouts, a
// machine's configure phase and ready check. A step that fails fails as a
// golden does (goldenFailed): its box or machine is FAILED, saying why and
// when the step runs again, after a wait of StepRetryFirst, doubled after
// each failure in a row, up to StepRetryMax. The failures are counted per
// run of the instance and generation of the box or machine, in an internal
// record a restart keeps: a new run, a change of its spec or a success
// starts afresh. A target that did not answer is no failure: the step
// waits for it.
const (
	StepRetryFirst = GoldenRetryFirst
	StepRetryMax   = GoldenRetryMax
)

// stepBackoff is how a step failed, and when it runs again.
type stepBackoff struct {
	// Run is the instance's run the failures were in (its uid and runOf),
	// Generation its owner's generation.
	Run        string    `json:"run"`
	Generation int64     `json:"generation"`
	Failures   int       `json:"failures"`
	RetryAt    time.Time `json:"retryAt"`
	Reason     string    `json:"reason"`
}

// stepRef names one step of one box or machine, in one run of its
// instance at one generation.
type stepRef struct {
	key        store.Key
	run        string
	generation int64
}

func newStepRef(kind, project, name, step, run string, generation int64) stepRef {
	return stepRef{key: store.Key{Kind: stepBackoffKind, Project: project, Name: kind + "/" + name + "/" + step}, run: run, generation: generation}
}

// message is the failure as its owner says it: why, and when it runs
// again.
func (b stepBackoff) message() string {
	return b.Reason + "; tried again at " + b.RetryAt.UTC().Format(time.RFC3339)
}

// stepWaiting reports the step's failure and whether it waits on it yet:
// one of another run or generation is none.
func (s *Server) stepWaiting(ctx context.Context, ref stepRef) (stepBackoff, bool, error) {
	var b stepBackoff
	ok, err := s.getRecord(ctx, ref.key, &b)
	if err != nil || !ok || b.Run != ref.run || b.Generation != ref.generation {
		return stepBackoff{}, false, err
	}
	return b, s.clock.Now().Before(b.RetryAt), nil
}

// stepFailed counts a failure of the step and says when it runs again.
func (s *Server) stepFailed(ctx context.Context, ref stepRef, reason string) (stepBackoff, error) {
	var b stepBackoff
	if _, err := s.getRecord(ctx, ref.key, &b); err != nil {
		return stepBackoff{}, err
	}
	if b.Run != ref.run || b.Generation != ref.generation {
		b = stepBackoff{Run: ref.run, Generation: ref.generation}
	}
	wait := StepRetryFirst
	for range b.Failures {
		if wait *= 2; wait >= StepRetryMax {
			wait = StepRetryMax
			break
		}
	}
	b.Failures++
	b.RetryAt, b.Reason = s.clock.Now().Add(wait), reason
	s.log.Warn("an instance step failed", "project", ref.key.Project, "step", ref.key.Name,
		"failures", b.Failures, "retry_at", b.RetryAt, "reason", reason)
	return b, s.putRecord(ctx, ref.key, b)
}

// stepPassed forgets the step's failures.
func (s *Server) stepPassed(ctx context.Context, ref stepRef) error {
	return s.deleteRecord(ctx, ref.key)
}

// pruneStepBackoffs deletes the step failures of boxes and machines that
// are gone.
func (s *Server) pruneStepBackoffs(ctx context.Context) error {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: stepBackoffKind})
	if err != nil {
		return err
	}
	for _, o := range objs {
		kind, rest, _ := strings.Cut(o.Name, "/")
		name, _, _ := strings.Cut(rest, "/")
		owner := registry.Box
		if kind == "machine" {
			owner = registry.Machine
		}
		_, err := s.store.Get(ctx, store.Key{Kind: owner.Kind, Project: o.Project, Name: name})
		if !errors.Is(err, store.ErrNotFound) {
			if err != nil {
				return err
			}
			continue
		}
		if _, _, err := s.store.Delete(ctx, o.Key, o.Revision); err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// gate is what the reconcile of an instance step does next.
type gate struct {
	// Running says its job runs.
	Running bool
	// Waiting is a target that did not answer, to wait on.
	Waiting error
	// Failed, when set, is why it failed and when it runs again: its
	// owner is FAILED until then.
	Failed string
}

// stepGate reads the step's job and its failures: a job that failed is a
// failure counted; one that waits out its backoff is still FAILED; one
// whose backoff passed, or that never failed, is the caller's to start.
func (s *Server) stepGate(ctx context.Context, job string, ref stepRef) (gate, error) {
	jerr, running := s.jobs.lastError(job)
	if running {
		return gate{Running: true}, nil
	}
	if jerr != nil {
		s.jobs.forget(job)
		if transient(jerr) {
			return gate{Waiting: jerr}, nil
		}
		b, err := s.stepFailed(ctx, ref, jerr.Error())
		if err != nil {
			return gate{}, err
		}
		return gate{Failed: b.message()}, nil
	}
	b, waiting, err := s.stepWaiting(ctx, ref)
	if err != nil || !waiting {
		return gate{}, err
	}
	return gate{Failed: b.message()}, nil
}
