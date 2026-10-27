package apiserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/quantity"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/devcontainer"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
)

const (
	// GoldenArtifactFinalizer keeps a deleted golden until the golden
	// controller has deleted its artifact on the target.
	GoldenArtifactFinalizer = registry.Domain + "/artifact"
	// convergedAnnotation marks a builder instance whose golden phase has
	// run, so it is captured next.
	convergedAnnotation = registry.Domain + "/converged"
	// warmLabel marks a warm golden the warm controller holds for a
	// repository, by its name; pruning leaves it to that controller.
	warmLabel = registry.Domain + "/warm-for"
	// warmDroppedAnnotation marks a warm golden the warm controller let go
	// while something still referenced it: no new box is cloned from it,
	// and it is deleted once nothing references it.
	warmDroppedAnnotation = registry.Domain + "/warm-dropped"
	// GoldenReady says a golden can be made from, or why not.
	GoldenReady = "Ready"
)

// GoldenRetryFirst and GoldenRetryMax bound how long tower waits before it
// builds a failed golden again: the first wait, doubled after each failure
// in a row of the same inputs, up to the most.
const (
	GoldenRetryFirst = 10 * time.Minute
	GoldenRetryMax   = 6 * time.Hour
)

// errGoldenGoing waits for a golden being deleted to be gone before one of
// the same name is made again.
var errGoldenGoing = fmt.Errorf("a golden of the same inputs is being deleted: %w", controller.ErrWaiting)

// buildSpec is what a golden's build reads besides its inputs: the request
// a box image is resolved from, a warm checkout's mirror, a machine's
// blueprint. tower's own, and no secret; it is kept as an internal record
// (goldenBuildKind), so hostd, which reads goldens, sees only their hashes.
type buildSpec struct {
	// Image is the request a box image is resolved from, and Env the
	// environment a box of it is given: its devcontainer's containerEnv.
	Image *devcontainer.Request `json:"image,omitempty"`
	Env   map[string]string     `json:"env,omitempty"`
	// Mirror is a warm checkout's, Branch the branch it follows.
	Mirror string `json:"mirror,omitempty"`
	Branch string `json:"branch,omitempty"`
	// Blueprint and GoldenVars are a machine's golden phase, and RunAs
	// the user it runs as, empty for root.
	Blueprint  string          `json:"blueprint,omitempty"`
	GoldenVars json.RawMessage `json:"goldenVars,omitempty"`
	RunAs      string          `json:"runAs,omitempty"`
}

func inputHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// goldenName is the name of the golden of a kind and input hash.
func goldenName(prefix, hash string) string { return prefix + "-" + hash[:16] }

func goldenBuildKey(project, name string) store.Key {
	return store.Key{Kind: goldenBuildKind, Project: project, Name: name}
}

// ensureGolden returns the golden of the name in project, making it with
// spec and build if it is not there: one per target and input hash,
// shared by everything of the same inputs. One being deleted is not
// there yet: errGoldenGoing.
func (s *Server) ensureGolden(ctx context.Context, project, name string, spec *envv1alpha1.GoldenSpec, build buildSpec, labels map[string]string) (*envv1alpha1.Golden, error) {
	get := func() (*envv1alpha1.Golden, bool, error) {
		g, ok, err := getTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, resource.Key{Project: project, Name: name})
		if err == nil && ok && g.GetMetadata().GetDeleteTime() != nil {
			return nil, true, errGoldenGoing
		}
		return g, ok, err
	}
	g, ok, err := get()
	if err != nil || ok {
		return g, err
	}
	// Written first: the golden controller reads it once the golden is.
	if err := s.putRecord(ctx, goldenBuildKey(project, name), build); err != nil {
		return nil, err
	}
	res, err := s.backend(registry.Golden).Create(s.system(ctx), &envv1alpha1.Golden{
		Metadata: &metav1.ObjectMeta{
			Project: project, Name: name, Labels: labels,
			Finalizers: []string{GoldenArtifactFinalizer},
		},
		Spec: spec,
	})
	if connect.CodeOf(err) == connect.CodeAlreadyExists {
		g, _, err = get()
		return g, err
	}
	if err != nil {
		return nil, err
	}
	return res.Object.(*envv1alpha1.Golden), nil
}

// liveGolden is the golden of the name, if it exists and is not being
// deleted.
func (s *Server) liveGolden(ctx context.Context, project, name string) (*envv1alpha1.Golden, bool, error) {
	g, ok, err := getTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, resource.Key{Project: project, Name: name})
	if err != nil || !ok || g.GetMetadata().GetDeleteTime() != nil {
		return nil, false, err
	}
	return g, true, nil
}

// goldenUsed records that an environment was made from the golden now,
// which pruning reads.
func (s *Server) goldenUsed(ctx context.Context, project, name string) error {
	for range maxRetries {
		g, ok, err := getTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, resource.Key{Project: project, Name: name})
		if err != nil || !ok {
			return err
		}
		st := proto.Clone(g.GetStatus()).(*envv1alpha1.GoldenStatus)
		st.LastUseTime = timestamppb.New(s.clock.Now())
		err = s.updateStatus(ctx, registry.Golden, g, st)
		if connect.CodeOf(err) != connect.CodeAborted {
			return err
		}
	}
	return errors.New("too many writers of a golden's status")
}

func goldenPhase(g *envv1alpha1.Golden) envv1alpha1.GoldenPhase { return g.GetStatus().GetPhase() }

// setGolden writes a golden's phase, and its Ready condition saying why.
func (s *Server) setGolden(ctx context.Context, g *envv1alpha1.Golden, phase envv1alpha1.GoldenPhase, reason, msg string, change func(*envv1alpha1.GoldenStatus)) error {
	st := proto.Clone(g.GetStatus()).(*envv1alpha1.GoldenStatus)
	if st == nil {
		st = &envv1alpha1.GoldenStatus{}
	}
	st.ObservedGeneration, st.Phase = g.GetMetadata().GetGeneration(), phase
	if change != nil {
		change(st)
	}
	// The failure is counted before it is written, so whatever reads the
	// golden failed finds when it is built again.
	if phase == envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED && goldenPhase(g) != envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED {
		b, err := s.goldenFailed(ctx, g, msg)
		if err != nil {
			return err
		}
		msg = b.message()
	}
	ready := &metav1.Condition{Type: GoldenReady, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg, ObservedGeneration: st.ObservedGeneration}
	if phase == envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY {
		ready.Status = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	}
	st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), ready, s.clock.Now())
	if err := s.updateStatus(ctx, registry.Golden, g, st); err != nil {
		return err
	}
	if phase == envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY {
		return s.deleteRecord(ctx, goldenBackoffKey(g))
	}
	return nil
}

func goldenBackoffKey(g *envv1alpha1.Golden) store.Key {
	return store.Key{Kind: goldenBackoffKind, Project: g.GetMetadata().GetProject(), Name: g.GetSpec().GetInputHash()}
}

// goldenFailed counts a failure of the golden's inputs, once per golden,
// and says when a golden of them is built again.
func (s *Server) goldenFailed(ctx context.Context, g *envv1alpha1.Golden, reason string) (goldenBackoff, error) {
	var b goldenBackoff
	if _, err := s.getRecord(ctx, goldenBackoffKey(g), &b); err != nil {
		return b, err
	}
	if b.Golden == g.GetMetadata().GetUid() {
		return b, nil
	}
	b.Golden = g.GetMetadata().GetUid()
	wait := GoldenRetryFirst
	for range b.Failures {
		if wait *= 2; wait >= GoldenRetryMax {
			wait = GoldenRetryMax
			break
		}
	}
	b.Failures++
	b.FailedAt = s.clock.Now()
	b.RetryAt, b.Reason = b.FailedAt.Add(wait), reason
	s.log.Warn("a golden failed", "project", g.GetMetadata().GetProject(), "name", g.GetMetadata().GetName(),
		"failures", b.Failures, "retry_at", b.RetryAt, "reason", reason)
	return b, s.putRecord(ctx, goldenBackoffKey(g), b)
}

// goldenRetryAt is when a failed golden is built again, zero if tower
// knows of no failure of its inputs.
func (s *Server) goldenRetryAt(ctx context.Context, g *envv1alpha1.Golden) (time.Time, error) {
	var b goldenBackoff
	if _, err := s.getRecord(ctx, goldenBackoffKey(g), &b); err != nil {
		return time.Time{}, err
	}
	return b.RetryAt, nil
}

// failedGoldenMessage is what waits on a failed golden say: why it
// failed, when, and when it is built again. A golden whose failure tower
// no longer counts (retried, or never recorded) says what its condition
// says.
func (s *Server) failedGoldenMessage(ctx context.Context, g *envv1alpha1.Golden) (string, error) {
	var b goldenBackoff
	ok, err := s.getRecord(ctx, goldenBackoffKey(g), &b)
	if err != nil {
		return "", err
	}
	if ok && !b.RetryAt.IsZero() {
		return b.message(), nil
	}
	return conditions.Find(g.GetStatus().GetConditions(), GoldenReady).GetMessage(), nil
}

// retryGolden deletes a failed golden something still asks for once its
// backoff has passed, so whatever asks makes it again, and reports
// whether it was asked for.
func (s *Server) retryGolden(ctx context.Context, g *envv1alpha1.Golden) (bool, error) {
	meta := g.GetMetadata()
	_, warm := meta.GetLabels()[warmLabel]
	asked := warm && meta.GetAnnotations()[warmDroppedAnnotation] == ""
	if !asked {
		var err error
		if asked, err = s.goldenReferenced(ctx, g); err != nil || !asked {
			return false, err
		}
	}
	// One made on a golden (a warm checkout on its image) waits while that
	// golden is there and not built: made again now, it would fail on it
	// again (convergeGolden). It queues this one once built or gone
	// (queueGoldensOn).
	if parent := g.GetSpec().GetParent(); parent != "" {
		pg, ok, err := s.liveGolden(ctx, meta.GetProject(), parent)
		if err != nil || (ok && goldenPhase(pg) != envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY) {
			return true, err
		}
	}
	// A failure is recorded before it is written (setGolden), so one with
	// no record is built again at once.
	at, err := s.goldenRetryAt(ctx, g)
	if err != nil {
		return true, err
	}
	if s.clock.Now().Before(at) {
		return true, nil
	}
	rv := meta.GetResourceVersion()
	_, err = s.backend(registry.Golden).Delete(s.system(ctx), resource.Key{Project: meta.GetProject(), Name: meta.GetName()}, &rv)
	if err == nil {
		s.log.Info("deleting a failed golden to build it again", "project", meta.GetProject(), "name", meta.GetName())
	}
	return true, err
}

// transient reports whether a provider's error passes by itself: the target
// did not answer, or there was no provider for it yet.
func transient(err error) bool {
	return errors.Is(err, provider.ErrUnavailable) || errors.Is(err, errNoProvider)
}

func (s *Server) reconcileGolden(ctx context.Context, key controller.Key) error {
	g, ok, err := getTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, resource.Key{Project: key.Project, Name: key.Name})
	if err != nil {
		return err
	}
	if !ok || goldenPhase(g) == envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY {
		if err := s.queueGoldensOn(ctx, key); err != nil || !ok {
			return err
		}
	}
	job := "golden/" + key.String()
	if g.GetMetadata().GetDeleteTime() != nil {
		return s.deleteGoldenArtifact(ctx, key, g, job)
	}
	switch goldenPhase(g) {
	case envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED:
		if err := s.deleteBuilder(ctx, g); err != nil {
			return err
		}
		if asked, err := s.retryGolden(ctx, g); err != nil || asked {
			return err
		}
		return s.pruneGolden(ctx, g)
	case envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY:
		return s.pruneGolden(ctx, g)
	case envv1alpha1.GoldenPhase_GOLDEN_PHASE_UNSPECIFIED:
		return s.setGolden(ctx, g, envv1alpha1.GoldenPhase_GOLDEN_PHASE_BUILDING, "Building", "", nil)
	}
	jerr, running := s.jobs.lastError(job)
	if running {
		return nil
	}
	if jerr != nil {
		// Said once: a transient failure is tried again when the key comes
		// back, as a target that did not answer may answer then.
		s.jobs.forget(job)
		if transient(jerr) {
			return fmt.Errorf("building golden %s: %w: %w", key, controller.ErrWaiting, jerr)
		}
		return s.setGolden(ctx, g, envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED, "BuildFailed", jerr.Error(), nil)
	}
	// No build starts on a target being deleted, whose teardown deletes
	// the golden; one running already may finish.
	if _, live, err := s.liveTarget(ctx, g.GetSpec().GetTarget()); err != nil || !live {
		return err
	}
	p, err := s.providerFor(g.GetSpec().GetTarget())
	if err != nil {
		return fmt.Errorf("golden %s: %w: %w", key, controller.ErrWaiting, err)
	}
	var build buildSpec
	if found, err := s.getRecord(ctx, goldenBuildKey(key.Project, key.Name), &build); err != nil {
		return err
	} else if !found {
		return s.setGolden(ctx, g, envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED, "BuildFailed", "what it is built from is not recorded", nil)
	}
	switch in := g.GetSpec().GetInputs().(type) {
	case *envv1alpha1.GoldenSpec_BoxImage:
		s.startJob(g.GetSpec().GetTarget(), job, s.goldenCtrl, key, func(ctx context.Context) error {
			if cur, ok, err := getTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, resource.Key{Project: key.Project, Name: key.Name}); err != nil || !ok || goldenPhase(cur) != envv1alpha1.GoldenPhase_GOLDEN_PHASE_BUILDING {
				return err
			}
			return s.buildImage(ctx, p, g, build)
		})
		return nil
	case *envv1alpha1.GoldenSpec_BoxWarm:
		return s.convergeGolden(ctx, p, g, job, build, in.BoxWarm)
	case *envv1alpha1.GoldenSpec_Machine:
		return s.convergeGolden(ctx, p, g, job, build, nil)
	}
	return s.setGolden(ctx, g, envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED, "BuildFailed", "the golden names no inputs", nil)
}

// buildImage builds a box image through the provider, from the base
// image the golden was made for: one whose tag moved since is built as the
// golden's inputs say.
func (s *Server) buildImage(ctx context.Context, p provider.Provider, g *envv1alpha1.Golden, build buildSpec) error {
	if build.Image == nil {
		return fmt.Errorf("the image's request is not recorded: %w", provider.ErrFailed)
	}
	ref, err := s.resolveAndBuild(ctx, p, g, *build.Image)
	if errors.Is(err, devcontainer.ErrUnresolvable) {
		return fmt.Errorf("%s: %w", unresolvableMessage(err), provider.ErrFailed)
	}
	if err != nil {
		return err
	}
	return s.retryStatus(ctx, g, func(g *envv1alpha1.Golden) error {
		return s.setGolden(ctx, g, envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY, "Built", "", func(st *envv1alpha1.GoldenStatus) {
			st.Artifact = ref
		})
	})
}

func (s *Server) resolveAndBuild(ctx context.Context, p provider.Provider, g *envv1alpha1.Golden, req devcontainer.Request) (string, error) {
	img, err := s.cfg.Resolver.Image(ctx, p, req)
	if err != nil {
		return "", err
	}
	if d := g.GetSpec().GetBoxImage().GetBaseImageDigest(); d != "" {
		img.BaseImageDigest = d
	}
	return s.cfg.Resolver.Build(ctx, p, req, img, g.GetMetadata().GetProject()+"/box")
}

// retryStatus runs write on the golden as stored until it is not refused
// for having moved on.
func (s *Server) retryStatus(ctx context.Context, g *envv1alpha1.Golden, write func(*envv1alpha1.Golden) error) error {
	key := resource.Key{Project: g.GetMetadata().GetProject(), Name: g.GetMetadata().GetName()}
	for range maxRetries {
		cur, ok, err := getTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, key)
		if err != nil || !ok || cur.GetMetadata().GetUid() != g.GetMetadata().GetUid() {
			return err
		}
		if err := write(cur); connect.CodeOf(err) != connect.CodeAborted {
			return err
		}
	}
	return errors.New("too many writers of a golden's status")
}

// convergeGolden makes a warm or a machine golden in a builder instance:
// it is made, its golden phase run in it, it is stopped and captured, and
// it goes. Each step waits for the instance's provider, which queues the
// golden again by its owner reference.
func (s *Server) convergeGolden(ctx context.Context, p provider.Provider, g *envv1alpha1.Golden, job string, build buildSpec, warm *envv1alpha1.BoxWarmInputs) error {
	project := g.GetMetadata().GetProject()
	key := controller.Key{Project: project, Name: g.GetMetadata().GetName()}
	name := childName("build", g.GetMetadata().GetName())
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: project, Name: name})
	if err != nil {
		return err
	}
	if !ok || !owns(registry.Golden, g, in) {
		spec := &envv1alpha1.InstanceSpec{Target: g.GetSpec().GetTarget(), Network: name, Alias: "builder"}
		if warm != nil {
			parent, ok, err := getTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, resource.Key{Project: project, Name: g.GetSpec().GetParent()})
			if err != nil {
				return err
			}
			if !ok || goldenPhase(parent) == envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED {
				return s.setGolden(ctx, g, envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED, "BuildFailed", "its image did not build", nil)
			}
			if goldenPhase(parent) != envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY {
				return nil // the parent's change queues it
			}
			spec.Kind, spec.Image = envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, parent.GetStatus().GetArtifact()
		} else {
			spec.Kind, spec.Image = envv1alpha1.InstanceKind_INSTANCE_KIND_VM, g.GetSpec().GetMachine().GetBaseImage()
		}
		spec.Resources = withDefaults(spec.Kind, nil)
		_, err := s.backend(registry.Instance).Create(s.system(ctx), &envv1alpha1.Instance{
			Metadata: &metav1.ObjectMeta{Project: project, Name: name, OwnerReferences: []*metav1.OwnerReference{ownedBy(registry.Golden, g)}},
			Spec:     spec,
		})
		if connect.CodeOf(err) == connect.CodeAlreadyExists {
			return nil
		}
		return err
	}
	// A job running, or its failure, is reconcileGolden's to see to.
	if _, running := s.jobs.lastError(job); running {
		return nil
	}
	ref := provider.Instance{Project: project, Name: name}
	converged := in.GetMetadata().GetAnnotations()[convergedAnnotation] == "true"
	switch phase := in.GetStatus().GetPhase(); {
	case phase == envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED || phase == envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE:
		msg := "its builder instance failed"
		if c := conditions.Find(in.GetStatus().GetConditions(), "Ready"); c != nil {
			msg += ": " + c.GetMessage()
		}
		if _, err := s.backend(registry.Instance).Delete(s.system(ctx), resource.Key{Project: project, Name: name}, nil); err != nil && !isNotFound(err) {
			return err
		}
		return s.setGolden(ctx, g, envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED, "BuildFailed", msg, nil)
	case phase == envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING && !converged:
		s.startJob(g.GetSpec().GetTarget(), job, s.goldenCtrl, key, func(ctx context.Context) error {
			if done, err := s.instanceMarked(ctx, project, name, convergedAnnotation); err != nil || done {
				return err
			}
			var err error
			if warm != nil {
				err = s.convergeWarm(ctx, p, ref, build, warm)
			} else {
				err = s.convergeMachine(ctx, p, ref, build)
			}
			if err != nil {
				return err
			}
			return s.markConverged(ctx, project, name)
		})
	case phase == envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED && converged:
		s.startJob(g.GetSpec().GetTarget(), job, s.goldenCtrl, key, func(ctx context.Context) error {
			if cur, ok, err := getTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, resource.Key{Project: project, Name: g.GetMetadata().GetName()}); err != nil || !ok || goldenPhase(cur) != envv1alpha1.GoldenPhase_GOLDEN_PHASE_BUILDING {
				return err
			}
			art, err := p.Capture(ctx, ref)
			if err != nil {
				return err
			}
			if err := s.retryStatus(ctx, g, func(g *envv1alpha1.Golden) error {
				return s.setGolden(ctx, g, envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY, "Captured", "", func(st *envv1alpha1.GoldenStatus) {
					st.Artifact, st.Size = art.Name, quantity.FormatDisk(art.Size)
				})
			}); err != nil {
				return err
			}
			if _, err := s.backend(registry.Instance).Delete(s.system(ctx), resource.Key{Project: project, Name: name}, nil); err != nil && !isNotFound(err) {
				return err
			}
			return nil
		})
	}
	return nil
}

// deleteBuilder deletes a failed golden's builder instance, which would
// otherwise hold its target's resources for as long as the golden is
// kept, through its backoff; its failure already quotes what went wrong.
// A golden built again is a new one, with a builder of its own.
func (s *Server) deleteBuilder(ctx context.Context, g *envv1alpha1.Golden) error {
	key := resource.Key{Project: g.GetMetadata().GetProject(), Name: childName("build", g.GetMetadata().GetName())}
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, key)
	if err != nil || !ok || !owns(registry.Golden, g, in) || in.GetMetadata().GetDeleteTime() != nil {
		return err
	}
	if _, err := s.backend(registry.Instance).Delete(s.system(ctx), key, nil); err != nil && !isNotFound(err) {
		return err
	}
	s.log.Info("deleted a failed golden's builder", "project", key.Project, "golden", g.GetMetadata().GetName(), "builder", key.Name)
	return nil
}

// convergeWarm checks the repository out in the builder and installs its
// toolchain and dependencies.
func (s *Server) convergeWarm(ctx context.Context, p provider.Provider, ref provider.Instance, build buildSpec, warm *envv1alpha1.BoxWarmInputs) error {
	if _, err := p.Checkout(ctx, ref, build.Mirror, warm.GetCommit(), "/workspace"); err != nil {
		return err
	}
	w, err := s.cfg.Resolver.Warm(ctx, p, build.Mirror, warm.GetCommit())
	if err != nil || len(w.Install) == 0 {
		return err
	}
	return s.mustRun(ctx, p, ref, nil, "/workspace", w.Install...)
}

// convergeMachine runs a blueprint's golden phase in the builder, with its
// golden vars, which are part of what the golden is: every task of the
// playbook but those tagged configure, which run in each machine
// (configureMachine). This is how workstream's blueprints are written:
// their tasks are untagged, and a task that needs a machine's own vars or
// secrets is tagged configure (workstream: src/machine/drivers/
// ansible-qemu.ts). A blueprint with a run_as user runs as it, made here
// first (prepareUser), so every clone of the golden has it. A base image
// without ansible is given it first (installAnsible).
func (s *Server) convergeMachine(ctx context.Context, p provider.Provider, ref provider.Instance, build buildSpec) error {
	if err := s.mustRun(ctx, p, ref, nil, "/", "sh", "-c", installAnsible); err != nil {
		return fmt.Errorf("installing ansible-core: %w", err)
	}
	if err := p.WriteFile(ctx, ref, "/opt/tower/blueprint.yml", 0o600, 0, strings.NewReader(build.Blueprint)); err != nil {
		return err
	}
	vars := build.GoldenVars
	if len(vars) == 0 {
		vars = json.RawMessage("{}")
	}
	if err := p.WriteFile(ctx, ref, "/opt/tower/golden-vars.json", 0o600, 0, bytes.NewReader(vars)); err != nil {
		return err
	}
	if err := s.prepareUser(ctx, p, ref, build.RunAs, "/opt/tower/blueprint.yml", "/opt/tower/golden-vars.json"); err != nil {
		return err
	}
	return s.mustRun(ctx, p, ref, nil, "/opt/tower", asUser(build.RunAs,
		"ansible-playbook", "--connection=local", "--inventory=localhost,", "--skip-tags=configure", "--extra-vars=@golden-vars.json", "blueprint.yml")...)
}

// installAnsible installs ansible-core in a golden's builder where
// ansible-playbook is not there: hostd's base images carry it
// (deploy/hostd/base-image.sh), a hangar target's catalogue images do not
// (docs/runbook/deploy.md). It waits for cloud-init's first boot to end,
// whose package runs hold the package manager's lock, and for that lock.
const installAnsible = `command -v ansible-playbook >/dev/null 2>&1 && exit 0
(cloud-init status --wait >/dev/null 2>&1 || true)
if command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get -o DPkg::Lock::Timeout=600 update -q && apt-get -o DPkg::Lock::Timeout=600 install -qy --no-install-recommends ansible-core || exit 1
elif command -v dnf >/dev/null 2>&1; then
  dnf install -y ansible-core || exit 1
else
  echo "no apt-get or dnf to install ansible-core with" >&2; exit 1
fi
command -v ansible-playbook >/dev/null`

// mustRun runs a command, a non-zero exit being a failure of the work,
// which quotes the end of its output; but not of a command given an
// environment, which may hold secret values the command printed.
func (s *Server) mustRun(ctx context.Context, p provider.Provider, ref provider.Instance, env map[string]string, dir string, command ...string) error {
	var out bytes.Buffer
	code, err := p.Exec(ctx, provider.ExecRequest{Instance: ref, Command: command, Env: env, Dir: dir, Stdout: &out, Stderr: &out})
	if err != nil {
		return err
	}
	if code != 0 && len(env) > 0 {
		return fmt.Errorf("%s exited %d: %w", command[0], code, provider.ErrFailed)
	}
	if code != 0 {
		tail := out.String()
		if len(tail) > 512 {
			tail = tail[len(tail)-512:]
		}
		return fmt.Errorf("%s exited %d: %s: %w", command[0], code, strings.TrimSpace(tail), provider.ErrFailed)
	}
	return nil
}

// markConverged annotates a builder instance as converged and stops it,
// for its capture.
func (s *Server) markConverged(ctx context.Context, project, name string) error {
	for range maxRetries {
		in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: project, Name: name})
		if err != nil || !ok {
			return err
		}
		in = proto.Clone(in).(*envv1alpha1.Instance)
		if in.Metadata.Annotations == nil {
			in.Metadata.Annotations = map[string]string{}
		}
		in.Metadata.Annotations[convergedAnnotation] = "true"
		in.Spec.RunState = envv1alpha1.RunState_RUN_STATE_STOPPED
		_, err = s.backend(registry.Instance).Apply(s.system(ctx), in)
		if connect.CodeOf(err) != connect.CodeAborted {
			return err
		}
	}
	return errors.New("too many writers of a builder instance")
}

// deleteGoldenArtifact deletes a deleted golden's artifact on its target,
// then lets the golden go.
func (s *Server) deleteGoldenArtifact(ctx context.Context, key controller.Key, g *envv1alpha1.Golden, job string) error {
	if !slices.Contains(g.GetMetadata().GetFinalizers(), GoldenArtifactFinalizer) {
		return nil
	}
	if err := s.deleteRecord(ctx, goldenBuildKey(key.Project, key.Name)); err != nil {
		return err
	}
	jerr, running := s.jobs.lastError(job)
	if running {
		return nil
	}
	if jerr != nil {
		s.jobs.forget(job)
		return fmt.Errorf("deleting the artifact of golden %s: %w: %w", key, controller.ErrWaiting, jerr)
	}
	art := g.GetStatus().GetArtifact()
	if art == "" || goldenPhase(g) != envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY {
		return s.RemoveFinalizer(ctx, registry.Golden, resource.Key{Project: key.Project, Name: key.Name}, GoldenArtifactFinalizer)
	}
	// A target's teardown deletes its Goldens before it lets the Target go;
	// one whose Target is gone all the same, abandoned, has no one to ask.
	if _, ok, err := getTyped[*targetsv1alpha1.Target](ctx, s, registry.Target, resource.Key{Name: g.GetSpec().GetTarget()}); err != nil {
		return err
	} else if !ok {
		s.log.Warn("letting a golden go with its artifact unconfirmed deleted: its target is gone",
			"project", key.Project, "golden", key.Name, "target", g.GetSpec().GetTarget(), "artifact", art)
		return s.RemoveFinalizer(ctx, registry.Golden, resource.Key{Project: key.Project, Name: key.Name}, GoldenArtifactFinalizer)
	}
	p, err := s.providerFor(g.GetSpec().GetTarget())
	if err != nil {
		return fmt.Errorf("golden %s: %w: %w", key, controller.ErrWaiting, err)
	}
	s.startJob(g.GetSpec().GetTarget(), job, s.goldenCtrl, key, func(ctx context.Context) error {
		// An image lives in the registry, which nothing prunes yet: tower
		// does not delete images.
		if _, isImage := g.GetSpec().GetInputs().(*envv1alpha1.GoldenSpec_BoxImage); !isImage {
			if err := p.DeleteArtifact(ctx, art); err != nil {
				return err
			}
		}
		return s.RemoveFinalizer(ctx, registry.Golden, resource.Key{Project: key.Project, Name: key.Name}, GoldenArtifactFinalizer)
	})
	return nil
}

// pruneGolden deletes a golden nothing was made from for GoldenPruneAfter
// and nothing references: no live box or machine made of it, no instance
// cloned from it, no golden made on it, and no warm golden the warm
// controller holds.
func (s *Server) pruneGolden(ctx context.Context, g *envv1alpha1.Golden) error {
	if _, held := g.GetMetadata().GetLabels()[warmLabel]; held {
		return nil
	}
	since := g.GetMetadata().GetCreateTime().AsTime()
	if t := g.GetStatus().GetLastUseTime(); t != nil && t.AsTime().After(since) {
		since = t.AsTime()
	}
	if s.clock.Since(since) < s.cfg.GoldenPruneAfter {
		return nil
	}
	used, err := s.goldenReferenced(ctx, g)
	if err != nil || used {
		return err
	}
	meta := g.GetMetadata()
	rv := meta.GetResourceVersion()
	_, err = s.backend(registry.Golden).Delete(s.system(ctx), resource.Key{Project: meta.GetProject(), Name: meta.GetName()}, &rv)
	if err == nil {
		s.log.Info("pruned a golden", "project", meta.GetProject(), "name", meta.GetName())
	}
	return err
}

// queueGoldensOn queues the FAILED goldens made on the golden key, which
// wait for it while it is there and not built (retryGolden).
func (s *Server) queueGoldensOn(ctx context.Context, key controller.Key) error {
	goldens, err := listTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, key.Project)
	if err != nil {
		return err
	}
	for _, g := range goldens {
		if g.GetSpec().GetParent() == key.Name && goldenPhase(g) == envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED {
			enqueue(s.goldenCtrl, controller.Key{Project: key.Project, Name: g.GetMetadata().GetName()})
		}
	}
	return nil
}

func (s *Server) goldenReferenced(ctx context.Context, g *envv1alpha1.Golden) (bool, error) {
	project, name := g.GetMetadata().GetProject(), g.GetMetadata().GetName()
	boxes, err := listTyped[*envv1alpha1.Box](ctx, s, registry.Box, project)
	if err != nil {
		return false, err
	}
	for _, b := range boxes {
		// A box that fell back from its devcontainer's image asks for it
		// until it is built (followDevcontainerGolden).
		if b.GetStatus().GetImageGolden() == name || b.GetStatus().GetWarmGolden() == name || b.GetStatus().GetDevcontainerGolden() == name {
			return true, nil
		}
	}
	machines, err := listTyped[*envv1alpha1.Machine](ctx, s, registry.Machine, project)
	if err != nil {
		return false, err
	}
	for _, m := range machines {
		if m.GetStatus().GetGolden() == name {
			return true, nil
		}
	}
	instances, err := listTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, project)
	if err != nil {
		return false, err
	}
	for _, in := range instances {
		if in.GetSpec().GetDiskGolden() == name || (g.GetStatus().GetArtifact() != "" && in.GetSpec().GetImage() == g.GetStatus().GetArtifact()) {
			return true, nil
		}
	}
	goldens, err := listTyped[*envv1alpha1.Golden](ctx, s, registry.Golden, project)
	if err != nil {
		return false, err
	}
	for _, other := range goldens {
		if other.GetSpec().GetParent() == name && other.GetMetadata().GetDeleteTime() == nil {
			return true, nil
		}
	}
	return false, nil
}

// structJSON is a Struct as JSON, which hashes the same for the same
// values.
func structJSON(st *structpb.Struct) (json.RawMessage, error) {
	if st == nil {
		return nil, nil
	}
	return json.Marshal(st.AsMap())
}
