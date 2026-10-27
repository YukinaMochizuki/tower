package hangartest

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// DoorLimits are a FrontDoor's.
type DoorLimits struct {
	// ConnRate and ConnBurst cap how fast the client opens connections;
	// one past them is closed as soon as it is accepted.
	ConnRate  float64
	ConnBurst int
	// RequestRate and RequestBurst cap how fast it sends requests, each
	// HTTP/2 stream one; one past them is answered 429.
	RequestRate  float64
	RequestBurst int
	// Streams caps the HTTP/2 streams of a connection.
	Streams int
	// ConnsPerClient caps the client's open connections; one more is
	// closed as soon as it is accepted. RequestsPerClient caps its requests
	// in flight, a WebSocket's for as long as it lasts; one more is
	// answered 429.
	ConnsPerClient, RequestsPerClient int
	// PlainHTTP serves HTTP/1.1 without TLS, as a hangar reached without
	// its front door is.
	PlainHTTP bool
	// RefuseFirst closes the first connections, whatever the rate; and
	// TooManyFirst answers the first requests 429.
	RefuseFirst, TooManyFirst int
}

// EdgeLimits are hangar-edge's defaults for one client (hangar:
// internal/edge/front/limits.go, DefaultLimits).
var EdgeLimits = DoorLimits{ConnRate: 10, ConnBurst: 50, RequestRate: 20, RequestBurst: 100, Streams: 32, ConnsPerClient: 64, RequestsPerClient: 128}

// FrontDoor stands in for hangar's front door, hangar-edge, before a
// handler: every connection is one client's, held to its limits as
// hangar-edge holds a client, refused as it refuses one (hangar:
// internal/edge/front/limits.go and docs/decisions/front-door.md).
type FrontDoor struct {
	// URL is where it is served; TLS is a client config that trusts it.
	URL string
	TLS *tls.Config

	mu                    sync.Mutex
	l                     DoorLimits
	conns, requests       *rate.Limiter
	accepted              []time.Time
	refused, tooMany, all int
	// open and inFlight are the connections open and the requests in
	// flight now, mostOpen and mostInFlight the most there have been.
	open, inFlight, mostOpen, mostInFlight int
}

// NewFrontDoor serves h behind a front door of l until the test ends.
func NewFrontDoor(t testing.TB, h http.Handler, l DoorLimits) *FrontDoor {
	t.Helper()
	d := &FrontDoor{
		l:        l,
		conns:    rate.NewLimiter(rate.Limit(l.ConnRate), l.ConnBurst),
		requests: rate.NewLimiter(rate.Limit(l.RequestRate), l.RequestBurst),
	}
	srv := httptest.NewUnstartedServer(d.limitRequests(h))
	srv.Listener = doorListener{Listener: srv.Listener, d: d}
	if l.PlainHTTP {
		srv.Start()
	} else {
		srv.EnableHTTP2 = true
		srv.Config.HTTP2 = &http.HTTP2Config{MaxConcurrentStreams: l.Streams}
		srv.StartTLS()
		pool := x509.NewCertPool()
		pool.AddCert(srv.Certificate())
		d.TLS = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	t.Cleanup(srv.Close)
	d.URL = srv.URL
	return d
}

// Accepted are when each connection it let in was accepted, in order.
func (d *FrontDoor) Accepted() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Time(nil), d.accepted...)
}

// Refused is how many connections it closed as it accepted them.
func (d *FrontDoor) Refused() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.refused
}

// Requests is how many requests reached it; TooMany how many of those it
// answered 429.
func (d *FrontDoor) Requests() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.all
}

func (d *FrontDoor) TooMany() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tooMany
}

// Open is how many connections are open now, MostOpen the most there have
// been at once.
func (d *FrontDoor) Open() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.open
}

func (d *FrontDoor) MostOpen() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mostOpen
}

// MostInFlight is the most requests there have been in flight at once.
func (d *FrontDoor) MostInFlight() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mostInFlight
}

// admit says whether a connection accepted now is let in.
func (d *FrontDoor) admit() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.l.RefuseFirst > 0 {
		d.l.RefuseFirst--
		d.refused++
		return false
	}
	if d.l.ConnsPerClient > 0 && d.open >= d.l.ConnsPerClient {
		d.refused++
		return false
	}
	if !d.conns.Allow() {
		d.refused++
		return false
	}
	d.accepted = append(d.accepted, time.Now())
	d.open++
	d.mostOpen = max(d.mostOpen, d.open)
	return true
}

// closed gives an admitted connection's place back.
func (d *FrontDoor) closed() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.open--
}

// limitRequests answers a request past the rate 429, with a Retry-After,
// as plain text, as hangar-edge's own pages are.
func (d *FrontDoor) limitRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.all++
		ok := true
		if d.l.RequestsPerClient > 0 && d.inFlight >= d.l.RequestsPerClient {
			ok = false
		} else if d.l.TooManyFirst > 0 {
			d.l.TooManyFirst--
			ok = false
		} else if !d.requests.Allow() {
			ok = false
		}
		if !ok {
			d.tooMany++
		} else {
			d.inFlight++
			d.mostInFlight = max(d.mostInFlight, d.inFlight)
		}
		d.mu.Unlock()
		if !ok {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests; try again shortly", http.StatusTooManyRequests)
			return
		}
		defer func() {
			d.mu.Lock()
			d.inFlight--
			d.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// doorConn gives its place among the open connections back as it closes.
type doorConn struct {
	net.Conn
	d    *FrontDoor
	once sync.Once
}

func (c *doorConn) Close() error {
	c.once.Do(c.d.closed)
	return c.Conn.Close()
}

// doorListener closes a connection past the door's limits as soon as it
// is accepted, before any TLS: its client sees the handshake end.
type doorListener struct {
	net.Listener
	d *FrontDoor
}

func (l doorListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.d.admit() {
			return &doorConn{Conn: c, d: l.d}, nil
		}
		_ = c.Close()
	}
}
