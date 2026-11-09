package hangar_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarsecrets "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/fake"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/hangartest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// tailnetWorld is a world whose tower opens the key of Secret "ts" as
// key, and of no other.
func tailnetWorld(t *testing.T, key string) (*hangartest.World, *fake.Fake) {
	t.Helper()
	return newWorld(t, func(c *hangar.Config) {
		c.TailnetKeys = func(_ context.Context, project, secret string) (string, error) {
			if project != apiservertest.Project || secret != "ts" || key == "" {
				return "", provider.ErrNotFound
			}
			return key, nil
		}
	})
}

// applyTailnetBox applies a container instance that joins a tailnet with
// Secret ts, as box-dev on it.
func applyTailnetBox(t *testing.T, w *hangartest.World, name string) {
	t.Helper()
	spec := container("")
	spec.Tailnet = &envv1alpha1.Tailnet{AuthSecret: "ts", Hostname: "box-dev", AcceptRoutes: true}
	if _, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, &envv1alpha1.Instance{
		Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name}, Spec: spec,
	}); err != nil {
		t.Fatal(err)
	}
}

// waitTailnet waits for an instance's TailnetJoined to say reason.
func waitTailnet(t *testing.T, w *hangartest.World, name, reason string) *metav1.Condition {
	t.Helper()
	var c *metav1.Condition
	testutil.Eventually(t, name+" TailnetJoined "+reason, func() (bool, string) {
		c = conditions.Find(w.Instance(t, name).GetStatus().GetConditions(), provider.InstanceTailnetJoined)
		return c.GetReason() == reason, c.String()
	})
	return c
}

// TestABoxContainerJoinsWithAHangarSecretOfItsOwn: a container instance
// with tailnet is a Container with spec.tailscale naming a hangar Secret
// of its own, which holds the key tower opened and carries its labels;
// hangar's TailnetJoined is the instance's; the sweep keeps the Secret
// while the instance is there, and it goes with the instance.
func TestABoxContainerJoinsWithAHangarSecretOfItsOwn(t *testing.T) {
	const key = "tskey-auth-kHangar-Qe55"
	w, f := tailnetWorld(t, key)
	applyTailnetBox(t, w, "box-dev")
	in := w.WaitPhase(t, "box-dev", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	c := waitTailnet(t, w, "box-dev", provider.TailnetJoined)
	if c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("TailnetJoined %v", c)
	}
	object := "t-" + in.GetMetadata().GetUid()
	ct, err := f.Containers().Get(t.Context(), object)
	if err != nil {
		t.Fatal(err)
	}
	ts := ct.GetSpec().GetTailscale()
	if ts.GetAuthSecret() != object+"-tailnet" || ts.GetHostname() != "box-dev" || !ts.GetAcceptRoutes() {
		t.Errorf("the container's tailscale is %v", ts)
	}
	sec := f.Secret(object + "-tailnet")
	if string(sec.GetSpec().GetData()[provider.TailnetKeyName]) != key || sec.GetSpec().GetType() != hangarsecrets.SecretType_SECRET_TYPE_TAILSCALE_AUTH {
		t.Fatalf("the hangar Secret is a %v holding %v", sec.GetSpec().GetType(), sec.GetSpec().GetData())
	}
	if l := sec.GetMetadata().GetLabels(); l[hangar.LabelUID] != in.GetMetadata().GetUid() || l[hangar.LabelTarget] != hangartest.Target {
		t.Errorf("the hangar Secret's labels: %v", l)
	}
	if strings.Contains(ct.String(), key) {
		t.Error("the Container's spec or status holds the key")
	}
	if err := w.Target.Sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.Secret(object+"-tailnet") == nil {
		t.Error("the sweep took the key of an instance still there")
	}
	if _, err := w.Env.AsSystem().Delete(t.Context(), registry.Instance, client.Key{Project: apiservertest.Project, Name: "box-dev"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the hangar Secret going with its instance", func() bool { return f.Secret(object+"-tailnet") == nil })
}

// TestADeletedBoxLeavesItsTailnetBeforeItsContainerGoes: an instance
// deleted has its Container applied without spec.tailscale, so hangar logs
// its node out, before the Container is deleted, and its key's hangar
// Secret goes only after that.
func TestADeletedBoxLeavesItsTailnetBeforeItsContainerGoes(t *testing.T) {
	w, f := tailnetWorld(t, "tskey-auth-kOrder-Pp31")
	applyTailnetBox(t, w, "box-dev")
	in := w.WaitPhase(t, "box-dev", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	waitTailnet(t, w, "box-dev", provider.TailnetJoined)
	object := "t-" + in.GetMetadata().GetUid()
	from := len(f.Writes())
	if _, err := w.Env.AsSystem().Delete(t.Context(), registry.Instance, client.Key{Project: apiservertest.Project, Name: "box-dev"}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the hangar Secret going with its instance", func() bool { return f.Secret(object+"-tailnet") == nil })

	left, gone, keyGone := -1, -1, -1
	var seen []string
	for i, wr := range f.Writes()[from:] {
		switch {
		case wr.Kind == "container" && wr.Name == object && wr.Op == "apply":
			ts := wr.Object.(*hangarcompute.Container).GetSpec().GetTailscale()
			seen = append(seen, fmt.Sprintf("container applied, tailscale %v", ts != nil))
			if ts == nil && gone < 0 {
				left = i
			}
		case wr.Kind == "container" && wr.Name == object && wr.Op == "delete":
			seen = append(seen, "container deleted")
			if gone < 0 {
				gone = i
			}
		case wr.Kind == "secret" && wr.Name == object+"-tailnet" && wr.Op == "delete":
			seen = append(seen, "secret deleted")
			if keyGone < 0 {
				keyGone = i
			}
		}
	}
	if left < 0 || gone < left || keyGone < gone {
		t.Errorf("hangar took %q, want the Container applied without tailscale, then deleted, then its Secret deleted", seen)
	}
}

// TestAKeyTowerGivesLaterReachesHangar: a container whose key tower does
// not have as it is made runs, hangar saying NoKey, until tower gives the
// key (JoinTailnet), which writes it into its hangar Secret.
func TestAKeyTowerGivesLaterReachesHangar(t *testing.T) {
	const key = "tskey-auth-kLater-Hh20"
	w, f := tailnetWorld(t, "")
	applyTailnetBox(t, w, "box-dev")
	w.WaitPhase(t, "box-dev", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	waitTailnet(t, w, "box-dev", provider.TailnetNoKey)
	ref := provider.Instance{Project: apiservertest.Project, Name: "box-dev"}
	if err := w.Target.JoinTailnet(t.Context(), provider.TailnetJoin{Instance: ref, Key: key}); err != nil {
		t.Fatal(err)
	}
	waitTailnet(t, w, "box-dev", provider.TailnetJoined)
	if n := f.TailnetJoins(); n != 1 {
		t.Errorf("hangar joined %d containers", n)
	}
}

// TestHangarsRejectionIsTheInstances: a key hangar's tailnet refuses is
// the instance's KeyRejected, which leaves it running.
func TestHangarsRejectionIsTheInstances(t *testing.T) {
	w, _ := tailnetWorld(t, "tskey-rejected-kHangar")
	applyTailnetBox(t, w, "box-dev")
	waitTailnet(t, w, "box-dev", provider.TailnetKeyRejected)
	if p := w.Instance(t, "box-dev").GetStatus().GetPhase(); p != envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING {
		t.Errorf("a refused key left the instance %v", p)
	}
}

// TestATailnetComesAndGoesOnALiveContainer: added to a running container,
// a tailnet is its spec.tailscale, with its key's hangar Secret, and joins
// with no restart; a new hostname reaches the Container; removed, the
// Container joins none, its Secret goes, and the instance has no
// TailnetJoined.
func TestATailnetComesAndGoesOnALiveContainer(t *testing.T) {
	w, f := tailnetWorld(t, "tskey-auth-kLiveHangar")
	apply := func(tn *envv1alpha1.Tailnet) {
		t.Helper()
		spec := container("")
		spec.Tailnet = tn
		if _, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, &envv1alpha1.Instance{
			Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: "box-dev"}, Spec: spec,
		}); err != nil {
			t.Fatal(err)
		}
	}
	apply(nil)
	in := w.WaitPhase(t, "box-dev", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	object := "t-" + in.GetMetadata().GetUid()
	started := in.GetStatus().GetStartTime().AsTime()
	tailscale := func() string {
		ct, err := f.Containers().Get(t.Context(), object)
		if err != nil {
			t.Fatal(err)
		}
		return ct.GetSpec().GetTailscale().GetHostname()
	}

	apply(&envv1alpha1.Tailnet{AuthSecret: "ts", Hostname: "box-dev"})
	waitTailnet(t, w, "box-dev", provider.TailnetJoined)
	if f.Secret(object+"-tailnet") == nil || tailscale() != "box-dev" {
		t.Errorf("added: its Secret %v, its tailscale's hostname %q", f.Secret(object+"-tailnet") != nil, tailscale())
	}
	if got := w.Instance(t, "box-dev").GetStatus().GetStartTime().AsTime(); !got.Equal(started) {
		t.Errorf("its container started again, at %v, for its tailnet", got)
	}

	apply(&envv1alpha1.Tailnet{AuthSecret: "ts", Hostname: "box-renamed"})
	testutil.Eventually(t, "the new hostname on the Container", func() bool { return tailscale() == "box-renamed" })

	apply(nil)
	testutil.Eventually(t, "the tailnet gone from the Container, its Secret and the instance", func() (bool, string) {
		c := conditions.Find(w.Instance(t, "box-dev").GetStatus().GetConditions(), provider.InstanceTailnetJoined)
		return tailscale() == "" && f.Secret(object+"-tailnet") == nil && c == nil, c.String()
	})
}
