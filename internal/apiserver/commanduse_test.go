package apiserver_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// commandUse is a CommandUse record as the store holds it.
type commandUse struct {
	Repository, Dir, Key, Command string
	Runs                          []struct {
		Box, Process, Command string
		ExitCode              int
		MemoryPeak            *int64
		OOMKilled             *bool
		Wall                  time.Duration
		RanOn                 struct{ Kind, CPU, Memory string }
	}
}

func (w *world) commandUses() map[string]commandUse {
	w.t.Helper()
	objs, _, err := w.store.List(w.t.Context(), store.ListOptions{Kind: "CommandUse", Project: "web"})
	if err != nil {
		w.t.Fatal(err)
	}
	out := map[string]commandUse{}
	for _, o := range objs {
		var r commandUse
		if err := json.Unmarshal(o.Data, &r); err != nil {
			w.t.Fatal(err)
		}
		out[r.Key+" "+r.Dir+" "+r.Command] = r
	}
	return out
}

// TestProcessUsage: a measured process's usage reaches its exit as the API
// gives it, and tower keeps it under the command's exact and coarse keys,
// once however often the exit is read. An unmeasured exit says no usage,
// and is not kept.
func TestProcessUsage(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("dev")
	envs := w.as(workstream).envs
	ctx := t.Context()
	in := provider.Instance{Project: "web", Name: w.box("dev").GetStatus().GetInstance()}
	peak, killed, cpu := int64(3<<30), true, 90*time.Second
	usage := &provider.ProcessUsage{MemoryPeak: &peak, OOMKilled: &killed, CPUUsage: &cpu, Wall: 2 * time.Minute}
	sleep := func(dir string) string {
		return startProcess(t, envs, &envv1alpha1.StartProcessRequest{
			Environment: boxEnv("dev"), Command: &envv1alpha1.StartProcessRequest_Argv{Argv: &envv1alpha1.Argv{Args: []string{"sleep", "60"}}}, Dir: dir,
		})
	}

	first := sleep("/workspace")
	if err := w.fake.EndProcessWith(in, first, provider.ProcessExit{Code: 137, Signal: provider.SignalKill, Usage: usage}); err != nil {
		t.Fatal(err)
	}
	_, end, err := attachProcess(ctx, envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: first})
	if err != nil {
		t.Fatal(err)
	}
	exit := end.GetExit()
	if exit.GetMemoryPeakBytes() != peak || !exit.GetOomKilled() || exit.GetCpuUsage().AsDuration() != cpu || exit.CpuWait != nil || exit.GetWall().AsDuration() != 2*time.Minute {
		t.Errorf("the attach's end says %v", exit)
	}
	uses := w.commandUses()
	exact, coarse := uses["exact . sleep 60"], uses["coarse . sleep"]
	if len(uses) != 2 || len(exact.Runs) != 1 || len(coarse.Runs) != 1 {
		t.Fatalf("recorded %+v", uses)
	}
	run := exact.Runs[0]
	if exact.Repository != "api" || run.Box != "dev" || run.Process != first || run.ExitCode != 137 || run.MemoryPeak == nil || *run.MemoryPeak != peak ||
		run.OOMKilled == nil || !*run.OOMKilled || run.Wall != 2*time.Minute || run.RanOn.Kind != "box" || run.RanOn.Memory == "" {
		t.Errorf("the exact record holds %+v, %+v", exact, run)
	}
	if coarse.Runs[0].Command != "sleep 60" {
		t.Errorf("the coarse record's run says command %q", coarse.Runs[0].Command)
	}

	// Read again, by attach and list, it is counted once.
	if _, _, err := attachProcess(ctx, envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: first}); err != nil {
		t.Fatal(err)
	}
	second := sleep("/workspace/")
	unmeasured := sleep("/workspace")
	if err := w.fake.EndProcessWith(in, second, provider.ProcessExit{Usage: usage}); err != nil {
		t.Fatal(err)
	}
	if err := w.fake.EndProcess(in, unmeasured, 0); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		res, err := envs.ListProcesses(ctx, connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: boxEnv("dev")}))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range res.Msg.GetProcesses() {
			measured := p.GetProcess() != unmeasured
			if (p.GetExit().MemoryPeakBytes != nil) != measured || (p.GetExit().Wall != nil) != measured {
				t.Errorf("listed %v", p)
			}
		}
	}
	uses = w.commandUses()
	exact = uses["exact . sleep 60"]
	var processes []string
	for _, r := range exact.Runs {
		processes = append(processes, r.Process)
	}
	if len(uses) != 2 || !slices.Equal(processes, []string{first, second}) || len(uses["coarse . sleep"].Runs) != 2 {
		t.Errorf("after reading the exits again, the records hold runs of %v: %+v", processes, uses)
	}

	// Unused for longer than they are kept, they go.
	w.advance(91 * 24 * time.Hour)
	testutil.Eventually(t, "the command uses pruned", func() bool { return w.records("CommandUse") == 0 })
}

// TestProcessUsageKeepsTheLastRuns: a command's records keep its newest
// runs alone, and a box gone takes what it counted with it.
func TestProcessUsageKeepsTheLastRuns(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("dev")
	envs := w.as(workstream).envs
	in := provider.Instance{Project: "web", Name: w.box("dev").GetStatus().GetInstance()}
	var ids []string
	for i := range 12 {
		id := startProcess(t, envs, &envv1alpha1.StartProcessRequest{
			Environment: boxEnv("dev"), Command: &envv1alpha1.StartProcessRequest_Argv{Argv: &envv1alpha1.Argv{Args: []string{"sleep", "go", "test"}}},
		})
		ids = append(ids, id)
		if err := w.fake.EndProcessWith(in, id, provider.ProcessExit{Usage: &provider.ProcessUsage{Wall: time.Duration(i+1) * time.Second}}); err != nil {
			t.Fatal(err)
		}
		w.clock.Advance(time.Second)
	}
	if _, err := envs.ListProcesses(t.Context(), connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: boxEnv("dev")})); err != nil {
		t.Fatal(err)
	}
	uses := w.commandUses()
	exact := uses["exact  sleep go test"]
	var got []string
	for _, r := range exact.Runs {
		got = append(got, r.Process)
	}
	if !slices.Equal(got, ids[2:]) || len(uses["coarse  sleep go"].Runs) != 10 {
		t.Errorf("kept runs of %v, want %v; records %v", got, ids[2:], uses)
	}
	if n := w.records("ProcessesRecorded"); n != 1 {
		t.Fatalf("%d boxes' counted processes", n)
	}
	if _, err := w.asSystem().generic.Delete(t.Context(), registry.Box, client.Key{Project: "web", Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the box gone", func() bool {
		_, err := w.get(registry.Box, "web", "dev")
		return connect.CodeOf(err) == connect.CodeNotFound
	})
	w.advance(2 * time.Minute)
	testutil.Eventually(t, "the counted processes of a box gone pruned", func() bool { return w.records("ProcessesRecorded") == 0 })
}
