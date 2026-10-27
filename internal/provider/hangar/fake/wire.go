package fake

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"

	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
)

// Wire serves the fake's machine WebSockets, exec and port forward, as
// hangar's API does (hangar: proto/hangar/compute/v1alpha1/
// machine_capability.proto), to a caller with token: so a hangar.Connect
// reaches the fake's machines over the network, through what stands
// between, such as a front door.
func (f *Fake) Wire(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/projects/{project}/{plural}/{nameAction}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			refuse(w, http.StatusUnauthorized, connect.CodeUnauthenticated, "no token, or not the project's")
			return
		}
		if r.PathValue("project") != Project {
			refuse(w, http.StatusNotFound, connect.CodeNotFound, "project "+r.PathValue("project")+" not found")
			return
		}
		plural := r.PathValue("plural")
		name, action, _ := strings.Cut(r.PathValue("nameAction"), ":")
		switch action {
		case "exec":
			f.serveExec(w, r, plural, name)
		case "portforward":
			port, err := strconv.Atoi(r.URL.Query().Get("port"))
			if err != nil {
				refuse(w, http.StatusBadRequest, connect.CodeInvalidArgument, "port: not a number")
				return
			}
			f.serveForward(w, r, plural, name, int32(port))
		default:
			refuse(w, http.StatusNotFound, connect.CodeNotFound, "no action "+action)
		}
	})
	return mux
}

// refuse answers before the upgrade with a Connect error's JSON.
func refuse(w http.ResponseWriter, status int, code connect.Code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := code.MarshalText()
	_, _ = io.WriteString(w, `{"code":"`+string(b)+`","message":"`+strings.ReplaceAll(msg, `"`, `'`)+`"}`)
}

// closeWith closes c as hangar closes a WebSocket that failed: 4000 plus
// err's Connect code.
func closeWith(c *websocket.Conn, err error) {
	code := connect.CodeOf(err)
	if errors.Is(err, hangar.ErrNotFound) {
		code = connect.CodeNotFound
	}
	reason := err.Error()
	// A close frame's reason holds 123 bytes.
	if len(reason) > 120 {
		reason = reason[:120]
	}
	_ = c.Close(websocket.StatusCode(4000+int(code)), reason)
}

// serveExec runs one exec in the fake, its stdin, output and exit carried
// as hangar's MachineExec messages.
func (f *Fake) serveExec(w http.ResponseWriter, r *http.Request, plural, name string) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"hangar.exec.v1alpha1"}})
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(256 << 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		return
	}
	first := &hangarcompute.MachineExecRequest{}
	if err := proto.Unmarshal(b, first); err != nil || first.GetStart() == nil {
		_ = c.Close(websocket.StatusCode(4000+int(connect.CodeInvalidArgument)), "the first message is not a start")
		return
	}
	start := first.GetStart()
	var wmu sync.Mutex
	send := func(m *hangarcompute.MachineExecResponse) error {
		b, err := proto.Marshal(m)
		if err != nil {
			return err
		}
		wmu.Lock()
		defer wmu.Unlock()
		return c.Write(ctx, websocket.MessageBinary, b)
	}
	stdinR, stdinW := io.Pipe()
	defer func() { _ = stdinR.CloseWithError(io.ErrClosedPipe) }()
	// What the client sends until it goes: its stdin, whose end, or the
	// client's, ends the exec's stdin, and the client gone ends the exec.
	go func() {
		for {
			_, b, err := c.Read(ctx)
			if err != nil {
				cancel()
				_ = stdinW.CloseWithError(err)
				return
			}
			m := &hangarcompute.MachineExecRequest{}
			if proto.Unmarshal(b, m) != nil {
				continue
			}
			switch {
			case m.GetStdinClose() != nil:
				_ = stdinW.Close()
			case len(m.GetStdin()) > 0:
				if _, err := stdinW.Write(m.GetStdin()); err != nil {
					// The exec read no more of it: the rest is dropped.
					continue
				}
			}
		}
	}()
	var stdin io.Reader
	if start.GetStdin() {
		stdin = stdinR
	}
	exit, err := f.Exec(ctx, hangar.ExecRequest{
		Plural: plural, Name: name, Start: start, Stdin: stdin,
		Stdout: writerFunc(func(p []byte) error {
			return send(&hangarcompute.MachineExecResponse{Message: &hangarcompute.MachineExecResponse_Stdout{Stdout: p}})
		}),
		Stderr: writerFunc(func(p []byte) error {
			return send(&hangarcompute.MachineExecResponse{Message: &hangarcompute.MachineExecResponse_Stderr{Stderr: p}})
		}),
	})
	// Stdin the exec left unread no longer holds up the reads, which the
	// close below needs.
	_ = stdinR.CloseWithError(io.ErrClosedPipe)
	if err != nil {
		closeWith(c, err)
		return
	}
	_ = send(&hangarcompute.MachineExecResponse{Message: &hangarcompute.MachineExecResponse_Exit{Exit: &hangarcompute.MachineExecExit{
		Code: int32(exit.Code), Signal: int32(exit.Signal),
	}}})
	_ = c.Close(websocket.StatusNormalClosure, "")
}

// serveForward relays one port forward of the fake's to the client.
func (f *Fake) serveForward(w http.ResponseWriter, r *http.Request, plural, name string, port int32) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"hangar.portforward.v1alpha1"}})
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(256 << 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, err := f.PortForward(ctx, plural, name, port)
	if err != nil {
		closeWith(c, err)
		return
	}
	defer func() { _ = conn.Close() }()
	ws := websocket.NetConn(ctx, c, websocket.MessageBinary)
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(conn, ws); done <- struct{}{} }()
	go func() { _, _ = io.Copy(ws, conn); done <- struct{}{} }()
	<-done
}

// writerFunc is a Writer that hands each write to its function.
type writerFunc func(p []byte) error

func (w writerFunc) Write(p []byte) (int, error) {
	if err := w(p); err != nil {
		return 0, err
	}
	return len(p), nil
}
