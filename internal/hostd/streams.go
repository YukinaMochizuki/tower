package hostd

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/ticket"
)

// Direct streams (docs/decisions/traffic.md): a client upgrades an
// HTTP/1.1 request to StreamPath<project>/<instance>/<port> with the
// ticket tower issued as its bearer token, and the connection is then the
// instance's port.
const (
	StreamPath = "/streams/"
	// StreamProtocol is the upgrade's token.
	StreamProtocol = "tower-stream/1"
)

// Streams serves direct streams. A request is refused, and told nothing of
// the instance, unless its ticket is tower's, unexpired, and for this
// target and the instance and port it names.
func (h *Host) Streams() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(StreamPath, h.serveStream)
	return mux
}

func (h *Host) serveStream(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, StreamPath), "/")
	if len(parts) != 3 {
		http.Error(w, "a stream is "+StreamPath+"<project>/<instance>/<port>", http.StatusNotFound)
		return
	}
	port, err := strconv.ParseInt(parts[2], 10, 32)
	if err != nil {
		http.Error(w, "the port is not a number", http.StatusNotFound)
		return
	}
	want := ticket.Claims{Target: h.cfg.Target, Project: parts[0], Instance: parts[1], Port: int32(port)}
	tk, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || h.cfg.TicketKey == nil {
		http.Error(w, "a stream takes a ticket tower issued, as its bearer token", http.StatusUnauthorized)
		return
	}
	if _, err := ticket.Verify(h.cfg.TicketKey, tk, want, h.clock.Now()); err != nil {
		h.log.Warn("refused a stream", "project", want.Project, "instance", want.Instance, "port", want.Port, "error", err)
		http.Error(w, "refused: "+err.Error(), http.StatusForbidden)
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), StreamProtocol) {
		w.Header().Set("Upgrade", StreamProtocol)
		http.Error(w, "a stream is an HTTP/1.1 upgrade to "+StreamProtocol, http.StatusUpgradeRequired)
		return
	}
	ctx := r.Context()
	p, err := h.running(ctx, provider.Instance{Project: want.Project, Name: want.Instance})
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, provider.ErrNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	upstream, err := p.g.Dial(ctx, p.name, want.Port)
	if err != nil {
		http.Error(w, "dialing the instance's port: "+err.Error(), http.StatusBadGateway)
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
	if _, err := io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: "+StreamProtocol+"\r\n\r\n"); err != nil {
		return
	}
	h.log.Info("a direct stream opened", "project", want.Project, "instance", want.Instance, "port", want.Port)
	ref := provider.Instance{Project: want.Project, Name: want.Instance}
	h.countStream(ref, 1)
	defer h.countStream(ref, -1)
	pipe(conn, buf.Reader, upstream)
}

// streamCount is the direct streams open to each instance: connections
// this hostd holds, which end with it.
type streamCount struct {
	mu   sync.Mutex
	open map[provider.Instance]int
}

// countStream counts a stream to the instance opening (1) or ending (-1),
// and reads the instance again to report it.
func (h *Host) countStream(ref provider.Instance, d int) {
	h.streams.mu.Lock()
	if h.streams.open == nil {
		h.streams.open = map[provider.Instance]int{}
	}
	h.streams.open[ref] += d
	if h.streams.open[ref] <= 0 {
		delete(h.streams.open, ref)
	}
	h.streams.mu.Unlock()
	h.changed(ref)
}

// streamsOpen is how many direct streams to the instance are open.
func (h *Host) streamsOpen(ref provider.Instance) int32 {
	h.streams.mu.Lock()
	defer h.streams.mu.Unlock()
	return int32(h.streams.open[ref])
}

// pipe copies both ways until either side ends; in holds what the
// client sent past its request.
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

// DialStream opens a direct stream at url, as a client does, with the
// ticket tower issued for it.
func DialStream(ctx context.Context, url, tk string) (net.Conn, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tk)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", StreamProtocol)
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
		return nil, &StreamError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// StreamError is a stream the host refused.
type StreamError struct {
	Status  int
	Message string
}

func (e *StreamError) Error() string {
	return "the stream was refused: " + strconv.Itoa(e.Status) + " " + e.Message
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
