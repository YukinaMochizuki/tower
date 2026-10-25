package hostd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hostdv1 "github.com/YukinaMochizuki/tower/gen/go/tower/hostd/v1"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// The signals and outputs, as HostService names them.
var (
	signalsOnWire = map[provider.Signal]hostdv1.Signal{
		provider.SignalTerm: hostdv1.Signal_SIGNAL_TERM, provider.SignalKill: hostdv1.Signal_SIGNAL_KILL, provider.SignalInt: hostdv1.Signal_SIGNAL_INT,
	}
	streamsOnWire = map[provider.OutputStream]hostdv1.OutputStream{
		provider.Stdout: hostdv1.OutputStream_OUTPUT_STREAM_STDOUT, provider.Stderr: hostdv1.OutputStream_OUTPUT_STREAM_STDERR,
	}
)

func signalOf(s hostdv1.Signal) (provider.Signal, bool) {
	for k, v := range signalsOnWire {
		if v == s {
			return k, true
		}
	}
	return 0, false
}

func streamOf(s hostdv1.OutputStream) provider.OutputStream {
	for k, v := range streamsOnWire {
		if v == s {
			return k
		}
	}
	return provider.Stdout
}

func exitOnWire(e *provider.ProcessExit) *hostdv1.ProcessExit {
	if e == nil {
		return nil
	}
	return &hostdv1.ProcessExit{Code: int32(e.Code), Signal: signalsOnWire[e.Signal]}
}

func exitOf(e *hostdv1.ProcessExit) *provider.ProcessExit {
	if e == nil {
		return nil
	}
	sig, _ := signalOf(e.GetSignal())
	return &provider.ProcessExit{Code: int(e.GetCode()), Signal: sig}
}

// The HostService side.

func (s service) StartProcess(ctx context.Context, req *connect.Request[hostdv1.StartProcessRequest]) (*connect.Response[hostdv1.StartProcessResponse], error) {
	m := req.Msg
	id, err := s.p.StartProcess(ctx, provider.ProcessRequest{
		Instance: instanceOf(m.GetInstance()), Command: m.GetCommand(), Env: m.GetEnv(), Dir: m.GetDir(), Name: m.GetName(),
	})
	if err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&hostdv1.StartProcessResponse{Process: id}), nil
}

func (s service) AttachProcess(ctx context.Context, req *connect.Request[hostdv1.AttachProcessRequest], stream *connect.ServerStream[hostdv1.AttachProcessResponse]) error {
	m := req.Msg
	at, err := s.p.AttachProcess(ctx, instanceOf(m.GetInstance()), m.GetProcess(), m.GetOffset(), m.GetFollow(), func(c provider.OutputChunk) error {
		// Sent in pieces a message carries.
		for len(c.Data) > 0 {
			k := min(len(c.Data), chunkSize)
			if err := stream.Send(&hostdv1.AttachProcessResponse{Response: &hostdv1.AttachProcessResponse_Output{Output: &hostdv1.ProcessOutput{
				Stream: streamsOnWire[c.Stream], Offset: c.Offset, Data: c.Data[:k],
			}}}); err != nil {
				return err
			}
			c.Data, c.Offset = c.Data[k:], c.Offset+int64(k)
		}
		return nil
	})
	if err != nil {
		return toConnect(err)
	}
	return stream.Send(&hostdv1.AttachProcessResponse{Response: &hostdv1.AttachProcessResponse_End{End: &hostdv1.ProcessEnd{
		HeldFrom: at.HeldFrom, NextOffset: at.Next, Exit: exitOnWire(at.Exit),
	}}})
}

func (s service) SignalProcess(ctx context.Context, req *connect.Request[hostdv1.SignalProcessRequest]) (*connect.Response[hostdv1.SignalProcessResponse], error) {
	sig, ok := signalOf(req.Msg.GetSignal())
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("signal %v is not one a process can be sent", req.Msg.GetSignal()))
	}
	if err := s.p.SignalProcess(ctx, instanceOf(req.Msg.GetInstance()), req.Msg.GetProcess(), sig); err != nil {
		return nil, toConnect(err)
	}
	return connect.NewResponse(&hostdv1.SignalProcessResponse{}), nil
}

func (s service) ListProcesses(ctx context.Context, req *connect.Request[hostdv1.ListProcessesRequest]) (*connect.Response[hostdv1.ListProcessesResponse], error) {
	ps, err := s.p.ListProcesses(ctx, instanceOf(req.Msg.GetInstance()))
	if err != nil {
		return nil, toConnect(err)
	}
	res := &hostdv1.ListProcessesResponse{}
	for _, p := range ps {
		res.Processes = append(res.Processes, &hostdv1.Process{
			Process: p.ID, Name: p.Name, Command: p.Command, StartTime: timestamppb.New(p.Started), Exit: exitOnWire(p.Exit),
		})
	}
	return connect.NewResponse(res), nil
}

// The client side.

func (c *Client) StartProcess(ctx context.Context, req provider.ProcessRequest) (string, error) {
	k, err := c.begin(ctx, req.Instance)
	if err != nil {
		return "", err
	}
	defer k.release()
	res, err := k.host.StartProcess(ctx, connect.NewRequest(&hostdv1.StartProcessRequest{
		Instance: instance(req.Instance), Command: req.Command, Env: req.Env, Dir: req.Dir, Name: req.Name,
	}))
	if err != nil {
		return "", k.failed(ctx, err)
	}
	return res.Msg.GetProcess(), nil
}

func (c *Client) AttachProcess(ctx context.Context, in provider.Instance, id string, from int64, follow bool, out func(provider.OutputChunk) error) (provider.Attached, error) {
	k, err := c.begin(ctx, in)
	if err != nil {
		return provider.Attached{}, err
	}
	defer k.release()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := k.host.AttachProcess(ctx, connect.NewRequest(&hostdv1.AttachProcessRequest{Instance: instance(in), Process: id, Offset: from, Follow: follow}))
	if err != nil {
		return provider.Attached{}, k.failed(ctx, err)
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() {
		switch r := stream.Msg().GetResponse().(type) {
		case *hostdv1.AttachProcessResponse_Output:
			o := r.Output
			if err := out(provider.OutputChunk{Stream: streamOf(o.GetStream()), Offset: o.GetOffset(), Data: o.GetData()}); err != nil {
				return provider.Attached{}, err
			}
		case *hostdv1.AttachProcessResponse_End:
			e := r.End
			return provider.Attached{HeldFrom: e.GetHeldFrom(), Next: e.GetNextOffset(), Exit: exitOf(e.GetExit())}, nil
		}
	}
	if err := stream.Err(); err != nil {
		return provider.Attached{}, k.failed(ctx, err)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return provider.Attached{}, ctx.Err()
	}
	return provider.Attached{}, fmt.Errorf("target %s: the host ended an attach without saying where it ended: %w: %w", c.Target, io.ErrUnexpectedEOF, provider.ErrUnavailable)
}

func (c *Client) SignalProcess(ctx context.Context, in provider.Instance, id string, sig provider.Signal) error {
	wire, ok := signalsOnWire[sig]
	if !ok {
		return fmt.Errorf("signal %d is not one a process can be sent: %w", sig, provider.ErrFailed)
	}
	k, err := c.begin(ctx, in)
	if err != nil {
		return err
	}
	defer k.release()
	if _, err := k.host.SignalProcess(ctx, connect.NewRequest(&hostdv1.SignalProcessRequest{Instance: instance(in), Process: id, Signal: wire})); err != nil {
		return k.failed(ctx, err)
	}
	return nil
}

func (c *Client) ListProcesses(ctx context.Context, in provider.Instance) ([]provider.Process, error) {
	k, err := c.begin(ctx, in)
	if err != nil {
		return nil, err
	}
	defer k.release()
	res, err := k.host.ListProcesses(ctx, connect.NewRequest(&hostdv1.ListProcessesRequest{Instance: instance(in)}))
	if err != nil {
		return nil, k.failed(ctx, err)
	}
	out := make([]provider.Process, 0, len(res.Msg.GetProcesses()))
	for _, p := range res.Msg.GetProcesses() {
		out = append(out, provider.Process{
			ID: p.GetProcess(), Name: p.GetName(), Command: p.GetCommand(), Started: p.GetStartTime().AsTime(), Exit: exitOf(p.GetExit()),
		})
	}
	return out, nil
}
