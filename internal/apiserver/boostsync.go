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
// starts, the box's work tree is packed in the box (tower-guest
// boost-pack), copied to the boost through Provider.Copy, and applied
// there (boost-apply); the process runs under boost-job, whose report and
// artifacts tower reads once it ends.

const (
	// boostSyncMax is the most one sync sends.
	boostSyncMax = 2 << 30
	// boostSyncWait is how long a boosted start waits for its sync before
	// it answers pending SYNCING.
	boostSyncWait = 10 * time.Second
	// boostArtifactsMax is the most a process's artifacts copy back,
	// together.
	boostArtifactsMax = 1 << 30
	// boostSyncKind keeps, per box, the tree last sent to its boost.
	boostSyncKind = "BoostSync"
	// checkoutCommitAnnotation is, on a boost's instance, the commit its
	// first repository was checked out at: the base of its first sync.
	checkoutCommitAnnotation = registry.Domain + "/checkout-commit"
	// workspace is where a box's first repository is, and its boost's.
	workspace = "/workspace"
)

// boostDir is where the boost subcommands keep their files, in a
// checkout's git directory.
var boostDir = path.Join(workspace, ".git", guestbin.BoostDir)

// boostSyncRecord is what boostSyncKind holds: the tree last sent to the
// box's boost, which a later sync sends what changed since; it counts for
// that boost's instance alone.
type boostSyncRecord struct {
	BoostUID    string `json:"boostUid"`
	InstanceUID string `json:"instanceUid"`
	Tree        string `json:"tree"`
	Head        string `json:"head"`
}

func boostSyncKey(box controller.Key) store.Key {
	return store.Key{Kind: boostSyncKind, Project: box.Project, Name: box.Name}
}

// syncStats are what a sync did, as a process's exit says them.
type syncStats struct {
	Tree       string        `json:"tree,omitempty"`
	Took       time.Duration `json:"took"`
	Bytes      int64         `json:"bytes"`
	Files      int           `json:"files"`
	Deleted    int           `json:"deleted"`
	Full       bool          `json:"full,omitempty"`
	Shared     bool          `json:"shared,omitempty"`
	Submodules []string      `json:"submodules,omitempty"`
}

// runResult is what a boosted process's run left once it ended: what it
// changed on the boost, and its artifacts.
type runResult struct {
	Changed   []guestbin.Changed `json:"changed,omitempty"`
	Total     int                `json:"total"`
	Artifacts []artifactResult   `json:"artifacts,omitempty"`
	Error     string             `json:"error,omitempty"`
}

type artifactResult struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	Copied bool   `json:"copied"`
	Error  string `json:"error,omitempty"`
}

// boostRunOf is what the API says of a boost process's run.
func boostRunOf(bp boostProcess) *envv1alpha1.BoostRun {
	if bp.Sync == nil && bp.Run == nil {
		return nil
	}
	r := &envv1alpha1.BoostRun{}
	if s := bp.Sync; s != nil {
		r.Sync, r.SyncBytes, r.SyncFiles, r.SyncDeleted = durationpb.New(s.Took), s.Bytes, int32(s.Files), int32(s.Deleted)
		r.SyncFull, r.BaseShared, r.SubmodulesNotSynced = s.Full, s.Shared, s.Submodules
	}
	if run := bp.Run; run != nil {
		r.Error = run.Error
		r.Changed = &envv1alpha1.ChangedFiles{Total: int32(run.Total), Truncated: run.Total > len(run.Changed)}
		for _, c := range run.Changed {
			r.Changed.Files = append(r.Changed.Files, &envv1alpha1.ChangedFile{Path: c.Path, Status: c.Status})
		}
		for _, a := range run.Artifacts {
			r.Artifacts = append(r.Artifacts, &envv1alpha1.ArtifactResult{Path: a.Path, Bytes: a.Bytes, Copied: a.Copied, Error: a.Error})
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

// errSyncTooLarge is a sync past boostSyncMax, which no start sends.
var errSyncTooLarge = errors.New("the box's changes are too large to sync")

// syncBoost sends the box's work tree to its boost's instance in: what
// changed since the tree last sent to it (boostSyncKind), or since the
// commit it was checked out at, or the whole tree; a send the boost has
// no base for is sent again whole, once, in the call. shared says a
// process of the box runs on the boost: what it wrote stays. A box with
// no checkout has no work tree to send.
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
		// What the ended ones changed and copied back, before the sync puts
		// their writes back; if that fails, their writes stay.
		if err := s.settleInstance(ctx, boxKey, b, in, e.p); err != nil {
			s.log.Warn("settling a boost's ended processes before a sync failed; what they wrote stays", "environment", e.String(), "error", err)
			shared = true
		}
	}
	var rec boostSyncRecord
	if _, err := s.getRecord(ctx, boostSyncKey(boxKey), &rec); err != nil {
		return nil, err
	}
	known := rec.BoostUID == b.GetMetadata().GetUid() && rec.InstanceUID == in.GetMetadata().GetUid() && rec.Tree != ""
	base := in.GetMetadata().GetAnnotations()[checkoutCommitAnnotation]
	if known {
		base = rec.Tree
	}
	bundleAll, wholeTree := false, false
	for {
		id := "sync-" + randomID()
		stage := path.Join(boostDir, id)
		args := []string{"boost-pack", "--dir", workspace, "--base", base, "--out", stage, "--max", strconv.Itoa(boostSyncMax)}
		if bundleAll {
			args = append(args, "--bundle-all")
		}
		code, out, msg, err := guestRun(ctx, e.p, e.ref, args...)
		clean := func() {
			// Removed whatever came of the sync; a crash's leftovers are
			// swept by the next.
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if _, _, msg, err := guestRun(cctx, e.p, e.ref, "boost-clean", "--dir", workspace, "--name", id); err != nil || msg != "" {
				s.log.Warn("removing a box's sync staging failed", "environment", e.String(), "error", err, "stderr", msg)
			}
		}
		switch {
		case err != nil:
			clean()
			return nil, providerError(e, fmt.Errorf("packing the box's work tree: %w", err))
		case code == guestbin.ExitTooLarge:
			clean()
			return nil, failedPrecondition("%s: %s (%v)", e, msg, errSyncTooLarge)
		case code != 0:
			clean()
			return nil, providerError(e, fmt.Errorf("packing the box's work tree: exit %d: %s: %w", code, msg, provider.ErrFailed))
		}
		var m guestbin.Manifest
		if err := json.Unmarshal(out, &m); err != nil {
			clean()
			return nil, fmt.Errorf("boost-pack's answer: %w", err)
		}
		_, err = s.applySync(ctx, e, boostRef, stage, &m, known && rec.Head == m.Head && !bundleAll, shared)
		clean()
		switch {
		case (errors.Is(err, errNoBase) || errors.Is(err, errTreeMismatch)) && !wholeTree && !m.Full:
			// The boost lacks the base, or made another tree: the whole
			// tree, once.
			s.log.Warn("a boost lacks its sync's base; sending the whole tree", "environment", e.String(), "base", base, "error", err)
			if err := s.deleteRecord(ctx, boostSyncKey(boxKey)); err != nil {
				return nil, err
			}
			base, known, wholeTree = "", false, true
			continue
		case errors.Is(err, errTreeMismatch):
			// Even the whole tree made another: tower's fault, or the
			// boost's checkout's, never the box's.
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("%s: syncing the box to its boost: the whole tree sent made another tree there: %w", e, err))
		case errors.Is(err, errNoHead) && !bundleAll:
			// The boost lacks what the bundle needs: HEAD's whole history,
			// once.
			s.log.Warn("a boost lacks what its sync's bundle needs; sending HEAD's history", "environment", e.String(), "error", err)
			bundleAll = true
			continue
		case err != nil:
			return nil, err
		}
		if err := s.putRecord(ctx, boostSyncKey(boxKey), boostSyncRecord{
			BoostUID: b.GetMetadata().GetUid(), InstanceUID: in.GetMetadata().GetUid(), Tree: m.Tree, Head: m.Head,
		}); err != nil {
			return nil, err
		}
		st := &syncStats{Tree: m.Tree, Took: s.clock.Since(started), Bytes: m.Bytes, Files: m.Files, Deleted: m.Deleted, Full: m.Full, Shared: shared}
		for _, g := range m.Links {
			st.Submodules = append(st.Submodules, g.Path)
		}
		s.log.Info("synced a box to its boost", "environment", e.String(), "files", st.Files, "deleted", st.Deleted, "bytes", st.Bytes,
			"full", st.Full, "shared", st.Shared, "took", st.Took)
		return st, nil
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

// errNoBase is a sync's base the boost lacks.
var errNoBase = errors.New("the boost lacks the sync's base")

// errTreeMismatch is a tree the boost made that is not the one sent.
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
	case code == guestbin.ExitBaseMissing:
		return nil, fmt.Errorf("%w: %s", errNoBase, msg)
	case code == guestbin.ExitTreeMismatch:
		return nil, fmt.Errorf("%w: %s", errTreeMismatch, msg)
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
// changed relative to tree, and measures artifacts, once it ends.
func boostJobCommand(tree, report string, artifacts, command []string) []string {
	out := []string{provider.GuestBinary, "boost-job", "--dir", workspace, "--tree", tree, "--report", report}
	for _, a := range artifacts {
		out = append(out, "--artifact", a)
	}
	return append(append(out, "--"), command...)
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

// checkArtifacts refuses artifacts that are no paths in /workspace, or
// are in a .git directory, its own or a submodule's.
func checkArtifacts(artifacts []string) error {
	for _, a := range artifacts {
		clean := path.Clean(a)
		if a == "" || path.IsAbs(a) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") ||
			slices.Contains(strings.Split(clean, "/"), ".git") {
			return invalid("artifact %q is no path in /workspace: one relative to it, not climbing out, not in a .git directory", a)
		}
	}
	return nil
}

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

// settleBoostProcess, once, for an ended boost process: reads what it
// changed and its artifacts from its report on boostRef, copies its
// artifacts back to the box (up to boostArtifactsMax), removes the
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
		run.Error = "it wrote no report: its wrapper was killed, or its boost went"
	case err != nil:
		return bp, err
	default:
		var r guestbin.Report
		if err := json.Unmarshal(buf.Bytes(), &r); err != nil {
			run.Error = "its report could not be read: " + err.Error()
		} else {
			run.Changed, run.Total, run.Error = r.Changed, r.Total, r.Error
			run.Artifacts = s.copyArtifacts(ctx, p, boxKey, boostRef, r.Artifacts)
		}
	}
	for _, a := range bp.Artifacts {
		if !slices.ContainsFunc(run.Artifacts, func(r artifactResult) bool { return r.Path == a }) {
			run.Artifacts = append(run.Artifacts, artifactResult{Path: a, Error: "not measured: " + run.Error})
		}
	}
	// Settled before its report goes: one settled again after a crash
	// reads the report, and copies its artifacts, again.
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
	if code, _, msg, err := guestRun(ctx, p, boostRef, "boost-clean", "--dir", workspace, "--name", path.Base(reportPath(bp.RunID))); err != nil || code != 0 {
		s.log.Warn("removing a boost process's report failed", "box", boxKey.String(), "process", id, "error", err, "stderr", msg)
	}
	return bp, nil
}

// copyArtifacts copies measured artifacts back to the box, in order, up
// to boostArtifactsMax together.
func (s *Server) copyArtifacts(ctx context.Context, prov provider.Provider, boxKey controller.Key, boostRef provider.Instance, measured []guestbin.Artifact) []artifactResult {
	var out []artifactResult
	if len(measured) == 0 {
		return nil
	}
	var boxRef provider.Instance
	box, ok, err := getTyped[*envv1alpha1.Box](ctx, s, registry.Box, resource.Key{Project: boxKey.Project, Name: boxKey.Name})
	running := err == nil && ok && box.GetStatus().GetPhase() == envv1alpha1.BoxPhase_BOX_PHASE_READY
	if running {
		boxRef = provider.Instance{Project: boxKey.Project, Name: box.GetStatus().GetInstance()}
	}
	// Measured again right before they are copied: what the report says
	// was measured when the process ended, and what is there now may have
	// grown, or become a link out of /workspace.
	var paths []string
	for _, a := range measured {
		if a.Error == "" && a.Exists {
			paths = append(paths, a.Path)
		}
	}
	now, merr := s.measureArtifacts(ctx, prov, boostRef, paths)
	var total int64
	for _, a := range measured {
		if a.Error == "" && a.Exists {
			switch m, ok := now[a.Path]; {
			case merr != nil:
				a.Error = "it could not be measured again: " + merr.Error()
			case !ok:
				a.Error = "it could not be measured again"
			default:
				a = m
			}
		}
		r := artifactResult{Path: a.Path, Bytes: a.Bytes, Error: a.Error}
		switch {
		case a.Error != "" || !a.Exists:
			r.Error = "not copied: " + cmp.Or(a.Error, "not there")
		case total+a.Bytes > s.artifactsLimit():
			r.Error = fmt.Sprintf("not copied: past the %d bytes artifacts copy back together", s.artifactsLimit())
		case !running:
			r.Error = "not copied: the box is not running"
		default:
			p := path.Join(workspace, path.Clean(a.Path))
			pr := prov
			if err := pr.Copy(ctx, boostRef, p, boxRef, p); err != nil {
				r.Error = "not copied: " + err.Error()
			} else {
				r.Copied, total = true, total+a.Bytes
			}
		}
		out = append(out, r)
	}
	return out
}

// measureArtifacts measures paths on the boost, as boost-job does.
func (s *Server) measureArtifacts(ctx context.Context, p provider.Provider, boostRef provider.Instance, paths []string) (map[string]guestbin.Artifact, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	args := []string{"boost-measure", "--dir", workspace}
	for _, a := range paths {
		args = append(args, "--artifact", a)
	}
	code, out, msg, err := guestRun(ctx, p, boostRef, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("exit %d: %s", code, msg)
	}
	var as []guestbin.Artifact
	if err := json.Unmarshal(out, &as); err != nil {
		return nil, err
	}
	m := map[string]guestbin.Artifact{}
	for _, a := range as {
		m[a.Path] = a
	}
	return m, nil
}

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

func (s *Server) artifactsLimit() int64 { return cmp.Or(s.artifactsMax, boostArtifactsMax) }

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
