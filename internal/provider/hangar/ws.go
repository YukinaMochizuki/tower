package hangar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
)

// hangar's machine WebSockets, as hangar documents them
// (hangar: proto/hangar/compute/v1alpha1/machine_capability.proto and
// docs/decisions/streaming-transport.md): a GET to
// /v1/projects/{project}/{plural}/{name}:{action} with a subprotocol per
// action, authenticated as a Connect call is. A refusal before the upgrade
// is a Connect error's JSON; one after it closes with 4000 plus the
// Connect code. hangar's own client is in its internal/, so this is
// written against the wire it documents.
const (
	execSubprotocol        = "hangar.exec.v1alpha1"
	portForwardSubprotocol = "hangar.portforward.v1alpha1"
	// maxExecMessage bounds a message either way, as hangar's does.
	maxExecMessage = 256 << 10
	// closeBase plus a Connect code is the close status of a WebSocket
	// failed with that code.
	closeBase = 4000
	// keepAliveEvery is how often hangar is pinged, and pongTimeout how
	// long its answer may take.
	keepAliveEvery = 30 * time.Second
	pongTimeout    = 30 * time.Second
	// stdinChunk is the most of stdin one message carries.
	stdinChunk = 32 << 10
)

// capabilityURL is the WebSocket URL of an object's action.
func (c *Connect) capabilityURL(plural, name, action string, query url.Values) string {
	u := c.endpoint + "/v1/projects/" + url.PathEscape(c.project) + "/" + url.PathEscape(plural) + "/" + url.PathEscape(name) + ":" + action
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// dial opens a machine WebSocket of the subprotocol.
func (c *Connect) dial(ctx context.Context, u, subprotocol string) (*websocket.Conn, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+c.token)
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPClient: c.hc, HTTPHeader: header, Subprotocols: []string{subprotocol},
	})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			defer func() { _ = resp.Body.Close() }()
			return nil, readRefusal(resp)
		}
		return nil, err
	}
	if conn.Subprotocol() != subprotocol {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("hangar answered with subprotocol %q, not %q", conn.Subprotocol(), subprotocol)
	}
	return conn, nil
}

// readRefusal is the Connect error a refusal before the upgrade carries.
func readRefusal(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var we struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	var code connect.Code
	if json.Unmarshal(b, &we) == nil && code.UnmarshalText([]byte(we.Code)) == nil {
		return connect.NewError(code, errors.New(we.Message))
	}
	return connect.NewError(connect.CodeUnknown, fmt.Errorf("hangar refused the WebSocket: %s", resp.Status))
}

// callCap is err, but for hangar's cap on a caller's open calls to its
// machines, which it refuses as ResourceExhausted (hangar:
// internal/apiserver/nodecalls.go): a cap that frees as calls end, said as
// Unavailable, not a quota out of room.
func callCap(err error) error {
	if connect.CodeOf(err) == connect.CodeResourceExhausted {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("hangar's cap on open calls to machines: %w", err))
	}
	return err
}

// closeError is the error a WebSocket closed with, nil for a normal
// closure.
func closeError(err error) error {
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		return err
	}
	if ce.Code == websocket.StatusNormalClosure {
		return nil
	}
	code := connect.Code(int(ce.Code) - closeBase)
	if code < connect.CodeCanceled || code > connect.CodeUnauthenticated {
		return connect.NewError(connect.CodeUnknown, fmt.Errorf("hangar closed the WebSocket with %d: %s", ce.Code, ce.Reason))
	}
	return connect.NewError(code, errors.New(ce.Reason))
}

func (c *Connect) Exec(ctx context.Context, req ExecRequest) (ExecExit, error) {
	conn, err := c.dial(ctx, c.capabilityURL(req.Plural, req.Name, "exec", nil), execSubprotocol)
	if err != nil {
		return ExecExit{}, apiError("exec in "+req.Name, callCap(err))
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(maxExecMessage)
	// Reads and writes are not given ctx: coder/websocket closes a
	// connection whose read or write is cancelled. Closing it on ctx's end
	// ends them instead, and hangar then kills what the exec runs.
	ws := context.WithoutCancel(ctx)
	stop := context.AfterFunc(ctx, func() { _ = conn.CloseNow() })
	defer stop()
	send := func(m *hangarcompute.MachineExecRequest) error {
		b, err := proto.Marshal(m)
		if err != nil {
			return err
		}
		return conn.Write(ws, websocket.MessageBinary, b)
	}
	start := proto.Clone(req.Start).(*hangarcompute.MachineExecStart)
	start.Stdin = req.Stdin != nil
	if err := send(&hangarcompute.MachineExecRequest{Message: &hangarcompute.MachineExecRequest_Start{Start: start}}); err != nil {
		return ExecExit{}, err
	}
	done := make(chan struct{})
	defer close(done)
	go keepAlive(conn, done)
	if req.Stdin != nil {
		go func() {
			buf := make([]byte, stdinChunk)
			for {
				n, err := req.Stdin.Read(buf)
				if n > 0 {
					if send(&hangarcompute.MachineExecRequest{Message: &hangarcompute.MachineExecRequest_Stdin{Stdin: buf[:n]}}) != nil {
						return
					}
				}
				if errors.Is(err, io.EOF) {
					_ = send(&hangarcompute.MachineExecRequest{Message: &hangarcompute.MachineExecRequest_StdinClose{StdinClose: &hangarcompute.MachineExecStdinClose{}}})
					return
				}
				if err != nil {
					// What feeds stdin failed: the exec cannot go on as asked.
					_ = conn.Close(websocket.StatusNormalClosure, "")
					return
				}
			}
		}()
	}
	stdout, stderr := orDiscard(req.Stdout), orDiscard(req.Stderr)
	var exit *hangarcompute.MachineExecExit
	for {
		_, b, err := conn.Read(ws)
		if err != nil {
			if ctx.Err() != nil {
				return ExecExit{}, ctx.Err()
			}
			if err := closeError(err); err != nil {
				return ExecExit{}, apiError("exec in "+req.Name, err)
			}
			if exit == nil {
				return ExecExit{}, fmt.Errorf("exec in %s: it ended without saying how its process did", req.Name)
			}
			return ExecExit{Code: int(exit.GetCode()), Signal: int(exit.GetSignal())}, nil
		}
		res := &hangarcompute.MachineExecResponse{}
		if err := proto.Unmarshal(b, res); err != nil {
			return ExecExit{}, fmt.Errorf("exec in %s: a message that is not one: %w", req.Name, err)
		}
		switch m := res.GetMessage().(type) {
		case *hangarcompute.MachineExecResponse_Stdout:
			_, err = stdout.Write(m.Stdout)
		case *hangarcompute.MachineExecResponse_Stderr:
			_, err = stderr.Write(m.Stderr)
		case *hangarcompute.MachineExecResponse_Exit:
			exit = m.Exit
		}
		if err != nil {
			// Whoever reads the output is gone: the exec is no use.
			return ExecExit{}, err
		}
	}
}

// keepAlive pings hangar every keepAliveEvery until done, as hangar pings
// its clients: hangar takes a WebSocket that says nothing for long as
// gone, and one whose ping goes unanswered for pongTimeout is taken as gone
// here, closed, which ends what reads or writes it. A ping is answered
// only while something reads the connection, as the exec's loop and a
// relayed stream do.
func keepAlive(conn *websocket.Conn, done <-chan struct{}) {
	t := time.NewTicker(keepAliveEvery)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), pongTimeout)
		err := conn.Ping(ctx)
		cancel()
		if err != nil {
			_ = conn.CloseNow()
			return
		}
	}
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

func (c *Connect) PortForward(ctx context.Context, plural, name string, port int32) (net.Conn, error) {
	u := c.capabilityURL(plural, name, "portforward", url.Values{"port": {strconv.Itoa(int(port))}})
	conn, err := c.dial(ctx, u, portForwardSubprotocol)
	if err != nil {
		return nil, apiError(fmt.Sprintf("port %d of %s", port, name), callCap(err))
	}
	conn.SetReadLimit(maxExecMessage)
	done := make(chan struct{})
	go keepAlive(conn, done)
	// The connection outlives the dial's ctx: it ends as it is closed.
	return &forwardConn{Conn: websocket.NetConn(context.Background(), conn, websocket.MessageBinary), done: done}, nil
}

// forwardConn is a port forward's WebSocket as a connection, whose read
// error says why hangar closed it. A WebSocket cannot half-close: its
// CloseWrite, which a relay calls once its client is done sending, is not
// there, and the relay closes it whole.
type forwardConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (f *forwardConn) Close() error {
	f.once.Do(func() { close(f.done) })
	return f.Conn.Close()
}

func (f *forwardConn) Read(p []byte) (int, error) {
	n, err := f.Conn.Read(p)
	var ce websocket.CloseError
	if err != nil && errors.As(err, &ce) {
		if err = closeError(err); err == nil {
			err = io.EOF
		}
	}
	return n, err
}
