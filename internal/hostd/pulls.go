package hostd

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// pulls pulls images off the host lock, one pull per reference however
// many instances want it, so a slow registry holds up no other instance's
// reconcile, as hangar's node agent's puller does (hangar:
// internal/node/images.go). A reconcile asks for its image; the pull runs
// apart, and when it ends every instance that asked is reconciled again.
type pulls struct {
	images driver.Images
	clock  clockwork.Clock
	// enqueue reconciles an instance again.
	enqueue func(controller.Key)

	mu   sync.Mutex
	ctx  context.Context
	jobs map[string]*pullJob
}

type pullJob struct {
	running, ready bool
	err            error
	failures       int
	retryAt        time.Time
	// waiters are the instances to reconcile once it ends.
	waiters map[controller.Key]bool
}

// maxPullRetry bounds how long a failed pull waits to be tried again.
const maxPullRetry = time.Minute

func newPulls(images driver.Images, clock clockwork.Clock, enqueue func(controller.Key)) *pulls {
	return &pulls{images: images, clock: clock, enqueue: enqueue, ctx: context.Background(), jobs: map[string]*pullJob{}}
}

// start runs pulls under ctx from now on.
func (p *pulls) start(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ctx = ctx
}

// get says whether ref is on the host for the instance of key: true once
// a pull of it has ended well; false while one runs, starting one if none
// does, key being reconciled again when it ends; or the error the last
// pull ended with, until it is tried again.
func (p *pulls) get(ref string, key controller.Key) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	j := p.jobs[ref]
	if j == nil {
		j = &pullJob{waiters: map[controller.Key]bool{}}
		p.jobs[ref] = j
	}
	switch {
	case j.ready:
		return true, nil
	case j.running:
		j.waiters[key] = true
		return false, nil
	case j.err != nil && p.clock.Now().Before(j.retryAt):
		return false, j.err
	}
	j.running, j.err = true, nil
	j.waiters[key] = true
	go p.run(p.ctx, ref, j)
	return false, nil
}

// forget drops what is known of ref, as when an instance finds it is not
// on the host after all.
func (p *pulls) forget(ref string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if j := p.jobs[ref]; j != nil && !j.running {
		delete(p.jobs, ref)
	}
}

func (p *pulls) run(ctx context.Context, ref string, j *pullJob) {
	err := p.images.Pull(ctx, ref)
	p.mu.Lock()
	j.running = false
	if err == nil {
		j.ready, j.failures = true, 0
	} else {
		j.err = err
		j.retryAt = p.clock.Now().Add(min(time.Second<<min(j.failures, 6), maxPullRetry))
		j.failures++
	}
	waiters := j.waiters
	j.waiters = map[controller.Key]bool{}
	p.mu.Unlock()
	if errors.Is(err, context.Canceled) {
		return
	}
	for k := range waiters {
		p.enqueue(k)
	}
}
