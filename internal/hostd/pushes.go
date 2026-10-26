package hostd

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// pushes pushes the images built on the host to the registry, apart from
// the build that made them, one at a time, oldest first: a build ends, and
// what is made of its image is made, as soon as the image is on the host
// (docs/decisions/images.md). A push that fails is tried again, after a
// wait that doubles up to retryMost, until it is done; what is left to
// push is the driver's to keep (Images.Unpushed), so a hostd restarted
// takes it up again. Nothing here waits on a push: what is pushed and what
// is not is reported (Target condition ImagesPushed), and policy, tower's,
// decides what waits on it.
type pushes struct {
	images driver.Images
	clock  clockwork.Clock
	log    *slog.Logger
	// retryFirst and retryMost bound the wait after a failed push.
	retryFirst, retryMost time.Duration

	mu sync.Mutex
	// queue is what is left to push, oldest first.
	queue []string
	jobs  map[string]*pushJob
	// pushing is the push running, "" for none.
	pushing string
	// wake wakes run when there is more to push.
	wake chan struct{}
}

type pushJob struct {
	failures int
	err      error
	retryAt  time.Time
	// again says it was built again while it was pushed, which may have
	// held it again after the push let it go: it is pushed once more.
	again bool
}

// Defaults of the waits after a failed push.
const (
	DefaultPushRetryFirst = 5 * time.Second
	DefaultPushRetryMost  = 5 * time.Minute
)

func newPushes(images driver.Images, clock clockwork.Clock, log *slog.Logger, first, most time.Duration) *pushes {
	return &pushes{
		images: images, clock: clock, log: log, retryFirst: first, retryMost: most,
		jobs: map[string]*pushJob{}, wake: make(chan struct{}, 1),
	}
}

// add asks for ref, built on the host, to be pushed.
func (p *pushes) add(ref string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if j, ok := p.jobs[ref]; ok {
		j.again = j.again || p.pushing == ref
		return
	}
	p.jobs[ref] = &pushJob{}
	p.queue = append(p.queue, ref)
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// run pushes until ctx is done: first what the driver held to be pushed
// when hostd started, then whatever is added.
func (p *pushes) run(ctx context.Context) {
	for ctx.Err() == nil {
		list, err := p.images.Unpushed(ctx)
		if err == nil {
			for _, ref := range list {
				p.add(ref)
			}
			break
		}
		p.log.Error("listing the images left to push failed", "error", err)
		select {
		case <-ctx.Done():
		case <-p.clock.After(p.retryFirst):
		}
	}
	for ctx.Err() == nil {
		ref, wait := p.next()
		if ref == "" {
			var after <-chan time.Time
			if wait > 0 {
				after = p.clock.After(wait)
			}
			select {
			case <-ctx.Done():
			case <-p.wake:
			case <-after:
			}
			continue
		}
		start := p.clock.Now()
		err := p.images.Push(ctx, ref)
		p.done(ctx, ref, err, p.clock.Since(start))
	}
}

// next is the oldest ref due to be pushed, marked as pushing; else "" and
// how long until one is due, zero for none.
func (p *pushes) next() (string, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock.Now()
	var wait time.Duration
	for _, ref := range p.queue {
		at := p.jobs[ref].retryAt
		if !now.Before(at) {
			p.pushing = ref
			return ref, 0
		}
		if d := at.Sub(now); wait == 0 || d < wait {
			wait = d
		}
	}
	return "", wait
}

func (p *pushes) done(ctx context.Context, ref string, err error, took time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushing = ""
	j := p.jobs[ref]
	if err == nil && j.again {
		p.jobs[ref] = &pushJob{}
		return
	}
	if err == nil {
		delete(p.jobs, ref)
		p.queue = slices.DeleteFunc(p.queue, func(r string) bool { return r == ref })
		p.log.Info("pushed an image built here", "image", ref, "took", took.Round(time.Millisecond))
		return
	}
	if ctx.Err() != nil {
		// hostd is stopping; the image is still held, and a hostd started
		// again pushes it.
		return
	}
	wait := p.retryFirst
	for range j.failures {
		if wait *= 2; wait >= p.retryMost {
			wait = p.retryMost
			break
		}
	}
	j.failures++
	j.err, j.retryAt = err, p.clock.Now().Add(wait)
	p.log.Warn("pushing an image built here failed; it is tried again", "image", ref, "failures", j.failures, "retry_in", wait, "error", err)
}

// pushState is what is left to push, as reported.
type pushState struct {
	// left is what is left to push, oldest first; pushing the push
	// running, "" for none.
	left    []string
	pushing string
	// failed is the image whose last push failed most recently, and why.
	failed   string
	failures int
	err      error
	retryAt  time.Time
}

func (p *pushes) state() pushState {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := pushState{left: slices.Clone(p.queue), pushing: p.pushing}
	for _, ref := range p.queue {
		j := p.jobs[ref]
		if j.err != nil && (s.failed == "" || j.retryAt.After(s.retryAt)) {
			s.failed, s.failures, s.err, s.retryAt = ref, j.failures, j.err, j.retryAt
		}
	}
	return s
}

// condition is the Target condition ImagesPushed the state says: true
// once every image built on the host is in the registry.
func (s pushState) condition() (bool, string, string) {
	if len(s.left) == 0 {
		return true, "Pushed", "every image built on the host is in the registry"
	}
	n := fmt.Sprintf("%d image", len(s.left))
	if len(s.left) > 1 {
		n += "s"
	}
	msg := n + " built on the host not in the registry yet: " + strings.Join(s.left, ", ")
	if s.err == nil {
		if s.pushing != "" {
			msg += "; pushing " + s.pushing
		}
		return false, "Pushing", msg
	}
	msg += fmt.Sprintf("; pushing %s failed %d time", s.failed, s.failures)
	if s.failures > 1 {
		msg += "s"
	}
	msg += ", tried again at " + s.retryAt.UTC().Format(time.RFC3339) + ": " + s.err.Error()
	return false, "PushFailed", msg
}
