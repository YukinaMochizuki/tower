package apiserver_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1/environmentsv1alpha1connect"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

func startProcess(t *testing.T, envs environmentsv1alpha1connect.EnvironmentServiceClient, req *envv1alpha1.StartProcessRequest) string {
	t.Helper()
	res, err := envs.StartProcess(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.GetProcess()
}

// attachProcess attaches through EnvironmentService and returns the output
// and how the attach ended.
func attachProcess(ctx context.Context, envs environmentsv1alpha1connect.EnvironmentServiceClient, req *envv1alpha1.AttachProcessRequest) (string, *envv1alpha1.ProcessEnd, error) {
	stream, err := envs.AttachProcess(ctx, connect.NewRequest(req))
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = stream.Close() }()
	var out strings.Builder
	var end *envv1alpha1.ProcessEnd
	for stream.Receive() {
		switch r := stream.Msg().GetResponse().(type) {
		case *envv1alpha1.AttachProcessResponse_Output:
			out.Write(r.Output.GetData())
		case *envv1alpha1.AttachProcessResponse_End:
			end = r.End
		}
	}
	return out.String(), end, stream.Err()
}

// processesRunning is what the box's instance reports running.
func (w *world) processesRunning(box string) int32 {
	w.t.Helper()
	in, _ := w.instance(w.box(box).GetStatus().GetInstance())
	return in.GetStatus().GetProcessesRunning()
}

// TestARunningProcessKeepsABox: a box whose instance runs a process is
// never stopped for idleness, and the process ending is a use of it: the
// idle clock starts again then, so the output can still be read.
func TestARunningProcessKeepsABox(t *testing.T) {
	t.Parallel()
	w := newWorld(t, idleAfter(time.Hour))
	w.readyBox("dev")
	envs := w.as(workstream).envs
	id := startProcess(t, envs, &envv1alpha1.StartProcessRequest{Environment: boxEnv("dev"), Command: &envv1alpha1.StartProcessRequest_Argv{Argv: &envv1alpha1.Argv{Args: []string{"sleep"}}}, Name: "server"})
	testutil.Eventually(t, "the process reported running", func() bool { return w.processesRunning("dev") == 1 })
	w.advance(3 * time.Hour)
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("a box running a process is %v after three hours", p)
	}
	// It ends by itself, not through a call, which would be a use.
	in := provider.Instance{Project: "web", Name: w.box("dev").GetStatus().GetInstance()}
	if err := w.fake.EndProcess(in, id, 0); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the process reported ended", func() bool { return w.processesRunning("dev") == 0 })
	w.advance(59 * time.Minute)
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("a box whose process ended 59 minutes ago is %v", p)
	}
	w.advance(2 * time.Minute)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
}

// TestAnOpenStreamKeepsABox: a box whose instance has a direct stream open,
// an editor connected on a ticket, is never stopped for idleness, and the
// last stream's end is a use of it.
func TestAnOpenStreamKeepsABox(t *testing.T) {
	t.Parallel()
	w := newWorld(t, idleAfter(time.Hour))
	w.readyBox("dev")
	in := provider.Instance{Project: "web", Name: w.box("dev").GetStatus().GetInstance()}
	streams := func() int32 {
		got, _ := w.instance(in.Name)
		return got.GetStatus().GetStreamsOpen()
	}
	if err := w.fake.SetStreams(in, 1); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the stream reported open", func() bool { return streams() == 1 })
	w.advance(3 * time.Hour)
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("a box with a stream open is %v after three hours", p)
	}
	if err := w.fake.SetStreams(in, 0); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the stream reported closed", func() bool { return streams() == 0 })
	w.advance(59 * time.Minute)
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("a box whose stream closed 59 minutes ago is %v", p)
	}
	w.advance(2 * time.Minute)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
}

// TestARequestedStopEndsProcesses: stopping a box at workstream's request
// ends its processes.
func TestARequestedStopEndsProcesses(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("dev")
	id := startProcess(t, w.as(workstream).envs, &envv1alpha1.StartProcessRequest{Environment: boxEnv("dev"), Command: &envv1alpha1.StartProcessRequest_Argv{Argv: &envv1alpha1.Argv{Args: []string{"sleep"}}}})
	testutil.Eventually(t, "the process reported running", func() bool { return w.processesRunning("dev") == 1 })
	b := proto.Clone(w.box("dev")).(*envv1alpha1.Box)
	b.Spec.RunState = envv1alpha1.RunState_RUN_STATE_STOPPED
	w.mustApply(registry.Box, b)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
	list, err := w.fake.ListProcesses(t.Context(), provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != id || list[0].Exit == nil {
		t.Errorf("after the box stopped, its process: %+v", list)
	}
}

// TestProcessesThroughTower: a process outlives the call that started it;
// its output reads back from any offset, and a follow ends with its exit;
// it is listed and signalled; a privileged box's Secret reaches its
// environment and nowhere else; and an attach keeps the box in use.
func TestProcessesThroughTower(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	const canary = "canary-proc-Hx52"
	w.mustApply(registry.Secret, secret("web", "forge", map[string]string{"GH_TOKEN": canary}))
	w.readyBox("dev", func(b *envv1alpha1.Box) {
		b.Spec.PrivilegedSecrets = []*envv1alpha1.SecretRef{{Secret: "forge", Key: "GH_TOKEN", As: "GH_TOKEN"}}
	})
	envs := w.as(workstream).envs
	ctx := t.Context()

	id := startProcess(t, envs, &envv1alpha1.StartProcessRequest{
		Environment: boxEnv("dev"), Command: &envv1alpha1.StartProcessRequest_Argv{Argv: &envv1alpha1.Argv{Args: []string{"emit", "out:built", "err:warned"}}},
		Env: map[string]string{"CI": "1"}, Name: "build",
	})
	execs := w.fake.Execs()
	if last := execs[len(execs)-1]; !last.Process || last.Env["GH_TOKEN"] != canary || last.Env["CI"] != "1" {
		t.Errorf("the process was given %+v", last)
	}
	out, end, err := attachProcess(ctx, envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: id, Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	if out != "built\nwarned\n" || end.GetExit() == nil || end.GetExit().GetCode() != 0 || end.GetNextOffset() != int64(len(out)) {
		t.Errorf("attached %q, ended %v", out, end)
	}
	if rest, _, err := attachProcess(ctx, envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: id, Offset: 6}); err != nil || rest != "warned\n" {
		t.Errorf("from offset 6: %q, %v", rest, err)
	}

	server := startProcess(t, envs, &envv1alpha1.StartProcessRequest{Environment: boxEnv("dev"), Command: &envv1alpha1.StartProcessRequest_Shell{Shell: "npm run dev"}, Name: "dev server"})
	sleeper := startProcess(t, envs, &envv1alpha1.StartProcessRequest{Environment: boxEnv("dev"), Command: &envv1alpha1.StartProcessRequest_Argv{Argv: &envv1alpha1.Argv{Args: []string{"sleep"}}}})
	// An attach that follows keeps the box in use.
	actx, cancel := context.WithCancel(ctx)
	followed := make(chan *envv1alpha1.ProcessEnd, 1)
	go func() {
		_, end, _ := attachProcess(actx, envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: sleeper, Follow: true})
		followed <- end
	}()
	testutil.Eventually(t, "the attach following", func() bool { return w.fake.Following() == 1 })
	if !w.srv.InUse("web", "dev") {
		t.Error("a box with an attach following is not in use")
	}
	if _, err := envs.SignalProcess(ctx, connect.NewRequest(&envv1alpha1.SignalProcessRequest{Environment: boxEnv("dev"), Process: sleeper, Signal: envv1alpha1.Signal_SIGNAL_TERM})); err != nil {
		t.Fatal(err)
	}
	if end := <-followed; end.GetExit().GetSignal() != envv1alpha1.Signal_SIGNAL_TERM || end.GetExit().GetCode() != 143 {
		t.Errorf("a follow of a process sent TERM ended %v", end)
	}
	cancel()
	testutil.Eventually(t, "the box no longer in use", func() bool { return !w.srv.InUse("web", "dev") })

	res, err := envs.ListProcesses(ctx, connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: boxEnv("dev")}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range res.Msg.GetProcesses() {
		got = append(got, fmt.Sprintf("%s|%s|%s|%t|%d", p.GetProcess(), p.GetName(), p.GetCommand(), p.GetRunning(), p.GetExit().GetCode()))
	}
	want := []string{
		id + "|build|emit out:built err:warned|false|0",
		server + "|dev server|/bin/sh -c npm run dev|false|0",
		sleeper + "||sleep|false|143",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("listed\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	_, err = envs.SignalProcess(ctx, connect.NewRequest(&envv1alpha1.SignalProcessRequest{Environment: boxEnv("dev"), Process: "p99", Signal: envv1alpha1.Signal_SIGNAL_KILL}))
	wantCode(t, err, connect.CodeNotFound, "signalling a process not there")
	_, err = envs.SignalProcess(ctx, connect.NewRequest(&envv1alpha1.SignalProcessRequest{Environment: boxEnv("dev"), Process: sleeper}))
	wantCode(t, err, connect.CodeInvalidArgument, "a signal not named")
	w.waitSettled()
	w.nowhere(canary, &logs)
}

// TestProcessesInAMachine: a machine runs processes as a box does, given
// the environment of their own, apart from any call.
func TestProcessesInAMachine(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Machine, machine("k3s"))
	w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	envs := w.as(workstream).envs
	ctx := t.Context()
	id := startProcess(t, envs, &envv1alpha1.StartProcessRequest{
		Environment: machineEnv("k3s"), Command: &envv1alpha1.StartProcessRequest_Argv{Argv: &envv1alpha1.Argv{Args: []string{"emit", "out:served"}}},
		Env: map[string]string{"PORT": "6443"}, Name: "serve",
	})
	execs := w.fake.Execs()
	if last := execs[len(execs)-1]; !last.Process || last.Env["PORT"] != "6443" || last.Instance.Name != w.machine("k3s").GetStatus().GetInstance() {
		t.Errorf("the process was given %+v", last)
	}
	out, end, err := attachProcess(ctx, envs, &envv1alpha1.AttachProcessRequest{Environment: machineEnv("k3s"), Process: id, Follow: true})
	if err != nil || out != "served\n" || end.GetExit() == nil {
		t.Errorf("attached %q, ended %v, %v", out, end, err)
	}
	list, err := envs.ListProcesses(ctx, connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: machineEnv("k3s")}))
	if err != nil || len(list.Msg.GetProcesses()) != 1 || list.Msg.GetProcesses()[0].GetName() != "serve" {
		t.Errorf("listed %v, %v", list.Msg.GetProcesses(), err)
	}
	if _, err := envs.SignalProcess(ctx, connect.NewRequest(&envv1alpha1.SignalProcessRequest{Environment: machineEnv("k3s"), Process: id, Signal: envv1alpha1.Signal_SIGNAL_TERM})); err != nil {
		t.Errorf("signalling a process that ended: %v", err)
	}
}
