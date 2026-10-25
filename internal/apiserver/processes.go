package apiserver

import (
	"context"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// commandSummaryLen bounds the command line ListProcesses shows.
const commandSummaryLen = 200

// The signals and outputs, as EnvironmentService names them.
var (
	signals = map[envv1alpha1.Signal]provider.Signal{
		envv1alpha1.Signal_SIGNAL_TERM: provider.SignalTerm, envv1alpha1.Signal_SIGNAL_KILL: provider.SignalKill, envv1alpha1.Signal_SIGNAL_INT: provider.SignalInt,
	}
	outputStreams = map[provider.OutputStream]envv1alpha1.OutputStream{
		provider.Stdout: envv1alpha1.OutputStream_OUTPUT_STREAM_STDOUT, provider.Stderr: envv1alpha1.OutputStream_OUTPUT_STREAM_STDERR,
	}
)

func processExit(e *provider.ProcessExit) *envv1alpha1.ProcessExit {
	if e == nil {
		return nil
	}
	out := &envv1alpha1.ProcessExit{Code: int32(e.Code)}
	for k, v := range signals {
		if v == e.Signal && e.Signal != 0 {
			out.Signal = k
		}
	}
	return out
}

func (h environmentService) StartProcess(ctx context.Context, req *connect.Request[envv1alpha1.StartProcessRequest]) (*connect.Response[envv1alpha1.StartProcessResponse], error) {
	m := req.Msg
	c, err := h.begin(ctx, m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	command, err := commandLine(m.GetArgv(), m.GetShell())
	if err != nil {
		return nil, err
	}
	e, err := h.open(ctx, m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	defer e.release()
	id, err := e.p.StartProcess(ctx, provider.ProcessRequest{
		Instance: e.ref, Command: command, Env: e.commandEnv(m.GetEnv()), Dir: m.GetDir(), Name: m.GetName(),
	})
	// Never the command's arguments or its environment's values: either may
	// hold a secret.
	h.s.log.Info("process started", "caller", c.String(), "environment", e.String(), "process", id, "program", command[0],
		"env_names", slices.Sorted(maps.Keys(m.GetEnv())), "privileged", e.secretEnv != nil, "failed", err != nil)
	if err != nil {
		return nil, providerError(e, err)
	}
	return connect.NewResponse(&envv1alpha1.StartProcessResponse{Process: id}), nil
}

func (h environmentService) AttachProcess(ctx context.Context, req *connect.Request[envv1alpha1.AttachProcessRequest], stream *connect.ServerStream[envv1alpha1.AttachProcessResponse]) error {
	m := req.Msg
	if _, err := h.begin(ctx, m.GetEnvironment()); err != nil {
		return err
	}
	h.held(ctx)
	if m.GetProcess() == "" || m.GetOffset() < 0 {
		return invalid("process is required, and offset at least 0")
	}
	// The box is in use until the attach ends, a follow included.
	e, err := h.open(ctx, m.GetEnvironment())
	if err != nil {
		return err
	}
	defer e.release()
	at, err := e.p.AttachProcess(ctx, e.ref, m.GetProcess(), m.GetOffset(), m.GetFollow(), func(c provider.OutputChunk) error {
		for len(c.Data) > 0 {
			k := min(len(c.Data), outputChunk)
			if err := stream.Send(&envv1alpha1.AttachProcessResponse{Response: &envv1alpha1.AttachProcessResponse_Output{Output: &envv1alpha1.ProcessOutput{
				Stream: outputStreams[c.Stream], Offset: c.Offset, Data: c.Data[:k],
			}}}); err != nil {
				return err
			}
			c.Data, c.Offset = c.Data[k:], c.Offset+int64(k)
		}
		return nil
	})
	if err != nil {
		return providerError(e, err)
	}
	return stream.Send(&envv1alpha1.AttachProcessResponse{Response: &envv1alpha1.AttachProcessResponse_End{End: &envv1alpha1.ProcessEnd{
		HeldFrom: at.HeldFrom, NextOffset: at.Next, Exit: processExit(at.Exit),
	}}})
}

func (h environmentService) SignalProcess(ctx context.Context, req *connect.Request[envv1alpha1.SignalProcessRequest]) (*connect.Response[envv1alpha1.SignalProcessResponse], error) {
	m := req.Msg
	c, err := h.begin(ctx, m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	sig, ok := signals[m.GetSignal()]
	if !ok || m.GetProcess() == "" {
		return nil, invalid("process is required, and signal one of SIGNAL_TERM, SIGNAL_KILL or SIGNAL_INT")
	}
	e, err := h.open(ctx, m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	defer e.release()
	err = e.p.SignalProcess(ctx, e.ref, m.GetProcess(), sig)
	h.s.log.Info("process signalled", "caller", c.String(), "environment", e.String(), "process", m.GetProcess(), "signal", m.GetSignal().String(), "failed", err != nil)
	if err != nil {
		return nil, providerError(e, err)
	}
	return connect.NewResponse(&envv1alpha1.SignalProcessResponse{}), nil
}

func (h environmentService) ListProcesses(ctx context.Context, req *connect.Request[envv1alpha1.ListProcessesRequest]) (*connect.Response[envv1alpha1.ListProcessesResponse], error) {
	m := req.Msg
	if _, err := h.begin(ctx, m.GetEnvironment()); err != nil {
		return nil, err
	}
	e, err := h.open(ctx, m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	defer e.release()
	ps, err := e.p.ListProcesses(ctx, e.ref)
	if err != nil {
		return nil, providerError(e, err)
	}
	res := &envv1alpha1.ListProcessesResponse{}
	for _, p := range ps {
		cmd := strings.Join(p.Command, " ")
		if len(cmd) > commandSummaryLen {
			cmd = cmd[:commandSummaryLen] + "..."
		}
		res.Processes = append(res.Processes, &envv1alpha1.ProcessInfo{
			Process: p.ID, Name: p.Name, Command: cmd, StartTime: timestamppb.New(p.Started), Running: p.Exit == nil, Exit: processExit(p.Exit),
		})
	}
	return connect.NewResponse(res), nil
}
