package tunnel_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/net/http2"

	"github.com/YukinaMochizuki/tower/internal/testutil"
	"github.com/YukinaMochizuki/tower/internal/tunnel"
)

// fast is a liveness tests can wait out.
var fast = tunnel.Liveness{Idle: 100 * time.Millisecond, Timeout: 100 * time.Millisecond}

// apiserver is a hub behind a socket served as the apiserver serves
// its node socket, HTTP/1.1 and cleartext HTTP/2, taking every tunnel as
// node n1's.
type apiserver struct {
	hub *tunnel.Hub
	// dial connects to its socket.
	dial func(ctx context.Context) (net.Conn, error)

	mu sync.Mutex
	// authorization is what the last request opening a tunnel carried.
	authorization string
	// changes are the nodes the hub told of, in order.
	changes []string
}

func (a *apiserver) seen() (authorization string, changes []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.authorization, slices.Clone(a.changes)
}

func newAPIServer(t *testing.T) *apiserver {
	t.Helper()
	dir, err := os.MkdirTemp("", "tunnel")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "node.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	return serveAPIServer(t, ln, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	})
}

// newPipeAPIServer is newAPIServer on net.Pipe connections, which a
// synctest bubble can hold, as it cannot a socket.
func newPipeAPIServer(t *testing.T) *apiserver {
	t.Helper()
	ln := &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
	return serveAPIServer(t, ln, ln.dial)
}

func serveAPIServer(t *testing.T, ln net.Listener, dial func(context.Context) (net.Conn, error)) *apiserver {
	t.Helper()
	a := &apiserver{dial: dial}
	a.hub = &tunnel.Hub{Liveness: fast, Changed: func(node string) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.changes = append(a.changes, node)
	}}
	mux := http.NewServeMux()
	mux.HandleFunc(tunnel.Path, func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.authorization = r.Header.Get("Authorization")
		a.mu.Unlock()
		a.hub.Accept(w, r, "n1")
	})
	p := &http.Protocols{}
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: p}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close()
		a.hub.Close()
	})
	return a
}

// pipeListener accepts the server ends of the net.Pipe connections dial
// makes.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// node is the handler an agent serves: /hello answers, /stream writes a
// line every 10 ms until its call ends, counting the streams under way.
type node struct {
	streams atomic.Int32
	// flooded counts what /flood calls have written.
	flooded atomic.Int64
}

func (n *node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/hello":
		_, _ = fmt.Fprintf(w, "hello over %s", r.Proto)
	case "/flood":
		chunk := make([]byte, 16<<10)
		for r.Context().Err() == nil {
			k, err := w.Write(chunk)
			n.flooded.Add(int64(k))
			if err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	case "/until-stopped":
		n.streams.Add(1)
		defer n.streams.Add(-1)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		_, _ = io.WriteString(w, "the agent is stopping")
	case "/stream":
		n.streams.Add(1)
		defer n.streams.Add(-1)
		for i := 0; ; i++ {
			if _, err := fmt.Fprintf(w, "%d\n", i); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	default:
		http.NotFound(w, r)
	}
}

type agent struct {
	opened atomic.Int32
	closed atomic.Int32
	stop   func()
}

func startAgent(t *testing.T, a *apiserver, handler http.Handler, dial func(context.Context) (net.Conn, error)) *agent {
	t.Helper()
	return startAgentLive(t, a, handler, dial, fast)
}

// startAgentLive is startAgent with the agent's liveness live.
func startAgentLive(t *testing.T, a *apiserver, handler http.Handler, dial func(context.Context) (net.Conn, error), live tunnel.Liveness) *agent {
	t.Helper()
	ag := &agent{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&tunnel.Agent{
			Dial: dial, Handler: handler, Liveness: live, Header: http.Header{"Authorization": {"Bearer twr_test"}},
			MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
			Opened: func() { ag.opened.Add(1) },
			Closed: func() { ag.closed.Add(1) },
		}).Run(ctx)
	}()
	ag.stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the agent did not stop")
		}
	}
	t.Cleanup(ag.stop)
	return ag
}

func tunnelOf(t *testing.T, a *apiserver) *tunnel.Tunnel {
	t.Helper()
	var tun *tunnel.Tunnel
	testutil.EventuallyWithin(t, 5*time.Second, "a tunnel open", func() bool {
		var err error
		tun, err = a.hub.Node("n1")
		return err == nil
	})
	return tun
}

func hello(t *testing.T, tun *tunnel.Tunnel) {
	t.Helper()
	resp, err := tun.HTTPClient().Get("http://node/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "hello over HTTP/2.0" {
		t.Errorf("answered %q", b)
	}
}

// noLeaks fails if goroutines started during the test outlive it.
func noLeaks(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		testutil.EventuallyWithin(t, 5*time.Second, "no goroutine outliving the test", func() (bool, string) {
			if n := runtime.NumGoroutine(); n > before {
				buf := make([]byte, 1<<20)
				return false, fmt.Sprintf("%d goroutines, %d before:\n%s", n, before, buf[:runtime.Stack(buf, true)])
			}
			return true, ""
		})
	})
}

func TestARoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		noLeaks(t)
		a := newPipeAPIServer(t)
		startAgent(t, a, &node{}, a.dial)
		tun := tunnelOf(t, a)
		if tun.Node() != "n1" {
			t.Errorf("the tunnel is node %q's", tun.Node())
		}
		hello(t, tun)
		if auth, _ := a.seen(); auth != "Bearer twr_test" {
			t.Errorf("the request opening the tunnel carried Authorization %q, not the agent's header", auth)
		}
		// Idle beyond its liveness, the tunnel stays up: pings keep it.
		time.Sleep(5 * (fast.Idle + fast.Timeout)) //nolint:forbidigo // virtual time
		synctest.Wait()
		select {
		case <-tun.Done():
			t.Fatal("an idle tunnel dropped")
		default:
		}
		hello(t, tun)
	})
}

// TestAStoppingAgentLetsItsCallsAnswer: an agent stopping ends its calls,
// and their answers reach the apiserver before the tunnel closes.
func TestAStoppingAgentLetsItsCallsAnswer(t *testing.T) {
	noLeaks(t)
	a := newAPIServer(t)
	n := &node{}
	ag := startAgent(t, a, n, a.dial)
	tun := tunnelOf(t, a)
	resp, err := tun.HTTPClient().Get("http://node/until-stopped")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	testutil.EventuallyWithin(t, 5*time.Second, "the call begun", func() bool { return n.streams.Load() == 1 })
	go ag.stop()
	b, err := io.ReadAll(resp.Body)
	if err != nil || string(b) != "the agent is stopping" {
		t.Errorf("the call's answer: %q %v", b, err)
	}
	select {
	case <-tun.Done():
	case <-time.After(5 * time.Second):
		t.Error("the tunnel stayed open after the agent stopped")
	}
}

// TestCallsNotReadDoNotStopTheOthers: calls whose bodies the apiserver
// does not read, as when their callers stop reading, each hold their
// stream's window. With a connection window smaller than theirs together
// they hold it all, and another call gets no data; with the tunnel's
// sizing, the connection's window every stream's, it does. The windows
// are scaled down from the tunnel's.
func TestCallsNotReadDoNotStopTheOthers(t *testing.T) {
	const stream = 64 << 10
	for _, tt := range []struct {
		name       string
		connection int
		stalls     bool
	}{
		{"a connection window less than its streams'", 4 * stream, true},
		{"the connection window every stream's", tunnel.MaxStreams * stream, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := newAPIServer(t)
			a.hub.SetWindows(stream, tt.connection)
			n := &node{}
			startAgent(t, a, n, a.dial)
			tun := tunnelOf(t, a)
			const held = 8
			for range held {
				resp, err := tun.HTTPClient().Get("http://node/flood")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = resp.Body.Close() })
			}
			// The floods fill what the windows let them, then stall.
			last, since := int64(-1), time.Now()
			testutil.Eventually(t, "the floods stalled", func() (bool, string) {
				if f := n.flooded.Load(); f != last {
					last, since = f, time.Now()
				}
				return time.Since(since) >= 200*time.Millisecond, fmt.Sprintf("%d bytes", last)
			})
			t.Logf("%d calls not read hold %d KiB", held, last>>10)

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://node/hello", nil)
			resp, err := tun.HTTPClient().Do(req)
			var body []byte
			if err == nil {
				body, err = io.ReadAll(resp.Body)
				_ = resp.Body.Close()
			}
			got := err == nil && string(body) == "hello over HTTP/2.0"
			switch {
			case tt.stalls && got:
				t.Error("another call got its data: the stall this sizing allows did not happen")
			case !tt.stalls && !got:
				t.Errorf("another call got no data: %q %v", body, err)
			}
		})
	}
}

func TestNoTunnel(t *testing.T) {
	a := newAPIServer(t)
	if _, err := a.hub.Node("n1"); !errors.Is(err, tunnel.ErrNoTunnel) {
		t.Errorf("a node never connected: %v", err)
	}
}

// TestADisconnectMidStream: a tunnel dropped under a stream fails the
// apiserver's read at once and ends the node's call; the agent opens the
// tunnel again, and calls go through it.
func TestADisconnectMidStream(t *testing.T) {
	noLeaks(t)
	a := newAPIServer(t)
	n := &node{}
	ag := startAgent(t, a, n, a.dial)
	tun := tunnelOf(t, a)
	resp, err := tun.HTTPClient().Get("http://node/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	r := bufio.NewReader(resp.Body)
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	tun.Close()
	start := time.Now()
	for {
		if _, err := r.ReadString('\n'); err != nil {
			break
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the read failed %v after the drop", took)
	}
	testutil.EventuallyWithin(t, 5*time.Second, "the node's call ended", func() bool { return n.streams.Load() == 0 })

	testutil.EventuallyWithin(t, 5*time.Second, "the tunnel opened again", func() bool { return ag.opened.Load() >= 2 })
	again := tunnelOf(t, a)
	if again == tun {
		t.Fatal("the dropped tunnel is still the node's")
	}
	hello(t, again)
	if opened, closed := ag.opened.Load(), ag.closed.Load(); closed != opened-1 {
		t.Errorf("the agent was told of %d tunnels closed of %d opened, one of them open", closed, opened)
	}
}

// TestCancellingACallEndsItOnTheNode: a call the apiserver gives up ends
// on the node, and the tunnel carries on.
func TestCancellingACallEndsItOnTheNode(t *testing.T) {
	noLeaks(t)
	a := newAPIServer(t)
	n := &node{}
	startAgent(t, a, n, a.dial)
	tun := tunnelOf(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://node/stream", nil)
	resp, err := tun.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	testutil.EventuallyWithin(t, 5*time.Second, "the node's call begun", func() bool { return n.streams.Load() == 1 })
	cancel()
	_ = resp.Body.Close()
	testutil.EventuallyWithin(t, 5*time.Second, "the node's call ended", func() bool { return n.streams.Load() == 0 })
	hello(t, tun)
}

// blackHole is a connection that, once cut, drops whatever is written to
// it and never reads anything again, as a peer gone dead behind a network
// that does not say so.
type blackHole struct {
	net.Conn
	// readsOnly, once cut, drops only what comes from the apiserver.
	readsOnly bool
	cut       chan struct{}
	once      sync.Once
	closed    chan struct{}
	shut      sync.Once
}

func (b *blackHole) Read(p []byte) (int, error) {
	select {
	case <-b.cut:
	default:
		n, err := b.Conn.Read(p)
		select {
		case <-b.cut:
			// Cut while reading: what it read went nowhere.
		default:
			return n, err
		}
	}
	<-b.closed
	return 0, net.ErrClosed
}

func (b *blackHole) Write(p []byte) (int, error) {
	select {
	case <-b.cut:
		if b.readsOnly {
			return b.Conn.Write(p)
		}
		return len(p), nil
	default:
		return b.Conn.Write(p)
	}
}

func (b *blackHole) Close() error {
	b.shut.Do(func() { close(b.closed) })
	return b.Conn.Close()
}

// TestAPingUnansweredDropsTheTunnel: a tunnel whose node has gone silent
// is dropped by the apiserver within its liveness, failing the call on it,
// and the agent, hearing nothing either, opens another.
func TestAPingUnansweredDropsTheTunnel(t *testing.T) {
	noLeaks(t)
	a := newAPIServer(t)
	n := &node{}
	var mu sync.Mutex
	var holes []*blackHole
	ag := startAgent(t, a, n, func(ctx context.Context) (net.Conn, error) {
		c, err := a.dial(ctx)
		if err != nil {
			return nil, err
		}
		b := &blackHole{Conn: c, cut: make(chan struct{}), closed: make(chan struct{})}
		mu.Lock()
		holes = append(holes, b)
		mu.Unlock()
		return b, nil
	})
	tun := tunnelOf(t, a)
	resp, err := tun.HTTPClient().Get("http://node/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	r := bufio.NewReader(resp.Body)
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	first := holes[0]
	mu.Unlock()
	first.once.Do(func() { close(first.cut) })
	start := time.Now()
	bound := 3 * (fast.Idle + fast.Timeout)
	readErr := make(chan error, 1)
	go func() {
		for {
			if _, err := r.ReadString('\n'); err != nil {
				readErr <- err
				return
			}
		}
	}()
	select {
	case <-tun.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the dead tunnel was never dropped")
	}
	if took := time.Since(start); took > bound {
		t.Errorf("dropped after %v, past %v", took, bound)
	}
	select {
	case <-readErr:
	case <-time.After(time.Second):
		t.Error("the call on the dropped tunnel did not fail")
	}
	testutil.EventuallyWithin(t, 5*time.Second, "the agent opened another tunnel", func() bool { return ag.opened.Load() >= 2 })
	testutil.EventuallyWithin(t, 5*time.Second, "the node's call ended", func() bool { return n.streams.Load() == 0 })
	hello(t, tunnelOf(t, a))
}

// TestTheApiserversPingDropsADeadTunnel: what the apiserver sends no
// longer reaching the agent, and the agent pinging only after an hour, the
// apiserver's own ping drops the tunnel within its liveness.
func TestTheApiserversPingDropsADeadTunnel(t *testing.T) {
	noLeaks(t)
	a := newAPIServer(t)
	var hole *blackHole
	var mu sync.Mutex
	startAgentLive(t, a, &node{}, func(ctx context.Context) (net.Conn, error) {
		c, err := a.dial(ctx)
		if err != nil {
			return nil, err
		}
		b := &blackHole{Conn: c, readsOnly: true, cut: make(chan struct{}), closed: make(chan struct{})}
		mu.Lock()
		if hole == nil {
			hole = b
		}
		mu.Unlock()
		return b, nil
	}, tunnel.Liveness{Idle: time.Hour, Timeout: time.Hour})
	tun := tunnelOf(t, a)
	hello(t, tun)
	mu.Lock()
	hole.once.Do(func() { close(hole.cut) })
	mu.Unlock()
	start := time.Now()
	select {
	case <-tun.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the apiserver never dropped the dead tunnel")
	}
	if took, bound := time.Since(start), 3*(fast.Idle+fast.Timeout); took > bound {
		t.Errorf("dropped after %v, past %v", took, bound)
	}
}

// TestANewerTunnelReplacesTheOlder: a node opening a tunnel again, as a
// restarted agent does while its old connection lingers, takes the
// node's place; the older is dropped.
func TestANewerTunnelReplacesTheOlder(t *testing.T) {
	noLeaks(t)
	a := newAPIServer(t)
	serve := func() (done chan struct{}) {
		c, err := a.dial(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		c, err = tunnel.Open(t.Context(), c, nil)
		if err != nil {
			t.Fatal(err)
		}
		done = make(chan struct{})
		go func() {
			defer close(done)
			(&http2.Server{}).ServeConn(c, &http2.ServeConnOpts{Handler: &node{}}) //nolint:staticcheck // SA1019: as the agent serves, tunnel.go
			_ = c.Close()
		}()
		t.Cleanup(func() { _ = c.Close(); <-done })
		return done
	}
	firstServed := serve()
	first := tunnelOf(t, a)
	serve()
	testutil.EventuallyWithin(t, 5*time.Second, "the second tunnel the node's", func() bool {
		t2, err := a.hub.Node("n1")
		return err == nil && t2 != first
	})
	select {
	case <-first.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the older tunnel was not dropped")
	}
	select {
	case <-firstServed:
	case <-time.After(5 * time.Second):
		t.Fatal("the node's end of the older tunnel did not end")
	}
	second, _ := a.hub.Node("n1")
	hello(t, second)
	// The older dropping is no change: the node is still connected.
	if _, changes := a.seen(); len(changes) != 2 {
		t.Errorf("the hub told of %v, want the two opens alone", changes)
	}
}

// TestTheHubTellsOfOpensAndDrops: Changed hears of a tunnel opening and
// dropping, each once Node says so.
func TestTheHubTellsOfOpensAndDrops(t *testing.T) {
	noLeaks(t)
	a := newAPIServer(t)
	var connected []bool
	a.hub.Changed = func(node string) {
		_, err := a.hub.Node(node)
		a.mu.Lock()
		defer a.mu.Unlock()
		connected = append(connected, err == nil)
	}
	ag := startAgent(t, a, &node{}, a.dial)
	tunnelOf(t, a)
	ag.stop()
	testutil.EventuallyWithin(t, 5*time.Second, "an open, then a drop", func() (bool, string) {
		a.mu.Lock()
		defer a.mu.Unlock()
		return slices.Equal(connected, []bool{true, false}), fmt.Sprint(connected)
	})
}

// TestOnlyAnUpgradeOpensATunnel: a plain request, or one over HTTP/2,
// cannot be taken over as a tunnel.
func TestOnlyAnUpgradeOpensATunnel(t *testing.T) {
	a := newAPIServer(t)
	plain := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return a.dial(ctx) }}}
	resp, err := plain.Get("http://hangar" + tunnel.Path)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("a plain request: %s", resp.Status)
	}
	p := &http.Protocols{}
	p.SetUnencryptedHTTP2(true)
	h2c := &http.Client{Transport: &http.Transport{Protocols: p, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return a.dial(ctx) }}}
	resp, err = h2c.Get("http://hangar" + tunnel.Path)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired || resp.ProtoMajor != 2 {
		t.Errorf("a request over HTTP/2: %s %s", resp.Proto, resp.Status)
	}
	if _, err := a.hub.Node("n1"); err == nil {
		t.Error("a plain request opened a tunnel")
	}
}

// TestAStoppedHubRefusesTunnels: the apiserver stopping drops every
// tunnel, and the agent cannot open one.
func TestAStoppedHubRefusesTunnels(t *testing.T) {
	noLeaks(t)
	a := newAPIServer(t)
	startAgent(t, a, &node{}, a.dial)
	tun := tunnelOf(t, a)
	a.hub.Close()
	select {
	case <-tun.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("closing the hub left a tunnel open")
	}
	c, err := a.dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tunnel.Open(t.Context(), c, nil); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("opening a tunnel to a stopped hub: %v", err)
	}
}
