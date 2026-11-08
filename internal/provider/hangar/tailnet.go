package hangar

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarsecrets "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/controller"
	"github.com/YukinaMochizuki/tower/internal/provider"
)

// A container that joins a tailnet is a hangar Container with
// spec.tailscale, which hangar joins, with the key a hangar Secret of the
// instance's holds, and leaves as the spec comes and goes, with no restart
// (docs/decisions/tailnet.md). The provider writes that Secret before the
// Container names it, the key opened by tower (Config.TailnetKeys), again
// when tower gives the key (JoinTailnet), and deletes it with the
// instance, or with its tailnet once the Container no longer names it
// (dropTailnetKey).

// tailnetSecretName is the hangar Secret holding the key of a container's
// tailnet, named for its object.
func tailnetSecretName(object string) string { return object + "-tailnet" }

// tailscaleOf is the tailscale of the instance's Container, nil for one
// that joins no tailnet.
func tailscaleOf(in *envv1alpha1.Instance, object string) *hangarcompute.ContainerTailscale {
	tn := in.GetSpec().GetTailnet()
	if tn == nil {
		return nil
	}
	return &hangarcompute.ContainerTailscale{AuthSecret: tailnetSecretName(object), Hostname: tn.GetHostname(), AcceptRoutes: tn.GetAcceptRoutes()}
}

// ensureTailnetKey writes the hangar Secret of a container that joins a
// tailnet once, with the key tower opens; a key tower does not have is
// left for tower to give later (JoinTailnet), hangar saying NoKey
// meanwhile.
func (t *Target) ensureTailnetKey(ctx context.Context, in *envv1alpha1.Instance, object string) error {
	tn := in.GetSpec().GetTailnet()
	if tn == nil || t.cfg.TailnetKeys == nil {
		return nil
	}
	name := tailnetSecretName(object)
	if _, ok := t.tailnetKeys.Load(name); ok {
		return nil
	}
	if _, err := t.api.Secrets().Get(ctx, name); err == nil {
		t.tailnetKeys.Store(name, true)
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	key, err := t.cfg.TailnetKeys(ctx, in.GetMetadata().GetProject(), tn.GetAuthSecret())
	if errors.Is(err, provider.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return t.writeTailnetKey(ctx, in, name, key)
}

// writeTailnetKey writes the key into the instance's hangar Secret, which
// carries its labels, so the sweep and its release take it with it.
func (t *Target) writeTailnetKey(ctx context.Context, in *envv1alpha1.Instance, name, key string) error {
	_, err := t.api.Secrets().Apply(ctx, &hangarsecrets.Secret{
		Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: t.labels(in)},
		Spec: &hangarsecrets.SecretSpec{
			Type: hangarsecrets.SecretType_SECRET_TYPE_TAILSCALE_AUTH,
			Data: map[string][]byte{provider.TailnetKeyName: []byte(key)},
		},
	})
	if err != nil {
		// hangar's words, which never quote a secret's value.
		return fmt.Errorf("writing the tailnet key of %s: %w", name, err)
	}
	t.tailnetKeys.Store(name, true)
	return nil
}

// dropTailnetKey deletes the key's Secret of a container that no longer
// joins a tailnet, once its Container no longer names it: hangar leaves
// the tailnet with the key still there to read.
func (t *Target) dropTailnetKey(ctx context.Context, object, secret string) error {
	ct, err := t.api.Containers().Get(ctx, object)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return err
	case ct.GetSpec().GetTailscale() != nil:
		// Read again at its next reconcile.
		return nil
	}
	if err := t.api.Secrets().Delete(ctx, secret); err != nil {
		return err
	}
	t.tailnetKeys.Delete(secret)
	return nil
}

// tailnetCondition is the instance's TailnetJoined, from its Container's,
// nil for one that joins no tailnet or does not run.
func tailnetCondition(in *envv1alpha1.Instance, ct *hangarcompute.Container, running bool) *metav1.Condition {
	if in.GetSpec().GetTailnet() == nil || ct == nil || !running {
		return nil
	}
	c := &metav1.Condition{Type: provider.InstanceTailnetJoined, Status: metav1.ConditionStatus_CONDITION_STATUS_FALSE}
	hc := conditionOf(ct.GetStatus().GetConditions(), provider.InstanceTailnetJoined)
	if hc == nil || hc.GetObservedGeneration() < ct.GetMetadata().GetGeneration() {
		c.Reason, c.Message = provider.TailnetStarting, "hangar has not said how its join went as its tailnet is now"
		return c
	}
	c.Reason, c.Message = hc.GetReason(), hc.GetMessage()
	if hc.GetStatus() == hangarmeta.ConditionStatus_CONDITION_STATUS_TRUE {
		c.Status = metav1.ConditionStatus_CONDITION_STATUS_TRUE
	}
	return c
}

// JoinTailnet writes the key tower gives into the hangar Secret the
// instance's Container joins with, for hangar's next join.
func (t *Target) JoinTailnet(ctx context.Context, req provider.TailnetJoin) error {
	o, err := t.c.Get(ctx, registry.Instance, client.Key{Project: req.Instance.Project, Name: req.Instance.Name})
	if connect.CodeOf(err) == connect.CodeNotFound {
		return fmt.Errorf("instance %s/%s: %w", req.Instance.Project, req.Instance.Name, provider.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("reading instance %s/%s: %w: %w", req.Instance.Project, req.Instance.Name, err, provider.ErrUnavailable)
	}
	in := o.(*envv1alpha1.Instance)
	if in.GetSpec().GetTarget() != t.cfg.Target {
		return fmt.Errorf("instance %s/%s is target %q's: %w", req.Instance.Project, req.Instance.Name, in.GetSpec().GetTarget(), provider.ErrNotFound)
	}
	if in.GetSpec().GetKind() != envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER || in.GetSpec().GetTailnet() == nil {
		return fmt.Errorf("instance %s/%s joins no tailnet: %w", req.Instance.Project, req.Instance.Name, provider.ErrFailed)
	}
	if err := t.writeTailnetKey(ctx, in, tailnetSecretName(objectName(in.GetMetadata().GetUid())), req.Key); err != nil {
		return err
	}
	t.ctrl.Enqueue(controller.Key{Project: req.Instance.Project, Name: req.Instance.Name})
	return nil
}
