package apiserver

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// taskUseKind holds what the last runs of a line's tasks used, per
// project, repository and tasks; never a command's text beyond its
// tasks' plain names, nor its environment
// (docs/decisions/process-usage.md, docs/decisions/boost.md).
// Not an API kind.
const taskUseKind = "TaskUse"

// commandUseKind held runs by the command line, which routing by task
// replaced; its records are deleted (pruneTaskUses).
const commandUseKind = "CommandUse"

// processesRecordedKind holds, per box, the processes whose exits its
// task uses count, so an exit read twice is counted once.
const processesRecordedKind = "ProcessesRecorded"

const (
	// taskRunsKept is how many runs a task use keeps, the newest.
	taskRunsKept = 10
	// taskUsesKeptFor is how long a task use outlives its last write, as a
	// box's use record does its last use.
	taskUsesKeptFor = useRecordsKeptFor
	// taskUsesPruneEvery is how often task uses are pruned: each prune
	// reads them all.
	taskUsesPruneEvery = time.Hour
	// maxTaskUses bounds a project's task uses, the least recently written
	// going first: about 4KiB each, 4MiB a project, far above the tasks a
	// project's repositories declare.
	maxTaskUses = 1000
	// recordTimeout bounds recording the exits a call read, which goes on
	// once the call ends: a run claimed and not recorded is lost.
	recordTimeout = 30 * time.Second
)

// taskUse is what taskUseKind holds: the runs of a line's tasks, one or
// more in order.
type taskUse struct {
	Repository string      `json:"repository"`
	Tasks      []taskEntry `json:"tasks"`
	// LastRecorded is when a run was last written, on tower's clock.
	LastRecorded time.Time `json:"lastRecorded"`
	Runs         []taskRun `json:"runs,omitempty"`
}

// taskRun is one run of a line's tasks and what it used.
type taskRun struct {
	// At is when it was recorded, on tower's clock.
	At       time.Time `json:"at"`
	Box      string    `json:"box"`
	Process  string    `json:"process"`
	ExitCode int       `json:"exitCode"`
	Signal   int       `json:"signal,omitempty"`
	// MemoryPeak to Wall are as ProcessExit says them; nil where the box's
	// kernel did not offer one.
	MemoryPeak *int64         `json:"memoryPeak,omitempty"`
	OOMKilled  *bool          `json:"oomKilled,omitempty"`
	CPUUsage   *time.Duration `json:"cpuUsage,omitempty"`
	CPUWait    *time.Duration `json:"cpuWait,omitempty"`
	Wall       time.Duration  `json:"wall"`
	// RanOn is the instance it ran on, so a peak reads against the size
	// it ran at.
	RanOn ranOn `json:"ranOn"`
}

// The kinds of instance a run ran on (ranOn).
const (
	ranOnBox   = "box"
	ranOnBoost = "boost"
)

// ranOn is an instance a run ran on: its kind (ranOnBox, ranOnBoost) and
// its size as its spec had it when the run started (a boost's) or its
// exit was read (a box's).
type ranOn struct {
	Kind   string `json:"kind"`
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// processesRecorded is what processesRecordedKind holds: the ids of the
// box's processes counted that its instance still keeps.
type processesRecorded struct {
	IDs []string `json:"ids"`
}

// taskUseKey is the key of the record of a line's tasks, a hash of what
// keys it.
func taskUseKey(project, repository string, tasks []taskEntry) store.Key {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d:%s\n", len(repository), repository)
	for _, t := range tasks {
		for _, part := range []string{t.Dir, t.Tool, t.Name} {
			_, _ = fmt.Fprintf(h, "%d:%s\n", len(part), part)
		}
	}
	return store.Key{Kind: taskUseKind, Project: project, Name: hex.EncodeToString(h.Sum(nil))[:40]}
}

// repositoryOf is a box's first checkout's repository, which its tasks
// are kept under.
func repositoryOf(box *envv1alpha1.Box) string {
	if cs := box.GetSpec().GetCheckouts(); len(cs) > 0 {
		return cs[0].GetRepository()
	}
	return ""
}

// workspaceRelative is dir relative to /workspace: "." for /workspace
// itself, absolute for one outside it, "" for none.
func workspaceRelative(dir string) string {
	if dir == "" {
		return ""
	}
	clean := path.Clean(dir)
	if rel, ok := strings.CutPrefix(clean, "/workspace"); ok && (rel == "" || rel[0] == '/') {
		if rel == "" {
			return "."
		}
		return rel[1:]
	}
	return clean
}

// shells are the programs whose "-c" line is a command's text: what
// commandLine runs a shell line with, and what a caller's argv may name.
var shells = []string{"sh", "/bin/sh", "/usr/bin/sh", "bash", "/bin/bash", "/usr/bin/bash"}

// commandText is a process's command as one line: a shell's -c line as it
// is, other argv joined by spaces, each word quoted where shell would
// read it otherwise.
func commandText(argv []string) string {
	if len(argv) == 3 && slices.Contains(shells, argv[0]) && (argv[1] == "-c" || argv[1] == "-lc") {
		return argv[2]
	}
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = shellQuote(a)
	}
	return strings.Join(words, " ")
}

// shellQuote is w as one shell word.
func shellQuote(w string) string {
	plain := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,+@%", r)
	}
	if w != "" && !strings.ContainsFunc(w, func(r rune) bool { return !plain(r) }) {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}

// exitRead records the usage of a box's process whose exit an attach
// read, unless it was counted. The exit does not say the command, which
// the box's processes are listed for, its boost's with them.
func (s *Server) exitRead(ctx context.Context, e *environment, id string, exit *provider.ProcessExit) {
	if e.box == nil || exit == nil || exit.Usage == nil {
		return
	}
	var rec processesRecorded
	key := store.Key{Kind: processesRecordedKind, Project: e.key.Project, Name: e.key.Name}
	if _, err := s.getRecord(ctx, key, &rec); err != nil {
		s.log.Error("reading the processes counted failed", "environment", e.String(), "error", err)
		return
	}
	if slices.Contains(rec.IDs, id) {
		return
	}
	ps, err := s.boxProcessesOf(ctx, e)
	if err != nil {
		s.log.Error("listing processes to count one's usage failed", "environment", e.String(), "process", id, "error", err)
		return
	}
	s.exitsListed(ctx, e, ps, true)
}

// exitsListed records the usage of a box's finished processes not counted
// yet, apart from ctx's end. Each is claimed first, so one read by two
// calls at once is counted once; one kept from a boost deleted was
// counted before it went, and is never claimed. complete says ps are all
// the box's processes, so those it lacks are dropped from the claims. A
// failure is logged, never the call's.
func (s *Server) exitsListed(ctx context.Context, e *environment, ps []listedProcess, complete bool) {
	if e.box == nil || !slices.ContainsFunc(ps, func(p listedProcess) bool { return p.Exit != nil && p.Exit.Usage != nil && !p.kept }) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	var claimed []listedProcess
	key := store.Key{Kind: processesRecordedKind, Project: e.key.Project, Name: e.key.Name}
	err := changeRecord(ctx, s, key, func(r *processesRecorded) bool {
		claimed = nil
		var kept []string
		if !complete {
			kept = slices.Clone(r.IDs)
		}
		for _, p := range ps {
			switch {
			case slices.Contains(r.IDs, p.ID):
				if complete {
					kept = append(kept, p.ID)
				}
			case p.Exit != nil && p.Exit.Usage != nil && !p.kept:
				kept = append(kept, p.ID)
				claimed = append(claimed, p)
			}
		}
		// Those the box keeps no more are read never again.
		if len(claimed) == 0 && len(kept) == len(r.IDs) {
			return false
		}
		r.IDs = kept
		return true
	})
	if err != nil {
		s.log.Error("claiming processes to count failed", "environment", e.String(), "error", err)
		return
	}
	for _, p := range claimed {
		if err := s.recordRun(ctx, e, p); err != nil {
			s.log.Error("recording a process's usage failed", "environment", e.String(), "process", p.ID, "error", err)
		}
	}
}

// recordRun adds a process's run to the records of its line's tasks: as
// found when it started, if it was kept then (a boost's process), else
// found now. A line with no task is not recorded.
func (s *Server) recordRun(ctx context.Context, e *environment, p listedProcess) error {
	tasks := p.tasks
	if !p.tasksKnown {
		tasks = findTasks(commandText(e.givenCommand(p.Command)), p.Dir, s.packagesIn(ctx, e))
	}
	if len(tasks) == 0 {
		return nil
	}
	repository := repositoryOf(e.box)
	u, now := p.Exit.Usage, s.clock.Now()
	run := taskRun{
		At: now, Box: e.key.Name, Process: p.ID, ExitCode: p.Exit.Code, Signal: int(p.Exit.Signal),
		MemoryPeak: u.MemoryPeak, OOMKilled: u.OOMKilled, CPUUsage: u.CPUUsage, CPUWait: u.CPUWait, Wall: u.Wall, RanOn: p.at,
	}
	return changeRecord(ctx, s, taskUseKey(e.key.Project, repository, tasks), func(old *taskUse) bool {
		if slices.ContainsFunc(old.Runs, func(o taskRun) bool { return o.Box == run.Box && o.Process == run.Process }) {
			return false
		}
		runs := append(old.Runs, run)
		if len(runs) > taskRunsKept {
			runs = runs[len(runs)-taskRunsKept:]
		}
		old.Repository, old.Tasks, old.Runs, old.LastRecorded = repository, tasks, runs, now
		return true
	})
}

// pruneTaskUses, at most every taskUsesPruneEvery, deletes the task uses
// not written for taskUsesKeptFor, those of a project past maxTaskUses,
// the counted processes of boxes gone, and every CommandUse left by a
// tower that kept runs by the command line.
func (s *Server) pruneTaskUses(ctx context.Context) error {
	now := s.clock.Now()
	if last := s.taskUsesPruned.Load(); last != 0 && now.Sub(time.Unix(0, last)) < taskUsesPruneEvery {
		return nil
	}
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: taskUseKind})
	if err != nil {
		return err
	}
	type use struct {
		o    store.Object
		last time.Time
	}
	byProject := map[string][]use{}
	var gone []store.Object
	for _, o := range objs {
		var r taskUse
		if json.Unmarshal(o.Data, &r) != nil || now.Sub(r.LastRecorded) > taskUsesKeptFor {
			gone = append(gone, o)
			continue
		}
		byProject[o.Project] = append(byProject[o.Project], use{o, r.LastRecorded})
	}
	for _, uses := range byProject {
		if len(uses) <= maxTaskUses {
			continue
		}
		slices.SortFunc(uses, func(a, b use) int { return cmp.Or(a.last.Compare(b.last), strings.Compare(a.o.Name, b.o.Name)) })
		for _, u := range uses[:len(uses)-maxTaskUses] {
			gone = append(gone, u.o)
		}
	}
	// Runs kept by the command line are dropped: their keys never
	// repeated, and routing reads tasks' records alone.
	legacy, _, err := s.store.List(ctx, store.ListOptions{Kind: commandUseKind})
	if err != nil {
		return err
	}
	gone = append(gone, legacy...)
	objs, _, err = s.store.List(ctx, store.ListOptions{Kind: processesRecordedKind})
	if err != nil {
		return err
	}
	boxProcs, _, err := s.store.List(ctx, store.ListOptions{Kind: boxProcessesKind})
	if err != nil {
		return err
	}
	for _, o := range append(objs, boxProcs...) {
		_, ok, err := getTyped[*envv1alpha1.Box](ctx, s, registry.Box, resource.Key{Project: o.Project, Name: o.Name})
		if err != nil {
			return err
		}
		if !ok {
			gone = append(gone, o)
		}
	}
	for _, o := range gone {
		if _, _, err := s.store.Delete(ctx, o.Key, o.Revision); err != nil && !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	s.taskUsesPruned.Store(now.UnixNano())
	return nil
}
