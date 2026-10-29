package hangar

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
	"github.com/YukinaMochizuki/tower/internal/guest/guestops"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/system"
)

// Locked reports whether anyone holds the target's lock, as a reconcile,
// makeSystem or Teardown does.
func (t *Target) Locked() bool {
	if t.mu.TryLock() {
		t.mu.Unlock()
		return false
	}
	return true
}

// ApplyRefused is what an apply hangar refused with err makes of an
// instance: its reason, and whether it waits.
func ApplyRefused(err error) (reason string, waits bool) {
	o, _ := (&Target{}).applyFailed(err)
	return o.reason, o.wait
}

// SetDial has f open its TCP connections with dial, before it is used.
func (f *Front) SetDial(dial func(ctx context.Context, network, addr string) (net.Conn, error)) {
	f.dialNet = dial
}

// ProviderError is what an error of hangar's is to tower.
func ProviderError(err error) error { return providerError(err) }

// Converge converges in once, as a reconcile of it does.
func (t *Target) Converge(ctx context.Context, in *envv1alpha1.Instance) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, err := t.converge(ctx, in)
	return err
}

// VMConnections is how many live connections the provider holds to its
// VMs.
func (t *Target) VMConnections() int {
	t.vms.mu.Lock()
	defer t.vms.mu.Unlock()
	n := 0
	for _, c := range t.vms.conns {
		if !c.ended() {
			n++
		}
	}
	return n
}

// CloseVMConnections closes every connection to the target's VMs, as its
// Run returning does.
func (t *Target) CloseVMConnections() { t.vms.closeAll() }

// PresentElsewhere grants a checkout of mirror to the box of owner, and
// has the box from present it over its own network's endpoint, its probe
// naming probeAs's network: how the server answers a credential that
// comes from where it is not for.
func (t *Target) PresentElsewhere(ctx context.Context, owner, from, probeAs provider.Instance, mirror string) (guestbin.HTTPAnswer, error) {
	o, err := t.sysOps()
	if err != nil {
		return guestbin.HTTPAnswer{}, err
	}
	routes := map[provider.Instance]route{}
	var box guestops.Ops
	for _, in := range []provider.Instance{owner, from, probeAs} {
		p, b, err := t.running(ctx, in)
		if err != nil {
			return guestbin.HTTPAnswer{}, err
		}
		r, why := t.routeOf(ctx, p)
		if why != "" {
			return guestbin.HTTPAnswer{}, errors.New(why)
		}
		routes[in] = r
		if in == from {
			box = b
		}
	}
	cred := strings.Repeat("c", 43)
	if err := o.Grant(ctx, system.GrantSpec{Credential: cred, Op: system.GrantCheckout, Mirror: mirror, Network: routes[owner].src.Network, TTL: time.Minute,
		Sources: []system.Source{routes[owner].src, routes[from].src}}); err != nil {
		return guestbin.HTTPAnswer{}, err
	}
	base := routes[from].base
	return box.Fetch(ctx, guestops.HTTPTransfer{
		URL: base + system.PathPrefix + system.GrantCheckout, Probe: base + system.PathPing + "?network=" + routes[probeAs].src.Network, Credential: cred,
	}, "/tmp/elsewhere")
}

// Present has the box from present cred for a checkout over its own
// network's endpoint: how the server answers a credential afterwards.
func (t *Target) Present(ctx context.Context, from provider.Instance, cred string) (guestbin.HTTPAnswer, error) {
	p, box, err := t.running(ctx, from)
	if err != nil {
		return guestbin.HTTPAnswer{}, err
	}
	r, why := t.routeOf(ctx, p)
	if why != "" {
		return guestbin.HTTPAnswer{}, errors.New(why)
	}
	return box.Fetch(ctx, guestops.HTTPTransfer{
		URL: r.base + system.PathPrefix + system.GrantCheckout, Probe: r.base + system.PathPing + "?network=" + r.src.Network, Credential: cred,
	}, "/tmp/presented")
}
