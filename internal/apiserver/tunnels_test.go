package apiserver_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/fake"
	hostdprovider "github.com/YukinaMochizuki/tower/internal/provider/hostd"
	"github.com/YukinaMochizuki/tower/internal/testutil"
	"github.com/YukinaMochizuki/tower/internal/tunnel"
)

// hosts is an env with hostd targets a and b and tower's own providers:
// each target's is its host over the tunnel the host opens.
type hosts struct {
	*env
	// secrets are the ROLE_HOSTD tokens' of each target.
	secrets map[string]string
}

func newHosts(t *testing.T) *hosts {
	t.Helper()
	h := &hosts{env: newEnv(t, func(c *apiserver.Config) { c.Providers = nil }), secrets: map[string]string{}}
	for _, target := range []string{"a", "b"} {
		if _, err := h.asSystem().generic.Apply(t.Context(), registry.Target, hostdTarget(target)); err != nil {
			t.Fatal(err)
		}
		h.secrets[target] = must[iamv1alpha1.CreateServiceTokenResponse](t)(h.local().issuer.CreateServiceToken(t.Context(),
			createToken("hostd-"+target, iamv1alpha1.Role_ROLE_HOSTD, target))).GetSecret()
	}
	return h
}

// dial connects to the apiserver's public listener.
func (h *hosts) dial(ctx context.Context) (net.Conn, error) {
	u, err := url.Parse(h.url)
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
}

// host runs a host's end of a tunnel opened with secret, serving
// HostService over a fake of its own, until stop is called.
func (h *hosts) host(secret string) (stop func()) {
	mux := http.NewServeMux()
	mux.Handle(hostdprovider.NewHandler(fake.New("host", clockwork.NewRealClock())))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&tunnel.Agent{
			Dial: h.dial, Header: http.Header{"Authorization": {"Bearer " + secret}}, Handler: mux,
			MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		}).Run(ctx)
	}()
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
		}
	}
	h.t.Cleanup(stop)
	return stop
}

// call calls target's host through tower's provider of it: a command in
// an instance no host has, which a host answers ErrNotFound.
func (h *hosts) call(target string) error {
	p, err := h.srv.ProviderFor(target)
	if err != nil {
		return err
	}
	_, err = p.Exec(h.t.Context(), provider.ExecRequest{Instance: provider.Instance{Project: "web", Name: "none"}, Command: []string{"true"}})
	return err
}

// waitReachable waits until target's Reachable condition has reason.
func (h *hosts) waitReachable(target, reason string) *targetsv1alpha1.TargetStatus {
	h.t.Helper()
	var st *targetsv1alpha1.TargetStatus
	testutil.Eventually(h.t, fmt.Sprintf("target %s reachable: %s", target, reason), func() (bool, string) {
		o, err := h.asSystem().generic.Get(h.t.Context(), registry.Target, client.Key{Name: target})
		if err != nil {
			return false, err.Error()
		}
		st = o.(*targetsv1alpha1.Target).GetStatus()
		c := condition(st.GetConditions(), apiserver.TargetReachable)
		return c.GetReason() == reason, st.String()
	})
	return st
}

// TestAHostdTokenOpensItsOwnTargetsTunnel: a host opens the tunnel of its
// token's target alone; calls to that target cross it, and calls to a
// target with no tunnel are Unavailable. Reachable says which.
func TestAHostdTokenOpensItsOwnTargetsTunnel(t *testing.T) {
	h := newHosts(t)
	if err := h.call("a"); !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("a call to a target never connected: %v, want ErrUnavailable", err)
	}
	h.host(h.secrets["a"])
	st := h.waitReachable("a", "Connected")
	if c := condition(st.GetConditions(), apiserver.TargetReachable); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE || st.GetLastError() != "" {
		t.Errorf("a connected target's status: %v", st)
	}
	if err := h.call("a"); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("a call to a's host: %v, want its host's ErrNotFound", err)
	}
	if err := h.call("b"); !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("a call to b, whose host never connected: %v, want ErrUnavailable", err)
	}
	st = h.waitReachable("b", "NeverConnected")
	if c := condition(st.GetConditions(), apiserver.TargetReachable); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || st.GetLastError() == "" {
		t.Errorf("a target never connected: %v", st)
	}
}

// TestOnlyHostdTokensOpenTunnels: a request opening a tunnel without a
// ROLE_HOSTD token is refused, and opens nothing.
func TestOnlyHostdTokensOpenTunnels(t *testing.T) {
	h := newHosts(t)
	ws := must[iamv1alpha1.CreateServiceTokenResponse](t)(h.local().issuer.CreateServiceToken(t.Context(),
		createToken("ws", iamv1alpha1.Role_ROLE_WORKSTREAM, ""))).GetSecret()
	for _, tt := range []struct {
		who    string
		header http.Header
		status string
	}{
		{"no credentials", nil, "401"},
		{"a bad secret", http.Header{"Authorization": {"Bearer twr_" + strings.Repeat("x", 43)}}, "401"},
		{"a workstream token", http.Header{"Authorization": {"Bearer " + ws}}, "403"},
		{"an admin, by the dev header", http.Header{auth.DevPrincipalHeader: {"serviceToken:" + admin}}, "403"},
	} {
		conn, err := h.dial(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tunnel.Open(t.Context(), conn, tt.header); err == nil || !strings.Contains(err.Error(), tt.status) {
			t.Errorf("a tunnel opened with %s: %v, want %s", tt.who, err, tt.status)
		}
	}
	for _, target := range []string{"a", "b"} {
		if err := h.call(target); !errors.Is(err, provider.ErrUnavailable) {
			t.Errorf("target %s after refused tunnels: %v, want ErrUnavailable", target, err)
		}
	}
}

// TestReachableFollowsTheTunnel: a tunnel that drops shows on its target,
// and so does its opening again.
func TestReachableFollowsTheTunnel(t *testing.T) {
	h := newHosts(t)
	stop := h.host(h.secrets["a"])
	h.waitReachable("a", "Connected")
	stop()
	st := h.waitReachable("a", "Dropped")
	if c := condition(st.GetConditions(), apiserver.TargetReachable); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || !strings.Contains(st.GetLastError(), "dropped") {
		t.Errorf("a dropped target's status: %v", st)
	}
	if err := h.call("a"); !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("a call after the drop: %v, want ErrUnavailable", err)
	}
	h.host(h.secrets["a"])
	if st := h.waitReachable("a", "Connected"); st.GetLastError() != "" {
		t.Errorf("last_error %q once connected again", st.GetLastError())
	}
}

// TestADeletedTokensTunnelIsDropped: a tunnel stands only while its token
// does; deleted, its tunnel drops at the next recheck.
func TestADeletedTokensTunnelIsDropped(t *testing.T) {
	h := newHosts(t)
	h.host(h.secrets["a"])
	h.waitReachable("a", "Connected")
	if _, err := h.asSystem().tokens.DeleteServiceToken(t.Context(), deleteToken("hostd-a")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the tunnel dropped", func() bool {
		h.clock.Advance(auth.RecheckInterval)
		return errors.Is(h.call("a"), provider.ErrUnavailable)
	})
	h.waitReachable("a", "Dropped")
}
