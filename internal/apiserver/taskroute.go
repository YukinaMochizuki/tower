package apiserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"time"

	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/provider"
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
	// nearMemory is the share of the box's memory, in percent, a peak must
	// reach for the next run to go to a boost of memoryGrowth times it.
	nearMemory = 80
	// memoryGrowth is how much larger than a run's peak, in percent, the
	// boost its next run goes to is.
	memoryGrowth = 150
	// dryRunTimeout bounds make -n in the box; past it the task's
	// expansion is not known.
	dryRunTimeout = 10 * time.Second
	// maxSourceBytes bounds a makefile, a package.json and make -n's
	// output read for routing.
	maxSourceBytes = 1 << 20
	// maxScriptDepth bounds the scripts a script's expansion follows.
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
	tasks := findTasks(commandText(command), dir, pk.scripts)
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
		return j.expansion(ctx, tasks[0], use.Expansion)
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
	// Memory first: a run near the box's memory, or killed for it.
	var peak uint64
	why := ""
	for _, r := range last {
		p := uint64(0)
		if r.MemoryPeak != nil && *r.MemoryPeak > 0 {
			p = uint64(*r.MemoryPeak)
		}
		oom := r.OOMKilled != nil && *r.OOMKilled
		if oom && p == 0 {
			// Killed with no peak told: at least the memory it ran in.
			p, _ = quantity.Bytes(r.RanOn.Memory)
		}
		near := j.box.memory > 0 && p*100 >= j.box.memory*nearMemory
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
		why = fmt.Sprintf("%s %s %s %s of this box's %s", of, where, what, bytesText(p), bytesText(j.box.memory))
	}
	if peak > 0 {
		size, err := parseBoostSize("", quantity.FormatMemory(peak*memoryGrowth/100), j.set.boostResources)
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
	why = "its last run, on a boost, fits this box"
	if used != "" && latest.MemoryPeak != nil {
		why = fmt.Sprintf("its last run, on a boost, used %s vCPU on average and peaked at %s, which this box (%s vCPU, %s) holds",
			used, bytesText(uint64(max(*latest.MemoryPeak, 0))), formatMilliCPU(j.box.milliCPU), bytesText(j.box.memory))
	}
	return routing{why: text + ": " + why + ": in the box"}
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

// expansion routes a task with no runs by what it runs.
func (j judge) expansion(ctx context.Context, t taskEntry, cached *taskExpansion) routing {
	text := t.String()
	if heavyTool(t) {
		size := j.set.boostResources
		return routing{boost: true, size: size, why: fmt.Sprintf("%s: no runs recorded yet; %s is a heavy tool: %s", text, t.Tool, j.onBoost(size))}
	}
	var (
		x   *taskExpansion
		err error
	)
	switch {
	case t.Tool == "make":
		x, err = j.makeExpansion(ctx, t, cached)
	case t.script():
		x, err = j.scriptExpansion(ctx, t, cached)
	default:
		return routing{why: fmt.Sprintf("%s: no runs recorded yet, and what a %s recipe runs is not read: in the box", text, t.Tool)}
	}
	if err != nil {
		return routing{why: fmt.Sprintf("%s: no runs recorded yet, and what it runs is not known: %v: in the box", text, err)}
	}
	if x.Heavy == "" {
		return routing{why: fmt.Sprintf("%s: no runs recorded yet, and what it runs (%s) holds no heavy tool: in the box", text, x.Source)}
	}
	size := j.set.boostResources
	return routing{boost: true, size: size, why: fmt.Sprintf("%s: no runs recorded yet; what it runs (%s) holds %s, a heavy tool: %s", text, x.Source, x.Heavy, j.onBoost(size))}
}

// heavyTool says t is a heavy tool called directly.
func heavyTool(t taskEntry) bool {
	switch t.Tool {
	case "go test", "go build", "cargo test", "cargo build":
		return true
	}
	return slices.Contains(directTools, t.Tool) || slices.Contains(expansionTools, t.Tool)
}

// keepExpansion keeps x as t's, for the next start while its file is the
// same; a failure is logged, the routing stands.
func (j judge) keepExpansion(ctx context.Context, t taskEntry, x *taskExpansion) {
	tasks := []taskEntry{t}
	now := j.s.clock.Now()
	err := changeRecord(ctx, j.s, taskUseKey(j.e.key.Project, j.repository, tasks), func(u *taskUse) bool {
		if u.Expansion != nil && *u.Expansion == *x {
			return false
		}
		u.Repository, u.Tasks, u.Expansion, u.LastRecorded = j.repository, tasks, x, now
		return true
	})
	if err != nil {
		j.s.log.Warn("keeping a task's expansion failed", "environment", j.e.String(), "error", err)
	}
}

// makefiles are the files make reads in a directory, in its order.
var makefiles = []string{"GNUmakefile", "makefile", "Makefile"}

// errDryRun is a dry run that did not tell what a target runs.
var errDryRun = errors.New("its dry run (make -n)")

// makeExpansion is what t, a make target, runs: its make -n in the box,
// as its user, within dryRunTimeout; cached by its makefile's hash.
func (j judge) makeExpansion(ctx context.Context, t taskEntry, cached *taskExpansion) (*taskExpansion, error) {
	dir := path.Join("/workspace", t.Dir)
	sum := ""
	for _, name := range makefiles {
		if b, ok := j.pk.read(ctx, path.Join(dir, name)); ok {
			h := sha256.Sum256(b)
			sum = hex.EncodeToString(h[:])
			break
		}
	}
	if sum == "" {
		return nil, errors.New("it has no makefile")
	}
	if cached != nil && cached.Source == expansionMake && cached.File == sum {
		return cached, nil
	}
	dctx, cancel := context.WithTimeout(ctx, dryRunTimeout)
	defer cancel()
	out := &capped{max: maxSourceBytes}
	argv := []string{"timeout", "-k", "2", fmt.Sprint(int(dryRunTimeout / time.Second)), "make", "-n"}
	if t.Name != "" {
		argv = append(argv, splitNames(t.Name)...)
	}
	code, err := j.e.p.Exec(dctx, provider.ExecRequest{Instance: j.e.ref, Command: argv, Dir: dir, Stdout: out, Stderr: io.Discard})
	switch {
	case dctx.Err() != nil || code == 124 || code == 137:
		return nil, fmt.Errorf("%w ran past %s", errDryRun, dryRunTimeout)
	case err != nil:
		return nil, fmt.Errorf("%w could not run", errDryRun)
	case code != 0:
		return nil, fmt.Errorf("%w failed, exit %d", errDryRun, code)
	}
	x := &taskExpansion{Source: expansionMake, File: sum, Heavy: j.heavyIn(expansionTasks(out.String(), t.Dir, j.pk.scripts), 0, nil)}
	j.keepExpansion(ctx, t, x)
	return x, nil
}

// scriptExpansion is what t, a package.json script, runs: its text and
// the scripts it runs; cached by its package.json's hash.
func (j judge) scriptExpansion(ctx context.Context, t taskEntry, cached *taskExpansion) (*taskExpansion, error) {
	p := j.pk.get(t.Dir)
	if !p.ok {
		return nil, errors.New("its package.json cannot be read")
	}
	if _, ok := p.scripts[t.Name]; !ok {
		return nil, errors.New("its package.json has no such script")
	}
	if cached != nil && cached.Source == expansionScript && cached.File == p.sum {
		return cached, nil
	}
	x := &taskExpansion{Source: expansionScript, File: p.sum, Heavy: j.heavyIn([]taskEntry{t}, 0, map[taskEntry]bool{})}
	j.keepExpansion(ctx, t, x)
	return x, nil
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
			if h := j.heavyIn(expansionTasks(text, t.Dir, j.pk.scripts), depth+1, seen); h != "" {
				return h
			}
		}
	}
	return ""
}

// splitNames is a task's names, as Name joins them.
func splitNames(name string) []string {
	var out []string
	for _, n := range bytes.Fields([]byte(name)) {
		out = append(out, string(n))
	}
	return out
}

// packages reads the box's package.json files for a routing or a
// record, each once.
type packages struct {
	ctx  context.Context
	e    *environment
	read func(ctx context.Context, p string) ([]byte, bool)
	got  map[string]packageJSON
}

// packageJSON is what a package.json says of its scripts.
type packageJSON struct {
	ok      bool
	sum     string
	scripts map[string]string
}

// packagesIn reads e's files through its provider, as its user.
func (s *Server) packagesIn(ctx context.Context, e *environment) *packages {
	p := &packages{ctx: ctx, e: e, got: map[string]packageJSON{}}
	p.read = func(ctx context.Context, file string) ([]byte, bool) {
		if e.p == nil {
			return nil, false
		}
		out := &capped{max: maxSourceBytes}
		if err := e.p.ReadFile(ctx, e.ref, file, out); err != nil || out.over {
			return nil, false
		}
		return out.Bytes(), true
	}
	return p
}

// scriptsIn is a scriptsFunc reading e's package.json files.
func (s *Server) scriptsIn(ctx context.Context, e *environment) scriptsFunc {
	return s.packagesIn(ctx, e).scripts
}

func (p *packages) get(dir string) packageJSON {
	if got, ok := p.got[dir]; ok {
		return got
	}
	var out packageJSON
	if b, ok := p.read(p.ctx, path.Join("/workspace", dir, "package.json")); ok {
		var pj struct {
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(b, &pj) == nil {
			sum := sha256.Sum256(b)
			out = packageJSON{ok: true, sum: hex.EncodeToString(sum[:]), scripts: pj.Scripts}
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
