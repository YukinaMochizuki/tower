package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// A machine's conditions, all the machine controller's.
const (
	MachineGoldenBuilt = "GoldenBuilt"
	MachineConfigured  = "Configured"
	MachineReady       = "Ready"
	// configuredAnnotation marks an instance whose configure phase has run.
	configuredAnnotation = registry.Domain + "/configured"
	// readyAnnotation is the run of an instance (runOf) its blueprint's
	// ready check passed in.
	readyAnnotation = registry.Domain + "/ready"

	// readyCheckEvery is how long a ready check waits between tries, and
	// defaultReadyTimeout how long it tries for unless its blueprint says:
	// workstream's, which tries 40 times, 5 seconds apart
	// (workstream: src/machine/drivers/ansible-qemu.ts).
	readyCheckEvery     = 5 * time.Second
	defaultReadyTimeout = 40 * readyCheckEvery
)

// machineInputs are the inputs of a machine's golden, and its hash.
func machineInputs(spec *envv1alpha1.MachineSpec) (*envv1alpha1.MachineInputs, json.RawMessage, error) {
	bp := spec.GetBlueprint()
	vars, err := structJSON(bp.GetGoldenVars())
	if err != nil {
		return nil, nil, err
	}
	return &envv1alpha1.MachineInputs{BlueprintHash: inputHash(bp.GetContent(), string(vars)), BaseImage: bp.GetBaseImage(), RunAs: bp.GetRunAs().GetUser()}, vars, nil
}

// machineGoldenHash is the input hash of a machine's golden on target. A
// blueprint without run_as hashes as it did before run_as was, so its
// golden stays the one it had.
func machineGoldenHash(target string, in *envv1alpha1.MachineInputs) string {
	parts := []string{"machine", target, in.GetBlueprintHash(), in.GetBaseImage()}
	if in.GetRunAs() != "" {
		parts = append(parts, "run-as", in.GetRunAs())
	}
	return inputHash(parts...)
}

func (s *Server) reconcileMachine(ctx context.Context, key controller.Key) error {
	m, ok, err := getTyped[*envv1alpha1.Machine](ctx, s, registry.Machine, resource.Key{Project: key.Project, Name: key.Name})
	if err != nil || !ok || m.GetMetadata().GetDeleteTime() != nil {
		return err
	}
	st := proto.Clone(m.GetStatus()).(*envv1alpha1.MachineStatus)
	if st == nil {
		st = &envv1alpha1.MachineStatus{}
	}
	st.ObservedGeneration = m.GetMetadata().GetGeneration()
	waitErr := s.machineSteps(ctx, key, m, st)
	if err := s.updateStatus(ctx, registry.Machine, m, st); err != nil {
		return err
	}
	return waitErr
}

// machineCond sets one of a machine's conditions in st.
func (s *Server) machineCond(st *envv1alpha1.MachineStatus, typ string, ok bool, reason, msg string) {
	c := &metav1.Condition{Type: typ, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE, Reason: reason, Message: msg, ObservedGeneration: st.GetObservedGeneration()}
	if ok {
		c.Status = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	}
	st.Conditions = conditions.Set(slices.Clone(st.GetConditions()), c, s.clock.Now())
}

func (s *Server) machineSteps(ctx context.Context, key controller.Key, m *envv1alpha1.Machine, st *envv1alpha1.MachineStatus) error {
	not := func(phase envv1alpha1.MachinePhase, reason, msg string) {
		st.Phase = phase
		s.machineCond(st, MachineReady, false, reason, msg)
	}
	spec := m.GetSpec()
	if st.Phase == envv1alpha1.MachinePhase_MACHINE_PHASE_UNSPECIFIED {
		st.Phase = envv1alpha1.MachinePhase_MACHINE_PHASE_PENDING
	}
	if st.Target == "" {
		t, reason, err := s.placedTarget(ctx, key.Project)
		if err != nil {
			return err
		}
		if t == nil {
			not(envv1alpha1.MachinePhase_MACHINE_PHASE_UNPLACED, "Unplaced", reason)
			return nil
		}
		st.Target = t.GetMetadata().GetName()
	} else if _, live, err := s.liveTarget(ctx, st.Target); err != nil {
		return err
	} else if !live {
		not(envv1alpha1.MachinePhase_MACHINE_PHASE_FAILED, "TargetGone", fmt.Sprintf("its target %q is gone", st.Target))
		return nil
	}
	p, err := s.providerFor(st.Target)
	if err != nil {
		s.machineCond(st, MachineReady, false, "TargetUnavailable", err.Error())
		return fmt.Errorf("machine %s: %w: %w", key, controller.ErrWaiting, err)
	}

	// Its golden: one per blueprint, golden vars, base image and run_as
	// user.
	inputs, vars, err := machineInputs(spec)
	if err != nil {
		return err
	}
	hash := machineGoldenHash(st.Target, inputs)
	st.Golden = goldenName("mc", hash)
	g, err := s.ensureGolden(ctx, key.Project, st.Golden, &envv1alpha1.GoldenSpec{
		Target: st.Target, InputHash: hash, Inputs: &envv1alpha1.GoldenSpec_Machine{Machine: inputs},
	}, buildSpec{Blueprint: spec.GetBlueprint().GetContent(), GoldenVars: vars, RunAs: inputs.GetRunAs()}, nil)
	if err != nil {
		return err
	}
	switch goldenPhase(g) {
	case envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED:
		msg, err := s.failedGoldenMessage(ctx, g)
		if err != nil {
			return err
		}
		s.machineCond(st, MachineGoldenBuilt, false, "BuildFailed", msg)
		not(envv1alpha1.MachinePhase_MACHINE_PHASE_FAILED, "GoldenBuildFailed", msg)
		return nil
	case envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY:
		s.machineCond(st, MachineGoldenBuilt, true, "Built", "")
	default:
		s.machineCond(st, MachineGoldenBuilt, false, "Building", "")
		not(envv1alpha1.MachinePhase_MACHINE_PHASE_BUILDING, "Building", "its golden is being converged")
		return nil
	}

	// Its instance, cloned from the golden.
	name := childName("mc", key.Name)
	alias := spec.GetBlueprint().GetNetAlias()
	if alias == "" {
		alias = spec.GetBlueprint().GetName()
	}
	in, ok, err := getTyped[*envv1alpha1.Instance](ctx, s, registry.Instance, resource.Key{Project: key.Project, Name: name})
	if err != nil {
		return err
	}
	if ok && !owns(registry.Machine, m, in) {
		not(envv1alpha1.MachinePhase_MACHINE_PHASE_PENDING, "Pending", "an earlier machine's instance is still going")
		return nil
	}
	if ok && in.GetStatus().GetPhase() == envv1alpha1.InstancePhase_INSTANCE_PHASE_GONE {
		if _, err := s.backend(registry.Instance).Delete(s.system(ctx), resource.Key{Project: key.Project, Name: name}, nil); err != nil && !isNotFound(err) {
			return err
		}
		s.log.Warn("a machine's instance vanished from its target; making it again", "project", key.Project, "machine", key.Name)
		not(envv1alpha1.MachinePhase_MACHINE_PHASE_CONFIGURING, "InstanceGone", "its instance vanished from the target; it is made again")
		return nil
	}
	desired := envv1alpha1.RunState_RUN_STATE_RUNNING
	if spec.GetRunState() == envv1alpha1.RunState_RUN_STATE_STOPPED {
		desired = envv1alpha1.RunState_RUN_STATE_STOPPED
	}
	if !ok {
		// Recorded before the instance names it: see makeBoxInstance.
		if err := s.goldenUsed(ctx, key.Project, st.Golden); err != nil {
			return err
		}
		_, err := s.backend(registry.Instance).Create(s.system(ctx), &envv1alpha1.Instance{
			Metadata: &metav1.ObjectMeta{Project: key.Project, Name: name, OwnerReferences: []*metav1.OwnerReference{ownedBy(registry.Machine, m)}},
			Spec: &envv1alpha1.InstanceSpec{
				Target: st.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_VM, Image: inputs.GetBaseImage(), DiskGolden: st.Golden,
				Network: spec.GetWorkstream(), Alias: alias, ExposePorts: spec.GetBlueprint().GetExposePorts(),
				Resources: withDefaults(envv1alpha1.InstanceKind_INSTANCE_KIND_VM, spec.GetResources()), RunState: desired,
			},
		})
		if err != nil && connect.CodeOf(err) != connect.CodeAlreadyExists {
			return err
		}
		st.Instance, st.Alias = name, alias
		not(envv1alpha1.MachinePhase_MACHINE_PHASE_CONFIGURING, "Starting", "its instance is being made")
		return nil
	}
	st.Instance, st.Alias, st.Address = name, alias, in.GetStatus().GetAddress()
	if in.GetSpec().GetRunState() != desired {
		if err := s.setRunState(ctx, in, desired); err != nil {
			return err
		}
	}
	phase := in.GetStatus().GetPhase()
	if desired == envv1alpha1.RunState_RUN_STATE_STOPPED {
		if phase == envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED {
			st.Phase = envv1alpha1.MachinePhase_MACHINE_PHASE_STOPPED
		}
		s.machineCond(st, MachineReady, false, "Stopped", "stopped: "+stoppedRequested)
		return nil
	}
	switch phase {
	case envv1alpha1.InstancePhase_INSTANCE_PHASE_FAILED:
		not(envv1alpha1.MachinePhase_MACHINE_PHASE_FAILED, "InstanceFailed", conditions.Find(in.GetStatus().GetConditions(), "Ready").GetMessage())
		return nil
	case envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING:
	default:
		not(envv1alpha1.MachinePhase_MACHINE_PHASE_CONFIGURING, "Starting", "its instance is starting")
		return nil
	}
	if in.GetMetadata().GetAnnotations()[configuredAnnotation] != "true" {
		return s.configureMachine(ctx, key, m, in, p, st, not)
	}
	s.machineCond(st, MachineConfigured, true, "Configured", "")
	if m.GetSpec().GetBlueprint().GetReadyCheck() != nil && in.GetMetadata().GetAnnotations()[readyAnnotation] != runOf(in) {
		return s.checkReady(ctx, key, m, in, p, st, not)
	}
	st.Phase = envv1alpha1.MachinePhase_MACHINE_PHASE_READY
	s.machineCond(st, MachineReady, true, "Ready", "")
	return nil
}

// machineStep is one of the machine's instance steps, in its instance's
// run.
func machineStep(key controller.Key, m *envv1alpha1.Machine, in *envv1alpha1.Instance, name string) stepRef {
	return newStepRef("machine", key.Project, key.Name, name, in.GetMetadata().GetUid()+"/"+runOf(in), m.GetMetadata().GetGeneration())
}

// checkReady runs the blueprint's ready check in the machine's instance,
// every readyCheckEvery until it passes or its timeout runs out, then marks
// the instance with the run it passed in: after the next start of the
// instance it is checked again. A check that never passes fails as any
// instance step does (steps.go), with the end of its last output, which
// can quote nothing secret: the check is run without the machine's
// secrets. It runs as the blueprint's run_as user, root without one.
func (s *Server) checkReady(ctx context.Context, key controller.Key, m *envv1alpha1.Machine, in *envv1alpha1.Instance, p provider.Provider, st *envv1alpha1.MachineStatus, not func(envv1alpha1.MachinePhase, string, string)) error {
	run := runOf(in)
	job := "ready/" + key.String()
	step := machineStep(key, m, in, "ready")
	g, err := s.stepGate(ctx, job, step)
	switch {
	case err != nil:
		return err
	case g.Waiting != nil:
		not(envv1alpha1.MachinePhase_MACHINE_PHASE_CONFIGURING, "WaitingForReady", g.Waiting.Error())
		return fmt.Errorf("machine %s: %w: %w", key, controller.ErrWaiting, g.Waiting)
	case g.Failed != "":
		not(envv1alpha1.MachinePhase_MACHINE_PHASE_FAILED, "ReadyCheckFailed", g.Failed)
		return nil
	}
	not(envv1alpha1.MachinePhase_MACHINE_PHASE_CONFIGURING, "WaitingForReady", "its ready check has not passed yet")
	if g.Running {
		return nil
	}
	check := m.GetSpec().GetBlueprint().GetReadyCheck()
	timeout := defaultReadyTimeout
	if check.GetTimeout() != nil {
		timeout = check.GetTimeout().AsDuration()
	}
	ref := provider.Instance{Project: key.Project, Name: in.GetMetadata().GetName()}
	user := m.GetSpec().GetBlueprint().GetRunAs().GetUser()
	s.startJob(st.Target, job, s.machineCtrl, key, func(ctx context.Context) error {
		deadline := s.clock.Now().Add(timeout)
		for {
			var out bytes.Buffer
			code, err := p.Exec(ctx, provider.ExecRequest{Instance: ref, Command: asUser(user, "sh", "-c", check.GetCommand()), Stdout: &out, Stderr: &out})
			if err != nil {
				return err
			}
			if code == 0 {
				if err := s.annotateInstance(ctx, key.Project, ref.Name, readyAnnotation, run); err != nil {
					return err
				}
				return s.stepPassed(ctx, step)
			}
			if !s.clock.Now().Before(deadline) {
				tail := strings.TrimSpace(out.String())
				if len(tail) > 512 {
					tail = tail[len(tail)-512:]
				}
				return fmt.Errorf("its ready check did not pass in %s; it last exited %d: %s: %w", timeout, code, tail, provider.ErrFailed)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-s.clock.After(readyCheckEvery):
			}
		}
	})
	return nil
}

// configureMachine runs the blueprint's configure phase in the machine's
// own instance, with its own vars and secrets: the playbook's tasks tagged
// configure, which its golden phase skipped (convergeMachine); a playbook
// with none runs nothing here. The secrets' values are
// opened for the one command and given as its environment alone: they
// enter no object, no event and no log. It runs as the blueprint's
// run_as user, which its golden has (convergeMachine), root without one.
func (s *Server) configureMachine(ctx context.Context, key controller.Key, m *envv1alpha1.Machine, in *envv1alpha1.Instance, p provider.Provider, st *envv1alpha1.MachineStatus, not func(envv1alpha1.MachinePhase, string, string)) error {
	job := "configure/" + key.String()
	step := machineStep(key, m, in, "configure")
	g, err := s.stepGate(ctx, job, step)
	switch {
	case err != nil:
		return err
	case g.Waiting != nil:
		not(envv1alpha1.MachinePhase_MACHINE_PHASE_CONFIGURING, "Configuring", g.Waiting.Error())
		return fmt.Errorf("machine %s: %w: %w", key, controller.ErrWaiting, g.Waiting)
	case g.Failed != "":
		s.machineCond(st, MachineConfigured, false, "ConfigureFailed", g.Failed)
		not(envv1alpha1.MachinePhase_MACHINE_PHASE_FAILED, "ConfigureFailed", g.Failed)
		return nil
	}
	s.machineCond(st, MachineConfigured, false, "Configuring", "")
	not(envv1alpha1.MachinePhase_MACHINE_PHASE_CONFIGURING, "Configuring", "its configure phase runs")
	if g.Running {
		return nil
	}
	spec := m.GetSpec()
	ref := provider.Instance{Project: key.Project, Name: in.GetMetadata().GetName()}
	s.startJob(st.Target, job, s.machineCtrl, key, func(ctx context.Context) error {
		// A reconcile that read the instance before an earlier job marked
		// it may start this one: the mark, read again, says it is done.
		if done, err := s.instanceMarked(ctx, key.Project, ref.Name, configuredAnnotation); err != nil || done {
			return err
		}
		env := map[string]string{}
		for _, sr := range spec.GetSecrets() {
			data, err := s.OpenSecret(s.system(ctx), resource.Key{Project: key.Project, Name: sr.GetSecret()})
			if err != nil {
				return fmt.Errorf("secret %q of the machine: %s: %w", sr.GetSecret(), messageOf(err), provider.ErrFailed)
			}
			v, ok := data[sr.GetKey()]
			if !ok {
				return fmt.Errorf("secret %q holds no key %q: %w", sr.GetSecret(), sr.GetKey(), provider.ErrFailed)
			}
			env[sr.GetAs()] = string(v)
		}
		vars, err := structJSON(spec.GetVars())
		if err != nil {
			return err
		}
		if len(vars) == 0 {
			vars = json.RawMessage("{}")
		}
		if err := p.WriteFile(ctx, ref, "/opt/tower/vars.json", 0o600, 0, bytes.NewReader(vars)); err != nil {
			return err
		}
		if err := p.WriteFile(ctx, ref, "/opt/tower/blueprint.yml", 0o600, 0, bytes.NewReader([]byte(spec.GetBlueprint().GetContent()))); err != nil {
			return err
		}
		user := spec.GetBlueprint().GetRunAs().GetUser()
		if err := s.prepareUser(ctx, p, ref, user, "/opt/tower/vars.json", "/opt/tower/blueprint.yml"); err != nil {
			return err
		}
		if err := s.mustRun(ctx, p, ref, env, "/opt/tower", asUser(user,
			"ansible-playbook", "--connection=local", "--inventory=localhost,", "--tags=configure", "--extra-vars=@vars.json", "blueprint.yml")...); err != nil {
			return err
		}
		if err := s.annotateInstance(ctx, key.Project, ref.Name, configuredAnnotation, "true"); err != nil {
			return err
		}
		return s.stepPassed(ctx, step)
	})
	return nil
}
