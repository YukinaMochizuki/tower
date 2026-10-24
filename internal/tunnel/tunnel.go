// Package tunnel carries calls from the apiserver to a node agent over a
// connection the agent opens. The agent dials the apiserver's node socket
// and upgrades an HTTP/1.1 request to a tunnel; the roles then reverse, and
// the agent serves HTTP/2 on the connection, the apiserver being the
// client. A node so never listens, and a tunnel is as much the node's as
// any call the socket authenticates. This package is the transport alone:
// who may open a tunnel is the apiserver's to decide, and what is served
// on it the agent's. See docs/decisions/node-tunnel.md.
package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

const (
	// Path is the request that opens a tunnel.
	Path = "/hangar.node.v1.Tunnel/Open"
	// Protocol is the tunnel's Upgrade token.
	Protocol = "hangar-node-tunnel/1"
)

// Liveness is how a tunnel's two ends tell it is dead: each pings the
// other once the tunnel has been silent for Idle, and drops it if the
// ping is not answered within Timeout, nor a write taken within Timeout.
// A dead tunnel is so dropped within Idle plus Timeout.
type Liveness struct {
	Idle, Timeout time.Duration
}

// DefaultLiveness drops a dead tunnel within 20 seconds.
var DefaultLiveness = Liveness{Idle: 10 * time.Second, Timeout: 10 * time.Second}

func (l Liveness) orDefault() Liveness {
	if l.Idle == 0 || l.Timeout == 0 {
		return DefaultLiveness
	}
	return l
}

// handshakeTimeout bounds opening a tunnel, from either end.
const handshakeTimeout = 10 * time.Second

// Agent keeps a tunnel open to the apiserver, from the node agent, and
// serves Handler on it: it dials again whenever the tunnel drops, waiting
// longer after each failure in a row.
type Agent struct {
	// Dial connects to the apiserver's node socket.
	Dial    func(ctx context.Context) (net.Conn, error)
	Handler http.Handler
	// Liveness zero means DefaultLiveness.
	Liveness Liveness
	// MinBackoff and MaxBackoff bound the wait before dialing again; zero
	// means 200 ms and 5 s.
	MinBackoff, MaxBackoff time.Duration
	Log                    *slog.Logger
	// Opened, if set, is told of each tunnel opened, and Closed of each
	// opened tunnel ending.
	Opened, Closed func()
}

// Run keeps the tunnel open until ctx ends, and returns once every call
// served on it has.
func (a *Agent) Run(ctx context.Context) {
	minWait, maxWait := a.MinBackoff, a.MaxBackoff
	if minWait == 0 {
		minWait = 200 * time.Millisecond
	}
	if maxWait == 0 {
		maxWait = 5 * time.Second
	}
	log := a.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	var calls calls
	defer calls.wait()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.add(1)
		defer calls.add(-1)
		a.Handler.ServeHTTP(w, r)
	})
	wait := minWait
	for ctx.Err() == nil {
		opened, err := a.serve(ctx, handler, &calls)
		if ctx.Err() != nil {
			return
		}
		if opened {
			wait = minWait
			log.Warn("the tunnel to the apiserver dropped; opening it again", "error", err)
		} else {
			log.Error("opening the tunnel to the apiserver failed", "error", err, "retry_in", wait)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if !opened {
			wait = min(2*wait, maxWait)
		}
	}
}

// calls counts the calls under way. Not a sync.WaitGroup: a call may
// begin while Run waits, its context already done.
type calls struct {
	mu   sync.Mutex
	n    int
	idle *sync.Cond
}

func (c *calls) add(d int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += d
	if c.n == 0 && c.idle != nil {
		c.idle.Broadcast()
	}
}

func (c *calls) wait() { c.waitFor(-1) }

// waitFor waits for the calls to end, for at most d unless d is negative.
func (c *calls) waitFor(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.idle == nil {
		c.idle = sync.NewCond(&c.mu)
	}
	expired := false
	if d >= 0 {
		t := time.AfterFunc(d, func() {
			c.mu.Lock()
			expired = true
			c.idle.Broadcast()
			c.mu.Unlock()
		})
		defer t.Stop()
	}
	for c.n > 0 && !expired {
		c.idle.Wait()
	}
}

// MaxStreams is how many calls a node agent takes at once on its tunnel:
// the apiserver keeps its own calls to a node below it.
const MaxStreams = 1000

// HTTP/2 flow control windows of a tunnel, each end's for what it
// receives. A call whose reader stops holds at most its stream's window;
// the connection's holds every stream's at once, so no set of calls a
// tunnel takes, however slowly read, stops the others (node-tunnel.md).
const (
	StreamWindow     = 1 << 20
	ConnectionWindow = MaxStreams * StreamWindow
)

// Stopping the agent, the calls on its tunnel are given stopAnswer to
// answer, their contexts done, before the tunnel closes, and flushAnswers
// more for their answers to leave.
const (
	stopAnswer   = 2 * time.Second
	flushAnswers = 50 * time.Millisecond
)

// serve opens one tunnel and serves on it until it drops, reporting
// whether it opened.
func (a *Agent) serve(ctx context.Context, handler http.Handler, calls *calls) (bool, error) {
	conn, err := a.Dial(ctx)
	if err != nil {
		return false, err
	}
	conn, err = Open(ctx, conn)
	if err != nil {
		return false, err
	}
	if a.Opened != nil {
		a.Opened()
	}
	if a.Closed != nil {
		defer a.Closed()
	}
	live := a.Liveness.orDefault()
	// x/net deprecates http2.Server; net/http's cannot serve one accepted conn.
	//nolint:staticcheck // SA1019: see above
	srv := &http2.Server{
		ReadIdleTimeout: live.Idle, PingTimeout: live.Timeout, WriteByteTimeout: live.Timeout,
		MaxConcurrentStreams:     MaxStreams,
		MaxUploadBufferPerStream: StreamWindow, MaxUploadBufferPerConnection: ConnectionWindow,
	}
	stop := context.AfterFunc(ctx, func() {
		calls.waitFor(stopAnswer)
		time.Sleep(flushAnswers)
		_ = conn.Close()
	})
	defer stop()
	srv.ServeConn(conn, &http2.ServeConnOpts{Context: ctx, Handler: handler}) //nolint:staticcheck // SA1019: see the server above
	_ = conn.Close()
	return true, errors.New("the connection ended")
}

// Open upgrades conn, a connection to the apiserver's node socket, to a
// tunnel, and returns the connection to serve HTTP/2 on. It closes conn
// if it fails.
func Open(ctx context.Context, conn net.Conn) (net.Conn, error) {
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	fail := func(err error) (net.Conn, error) {
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("opening a tunnel: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	if _, err := io.WriteString(conn, "GET "+Path+" HTTP/1.1\r\nHost: hangar\r\nConnection: Upgrade\r\nUpgrade: "+Protocol+"\r\n\r\n"); err != nil {
		return fail(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return fail(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(resp.Header.Get("Upgrade"), Protocol) {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		_ = resp.Body.Close()
		return fail(fmt.Errorf("the apiserver answered %s: %s", resp.Status, strings.TrimSpace(string(msg))))
	}
	if !stop() {
		return fail(ctx.Err())
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn reads first what the handshake's reader took past the
// response: the start of the apiserver's HTTP/2 preface.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// ErrNoTunnel means a node has no tunnel open.
var ErrNoTunnel = errors.New("the node is not connected")

// Hub holds the tunnels node agents have opened to the apiserver, one per
// node: a newer tunnel replaces an older one of the same node.
type Hub struct {
	// Liveness zero means DefaultLiveness.
	Liveness Liveness
	Log      *slog.Logger

	mu      sync.Mutex
	tunnels map[string]*Tunnel
	closed  bool
	// streamWindow and connWindow, if set, replace StreamWindow and
	// ConnectionWindow, for tests.
	streamWindow, connWindow int
}

// Tunnel is one node's tunnel, open until Done.
type Tunnel struct {
	node   string
	cc     *http.ClientConn
	conn   *closeConn
	client *http.Client
}

// Node is the node the tunnel is of.
func (t *Tunnel) Node() string { return t.node }

// Done is closed once the tunnel has dropped; calls on it then fail.
func (t *Tunnel) Done() <-chan struct{} { return t.conn.done }

// Closed reports whether the tunnel has dropped. It is true for a call
// the drop failed by the time the call fails: net/http marks its HTTP/2
// connection closed before it fails the connection's streams, while Done
// may be closed only after.
func (t *Tunnel) Closed() bool {
	select {
	case <-t.conn.done:
		return true
	default:
		return t.cc.Err() != nil
	}
}

// HTTPClient sends requests to the node over the tunnel, whatever their
// URL's host.
func (t *Tunnel) HTTPClient() *http.Client { return t.client }

// Close drops the tunnel.
func (t *Tunnel) Close() { _ = t.conn.Close() }

// closeConn tells when it is closed: an HTTP/2 client connection closes
// its connection however it ends, a ping unanswered included.
type closeConn struct {
	net.Conn
	once sync.Once
	done chan struct{}
}

func (c *closeConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}

// Accept upgrades a request opening a tunnel, as Agent sends one, to
// node's tunnel, replacing any it had. The caller has authenticated the
// request as coming from node; nothing in it says which node it is.
func (h *Hub) Accept(w http.ResponseWriter, r *http.Request, node string) {
	if r.ProtoMajor != 1 || !strings.EqualFold(r.Header.Get("Upgrade"), Protocol) || !headerHas(r.Header, "Connection", "upgrade") {
		w.Header().Set("Upgrade", Protocol)
		http.Error(w, "a tunnel is opened by an HTTP/1.1 upgrade to "+Protocol, http.StatusUpgradeRequired)
		return
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		http.Error(w, "the apiserver is stopping", http.StatusServiceUnavailable)
		return
	}
	raw, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "the connection cannot be taken over", http.StatusInternalServerError)
		return
	}
	conn := &closeConn{Conn: raw, done: make(chan struct{})}
	// The agent sends nothing before the switch is answered.
	if brw.Reader.Buffered() > 0 {
		_ = conn.Close()
		return
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+Protocol+"\r\n\r\n"); err != nil {
		_ = conn.Close()
		return
	}
	_ = conn.SetDeadline(time.Time{})
	cc, err := h.clientConn(r.Context(), conn)
	if err != nil {
		_ = conn.Close()
		h.log().Error("opening a tunnel", "node", node, "error", err)
		return
	}
	t := &Tunnel{node: node, cc: cc, conn: conn, client: &http.Client{Transport: cc}}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		t.Close()
		return
	}
	if h.tunnels == nil {
		h.tunnels = map[string]*Tunnel{}
	}
	old := h.tunnels[node]
	h.tunnels[node] = t
	h.mu.Unlock()
	if old != nil {
		old.Close()
	}
	h.log().Info("node tunnel opened", "node", node, "replaced", old != nil)
	go func() {
		<-t.Done()
		_ = cc.Close()
		h.mu.Lock()
		if h.tunnels[node] == t {
			delete(h.tunnels, node)
		}
		h.mu.Unlock()
		h.log().Info("node tunnel dropped", "node", node)
	}()
}

// clientConn makes the apiserver's end of a tunnel on conn, with
// net/http's HTTP/2 client. Not x/net's http2.Transport: it passes no
// window size on to net/http.
func (h *Hub) clientConn(ctx context.Context, conn net.Conn) (*http.ClientConn, error) {
	live := h.Liveness.orDefault()
	stream, connection := StreamWindow, ConnectionWindow
	if h.streamWindow != 0 {
		stream, connection = h.streamWindow, h.connWindow
	}
	var dialed sync.Once
	protocols := &http.Protocols{}
	protocols.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: protocols,
		HTTP2: &http.HTTP2Config{
			MaxReceiveBufferPerStream: stream, MaxReceiveBufferPerConnection: connection,
			SendPingTimeout: live.Idle, PingTimeout: live.Timeout, WriteByteTimeout: live.Timeout,
		},
		// The tunnel's one connection, once.
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			var c net.Conn
			dialed.Do(func() { c = conn })
			if c == nil {
				return nil, errors.New("a tunnel has one connection")
			}
			return c, nil
		},
	}
	return tr.NewClientConn(context.WithoutCancel(ctx), "http", "node:80")
}

func (h *Hub) log() *slog.Logger {
	if h.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Log
}

// Node returns node's tunnel, or ErrNoTunnel.
func (h *Hub) Node(node string) (*Tunnel, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.tunnels[node]
	if !ok {
		return nil, fmt.Errorf("%w: node %s", ErrNoTunnel, node)
	}
	return t, nil
}

// Close drops every tunnel and refuses new ones.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	all := h.tunnels
	h.tunnels = nil
	h.mu.Unlock()
	for _, t := range all {
		t.Close()
	}
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, s := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(s), token) {
				return true
			}
		}
	}
	return false
}
