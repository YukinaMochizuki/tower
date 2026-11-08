package providertest

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// TailnetHarness sets a running instance's tailnet as tower's box
// controller does and waits for its provider's TailnetJoined.
type TailnetHarness struct {
	// Set sets the instance's spec.tailnet, nil for none.
	Set func(t *testing.T, in provider.Instance, tn *envv1alpha1.Tailnet)
	// Joined waits until the instance's TailnetJoined, as its provider
	// reports it of the spec as it is, says reason, "" for none at all,
	// and returns it.
	Joined func(t *testing.T, in provider.Instance, reason string) *metav1.Condition
}

// TailnetThrough is a Harness.Tailnet for a harness whose Instances, in
// project, are applied and read through c, waiting up to within.
func TailnetThrough(c *client.Client, project string, within time.Duration) *TailnetHarness {
	get := func(t *testing.T, ref provider.Instance) *envv1alpha1.Instance {
		o, err := c.Get(t.Context(), registry.Instance, client.Key{Project: project, Name: ref.Name})
		if err != nil {
			t.Fatal(err)
		}
		return o.(*envv1alpha1.Instance)
	}
	return &TailnetHarness{
		Set: func(t *testing.T, ref provider.Instance, tn *envv1alpha1.Tailnet) {
			t.Helper()
			// The provider writes the instance's status as it goes, which
			// can move it between the read and the apply.
			testutil.EventuallyWithin(t, within, "the instance's tailnet set", func() (bool, string) {
				in := proto.Clone(get(t, ref)).(*envv1alpha1.Instance)
				in.Spec.Tailnet = tn
				_, err := c.Apply(t.Context(), registry.Instance, in)
				if err != nil && connect.CodeOf(err) != connect.CodeAborted {
					t.Fatal(err)
				}
				return err == nil, fmt.Sprint(err)
			})
		},
		Joined: func(t *testing.T, ref provider.Instance, reason string) *metav1.Condition {
			t.Helper()
			var cond *metav1.Condition
			testutil.EventuallyWithin(t, within, "TailnetJoined "+reason, func() (bool, string) {
				in := get(t, ref)
				cond = conditions.Find(in.GetStatus().GetConditions(), provider.InstanceTailnetJoined)
				if reason == "" {
					return cond == nil, cond.String()
				}
				return cond.GetReason() == reason && cond.GetObservedGeneration() >= in.GetMetadata().GetGeneration(), cond.String()
			})
			return cond
		},
	}
}

// testTailnet: a container whose spec joins a tailnet says NoKey until it
// is given the key, then Joined; a new hostname keeps it joined; with no
// tailnet it says nothing of one and refuses a key; joining again, its
// node forgotten, it waits for a key again.
func testTailnet(t *testing.T, h Harness) {
	if h.Tailnet == nil {
		t.Skip("the harness sets no instance's tailnet")
	}
	tn := &envv1alpha1.Tailnet{AuthSecret: "ts", Hostname: "tn-node"}
	spec := h.container("net-tn", "tn")
	spec.Tailnet = tn
	in, _ := h.start(t, "tailnet", spec)
	h.Tailnet.Joined(t, in, provider.TailnetNoKey)
	if err := h.Provider.JoinTailnet(t.Context(), provider.TailnetJoin{Instance: in, Key: "tskey-auth-kContract"}); err != nil {
		t.Fatal(err)
	}
	if c := h.Tailnet.Joined(t, in, provider.TailnetJoined); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("joined: %v", c)
	}
	renamed := &envv1alpha1.Tailnet{AuthSecret: "ts", Hostname: "tn-renamed"}
	h.Tailnet.Set(t, in, renamed)
	h.Tailnet.Joined(t, in, provider.TailnetJoined)

	h.Tailnet.Set(t, in, nil)
	h.Tailnet.Joined(t, in, "")
	err := h.Provider.JoinTailnet(t.Context(), provider.TailnetJoin{Instance: in, Key: "tskey-auth-kNone"})
	if !errors.Is(err, provider.ErrFailed) {
		t.Errorf("a key given to an instance that joins no tailnet: %v, want ErrFailed", err)
	}
	h.Tailnet.Set(t, in, renamed)
	h.Tailnet.Joined(t, in, provider.TailnetNoKey)
}
