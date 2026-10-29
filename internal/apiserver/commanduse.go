package apiserver

import (
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

// commandUseKind holds what the last runs of a command used, per project,
// repository (its box's first checkout's), working directory and command:
// under its exact key (normalizeCommand) and its coarse one
// (coarseCommand), which is advisory, for hints alone, never to route a
// run by (docs/decisions/process-usage.md). It is not an API kind; nothing
// serves or watches it.
//
// It holds a command line as the caller gave it, as the guest's process
// list does and ListProcesses shows, and never a command's environment,
// where a privileged box's secrets go (docs/decisions/secrets.md). A
// secret written into the command line itself is in both already; this
// record reaches no client.
const commandUseKind = "CommandUse"

// processesRecordedKind holds, per box, the processes whose exits its
// command uses count, so an exit read twice is counted once.
const processesRecordedKind = "ProcessesRecorded"

const (
	// commandRunsKept is how many runs a command use keeps, the newest.
	commandRunsKept = 10
	// commandUsesKeptFor is how long a command use outlives its last run,
	// as a box's use record does its last use.
	commandUsesKeptFor = useRecordsKeptFor
	// maxRecordedCommand bounds the command line a record keeps; its key
	// is made of the whole of it.
	maxRecordedCommand = 4 << 10
)

// The keys a command use is kept under.
const (
	commandKeyExact  = "exact"
	commandKeyCoarse = "coarse"
)

// commandUse is what commandUseKind holds.
type commandUse struct {
	Repository string `json:"repository"`
	// Dir is the working directory relative to /workspace: "." for
	// /workspace itself, absolute for one outside it, empty for the
	// box's default.
	Dir string `json:"dir"`
	// Key is commandKeyExact or commandKeyCoarse, and Command the
	// normalized command or its coarse key.
	Key     string       `json:"key"`
	Command string       `json:"command"`
	LastRun time.Time    `json:"lastRun"`
	Runs    []commandRun `json:"runs"`
}

// commandRun is one run of a command and what it used.
type commandRun struct {
	// At is when it ended.
	At      time.Time `json:"at"`
	Box     string    `json:"box"`
	Process string    `json:"process"`
	// Command is the normalized command, in a coarse record.
	Command  string `json:"command,omitempty"`
	ExitCode int    `json:"exitCode"`
	Signal   int    `json:"signal,omitempty"`
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

// ranOn is an instance a run ran on: its kind ("box"; later a boost) and
// its size as its spec had it when the exit was read.
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

// commandUseKey is the key of the record of a command, a hash of what
// keys it: commands can be long, and hold any byte.
func commandUseKey(project, repository, dir, key, command string) store.Key {
	h := sha256.New()
	for _, part := range []string{repository, dir, key, command} {
		_, _ = fmt.Fprintf(h, "%d:%s\n", len(part), part)
	}
	return store.Key{Kind: commandUseKind, Project: project, Name: hex.EncodeToString(h.Sum(nil))[:40]}
}

// workspaceRelative is dir relative to /workspace, as commandUse.Dir says.
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

// changeRecord changes the record of key, made from its zero value if
// there is none; change reports whether to write it.
func changeRecord[T any](ctx context.Context, s *Server, key store.Key, change func(*T) bool) error {
	for range maxRetries {
		var r T
		o, err := s.store.Get(ctx, key)
		found := err == nil
		switch {
		case found:
			if err := json.Unmarshal(o.Data, &r); err != nil {
				return err
			}
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		if !change(&r) {
			return nil
		}
		data, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if found {
			_, err = s.store.Update(ctx, key, data, o.Revision)
		} else {
			_, err = s.store.Create(ctx, key, data)
		}
		if !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrExists) {
			return err
		}
	}
	return fmt.Errorf("too many writers of %s %s/%s", key.Kind, key.Project, key.Name)
}

// exitRead records the usage of a box's process whose exit an attach
// read, unless it was counted. The exit does not say the command, which
// the box's processes are listed for.
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
	ps, err := e.p.ListProcesses(ctx, e.ref)
	if err != nil {
		s.log.Error("listing processes to count one's usage failed", "environment", e.String(), "process", id, "error", err)
		return
	}
	s.exitsListed(ctx, e, ps)
}

// exitsListed records the usage of a box's finished processes not counted
// yet. Each is claimed first, so one read by two calls at once is counted
// by one; a claimed one whose record fails to be written is not counted.
// A failure is logged, never the call's.
func (s *Server) exitsListed(ctx context.Context, e *environment, ps []provider.Process) {
	if e.box == nil {
		return
	}
	measured := slices.ContainsFunc(ps, func(p provider.Process) bool { return p.Exit != nil && p.Exit.Usage != nil })
	if !measured {
		return
	}
	var claimed []provider.Process
	key := store.Key{Kind: processesRecordedKind, Project: e.key.Project, Name: e.key.Name}
	err := changeRecord(ctx, s, key, func(r *processesRecorded) bool {
		claimed = nil
		var kept []string
		for _, p := range ps {
			switch {
			case slices.Contains(r.IDs, p.ID):
				kept = append(kept, p.ID)
			case p.Exit != nil && p.Exit.Usage != nil:
				kept = append(kept, p.ID)
				claimed = append(claimed, p)
			}
		}
		// Those the instance keeps no more are read never again.
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
	if len(claimed) == 0 {
		return
	}
	at := s.ranOn(ctx, e)
	for _, p := range claimed {
		if err := s.recordRun(ctx, e, p, at); err != nil {
			s.log.Error("recording a process's usage failed", "environment", e.String(), "process", p.ID, "error", err)
		}
	}
}

// ranOn is the instance e's processes run on, as its spec has it now.
func (s *Server) ranOn(ctx context.Context, e *environment) ranOn {
	r := ranOn{Kind: "box"}
	res := e.box.GetSpec().GetResources()
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: e.ref.Project, Name: e.ref.Name})
	if err == nil && ok {
		res = in.GetSpec().GetResources()
	}
	r.CPU, r.Memory = res.GetCpu(), res.GetMemory()
	return r
}

// recordRun adds a process's run to its command's records under both
// keys.
func (s *Server) recordRun(ctx context.Context, e *environment, p provider.Process, at ranOn) error {
	var repository string
	if cs := e.box.GetSpec().GetCheckouts(); len(cs) > 0 {
		repository = cs[0].GetRepository()
	}
	dir := workspaceRelative(p.Dir)
	exact := normalizeCommand(commandText(e.givenCommand(p.Command)))
	u := p.Exit.Usage
	run := commandRun{
		At: p.Started.Add(u.Wall), Box: e.key.Name, Process: p.ID, ExitCode: p.Exit.Code, Signal: int(p.Exit.Signal),
		MemoryPeak: u.MemoryPeak, OOMKilled: u.OOMKilled, CPUUsage: u.CPUUsage, CPUWait: u.CPUWait, Wall: u.Wall, RanOn: at,
	}
	coarseRun := run
	coarseRun.Command = bounded(exact)
	var errs []error
	for _, k := range []struct {
		key, command string
		run          commandRun
	}{{commandKeyExact, exact, run}, {commandKeyCoarse, coarseCommand(exact), coarseRun}} {
		if k.command == "" {
			continue
		}
		rk := commandUseKey(e.key.Project, repository, dir, k.key, k.command)
		errs = append(errs, changeRecord(ctx, s, rk, func(r *commandUse) bool {
			if slices.ContainsFunc(r.Runs, func(old commandRun) bool { return old.Box == run.Box && old.Process == run.Process }) {
				return false
			}
			r.Repository, r.Dir, r.Key, r.Command = repository, dir, k.key, bounded(k.command)
			r.Runs = append(r.Runs, k.run)
			slices.SortStableFunc(r.Runs, func(a, b commandRun) int { return a.At.Compare(b.At) })
			if len(r.Runs) > commandRunsKept {
				r.Runs = r.Runs[len(r.Runs)-commandRunsKept:]
			}
			r.LastRun = maxTime(r.LastRun, run.At)
			return true
		}))
	}
	return errors.Join(errs...)
}

// bounded is a command line as a record keeps it.
func bounded(command string) string {
	if len(command) > maxRecordedCommand {
		return command[:maxRecordedCommand] + "..."
	}
	return command
}

// pruneCommandUses deletes the command uses not run for
// commandUsesKeptFor, and the counted processes of boxes gone.
func (s *Server) pruneCommandUses(ctx context.Context) error {
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: commandUseKind})
	if err != nil {
		return err
	}
	for _, o := range objs {
		var r commandUse
		if json.Unmarshal(o.Data, &r) == nil && s.clock.Since(r.LastRun) > commandUsesKeptFor {
			if _, _, err := s.store.Delete(ctx, o.Key, o.Revision); err != nil && !errors.Is(err, store.ErrConflict) {
				return err
			}
		}
	}
	objs, _, err = s.store.List(ctx, store.ListOptions{Kind: processesRecordedKind})
	if err != nil {
		return err
	}
	for _, o := range objs {
		_, ok, err := getTyped[*envv1alpha1.Box](ctx, s, registry.Box, resource.Key{Project: o.Project, Name: o.Name})
		if err != nil {
			return err
		}
		if !ok {
			if _, _, err := s.store.Delete(ctx, o.Key, o.Revision); err != nil && !errors.Is(err, store.ErrConflict) {
				return err
			}
		}
	}
	return nil
}
