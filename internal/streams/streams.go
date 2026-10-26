// Package streams is the direct stream of docs/decisions/traffic.md: a
// client upgrades an HTTP/1.1 request to <prefix><project>/<instance>/<port>
// with the ticket tower issued (internal/ticket) as its bearer token, and
// the connection is then the instance's port. hostd serves it on its host;
// tower serves it for a hangar target, relaying each stream to hangar's
// port forward. Dial is the client's side.
package streams

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/ticket"
)

// Protocol is the upgrade's token.
const Protocol = "tower-stream/1"

// Server serves the direct streams of one target.
type Server struct {
	// Target is the target whose tickets it takes.
	Target string
	// Key is tower's public key, which tickets are verified with; nil
	// refuses every stream.
	Key ed25519.PublicKey
	// Prefix is the path streams are served under, ending in "/".
	Prefix string
	// Dial connects to a port of a running instance. An instance not there
	// is provider.ErrNotFound, one not running provider.ErrFailed, and a
	// target that does not answer provider.ErrUnavailable; any other error
	// is the port's.
	Dial func(ctx context.Context, in provider.Instance, port int32) (net.Conn, error)
	// Changed, if set, hears of a stream to an instance opening or ending.
	Changed func(in provider.Instance)
	Clock   clockwork.Clock
	Log     *slog.Logger

	mu   sync.Mutex
	open map[provider.Instance]int
}

// ServeHTTP serves a stream. A request is refused, and told nothing of the
// instance, unless its ticket is tower's, unexpired, and for this target
// and the instance and port it names.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, s.Prefix)
	parts := strings.Split(rest, "/")
	if !ok || len(parts) != 3 {
		http.Error(w, "a stream is "+s.Prefix+"<project>/<instance>/<port>", http.StatusNotFound)
		return
	}
	port, err := strconv.ParseInt(parts[2], 10, 32)
	if err != nil {
		http.Error(w, "the port is not a number", http.StatusNotFound)
		return
	}
	want := ticket.Claims{Target: s.Target, Project: parts[0], Instance: parts[1], Port: int32(port)}
	tk, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || s.Key == nil {
		http.Error(w, "a stream takes a ticket tower issued, as its bearer token", http.StatusUnauthorized)
		return
	}
	if _, err := ticket.Verify(s.Key, tk, want, s.clock().Now()); err != nil {
		s.log().Warn("refused a stream", "target", s.Target, "project", want.Project, "instance", want.Instance, "port", want.Port, "error", err)
		http.Error(w, "refused: "+err.Error(), http.StatusForbidden)
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), Protocol) {
		w.Header().Set("Upgrade", Protocol)
		http.Error(w, "a stream is an HTTP/1.1 upgrade to "+Protocol, http.StatusUpgradeRequired)
		return
	}
	ref := provider.Instance{Project: want.Project, Name: want.Instance}
	upstream, err := s.Dial(r.Context(), ref, want.Port)
	if err != nil {
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, provider.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, provider.ErrUnavailable), errors.Is(err, provider.ErrFailed):
			status = http.StatusServiceUnavailable
		}
		http.Error(w, "dialing the instance's port: "+err.Error(), status)
		return
	}
	defer func() { _ = upstream.Close() }()
	conn, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "the connection cannot be taken over", http.StatusInternalServerError)
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Time{})
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+Protocol+"\r\n\r\n"); err != nil {
		return
	}
	s.log().Info("a direct stream opened", "target", s.Target, "project", want.Project, "instance", want.Instance, "port", want.Port)
	s.count(ref, 1)
	defer s.count(ref, -1)
	pipe(conn, buf.Reader, upstream)
}

func (s *Server) clock() clockwork.Clock {
	if s.Clock == nil {
		return clockwork.NewRealClock()
	}
	return s.Clock
}

func (s *Server) log() *slog.Logger {
	if s.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Log
}

// count counts a stream to the instance opening (1) or ending (-1).
func (s *Server) count(ref provider.Instance, d int) {
	s.mu.Lock()
	if s.open == nil {
		s.open = map[provider.Instance]int{}
	}
	s.open[ref] += d
	if s.open[ref] <= 0 {
		delete(s.open, ref)
	}
	s.mu.Unlock()
	if s.Changed != nil {
		s.Changed(ref)
	}
}

// Open is how many streams to the instance are open: connections this
// server holds, which end with it.
func (s *Server) Open(ref provider.Instance) int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int32(s.open[ref])
}

// pipe copies both ways until either side ends; in holds what the client
// sent past its request.
func pipe(client net.Conn, in *bufio.Reader, upstream net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, in)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		closeWrite(client)
	}()
	wg.Wait()
}

// closeWrite ends what is sent on c, or c itself if it cannot half-close.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// Dial opens a direct stream at url, as a client does, with the ticket
// tower issued for it.
func Dial(ctx context.Context, url, tk string) (net.Conn, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tk)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", Protocol)
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", req.URL.Host)
	if err != nil {
		return nil, err
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, &Error{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// Error is a stream refused.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return "the stream was refused: " + strconv.Itoa(e.Status) + " " + e.Message
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
