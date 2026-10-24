package hostd

import (
	"errors"
	"fmt"
	"sync"

	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/tunnel"
)

// Calls to hostd hosts open at once are capped per caller, per project and
// per target, as hangar caps its calls to nodes (hangar:
// docs/decisions/node-tunnel.md): no caller or project takes a target's
// streams from the others, and a target's cap stays below what its tunnel
// takes, leaving room for tower's own calls, which are not counted. A call
// over a cap fails at once; none waits.
const (
	DefaultMaxCallsPerCaller  = 64
	DefaultMaxCallsPerProject = 64
	DefaultMaxCallsPerTarget  = tunnel.MaxStreams - 100
)

// ErrTooManyCalls means a call was refused for a cap on the calls open.
var ErrTooManyCalls = errors.New("too many calls open to hostd hosts")

// Calls counts the calls open to hostd hosts, for every target's Client
// at once. Its zero value takes the default caps.
type Calls struct {
	// MaxPerCaller, MaxPerProject and MaxPerTarget zero are the defaults.
	MaxPerCaller, MaxPerProject, MaxPerTarget int

	mu         sync.Mutex
	perCaller  map[auth.Principal]int
	perProject map[string]int
	perTarget  map[string]int
}

func orDefault(n, def int) int {
	if n == 0 {
		return def
	}
	return n
}

// take takes a slot for a call by p in project (empty for a call of no
// project) to target, and returns what gives it back. The system's calls
// take none.
func (c *Calls) take(p auth.Principal, project, target string) (func(), error) {
	if c == nil || p.Type == auth.System {
		return func() {}, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.perCaller == nil {
		c.perCaller, c.perProject, c.perTarget = map[auth.Principal]int{}, map[string]int{}, map[string]int{}
	}
	if n := orDefault(c.MaxPerCaller, DefaultMaxCallsPerCaller); c.perCaller[p] >= n {
		return nil, fmt.Errorf("%w: %s has %d open, the most one may; end one first", ErrTooManyCalls, p, n)
	}
	if n := orDefault(c.MaxPerProject, DefaultMaxCallsPerProject); project != "" && c.perProject[project] >= n {
		return nil, fmt.Errorf("%w: project %q has %d open, the most it may; end one first", ErrTooManyCalls, project, n)
	}
	if n := orDefault(c.MaxPerTarget, DefaultMaxCallsPerTarget); c.perTarget[target] >= n {
		return nil, fmt.Errorf("%w: target %s has %d open, the most it takes; try again later", ErrTooManyCalls, target, n)
	}
	c.perCaller[p]++
	if project != "" {
		c.perProject[project]++
	}
	c.perTarget[target]++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.perCaller[p]--; c.perCaller[p] == 0 {
				delete(c.perCaller, p)
			}
			if project != "" {
				if c.perProject[project]--; c.perProject[project] == 0 {
					delete(c.perProject, project)
				}
			}
			if c.perTarget[target]--; c.perTarget[target] == 0 {
				delete(c.perTarget, target)
			}
		})
	}, nil
}
