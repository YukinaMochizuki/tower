package hostd_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/fake/faketest"
	"github.com/YukinaMochizuki/tower/internal/provider/hostd"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
	"github.com/YukinaMochizuki/tower/internal/tunnel"
)

// fast is a tunnel liveness a test can wait out.
var fast = tunnel.Liveness{Idle: 100 * time.Millisecond, Timeout: 100 * time.Millisecond}

// carried is a world whose fake's work tower reaches through a hostd
// client, over a real tunnel to a HostService served over the fake.
type carried struct {
	faketest.World
	hub    *tunnel.Hub
	client *hostd.Client
	// stopAgent stops the host's end of the tunnel.
	stopAgent func()
}

func newCarried(t *testing.T, calls *hostd.Calls) *carried {
	t.Helper()
	w := faketest.New(t, nil)
	c := &carried{World: w, hub: &tunnel.Hub{Liveness: fast}}
	c.client = &hostd.Client{Target: faketest.Target, Tunnels: c.hub, Calls: calls}
	mux := http.NewServeMux()
	mux.HandleFunc(tunnel.Path, func(w http.ResponseWriter, r *http.Request) { c.hub.Accept(w, r, faketest.Target) })
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		c.hub.Close()
	})
	host := http.NewServeMux()
	host.Handle(hostd.NewHandler(w.Fake))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&tunnel.Agent{
			Dial: func(ctx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", srv.Listener.Addr().String())
			},
			Handler: host, Liveness: fast, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		}).Run(ctx)
	}()
	c.stopAgent = func() {
		cancel()
		<-done
	}
	t.Cleanup(c.stopAgent)
	c.waitTunnel(t)
	return c
}

func (c *carried) waitTunnel(t *testing.T) *tunnel.Tunnel {
	t.Helper()
	var tun *tunnel.Tunnel
	testutil.Eventually(t, "the host's tunnel open", func() bool {
		var err error
		tun, err = c.hub.Node(faketest.Target)
		return err == nil
	})
	return tun
}

// TestConformanceThroughATunnel holds the hostd client, carrying the fake's
// work over a tunnel, to the provider contract: what the real hostd will
// be held to through the same seam.
func TestConformanceThroughATunnel(t *testing.T) {
	c := newCarried(t, nil)
	providertest.Run(t, c.Harness(c.client))
}

func container() *envv1alpha1.InstanceSpec {
	return &envv1alpha1.InstanceSpec{Target: faketest.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, Image: "img"}
}

// TestErrorsCrossTheTunnel: each provider error the host's provider
// returns is the same error value at tower's end.
func TestErrorsCrossTheTunnel(t *testing.T) {
	c := newCarried(t, nil)
	h := c.Harness(c.client)
	in, _ := h.Start(t, "errs", container())
	ctx := t.Context()
	if err := c.client.ReadFile(ctx, in, "/none", io.Discard); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file not there: %v, want fs.ErrNotExist", err)
	}
	if err := c.client.ReadFile(ctx, provider.Instance{Project: in.Project, Name: "absent"}, "/x", io.Discard); !errors.Is(err, provider.ErrNotFound) {
		t.Errorf("an instance not there: %v, want ErrNotFound", err)
	}
	var none uint64
	c.Fake.SetDiskFree(&none)
	if _, err := c.client.BuildImage(ctx, provider.BuildRequest{Context: strings.NewReader("ctx"), Repository: "r"}); !errors.Is(err, provider.ErrNoSpace) {
		t.Errorf("a build on a full disk: %v, want ErrNoSpace", err)
	}
	c.Fake.SetDiskFree(nil)
	c.Fake.FailBuilds(errors.New("step 3 failed: " + provider.ErrFailed.Error()))
	_, err := c.client.BuildImage(ctx, provider.BuildRequest{Context: strings.NewReader("ctx"), Repository: "r"})
	if err == nil || errors.Is(err, provider.ErrFailed) || !strings.Contains(err.Error(), "step 3 failed") {
		t.Errorf("an error of no kind: %v, want its message and no provider error", err)
	}
	c.Fake.FailBuilds(nil)
	c.Fake.SetUnavailable(true)
	if _, err := c.client.Exec(ctx, provider.ExecRequest{Instance: in, Command: []string{"true"}}); !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("a host whose provider does not answer: %v, want ErrUnavailable", err)
	}
	c.Fake.SetUnavailable(false)
}

// TestStdinReachesTheCommand: what the caller gives as stdin crosses the
// tunnel; the fake's cat with no file echoes it.
func TestStdinReachesTheCommand(t *testing.T) {
	c := newCarried(t, nil)
	in, _ := c.Harness(c.client).Start(t, "stdin", container())
	var out bytes.Buffer
	c.Fake.OnExec(func(req provider.ExecRequest) (int, bool) {
		if req.Stdin == nil {
			return 2, true
		}
		b, _ := io.ReadAll(req.Stdin)
		_, _ = req.Stdout.Write(b)
		return 0, true
	})
	code, err := c.client.Exec(t.Context(), provider.ExecRequest{Instance: in, Command: []string{"read"}, Stdin: strings.NewReader("typed"), Stdout: &out})
	if err != nil || code != 0 || out.String() != "typed" {
		t.Errorf("stdin came back as %q, exit %d, %v", out.String(), code, err)
	}
	code, err = c.client.Exec(t.Context(), provider.ExecRequest{Instance: in, Command: []string{"read"}})
	if err != nil || code != 2 {
		t.Errorf("a command given no stdin was given one: exit %d, %v", code, err)
	}
}

// TestNoTunnelIsUnavailable: a target whose host has no tunnel open
// answers ErrUnavailable, at once.
func TestNoTunnelIsUnavailable(t *testing.T) {
	cl := &hostd.Client{Target: "t9", Tunnels: &tunnel.Hub{}}
	if _, err := cl.Exec(t.Context(), provider.ExecRequest{Command: []string{"true"}}); !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("a call to a target never connected: %v, want ErrUnavailable", err)
	}
}

// TestADroppedTunnelIsUnavailable: a tunnel that drops under a call ends
// it with ErrUnavailable, and so does a call made after.
func TestADroppedTunnelIsUnavailable(t *testing.T) {
	c := newCarried(t, nil)
	in, _ := c.Harness(c.client).Start(t, "dropped", container())
	errc := make(chan error, 1)
	go func() {
		_, err := c.client.Exec(context.Background(), provider.ExecRequest{Instance: in, Command: []string{"sleep"}})
		errc <- err
	}()
	testutil.Eventually(t, "the command running", func() bool {
		execs := c.Fake.Execs()
		return len(execs) > 0 && execs[len(execs)-1].Command[0] == "sleep"
	})
	c.stopAgent()
	select {
	case err := <-errc:
		if !errors.Is(err, provider.ErrUnavailable) {
			t.Errorf("a call whose tunnel dropped: %v, want ErrUnavailable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a call whose tunnel dropped never ended")
	}
	testutil.Eventually(t, "the tunnel gone", func() bool {
		_, err := c.hub.Node(faketest.Target)
		return err != nil
	})
	if _, err := c.client.Exec(t.Context(), provider.ExecRequest{Instance: in, Command: []string{"true"}}); !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("a call after the drop: %v, want ErrUnavailable", err)
	}
}

// TestCallsAreCapped: a call over its caller's cap, or its project's, is
// refused at once, and tower's own calls are not counted.
func TestCallsAreCapped(t *testing.T) {
	c := newCarried(t, &hostd.Calls{MaxPerCaller: 1, MaxPerProject: 2})
	in, _ := c.Harness(c.client).Start(t, "capped", container())
	as := func(id string) context.Context {
		return auth.WithPrincipal(t.Context(), auth.Principal{Type: auth.ServiceToken, ID: id})
	}
	run := func(ctx context.Context, command string) error {
		_, err := c.client.Exec(ctx, provider.ExecRequest{Instance: in, Command: []string{command}})
		return err
	}
	// Two callers each hold a call open: the project is at its cap.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{}, 2)
	for _, who := range []string{"workstream", "other"} {
		go func() {
			_ = run(auth.WithPrincipal(ctx, auth.Principal{Type: auth.ServiceToken, ID: who}), "sleep")
			done <- struct{}{}
		}()
	}
	testutil.Eventually(t, "both commands running", func() bool { return len(c.Fake.Execs()) == 2 })
	if err := run(as("workstream"), "true"); !errors.Is(err, hostd.ErrTooManyCalls) || !strings.Contains(err.Error(), "serviceToken:workstream") {
		t.Errorf("a call over the caller's cap: %v, want ErrTooManyCalls naming the caller", err)
	}
	if err := run(as("third"), "true"); !errors.Is(err, hostd.ErrTooManyCalls) || !strings.Contains(err.Error(), "project") {
		t.Errorf("a call over the project's cap: %v, want ErrTooManyCalls naming the project", err)
	}
	if err := run(auth.WithPrincipal(t.Context(), auth.SystemPrincipal), "true"); err != nil {
		t.Errorf("tower's own call, the caps full: %v", err)
	}
	cancel()
	<-done
	<-done
	if err := run(as("workstream"), "true"); err != nil {
		t.Errorf("a call once the first ended: %v", err)
	}
}
