package apiserver_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// sleeping reports whether the fake's last command is a sleep, which runs
// until its call ends.
func (w *world) sleeping() bool {
	execs := w.fake.Execs()
	return len(execs) > 0 && execs[len(execs)-1].Command[0] == "sleep"
}

// TestACallInFlightKeepsABoxInUse: a box is never stopped for idleness
// while a call into it runs, however long, and the call's end is a use.
func TestACallInFlightKeepsABoxInUse(t *testing.T) {
	w := newWorld(t, idleAfter(time.Hour))
	w.readyBox("dev")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := exec(ctx, w.as(workstream).envs, &envv1alpha1.ExecRequest{Environment: boxEnv("dev"), Command: argv("sleep")})
		done <- err
	}()
	testutil.Eventually(t, "the command running", w.sleeping)
	w.advance(2 * time.Hour)
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("a box with a call running for two hours is %v", p)
	}
	cancel()
	<-done
	w.advance(59 * time.Minute)
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("a box whose call ended 59 minutes ago is %v", p)
	}
	w.advance(2 * time.Minute)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
}

// TestAUseAsAnIdleStopIsDecidedKeepsTheBox: a use that comes between an
// idle stop being decided and its being written keeps the box running.
func TestAUseAsAnIdleStopIsDecidedKeepsTheBox(t *testing.T) {
	w := newWorld(t, idleAfter(time.Hour))
	w.readyBox("dev")
	var once sync.Once
	w.srv.SetBeforeIdleStop(func(project, box string) { once.Do(func() { w.srv.Touch(project, box) }) })
	w.advance(61 * time.Minute)
	if b := w.box("dev"); b.GetStatus().GetPhase() != envv1alpha1.BoxPhase_BOX_PHASE_READY || b.GetStatus().GetStoppedReason() != "" {
		t.Errorf("a box used as its idle stop was decided: %v", b.GetStatus())
	}
	w.advance(61 * time.Minute)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
}

// TestACallToAFailedOrGoingBoxEndsAtOnce: a call into a FAILED box is
// refused at once, saying why, as is one into a box being deleted while
// the call waits for it.
func TestACallToAFailedOrGoingBoxEndsAtOnce(t *testing.T) {
	w := newWorld(t)
	w.fake.FailBuilds(fmt.Errorf("step 3/7: npm ci exited 1: %w", provider.ErrFailed))
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_FAILED)
	ctx, cancel := context.WithTimeout(t.Context(), testutil.Wait)
	defer cancel()
	_, err := exec(ctx, w.as(workstream).envs, &envv1alpha1.ExecRequest{Environment: boxEnv("dev"), Command: argv("true")})
	wantCode(t, err, connect.CodeFailedPrecondition, "a call into a failed box")
	if !strings.Contains(messageOfErr(err), "npm ci exited 1") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	// A box still building, which the call waits for, deleted meanwhile.
	w.fake.FailBuilds(nil)
	release := w.fake.HoldBuilds()
	defer release()
	going := box("going", "api")
	going.Spec.Addons = []string{"desktop"}
	going.Metadata.Finalizers = []string{"test.example.com/hold"}
	w.mustApply(registry.Box, going)
	w.waitBox("going", envv1alpha1.BoxPhase_BOX_PHASE_BUILDING)
	done := make(chan error, 1)
	go func() {
		_, err := exec(ctx, w.as(workstream).envs, &envv1alpha1.ExecRequest{Environment: boxEnv("going"), Command: argv("true")})
		done <- err
	}()
	testutil.Eventually(t, "the call waiting for the box", func() bool { return w.srv.InUse("web", "going") })
	if _, err := w.asSystem().generic.Delete(t.Context(), registry.Box, client.Key{Project: "web", Name: "going"}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, <-done, connect.CodeNotFound, "a call into a box deleted as it waited")
}

// slowReads holds a read of /slow until its call ends.
type slowReads struct{ provider.Provider }

func (p slowReads) ReadFile(ctx context.Context, in provider.Instance, path string, w io.Writer) error {
	if path == "/slow" {
		<-ctx.Done()
		return ctx.Err()
	}
	return p.Provider.ReadFile(ctx, in, path, w)
}

// TestAStreamEndsWhenItsTokenGoes: an Exec or a ReadFile stream under way
// ends once the workstream token it was opened with is rotated or deleted.
func TestAStreamEndsWhenItsTokenGoes(t *testing.T) {
	w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
		base := c.Providers
		c.Providers = func(target string) (provider.Provider, error) {
			p, err := base(target)
			if p == nil {
				return p, err
			}
			return slowReads{p}, err
		}
	}})
	w.readyBox("dev")
	streams := map[string]func(clients) error{
		"Exec": func(c clients) error {
			_, err := exec(t.Context(), c.envs, &envv1alpha1.ExecRequest{Environment: boxEnv("dev"), Command: argv("sleep")})
			return err
		},
		"ReadFile": func(c clients) error {
			_, _, err := readFile(t.Context(), c.envs, &envv1alpha1.ReadFileRequest{Environment: boxEnv("dev"), Path: "/slow"})
			return err
		},
	}
	n := 0
	for name, stream := range streams {
		for _, how := range []string{"rotated", "deleted"} {
			n++
			token := fmt.Sprintf("ws-%d", n)
			secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(w.local().issuer.CreateServiceToken(t.Context(),
				createToken(token, iamv1alpha1.Role_ROLE_WORKSTREAM, ""))).GetSecret()
			done := make(chan error, 1)
			go func() { done <- stream(w.withToken(secret)) }()
			testutil.Eventually(t, name+" under way", func() bool { return w.srv.InUse("web", "dev") })
			switch how {
			case "rotated":
				must[iamv1alpha1.RotateServiceTokenResponse](t)(w.local().issuer.RotateServiceToken(t.Context(), rotateToken(token)))
			case "deleted":
				if _, err := w.asSystem().tokens.DeleteServiceToken(t.Context(), deleteToken(token)); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			testutil.Eventually(t, fmt.Sprintf("the %s stream ended, its token %s", name, how), func() bool {
				w.clock.Advance(auth.RecheckInterval)
				select {
				case err = <-done:
					return true
				default:
					return false
				}
			})
			if c := connect.CodeOf(err); c != connect.CodeUnauthenticated && c != connect.CodePermissionDenied {
				t.Errorf("the %s stream whose token was %s ended with %v", name, how, err)
			}
			testutil.Eventually(t, "the call over", func() bool { return !w.srv.InUse("web", "dev") })
		}
	}
}

// TestTheTargetSaysWhenItsProviderFallsSilentOrAJobFails: Reporting turns
// false after three report intervals without a report, and last_error
// holds a job's failure, with when, until a job on the target succeeds.
func TestTheTargetSaysWhenItsProviderFallsSilentOrAJobFails(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	target := func() *targetsv1alpha1.TargetStatus {
		o, err := w.get(registry.Target, "", "t1")
		if err != nil {
			t.Fatal(err)
		}
		return o.(*targetsv1alpha1.Target).GetStatus()
	}
	reporting := func() string {
		c := condition(target().GetConditions(), apiserver.TargetReporting)
		return c.GetStatus().String() + "/" + c.GetReason()
	}
	// The fake reports once it follows the API, which it starts to.
	testutil.Eventually(t, "a report", func() bool {
		if err := w.fake.Report(ctx); err != nil {
			t.Fatal(err)
		}
		return target().GetLastReportTime() != nil
	})
	w.advance(2 * apiserver.DefaultReportInterval)
	if got := reporting(); got != "CONDITION_STATUS_TRUE/Reported" {
		t.Errorf("two intervals after a report, Reporting is %s", got)
	}
	w.advance(2 * apiserver.DefaultReportInterval)
	testutil.Eventually(t, "Reporting false", func() (bool, string) {
		got := reporting()
		return got == "CONDITION_STATUS_FALSE/Silent", got
	})
	if err := w.fake.Report(ctx); err != nil {
		t.Fatal(err)
	}
	if got := reporting(); got != "CONDITION_STATUS_TRUE/Reported" {
		t.Errorf("after a report, Reporting is %s", got)
	}

	w.fake.FailBuilds(fmt.Errorf("the registry refused the push: %w", provider.ErrFailed))
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_FAILED)
	when := w.clock.Now().UTC().Format(time.RFC3339)
	testutil.Eventually(t, "the failure in last_error", func() (bool, string) {
		e := target().GetLastError()
		return strings.Contains(e, "the registry refused the push") && strings.Contains(e, when) && strings.Contains(e, "golden/"), e
	})
	w.fake.FailBuilds(nil)
	w.readyBox("ok", func(b *envv1alpha1.Box) { b.Spec.Addons = []string{"desktop"} })
	testutil.Eventually(t, "last_error cleared by a job that succeeded", func() (bool, string) {
		e := target().GetLastError()
		return e == "", e
	})
}
