package apiserver_test

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/client"
)

func hangarTarget(name string) *targetsv1alpha1.Target {
	return &targetsv1alpha1.Target{
		Metadata: hostdTarget(name).GetMetadata(),
		Spec: &targetsv1alpha1.TargetSpec{Provider: &targetsv1alpha1.TargetSpec_Hangar{Hangar: &targetsv1alpha1.HangarTarget{
			Endpoint: "https://hangar.example.com", Project: "tower", CredentialSecret: "hangar-token",
		}}},
	}
}

// applyAs applies obj of kind r with c, and returns the error.
func applyAs(t *testing.T, c clients, r registry.Resource, obj proto.Message) error {
	t.Helper()
	_, err := c.generic.Apply(t.Context(), r, obj)
	return err
}

func TestTargetsAreChecked(t *testing.T) {
	e := newEnv(t)
	a := e.as(admin)
	if err := applyAs(t, a, registry.Target, hangarTarget("h")); err != nil {
		t.Fatal(err)
	}
	if err := applyAs(t, a, registry.Target, hostdTarget("d")); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*targetsv1alpha1.Target){
		"no provider":          func(t *targetsv1alpha1.Target) { t.Spec.Provider = nil },
		"an http endpoint":     func(t *targetsv1alpha1.Target) { t.Spec.GetHangar().Endpoint = "http://hangar.example.com" },
		"no endpoint":          func(t *targetsv1alpha1.Target) { t.Spec.GetHangar().Endpoint = "" },
		"no project":           func(t *targetsv1alpha1.Target) { t.Spec.GetHangar().Project = "" },
		"no credential secret": func(t *targetsv1alpha1.Target) { t.Spec.GetHangar().CredentialSecret = "" },
		"a budget that is no quantity": func(t *targetsv1alpha1.Target) {
			t.Spec.Settings = &targetsv1alpha1.TargetSettings{WarmBudget: "lots"}
		},
		"a negative idle time": func(t *targetsv1alpha1.Target) {
			t.Spec.Settings = &targetsv1alpha1.TargetSettings{IdleStopAfter: durationpb.New(-1)}
		},
		"negative commits behind": func(t *targetsv1alpha1.Target) {
			t.Spec.Settings = &targetsv1alpha1.TargetSettings{WarmMaxCommitsBehind: -1}
		},
	}
	for what, mutate := range bad {
		tg := hangarTarget("new")
		mutate(tg)
		if err := applyAs(t, a, registry.Target, tg); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("a target with %s: %v, want invalid", what, err)
		}
	}
	// A provider never changes, either way.
	hostd := hostdTarget("h")
	if err := applyAs(t, a, registry.Target, hostd); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a hangar target made a hostd one: %v, want invalid", err)
	}
	if err := applyAs(t, a, registry.Target, hangarTarget("d")); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a hostd target made a hangar one: %v, want invalid", err)
	}
	moved := hangarTarget("h")
	moved.Spec.GetHangar().Endpoint = "https://other.example.com"
	if err := applyAs(t, a, registry.Target, moved); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a hangar target moved to another endpoint: %v, want invalid", err)
	}
	set := hostdTarget("d")
	set.Spec.Settings = &targetsv1alpha1.TargetSettings{WarmBudget: "200Gi", IdleStopAfter: durationpb.New(3600e9)}
	if err := applyAs(t, a, registry.Target, set); err != nil {
		t.Errorf("settings on a target: %v", err)
	}
}

// TestAPlacementNamesAProjectAndATarget: a placement is named after an
// existing project and names an existing target.
func TestAPlacementNamesAProjectAndATarget(t *testing.T) {
	e := newEnv(t)
	e.setup()
	a := e.as(admin)
	if err := applyAs(t, a, registry.Placement, placement("web", "t1")); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a placement on a target that is not there: %v, want failed precondition", err)
	}
	if err := applyAs(t, a, registry.Target, hostdTarget("t1")); err != nil {
		t.Fatal(err)
	}
	if err := applyAs(t, a, registry.Placement, placement("ghost", "t1")); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a placement of a project that is not there: %v, want invalid", err)
	}
	if err := applyAs(t, a, registry.Placement, placement("web", "t1")); err != nil {
		t.Errorf("a placement: %v", err)
	}
}

// TestATargetInUseIsNotDeleted: deleting a target a Placement or an
// Instance names is refused, saying which, to everyone but the system.
func TestATargetInUseIsNotDeleted(t *testing.T) {
	e := newEnv(t)
	e.fixtures()
	ctx := t.Context()
	_, err := e.as(admin).generic.Delete(ctx, registry.Target, client.Key{Name: "t1"})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "placement web") || !strings.Contains(err.Error(), "instance web/in") {
		t.Errorf("deleting a target in use: %v, want refused naming placement web and instance web/in", err)
	}
	if _, err := e.as(admin).generic.Get(ctx, registry.Target, client.Key{Name: "t1"}); err != nil {
		t.Errorf("the target after a refused delete: %v", err)
	}
	if _, err := e.asSystem().generic.Delete(ctx, registry.Target, client.Key{Name: "t1"}); err != nil {
		t.Errorf("the system deleting a target in use: %v", err)
	}
}

func TestRepositoriesAreChecked(t *testing.T) {
	e := newEnv(t)
	e.setup()
	a := e.as(admin)
	if err := applyAs(t, a, registry.Repository, repository("ok")); err != nil {
		t.Fatal(err)
	}
	for what, url := range map[string]string{
		"an http URL": "http://git.example.com/r.git", "an ssh URL": "git@git.example.com:r.git",
		"no URL": "", "credentials in the URL": "https://user:pass@git.example.com/r.git",
	} {
		r := repository("bad")
		r.Spec.Url = url
		err := applyAs(t, a, registry.Repository, r)
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("a repository with %s: %v, want invalid", what, err)
		}
		if err != nil && strings.Contains(err.Error(), "pass") {
			t.Errorf("the refusal quotes the URL's password: %v", err)
		}
	}
}

// TestABoxNamesWhatItIsMadeOf: a box's checkouts are repositories of its
// project, its privileged secret a secret of it, an empty box names its
// image, and its quantities parse.
func TestABoxNamesWhatItIsMadeOf(t *testing.T) {
	e := newEnv(t)
	e.fixtures()
	w := e.as(workstream)
	if err := applyAs(t, w, registry.Box, box("ok", "repo")); err != nil {
		t.Fatal(err)
	}
	empty := box("empty")
	empty.Spec.Image = "ghcr.io/acme/dev:1"
	if err := applyAs(t, w, registry.Box, empty); err != nil {
		t.Errorf("an empty box with an image: %v", err)
	}
	for what, tc := range map[string]struct {
		b    *envv1alpha1.Box
		code connect.Code
	}{
		"a repository that is not there": {box("b", "ghost"), connect.CodeFailedPrecondition},
		"a repository twice":             {box("b", "repo", "repo"), connect.CodeInvalidArgument},
		"no checkout and no image":       {box("b"), connect.CodeInvalidArgument},
		"no workstream": {func() *envv1alpha1.Box {
			b := box("b", "repo")
			b.Spec.Workstream = ""
			return b
		}(), connect.CodeInvalidArgument},
		"a privileged secret that is not there": {func() *envv1alpha1.Box {
			b := box("b", "repo")
			b.Spec.PrivilegedSecret = "ghost"
			return b
		}(), connect.CodeFailedPrecondition},
		"memory that is no quantity": {func() *envv1alpha1.Box {
			b := box("b", "repo")
			b.Spec.Resources = &envv1alpha1.Resources{Memory: "much"}
			return b
		}(), connect.CodeInvalidArgument},
		"a cpu that is no quantity": {func() *envv1alpha1.Box {
			b := box("b", "repo")
			b.Spec.Resources = &envv1alpha1.Resources{Cpu: "x"}
			return b
		}(), connect.CodeInvalidArgument},
	} {
		if err := applyAs(t, w, registry.Box, tc.b); connect.CodeOf(err) != tc.code {
			t.Errorf("a box with %s: %v, want %v", what, err, tc.code)
		}
	}
	privileged := box("priv", "repo")
	privileged.Spec.PrivilegedSecret = "s"
	privileged.Spec.Resources = &envv1alpha1.Resources{Cpu: "2", Memory: "4Gi", Disk: "40Gi"}
	if err := applyAs(t, w, registry.Box, privileged); err != nil {
		t.Errorf("a privileged box: %v", err)
	}
}

func TestAMachineNamesItsSecrets(t *testing.T) {
	e := newEnv(t)
	e.fixtures()
	w := e.as(workstream)
	ok := machine("ok")
	ok.Spec.Secrets = []*envv1alpha1.SecretRef{{Secret: "s", Key: "A", As: "K3S_TOKEN"}}
	if err := applyAs(t, w, registry.Machine, ok); err != nil {
		t.Fatal(err)
	}
	for what, tc := range map[string]struct {
		mutate func(*envv1alpha1.MachineSpec)
		code   connect.Code
	}{
		"a secret that is not there": {func(s *envv1alpha1.MachineSpec) {
			s.Secrets = []*envv1alpha1.SecretRef{{Secret: "ghost", Key: "A", As: "X"}}
		}, connect.CodeFailedPrecondition},
		"a secret given as no variable": {func(s *envv1alpha1.MachineSpec) {
			s.Secrets = []*envv1alpha1.SecretRef{{Secret: "s", Key: "A", As: "1-x"}}
		}, connect.CodeInvalidArgument},
		"no blueprint content": {func(s *envv1alpha1.MachineSpec) { s.Blueprint.Content = "" }, connect.CodeInvalidArgument},
		"no base image":        {func(s *envv1alpha1.MachineSpec) { s.Blueprint.BaseImage = "" }, connect.CodeInvalidArgument},
		"a disk that is no quantity": {func(s *envv1alpha1.MachineSpec) {
			s.Resources = &envv1alpha1.Resources{Disk: "big"}
		}, connect.CodeInvalidArgument},
	} {
		m := machine("m")
		tc.mutate(m.Spec)
		if err := applyAs(t, w, registry.Machine, m); connect.CodeOf(err) != tc.code {
			t.Errorf("a machine with %s: %v, want %v", what, err, tc.code)
		}
	}
}
