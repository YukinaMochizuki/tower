package apiserver

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/authz"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hostd"
)

// TargetReachable says whether tower can call a target's provider. For a
// hostd target, tower owns it: it follows the tunnel the host opens.
const TargetReachable = "Reachable"

// openTunnel takes a hostd host's tunnel (internal/tunnel). Only a
// ROLE_HOSTD token opens one, and the tunnel is its token's target, never
// anything the request says. The tunnel stands while the token does: it
// is rechecked as a stream is, and dropped once the token is gone, rotated
// away, or names another target.
func (s *Server) openTunnel(chain auth.Chain) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The tunnel outlives the request's own context, which ends once
		// its connection is taken over; Run's end drops it.
		ctx, release, err := chain.Hold(context.WithoutCancel(r.Context()), r.Header, s.clock)
		if err != nil {
			http.Error(w, "unauthenticated: "+err.Error(), http.StatusUnauthorized)
			return
		}
		defer release()
		c, err := s.begin(ctx)
		if err != nil {
			http.Error(w, messageOf(err), http.StatusServiceUnavailable)
			return
		}
		if !authz.MayOpenTunnel(c) {
			s.log.Warn("refused a tunnel", "principal", c.String())
			http.Error(w, c.String()+" may not open a tunnel: only a ROLE_HOSTD token does, for its target", http.StatusForbidden)
			return
		}
		target := c.Target
		auth.Recheck(ctx, func(ctx context.Context) error {
			c, err := s.begin(ctx)
			if err != nil {
				return err
			}
			if !authz.MayOpenTunnel(c) || c.Target != target {
				return denied("%s no longer opens target %s's tunnel", c, target)
			}
			return nil
		})
		t := s.tunnels.Accept(w, r, target)
		if t == nil {
			return
		}
		select {
		case <-t.Done():
		case <-ctx.Done():
			s.log.Warn("dropped a tunnel whose token no longer stands", "target", target, "principal", c.String())
			t.Close()
		}
	}
}

// reachability is what tower knows of each hostd target's tunnel: whether
// it is open, and when it last dropped.
type reachability struct {
	mu      sync.Mutex
	dropped map[string]time.Time
}

// tunnelChanged hears of a target's tunnel opening or dropping.
func (s *Server) tunnelChanged(target string) {
	_, err := s.tunnels.Node(target)
	s.reach.mu.Lock()
	if s.reach.dropped == nil {
		s.reach.dropped = map[string]time.Time{}
	}
	if err != nil {
		s.reach.dropped[target] = s.clock.Now()
	}
	s.reach.mu.Unlock()
	if s.targetCtrl != nil {
		s.targetCtrl.Enqueue(controller.Key{Name: target})
	}
}

// reachable sets a hostd target's Reachable in st by its tunnel: open,
// dropped, or never opened since tower started; and last_error, while it
// is not open.
func (s *Server) reachable(target string, st *targetsv1alpha1.TargetStatus) {
	c := &metav1.Condition{Type: TargetReachable, Status: metav1.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Connected",
		Message: "hostd's tunnel is open"}
	if _, err := s.tunnels.Node(target); err != nil {
		s.reach.mu.Lock()
		at, dropped := s.reach.dropped[target]
		s.reach.mu.Unlock()
		c.Status, c.Reason = metav1.ConditionStatus_CONDITION_STATUS_FALSE, "NeverConnected"
		c.Message = "hostd has not opened its tunnel since tower started"
		if dropped {
			c.Reason, c.Message = "Dropped", "hostd's tunnel dropped at "+at.UTC().Format(time.RFC3339)+" and has not opened again"
		}
	}
	// Connected, last_error is the jobs' (lastJobError).
	if c.Status != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		st.LastError = c.Message
	}
	st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), c, s.clock.Now())
}

// defaultProvider is tower's own provider of a target: a hostd target's
// host, over its tunnel; a hangar target's, which tower runs itself
// (hangar.go).
func (s *Server) defaultProvider(target string) (provider.Provider, error) {
	t, live, err := s.liveTarget(context.Background(), target)
	if err != nil {
		return nil, err
	}
	if !live {
		return nil, fmt.Errorf("target %q is gone", target)
	}
	if t.GetSpec().GetHangar() != nil {
		p, err := s.hangarProvider(target)
		if err != nil {
			return nil, err
		}
		return p, nil
	}
	return &hostd.Client{Target: target, Tunnels: s.tunnels, Calls: s.hostdCalls}, nil
}
