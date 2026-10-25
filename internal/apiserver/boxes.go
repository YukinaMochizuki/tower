package apiserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/devcontainer"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// A box's conditions, all the box controller's.
const (
	BoxImageBuilt    = "ImageBuilt"
	BoxMaterialized  = "Materialized"
	BoxReady         = "Ready"
	stoppedIdle      = "Idle"
	stoppedRequested = "Requested"
	// materializedAnnotation marks an instance whose checkouts are made
	// (and, on the slow path, its dependencies installed).
	materializedAnnotation = registry.Domain + "/materialized"
)

// errMirrorNotFetched waits for a repository's mirror on the target.
var errMirrorNotFetched = fmt.Errorf("the repository's mirror is not fetched yet: %w", provider.ErrUnavailable)

// boxState is the status a reconcile of a box builds, written once.
type boxState struct {
	s  *Server
	st *envv1alpha1.BoxStatus
	// idleStop says the reconcile decided to stop the box for idleness, at
	// its use epoch idleEpoch: the status is written only if the box was
	// not used since (commitIdleStop).
	idleStop  bool
	idleEpoch uint64
}

func (b boxState) cond(typ string, ok bool, reason, msg string) {
	c := &metav1.Condition{Type: typ, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg, ObservedGeneration: b.st.GetObservedGeneration()}
	if ok {
		c.Status = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	}
	b.st.Conditions = conditions.Set(slices.Clone(b.st.GetConditions()), c, b.s.clock.Now())
}

// not puts the box in phase, not ready for reason.
func (b boxState) not(phase envv1alpha1.BoxPhase, reason, msg string) {
	b.st.Phase = phase
	b.cond(BoxReady, false, reason, msg)
}

func (s *Server) reconcileBox(ctx context.Context, key controller.Key) error {
	box, ok, err := getTyped[*envv1alpha1.Box](ctx, s, registry.Box, resource.Key{Project: key.Project, Name: key.Name})
	if err != nil || !ok || box.GetMetadata().GetDeleteTime() != nil {
		return err
	}
	st := proto.Clone(box.GetStatus()).(*envv1alpha1.BoxStatus)
	if st == nil {
		st = &envv1alpha1.BoxStatus{}
	}
	st.ObservedGeneration = box.GetMetadata().GetGeneration()
	bs := &boxState{s: s, st: st}
	waitErr := s.boxSteps(ctx, key, box, bs)
	write := func() error { return s.updateStatus(ctx, registry.Box, box, st) }
	if !bs.idleStop {
		if err := write(); err != nil {
			return err
		}
		return waitErr
	}
	if hook := s.beforeIdleStop.Load(); hook != nil {
		(*hook)(key)
	}
	stopped, err := s.commitIdleStop(key, bs.idleEpoch, write)
	if err != nil {
		return err
	}
	if !stopped {
		// Used as it was decided: decided again from what it is now.
		s.boxCtrl.Enqueue(key)
		return nil
	}
	s.log.Info("stopping a box unused for its target's idle_stop_after", "project", key.Project, "box", key.Name)
	return waitErr
}

// boxSteps brings the box along as far as it can go now, saying in bs
// where it is; its error is one to retry.
func (s *Server) boxSteps(ctx context.Context, key controller.Key, box *envv1alpha1.Box, bs *boxState) error {
	st, spec := bs.st, box.GetSpec()
	if st.Phase == envv1alpha1.BoxPhase_BOX_PHASE_UNSPECIFIED {
		st.Phase = envv1alpha1.BoxPhase_BOX_PHASE_PENDING
	}
	// Where it runs is decided once: a placement changed later moves only
	// boxes made after.
	var target *targetsv1alpha1.Target
	if st.Target == "" {
		t, reason, err := s.placedTarget(ctx, key.Project)
		if err != nil {
			return err
		}
		if t == nil {
			bs.not(envv1alpha1.BoxPhase_BOX_PHASE_UNPLACED, "Unplaced", reason)
			return nil
		}
		target, st.Target = t, t.GetMetadata().GetName()
	} else {
		t, live, err := s.liveTarget(ctx, st.Target)
		if err != nil {
			return err
		}
		if !live {
			bs.not(envv1alpha1.BoxPhase_BOX_PHASE_FAILED, "TargetGone", fmt.Sprintf("its target %q is gone", st.Target))
			return nil
		}
		target = t
	}
	p, err := s.providerFor(st.Target)
	if err != nil {
		bs.cond(BoxReady, false, "TargetUnavailable", err.Error())
		return fmt.Errorf("box %s: %w: %w", key, controller.ErrWaiting, err)
	}

	// Its image.
	if st.ImageGolden == "" {
		name, reason, err := s.boxImageGolden(ctx, key.Project, st.Target, spec)
		if err != nil {
			return err
		}
		if name == "" {
			bs.not(envv1alpha1.BoxPhase_BOX_PHASE_PENDING, reason, "")
			return nil
		}
		st.ImageGolden = name
	}
	img, ok, err := s.liveGolden(ctx, key.Project, st.ImageGolden)
	if err != nil {
		return err
	}
	if !ok {
		// Pruned, or deleted to be built again, before the box ran:
		// resolve it again.
		st.ImageGolden = ""
		bs.not(envv1alpha1.BoxPhase_BOX_PHASE_PENDING, "ImageGoldenGone", "its image golden is gone; it is resolved again")
		return fmt.Errorf("box %s: its image golden is gone; resolving it again: %w", key, controller.ErrWaiting)
	}
	switch goldenPhase(img) {
	case envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED:
		msg, err := s.failedGoldenMessage(ctx, img)
		if err != nil {
			return err
		}
		bs.cond(BoxImageBuilt, false, "BuildFailed", msg)
		bs.not(envv1alpha1.BoxPhase_BOX_PHASE_FAILED, "ImageBuildFailed", msg)
		return nil
	case envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY:
		bs.cond(BoxImageBuilt, true, "Built", "")
	default:
		bs.cond(BoxImageBuilt, false, "Building", "")
		bs.not(envv1alpha1.BoxPhase_BOX_PHASE_BUILDING, "Building", "its image is being built")
		return nil
	}

	// Its instance.
	name := childName("box", key.Name)
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: key.Project, Name: name})
	if err != nil {
		return err
	}
	if ok && !owns(registry.Box, box, in) {
		// An earlier box's of the same name, which goes with it.
		bs.not(envv1alpha1.BoxPhase_BOX_PHASE_STARTING, "Starting", "an earlier box's instance is still going")
		return nil
	}
	if ok && in.GetStatus().GetPhase() == envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE {
		if _, err := s.backend(registry.Instance).Delete(s.system(ctx), resource.Key{Project: key.Project, Name: name}, nil); err != nil && !isNotFound(err) {
			return err
		}
		s.log.Warn("a box's instance vanished from its target; making it again", "project", key.Project, "box", key.Name, "target", st.Target)
		bs.cond(BoxMaterialized, false, "InstanceGone", "its instance vanished from the target; it is made again")
		bs.not(envv1alpha1.BoxPhase_BOX_PHASE_STARTING, "InstanceGone", "its instance vanished from the target; it is made again")
		return nil
	}
	if !ok {
		return s.makeBoxInstance(ctx, key, box, img, name, bs)
	}
	st.Instance, st.Alias = name, key.Name

	s.processesSeen(key, in.GetStatus().GetProcessesRunning())

	// Whether it runs.
	desired, reason := envv1alpha1.RunState_RUN_STATE_RUNNING, ""
	switch {
	case spec.GetRunState() == envv1alpha1.RunState_RUN_STATE_STOPPED:
		desired, reason = envv1alpha1.RunState_RUN_STATE_STOPPED, stoppedRequested
	case st.StoppedReason == stoppedIdle:
		desired, reason = envv1alpha1.RunState_RUN_STATE_STOPPED, stoppedIdle
	case st.Phase == envv1alpha1.BoxPhase_BOX_PHASE_READY:
		if idle, epoch := s.idle(key, box, in, target); idle {
			// Decided here, written by reconcileBox unless the box is used
			// meanwhile; its instance stops on the next reconcile.
			st.StoppedReason, bs.idleStop, bs.idleEpoch = stoppedIdle, true, epoch
			bs.cond(BoxReady, false, "Stopping", "stopping: "+stoppedIdle)
			return nil
		}
	}
	st.StoppedReason = reason
	if in.GetSpec().GetRunState() != desired {
		if err := s.setRunState(ctx, in, desired); err != nil {
			return err
		}
	}
	phase := in.GetStatus().GetPhase()
	if desired == envv1alpha1.RunState_RUN_STATE_STOPPED {
		if phase == envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED {
			st.Phase = envv1alpha1.BoxPhase_BOX_PHASE_STOPPED
		}
		bs.cond(BoxReady, false, "Stopped", "stopped: "+reason)
		return nil
	}
	switch phase {
	case envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED:
		msg := conditions.Find(in.GetStatus().GetConditions(), "Ready").GetMessage()
		bs.not(envv1alpha1.BoxPhase_BOX_PHASE_FAILED, "InstanceFailed", msg)
		return nil
	case envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING:
	default:
		bs.not(envv1alpha1.BoxPhase_BOX_PHASE_STARTING, "Starting", "its instance is starting")
		return nil
	}
	if in.GetMetadata().GetAnnotations()[materializedAnnotation] != "true" {
		return s.materializeBox(ctx, key, box, in, p, bs)
	}
	bs.cond(BoxMaterialized, true, "Materialized", "")
	if st.Phase != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		repo := ""
		if cs := spec.GetCheckouts(); len(cs) > 0 {
			repo = cs[0].GetRepository()
		}
		if err := s.boxOpened(ctx, key, repo); err != nil {
			return err
		}
	}
	st.Phase = envv1alpha1.BoxPhase_BOX_PHASE_READY
	bs.cond(BoxReady, true, "Ready", "")
	return nil
}

// boxImageGolden resolves the box's image and makes its Golden, shared by
// every box of the same inputs, returning its name; "" with a reason while
// what it is resolved from is not there yet.
func (s *Server) boxImageGolden(ctx context.Context, project, target string, spec *envv1alpha1.BoxSpec) (string, string, error) {
	req := devcontainer.Request{Target: target, Image: spec.GetImage(), Addons: spec.GetAddons()}
	if req.Image == "" {
		first := spec.GetCheckouts()[0]
		repo, ok, err := getTyped[*envv1alpha1.Repository](ctx, s, registry.Repository, resource.Key{Project: project, Name: first.GetRepository()})
		if err != nil {
			return "", "", err
		}
		if !ok {
			return "", "RepositoryMissing", nil
		}
		head := mirrorHead(repo, target)
		if head == "" {
			s.fetchNow(controller.Key{Project: project, Name: first.GetRepository()})
			return "", "WaitingForMirror", nil
		}
		req.Mirror, req.Commit = mirrorName(project, first.GetRepository()), head
		if first.GetRef() != "" {
			req.Commit = first.GetRef()
		}
	}
	img, err := s.cfg.Resolver.Image(ctx, req)
	if err != nil {
		return "", "", err
	}
	inputs := &envv1alpha1.BoxImageInputs{BaseImageDigest: img.BaseImageDigest, DevcontainerHash: img.DevcontainerHash, Addons: img.Addons}
	hash := inputHash(append([]string{"box-image", target, img.BaseImageDigest, img.DevcontainerHash}, img.Addons...)...)
	name := goldenName("img", hash)
	_, err = s.ensureGolden(ctx, project, name, &envv1alpha1.GoldenSpec{
		Target: target, InputHash: hash, Inputs: &envv1alpha1.GoldenSpec_BoxImage{BoxImage: inputs},
	}, buildSpec{Image: &req}, nil)
	if err != nil {
		return "", "", err
	}
	return name, "", nil
}

// makeBoxInstance makes the box's instance from its image, with its disk
// cloned from its first repository's warm golden when one is ready.
func (s *Server) makeBoxInstance(ctx context.Context, key controller.Key, box *envv1alpha1.Box, img *envv1alpha1.Golden, name string, bs *boxState) error {
	st, spec := bs.st, box.GetSpec()
	warm := ""
	if cs := spec.GetCheckouts(); len(cs) > 0 {
		var err error
		if warm, err = s.readyWarm(ctx, key.Project, cs[0].GetRepository(), st.Target, img.GetMetadata().GetName()); err != nil {
			return err
		}
	}
	runState := envv1alpha1.RunState_RUN_STATE_RUNNING
	if spec.GetRunState() == envv1alpha1.RunState_RUN_STATE_STOPPED {
		runState = envv1alpha1.RunState_RUN_STATE_STOPPED
	}
	// Recorded before the instance names them, so pruning, which reads the
	// use, never takes a golden an instance is about to be made of.
	for _, g := range []string{img.GetMetadata().GetName(), warm} {
		if g == "" {
			continue
		}
		if err := s.goldenUsed(ctx, key.Project, g); err != nil {
			return err
		}
	}
	_, err := s.backend(registry.Instance).Create(s.system(ctx), &envv1alpha1.Instance{
		Metadata: &metav1.ObjectMeta{Project: key.Project, Name: name, OwnerReferences: []*metav1.OwnerReference{ownedBy(registry.Box, box)}},
		Spec: &envv1alpha1.InstanceSpec{
			Target: st.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, Image: img.GetStatus().GetArtifact(),
			DiskGolden: warm, Network: spec.GetWorkstream(), Alias: key.Name,
			Resources: withDefaults(envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, spec.GetResources()), RunState: runState,
		},
	})
	if err != nil && connect.CodeOf(err) != connect.CodeAlreadyExists {
		return err
	}
	st.Instance, st.WarmGolden, st.Alias = name, warm, key.Name
	bs.cond(BoxMaterialized, false, "Creating", "")
	bs.not(envv1alpha1.BoxPhase_BOX_PHASE_STARTING, "Starting", "its instance is being made")
	return nil
}

// readyWarm is the repository's newest ready warm golden on target made on
// the image golden parent, "" if none: a box of another image takes the
// slow path, since a disk cloned from it would not match its image.
func (s *Server) readyWarm(ctx context.Context, project, repo, target, parent string) (string, error) {
	goldens, err := listTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, project)
	if err != nil {
		return "", err
	}
	var best *envv1alpha1.Golden
	for _, g := range goldens {
		if g.GetMetadata().GetLabels()[warmLabel] != repo || goldenPhase(g) != envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY ||
			g.GetMetadata().GetDeleteTime() != nil || dropped(g) ||
			g.GetSpec().GetTarget() != target || g.GetSpec().GetParent() != parent {
			continue
		}
		if best == nil || g.GetMetadata().GetCreateTime().AsTime().After(best.GetMetadata().GetCreateTime().AsTime()) {
			best = g
		}
	}
	if best == nil {
		return "", nil
	}
	return best.GetMetadata().GetName(), nil
}

// materializeBox checks the box's repositories out in its running
// instance, and on the slow path, a blank disk, installs the first one's
// dependencies.
func (s *Server) materializeBox(ctx context.Context, key controller.Key, box *envv1alpha1.Box, in *envv1alpha1.Instance, p provider.Provider, bs *boxState) error {
	job := "materialize/" + key.String()
	jerr, running := s.jobs.lastError(job)
	if jerr != nil && !running {
		s.jobs.forget(job)
		if transient(jerr) {
			bs.cond(BoxMaterialized, false, "Waiting", jerr.Error())
			bs.not(envv1alpha1.BoxPhase_BOX_PHASE_STARTING, "Starting", jerr.Error())
			return fmt.Errorf("box %s: %w: %w", key, controller.ErrWaiting, jerr)
		}
		bs.cond(BoxMaterialized, false, "CheckoutFailed", jerr.Error())
		bs.not(envv1alpha1.BoxPhase_BOX_PHASE_FAILED, "CheckoutFailed", jerr.Error())
		return nil
	}
	bs.cond(BoxMaterialized, false, "CheckingOut", "")
	bs.not(envv1alpha1.BoxPhase_BOX_PHASE_STARTING, "Starting", "its repositories are being checked out")
	if running {
		return nil
	}
	target, warm := bs.st.GetTarget(), in.GetSpec().GetDiskGolden()
	ref := provider.Instance{Project: key.Project, Name: in.GetMetadata().GetName()}
	checkouts := box.GetSpec().GetCheckouts()
	s.startJob(target, job, s.boxCtrl, key, func(ctx context.Context) error {
		if done, err := s.instanceMarked(ctx, key.Project, ref.Name, materializedAnnotation); err != nil || done {
			return err
		}
		var first string
		for i, c := range checkouts {
			repo, ok, err := getTyped[*envv1alpha1.Repository](ctx, s, registry.Repository, resource.Key{Project: key.Project, Name: c.GetRepository()})
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("repository %q is gone: %w", c.GetRepository(), provider.ErrFailed)
			}
			if mirrorHead(repo, target) == "" {
				s.fetchNow(controller.Key{Project: key.Project, Name: c.GetRepository()})
				return errMirrorNotFetched
			}
			dir := "/workspace"
			if i > 0 {
				dir = "/repos/" + c.GetRepository()
			}
			commit, err := p.Checkout(ctx, ref, mirrorName(key.Project, c.GetRepository()), c.GetRef(), dir)
			if err != nil {
				return err
			}
			if i == 0 {
				first = commit
			}
		}
		if warm == "" && len(checkouts) > 0 {
			w, err := s.cfg.Resolver.Warm(ctx, target, mirrorName(key.Project, checkouts[0].GetRepository()), first)
			if err != nil {
				return err
			}
			if err := s.mustRun(ctx, p, ref, nil, "/workspace", w.Install...); err != nil {
				return err
			}
		}
		return s.annotateInstance(ctx, key.Project, ref.Name, materializedAnnotation, "true")
	})
	return nil
}

// instanceMarked reports whether an instance carries one of tower's marks:
// a job whose work an earlier one did finds it done.
func (s *Server) instanceMarked(ctx context.Context, project, name, mark string) (bool, error) {
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: project, Name: name})
	if err != nil || !ok {
		return false, err
	}
	return in.GetMetadata().GetAnnotations()[mark] == "true", nil
}

// annotateInstance sets one of tower's annotations on an instance.
func (s *Server) annotateInstance(ctx context.Context, project, name, k, v string) error {
	for range maxRetries {
		in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: project, Name: name})
		if err != nil || !ok {
			return err
		}
		in = proto.Clone(in).(*envv1alpha1.Instance)
		if in.Metadata.Annotations == nil {
			in.Metadata.Annotations = map[string]string{}
		}
		in.Metadata.Annotations[k] = v
		_, err = s.backend(registry.Instance).Apply(s.system(ctx), in)
		if connect.CodeOf(err) != connect.CodeAborted {
			return err
		}
	}
	return errors.New("too many writers of an instance")
}

// setRunState writes an instance's run_state, conditional on the
// instance as read.
func (s *Server) setRunState(ctx context.Context, in *envv1alpha1.Instance, rs envv1alpha1.RunState) error {
	in = proto.Clone(in).(*envv1alpha1.Instance)
	in.Spec.RunState = rs
	_, err := s.backend(registry.Instance).Apply(s.system(ctx), in)
	return err
}

// idle reports whether a box has gone unused for its target's
// idle_stop_after, measured from its last use or from when it last became
// ready. A box workstream keeps, one a call is under way in, and one whose
// instance reports a process running, never is.
func (s *Server) idle(key controller.Key, box *envv1alpha1.Box, in *envv1alpha1.Instance, target *targetsv1alpha1.Target) (bool, uint64) {
	touched, epoch, busy := s.useOf(key)
	if busy || box.GetSpec().GetKeep() || in.GetStatus().GetProcessesRunning() > 0 {
		return false, epoch
	}
	var last time.Time
	if t := box.GetStatus().GetLastUseTime(); t != nil {
		last = t.AsTime()
	}
	last = maxTime(last, touched)
	if c := conditions.Find(box.GetStatus().GetConditions(), BoxReady); c.GetLastTransitionTime() != nil {
		last = maxTime(last, c.GetLastTransitionTime().AsTime())
	}
	return !last.IsZero() && s.clock.Since(last) >= settingsOf(target).idleStopAfter, epoch
}
