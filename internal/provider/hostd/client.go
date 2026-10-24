// Package hostd is tower's provider of a hostd target, and what serves its
// calls on the host. Client implements provider.Provider by calling
// tower.hostd.v1.HostService over the tunnel the target's host opened
// (internal/tunnel); NewHandler serves HostService over any provider, as
// hostd does on its host and tests do over a fake. Errors cross the tunnel
// as the provider's own error values. Who may open a tunnel, and which
// target it is, is the apiserver's to decide; what runs on the host is
// hostd's. See docs/decisions/traffic.md.
package hostd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"connectrpc.com/connect"

	hostdv1 "github.com/YukinaMochizuki/tower/gen/go/tower/hostd/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/hostd/v1/hostdv1connect"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/tunnel"
)

// Client is the provider of one hostd target.
type Client struct {
	Target  string
	Tunnels *tunnel.Hub
	// Calls caps the calls open, shared by every target's client; nil caps
	// nothing.
	Calls *Calls
}

var _ provider.Provider = (*Client)(nil)

// call is one call under way to the host, holding its slot.
type call struct {
	host    hostdv1connect.HostServiceClient
	tunnel  *tunnel.Tunnel
	target  string
	release func()
}

// begin begins a call by ctx's caller about in, zero for a call about no
// instance. With no tunnel, it is ErrUnavailable.
func (c *Client) begin(ctx context.Context, in provider.Instance) (*call, error) {
	t, err := c.Tunnels.Node(c.Target)
	if errors.Is(err, tunnel.ErrNoTunnel) {
		return nil, fmt.Errorf("target %s: hostd is not connected: %w", c.Target, provider.ErrUnavailable)
	}
	if err != nil {
		return nil, err
	}
	p, ok := auth.FromContext(ctx)
	if !ok {
		p = auth.SystemPrincipal
	}
	release, err := c.Calls.take(p, in, c.Target)
	if err != nil {
		return nil, err
	}
	return &call{
		host:   hostdv1connect.NewHostServiceClient(t.HTTPClient(), "http://"+c.Target, connect.WithReadMaxBytes(MaxMessage)),
		tunnel: t, target: c.Target, release: release,
	}, nil
}

// failed is the error a call ends with when the host's answer is err: a
// tunnel that dropped under it is ErrUnavailable, a call its caller ended
// is ctx's error, and the host's own is the provider's error it stands for.
func (k *call) failed(ctx context.Context, err error) error {
	if k.tunnel.Closed() {
		return fmt.Errorf("target %s: the tunnel from hostd dropped: %w", k.target, provider.ErrUnavailable)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fromConnect(err)
}

func instance(in provider.Instance) *hostdv1.Instance {
	return &hostdv1.Instance{Project: in.Project, Name: in.Name}
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// sendAll sends what r holds, a piece at most chunkSize at a time, until it
// ends or the host stops taking it. It returns r's error; the host's comes
// with the call's answer.
func sendAll(r io.Reader, send func([]byte) error) error {
	buf := make([]byte, chunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if serr := send(append([]byte(nil), buf[:n]...)); serr != nil {
				// The host's error comes with the call's answer.
				return nil //nolint:nilerr // see above
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (c *Client) Exec(ctx context.Context, req provider.ExecRequest) (int, error) {
	k, err := c.begin(ctx, req.Instance)
	if err != nil {
		return 0, err
	}
	defer k.release()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := k.host.Exec(ctx)
	start := &hostdv1.ExecStart{Instance: instance(req.Instance), Command: req.Command, Env: req.Env, Dir: req.Dir, Stdin: req.Stdin != nil}
	// A send the host refused is told by the receive.
	_ = stream.Send(&hostdv1.ExecRequest{Request: &hostdv1.ExecRequest_Start{Start: start}})
	if req.Stdin == nil {
		_ = stream.CloseRequest()
	} else {
		go func() {
			_ = sendAll(req.Stdin, func(b []byte) error {
				return stream.Send(&hostdv1.ExecRequest{Request: &hostdv1.ExecRequest_Stdin{Stdin: b}})
			})
			_ = stream.CloseRequest()
		}()
	}
	defer func() { _ = stream.CloseResponse() }()
	stdout, stderr := orDiscard(req.Stdout), orDiscard(req.Stderr)
	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return 0, fmt.Errorf("target %s: the host ended the command without saying how: %w", c.Target, provider.ErrUnavailable)
		}
		if err != nil {
			return 0, k.failed(ctx, err)
		}
		switch r := msg.GetResponse().(type) {
		case *hostdv1.ExecResponse_Stdout:
			if _, err := stdout.Write(r.Stdout); err != nil {
				return 0, err
			}
		case *hostdv1.ExecResponse_Stderr:
			if _, err := stderr.Write(r.Stderr); err != nil {
				return 0, err
			}
		case *hostdv1.ExecResponse_ExitCode:
			return int(r.ExitCode), nil
		}
	}
}

func (c *Client) ReadFile(ctx context.Context, in provider.Instance, path string, w io.Writer) error {
	k, err := c.begin(ctx, in)
	if err != nil {
		return err
	}
	defer k.release()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := k.host.ReadFile(ctx, connect.NewRequest(&hostdv1.ReadFileRequest{Instance: instance(in), Path: path}))
	if err != nil {
		return k.failed(ctx, err)
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() {
		if _, err := w.Write(stream.Msg().GetData()); err != nil {
			return err
		}
	}
	if err := stream.Err(); err != nil {
		return k.failed(ctx, err)
	}
	return nil
}

func (c *Client) WriteFile(ctx context.Context, in provider.Instance, path string, mode fs.FileMode, offset int64, r io.Reader) error {
	k, err := c.begin(ctx, in)
	if err != nil {
		return err
	}
	defer k.release()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := k.host.WriteFile(ctx)
	start := &hostdv1.WriteFileStart{Instance: instance(in), Path: path, Mode: uint32(mode.Perm()), Offset: offset}
	if stream.Send(&hostdv1.WriteFileRequest{Request: &hostdv1.WriteFileRequest_Start{Start: start}}) == nil {
		if err := sendAll(r, func(b []byte) error {
			return stream.Send(&hostdv1.WriteFileRequest{Request: &hostdv1.WriteFileRequest_Data{Data: b}})
		}); err != nil {
			return err
		}
	}
	if _, err := stream.CloseAndReceive(); err != nil {
		return k.failed(ctx, err)
	}
	return nil
}

func (c *Client) Copy(ctx context.Context, from provider.Instance, fromPath string, to provider.Instance, toPath string) error {
	k, err := c.begin(ctx, from)
	if err != nil {
		return err
	}
	defer k.release()
	_, err = k.host.Copy(ctx, connect.NewRequest(&hostdv1.CopyRequest{From: instance(from), FromPath: fromPath, To: instance(to), ToPath: toPath}))
	if err != nil {
		return k.failed(ctx, err)
	}
	return nil
}

func (c *Client) BuildImage(ctx context.Context, req provider.BuildRequest) (string, error) {
	k, err := c.begin(ctx, provider.Instance{})
	if err != nil {
		return "", err
	}
	defer k.release()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := k.host.BuildImage(ctx)
	start := &hostdv1.BuildImageStart{Repository: req.Repository}
	if stream.Send(&hostdv1.BuildImageRequest{Request: &hostdv1.BuildImageRequest_Start{Start: start}}) == nil {
		if err := sendAll(req.Context, func(b []byte) error {
			return stream.Send(&hostdv1.BuildImageRequest{Request: &hostdv1.BuildImageRequest_Context{Context: b}})
		}); err != nil {
			return "", err
		}
	}
	res, err := stream.CloseAndReceive()
	if err != nil {
		return "", k.failed(ctx, err)
	}
	return res.Msg.GetImage(), nil
}

func (c *Client) Capture(ctx context.Context, in provider.Instance) (provider.Artifact, error) {
	k, err := c.begin(ctx, in)
	if err != nil {
		return provider.Artifact{}, err
	}
	defer k.release()
	res, err := k.host.Capture(ctx, connect.NewRequest(&hostdv1.CaptureRequest{Instance: instance(in)}))
	if err != nil {
		return provider.Artifact{}, k.failed(ctx, err)
	}
	return provider.Artifact{Name: res.Msg.GetArtifact(), Size: res.Msg.GetSize()}, nil
}

func (c *Client) DeleteArtifact(ctx context.Context, artifact string) error {
	k, err := c.begin(ctx, provider.Instance{})
	if err != nil {
		return err
	}
	defer k.release()
	if _, err := k.host.DeleteArtifact(ctx, connect.NewRequest(&hostdv1.DeleteArtifactRequest{Artifact: artifact})); err != nil {
		return k.failed(ctx, err)
	}
	return nil
}

func (c *Client) Fetch(ctx context.Context, req provider.FetchRequest) (string, error) {
	k, err := c.begin(ctx, provider.Instance{})
	if err != nil {
		return "", err
	}
	defer k.release()
	res, err := k.host.Fetch(ctx, connect.NewRequest(&hostdv1.FetchRequest{Mirror: req.Mirror, Url: req.URL, Token: req.Token, Branch: req.Branch}))
	if err != nil {
		return "", k.failed(ctx, err)
	}
	return res.Msg.GetHead(), nil
}

func (c *Client) Checkout(ctx context.Context, in provider.Instance, mirror, ref, dir string) (string, error) {
	k, err := c.begin(ctx, in)
	if err != nil {
		return "", err
	}
	defer k.release()
	res, err := k.host.Checkout(ctx, connect.NewRequest(&hostdv1.CheckoutRequest{Instance: instance(in), Mirror: mirror, Ref: ref, Dir: dir}))
	if err != nil {
		return "", k.failed(ctx, err)
	}
	return res.Msg.GetCommit(), nil
}

func (c *Client) UpdateOrigin(ctx context.Context, in provider.Instance, mirror, dir string) error {
	k, err := c.begin(ctx, in)
	if err != nil {
		return err
	}
	defer k.release()
	if _, err := k.host.UpdateOrigin(ctx, connect.NewRequest(&hostdv1.UpdateOriginRequest{Instance: instance(in), Mirror: mirror, Dir: dir})); err != nil {
		return k.failed(ctx, err)
	}
	return nil
}

func (c *Client) CommitsBehind(ctx context.Context, mirror, commit, branch string) (int, error) {
	k, err := c.begin(ctx, provider.Instance{})
	if err != nil {
		return 0, err
	}
	defer k.release()
	res, err := k.host.CommitsBehind(ctx, connect.NewRequest(&hostdv1.CommitsBehindRequest{Mirror: mirror, Commit: commit, Branch: branch}))
	if err != nil {
		return 0, k.failed(ctx, err)
	}
	return int(res.Msg.GetCommits()), nil
}

func (c *Client) Push(ctx context.Context, req provider.PushRequest) error {
	k, err := c.begin(ctx, req.Instance)
	if err != nil {
		return err
	}
	defer k.release()
	_, err = k.host.Push(ctx, connect.NewRequest(&hostdv1.PushRequest{
		Instance: instance(req.Instance), Dir: req.Dir, Mirror: req.Mirror, Url: req.URL,
		Token: req.Token, Branch: req.Branch, Lease: req.Lease,
	}))
	if err != nil {
		return k.failed(ctx, err)
	}
	return nil
}

func (c *Client) Ticket(ctx context.Context, in provider.Instance, port int32) (provider.Ticket, error) {
	k, err := c.begin(ctx, in)
	if err != nil {
		return provider.Ticket{}, err
	}
	defer k.release()
	res, err := k.host.Ticket(ctx, connect.NewRequest(&hostdv1.TicketRequest{Instance: instance(in), Port: port}))
	if err != nil {
		return provider.Ticket{}, k.failed(ctx, err)
	}
	tk := provider.Ticket{URL: res.Msg.GetUrl()}
	if e := res.Msg.GetExpireTime(); e != nil {
		tk.Expires = e.AsTime()
	}
	return tk, nil
}
