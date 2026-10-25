package apiserver_test

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/devcontainer"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// realResolver is a world whose boxes are made of their repositories'
// devcontainers, read from the fake's mirrors, as production's are.
func realResolver() worldOption {
	return worldOption{config: func(c *apiserver.Config) { c.Resolver = devcontainer.Real{} }}
}

// setupRun records the setup steps a fake box runs, and fails those whose
// command fails names.
type setupRun struct {
	mu   sync.Mutex
	runs []string
}

func (s *setupRun) on(w *world, fails string) {
	w.fake.OnExec(func(req provider.ExecRequest) (int, bool) {
		if len(req.Command) != 3 || req.Command[0] != "bash" || req.Command[1] != "-lc" {
			return 0, false
		}
		s.mu.Lock()
		s.runs = append(s.runs, req.Dir+": "+req.Command[2])
		s.mu.Unlock()
		if fails != "" && req.Command[2] == fails {
			_, _ = io.WriteString(req.Stdout, "make: *** No rule to make target 'deps'.\n")
			return 2, true
		}
		return 0, true
	})
}

func (s *setupRun) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.runs)
}

// dockerfileIn is the Dockerfile of a build context.
func dockerfileIn(t *testing.T, ctx []byte) string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(ctx))
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("the context holds no Dockerfile: %v", err)
		}
		if h.Name == "Dockerfile" {
			b, _ := io.ReadAll(tr)
			return string(b)
		}
	}
}

// TestABoxIsMadeOfItsRepositorysDevcontainer: a box of a repository is
// made as workstream makes it of the repository's devcontainer: built
// FROM its image by digest, with its containerEnv and remoteUser and the
// add-ons asked for, and, checked out, it runs mise's toolchain, its
// onCreateCommand and its postCreateCommand in /workspace. A step that
// fails leaves the box READY, saying so.
func TestABoxIsMadeOfItsRepositorysDevcontainer(t *testing.T) {
	w := newWorld(t, realResolver())
	w.fake.SetRemote("https://git.example.com/api.git", "main", map[string]string{
		".devcontainer/devcontainer.json": `{
			"image": "mcr.microsoft.com/devcontainers/go:1",
			"containerEnv": {"GOFLAGS": "-mod=mod", "MULTI": "one\ntwo", "CTRL": "a\u0001b"},
			"remoteUser": "vscode",
			"onCreateCommand": "make tools",
			"postCreateCommand": "make deps",
		}`,
		"mise.toml": "[tools]\ngo = '1.26'\n",
	}, "c1")
	w.fake.SetImage("mcr.microsoft.com/devcontainers/go:1", provider.ImageConfig{Digest: "sha256:aa", User: "1000"})
	var setup setupRun
	setup.on(w, "")
	b := w.readyBox("dev", func(b *envv1alpha1.Box) { b.Spec.Addons = []string{"browser"} })

	g, _ := w.golden(b.GetStatus().GetImageGolden())
	in := g.GetSpec().GetBoxImage()
	if in.GetBaseImageDigest() != "sha256:aa" || in.GetDevcontainerHash() == "" || len(in.GetAddons()) != 1 || !strings.HasPrefix(in.GetAddons()[0], "browser@") {
		t.Errorf("the image's inputs %v", in)
	}
	df := dockerfileIn(t, w.fake.BuildContext(g.GetStatus().GetArtifact()))
	if !strings.HasPrefix(df, "FROM mcr.microsoft.com/devcontainers/go:1@sha256:aa\nUSER root\n") || !strings.Contains(df, "_REMOTE_USER='vscode'") ||
		!strings.HasSuffix(df, "USER 1000\nUSER vscode\n") {
		t.Errorf("the box's image was built with\n%s", df)
	}
	// Its containerEnv is its instance's environment, each value as written.
	inst, _ := w.instance(b.GetStatus().GetInstance())
	if want := map[string]string{"GOFLAGS": "-mod=mod", "MULTI": "one\ntwo", "CTRL": "a\x01b"}; !maps.Equal(inst.GetSpec().GetEnv(), want) {
		t.Errorf("the box's environment %q, want %q", inst.GetSpec().GetEnv(), want)
	}
	want := []string{
		"/workspace: command -v mise >/dev/null 2>&1 && mise trust && mise install || true",
		"/workspace: make tools",
		"/workspace: make deps",
	}
	if got := setup.got(); !slices.Equal(got, want) {
		t.Errorf("the box set up with %q, want %q", got, want)
	}
	if c := condition(b.GetStatus().GetConditions(), apiserver.BoxSetUp); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("SetUp %v", c)
	}

	// A second box of it shares the image, and sets itself up too.
	failing := &setupRun{}
	failing.on(w, "make deps")
	second := w.readyBox("second")
	if second.GetStatus().GetImageGolden() == b.GetStatus().GetImageGolden() {
		t.Error("a box without add-ons shares the image of one with them")
	}
	if got := failing.got(); !slices.Equal(got, want) {
		t.Errorf("the second box set up with %q", got)
	}
	c := condition(second.GetStatus().GetConditions(), apiserver.BoxSetUp)
	if c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetReason() != "SetupFailed" ||
		c.GetMessage() != "postCreate: exit 2: make: *** No rule to make target 'deps'." {
		t.Errorf("a box whose postCreate failed: SetUp %v", c)
	}
}

// TestAnImageIsBuiltFromTheCommitRead: a box checking out a branch has its
// image built from the commit its devcontainer was read at, not from the
// branch read again when the build runs.
func TestAnImageIsBuiltFromTheCommitRead(t *testing.T) {
	w := newWorld(t, realResolver())
	w.fake.SetRemote("https://git.example.com/api.git", "main", map[string]string{".devcontainer.json": `{"image": "node:22"}`}, "c1", "c2")
	b := w.readyBox("dev", func(b *envv1alpha1.Box) { b.Spec.Checkouts[0].Ref = "main" })
	o, err := w.store.Get(t.Context(), store.Key{Kind: "GoldenBuild", Project: "web", Name: b.GetStatus().GetImageGolden()})
	if err != nil {
		t.Fatal(err)
	}
	var build struct {
		Image struct{ Commit string }
	}
	if err := json.Unmarshal(o.Data, &build); err != nil || build.Image.Commit != "c2" {
		t.Errorf("the image's build reads the repository at %q, %v; want the commit c2", build.Image.Commit, err)
	}
}

// TestABoxWhoseDevcontainerCannotBeResolvedFails: a box of a repository
// with no devcontainer tower can make an image of, and no image to fall
// back to, fails at once, saying why; one given an explicit image is made
// of it and sets nothing up.
func TestABoxWhoseDevcontainerCannotBeResolvedFails(t *testing.T) {
	w := newWorld(t, realResolver())
	w.fake.SetRemote("https://git.example.com/api.git", "main", map[string]string{"README": "no devcontainer"}, "c1")
	w.mustApply(registry.Repository, repository("api"))
	w.mustApply(registry.Box, box("dev", "api"))
	failed := w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_FAILED)
	c := condition(failed.GetStatus().GetConditions(), apiserver.BoxReady)
	if c.GetReason() != "DevcontainerUnresolvable" || c.GetMessage() != "the repository has no .devcontainer/devcontainer.json or .devcontainer.json at c1, and there is no image to fall back to" {
		t.Errorf("Ready %v", c)
	}
	if len(w.fake.Builds()) != 0 {
		t.Errorf("an unresolvable box built %v", w.fake.Builds())
	}

	var setup setupRun
	setup.on(w, "")
	w.fake.SetImage("ubuntu:24.04", provider.ImageConfig{Digest: "sha256:dd"})
	plain := w.readyBox("plain", func(b *envv1alpha1.Box) { b.Spec.Image = "ubuntu:24.04" })
	g, _ := w.golden(plain.GetStatus().GetImageGolden())
	if df := dockerfileIn(t, w.fake.BuildContext(g.GetStatus().GetArtifact())); df != "FROM ubuntu:24.04@sha256:dd\n" {
		t.Errorf("a box of an explicit image was built with\n%s", df)
	}
	if got := setup.got(); len(got) != 0 {
		t.Errorf("a box of an explicit image set up with %q", got)
	}
}

// TestABoxOfARepositoryWithoutADevcontainerFallsBack: a box of a
// repository without a devcontainer is made of its fallback_image, else
// of tower's default box image, with its add-ons, as workstream makes a
// plain box of it; it sets nothing up.
func TestABoxOfARepositoryWithoutADevcontainerFallsBack(t *testing.T) {
	w := newWorld(t, worldOption{config: func(c *apiserver.Config) {
		c.Resolver, c.DefaultBoxImage = devcontainer.Real{}, "ghcr.io/workstream/box:1"
	}})
	w.fake.SetRemote("https://git.example.com/api.git", "main", map[string]string{"README": "no devcontainer"}, "c1")
	w.fake.SetImage("ubuntu:24.04", provider.ImageConfig{Digest: "sha256:dd"})
	w.fake.SetImage("ghcr.io/workstream/box:1", provider.ImageConfig{Digest: "sha256:bb"})
	var setup setupRun
	setup.on(w, "")
	builtFrom := func(b *envv1alpha1.Box) string {
		t.Helper()
		g, _ := w.golden(b.GetStatus().GetImageGolden())
		return strings.SplitN(dockerfileIn(t, w.fake.BuildContext(g.GetStatus().GetArtifact())), "\n", 2)[0]
	}
	own := w.readyBox("own", func(b *envv1alpha1.Box) { b.Spec.FallbackImage, b.Spec.Addons = "ubuntu:24.04", []string{"desktop"} })
	if from := builtFrom(own); from != "FROM ubuntu:24.04@sha256:dd" {
		t.Errorf("a box with a fallback_image was built %s", from)
	}
	defaulted := w.readyBox("defaulted")
	if from := builtFrom(defaulted); from != "FROM ghcr.io/workstream/box:1@sha256:bb" {
		t.Errorf("a box without one was built %s", from)
	}
	if got := setup.got(); len(got) != 0 {
		t.Errorf("a box fallen back set up with %q", got)
	}
}
