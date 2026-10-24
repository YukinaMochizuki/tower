package hostd

import (
	"errors"
	"fmt"
	"sync"

	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/tunnel"
)

// Calls to hostd hosts open at once are capped per environment, by its
// instance, so no environment takes a target's streams from the others,
// and per target, below what its tunnel takes, leaving room for tower's
// own calls, which are not counted (as hangar caps its calls to nodes:
// hangar: docs/decisions/node-tunnel.md). Not per caller: tower's callers
// are a few programs, workstream above all, which calls for every
// environment. A call over a cap fails at once; none waits.
const (
	DefaultMaxCallsPerInstance = 64
	DefaultMaxCallsPerTarget   = tunnel.MaxStreams - 100
)

// ErrTooManyCalls means a call was refused for a cap on the calls open.
var ErrTooManyCalls = errors.New("too many calls open to hostd hosts")

// Calls counts the calls open to hostd hosts, for every target's Client
// at once. Its zero value takes the default caps.
type Calls struct {
	// MaxPerInstance and MaxPerTarget zero are the defaults.
	MaxPerInstance, MaxPerTarget int

	mu          sync.Mutex
	perInstance map[string]int
	perTarget   map[string]int
}

func orDefault(n, def int) int {
	if n == 0 {
		return def
	}
	return n
}

// take takes a slot for a call by p about in (zero for a call about no
// instance) to target, and returns what gives it back. The system's calls
// take none.
func (c *Calls) take(p auth.Principal, in provider.Instance, target string) (func(), error) {
	if c == nil || p.Type == auth.System {
		return func() {}, nil
	}
	instance := ""
	if in.Name != "" {
		instance = target + "/" + in.Project + "/" + in.Name
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.perInstance == nil {
		c.perInstance, c.perTarget = map[string]int{}, map[string]int{}
	}
	if n := orDefault(c.MaxPerInstance, DefaultMaxCallsPerInstance); instance != "" && c.perInstance[instance] >= n {
		return nil, fmt.Errorf("%w: instance %s/%s has %d open, the most one may; end one first", ErrTooManyCalls, in.Project, in.Name, n)
	}
	if n := orDefault(c.MaxPerTarget, DefaultMaxCallsPerTarget); c.perTarget[target] >= n {
		return nil, fmt.Errorf("%w: target %s has %d open, the most it takes; try again later", ErrTooManyCalls, target, n)
	}
	if instance != "" {
		c.perInstance[instance]++
	}
	c.perTarget[target]++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if instance != "" {
				if c.perInstance[instance]--; c.perInstance[instance] == 0 {
					delete(c.perInstance, instance)
				}
			}
			if c.perTarget[target]--; c.perTarget[target] == 0 {
				delete(c.perTarget, target)
			}
		})
	}, nil
}
