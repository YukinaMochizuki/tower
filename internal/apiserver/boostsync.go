package apiserver

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// A boost's sync (docs/decisions/boost.md): before a boosted process
// starts, the boost's /workspace is made the box's: tower-guest walks the
// boost's (boost-manifest), the manifest is copied to the box, where what
// differs is packed (boost-pack), copied to the boost through
// Provider.Copy, and applied there (boost-apply). The process runs under
// boost-job, which says what it changed; once it ends, that is packed on
// the boost (boost-back), copied to the box, and written back there
// (boost-writeback), the box winning a conflict.

const (
	// boostSyncMax is the most one sync sends.
	boostSyncMax = 2 << 30
	// boostBackMax is the most one process's changes bring back.
	boostBackMax = 2 << 30
	// boostSyncWait is how long a boosted start waits for its sync before
	// it answers pending SYNCING.
	boostSyncWait = 10 * time.Second
	// boostSyncKind keeps, per box, its last sync to its boost.
	boostSyncKind = "BoostSync"
	// checkoutCommitAnnotation is, on a boost's instance, the commit its
	// first repository was checked out at.
	checkoutCommitAnnotation = registry.Domain + "/checkout-commit"
	// workspace is where a box's first repository is, and its boost's.
	workspace = "/workspace"
	// forcedKept bounds the paths a sync sends again whatever their
	// records say.
	forcedKept = 1000
)

// boostDir is where the boost subcommands keep their files, in a
// checkout's git directory.
var boostDir = path.Join(workspace, ".git", guestbin.BoostDir)

// boostSyncRecord is what boostSyncKind holds: the box's last sync to its
// boost, for that boost's instance alone: the tree and HEAD sent, and the
// box's manifest as sent (LastSent, kept in the box), which a shared sync
// sends what changed since.
type boostSyncRecord struct {
	BoostUID    string `json:"boostUid"`
	InstanceUID string `json:"instanceUid"`
	Tree        string `json:"tree"`
	Head        string `json:"head"`
	LastSent    string `json:"lastSent,omitempty"`
}

func boostSyncKey(box controller.Key) store.Key {
	return store.Key{Kind: boostSyncKind, Project: box.Project, Name: box.Name}
}

// syncStats are what a sync did, as a process's exit says them. Sent
// names the box's manifest as sent, which the process's write-back
// compares the box with.
type syncStats struct {
	Tree       string             `json:"tree,omitempty"`
	Sent       string             `json:"sent,omitempty"`
	Took       time.Duration      `json:"took"`
	Bytes      int64              `json:"bytes"`
	Files      int                `json:"files"`
	Deleted    int                `json:"deleted"`
	Full       bool               `json:"full,omitempty"`
	Shared     bool               `json:"shared,omitempty"`
	Submodules []string           `json:"submodules,omitempty"`
	Over       []guestbin.OverCap `json:"over,omitempty"`
}

// runResult is what a boosted process's run left once it ended: what of
// its changes was written back.
type runResult struct {
	Back  *writeBack `json:"back,omitempty"`
	Error string     `json:"error,omitempty"`
}

// writeBack is a process's write-back to the box.
type writeBack struct {
	Took         time.Duration      `json:"took"`
	Files        int                `json:"files"`
	Deleted      int                `json:"deleted"`
	Bytes        int64              `json:"bytes"`
	Moved        []guestbin.Moved   `json:"moved,omitempty"`
	Total        int                `json:"total"`
	Conflicts    []string           `json:"conflicts,omitempty"`
	Over         []guestbin.OverCap `json:"over,omitempty"`
	HeadMoved    bool               `json:"headMoved,omitempty"`
	IndexChanged bool               `json:"indexChanged,omitempty"`
	Error        string             `json:"error,omitempty"`
}

func overCaps(over []guestbin.OverCap) []*envv1alpha1.OverCap {
	var out []*envv1alpha1.OverCap
	for _, o := range over {
		out = append(out, &envv1alpha1.OverCap{Path: o.Path, Bytes: o.Bytes})
	}
	return out
}

// boostRunOf is what the API says of a boost process's run.
func boostRunOf(bp boostProcess) *envv1alpha1.BoostRun {
	if bp.Sync == nil && bp.Run == nil {
		return nil
	}
	r := &envv1alpha1.BoostRun{}
	if s := bp.Sync; s != nil {
		r.Sync, r.SyncBytes, r.SyncFiles, r.SyncDeleted = durationpb.New(s.Took), s.Bytes, int32(s.Files), int32(s.Deleted)
		r.SyncFull, r.BaseShared, r.SubmodulesNotSynced, r.SyncOverCap = s.Full, s.Shared, s.Submodules, overCaps(s.Over)
	}
	if run := bp.Run; run != nil {
		r.Error = run.Error
		if b := run.Back; b != nil {
			r.Back = &envv1alpha1.WriteBack{
				Took: durationpb.New(b.Took), Files: int32(b.Files), Deleted: int32(b.Deleted), Bytes: b.Bytes, Total: int32(b.Total),
				Conflicts: b.Conflicts, OverCap: overCaps(b.Over), HeadMoved: b.HeadMoved, IndexChanged: b.IndexChanged, Error: b.Error,
			}
			for _, m := range b.Moved {
				r.Back.Moved = append(r.Back.Moved, &envv1alpha1.MovedFile{Path: m.Path, Status: m.Status, Bytes: m.Bytes})
			}
		}
	}
	return r
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// guestRun runs a tower-guest subcommand in ref, as argv, and returns its
// exit code, stdout and stderr.
func guestRun(ctx context.Context, p provider.Provider, ref provider.Instance, args ...string) (int, []byte, string, error) {
	var out, errOut bytes.Buffer
	code, err := p.Exec(ctx, provider.ExecRequest{Instance: ref, Command: append([]string{provider.GuestBinary}, args...), Dir: "/", Stdout: &out, Stderr: &errOut})
	return code, out.Bytes(), strings.TrimSpace(strings.TrimPrefix(errOut.String(), "tower-guest: ")), err
}

// errSyncTooLarge is git's state past boostSyncMax, which no sync sends.
var errSyncTooLarge = errors.New("the box's staged changes and commits are too large to sync")

// errSharedNoBase is a shared sync whose last sync is not known: what the
// running process writes cannot be told from what the box changed.
var errSharedNoBase = errors.New("another boosted process of the box runs, and the box's last sync is not known")

// syncBoost makes the boost's instance in's /workspace the box's: what
// differs between the boost's files and the box's is sent, ignored files
// too, never .git or /.ws-cache; a tracked file whose content the boost's
// check finds is not the box's is sent again, once. shared says a process
// of the box runs on the boost: only what the box changed since its last
// sync is sent, so what that one writes stays. A box with no checkout has
// nothing to send.
func (s *Server) syncBoost(ctx context.Context, e *environment, b *envv1alpha1.Boost, in *envv1alpha1.Instance, tq *targetBoosts) (*syncStats, error) {
	checkouts := e.box.GetSpec().GetCheckouts()
	if len(checkouts) == 0 {
		return nil, nil
	}
	started := s.clock.Now()
	boxKey := controller.Key{Project: e.key.Project, Name: e.key.Name}
	boostRef := provider.Instance{Project: e.key.Project, Name: in.GetMetadata().GetName()}
	// Whether a process of the box runs on the boost, read now, under the
	// box's sync lock, which a start holds too: what it wrote stays. One
	// not known to run nothing is taken to run something.
	shared, err := s.boostRunsSomething(ctx, e.p, boxKey, in)
	if err != nil {
		s.log.Warn("listing a boost's processes before a sync failed; what they wrote stays", "environment", e.String(), "error", err)
		shared = true
	}
	// A process being started counts as one that runs, which the start's
	// hold on the lock makes none; this holds without it.
	tq.mu.Lock()
	shared = shared || tq.starting[boxKey] > 0
	tq.mu.Unlock()
	if !shared {
		// What the ended ones changed is written back before the sync undoes
		// it on the boost; if that fails, their writes stay.
		if err := s.settleInstance(ctx, boxKey, b, in, e.p); err != nil {
			s.log.Warn("settling a boost's ended processes before a sync failed; what they wrote stays", "environment", e.String(), "error", err)
			shared = true
		} else if open, err := s.unsettled(ctx, boxKey, in); err != nil || open > 0 {
			// One not settled yet keeps what it wrote: never undone before it
			// is written back.
			shared = true
		}
	}
	var rec boostSyncRecord
	if _, err := s.getRecord(ctx, boostSyncKey(boxKey), &rec); err != nil {
		return nil, err
	}
	known := rec.BoostUID == b.GetMetadata().GetUid() && rec.InstanceUID == in.GetMetadata().GetUid() && rec.LastSent != ""
	if shared && !known {
		return nil, errSharedNoBase
	}
	full := !known
	bundleAll, checked := false, false
	var forced []string
	for {
		name := "sync-" + randomID()
		stage := path.Join(boostDir, name)
		clean := func(ref provider.Instance) {
			// Removed whatever came of the sync; a crash's leftovers are
			// swept by the next.
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if _, _, msg, err := guestRun(cctx, e.p, ref, "boost-clean", "--dir", workspace, "--name", name); err != nil || msg != "" {
				s.log.Warn("removing a sync's staging failed", "environment", e.String(), "instance", ref.Name, "error", err, "stderr", msg)
			}
		}
		args := []string{"boost-pack", "--dir", workspace, "--out", stage, "--max", strconv.Itoa(boostSyncMax)}
		if shared {
			args = append(args, "--shared")
		} else {
			// The boost's files, as its manifest says them, in the box.
			mine := path.Join(stage, "boost.manifest")
			code, _, msg, err := guestRun(ctx, e.p, boostRef, "boost-manifest", "--dir", workspace, "--out", mine)
			switch {
			case err != nil:
				return nil, providerError(e, fmt.Errorf("reading the boost's files: %w", err))
			case code != 0:
				clean(boostRef)
				return nil, providerError(e, fmt.Errorf("reading the boost's files: exit %d: %s: %w", code, msg, provider.ErrFailed))
			}
			err = e.p.Copy(ctx, boostRef, stage, e.ref, stage)
			clean(boostRef)
			if err != nil {
				clean(e.ref)
				return nil, providerError(e, fmt.Errorf("copying the boost's manifest to the box: %w", err))
			}
			args = append(args, "--boost", mine)
		}
		if known {
			args = append(args, "--since", rec.LastSent)
		}
		if bundleAll {
			args = append(args, "--bundle-all")
		}
		for _, f := range forced {
			args = append(args, "--force", f)
		}
		code, out, msg, err := guestRun(ctx, e.p, e.ref, args...)
		switch {
		case err != nil:
			clean(e.ref)
			return nil, providerError(e, fmt.Errorf("packing the box's files: %w", err))
		case code == guestbin.ExitTooLarge:
			clean(e.ref)
			return nil, failedPrecondition("%s: %s (%v)", e, msg, errSyncTooLarge)
		case code == guestbin.ExitNoSince:
			clean(e.ref)
			return nil, fmt.Errorf("%w: %s", errSharedNoBase, msg)
		case code != 0:
			clean(e.ref)
			return nil, providerError(e, fmt.Errorf("packing the box's files: exit %d: %s: %w", code, msg, provider.ErrFailed))
		}
		var m guestbin.Manifest
		if err := json.Unmarshal(out, &m); err != nil {
			clean(e.ref)
			return nil, fmt.Errorf("boost-pack's answer: %w", err)
		}
		a, err := s.applySync(ctx, e, boostRef, stage, &m, known && rec.Head == m.Head && !bundleAll, shared)
		clean(e.ref)
		switch {
		case errors.Is(err, errNoHead) && !bundleAll:
			// The boost lacks what the bundle needs: HEAD's whole history,
			// once.
			s.log.Warn("a boost lacks what its sync's bundle needs; sending HEAD's history", "environment", e.String(), "error", err)
			bundleAll = true
			continue
		case err != nil:
			return nil, err
		}
		rec = boostSyncRecord{BoostUID: b.GetMetadata().GetUid(), InstanceUID: in.GetMetadata().GetUid(), Tree: m.Tree, Head: m.Head, LastSent: m.Sent}
		if err := s.putRecord(ctx, boostSyncKey(boxKey), rec); err != nil {
			return nil, err
		}
		known = true
		if len(a.Mismatch) > 0 {
			if checked {
				// Sent again, and still not the box's: tower's fault, or the
				// boost's checkout's, never the box's.
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("%s: syncing the box to its boost: what the boost holds of %s is not the box's, sent again: %w",
					e, strings.Join(a.Mismatch[:min(len(a.Mismatch), 5)], ", "), errTreeMismatch))
			}
			s.log.Warn("a boost holds other content than the box's under the same records; sending it again", "environment", e.String(), "paths", len(a.Mismatch))
			forced, checked = a.Mismatch[:min(len(a.Mismatch), forcedKept)], true
			continue
		}
		st := &syncStats{Tree: m.Tree, Sent: m.Sent, Took: s.clock.Since(started), Bytes: m.Bytes, Files: m.Files, Deleted: m.Deleted,
			Full: full, Shared: shared, Over: m.Over}
		for _, g := range m.Links {
			st.Submodules = append(st.Submodules, g.Path)
		}
		s.pruneSent(ctx, e.p, e.ref, boxKey, m.Sent)
		s.log.Info("synced a box to its boost", "environment", e.String(), "files", st.Files, "deleted", st.Deleted, "bytes", st.Bytes,
			"over", len(st.Over), "full", st.Full, "shared", st.Shared, "took", st.Took)
		return st, nil
	}
}

// writeBackBeforeStop writes back, before the box's instance stops, what
// its boost's ended processes changed: as a job, once for the boost's
// instance; done says it was, or there is nothing to. Those still running
// are lost with the boost, which the stop deletes.
func (s *Server) writeBackBeforeStop(ctx context.Context, boxKey controller.Key) (done bool, err error) {
	// Its boost, READY or going (the stop deletes it), while its instance
	// runs.
	b, ok, err := getTyped[*envv1alpha1.Boost](ctx, s, registry.Boost, resource.Key{Project: boxKey.Project, Name: boxKey.Name})
	if err != nil || !ok {
		return true, err
	}
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: boxKey.Project, Name: boostInstanceName(b.GetMetadata().GetName())})
	if err != nil || !ok || !owns(registry.Boost, b, in) || in.GetStatus().GetPhase() != envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING {
		return true, err
	}
	uid := in.GetMetadata().GetUid()
	if v, ok := s.stopWrittenBack.Load(boxKey.String()); ok && v == uid {
		return true, nil
	}
	if n, err := s.unsettled(ctx, boxKey, in); err != nil || n == 0 {
		return err == nil, err
	}
	p, err := s.providerFor(b.GetSpec().GetTarget())
	if err != nil {
		// Its target's provider is not there: nothing can be written back,
		// and the stop does not wait on it.
		s.log.Warn("writing a box's boosted processes back before it stops: no provider", "box", boxKey.String(), "error", err)
		return true, nil //nolint:nilerr // the stop goes on; what is not written back says so once its boost goes
	}
	s.startJob(b.GetSpec().GetTarget(), "boost-stop-writeback/"+boxKey.String(), s.boxCtrl, boxKey, func(ctx context.Context) error {
		if err := s.settleInstance(ctx, boxKey, b, in, p); err != nil {
			// Its stop does not wait on a write-back that fails: what is not
			// written back says so once its boost goes.
			s.log.Warn("writing a box's boosted processes back before it stops failed", "box", boxKey.String(), "error", err)
		}
		s.stopWrittenBack.Store(boxKey.String(), uid)
		return nil
	})
	return false, nil
}

// unsettled counts the box's processes on the boost's instance in whose
// changes are not written back yet, running or ended.
func (s *Server) unsettled(ctx context.Context, boxKey controller.Key, in *envv1alpha1.Instance) (int, error) {
	var rec boxProcesses
	if _, err := s.getRecord(ctx, boxProcessesKey(boxKey), &rec); err != nil {
		return 0, err
	}
	n := 0
	for _, bp := range rec.Boost {
		if bp.InstanceUID == in.GetMetadata().GetUid() && bp.RunID != "" && !bp.Settled && !bp.Gone {
			n++
		}
	}
	return n, nil
}

// pruneSent removes the box's sent manifests none needs: all but its last
// sync's, and those of its processes not settled yet, whose write-backs
// compare the box with them.
func (s *Server) pruneSent(ctx context.Context, p provider.Provider, boxRef provider.Instance, boxKey controller.Key, last string) {
	var procs boxProcesses
	if _, err := s.getRecord(ctx, boxProcessesKey(boxKey), &procs); err != nil {
		return
	}
	args := []string{"boost-clean", "--dir", workspace, "--prune-sent"}
	if last != "" {
		args = append(args, "--keep", last)
	}
	for _, bp := range procs.Boost {
		if !bp.Settled && bp.SyncID != "" && !bp.Gone {
			args = append(args, "--keep", bp.SyncID)
		}
	}
	if code, _, msg, err := guestRun(ctx, p, boxRef, args...); err != nil || code != 0 {
		s.log.Warn("removing a box's old sent manifests failed", "box", boxKey.String(), "error", err, "stderr", msg)
	}
}

// boostRunsSomething says whether a process of the box runs on its
// boost's instance in, as the instance lists them now.
func (s *Server) boostRunsSomething(ctx context.Context, p provider.Provider, boxKey controller.Key, in *envv1alpha1.Instance) (bool, error) {
	ps, err := p.ListProcesses(ctx, provider.Instance{Project: boxKey.Project, Name: in.GetMetadata().GetName()})
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(ps, func(p provider.Process) bool { return p.Exit == nil }), nil
}

// errNoHead is a boost lacking the box's HEAD, or what its bundle needs.
var errNoHead = errors.New("the boost lacks the box's HEAD")

// errTreeMismatch is content on the boost that is not the box's, under the
// same records, sent again.
var errTreeMismatch = errors.New("the boost made another tree than the one sent")

// applySync brings the pack at stage in the box to the boost and applies
// it there: the box's HEAD's commits first, from the mirror, unless the
// boost has them from the last sync (headKnown).
func (s *Server) applySync(ctx context.Context, e *environment, boostRef provider.Instance, stage string, m *guestbin.Manifest, headKnown, shared bool) (*guestbin.Applied, error) {
	if m.Head != "" && !headKnown {
		mirror := mirrorName(e.key.Project, e.box.GetSpec().GetCheckouts()[0].GetRepository())
		if err := e.p.UpdateOrigin(ctx, boostRef, mirror, workspace); err != nil {
			return nil, providerError(e, fmt.Errorf("fetching the box's commits to its boost: %w", err))
		}
	}
	if err := e.p.Copy(ctx, e.ref, stage, boostRef, stage); err != nil {
		return nil, providerError(e, fmt.Errorf("copying the box's changes to its boost: %w", err))
	}
	args := []string{"boost-apply", "--dir", workspace, "--staging", stage}
	if shared {
		args = append(args, "--shared")
	}
	code, out, msg, err := guestRun(ctx, e.p, boostRef, args...)
	switch {
	case err != nil:
		return nil, providerError(e, fmt.Errorf("applying the box's changes on its boost: %w", err))
	case code == guestbin.ExitHeadMissing:
		return nil, fmt.Errorf("%w: %s", errNoHead, msg)
	case code != 0:
		return nil, providerError(e, fmt.Errorf("applying the box's changes on its boost: exit %d: %s: %w", code, msg, provider.ErrFailed))
	}
	var a guestbin.Applied
	if err := json.Unmarshal(out, &a); err != nil {
		return nil, fmt.Errorf("boost-apply's answer: %w", err)
	}
	return &a, nil
}

// boostJobCommand is command run under boost-job: it says what it
// changed, ignored files too, once it ends.
func boostJobCommand(report string, command []string) []string {
	return append([]string{provider.GuestBinary, "boost-job", "--dir", workspace, "--report", report, "--"}, command...)
}

// withoutBoostJob is command without the boost-job it runs under, if it
// does: its flags come in pairs up to "--".
func withoutBoostJob(command []string) []string {
	if len(command) < 2 || command[0] != provider.GuestBinary || command[1] != "boost-job" {
		return command
	}
	i := 2
	for i+1 < len(command) && command[i] != "--" {
		i += 2
	}
	if i < len(command) && command[i] == "--" {
		return command[i+1:]
	}
	return command
}

// reportPath is where a boosted run's boost-job writes its report.
func reportPath(runID string) string { return path.Join(boostDir, "job-"+runID+".json") }

// settling holds the settling of each boost process, so it is done once.
type settling struct{ m sync.Map }

func (st *settling) lock(id string) func() {
	v, _ := st.m.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return func() {
		// Its id goes once settled, so the map does not grow: a settling
		// begun meanwhile finds the process settled in the record, whose
		// write takes one settling alone.
		st.m.CompareAndDelete(id, mu)
		mu.Unlock()
	}
}

// legacyRun is what a process started under an earlier boost-job, whose
// report says nothing to bring back, is said to have done.
const legacyRun = "it started before this tower, which brings a boosted process's changes back: nothing was brought back"

// settleBoostProcess, once, for an ended boost process: reads its report
// on boostRef, writes what it changed back to the box, removes the
// report, and keeps the result in boxProcesses. What fails is said in
// the result, never in the process's exit code. It returns the process
// as kept.
func (s *Server) settleBoostProcess(ctx context.Context, p provider.Provider, boxKey controller.Key, boostRef provider.Instance, id string) (boostProcess, error) {
	defer s.boostSettling.lock(boxKey.String() + "/" + id)()
	var rec boxProcesses
	if _, err := s.getRecord(ctx, boxProcessesKey(boxKey), &rec); err != nil {
		return boostProcess{}, err
	}
	bp, ok := rec.find(id)
	if !ok || bp.Settled || bp.RunID == "" {
		return bp, nil
	}
	run := &runResult{}
	var buf bytes.Buffer
	switch err := p.ReadFile(ctx, boostRef, reportPath(bp.RunID), &buf); {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, provider.ErrNotFound):
		run.Error = "it wrote no report: its wrapper was killed, or its boost went; nothing was brought back"
	case err != nil:
		return bp, err
	default:
		var r guestbin.Report
		switch err := json.Unmarshal(buf.Bytes(), &r); {
		case err != nil:
			run.Error = "its report could not be read: " + err.Error()
		case r.Version < guestbin.ReportVersion || bp.SyncID == "":
			run.Error = legacyRun
		case r.Error != "":
			run.Error = r.Error
			run.Back = &writeBack{HeadMoved: r.HeadMoved, IndexChanged: r.IndexChanged, Error: "its changes could not be read: nothing was brought back"}
		default:
			back, err := s.writeBack(ctx, p, boxKey, boostRef, bp, r)
			if err != nil {
				// Not settled: settled again once its box is ready.
				return bp, err
			}
			run.Back = back
		}
	}
	// Settled before its report goes: one settled again after a crash
	// writes back again, where what it wrote is the box's now, in conflict.
	err := changeRecord(ctx, s, boxProcessesKey(boxKey), func(r *boxProcesses) bool {
		for i := range r.Boost {
			if r.Boost[i].ID == id && !r.Boost[i].Settled {
				r.Boost[i].Run, r.Boost[i].Settled = run, true
				bp = r.Boost[i]
				r.trimChanged()
				return true
			}
		}
		return false
	})
	if err != nil {
		return bp, err
	}
	report := path.Base(reportPath(bp.RunID))
	if code, _, msg, err := guestRun(ctx, p, boostRef, "boost-clean", "--dir", workspace, "--name", report,
		"--name", strings.TrimSuffix(report, ".json")+".changes"); err != nil || code != 0 {
		s.log.Warn("removing a boost process's report failed", "box", boxKey.String(), "process", id, "error", err, "stderr", msg)
	}
	return bp, nil
}

// writeBack writes what the process bp changed on boostRef back to its
// box: packed on the boost (boost-back), copied, and applied in the box
// (boost-writeback) against the box as the process's sync sent it, so a
// path the box changed since stays the box's. A box not running gets
// nothing; what fails is said in the result.
func (s *Server) writeBack(ctx context.Context, p provider.Provider, boxKey controller.Key, boostRef provider.Instance, bp boostProcess, r guestbin.Report) (*writeBack, error) {
	wb := &writeBack{HeadMoved: r.HeadMoved, IndexChanged: r.IndexChanged}
	started := s.clock.Now()
	defer func() { wb.Took = s.clock.Since(started) }()
	box, ok, err := getTyped[*envv1alpha1.Box](ctx, s, registry.Box, resource.Key{Project: boxKey.Project, Name: boxKey.Name})
	if err != nil {
		return nil, err
	}
	switch phase := box.GetStatus().GetPhase(); {
	case !ok || box.GetMetadata().GetDeleteTime() != nil:
		wb.Error = "the box is gone: nothing was brought back"
		return wb, nil
	case phase == envv1alpha1.BoxPhase_BOX_PHASE_READY:
		// Running, or about to stop once its boost's ended processes are
		// written back (writeBackBeforeStop).
	case phase == envv1alpha1.BoxPhase_BOX_PHASE_STOPPED || box.GetStatus().GetStoppedReason() != "" ||
		box.GetSpec().GetRunState() == envv1alpha1.RunState_RUN_STATE_STOPPED:
		// Stopped, its boost's ended processes written back before: this one
		// ended after, or still ran.
		wb.Error = "the box was stopped when it ended: nothing was brought back"
		return wb, nil
	case phase == envv1alpha1.BoxPhase_BOX_PHASE_FAILED:
		wb.Error = "the box failed: nothing was brought back"
		return wb, nil
	default:
		// Starting, or made again: written back once it is ready.
		return nil, fmt.Errorf("box %s is %s: its boosted process's changes are written back once it is ready: %w",
			boxKey, strings.TrimPrefix(phase.String(), "BOX_PHASE_"), errWriteBackLater)
	}
	boxRef := provider.Instance{Project: boxKey.Project, Name: box.GetStatus().GetInstance()}
	name := "sync-back-" + bp.RunID
	stage := path.Join(boostDir, name)
	clean := func(ref provider.Instance) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, _, msg, err := guestRun(cctx, p, ref, "boost-clean", "--dir", workspace, "--name", name); err != nil || msg != "" {
			s.log.Warn("removing a write-back's staging failed", "box", boxKey.String(), "instance", ref.Name, "error", err, "stderr", msg)
		}
	}
	code, out, msg, err := guestRun(ctx, p, boostRef, "boost-back", "--dir", workspace, "--report", reportPath(bp.RunID),
		"--out", stage, "--max", strconv.FormatInt(s.backLimit(), 10))
	if err != nil || code != 0 {
		clean(boostRef)
		wb.Error = fmt.Sprintf("its changes could not be packed on the boost: %s", cmp.Or(msg, fmt.Sprint(err), fmt.Sprintf("exit %d", code)))
		return wb, nil
	}
	var back guestbin.Back
	if err := json.Unmarshal(out, &back); err != nil {
		clean(boostRef)
		wb.Error = "boost-back's answer: " + err.Error()
		return wb, nil
	}
	wb.Moved, wb.Total, wb.Over = back.Moved, back.Total, back.Over
	if back.Total == 0 {
		clean(boostRef)
		return wb, nil
	}
	err = p.Copy(ctx, boostRef, stage, boxRef, stage)
	clean(boostRef)
	if err != nil {
		clean(boxRef)
		wb.Error = "its changes could not be copied to the box: " + err.Error()
		wb.Moved, wb.Total = nil, 0
		return wb, nil
	}
	var sync boostSyncRecord
	_, _ = s.getRecord(ctx, boostSyncKey(boxKey), &sync)
	args := []string{"boost-writeback", "--dir", workspace, "--staging", stage, "--sent", bp.SyncID}
	if sync.LastSent != "" {
		args = append(args, "--last", sync.LastSent)
	}
	code, out, msg, err = guestRun(ctx, p, boxRef, args...)
	if err != nil || code != 0 {
		clean(boxRef)
		wb.Error = fmt.Sprintf("its changes could not be written back: %s", cmp.Or(msg, fmt.Sprint(err), fmt.Sprintf("exit %d", code)))
		wb.Moved, wb.Total = nil, 0
		return wb, nil
	}
	var w guestbin.WroteBack
	if err := json.Unmarshal(out, &w); err != nil {
		wb.Error = "boost-writeback's answer: " + err.Error()
		return wb, nil
	}
	wb.Files, wb.Deleted, wb.Bytes, wb.Conflicts = w.Files, w.Deleted, w.Bytes, w.Conflicts
	// What moved is what was written: a conflict stayed the box's, and a
	// path the box already had as the process left it, another process's
	// write-back among them, moved nothing.
	if skip := len(w.Conflicts) + len(w.Unchanged); skip > 0 {
		wb.Moved = slices.DeleteFunc(wb.Moved, func(m guestbin.Moved) bool {
			return slices.Contains(w.Conflicts, m.Path) || slices.Contains(w.Unchanged, m.Path)
		})
		wb.Total = max(wb.Total-skip, len(wb.Moved))
	}
	// The sent manifests none needs are pruned by the next sync, under the
	// box's sync lock.
	return wb, nil
}

// errWriteBackLater is a write-back for a box not ready yet: the process
// stays unsettled, and is settled again.
var errWriteBackLater = fmt.Errorf("written back once the box is ready: %w", controller.ErrWaiting)

// syncRun is one boosted start's sync, run as a job: done once it ends.
type syncRun struct {
	id   string
	done chan struct{}
	// instance is the boost's instance it synced to, by uid; finished
	// when it ended, on tower's clock.
	instance string
	finished time.Time
	stats    *syncStats
	err      error
}

// startSync starts the sync of e's box to its boost's instance in, as a
// job, holding the box in use until it ends, under the box's sync lock,
// which a start on the boost holds too. A sync that succeeds is the box's
// last applied (tq.lastSync).
func (s *Server) startSync(e *environment, b *envv1alpha1.Boost, in *envv1alpha1.Instance, tq *targetBoosts, boxKey controller.Key) *syncRun {
	run := &syncRun{id: randomID(), done: make(chan struct{}), instance: in.GetMetadata().GetUid()}
	release := s.beginUse(e.key.Project, e.key.Name)
	lock := tq.syncLock(boxKey)
	job := "boost-sync/" + boxKey.String() + "/" + run.id
	started := s.jobs.start(s.runCtx, job, s.boostCtrl, boxKey, func(ctx context.Context) error {
		defer release()
		defer close(run.done)
		tq.mu.Lock()
		tq.syncWaiting[boxKey]++
		tq.mu.Unlock()
		lock.lock()
		tq.mu.Lock()
		tq.syncWaiting[boxKey]--
		tq.mu.Unlock()
		defer lock.unlock()
		run.stats, run.err = s.syncBoost(ctx, e, b, in, tq)
		run.finished = s.clock.Now()
		if run.err == nil {
			tq.mu.Lock()
			tq.lastSync[boxKey] = run.id
			tq.mu.Unlock()
		}
		return run.err
	})
	if !started {
		release()
		run.err = errors.New("the sync could not start")
		close(run.done)
	}
	return run
}

func (s *Server) backLimit() int64 { return cmp.Or(s.backMax, boostBackMax) }

func (s *Server) syncWaitFor() time.Duration {
	return cmp.Or(time.Duration(s.syncWait.Load()), boostSyncWait)
}

// syncValidFor is how long a sync that ended stays good for its start,
// asked again: the inline wait and a margin, so a caller asking again
// every 10 seconds, as it should, starts from it.
func (s *Server) syncValidFor() time.Duration { return max(s.syncWaitFor(), 0) + syncValidMargin }

// syncValidMargin is how much longer than the inline wait a sync that
// ended is good for.
const syncValidMargin = 30 * time.Second

// waitSync waits for run within wait, or ctx.
func waitSync(ctx context.Context, run *syncRun, wait time.Duration) bool {
	if wait <= 0 {
		select {
		case <-run.done:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-run.done:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
}

// syncError is what a boosted start answers for its sync's failure.
func syncError(e *environment, err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	return providerError(e, fmt.Errorf("syncing the box to its boost: %w", err))
}
