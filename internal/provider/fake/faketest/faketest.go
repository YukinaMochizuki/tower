// Package faketest runs an apiserver with a hostd target realized by a
// fake provider, and the providertest harness over it, for the tests of
// the fake and of what carries a provider elsewhere, such as the hostd
// client over a tunnel.
package faketest

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/fake"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// Target is the world's hostd target.
const Target = "t1"

// Remote is a repository the world's fake holds, on branch main at c3.
const Remote = "https://git.example.com/conformance.git"

// World is an apiserver with the hostd target Target, realized by Fake.
type World struct {
	Env  *apiservertest.Env
	Fake *fake.Fake
}

// New starts a world until the test ends; configure may adjust the
// apiserver's config.
func New(t *testing.T, configure func(*apiserver.Config)) World {
	t.Helper()
	env := apiservertest.Start(t, configure)
	env.Setup()
	if _, err := env.As(apiservertest.Admin).Apply(t.Context(), registry.Target, &targetsv1alpha1.Target{
		Metadata: &metav1.ObjectMeta{Name: Target},
		Spec:     &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hostd{Hostd: &targetsv1alpha1.HostdTarget{}}},
	}); err != nil {
		t.Fatal(err)
	}
	hostd := env.AsToken("hostd-"+Target, iamv1alpha1.Role_ROLE_HOSTD, Target)
	f := fake.New(Target, clockwork.NewRealClock())
	f.SetRemote(Remote, "main", map[string]string{"go.mod": "module x\n"}, "c1", "c2", "c3")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, hostd) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return World{Env: env, Fake: f}
}

// Instance reads an instance of the world's project.
func (w World) Instance(t *testing.T, name string) *envv1alpha1.Instance {
	t.Helper()
	o, err := w.Env.AsSystem().Get(t.Context(), registry.Instance, client.Key{Project: apiservertest.Project, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return o.(*envv1alpha1.Instance)
}

// WaitPhase waits until the fake reports the instance in phase.
func (w World) WaitPhase(t *testing.T, name string, phase envv1alpha1.InstancePhase) *envv1alpha1.Instance {
	t.Helper()
	var in *envv1alpha1.Instance
	testutil.Eventually(t, fmt.Sprintf("instance %s %v", name, phase), func() (bool, string) {
		in = w.Instance(t, name)
		return in.GetStatus().GetPhase() == phase, in.GetStatus().String()
	})
	return in
}

// Harness is the providertest harness of the world, through p, which
// carries the fake's work some way or is the fake itself.
func (w World) Harness(p provider.Provider) providertest.Harness {
	return providertest.Harness{
		Provider: p, Target: Target, Image: "registry.tower.internal/base@sha256:00", BaseImage: "ubuntu-24.04",
		Start: func(t *testing.T, name string, spec *envv1alpha1.InstanceSpec) (provider.Instance, *envv1alpha1.InstanceStatus) {
			t.Helper()
			if _, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, &envv1alpha1.Instance{
				Metadata: &metav1.ObjectMeta{Project: apiservertest.Project, Name: name}, Spec: spec,
			}); err != nil {
				t.Fatal(err)
			}
			in := w.WaitPhase(t, name, envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
			return provider.Instance{Project: apiservertest.Project, Name: name}, in.GetStatus()
		},
		Stop: func(t *testing.T, ref provider.Instance) {
			t.Helper()
			// The fake writes the instance's status as it goes, which can
			// move it between the read and the apply.
			testutil.Eventually(t, "the instance's run_state set STOPPED", func() (bool, string) {
				in := proto.Clone(w.Instance(t, ref.Name)).(*envv1alpha1.Instance)
				in.Spec.RunState = envv1alpha1.RunState_RUN_STATE_STOPPED
				_, err := w.Env.AsSystem().Apply(t.Context(), registry.Instance, in)
				if err != nil && connect.CodeOf(err) != connect.CodeAborted {
					t.Fatal(err)
				}
				return err == nil, fmt.Sprint(err)
			})
			w.WaitPhase(t, ref.Name, envv1alpha1.InstancePhase_INSTANCE_PHASE_STOPPED)
		},
		Golden: func(t *testing.T, art provider.Artifact) string {
			t.Helper()
			key := client.Key{Project: apiservertest.Project, Name: "golden-" + fmt.Sprint(len(art.Name))}
			if _, err := w.Env.AsSystem().Apply(t.Context(), registry.Golden, &envv1alpha1.Golden{
				Metadata: &metav1.ObjectMeta{Project: key.Project, Name: key.Name},
				Spec: &envv1alpha1.GoldenSpec{Target: Target, InputHash: art.Name,
					Inputs: &envv1alpha1.GoldenSpec_BoxWarm{BoxWarm: &envv1alpha1.BoxWarmInputs{Repository: "r"}}},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Env.AsSystem().UpdateStatus(t.Context(), registry.Golden, key, &envv1alpha1.GoldenStatus{
				Phase: envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY, Artifact: art.Name,
			}, nil); err != nil {
				t.Fatal(err)
			}
			return key.Name
		},
		Remote: Remote, Branch: "main", RemoteHead: "c3",
	}
}
