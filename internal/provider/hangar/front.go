package hangar

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/time/rate"

	"github.com/YukinaMochizuki/tower/internal/provider"
)

// hangar's front door, hangar-edge, holds each client address to limits of
// its own (hangar: internal/edge/front/limits.go and
// docs/decisions/front-door.md): a connection opened past its rate, or
// past its cap on open connections, is closed as it is accepted, which a
// client sees as an EOF in its TLS handshake, and a request past its rate,
// or past its cap on requests in flight, is answered 429. Every hangar
// target of a tower reaches an endpoint from one address, so they share
// one Front, which keeps them all under those limits together.

// FrontLimits pace what tower asks of one hangar endpoint.
type FrontLimits struct {
	// ConnRate and ConnBurst pace new connections: each WebSocket, an
	// exec's or a port forward's, is one, and so is each connection the
	// Connect calls open.
	ConnRate  float64
	ConnBurst int
	// RequestRate and RequestBurst pace requests: each call, and each
	// WebSocket's upgrade.
	RequestRate  float64
	RequestBurst int
	// MaxConns caps the connections open at once. The WebSockets, each
	// holding one for as long as it lasts, take all but frontCallConns of
	// them, which are kept for the calls.
	MaxConns int
	// MaxInFlight caps the requests in flight at once, each WebSocket one
	// for as long as it lasts.
	MaxInFlight int
}

// DefaultFrontLimits stay below hangar-edge's defaults for a client: 10
// connections a second with a burst of 50, 20 requests a second with a
// burst of 100, 64 connections open and 128 requests in flight. They leave
// room for the clock's skew and for the rest of tower's host behind the
// same address.
var DefaultFrontLimits = FrontLimits{ConnRate: 8, ConnBurst: 40, RequestRate: 16, RequestBurst: 80, MaxConns: 48, MaxInFlight: 96}

// withDefaults is l with each limit not set at its default.
func (l FrontLimits) withDefaults() FrontLimits {
	d := DefaultFrontLimits
	if l.ConnRate == 0 {
		l.ConnRate = d.ConnRate
	}
	if l.ConnBurst == 0 {
		l.ConnBurst = d.ConnBurst
	}
	if l.RequestRate == 0 {
		l.RequestRate = d.RequestRate
	}
	if l.RequestBurst == 0 {
		l.RequestBurst = d.RequestBurst
	}
	if l.MaxConns == 0 {
		l.MaxConns = d.MaxConns
	}
	if l.MaxInFlight == 0 {
		l.MaxInFlight = d.MaxInFlight
	}
	return l
}

// Validate says what is wrong with l, nothing when every limit is set.
func (l FrontLimits) Validate() error {
	if l.ConnRate <= 0 || l.ConnBurst <= 0 {
		return errors.New("a hangar endpoint's connection rate and burst must be positive")
	}
	if l.RequestRate <= 0 || l.RequestBurst <= 0 {
		return errors.New("a hangar endpoint's request rate and burst must be positive")
	}
	if l.MaxConns <= frontCallConns {
		return fmt.Errorf("a hangar endpoint's open connections must be more than the %d kept for its calls", frontCallConns)
	}
	if l.MaxInFlight < l.MaxConns {
		return errors.New("a hangar endpoint's requests in flight must be at least its open connections")
	}
	return nil
}

// FrontOptions are a Front's.
type FrontOptions struct {
	Limits FrontLimits
	// TLS, if set, is the client's TLS config, as a test's own CA needs.
	TLS *tls.Config
	// Attempts bounds the tries of a request the front door refused, and
	// Backoff is the wait before the second; zero is each one's default.
	Attempts int
	Backoff  time.Duration
	Log      *slog.Logger
}

const (
	// DefaultFrontAttempts and DefaultFrontBackoff are the tries of a
	// request the front door refused: the waits double up to
	// maxFrontBackoff, each jittered, and a Retry-After is waited out, up
	// to maxRetryAfter, and a little more.
	DefaultFrontAttempts = 6
	DefaultFrontBackoff  = 250 * time.Millisecond
	maxFrontBackoff      = 4 * time.Second
	maxRetryAfter        = 30 * time.Second
	// tlsCallDials caps the calls' dials over TLS in flight: the calls
	// waiting share the HTTP/2 connection one opens. (MaxConnsPerHost caps
	// HTTP/2's dials, not its connections.)
	tlsCallDials = 1
	// frontCallConns caps the calls' connections over plain HTTP/1.1, one
	// a call in flight; as many of MaxConns are kept from the WebSockets.
	frontCallConns = 8
	// backgroundShare is the share of each rate work that is not
	// interactive (provider.Interactive) may take; the rest is kept for it.
	backgroundShare = 0.75
	// refusalLogEvery is how often the refusals are summed up in the log.
	refusalLogEvery = 30 * time.Second
)

// Front is what tower's clients of one hangar endpoint share: a client
// whose connections and requests are paced and capped under FrontLimits,
// and whose requests the front door refused are tried again. Make it with
// NewFront.
type Front struct {
	// conns and requests pace everything, bgConns and bgRequests the work
	// that is not interactive, first.
	conns, requests, bgConns, bgRequests *rate.Limiter
	// open holds a token for each connection open, sockets for each
	// WebSocket, inFlight for each request in flight.
	open, sockets, inFlight chan struct{}
	attempts                int
	backoff                 time.Duration
	log                     *slog.Logger
	// dialNet opens a TCP connection, a net.Dialer's but in tests.
	dialNet func(ctx context.Context, network, addr string) (net.Conn, error)
	// tlsCalls and plainCalls carry the Connect calls, over https and
	// http; upgrades the WebSockets.
	tlsCalls, plainCalls, upgrades *http.Transport
	client                         *http.Client
	// jitter is a wait of about d, so the retries of many requests refused
	// at once spread out.
	jitter          func(d time.Duration) time.Duration
	dials, refusals atomic.Int64
	// logMu guards when refusals were last logged, and how many since.
	logMu    sync.Mutex
	lastLog  time.Time
	unlogged int
}

// FrontStats are what a Front has done.
type FrontStats struct {
	// Dials are the connections it tried to open; Refusals the requests the
	// front door refused, each tried again or failed with.
	Dials, Refusals int64
}

// NewFront returns a Front of o.
func NewFront(o FrontOptions) *Front {
	l := o.Limits.withDefaults()
	limiter := func(r float64, b int) *rate.Limiter { return rate.NewLimiter(rate.Limit(r), b) }
	f := &Front{
		conns: limiter(l.ConnRate, l.ConnBurst), requests: limiter(l.RequestRate, l.RequestBurst),
		bgConns:    limiter(l.ConnRate*backgroundShare, max(1, int(float64(l.ConnBurst)*backgroundShare))),
		bgRequests: limiter(l.RequestRate*backgroundShare, max(1, int(float64(l.RequestBurst)*backgroundShare))),
		open:       make(chan struct{}, l.MaxConns), sockets: make(chan struct{}, max(1, l.MaxConns-frontCallConns)),
		inFlight: make(chan struct{}, l.MaxInFlight),
		attempts: o.Attempts, backoff: o.Backoff, log: o.Log,
		dialNet: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		jitter:  func(d time.Duration) time.Duration { return d/2 + rand.N(d/2+1) },
	}
	if f.attempts <= 0 {
		f.attempts = DefaultFrontAttempts
	}
	if f.backoff <= 0 {
		f.backoff = DefaultFrontBackoff
	}
	if f.log == nil {
		f.log = slog.New(slog.DiscardHandler)
	}
	transport := func(http2 bool) *http.Transport {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DialContext = f.dial
		tr.ForceAttemptHTTP2 = http2
		if o.TLS != nil {
			tr.TLSClientConfig = o.TLS.Clone()
		}
		return tr
	}
	f.tlsCalls = transport(true)
	f.tlsCalls.MaxConnsPerHost = tlsCallDials
	f.plainCalls = transport(false)
	f.plainCalls.MaxConnsPerHost, f.plainCalls.MaxIdleConnsPerHost = frontCallConns, frontCallConns
	// A WebSocket's upgrade is HTTP/1.1's, on a connection of its own: one
	// an upgrade was refused on is not kept, as it would hold a place.
	f.upgrades = transport(false)
	f.upgrades.DisableKeepAlives = true
	f.client = &http.Client{Transport: paced{f: f}}
	return f
}

// Client is the HTTP client every call to the endpoint goes through, its
// WebSockets' included.
func (f *Front) Client() *http.Client { return f.client }

// Stats says what f has done so far.
func (f *Front) Stats() FrontStats {
	return FrontStats{Dials: f.dials.Load(), Refusals: f.refusals.Load()}
}

// CloseIdleConnections closes the connections no call uses.
func (f *Front) CloseIdleConnections() {
	f.tlsCalls.CloseIdleConnections()
	f.plainCalls.CloseIdleConnections()
	f.upgrades.CloseIdleConnections()
}

// wait waits for a token of all, and first of bg for work that is not
// interactive, so the interactive always find some of all.
func (f *Front) wait(ctx context.Context, all, bg *rate.Limiter) error {
	if !provider.IsInteractive(ctx) {
		if err := bg.Wait(ctx); err != nil {
			return err
		}
	}
	return all.Wait(ctx)
}

// take takes a token of sem, or fails as ctx ends.
func take(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dial opens a connection once one may be open and the rate allows one.
// Its failure is a dialError, which is never tried again: no front door
// refused it.
func (f *Front) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if err := take(ctx, f.open); err != nil {
		return nil, &dialError{fmt.Errorf("waiting for a connection to hangar to close: %w", err)}
	}
	var once sync.Once
	release := func() { once.Do(func() { <-f.open }) }
	if err := f.wait(ctx, f.conns, f.bgConns); err != nil {
		release()
		return nil, &dialError{fmt.Errorf("waiting to connect to hangar: %w", err)}
	}
	f.dials.Add(1)
	c, err := f.dialNet(ctx, network, addr)
	if err != nil {
		release()
		return nil, &dialError{err}
	}
	return &frontConn{Conn: c, release: release}, nil
}

// dialError is a connection that could not be opened.
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// frontConn gives its place among the open connections back as it closes.
type frontConn struct {
	net.Conn
	release func()
}

func (c *frontConn) Close() error {
	defer c.release()
	return c.Conn.Close()
}

// paced sends each request once one may be in flight and the rate allows
// one, and again, after a backoff, while the front door refuses it.
type paced struct {
	f *Front
}

func (p paced) RoundTrip(req *http.Request) (*http.Response, error) {
	f := p.f
	ctx := req.Context()
	next := f.tlsCalls
	switch {
	case isUpgrade(req):
		next = f.upgrades
	case req.URL.Scheme == "http":
		next = f.plainCalls
	}
	for attempt := 1; ; attempt++ {
		r := req
		if attempt > 1 {
			r = req.Clone(ctx)
			if hasBody(req) {
				body, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				r.Body = body
			}
		}
		release, err := f.admit(ctx, isUpgrade(req))
		if err != nil {
			if r.Body != nil {
				_ = r.Body.Close()
			}
			return nil, err
		}
		var gotConn atomic.Bool
		r = r.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(httptrace.GotConnInfo) { gotConn.Store(true) },
		}))
		resp, err := next.RoundTrip(r)
		why, after := refused(r, resp, err, gotConn.Load())
		if why == "" || ctx.Err() != nil {
			return held(resp, err, release)
		}
		f.noteRefusal(why)
		if attempt >= f.attempts || !replayable(req) {
			return held(resp, err, release)
		}
		wait := f.jitter(min(f.backoff<<(attempt-1), maxFrontBackoff))
		if after > 0 {
			after = min(after, maxRetryAfter)
			wait = max(wait, after+rand.N(after/4+1))
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait {
			// No later try would be in time: the caller's own backoff applies.
			return held(resp, err, release)
		}
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			_ = resp.Body.Close()
		}
		release()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// admit waits until a request may be in flight, and a WebSocket open if
// it is one's upgrade, and the rate allows one; release gives its places
// back.
func (f *Front) admit(ctx context.Context, upgrade bool) (release func(), err error) {
	var held []chan struct{}
	var once sync.Once
	release = func() {
		once.Do(func() {
			for _, c := range held {
				<-c
			}
		})
	}
	// A WebSocket waiting for its place holds no request's: the calls
	// always find some.
	sems := []chan struct{}{f.inFlight}
	if upgrade {
		sems = []chan struct{}{f.sockets, f.inFlight}
	}
	for _, sem := range sems {
		if err := take(ctx, sem); err != nil {
			release()
			return nil, fmt.Errorf("waiting for a call to hangar to end: %w", err)
		}
		held = append(held, sem)
	}
	if err := f.wait(ctx, f.requests, f.bgRequests); err != nil {
		release()
		return nil, fmt.Errorf("waiting to call hangar: %w", err)
	}
	return release, nil
}

// held is a round trip's answer, whose places are given back as its body
// is closed: a WebSocket's, as it ends.
func held(resp *http.Response, err error, release func()) (*http.Response, error) {
	if err != nil || resp == nil {
		release()
		return resp, err
	}
	b := &releasingBody{ReadCloser: resp.Body, release: release}
	if rw, ok := resp.Body.(io.ReadWriteCloser); ok {
		resp.Body = &releasingRWBody{releasingBody: b, w: rw}
	} else {
		resp.Body = b
	}
	return resp, nil
}

type releasingBody struct {
	io.ReadCloser
	release func()
}

func (b *releasingBody) Close() error {
	defer b.release()
	return b.ReadCloser.Close()
}

// releasingRWBody is a releasingBody that writes too, as a WebSocket's
// connection does.
type releasingRWBody struct {
	*releasingBody
	w io.Writer
}

func (b *releasingRWBody) Write(p []byte) (int, error) { return b.w.Write(p) }

// noteRefusal counts a refusal, and logs the refusals at most every
// refusalLogEvery: a refusal storm is one line, and is told from an
// outage, whose errors are not refusals.
func (f *Front) noteRefusal(why string) {
	f.refusals.Add(1)
	f.logMu.Lock()
	defer f.logMu.Unlock()
	f.unlogged++
	if now := time.Now(); now.Sub(f.lastLog) >= refusalLogEvery {
		f.log.Warn("hangar's front door refused requests; trying them again after a backoff", "refused", f.unlogged, "last", why)
		f.lastLog, f.unlogged = now, 0
	}
}

// refused says why the front door refused a request, and how long it
// asked to be left alone; "" for an answer that is not its refusal. It
// refuses a connection by closing it as it accepts it, so the request
// meets an EOF or a reset on a connection that was open: before it was
// sent, or before a WebSocket's upgrade, which starts nothing, was
// answered. A connection that could not be opened, or whose certificate
// does not hold, is no refusal.
func refused(req *http.Request, resp *http.Response, err error, gotConn bool) (string, time.Duration) {
	if err != nil {
		var de *dialError
		if errors.As(err, &de) || !closedByPeer(err) || (gotConn && !isUpgrade(req)) {
			return "", 0
		}
		return "it closed the connection: " + err.Error(), 0
	}
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
		return "", 0
	}
	// hangar's own refusals are Connect's errors, in JSON: its cap on calls
	// to machines is one, said as Unavailable, which tower's controllers
	// back off from. The front door's pages are plain text.
	if ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); ct == "application/json" {
		return "", 0
	}
	var after time.Duration
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		after = time.Duration(s) * time.Second
	}
	return "it answered " + resp.Status, after
}

// closedByPeer reports whether err is the other end closing the
// connection.
func closedByPeer(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET)
}

func hasBody(req *http.Request) bool { return req.Body != nil && req.Body != http.NoBody }

// replayable reports whether req can be sent again: it has no body, or
// one GetBody makes again, as Connect's unary calls do.
func replayable(req *http.Request) bool { return !hasBody(req) || req.GetBody != nil }

func isUpgrade(req *http.Request) bool {
	return req.Method == http.MethodGet && !hasBody(req) && req.Header.Get("Upgrade") != ""
}
