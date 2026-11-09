package apiserver_test

import (
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// tailnetOf asks a box or machine to join a tailnet with the key Secret
// ts holds.
func tailnetOf(hostname string) *envv1alpha1.Tailnet {
	return &envv1alpha1.Tailnet{AuthSecret: "ts", Hostname: hostname, AcceptRoutes: true}
}

// waitTailnet waits for a box's TailnetJoined to say reason, and returns
// the condition.
func (w *world) waitTailnet(name, reason string) *metav1.Condition {
	w.t.Helper()
	var c *metav1.Condition
	testutil.Eventually(w.t, "box "+name+" TailnetJoined "+reason, func() (bool, string) {
		c = conditions.Find(w.box(name).GetStatus().GetConditions(), apiserver.TailnetJoined)
		return c.GetReason() == reason, c.String()
	})
	return c
}

// TestABoxJoinsItsTailnetOnce: a box with tailnet has its instance join
// with the key its Secret holds, named in the instance's spec by the
// Secret alone, and comes back as the same node after a restart without
// joining again. The key reaches no object, event, log line or database
// file.
func TestABoxJoinsItsTailnetOnce(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	const key = "tskey-auth-kCanary-Wq81Zx"
	w.mustApply(registry.Secret, secret("web", "ts", map[string]string{"authkey": key}))
	b := w.readyBox("dev", func(b *envv1alpha1.Box) { b.Spec.Tailnet = tailnetOf("dev-box") })
	c := w.waitTailnet("dev", provider.TailnetJoined)
	if c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("TailnetJoined %v", c)
	}
	in, _ := w.instance(b.GetStatus().GetInstance())
	if got := in.GetSpec().GetTailnet(); got.GetAuthSecret() != "ts" || got.GetHostname() != "dev-box" || !got.GetAcceptRoutes() {
		t.Errorf("the instance's tailnet is %v", got)
	}
	ref := provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}
	if n := w.fake.TailnetJoins(ref); n != 1 {
		t.Errorf("it joined %d times", n)
	}

	b = w.box("dev")
	b.Spec.RunState = envv1alpha1.RunState_RUN_STATE_STOPPED
	w.mustApply(registry.Box, b)
	stopped := w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
	stopped.Spec.RunState = envv1alpha1.RunState_RUN_STATE_RUNNING
	w.mustApply(registry.Box, stopped)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_READY)
	w.waitTailnet("dev", provider.TailnetJoined)
	if n := w.fake.TailnetJoins(ref); n != 1 {
		t.Errorf("after a restart it joined %d times; its node has state", n)
	}
	w.waitSettled()
	w.nowhere(key, &logs)
}

// TestABoxWithoutItsKeyIsReady: a box whose auth_secret is not there yet
// is READY, its TailnetJoined saying NoKey and naming the Secret; once
// the Secret holds the key, the box joins.
func TestABoxWithoutItsKeyIsReady(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	b := w.readyBox("dev", func(b *envv1alpha1.Box) { b.Spec.Tailnet = tailnetOf("dev-box") })
	c := w.waitTailnet("dev", provider.TailnetNoKey)
	if !strings.Contains(c.GetMessage(), `"ts"`) {
		t.Errorf("NoKey says %q; want the Secret named", c.GetMessage())
	}
	w.mustApply(registry.Secret, secret("web", "ts", map[string]string{"authkey": "tskey-auth-kLate"}))
	w.waitTailnet("dev", provider.TailnetJoined)
	if w.box("dev").GetStatus().GetPhase() != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("the box left READY: %v", w.box("dev").GetStatus())
	}
	if n := w.fake.TailnetJoins(provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}); n != 1 {
		t.Errorf("it joined %d times", n)
	}
}

// TestARejectedKeyIsSaidNotFatal: a key the tailnet refuses leaves the box
// READY, its TailnetJoined saying KeyRejected, without the key.
func TestARejectedKeyIsSaidNotFatal(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const key = "tskey-rejected-kCanary-Lm22"
	w.mustApply(registry.Secret, secret("web", "ts", map[string]string{"authkey": key}))
	w.readyBox("dev", func(b *envv1alpha1.Box) { b.Spec.Tailnet = tailnetOf("dev-box") })
	c := w.waitTailnet("dev", provider.TailnetKeyRejected)
	if c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || strings.Contains(c.GetMessage(), key) {
		t.Errorf("TailnetJoined %v", c)
	}
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("a refused key left the box %v", p)
	}
}

// TestABoxWithoutTailnetJoinsNone: a box without tailnet has an instance
// that joins none, and no TailnetJoined.
func TestABoxWithoutTailnetJoinsNone(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	b := w.readyBox("dev")
	in, _ := w.instance(b.GetStatus().GetInstance())
	if in.GetSpec().GetTailnet() != nil || conditions.Find(b.GetStatus().GetConditions(), apiserver.TailnetJoined) != nil ||
		conditions.Find(in.GetStatus().GetConditions(), provider.InstanceTailnetJoined) != nil {
		t.Errorf("a box without tailnet: %v; its instance %v", b.GetStatus(), in)
	}
}

// TestATailnetIsValidated: a tailnet needs its Secret named and a hostname
// that is a DNS label.
func TestATailnetIsValidated(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Repository, repository("api"))
	for _, tn := range []*envv1alpha1.Tailnet{
		{Hostname: "dev-box"},
		{AuthSecret: "ts", Hostname: ""},
		{AuthSecret: "ts", Hostname: "Dev_Box"},
		{AuthSecret: "ts", Hostname: "dev.box"},
		{AuthSecret: "ts", Hostname: strings.Repeat("a", 64)},
		{AuthSecret: "Not A Name", Hostname: "dev-box"},
	} {
		b := box("dev", "api")
		b.Spec.Tailnet = tn
		_, err := w.asSystem().generic.Apply(t.Context(), registry.Box, b)
		wantCode(t, err, connect.CodeInvalidArgument, "a box's tailnet "+tn.String())
		m := machine("k3s")
		m.Spec.Tailnet = tn
		_, err = w.asSystem().generic.Apply(t.Context(), registry.Machine, m)
		wantCode(t, err, connect.CodeInvalidArgument, "a machine's tailnet "+tn.String())
	}
}

// TestAMachineJoinsItsTailnetBeforeConfigure: a machine with tailnet joins
// in its own instance before its configure phase, the key on that
// command's stdin alone: no command's arguments or environment, and
// nothing its golden's builder ran, hold it, nor any object, event, log
// line or database file.
func TestAMachineJoinsItsTailnetBeforeConfigure(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	const key = "tskey-auth-kMachine-Pp07"
	w.mustApply(registry.Secret, secret("web", "ts", map[string]string{"authkey": key}))
	m := machine("k3s")
	m.Spec.Tailnet = tailnetOf("k3s-1")
	w.mustApply(registry.Machine, m)
	ready := w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	c := conditions.Find(ready.GetStatus().GetConditions(), apiserver.TailnetJoined)
	if c.GetReason() != provider.TailnetJoined || c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("TailnetJoined %v", c)
	}
	joined, configured, scrubbed := -1, -1, false
	for i, e := range w.fake.Execs() {
		if slices.ContainsFunc(e.Command, func(a string) bool { return strings.Contains(a, key) }) {
			t.Errorf("a command was given the key in its arguments: %q", e.Command)
		}
		for k, v := range e.Env {
			if strings.Contains(v, key) {
				t.Errorf("a command was given the key in its environment as %s", k)
			}
		}
		join := isJoin(e.Command)
		golden := e.Instance.Name != ready.GetStatus().GetInstance()
		if golden && (join || e.Stdin) {
			t.Errorf("%s, its golden's builder, joined a tailnet or was given a stdin: %q", e.Instance.Name, e.Command)
		}
		if join && !e.Stdin {
			t.Errorf("the join was given no stdin: %q", e.Command)
		}
		if golden && strings.Contains(strings.Join(e.Command, " "), "tailscaled.state") {
			scrubbed = true
		}
		switch {
		case join && joined < 0:
			joined = i
		case slices.Contains(e.Command, "--tags=configure"):
			configured = i
		}
	}
	if !scrubbed {
		t.Error("its golden was not scrubbed of tailscale state")
	}
	if joined < 0 || configured < 0 || joined > configured {
		t.Errorf("the join ran at %d, the configure phase at %d; want it before", joined, configured)
	}
	w.waitSettled()
	w.nowhere(key, &logs)
}

// TestAMachineWithoutItsKeyIsReady: a machine whose auth_secret is not
// there is configured and READY all the same, TailnetJoined saying NoKey.
func TestAMachineWithoutItsKeyIsReady(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	m := machine("k3s")
	m.Spec.Tailnet = tailnetOf("k3s-1")
	w.mustApply(registry.Machine, m)
	ready := w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	if c := conditions.Find(ready.GetStatus().GetConditions(), apiserver.TailnetJoined); c.GetReason() != provider.TailnetNoKey || !strings.Contains(c.GetMessage(), `"ts"`) {
		t.Errorf("TailnetJoined %v", c)
	}
	for _, e := range w.fake.Execs() {
		if isJoin(e.Command) {
			t.Errorf("a machine without its key ran the join: %q", e.Command)
		}
	}
}

// TestABoxsTailnetComesAndGoes: a tailnet added to a READY box reaches its
// instance and joins; a new hostname reaches the instance; removed, the
// instance joins none and the box has no TailnetJoined; added again, its
// node, forgotten, is given the key again and joins anew. The box stays
// READY throughout.
func TestABoxsTailnetComesAndGoes(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Secret, secret("web", "ts", map[string]string{"authkey": "tskey-auth-kLive"}))
	b := w.readyBox("dev")
	ref := provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}
	instanceTailnet := func(want *envv1alpha1.Tailnet) {
		t.Helper()
		testutil.Eventually(t, "the instance's tailnet "+want.String(), func() (bool, string) {
			in, _ := w.instance(ref.Name)
			return proto.Equal(in.GetSpec().GetTailnet(), want), in.GetSpec().GetTailnet().String()
		})
	}
	set := func(tn *envv1alpha1.Tailnet) {
		t.Helper()
		b := w.box("dev")
		b.Spec.Tailnet, b.Metadata.ResourceVersion = tn, 0
		w.mustApply(registry.Box, b)
	}

	set(tailnetOf("dev-box"))
	instanceTailnet(tailnetOf("dev-box"))
	w.waitTailnet("dev", provider.TailnetJoined)

	renamed := tailnetOf("dev-renamed")
	set(renamed)
	instanceTailnet(renamed)
	w.waitTailnet("dev", provider.TailnetJoined)

	set(nil)
	instanceTailnet(nil)
	// Its provider says nothing of a tailnet once it left it.
	testutil.Eventually(t, "the box's and its instance's TailnetJoined going", func() (bool, string) {
		in, _ := w.instance(ref.Name)
		c := conditions.Find(w.box("dev").GetStatus().GetConditions(), apiserver.TailnetJoined)
		ic := conditions.Find(in.GetStatus().GetConditions(), provider.InstanceTailnetJoined)
		return c == nil && ic == nil, c.String() + " " + ic.String()
	})

	set(renamed)
	w.waitTailnet("dev", provider.TailnetJoined)
	if n := w.fake.TailnetJoins(ref); n != 2 {
		t.Errorf("it joined %d times; once, and once more after its node was forgotten", n)
	}
	if p := w.box("dev").GetStatus().GetPhase(); p != envv1alpha1.BoxPhase_BOX_PHASE_READY {
		t.Errorf("the box is %v", p)
	}
}

// TestAMachinesTailnetComesAndGoes: a tailnet added to a READY machine
// joins it over its SSH; a new hostname is set on its node; removed, it
// leaves, and the machine has no TailnetJoined. It stays READY.
func TestAMachinesTailnetComesAndGoes(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.mustApply(registry.Secret, secret("web", "ts", map[string]string{"authkey": "tskey-auth-kLiveVM"}))
	w.mustApply(registry.Machine, machine("k3s"))
	ready := w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
	instance := ready.GetStatus().GetInstance()
	ran := func(what string, match func([]string) bool) {
		t.Helper()
		testutil.Eventually(t, what, func() bool {
			for _, e := range w.fake.Execs() {
				if e.Instance.Name == instance && match(e.Command) {
					return true
				}
			}
			return false
		})
	}
	set := func(tn *envv1alpha1.Tailnet) {
		t.Helper()
		m := w.machine("k3s")
		m.Spec.Tailnet, m.Metadata.ResourceVersion = tn, 0
		w.mustApply(registry.Machine, m)
	}
	cond := func() *metav1.Condition {
		return conditions.Find(w.machine("k3s").GetStatus().GetConditions(), apiserver.TailnetJoined)
	}

	set(tailnetOf("k3s-1"))
	ran("the join", func(c []string) bool {
		return slices.Contains(c, "k3s-1") && strings.Contains(strings.Join(c, " "), "tailscale up")
	})
	testutil.Eventually(t, "TailnetJoined Joined", func() (bool, string) { return cond().GetReason() == provider.TailnetJoined, cond().String() })

	set(tailnetOf("k3s-2"))
	ran("tailscale set", func(c []string) bool {
		return slices.Contains(c, "k3s-2") && strings.Contains(strings.Join(c, " "), "tailscale set")
	})
	set(nil)
	ran("tailscale logout", func(c []string) bool { return strings.Contains(strings.Join(c, " "), "tailscale logout") })
	testutil.Eventually(t, "TailnetJoined going", func() (bool, string) { return cond() == nil, cond().String() })
	if p := w.machine("k3s").GetStatus().GetPhase(); p != envv1alpha1.MachinePhase_MACHINE_PHASE_READY {
		t.Errorf("the machine is %v", p)
	}
}

// TestARefusedKeyIsGivenAgain: a box whose key the tailnet refused is
// given it again once the step's backoff passes, not before, and is given
// a new key at once when its Secret changes, then joins.
func TestARefusedKeyIsGivenAgain(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	w.mustApply(registry.Secret, secret("web", "ts", map[string]string{"authkey": "tskey-rejected-kOld"}))
	b := w.readyBox("dev", func(b *envv1alpha1.Box) { b.Spec.Tailnet = tailnetOf("dev-box") })
	ref := provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}
	w.waitTailnet("dev", provider.TailnetKeyRejected)
	gives := func(n int) {
		t.Helper()
		testutil.Eventually(t, fmt.Sprintf("the key given %d times", n), func() (bool, string) {
			return w.fake.TailnetGives(ref) == n, fmt.Sprint(w.fake.TailnetGives(ref))
		})
	}
	gives(1)
	// A key given is no failure: the box says when it is given again.
	testutil.Eventually(t, "the give said pending", func() (bool, string) {
		c := conditions.Find(w.box("dev").GetStatus().GetConditions(), apiserver.TailnetJoined)
		return strings.Contains(c.GetMessage(), "given again at"), c.String()
	})
	if c := conditions.Find(w.box("dev").GetStatus().GetConditions(), apiserver.TailnetJoined); strings.Contains(c.GetMessage(), "failed at") {
		t.Errorf("a key given reads as a failure: %q", c.GetMessage())
	}
	w.advance(apiserver.StepRetryFirst / 2)
	w.waitSettled()
	if n := w.fake.TailnetGives(ref); n != 1 {
		t.Errorf("a refused key was given again %d times within its backoff", n-1)
	}
	w.advance(apiserver.StepRetryFirst)
	gives(2)
	w.mustApply(registry.Secret, secret("web", "ts", map[string]string{"authkey": "tskey-auth-kNew"}))
	w.waitTailnet("dev", provider.TailnetJoined)
	if n := w.fake.TailnetJoins(ref); n != 1 {
		t.Errorf("it joined %d times", n)
	}
	if strings.Contains(logs.String(), "an instance step failed") {
		t.Error("a key given was logged as a step that failed")
	}
}

// TestAKeyAFailedGiveQuotesIsNowhere: a target that fails a give quoting
// the key leaves it in no condition, event, object or log; the box stays
// READY, saying it could not be given.
func TestAKeyAFailedGiveQuotesIsNowhere(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	const key = "tskey-auth-kQuoted-Mm73"
	w.fake.FailJoinTailnet(func(k string) error { return fmt.Errorf("the target refused %s: %w", k, provider.ErrFailed) })
	w.mustApply(registry.Secret, secret("web", "ts", map[string]string{"authkey": key}))
	w.readyBox("dev", func(b *envv1alpha1.Box) { b.Spec.Tailnet = tailnetOf("dev-box") })
	testutil.Eventually(t, "the failed give said", func() (bool, string) {
		c := conditions.Find(w.box("dev").GetStatus().GetConditions(), apiserver.TailnetJoined)
		return strings.Contains(c.GetMessage(), "the target refused"), c.String()
	})
	w.waitSettled()
	w.nowhere(key, &logs)
}

// isJoin says a command is a VM's join.
func isJoin(command []string) bool {
	return strings.Contains(strings.Join(command, " "), "tailscale up")
}

// TestAMachinesUnjoinedKeyIsTriedAfterItsWait: a machine that did not join
// with its key, refused (KeyRejected) or failed (Failed), in its configure
// phase, is not tried again with it at once, nor in a loop: once more after
// the wait, then at once with a new key in its Secret. One whose Secret
// holds no key (NoKey) is not tried until it does, then at once.
func TestAMachinesUnjoinedKeyIsTriedAfterItsWait(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		key, reason string
		code        int
	}{
		{"tskey-rejected-kVM", provider.TailnetKeyRejected, 10},
		{"tskey-auth-kStopped", provider.TailnetFailed, 11},
		{"", provider.TailnetNoKey, 0},
	} {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			var mu sync.Mutex
			joins := 0
			w.fake.OnCommand(func(req provider.ExecRequest) (int, bool) {
				if !isJoin(req.Command) {
					return 0, false
				}
				b, _ := io.ReadAll(req.Stdin)
				mu.Lock()
				joins++
				mu.Unlock()
				if strings.TrimSpace(string(b)) == tt.key {
					_, _ = io.WriteString(req.Stdout, "backend error: invalid key: not valid")
					return tt.code, true
				}
				_, _ = io.WriteString(req.Stdout, "joined as k3s-1")
				return 0, true
			})
			tried := func() int { mu.Lock(); defer mu.Unlock(); return joins }
			if tt.key != "" {
				w.mustApply(registry.Secret, secret("web", "ts", map[string]string{"authkey": tt.key}))
			}
			m := machine("k3s")
			m.Spec.Tailnet = tailnetOf("k3s-1")
			w.mustApply(registry.Machine, m)
			ready := w.waitMachine("k3s", envv1alpha1.MachinePhase_MACHINE_PHASE_READY)
			if c := conditions.Find(ready.GetStatus().GetConditions(), apiserver.TailnetJoined); c.GetReason() != tt.reason {
				t.Errorf("TailnetJoined %v", c)
			}
			want := 1
			if tt.key == "" {
				want = 0
			}
			w.waitSettled()
			w.advance(apiserver.StepRetryFirst / 2)
			w.waitSettled()
			if n := tried(); n != want {
				t.Fatalf("tried %d times within the wait, want %d", n, want)
			}
			if tt.key != "" {
				w.advance(apiserver.StepRetryFirst)
				want++
				testutil.Eventually(t, "a try after the wait", func() (bool, string) { return tried() == want, fmt.Sprint(tried()) })
				w.waitSettled()
				if n := tried(); n != want {
					t.Errorf("tried %d times after one wait, want %d", n, want)
				}
			}
			w.mustApply(registry.Secret, secret("web", "ts", map[string]string{"authkey": "tskey-auth-kNewVM"}))
			testutil.Eventually(t, "TailnetJoined Joined with the new key", func() (bool, string) {
				c := conditions.Find(w.machine("k3s").GetStatus().GetConditions(), apiserver.TailnetJoined)
				return c.GetReason() == provider.TailnetJoined, c.String()
			})
			if n := tried(); n != want+1 {
				t.Errorf("tried %d times in all, want %d", n, want+1)
			}
		})
	}
}
