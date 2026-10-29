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

// commandUseKind holds what the last runs of a command used, per project,
// repository, directory and command key, never the command's text or
// environment (docs/decisions/process-usage.md). Not an API kind.
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
	// commandUsesPruneEvery is how often command uses are pruned: each
	// prune reads them all.
	commandUsesPruneEvery = time.Hour
	// maxCommandUses bounds a project's command uses, the least recently
	// run going first: about 4KiB each, 4MiB a project, far above the
	// distinct commands a project's boxes run in commandUsesKeptFor.
	maxCommandUses = 1000
	// recordTimeout bounds recording the exits a call read, which goes on
	// once the call ends: a run claimed and not recorded is lost.
	recordTimeout = 30 * time.Second
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
	// /workspace itself, absolute for one outside it.
	Dir string `json:"dir"`
	// Key is commandKeyExact, with CommandSHA256 the exact key, or
	// commandKeyCoarse, with Coarse the coarse key.
	Key           string `json:"key"`
	CommandSHA256 string `json:"commandSha256,omitempty"`
	Coarse        string `json:"coarse,omitempty"`
	// LastRecorded is when its last run was recorded, on tower's clock.
	LastRecorded time.Time    `json:"lastRecorded"`
	Runs         []commandRun `json:"runs"`
}

// commandRun is one run of a command and what it used.
type commandRun struct {
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

// commandUseKey is the key of the record of a command, a hash of what
// keys it.
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
		if err := s.recordRun(ctx, e, p.Process, p.at); err != nil {
			s.log.Error("recording a process's usage failed", "environment", e.String(), "process", p.ID, "error", err)
		}
	}
}

// recordRun adds a process's run to its command's records under both
// keys.
func (s *Server) recordRun(ctx context.Context, e *environment, p provider.Process, at ranOn) error {
	var repository string
	if cs := e.box.GetSpec().GetCheckouts(); len(cs) > 0 {
		repository = cs[0].GetRepository()
	}
	dir := workspaceRelative(p.Dir)
	text := commandText(e.givenCommand(p.Command))
	u, now := p.Exit.Usage, s.clock.Now()
	run := commandRun{
		At: now, Box: e.key.Name, Process: p.ID, ExitCode: p.Exit.Code, Signal: int(p.Exit.Signal),
		MemoryPeak: u.MemoryPeak, OOMKilled: u.OOMKilled, CPUUsage: u.CPUUsage, CPUWait: u.CPUWait, Wall: u.Wall, RanOn: at,
	}
	var errs []error
	for _, r := range []commandUse{
		{Repository: repository, Dir: dir, Key: commandKeyExact, CommandSHA256: exactKey(text)},
		{Repository: repository, Dir: dir, Key: commandKeyCoarse, Coarse: coarseCommand(text)},
	} {
		if r.CommandSHA256 == "" && r.Coarse == "" {
			continue
		}
		rk := commandUseKey(e.key.Project, repository, dir, r.Key, r.CommandSHA256+r.Coarse)
		errs = append(errs, changeRecord(ctx, s, rk, func(old *commandUse) bool {
			if slices.ContainsFunc(old.Runs, func(o commandRun) bool { return o.Box == run.Box && o.Process == run.Process }) {
				return false
			}
			runs := append(old.Runs, run)
			if len(runs) > commandRunsKept {
				runs = runs[len(runs)-commandRunsKept:]
			}
			*old = r
			old.Runs, old.LastRecorded = runs, now
			return true
		}))
	}
	return errors.Join(errs...)
}

// pruneCommandUses, at most every commandUsesPruneEvery, deletes the
// command uses not run for commandUsesKeptFor, those of a project past
// maxCommandUses, and the counted processes of boxes gone.
func (s *Server) pruneCommandUses(ctx context.Context) error {
	now := s.clock.Now()
	if last := s.commandUsesPruned.Load(); last != 0 && now.Sub(time.Unix(0, last)) < commandUsesPruneEvery {
		return nil
	}
	objs, _, err := s.store.List(ctx, store.ListOptions{Kind: commandUseKind})
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
		var r commandUse
		if json.Unmarshal(o.Data, &r) != nil || now.Sub(r.LastRecorded) > commandUsesKeptFor {
			gone = append(gone, o)
			continue
		}
		byProject[o.Project] = append(byProject[o.Project], use{o, r.LastRecorded})
	}
	for _, uses := range byProject {
		if len(uses) <= maxCommandUses {
			continue
		}
		slices.SortFunc(uses, func(a, b use) int { return cmp.Or(a.last.Compare(b.last), strings.Compare(a.o.Name, b.o.Name)) })
		for _, u := range uses[:len(uses)-maxCommandUses] {
			gone = append(gone, u.o)
		}
	}
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
	s.commandUsesPruned.Store(now.UnixNano())
	return nil
}
