package apiserver

import (
	"context"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
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

// processMemoryReserve is the memory of a box its measured processes
// leave to the rest of it, all together. Idle boxes measured 230-660M on
// hangar in 2026-10, services included; this is above that.
const processMemoryReserve = 768 << 20

// processMemoryMax is the memory a box of memory bytes holds its measured
// processes to, all together: all but processMemoryReserve, and at least
// half, so a small box's processes are not starved by the reserve.
func processMemoryMax(memory int64) int64 {
	return max(memory-processMemoryReserve, memory/2)
}

// instanceResources is the size of e's instance as its spec has it, or,
// if it cannot be read, as its box asks.
func (s *Server) instanceResources(ctx context.Context, e *environment) *envv1alpha1.Resources {
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: e.ref.Project, Name: e.ref.Name})
	if err != nil || !ok {
		return e.box.GetSpec().GetResources()
	}
	return in.GetSpec().GetResources()
}

// processMemoryMaxOf is the memory e's measured processes are held to; 0,
// no bound, when its instance's memory is not known.
func (s *Server) processMemoryMaxOf(ctx context.Context, e *environment) int64 {
	memory, err := quantity.Bytes(s.instanceResources(ctx, e).GetMemory())
	if err != nil || memory == 0 || memory > math.MaxInt64 {
		return 0
	}
	return processMemoryMax(int64(memory))
}

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
	if u := e.Usage; u != nil {
		out.MemoryPeakBytes, out.OomKilled = u.MemoryPeak, u.OOMKilled
		out.CpuUsage, out.CpuWait, out.Wall = durationOf(u.CPUUsage), durationOf(u.CPUWait), durationpb.New(u.Wall)
	}
	return out
}

func durationOf(d *time.Duration) *durationpb.Duration {
	if d == nil {
		return nil
	}
	return durationpb.New(*d)
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
	if m.GetPending() != "" && m.GetBoost() == nil && !m.GetCancel() {
		return nil, invalid("pending asks again for a boosted start: give its boost as first asked")
	}
	e, err := h.open(ctx, m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	defer e.release()
	if m.GetBoost() != nil || m.GetPending() != "" {
		res, err := h.startBoosted(ctx, c, e, m, command)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(res), nil
	}
	id, err := e.p.StartProcess(ctx, provider.ProcessRequest{
		Instance: e.ref, Command: e.asRunAs(command, m.GetDir()), Env: e.commandEnv(m.GetEnv()), Dir: m.GetDir(), Name: m.GetName(),
		MemoryMax: h.s.processMemoryMaxOf(ctx, e),
	})
	// Never the command's arguments or its environment's values: either may
	// hold a secret.
	h.s.log.Info("process started", "caller", c.String(), "environment", e.String(), "process", id, "program", command[0],
		"env_names", slices.Sorted(maps.Keys(m.GetEnv())), "privileged", e.secretEnv != nil, "failed", err != nil)
	if err != nil {
		return nil, providerError(e, err)
	}
	res := &envv1alpha1.StartProcessResponse{Process: id}
	if e.box != nil {
		r := h.s.instanceResources(ctx, e)
		res.RanOn = ranOn{Kind: ranOnBox, CPU: r.GetCpu(), Memory: r.GetMemory()}.proto()
	}
	return connect.NewResponse(res), nil
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
	ref, pid := e.ref, m.GetProcess()
	if strings.HasPrefix(pid, boostProcessPrefix) && e.box != nil {
		bp, err := h.s.boostProcessOf(ctx, e, pid)
		if err != nil {
			return err
		}
		live, err := h.s.liveInstanceOf(ctx, e.key.Project, bp)
		if err != nil {
			return apiError(err)
		}
		if !live {
			// Its boost's instance is gone, its output with it: its exit as
			// kept, or lost.
			p := listedProcess{Process: provider.Process{Exit: bp.Exit}, lost: bp.Exit == nil}
			return stream.Send(&envv1alpha1.AttachProcessResponse{Response: &envv1alpha1.AttachProcessResponse_End{End: &envv1alpha1.ProcessEnd{
				HeldFrom: m.GetOffset(), NextOffset: m.GetOffset(), Exit: p.exit(),
			}}})
		}
		ref, pid = provider.Instance{Project: e.key.Project, Name: bp.Instance}, bp.ProviderID
	}
	at, err := e.p.AttachProcess(ctx, ref, pid, m.GetOffset(), m.GetFollow(), func(c provider.OutputChunk) error {
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
	h.s.exitRead(ctx, e, m.GetProcess(), at.Exit)
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
	ref, pid := e.ref, m.GetProcess()
	if strings.HasPrefix(pid, boostProcessPrefix) && e.box != nil {
		bp, err := h.s.boostProcessOf(ctx, e, pid)
		if err != nil {
			return nil, err
		}
		live, err := h.s.liveInstanceOf(ctx, e.key.Project, bp)
		if err != nil {
			return nil, apiError(err)
		}
		if !live {
			return nil, failedPrecondition("process %q ran on %s's boost, deleted since: it has ended", pid, e)
		}
		ref, pid = provider.Instance{Project: e.key.Project, Name: bp.Instance}, bp.ProviderID
	}
	err = e.p.SignalProcess(ctx, ref, pid, sig)
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
	ps, err := h.s.boxProcessesOf(ctx, e)
	if err != nil {
		return nil, providerError(e, err)
	}
	h.s.exitsListed(ctx, e, ps, true)
	res := &envv1alpha1.ListProcessesResponse{}
	for _, p := range ps {
		cmd := commandSummary(e.givenCommand(p.Command))
		if p.kept {
			cmd = p.Command[0]
		}
		res.Processes = append(res.Processes, &envv1alpha1.ProcessInfo{
			Process: p.ID, Name: p.Name, Command: cmd, StartTime: timestamppb.New(p.Started), Running: p.running(), Exit: p.exit(),
			RanOn: p.at.proto(),
		})
	}
	return connect.NewResponse(res), nil
}
