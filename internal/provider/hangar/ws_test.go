package hangar_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangartenancy "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1/tenancyv1alpha1connect"

	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
)

// wire is an httptest server speaking hangar's machine WebSockets as
// hangar documents them (hangar: proto/hangar/compute/v1alpha1/
// machine_capability.proto), for the client's tests: what an exec does is
// its command's first word.
func wire(t *testing.T) *hangar.Connect {
	t.Helper()
	srv := httptest.NewServer(wireMux(t))
	t.Cleanup(srv.Close)
	return hangar.NewConnect(srv.URL, "p", "tok", nil)
}

// wireMux is wire's handler, and the project p's ProjectService.
func wireMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(tenancyv1alpha1connect.NewProjectServiceHandler(projectP{}))
	mux.HandleFunc("GET /v1/projects/p/containers/{nameAction}", func(w http.ResponseWriter, r *http.Request) {
		// hangar refuses as Connect's ErrorWriter does, in JSON.
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"code":"unauthenticated","message":"no token"}`)
			return
		}
		name, action, _ := strings.Cut(r.PathValue("nameAction"), ":")
		if name == "refused" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"code":"permission_denied","message":"not yours"}`)
			return
		}
		if name == "busy" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"code":"resource_exhausted","message":"too many open calls"}`)
			return
		}
		sub := map[string]string{"exec": "hangar.exec.v1alpha1", "portforward": "hangar.portforward.v1alpha1"}[action]
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{sub}})
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		// hangar's bound on a message either way (hangar: internal/execws).
		c.SetReadLimit(256 << 10)
		ctx := context.Background()
		if action == "portforward" {
			if r.URL.Query().Get("port") == "9" {
				_ = c.Close(websocket.StatusCode(4000+int(connect.CodeUnavailable)), "nothing listens on 9")
				return
			}
			for {
				typ, b, err := c.Read(ctx)
				if err != nil {
					return
				}
				if err := c.Write(ctx, typ, b); err != nil {
					return
				}
			}
		}
		read := func() *hangarcompute.MachineExecRequest {
			_, b, err := c.Read(ctx)
			if err != nil {
				return nil
			}
			m := &hangarcompute.MachineExecRequest{}
			if proto.Unmarshal(b, m) != nil {
				return nil
			}
			return m
		}
		send := func(m *hangarcompute.MachineExecResponse) {
			b, _ := proto.Marshal(m)
			_ = c.Write(ctx, websocket.MessageBinary, b)
		}
		start := read().GetStart()
		switch start.GetCommand()[0] {
		case "echo-stdin":
			for {
				m := read()
				if m == nil || m.GetStdinClose() != nil {
					break
				}
				if in := m.GetStdin(); len(in) > 0 {
					send(&hangarcompute.MachineExecResponse{Message: &hangarcompute.MachineExecResponse_Stdout{Stdout: in}})
				}
			}
			send(&hangarcompute.MachineExecResponse{Message: &hangarcompute.MachineExecResponse_Stderr{Stderr: []byte("user=" + start.GetUser())}})
			send(&hangarcompute.MachineExecResponse{Message: &hangarcompute.MachineExecResponse_Exit{Exit: &hangarcompute.MachineExecExit{Code: 3}}})
			_ = c.Close(websocket.StatusNormalClosure, "")
		case "true":
			send(&hangarcompute.MachineExecResponse{Message: &hangarcompute.MachineExecResponse_Exit{Exit: &hangarcompute.MachineExecExit{}}})
			_ = c.Close(websocket.StatusNormalClosure, "")
		case "no-exit":
			_ = c.Close(websocket.StatusNormalClosure, "")
		case "fail":
			_ = c.Close(websocket.StatusCode(4000+int(connect.CodeUnavailable)), "the node is gone")
		case "hang":
			// Until the client goes.
			for read() != nil {
			}
		}
	})
	return mux
}

// projectP is a ProjectService that knows the project p alone.
type projectP struct {
	tenancyv1alpha1connect.UnimplementedProjectServiceHandler
}

func (projectP) GetProject(_ context.Context, r *connect.Request[hangartenancy.GetProjectRequest]) (*connect.Response[hangartenancy.GetProjectResponse], error) {
	if r.Header().Get("Authorization") != "Bearer tok" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("no token"))
	}
	if r.Msg.GetName() == "slow" {
		time.Sleep(300 * time.Millisecond) //nolint:forbidigo // the server's own work, which holds the request in flight
	}
	if r.Msg.GetName() != "p" {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no such project"))
	}
	return connect.NewResponse(&hangartenancy.GetProjectResponse{Project: &hangartenancy.Project{Metadata: &hangarmeta.ObjectMeta{Name: "p"}}}), nil
}

func execIn(t *testing.T, c *hangar.Connect, ctx context.Context, name string, stdin io.Reader, command ...string) (hangar.ExecExit, string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	exit, err := c.Exec(ctx, hangar.ExecRequest{
		Plural: hangar.PluralContainers, Name: name, Start: &hangarcompute.MachineExecStart{Command: command, User: "0"},
		Stdin: stdin, Stdout: &out, Stderr: &errOut,
	})
	return exit, out.String(), errOut.String(), err
}

// TestAnExecCarriesStdinOutputAndExit: stdin goes in pieces and closes,
// output comes back on its stream, and the exit is the process's.
func TestAnExecCarriesStdinOutputAndExit(t *testing.T) {
	c := wire(t)
	big := strings.Repeat("x", 100<<10)
	exit, out, errOut, err := execIn(t, c, t.Context(), "box", strings.NewReader(big), "echo-stdin")
	if err != nil || exit.Code != 3 || out != big || errOut != "user=0" {
		t.Errorf("exec: exit %+v, %d bytes out, stderr %q, %v", exit, len(out), errOut, err)
	}
}

// TestAnExecSaysHowItFailed: a refusal before the upgrade, a close with a
// Connect code after it, and an exec that ends without its exit each come
// back as what they are.
func TestAnExecSaysHowItFailed(t *testing.T) {
	c := wire(t)
	ctx := t.Context()
	if _, _, _, err := execIn(t, c, ctx, "refused", nil, "true"); connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "not yours") {
		t.Errorf("a refusal before the upgrade: %v", err)
	}
	if _, _, _, err := execIn(t, c, ctx, "busy", nil, "true"); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("hangar's cap on open calls: %v, want Unavailable", err)
	}
	if _, _, _, err := execIn(t, c, ctx, "box", nil, "fail"); connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "node is gone") {
		t.Errorf("a close with a code: %v", err)
	}
	if _, _, _, err := execIn(t, c, ctx, "box", nil, "no-exit"); err == nil {
		t.Error("an exec that ended without its exit is no error")
	}
	if _, _, _, err := execIn(t, c, ctx, "absent", nil, "fail"); errors.Is(err, hangar.ErrNotFound) {
		t.Errorf("a close for Unavailable read as not found: %v", err)
	}
}

// TestAnExecEndsWithItsContext: a ctx ended ends an exec hangar still runs.
func TestAnExecEndsWithItsContext(t *testing.T) {
	c := wire(t)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, _, _, err := execIn(t, c, ctx, "box", nil, "hang"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("an exec whose ctx ended: %v", err)
	}
}

// TestAPortForwardIsAConnection: bytes go both ways, and a port hangar
// cannot reach closes it saying so.
func TestAPortForwardIsAConnection(t *testing.T) {
	c := wire(t)
	conn, err := c.PortForward(t.Context(), hangar.PluralContainers, "box", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != "ping" {
		t.Errorf("the forward echoed %q, %v", got, err)
	}
	_ = conn.Close()
	conn, err = c.PortForward(t.Context(), hangar.PluralContainers, "box", 9)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Read(got); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("a forward hangar closed: %v, want Unavailable", err)
	}
	if _, err := c.PortForward(t.Context(), hangar.PluralContainers, "refused", 80); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a refused forward: %v", err)
	}
}
