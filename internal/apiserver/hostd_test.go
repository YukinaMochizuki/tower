package apiserver_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	hostdpkg "github.com/YukinaMochizuki/tower/internal/hostd"
	"github.com/YukinaMochizuki/tower/internal/hostd/hostdtest"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// TestHostdConformanceThroughTower holds tower's own provider of a hostd
// target, a real hostd over the fake host behind the tunnel it opens to
// this apiserver, to the provider contract: the seam a host's real drivers
// plug into.
func TestHostdConformanceThroughTower(t *testing.T) {
	t.Parallel()
	w := hostdtest.New(t, hostdtest.Options{})
	w.Tunnel(t)
	p, err := w.Env.Server.ProviderFor(hostdtest.Target)
	if err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the host answering over its tunnel", func() bool {
		// No such mirror, says the host once it answers.
		_, err := p.CommitsBehind(t.Context(), "never-fetched", "c1", "main")
		return errors.Is(err, provider.ErrNotFound)
	})
	providertest.Run(t, w.Harness(p))
}

// TestAHostdBoxJoinsItsTailnetThroughTheTunnel: a container instance with
// tailnet on a hostd target says NoKey until tower gives the key over the
// tunnel, then joins, once: a key given again to a node with state is not
// used. hostd keeps the key nowhere, its host's state file and its log
// included.
func TestAHostdBoxJoinsItsTailnetThroughTheTunnel(t *testing.T) {
	t.Parallel()
	const key = "tskey-auth-kHostd-Nr31"
	var logs syncBuffer
	state := filepath.Join(t.TempDir(), "host.json")
	w := hostdtest.New(t, hostdtest.Options{StatePath: state, Host: func(c *hostdpkg.Config) {
		c.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}})
	w.Tunnel(t)
	p, err := w.Env.Server.ProviderFor(hostdtest.Target)
	if err != nil {
		t.Fatal(err)
	}
	w.Apply(t, "box-dev", &envv1alpha1.InstanceSpec{
		Target: hostdtest.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, Image: hostdtest.Image,
		Tailnet: &envv1alpha1.Tailnet{AuthSecret: "ts", Hostname: "dev-box"},
	})
	joined := func(reason string) {
		t.Helper()
		testutil.Eventually(t, "TailnetJoined "+reason, func() (bool, string) {
			c := conditions.Find(w.Instance(t, "box-dev").GetStatus().GetConditions(), provider.InstanceTailnetJoined)
			return c.GetReason() == reason, c.String()
		})
	}
	joined(provider.TailnetNoKey)
	ref := provider.Instance{Project: apiservertest.Project, Name: "box-dev"}
	testutil.Eventually(t, "the key given over the tunnel", func() (bool, string) {
		err := p.JoinTailnet(t.Context(), provider.TailnetJoin{Instance: ref, Key: key})
		return err == nil, errString(err)
	})
	joined(provider.TailnetJoined)
	if err := p.JoinTailnet(t.Context(), provider.TailnetJoin{Instance: ref, Key: "tskey-rejected-again"}); err != nil {
		t.Fatal(err)
	}
	joined(provider.TailnetJoined)
	guest := "tower-" + w.Instance(t, "box-dev").GetMetadata().GetUid()
	if n := w.Fake.TailnetJoins(guest); n != 1 {
		t.Errorf("the container joined %d times", n)
	}
	// One that joins no tailnet is refused the key.
	w.Apply(t, "plain", &envv1alpha1.InstanceSpec{Target: hostdtest.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, Image: hostdtest.Image})
	w.WaitPhase(t, "plain", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	err = p.JoinTailnet(t.Context(), provider.TailnetJoin{Instance: provider.Instance{Project: apiservertest.Project, Name: "plain"}, Key: key})
	if !errors.Is(err, provider.ErrFailed) {
		t.Errorf("a key given to an instance without tailnet: %v", err)
	}
	if b, _ := os.ReadFile(state); bytes.Contains(b, []byte(key)) {
		t.Error("the host's state holds the key")
	}
	if bytes.Contains([]byte(logs.String()), []byte(key)) {
		t.Error("hostd's log holds the key")
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestAHostdBoxsTailnetComesAndGoes: a tailnet added to a running hostd
// container makes it again with its device and starts it, saying NoKey; a
// new hostname is set on its node as it runs; removed, it is made again
// without, its TailnetJoined gone.
func TestAHostdBoxsTailnetComesAndGoes(t *testing.T) {
	t.Parallel()
	w := hostdtest.New(t, hostdtest.Options{})
	w.Tunnel(t)
	p, err := w.Env.Server.ProviderFor(hostdtest.Target)
	if err != nil {
		t.Fatal(err)
	}
	apply := func(tn *envv1alpha1.Tailnet) {
		t.Helper()
		w.Apply(t, "box-dev", &envv1alpha1.InstanceSpec{Target: hostdtest.Target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_CONTAINER, Image: hostdtest.Image, Tailnet: tn})
	}
	cond := func() *metav1.Condition {
		return conditions.Find(w.Instance(t, "box-dev").GetStatus().GetConditions(), provider.InstanceTailnetJoined)
	}
	apply(nil)
	guest := "tower-" + w.WaitPhase(t, "box-dev", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING).GetMetadata().GetUid()

	apply(&envv1alpha1.Tailnet{AuthSecret: "ts", Hostname: "dev-box"})
	testutil.Eventually(t, "TailnetJoined NoKey", func() (bool, string) { return cond().GetReason() == provider.TailnetNoKey, cond().String() })
	if _, remade := w.Fake.TailnetSets(guest); remade != 1 {
		t.Errorf("its container was made again %d times for its tailnet", remade)
	}
	ref := provider.Instance{Project: apiservertest.Project, Name: "box-dev"}
	testutil.Eventually(t, "the key given", func() (bool, string) {
		err := p.JoinTailnet(t.Context(), provider.TailnetJoin{Instance: ref, Key: "tskey-auth-kLiveHostd"})
		return err == nil, errString(err)
	})
	testutil.Eventually(t, "TailnetJoined Joined", func() (bool, string) { return cond().GetReason() == provider.TailnetJoined, cond().String() })

	apply(&envv1alpha1.Tailnet{AuthSecret: "ts", Hostname: "dev-renamed"})
	testutil.Eventually(t, "the hostname set as it runs", func() bool {
		sets, remade := w.Fake.TailnetSets(guest)
		return sets == 1 && remade == 1
	})

	apply(nil)
	testutil.Eventually(t, "TailnetJoined going", func() (bool, string) { return cond() == nil, cond().String() })
	w.WaitPhase(t, "box-dev", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	if _, remade := w.Fake.TailnetSets(guest); remade != 2 {
		t.Errorf("its container was made again %d times for its tailnet", remade)
	}
}
