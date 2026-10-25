package apiserver_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// runsIn counts the commands named name run in the instance.
func (w *world) runsIn(instance, name string) int {
	n := 0
	for _, e := range w.fake.Execs() {
		if e.Instance.Name == instance && len(e.Command) > 0 && e.Command[0] == name {
			n++
		}
	}
	return n
}

// waitFailedSaying waits for the box to be FAILED with a Ready message
// holding each of want.
func (w *world) waitFailedSaying(name string, want ...string) *envv1alpha1.Box {
	w.t.Helper()
	var b *envv1alpha1.Box
	testutil.Eventually(w.t, fmt.Sprintf("box %s failed, saying %q", name, want), func() (bool, string) {
		b = w.box(name)
		msg := condition(b.GetStatus().GetConditions(), apiserver.BoxReady).GetMessage()
		for _, s := range want {
			if !strings.Contains(msg, s) {
				return false, b.GetStatus().String()
			}
		}
		return b.GetStatus().GetPhase() == envv1alpha1.BoxPhase_BOX_PHASE_FAILED, b.GetStatus().String()
	})
	return b
}

// TestAFailedCheckoutIsTriedAgainAfterABackoff: a box whose checkout step
// fails is FAILED, saying why and when it is tried again, and is not tried
// before: ten minutes, then twice as long after each failure in a row,
// which a restart keeps. A call into it meanwhile is refused at once with
// the reason; a success ends the backoff.
func TestAFailedCheckoutIsTriedAgainAfterABackoff(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.fake.FailCommands("install-dependencies")
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	at := func(d time.Duration) string { return w.clock.Now().Add(d).UTC().Format(time.RFC3339) }
	b := w.waitFailedSaying("dev", "install-dependencies exited 1", "tried again at "+at(apiserver.StepRetryFirst))
	if c := condition(b.GetStatus().GetConditions(), apiserver.BoxReady); c.GetReason() != "CheckoutFailed" {
		t.Errorf("Ready %v", c)
	}
	instance := b.GetStatus().GetInstance()

	// Used meanwhile: refused at once, with the reason.
	_, err := exec(t.Context(), w.as(workstream).envs, &envv1alpha1.ExecRequest{Environment: boxEnv("dev"), Command: argv("true")})
	wantCode(t, err, connect.CodeFailedPrecondition, "a command in a box whose checkout failed")
	if msg := messageOfErr(err); !strings.Contains(msg, "CheckoutFailed") || !strings.Contains(msg, "install-dependencies exited 1") {
		t.Errorf("the refusal says %q", msg)
	}

	w.advance(apiserver.StepRetryFirst - time.Minute)
	if n := w.runsIn(instance, "install-dependencies"); n != 1 {
		t.Fatalf("the step ran %d times before its backoff passed", n)
	}
	// The backoff outlives a restart.
	w.restart()
	w.advance(2 * time.Minute)
	w.waitFailedSaying("dev", "install-dependencies exited 1", "tried again at "+at(2*apiserver.StepRetryFirst))
	if n := w.runsIn(instance, "install-dependencies"); n != 2 {
		t.Fatalf("the step ran %d times once its backoff passed", n)
	}
	w.fake.FailCommands()
	w.advance(2*apiserver.StepRetryFirst - 2*time.Minute)
	if n := w.runsIn(instance, "install-dependencies"); n != 2 {
		t.Fatalf("the second backoff did not double: the step ran %d times", n)
	}
	w.advance(2 * time.Minute)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	w.waitSettled()
	if n := w.records("StepBackoff"); n != 0 {
		t.Errorf("%d step backoffs kept after the step passed", n)
	}
}

// TestAChangedSpecTriesAFailedStepAgain: a box's spec changed while one of
// its steps waits out a backoff runs the step again at once.
func TestAChangedSpecTriesAFailedStepAgain(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.fake.FailCommands("install-dependencies")
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	b := w.waitFailedSaying("dev", "install-dependencies exited 1")
	instance := b.GetStatus().GetInstance()
	w.fake.FailCommands()
	b = w.box("dev")
	b.Spec.Keep = true
	b.Status = nil
	w.mustApply(registry.Box, b)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	if n := w.runsIn(instance, "install-dependencies"); n != 2 {
		t.Errorf("the step ran %d times", n)
	}
}

// TestAStepsFailuresGoWithItsBox: the failures of a box's step are
// forgotten once the box is gone.
func TestAStepsFailuresGoWithItsBox(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.fake.FailCommands("install-dependencies")
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	w.waitFailedSaying("dev", "install-dependencies exited 1")
	if n := w.records("StepBackoff"); n != 1 {
		t.Fatalf("%d step backoffs kept for a failed step", n)
	}
	if _, err := w.asSystem().generic.Delete(t.Context(), registry.Box, client.Key{Project: "web", Name: "dev"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the box gone", func() (bool, string) {
		_, err := w.get(registry.Box, "web", "dev")
		return connect.CodeOf(err) == connect.CodeNotFound, fmt.Sprint(err)
	})
	// The periodic flush of uses prunes them, on a ticker of its own.
	testutil.Eventually(t, "the step's failures gone with their box", func() (bool, string) {
		if n := w.records("StepBackoff"); n != 0 {
			w.clock.Advance(apiserver.LastUseFlushEvery)
			return false, fmt.Sprintf("%d step backoffs kept", n)
		}
		return true, ""
	})
}

// TestAFailedConfigurePhaseIsTriedAgainAfterABackoff: a machine whose
// configure phase fails is FAILED, saying when it runs again, and runs
// again then.
func TestAFailedConfigurePhaseIsTriedAgainAfterABackoff(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	fail := true
	var configures int
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	w.fake.OnExec(func(req provider.ExecRequest) (int, bool) {
		if !slices.Contains(req.Command, "--tags=configure") {
			return 0, false
		}
		<-mu
		defer func() { mu <- struct{}{} }()
		configures++
		if fail {
			return 2, true
		}
		return 0, true
	})
	count := func() int { <-mu; defer func() { mu <- struct{}{} }(); return configures }
	w.mustApply(registry.Machine, machine("k3s"))
	at := func(d time.Duration) string { return w.clock.Now().Add(d).UTC().Format(time.RFC3339) }
	testutil.Eventually(t, "the machine failed, saying when it runs again", func() (bool, string) {
		st := w.machine("k3s").GetStatus()
		c := condition(st.GetConditions(), apiserver.MachineReady)
		return st.GetPhase() == envv1alpha1.MachinePhase_MACHINE_PHASE_FAILED && c.GetReason() == "ConfigureFailed" &&
			strings.Contains(c.GetMessage(), "ansible-playbook exited 2") && strings.Contains(c.GetMessage(), "tried again at "+at(apiserver.StepRetryFirst)), st.String()
	})
	w.advance(apiserver.StepRetryFirst - time.Minute)
	if n := count(); n != 1 {
		t.Fatalf("the configure phase ran %d times before its backoff passed", n)
	}
	<-mu
	fail = false
	mu <- struct{}{}
	w.advance(2 * time.Minute)
	w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	if n := count(); n != 2 {
		t.Errorf("the configure phase ran %d times", n)
	}
}
