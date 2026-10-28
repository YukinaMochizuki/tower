package hostd

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hostdv1 "github.com/YukinaMochizuki/tower/gen/go/tower/hostd/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/hostd/v1/hostdv1connect"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// chunkSize is the most a message carries of a stream's bytes: output,
// stdin, a file or a build context.
const chunkSize = 64 << 10

// MaxMessage bounds a HostService message either way.
const MaxMessage = 1 << 20

// NewHandler serves HostService over p, the provider of the host's one
// target: what hostd serves tower on its tunnel.
func NewHandler(p provider.Provider) (string, http.Handler) {
	return hostdv1connect.NewHostServiceHandler(service{p: p}, connect.WithReadMaxBytes(MaxMessage))
}

type service struct {
	p provider.Provider
}

func instanceOf(in *hostdv1.Instance) provider.Instance {
	return provider.Instance{Project: in.GetProject(), Name: in.GetName()}
}

// chunks is a writer that sends what is written in pieces of at most
// chunkSize, each a message it sends.
type chunks func(b []byte) error

func (c chunks) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		k := min(len(p), chunkSize)
		if err := c(append([]byte(nil), p[:k]...)); err != nil {
			return n, err
		}
		n, p = n+k, p[k:]
	}
	return n, nil
}

// received reads the bytes of a stream's later messages, each taken by
// next; next returns io.EOF once they end.
type received struct {
	next func() ([]byte, error)
	buf  []byte
}

func (r *received) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		b, err := r.next()
		if err != nil {
			return 0, err
		}
		r.buf = b
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (s service) Exec(ctx context.Context, stream *connect.BidiStream[hostdv1.ExecRequest, hostdv1.ExecResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("an exec's first request starts it"))
	}
	var mu sync.Mutex
	send := func(r *hostdv1.ExecResponse) error {
		mu.Lock()
		defer mu.Unlock()
		return stream.Send(r)
	}
	req := provider.ExecRequest{
		Instance: instanceOf(start.GetInstance()), Command: start.GetCommand(), Env: start.GetEnv(), Dir: start.GetDir(),
		Stdout: chunks(func(b []byte) error {
			return send(&hostdv1.ExecResponse{Response: &hostdv1.ExecResponse_Stdout{Stdout: b}})
		}),
		Stderr: chunks(func(b []byte) error {
			return send(&hostdv1.ExecResponse{Response: &hostdv1.ExecResponse_Stderr{Stderr: b}})
		}),
	}
	if start.GetStdin() {
		req.Stdin = &received{next: func() ([]byte, error) {
			m, err := stream.Receive()
			if err != nil {
				return nil, err
			}
			return m.GetStdin(), nil
		}}
	}
	code, err := s.p.Exec(ctx, req)
	if err != nil {
		return toConnect(err)
	}
	return send(&hostdv1.ExecResponse{Response: &hostdv1.ExecResponse_ExitCode{ExitCode: int32(code)}})
}

func (s service) ReadFile(ctx context.Context, req *connect.Request[hostdv1.ReadFileRequest], stream *connect.ServerStream[hostdv1.ReadFileResponse]) error {
	w := chunks(func(b []byte) error { return stream.Send(&hostdv1.ReadFileResponse{Data: b}) })
	return toConnect(s.p.ReadFile(ctx, instanceOf(req.Msg.GetInstance()), req.Msg.GetPath(), w))
}

func (s service) WriteFile(ctx context.Context, stream *connect.ClientStream[hostdv1.WriteFileRequest]) (*connect.Response[hostdv1.WriteFileResponse], error) {
	var start *hostdv1.WriteFileStart
	if stream.Receive() {
		start = stream.Msg().GetStart()
	} else if err := stream.Err(); err != nil {
		return nil, err
	}
	if start == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a write's first request says where it goes"))
	}
	r := &received{next: func() ([]byte, error) {
		if !stream.Receive() {
			return nil, receiveError(stream.Err())
		}
		return stream.Msg().GetData(), nil
	}}
	err := s.p.WriteFile(ctx, instanceOf(start.GetInstance()), start.GetPath(), fs.FileMode(start.GetMode()).Perm(), start.GetOffset(), r)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&hostdv1.WriteFileResponse{}), nil
}

// receiveError is what a client stream's end is read as: io.EOF when it
// ended with its last message.
func receiveError(err error) error {
	if err == nil {
		return io.EOF
	}
	return err
}

func (s service) Copy(ctx context.Context, req *connect.Request[hostdv1.CopyRequest]) (*connect.Response[hostdv1.CopyResponse], error) {
	m := req.Msg
	if err := s.p.Copy(ctx, instanceOf(m.GetFrom()), m.GetFromPath(), instanceOf(m.GetTo()), m.GetToPath()); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&hostdv1.CopyResponse{}), nil
}

func (s service) BuildImage(ctx context.Context, stream *connect.ClientStream[hostdv1.BuildImageRequest]) (*connect.Response[hostdv1.BuildImageResponse], error) {
	var start *hostdv1.BuildImageStart
	if stream.Receive() {
		start = stream.Msg().GetStart()
	} else if err := stream.Err(); err != nil {
		return nil, err
	}
	if start == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a build's first request says where its image goes"))
	}
	req := provider.BuildRequest{Repository: start.GetRepository(), Dir: start.GetDir(), Dockerfile: start.GetDockerfile()}
	if m := start.GetMirror(); m != nil {
		req.Mirror = &provider.MirrorSource{Mirror: m.GetMirror(), Ref: m.GetRef()}
	} else {
		req.Context = &received{next: func() ([]byte, error) {
			if !stream.Receive() {
				return nil, receiveError(stream.Err())
			}
			return stream.Msg().GetContext(), nil
		}}
	}
	image, err := s.p.BuildImage(ctx, req)
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&hostdv1.BuildImageResponse{Image: image}), nil
}

func (s service) Capture(ctx context.Context, req *connect.Request[hostdv1.CaptureRequest]) (*connect.Response[hostdv1.CaptureResponse], error) {
	a, err := s.p.Capture(ctx, instanceOf(req.Msg.GetInstance()))
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&hostdv1.CaptureResponse{Artifact: a.Name, Size: a.Size}), nil
}

func (s service) DeleteArtifact(ctx context.Context, req *connect.Request[hostdv1.DeleteArtifactRequest]) (*connect.Response[hostdv1.DeleteArtifactResponse], error) {
	if err := s.p.DeleteArtifact(ctx, req.Msg.GetArtifact()); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&hostdv1.DeleteArtifactResponse{}), nil
}

func (s service) Fetch(ctx context.Context, req *connect.Request[hostdv1.FetchRequest]) (*connect.Response[hostdv1.FetchResponse], error) {
	m := req.Msg
	head, err := s.p.Fetch(ctx, provider.FetchRequest{Mirror: m.GetMirror(), URL: m.GetUrl(), Token: m.GetToken(), Branch: m.GetBranch()})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&hostdv1.FetchResponse{Head: head}), nil
}

func (s service) Checkout(ctx context.Context, req *connect.Request[hostdv1.CheckoutRequest]) (*connect.Response[hostdv1.CheckoutResponse], error) {
	m := req.Msg
	commit, err := s.p.Checkout(ctx, instanceOf(m.GetInstance()), m.GetMirror(), m.GetRef(), m.GetDir())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&hostdv1.CheckoutResponse{Commit: commit}), nil
}

func (s service) UpdateOrigin(ctx context.Context, req *connect.Request[hostdv1.UpdateOriginRequest]) (*connect.Response[hostdv1.UpdateOriginResponse], error) {
	m := req.Msg
	if err := s.p.UpdateOrigin(ctx, instanceOf(m.GetInstance()), m.GetMirror(), m.GetDir()); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&hostdv1.UpdateOriginResponse{}), nil
}

func (s service) CommitsBehind(ctx context.Context, req *connect.Request[hostdv1.CommitsBehindRequest]) (*connect.Response[hostdv1.CommitsBehindResponse], error) {
	m := req.Msg
	n, err := s.p.CommitsBehind(ctx, m.GetMirror(), m.GetCommit(), m.GetBranch())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&hostdv1.CommitsBehindResponse{Commits: int32(n)}), nil
}

func (s service) ResolveImage(ctx context.Context, req *connect.Request[hostdv1.ResolveImageRequest]) (*connect.Response[hostdv1.ResolveImageResponse], error) {
	cfg, err := s.p.ResolveImage(ctx, req.Msg.GetRef())
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&hostdv1.ResolveImageResponse{Digest: cfg.Digest, User: cfg.User, Env: cfg.Env}), nil
}

func (s service) ReadMirror(ctx context.Context, req *connect.Request[hostdv1.ReadMirrorRequest]) (*connect.Response[hostdv1.ReadMirrorResponse], error) {
	m := req.Msg
	read, err := s.p.ReadMirror(ctx, provider.MirrorReadRequest{
		MirrorSource: provider.MirrorSource{Mirror: m.GetSource().GetMirror(), Ref: m.GetSource().GetRef()}, Paths: m.GetPaths(), Content: m.GetContent(),
	})
	if err != nil {
		return nil, toConnect(err)
	}
	res := &hostdv1.ReadMirrorResponse{Commit: read.Commit, Entries: map[string]*hostdv1.MirrorEntry{}}
	for p, e := range read.Entries {
		res.Entries[p] = &hostdv1.MirrorEntry{Object: e.Object, Dir: e.Dir, Size: e.Size, Content: e.Content}
	}
	return connect.NewResponse(res), nil
}

func (s service) Push(ctx context.Context, req *connect.Request[hostdv1.PushRequest]) (*connect.Response[hostdv1.PushResponse], error) {
	m := req.Msg
	err := s.p.Push(ctx, provider.PushRequest{
		Instance: instanceOf(m.GetInstance()), Dir: m.GetDir(), Mirror: m.GetMirror(), URL: m.GetUrl(),
		Token: m.GetToken(), Branch: m.GetBranch(), Lease: m.GetLease(),
	})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&hostdv1.PushResponse{}), nil
}

func (s service) Ticket(ctx context.Context, req *connect.Request[hostdv1.TicketRequest]) (*connect.Response[hostdv1.TicketResponse], error) {
	tk, err := s.p.Ticket(ctx, instanceOf(req.Msg.GetInstance()), req.Msg.GetPort())
	if err != nil {
		return nil, toConnect(err)
	}
	res := &hostdv1.TicketResponse{Url: tk.URL}
	if !tk.Expires.IsZero() {
		res.ExpireTime = timestamppb.New(tk.Expires)
	}
	return connect.NewResponse(res), nil
}
