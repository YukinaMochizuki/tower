package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"syscall"
	"time"

	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
)

// Routing a start that asks for auto by the tasks its line runs
// (docs/decisions/boost.md, "Routing by task").

const (
	// lastRunsJudged is how many of a task's last runs its routing reads.
	lastRunsJudged = 3
	// busyFor is how long a run in the box must have run for its CPU wait,
	// or its busy CPUs, to send the next to a boost.
	busyFor = 30 * time.Second
	// cpuWaitShare is the share of its time a run in the box waited for a
	// CPU, 1 in cpuWaitShare, that sends the next to a boost.
	cpuWaitShare = 5
	// allCPUsBusy is the share of the box's CPUs, in thousandths, a run's
	// CPU time over its wall time must reach to count as all of them busy:
	// a little below all, as a busy run never quite uses them whole.
	allCPUsBusy = 900
	// nearMemory is the share of the memory the box holds its measured
	// processes to (processMemoryMax), in percent, a peak must reach for
	// the next run to go to a boost that holds them to memoryGrowth times
	// it.
	nearMemory = 80
	// memoryGrowth is how much more than a run's peak, in percent, the
	// boost its next run goes to holds its processes to.
	memoryGrowth = 150
	// maxSourceBytes bounds a package.json read for routing.
	maxSourceBytes = 1 << 20
	// maxScriptDepth bounds the scripts a script's text is followed through.
	maxScriptDepth = 8
)

// routing is where a start asking for auto goes, and why.
type routing struct {
	boost bool
	size  boostSize
	// why is said in the start's answer and its exit, in its tasks' words.
	why string
	// tasks are its line's, kept with a boost's process for its record.
	tasks []taskEntry
}

// route decides where e runs command, from dir, for a start asking for
// auto: by its line's tasks, their last runs, or what they run.
func (s *Server) route(ctx context.Context, e *environment, command []string, dir string) routing {
	if e.box == nil {
		return routing{why: "a machine has no boost: it runs on the machine"}
	}
	if e.target.GetSpec().GetHangar() == nil {
		return routing{why: "its box's target makes no boosts: it runs in the box"}
	}
	pk := s.packagesIn(ctx, e)
	tasks := findTasks(commandText(command), dir, pk)
	if len(tasks) == 0 {
		return routing{why: "no task found in the line (a make target, a package.json script, a just or task recipe, or a heavy tool): in the box"}
	}
	j := judge{s: s, e: e, pk: pk, set: settingsOf(e.target), box: boostSizeOf(s.instanceResources(ctx, e)), repository: repositoryOf(e.box)}
	r := j.tasks(ctx, tasks)
	r.tasks = tasks
	return r
}

// judge routes a box's tasks.
type judge struct {
	s          *Server
	e          *environment
	pk         *packages
	set        settings
	box        boostSize
	repository string
}

func (j judge) onBoost(size boostSize) string {
	return "on a boost of " + size.String()
}

// larger is the larger of b and o in each.
func (b boostSize) larger(o boostSize) boostSize {
	return boostSize{milliCPU: max(b.milliCPU, o.milliCPU), memory: max(b.memory, o.memory)}
}

// tasks routes a line's tasks: by their runs if they have some, else
// each by its own, the largest boost any asks for winning.
func (j judge) tasks(ctx context.Context, tasks []taskEntry) routing {
	text := tasksText(tasks)
	var use taskUse
	if _, err := j.s.getRecord(ctx, taskUseKey(j.e.key.Project, j.repository, tasks), &use); err != nil {
		j.s.log.Warn("reading a task's runs failed", "environment", j.e.String(), "error", err)
	}
	if len(use.Runs) > 0 {
		return j.runs(text, use.Runs)
	}
	if len(tasks) == 1 {
		return j.firstRun(tasks[0])
	}
	var out routing
	for _, t := range tasks {
		r := j.tasks(ctx, []taskEntry{t})
		switch {
		case r.boost && (!out.boost || !out.size.covers(r.size)):
			out = routing{boost: true, size: out.size.larger(r.size), why: text + ": " + r.why}
		case !r.boost && !out.boost:
			if out.why == "" {
				out.why = text + ": " + r.why
			} else {
				out.why += "; " + r.why
			}
		}
	}
	return out
}

// runs routes a task by its last runs.
func (j judge) runs(text string, runs []taskRun) routing {
	last := runs[max(len(runs)-lastRunsJudged, 0):]
	of := fmt.Sprintf("of its last %d runs, one", len(last))
	if len(last) == 1 {
		of = "its last run"
	}
	// Memory first: a run near what the box holds its processes to, or
	// killed for it, wherever it ran.
	limit := uint64(0)
	if j.box.memory > 0 {
		limit = uint64(processMemoryMax(int64(min(j.box.memory, 1<<62))))
	}
	var peak uint64
	why := ""
	for _, r := range last {
		p := uint64(0)
		if r.MemoryPeak != nil && *r.MemoryPeak > 0 {
			p = uint64(*r.MemoryPeak)
		}
		oom := r.OOMKilled != nil && *r.OOMKilled
		if oom && p == 0 {
			// Killed with no peak told: at least what the instance it ran
			// on held its processes to.
			if m, err := quantity.Bytes(r.RanOn.Memory); err == nil && m > 0 {
				p = uint64(processMemoryMax(int64(min(m, 1<<62))))
			}
		}
		near := limit > 0 && p*100 >= limit*nearMemory
		if (!oom && !near) || p <= peak {
			continue
		}
		peak = p
		what := "peaked at"
		if oom {
			what = "ran out of memory at"
		}
		where := "in this box"
		if r.RanOn.Kind == ranOnBoost {
			where = "on a boost"
		}
		why = fmt.Sprintf("%s %s %s %s, of the %s this box holds its processes to", of, where, what, bytesText(p), bytesText(limit))
	}
	if peak > 0 {
		size, err := parseBoostSize("", quantity.FormatMemory(memoryHolding(peak*memoryGrowth/100)), j.set.boostResources)
		if err != nil {
			size = j.set.boostResources
		}
		size = size.shaped(j.set.boostMax)
		return routing{boost: true, size: size, why: fmt.Sprintf("%s: %s: %s", text, why, j.onBoost(size))}
	}
	// Then CPU: a run in the box that waited for one, or kept all busy.
	for _, r := range slices.Backward(last) {
		if r.RanOn.Kind != ranOnBox || r.Wall < busyFor {
			continue
		}
		box := "this box"
		if m, err := parseMilliCPU(r.RanOn.CPU); err == nil && m > 0 {
			box = "this " + formatMilliCPU(m) + " vCPU box"
		}
		switch {
		case r.CPUWait != nil && *r.CPUWait*cpuWaitShare >= r.Wall:
			why = fmt.Sprintf("%s in %s waited for a CPU %d%% of its %s", of, box, int(*r.CPUWait*100/r.Wall), r.Wall.Round(time.Second))
		case r.CPUUsage != nil && j.busy(*r.CPUUsage, r.Wall, r.RanOn.CPU):
			why = fmt.Sprintf("%s kept all the CPUs of %s busy for %s", of, box, r.Wall.Round(time.Second))
		default:
			continue
		}
		size := j.set.boostResources
		return routing{boost: true, size: size, why: fmt.Sprintf("%s: %s: %s", text, why, j.onBoost(size))}
	}
	latest := last[len(last)-1]
	if latest.RanOn.Kind != ranOnBoost {
		runs := "its last run"
		if len(last) > 1 {
			runs = fmt.Sprintf("its last %d runs", len(last))
		}
		return routing{why: fmt.Sprintf("%s: %s came near neither this box's CPUs nor its memory: in the box", text, runs)}
	}
	// On a boost: back in the box if it would have fit there.
	used := ""
	if latest.CPUUsage != nil && latest.Wall > 0 {
		used = fmt.Sprintf("%.1f", float64(*latest.CPUUsage)/float64(latest.Wall))
		if j.box.milliCPU > 0 && int64(*latest.CPUUsage)*1000 > int64(latest.Wall)*j.box.milliCPU {
			size := j.set.boostResources
			return routing{boost: true, size: size, why: fmt.Sprintf("%s: its last run, on a boost, used %s vCPU on average, more than this box's %s: %s",
				text, used, formatMilliCPU(j.box.milliCPU), j.onBoost(size))}
		}
	}
	// Its peak is below nearMemory of the box's limit, or the memory rule
	// above would have kept it on a boost.
	why = "its last run, on a boost, fits this box"
	if used != "" && latest.MemoryPeak != nil {
		why = fmt.Sprintf("its last run, on a boost, used %s vCPU on average and peaked at %s, which this %s vCPU box, holding its processes to %s, holds",
			used, bytesText(uint64(max(*latest.MemoryPeak, 0))), formatMilliCPU(j.box.milliCPU), bytesText(limit))
	}
	return routing{why: text + ": " + why + ": in the box"}
}

// memoryHolding is the least memory of an instance whose measured
// processes are held to at least limit (processMemoryMax solved for the
// instance's memory).
func memoryHolding(limit uint64) uint64 {
	return min(limit+processMemoryReserve, 2*limit)
}

// busy says a run of usage CPU time over wall kept all of the CPUs it ran
// on, cpu, busy.
func (j judge) busy(usage, wall time.Duration, cpu string) bool {
	m, err := parseMilliCPU(cpu)
	if err != nil || m <= 0 || wall <= 0 {
		return false
	}
	return int64(usage)*1000*1000 >= int64(wall)*m*allCPUsBusy
}

// firstRun routes a task with no runs: a heavy tool by its name, a
// package.json script by its text and the scripts it runs, a make, just
// or task target to the box, where its first run is measured. Nothing is
// run to judge a task (docs/decisions/boost.md, "Routing by task").
func (j judge) firstRun(t taskEntry) routing {
	text := t.String()
	size := j.set.boostResources
	switch {
	case heavyTool(t):
		return routing{boost: true, size: size, why: fmt.Sprintf("%s: no runs recorded yet; %s is a heavy tool: %s", text, t.Tool, j.onBoost(size))}
	case !t.script():
		return routing{why: fmt.Sprintf("%s: no runs recorded yet; its first run here is measured, and decides the next: in the box", text)}
	}
	p := j.pk.get(t.Dir)
	if _, ok := p.scripts[t.Name]; !p.ok || !ok {
		return routing{why: fmt.Sprintf("%s: no runs recorded yet, and its package.json has no such script: in the box", text)}
	}
	if heavy := j.heavyIn([]taskEntry{t}, 0, nil); heavy != "" {
		return routing{boost: true, size: size, why: fmt.Sprintf("%s: no runs recorded yet; its script runs %s, a heavy tool: %s", text, heavy, j.onBoost(size))}
	}
	return routing{why: fmt.Sprintf("%s: no runs recorded yet, and its script runs no heavy tool: in the box", text)}
}

// heavyTool says t is a heavy tool called directly.
func heavyTool(t taskEntry) bool {
	switch t.Tool {
	case "go test", "go build", "go vet", "cargo test", "cargo build", "bun test":
		return true
	}
	return slices.Contains(directTools, t.Tool) || slices.Contains(scriptTools, t.Tool)
}

// heavyIn is the first heavy tool tasks run, following the scripts they
// run: "" for none.
func (j judge) heavyIn(tasks []taskEntry, depth int, seen map[taskEntry]bool) string {
	if seen == nil {
		seen = map[taskEntry]bool{}
	}
	for _, t := range tasks {
		if heavyTool(t) {
			return t.Tool
		}
		if !t.script() || seen[t] || depth >= maxScriptDepth {
			continue
		}
		seen[t] = true
		p := j.pk.get(t.Dir)
		names := []string{t.Name}
		if t.Tool == "npm" {
			// npm runs a script's pre and post scripts with it.
			names = []string{"pre" + t.Name, t.Name, "post" + t.Name}
		}
		for _, name := range names {
			text, ok := p.scripts[name]
			if !ok {
				continue
			}
			if h := j.heavyIn(scriptTasks(text, t.Dir, j.pk), depth+1, seen); h != "" {
				return h
			}
		}
	}
	return ""
}

// packages is a boxView of e: its package.json files and directories, read
// through its provider as its user, each once per routing or record.
type packages struct {
	ctx  context.Context
	e    *environment
	got  map[string]packageJSON
	dirs map[string]bool
}

// packageJSON is what a package.json says of its scripts.
type packageJSON struct {
	ok      bool
	scripts map[string]string
}

func (s *Server) packagesIn(ctx context.Context, e *environment) *packages {
	return &packages{ctx: ctx, e: e, got: map[string]packageJSON{}, dirs: map[string]bool{}}
}

// read is a file of the box's, at most maxSourceBytes.
func (p *packages) read(file string) ([]byte, bool) {
	if p.e.p == nil {
		return nil, false
	}
	out := &capped{max: maxSourceBytes}
	if err := p.e.p.ReadFile(p.ctx, p.e.ref, file, out); err != nil || out.over {
		return nil, false
	}
	return out.Bytes(), true
}

// isDirTimeout bounds the read that tells a directory: a guest answers
// one at once, and a file at its first byte.
const isDirTimeout = 2 * time.Second

// errFirstByte stops a read that tells a directory once a file sends
// anything: a large file, or a device that never ends, is not read on.
var errFirstByte = errors.New("a file, not a directory")

// firstByte refuses every byte written to it.
type firstByte struct{}

func (firstByte) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	return 0, errFirstByte
}

// isDir reads dir as a file: a directory's read fails as one
// (syscall.EISDIR), sending nothing; any other answer, a file's first
// byte included, is no directory.
func (p *packages) isDir(dir string) bool {
	if got, ok := p.dirs[dir]; ok {
		return got
	}
	is := false
	if p.e.p != nil && dir != unknownDir {
		ctx, cancel := context.WithTimeout(p.ctx, isDirTimeout)
		err := p.e.p.ReadFile(ctx, p.e.ref, path.Join("/workspace", dir), firstByte{})
		cancel()
		is = errors.Is(err, syscall.EISDIR)
	}
	p.dirs[dir] = is
	return is
}

func (p *packages) get(dir string) packageJSON {
	if got, ok := p.got[dir]; ok {
		return got
	}
	var out packageJSON
	if b, ok := p.read(path.Join("/workspace", dir, "package.json")); ok {
		var pj struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(b, &pj) == nil {
			out = packageJSON{ok: true, scripts: pj.Scripts}
		}
	}
	p.got[dir] = out
	return out
}

func (p *packages) scripts(dir string) (map[string]string, bool) {
	got := p.get(dir)
	return got.scripts, got.ok
}

// capped is a buffer that keeps its first max bytes, and says it was
// given more.
type capped struct {
	bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(b []byte) (int, error) {
	if room := c.max - c.Len(); len(b) > room {
		c.over = true
		c.Buffer.Write(b[:max(room, 0)])
		return len(b), nil
	}
	return c.Buffer.Write(b)
}

// bytesText is an amount of memory for people: "1.7Gi", "600Mi".
func bytesText(b uint64) string {
	if b >= 1<<30 {
		return fmt.Sprintf("%.1fGi", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%dMi", (b+(1<<20)-1)>>20)
}
