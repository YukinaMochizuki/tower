//go:build hangar

package hangar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarmeta "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	hangarnetwork "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/secrets/v1alpha1"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// labelled lists what the project holds labelled with target, as
// kind/name.
func labelled(ctx context.Context, api hangar.API, target string) ([]string, error) {
	sel := hangar.LabelTarget + "=" + target
	var out []string
	add := func(kind string, names []string, err error) error {
		for _, n := range names {
			out = append(out, kind+"/"+n)
		}
		return err
	}
	cts, err := api.Containers().List(ctx, sel)
	if err := add("container", names(cts), err); err != nil {
		return nil, err
	}
	disks, err := api.Disks().List(ctx, sel)
	if err := add("disk", names(disks), err); err != nil {
		return nil, err
	}
	nets, err := api.Networks().List(ctx, sel)
	if err := add("network", names(nets), err); err != nil {
		return nil, err
	}
	imgs, err := api.CustomImages().List(ctx, sel)
	if err := add("customimage", names(imgs), err); err != nil {
		return nil, err
	}
	secrets, err := api.Secrets().List(ctx, sel)
	if err := add("secret", names(secrets), err); err != nil {
		return nil, err
	}
	neps, err := api.NetworkEndpoints().List(ctx, sel)
	if err := add("networkendpoint", names(neps), err); err != nil {
		return nil, err
	}
	svcs, err := api.EndpointServices().List(ctx, sel)
	if err := add("endpointservice", names(svcs), err); err != nil {
		return nil, err
	}
	sgs, err := api.SecurityGroups().List(ctx, sel)
	if err := add("securitygroup", names(sgs), err); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

func names[T interface{ GetMetadata() *hangarmeta.ObjectMeta }](objs []T) []string {
	var out []string
	for _, o := range objs {
		out = append(out, o.GetMetadata().GetName())
	}
	return out
}

// TestDeletingATargetLeavesNothingInHangar runs a hangar target's
// provider as tower does, over a real hangar, with its system container,
// its endpoint service and security group, a network endpoint and a
// golden image, then deletes the Target: once it is gone, the project
// holds nothing labelled with it.
func TestDeletingATargetLeavesNothingInHangar(t *testing.T) {
	c := load(t)
	if c.system == "" {
		t.Skip("the run names no system image (TOWER_HANGAR_SYSTEM_IMAGE)")
	}
	api := hangar.NewConnect(c.endpoint, c.project, c.token, nil)
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	target := "l3-teardown-" + hex.EncodeToString(suffix)
	t.Cleanup(func() {
		// What a failed run left goes as a teardown takes it.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		p := hangar.New(hangar.Config{Target: target, API: api, Teardown: true})
		for ctx.Err() == nil {
			left, err := p.Teardown(ctx)
			if err == nil && len(left) == 0 {
				return
			}
			select {
			case <-ctx.Done():
				t.Logf("left in hangar after the test: %v, %v", left, err)
			case <-time.After(3 * time.Second):
			}
		}
	})

	env := apiservertest.Start(t, func(cfg *apiserver.Config) {
		cfg.Providers = nil
		cfg.HangarAPI = func(_, _, _ string) hangar.API { return api }
		cfg.SystemImage, cfg.Registry, cfg.RegistryPlainHTTP = c.system, c.registry, c.plainHTTP
	})
	env.Setup()
	ctx := t.Context()
	admin, sys := env.As(apiservertest.Admin), env.AsSystem()
	if _, err := admin.Apply(ctx, registry.Project, &tenancyv1alpha1.Project{Metadata: &metav1.ObjectMeta{Name: "system"}}); err != nil {
		t.Fatal(err)
	}
	// The provider is given the API above; the token here only has to be
	// there.
	secret := func(name string, data map[string][]byte) {
		t.Helper()
		if _, err := admin.Apply(ctx, registry.Secret, &secretsv1alpha1.Secret{
			Metadata: &metav1.ObjectMeta{Project: "system", Name: name}, Spec: &secretsv1alpha1.SecretSpec{Type: secretsv1alpha1.SecretType_SECRET_TYPE_OPAQUE, Data: data},
		}); err != nil {
			t.Fatal(err)
		}
	}
	secret("hangar-token", map[string][]byte{"token": []byte("given-to-the-test")})
	spec := &targetsv1alpha1.HangarTarget{Endpoint: "https://hangar.example", Project: c.project, CredentialSecret: "hangar-token", RegistryHost: c.registryHost}
	if len(c.pull) > 0 {
		secret("registry-pull", map[string][]byte{".dockerconfigjson": c.pull})
		spec.RegistryPullSecret = "registry-pull"
	}
	if _, err := admin.Apply(ctx, registry.Target, &targetsv1alpha1.Target{
		Metadata: &metav1.ObjectMeta{Name: target},
		Spec:     &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hangar{Hangar: spec}},
	}); err != nil {
		t.Fatal(err)
	}

	sysName := ""
	testutil.EventuallyWithin(t, 5*time.Minute, "the system container, its disk, network, endpoint service and security group in hangar", func() (bool, string) {
		got, err := labelled(ctx, api, target)
		if err != nil {
			return false, err.Error()
		}
		for _, s := range got {
			if n, ok := strings.CutPrefix(s, "container/"); ok {
				sysName = n
			}
		}
		return sysName != "" && slices.Contains(got, "disk/"+sysName) && slices.Contains(got, "network/"+sysName) &&
			slices.Contains(got, "endpointservice/"+sysName) && slices.Contains(got, "securitygroup/"+sysName), fmt.Sprint(got)
	})

	// A golden of the target's, as its provider captures one: an image of
	// a disk a container of the target's ran with, on a network of its.
	labels := map[string]string{hangar.LabelTarget: target}
	name := "t-" + target
	if _, err := api.Networks().Apply(ctx, &hangarnetwork.Network{
		Metadata: &hangarmeta.ObjectMeta{Name: "w-" + target, Labels: labels}, Spec: &hangarnetwork.NetworkSpec{Ipv4Cidr: hangar.NetworkCIDR},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Disks().Apply(ctx, &hangarstorage.Disk{
		Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: labels}, Spec: &hangarstorage.DiskSpec{Size: "1Gi"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := api.NetworkEndpoints().Apply(ctx, &hangarnetwork.NetworkEndpoint{
		Metadata: &hangarmeta.ObjectMeta{Name: "ep-" + target, Labels: labels}, Spec: &hangarnetwork.NetworkEndpointSpec{Network: "w-" + target, Service: sysName},
	}); err != nil {
		t.Fatal(err)
	}
	ctSpec := &hangarcompute.ContainerSpec{
		Image: c.image, Command: []string{"/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 86400 & wait $!; done"},
		RestartPolicy: hangarcompute.RestartPolicy_RESTART_POLICY_ALWAYS, RunState: hangarcompute.ContainerRunState_CONTAINER_RUN_STATE_RUNNING,
		Interfaces: []*hangarcompute.NetworkInterface{{Network: "w-" + target}},
		Volumes:    []*hangarcompute.ContainerVolume{{Type: "volume", Source: name, Target: hangar.WorkspaceDir}},
	}
	machinePhase := func(want hangarcompute.MachinePhase) {
		t.Helper()
		testutil.EventuallyWithin(t, 5*time.Minute, "the container's machine "+want.String(), func() (bool, string) {
			m, err := api.Machine(ctx, "ct-"+name)
			if err != nil {
				return false, err.Error()
			}
			return m.GetStatus().GetPhase() == want, m.GetStatus().String()
		})
	}
	if _, err := api.Containers().Apply(ctx, &hangarcompute.Container{Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: labels}, Spec: ctSpec}); err != nil {
		t.Fatal(err)
	}
	machinePhase(hangarcompute.MachinePhase_MACHINE_PHASE_RUNNING)
	ctSpec.RunState = hangarcompute.ContainerRunState_CONTAINER_RUN_STATE_STOPPED
	if _, err := api.Containers().Apply(ctx, &hangarcompute.Container{Metadata: &hangarmeta.ObjectMeta{Name: name, Labels: labels}, Spec: ctSpec}); err != nil {
		t.Fatal(err)
	}
	machinePhase(hangarcompute.MachinePhase_MACHINE_PHASE_STOPPED)
	if _, err := api.CustomImages().Apply(ctx, &hangarstorage.CustomImage{
		Metadata: &hangarmeta.ObjectMeta{Name: "g-" + target, Labels: labels},
		Spec: &hangarstorage.CustomImageSpec{Source: &hangarstorage.CustomImageSource{
			Source: &hangarstorage.CustomImageSource_Disk{Disk: &hangarstorage.CustomImageDiskSource{Name: name}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	testutil.EventuallyWithin(t, 5*time.Minute, "the golden image captured", func() (bool, string) {
		img, err := api.CustomImages().Get(ctx, "g-"+target)
		if err != nil {
			return false, err.Error()
		}
		for _, cond := range img.GetStatus().GetConditions() {
			if cond.GetType() == "Ready" && cond.GetStatus() == hangarmeta.ConditionStatus_CONDITION_STATUS_TRUE {
				return true, ""
			}
		}
		return false, img.GetStatus().String()
	})
	golden := client.Key{Project: apiservertest.Project, Name: "golden-" + target}
	if _, err := sys.Apply(ctx, registry.Golden, &envv1alpha1.Golden{
		Metadata: &metav1.ObjectMeta{Project: golden.Project, Name: golden.Name, Finalizers: []string{apiserver.GoldenArtifactFinalizer}},
		Spec: &envv1alpha1.GoldenSpec{Target: target, InputHash: target,
			Inputs: &envv1alpha1.GoldenSpec_BoxWarm{BoxWarm: &envv1alpha1.BoxWarmInputs{Repository: "r"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := sys.UpdateStatus(ctx, registry.Golden, golden, &envv1alpha1.GoldenStatus{
		Phase: envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY, Artifact: "g-" + target,
	}, nil); err != nil {
		t.Fatal(err)
	}
	before, err := labelled(ctx, api, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("before the delete, hangar holds %v", before)

	if _, err := admin.Delete(ctx, registry.Target, client.Key{Name: target}); err != nil {
		t.Fatal(err)
	}
	testutil.EventuallyWithin(t, 10*time.Minute, "the target gone", func() (bool, string) {
		o, err := sys.Get(ctx, registry.Target, client.Key{Name: target})
		if connect.CodeOf(err) == connect.CodeNotFound {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		st := o.(*targetsv1alpha1.Target).GetStatus()
		return false, fmt.Sprintf("remaining %v, last_error %q", st.GetRemaining(), st.GetLastError())
	})
	if got, err := labelled(ctx, api, target); err != nil || len(got) > 0 {
		t.Errorf("the target went, leaving %v in hangar (%v)", got, err)
	}
	if _, err := sys.Get(ctx, registry.Golden, golden); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("the target's golden: %v", err)
	}
}
