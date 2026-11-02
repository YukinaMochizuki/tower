package apiserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// taskUse is a TaskUse record as the store holds it.
type taskUse struct {
	Repository   string
	Tasks        []struct{ Dir, Tool, Name string }
	LastRecorded time.Time
	Runs         []struct {
		Box, Process string
		ExitCode     int
		MemoryPeak   *int64
		OOMKilled    *bool
		Wall         time.Duration
		RanOn        struct{ Kind, CPU, Memory string }
	}
	Expansion *struct{ Source, File, Heavy string }
}

// text is its tasks as a routed answer words them.
func (u taskUse) text() string {
	var words []string
	for _, t := range u.Tasks {
		s := t.Tool
		if t.Name != "" {
			s += " " + t.Name
		}
		if t.Dir != "." {
			s += " (in " + t.Dir + ")"
		}
		words = append(words, s)
	}
	return strings.Join(words, " && ")
}

// taskUses are the project web's task uses by their tasks' words, and
// every record's data together.
func (w *world) taskUses() (map[string]taskUse, string) {
	w.t.Helper()
	objs, _, err := w.store.List(w.t.Context(), store.ListOptions{Kind: "TaskUse", Project: "web"})
	if err != nil {
		w.t.Fatal(err)
	}
	out := map[string]taskUse{}
	var all strings.Builder
	for _, o := range objs {
		all.Write(o.Data)
		var r taskUse
		if err := json.Unmarshal(o.Data, &r); err != nil {
			w.t.Fatal(err)
		}
		out[r.text()] = r
	}
	return out, all.String()
}

func runsOf(r taskUse) []string {
	var out []string
	for _, run := range r.Runs {
		out = append(out, run.Process)
	}
	return out
}

func sleepArgv(args ...string) *envv1alpha1.StartProcessRequest_Argv {
	return &envv1alpha1.StartProcessRequest_Argv{Argv: &envv1alpha1.Argv{Args: append([]string{"sleep"}, args...)}}
}

// shellLine is a shell line the fake runs until it is ended, as its
// first word is sleep.
func shellLine(line string) *envv1alpha1.StartProcessRequest_Shell {
	return &envv1alpha1.StartProcessRequest_Shell{Shell: "sleep; " + line}
}

// TestProcessUsage: a process is held to its box's memory less tower's
// reserve; a measured process's usage reaches its exit as the API gives
// it, and tower keeps it under the line's task, once however often the
// exit is read, and never the line's other words. An unmeasured exit
// says no usage, and is not kept; a line with no task is not kept.
func TestProcessUsage(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("dev")
	envs := w.as(workstream).envs
	ctx := t.Context()
	inName := w.box("dev").GetStatus().GetInstance()
	in := provider.Instance{Project: "web", Name: inName}
	peak, killed, cpu := int64(3<<30), true, 90*time.Second
	usage := &provider.ProcessUsage{MemoryPeak: &peak, OOMKilled: &killed, CPUUsage: &cpu, Wall: 2 * time.Minute}
	const secret = "canary-arg-Qz81"
	start := func(dir string) string {
		return startProcess(t, envs, &envv1alpha1.StartProcessRequest{Environment: boxEnv("dev"), Command: shellLine("export TOKEN=" + secret + "; make test ARG=" + secret + " | tail -5"), Dir: dir})
	}

	first := start("/workspace")
	inst, _ := w.instance(inName)
	memory, err := quantity.Bytes(inst.GetSpec().GetResources().GetMemory())
	if err != nil {
		t.Fatal(err)
	}
	want := max(int64(memory)-768<<20, int64(memory)/2)
	if execs := w.fake.Execs(); execs[len(execs)-1].MemoryMax != want {
		t.Errorf("a box of %d bytes held its processes to %d, want %d", memory, execs[len(execs)-1].MemoryMax, want)
	}
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
	if exit.GetRouted() != "" {
		t.Errorf("a start that did not ask for auto says it was routed: %q", exit.GetRouted())
	}
	uses, all := w.taskUses()
	use := uses["make test"]
	if len(uses) != 1 || len(use.Runs) != 1 {
		t.Fatalf("recorded %+v", uses)
	}
	if strings.Contains(all, secret) || strings.Contains(all, "tail") || strings.Contains(all, "TOKEN") {
		t.Errorf("a record holds more of the line than its task: %s", all)
	}
	run := use.Runs[0]
	if use.Repository != "api" || run.Box != "dev" || run.Process != first || run.ExitCode != 137 || run.MemoryPeak == nil || *run.MemoryPeak != peak ||
		run.OOMKilled == nil || !*run.OOMKilled || run.Wall != 2*time.Minute || run.RanOn.Kind != "box" || run.RanOn.Memory == "" {
		t.Errorf("the record holds %+v, %+v", use, run)
	}

	// Read again, by attach and list, it is counted once.
	if _, _, err := attachProcess(ctx, envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: first}); err != nil {
		t.Fatal(err)
	}
	second := start("/workspace/")
	unmeasured := start("/workspace")
	noTask := startProcess(t, envs, &envv1alpha1.StartProcessRequest{Environment: boxEnv("dev"), Command: sleepArgv("60", secret)})
	if err := w.fake.EndProcessWith(in, second, provider.ProcessExit{Usage: usage}); err != nil {
		t.Fatal(err)
	}
	if err := w.fake.EndProcess(in, unmeasured, 0); err != nil {
		t.Fatal(err)
	}
	if err := w.fake.EndProcessWith(in, noTask, provider.ProcessExit{Usage: usage}); err != nil {
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
	uses, all = w.taskUses()
	if got := runsOf(uses["make test"]); len(uses) != 1 || !slices.Equal(got, []string{first, second}) {
		t.Errorf("after reading the exits again, the records hold runs of %v: %+v", got, uses)
	}
	if strings.Contains(all, noTask) {
		t.Errorf("a line with no task was recorded: %s", all)
	}

	// Unused for longer than they are kept, they go.
	w.advance(91 * 24 * time.Hour)
	testutil.Eventually(t, "the task uses pruned", func() bool { return w.records("TaskUse") == 0 })
}

// TestProcessUsageKeepsTheLastRuns: a task's records keep its newest runs
// alone, by its directory; and a box gone takes what it counted with it.
func TestProcessUsageKeepsTheLastRuns(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("dev")
	envs := w.as(workstream).envs
	in := provider.Instance{Project: "web", Name: w.box("dev").GetStatus().GetInstance()}
	var ids []string
	for i := range 12 {
		line := "go test ./... -run X"
		if i%2 == 1 {
			line = "cd /workspace/sub && go test ./... 2>&1 | tail -50"
		}
		id := startProcess(t, envs, &envv1alpha1.StartProcessRequest{Environment: boxEnv("dev"), Command: shellLine(line), Dir: "/workspace/sub"})
		ids = append(ids, id)
		if err := w.fake.EndProcessWith(in, id, provider.ProcessExit{Usage: &provider.ProcessUsage{Wall: time.Duration(i+1) * time.Second}}); err != nil {
			t.Fatal(err)
		}
		// Each is read on its own, so runs are recorded in order.
		if _, err := envs.ListProcesses(t.Context(), connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: boxEnv("dev")})); err != nil {
			t.Fatal(err)
		}
	}
	uses, _ := w.taskUses()
	if got := runsOf(uses["go test ./... (in sub)"]); len(uses) != 1 || !slices.Equal(got, ids[2:]) {
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
	// Pruned at most once an hour.
	w.advance(time.Hour + time.Minute)
	testutil.Eventually(t, "the counted processes of a box gone pruned", func() bool { return w.records("ProcessesRecorded") == 0 })
}

// TestProcessUsageFindsScriptsInTheBox: a runner's shorthand is recorded
// as a script when the box's package.json has it, and not otherwise.
func TestProcessUsageFindsScriptsInTheBox(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("dev")
	envs := w.as(workstream).envs
	in := provider.Instance{Project: "web", Name: w.box("dev").GetStatus().GetInstance()}
	if err := w.fake.WriteFile(t.Context(), in, "/workspace/web/package.json", 0o644, 0, bytes.NewReader([]byte(`{"scripts":{"lint":"eslint ."}}`))); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"cd web && pnpm lint", "pnpm lint"} {
		id := startProcess(t, envs, &envv1alpha1.StartProcessRequest{Environment: boxEnv("dev"), Command: shellLine(line)})
		if err := w.fake.EndProcessWith(in, id, provider.ProcessExit{Usage: &provider.ProcessUsage{Wall: time.Second}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := envs.ListProcesses(t.Context(), connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: boxEnv("dev")})); err != nil {
		t.Fatal(err)
	}
	uses, all := w.taskUses()
	if len(uses) != 1 || len(uses["pnpm lint (in web)"].Runs) != 1 {
		t.Errorf("recorded %s", all)
	}
}

// TestProcessUsageIsRecordedOnceAtOnce: exits read by calls at once,
// lists and an attach racing a list, are each counted once.
func TestProcessUsageIsRecordedOnceAtOnce(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("dev")
	envs := w.as(workstream).envs
	in := provider.Instance{Project: "web", Name: w.box("dev").GetStatus().GetInstance()}
	var ids []string
	for range 4 {
		id := startProcess(t, envs, &envv1alpha1.StartProcessRequest{Environment: boxEnv("dev"), Command: shellLine("make test")})
		if err := w.fake.EndProcessWith(in, id, provider.ProcessExit{Usage: &provider.ProcessUsage{Wall: time.Second}}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			if i%2 == 0 {
				_, err := envs.ListProcesses(t.Context(), connect.NewRequest(&envv1alpha1.ListProcessesRequest{Environment: boxEnv("dev")}))
				errs <- err
				return
			}
			_, _, err := attachProcess(t.Context(), envs, &envv1alpha1.AttachProcessRequest{Environment: boxEnv("dev"), Process: ids[i/2]})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	uses, _ := w.taskUses()
	got := runsOf(uses["make test"])
	slices.Sort(got)
	if !slices.Equal(got, ids) {
		t.Errorf("recorded runs of %v, want each of %v once", got, ids)
	}
}

// TestProcessUsageOutlivesTheCall: the exits a call read are recorded
// even though the call is cancelled as it records them.
func TestProcessUsageOutlivesTheCall(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.readyBox("dev")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ps := []provider.Process{{ID: "p1", Command: []string{"make", "e2e"}, Dir: "/workspace", Started: w.clock.Now(), Exit: &provider.ProcessExit{Usage: &provider.ProcessUsage{Wall: time.Minute}}}}
	if err := w.srv.RecordExits(ctx, "web", "dev", ps); err != nil {
		t.Fatal(err)
	}
	uses, _ := w.taskUses()
	if got := runsOf(uses["make e2e"]); !slices.Equal(got, []string{"p1"}) {
		t.Errorf("a cancelled call recorded %+v", uses)
	}
}

// TestTaskUsesAreCapped: a project keeps maxTaskUses task uses, the least
// recently written going.
func TestTaskUsesAreCapped(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	now := w.clock.Now()
	for i := range 1002 {
		data, _ := json.Marshal(map[string]any{"tasks": []map[string]string{{"dir": ".", "tool": "make", "name": fmt.Sprint("t", i)}}, "lastRecorded": now.Add(time.Duration(i) * time.Second)})
		if _, err := w.store.Create(t.Context(), store.Key{Kind: "TaskUse", Project: "web", Name: fmt.Sprintf("c%04d", i)}, data); err != nil {
			t.Fatal(err)
		}
	}
	w.advance(time.Hour + time.Minute)
	testutil.Eventually(t, "the task uses capped", func() bool { return w.records("TaskUse") == 1000 })
	for _, name := range []string{"c0000", "c0001"} {
		if _, err := w.store.Get(t.Context(), store.Key{Kind: "TaskUse", Project: "web", Name: name}); err == nil {
			t.Errorf("%s, among the least recently written, was kept", name)
		}
	}
}

// TestCommandUsesAreDropped: the runs a tower kept by the command line
// are deleted at the first prune, however recent.
func TestCommandUsesAreDropped(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	data, _ := json.Marshal(map[string]any{"key": "coarse", "coarse": "go test", "lastRecorded": w.clock.Now()})
	for _, project := range []string{"web", "other"} {
		if _, err := w.store.Create(t.Context(), store.Key{Kind: "CommandUse", Project: project, Name: "c1"}, data); err != nil {
			t.Fatal(err)
		}
	}
	w.advance(time.Hour + time.Minute)
	testutil.Eventually(t, "the command uses dropped", func() bool {
		objs, _, err := w.store.List(t.Context(), store.ListOptions{Kind: "CommandUse"})
		return err == nil && len(objs) == 0
	})
}
