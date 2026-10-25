package devcontainer_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/jonboulle/clockwork"

	"github.com/YukinaMochizuki/tower/internal/devcontainer"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/fake"
)

// repo is a provider holding a mirror of one repository of files.
func repo(t *testing.T, files map[string]string) (*fake.Fake, devcontainer.Request) {
	t.Helper()
	f := fake.New("t1", clockwork.NewRealClock())
	f.SetRemote("https://git.example.com/app.git", "main", files, "c1")
	if _, err := f.Fetch(t.Context(), provider.FetchRequest{Mirror: "web/app", URL: "https://git.example.com/app.git", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	return f, devcontainer.Request{Target: "t1", Mirror: "web/app", Commit: "c1"}
}

// dockerfileOf is the Dockerfile an image was built with.
func dockerfileOf(t *testing.T, f *fake.Fake, image string) string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(f.BuildContext(image)))
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("%s's context holds no Dockerfile: %v", image, err)
		}
		if h.Name == "Dockerfile" {
			b, _ := io.ReadAll(tr)
			return string(b)
		}
	}
}

func TestAnImageDevcontainer(t *testing.T) {
	const config = `{
		// workstream's own devcontainer is JSONC
		"image": "mcr.microsoft.com/devcontainers/go:1",
		"containerEnv": {"GOFLAGS": "-mod=mod", "WS": "${containerWorkspaceFolder}"},
		"remoteUser": "vscode",
		"onCreateCommand": "make tools",
		"postCreateCommand": ["make", "deps"],
	}`
	f, req := repo(t, map[string]string{".devcontainer/devcontainer.json": config, "mise.toml": "[tools]\ngo = '1.26'\n", "README": "a"})
	f.SetImage("mcr.microsoft.com/devcontainers/go:1", provider.ImageConfig{Digest: "sha256:aa"})
	r, ctx := devcontainer.Real{}, t.Context()
	img, err := r.Image(ctx, f, req)
	if err != nil {
		t.Fatal(err)
	}
	if img.BaseImageDigest != "sha256:aa" || img.DevcontainerHash == "" || img.BuildHash != "" {
		t.Errorf("resolved to %+v", img)
	}
	// What the devcontainer is does not change with a file it does not
	// read; it does with its devcontainer.json.
	other, oreq := repo(t, map[string]string{".devcontainer/devcontainer.json": config, "mise.toml": "[tools]\ngo = '1.26'\n", "README": "b"})
	other.SetImage("mcr.microsoft.com/devcontainers/go:1", provider.ImageConfig{Digest: "sha256:aa"})
	if again, _ := r.Image(ctx, other, oreq); again.DevcontainerHash != img.DevcontainerHash {
		t.Error("a file the devcontainer does not read changed its hash")
	}
	changed, creq := repo(t, map[string]string{".devcontainer/devcontainer.json": strings.Replace(config, "-mod=mod", "-mod=vendor", 1)})
	changed.SetImage("mcr.microsoft.com/devcontainers/go:1", provider.ImageConfig{Digest: "sha256:aa"})
	if moved, _ := r.Image(ctx, changed, creq); moved.DevcontainerHash == img.DevcontainerHash {
		t.Error("a changed devcontainer.json kept its hash")
	}

	// Built FROM the image by the digest resolved, its containerEnv and
	// its remote user baked in.
	ref, err := r.Build(ctx, f, req, img, "web/box")
	if err != nil {
		t.Fatal(err)
	}
	want := "FROM mcr.microsoft.com/devcontainers/go:1@sha256:aa\nENV GOFLAGS=\"-mod=mod\"\nENV WS=\"/workspace\"\nUSER vscode\n"
	if df := dockerfileOf(t, f, ref); df != want {
		t.Errorf("built with\n%s\nwant\n%s", df, want)
	}
	// The base its golden was made for, should its tag have moved since.
	img.BaseImageDigest = "sha256:bb"
	pinned, err := r.Build(ctx, f, req, img, "web/box")
	if err != nil {
		t.Fatal(err)
	}
	if df := dockerfileOf(t, f, pinned); !strings.HasPrefix(df, "FROM mcr.microsoft.com/devcontainers/go:1@sha256:bb\n") {
		t.Errorf("built with\n%s", df)
	}

	// Each box of it runs mise's toolchain, then onCreate, then
	// postCreate, as workstream does.
	steps, err := r.Setup(ctx, f, req)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range steps {
		names = append(names, s.Name+": "+strings.Join(s.Command, " "))
	}
	if !slices.Equal(names, []string{
		"mise install: bash -lc command -v mise >/dev/null 2>&1 && mise trust && mise install || true",
		"onCreate: bash -lc make tools",
		"postCreate: bash -lc make deps",
	}) {
		t.Errorf("setup %q", names)
	}
}

func TestADockerfileDevcontainer(t *testing.T) {
	files := map[string]string{
		".devcontainer.json": `{"build": {"dockerfile": "docker/Dockerfile", "context": "src"}}`,
		"docker/Dockerfile":  "FROM debian:13\nCOPY . /src\n",
		"src/main.go":        "package main\n",
		"README":             "a",
	}
	f, req := repo(t, files)
	r, ctx := devcontainer.Real{}, t.Context()
	img, err := r.Image(ctx, f, req)
	if err != nil {
		t.Fatal(err)
	}
	if img.BaseImageDigest != "" || img.DevcontainerHash == "" {
		t.Errorf("resolved to %+v", img)
	}
	// A change in its context is another image; one outside it is not.
	hashWith := func(name, content string) string {
		t.Helper()
		changed := map[string]string{}
		for k, v := range files {
			changed[k] = v
		}
		changed[name] = content
		g, greq := repo(t, changed)
		i, err := r.Image(ctx, g, greq)
		if err != nil {
			t.Fatal(err)
		}
		return i.DevcontainerHash
	}
	if hashWith("README", "b") != img.DevcontainerHash {
		t.Error("a file outside the build's context changed its hash")
	}
	if hashWith("src/main.go", "package other\n") == img.DevcontainerHash || hashWith("docker/Dockerfile", "FROM debian:12\n") == img.DevcontainerHash {
		t.Error("a change to the build's context or Dockerfile kept its hash")
	}
	// Built from the mirror on the target, then a layer on it.
	if _, err := r.Build(ctx, f, req, img, "web/box"); err != nil {
		t.Fatal(err)
	}
	builds := f.Builds()
	if len(builds) != 2 || builds[0].Mirror == nil || builds[0].Mirror.Mirror != "web/app" || builds[0].Mirror.Ref != "c1" ||
		builds[0].Dir != "src" || builds[0].Dockerfile != "docker/Dockerfile" || builds[1].Mirror != nil {
		t.Errorf("builds %+v", builds)
	}
	if df := dockerfileOf(t, f, builds[1].Image); df != "FROM "+builds[0].Image+"\n" {
		t.Errorf("the layer is\n%s", df)
	}
	if steps, err := r.Setup(ctx, f, req); err != nil || len(steps) != 0 {
		t.Errorf("a devcontainer of no commands sets up %v, %v", steps, err)
	}
}

func TestAddonsOnAnyImage(t *testing.T) {
	f, req := repo(t, map[string]string{".devcontainer/devcontainer.json": `{"image": "node:22", "remoteUser": "node"}`})
	f.SetImage("node:22", provider.ImageConfig{Digest: "sha256:cc", User: "1000"})
	r, ctx := devcontainer.Real{}, t.Context()
	req.Addons = []string{"desktop", "browser"}
	img, err := r.Image(ctx, f, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(img.Addons) != 2 || !strings.HasPrefix(img.Addons[0], "desktop@") || !strings.HasPrefix(img.Addons[1], "browser@") {
		t.Errorf("add-ons %v, want desktop then browser, with their versions", img.Addons)
	}
	ref, err := r.Build(ctx, f, req, img, "web/box")
	if err != nil {
		t.Fatal(err)
	}
	df := dockerfileOf(t, f, ref)
	// Installed for the remote user, the base's user restored after them,
	// the remote user's at the end.
	if !strings.Contains(df, "_REMOTE_USER='node'") || !strings.Contains(df, "\nUSER 1000\nUSER node\n") || strings.Index(df, "COPY desktop/") > strings.Index(df, "COPY browser/") {
		t.Errorf("built with\n%s", df)
	}
	if !bytes.Contains(f.BuildContext(ref), []byte("set -euo pipefail")) {
		t.Error("the add-ons' files are not in the layer's context")
	}

	// An explicit image and a box's own build take add-ons too, and no
	// devcontainer, nor its setup.
	f.SetImage("ubuntu:24.04", provider.ImageConfig{Digest: "sha256:dd"})
	plain := devcontainer.Request{Target: "t1", Image: "ubuntu:24.04", Addons: []string{"browser"}}
	pimg, err := r.Image(ctx, f, plain)
	if err != nil || pimg.BaseImageDigest != "sha256:dd" || pimg.DevcontainerHash != "" {
		t.Fatalf("an explicit image resolved to %+v, %v", pimg, err)
	}
	if ref, err := r.Build(ctx, f, plain, pimg, "web/box"); err != nil || !strings.HasPrefix(dockerfileOf(t, f, ref), "FROM ubuntu:24.04@sha256:dd\nUSER root\n") {
		t.Errorf("an explicit image built %q, %v", ref, err)
	}
	own := devcontainer.Request{Target: "t1", Build: &devcontainer.Build{Dockerfile: "FROM ubuntu:24.04\nUSER 7\n"}}
	oimg, err := r.Image(ctx, f, own)
	if err != nil || oimg.BuildHash == "" {
		t.Fatalf("a box's own build resolved to %+v, %v", oimg, err)
	}
	if ref, err := r.Build(ctx, f, own, oimg, "web/box"); err != nil || f.BuildContext(ref) == nil {
		t.Errorf("a box's own build without add-ons built %q, %v", ref, err)
	}
	own.Addons = []string{"desktop"}
	oimg, _ = r.Image(ctx, f, own)
	ref, err = r.Build(ctx, f, own, oimg, "web/box")
	if err != nil || !strings.HasSuffix(dockerfileOf(t, f, ref), "USER 7\n") {
		t.Errorf("a box's own build with an add-on built %q, %v", ref, err)
	}
	for _, q := range []devcontainer.Request{plain, own} {
		if steps, err := r.Setup(ctx, f, q); err != nil || steps != nil {
			t.Errorf("%+v sets up %v, %v", q, steps, err)
		}
	}
}

func TestWhatCannotBeResolved(t *testing.T) {
	r, ctx := devcontainer.Real{}, t.Context()
	for what, tc := range map[string]struct {
		files map[string]string
		image string
		addon string
		says  string
	}{
		"no devcontainer":       {files: map[string]string{"README": "x"}, says: "has no .devcontainer/devcontainer.json or .devcontainer.json at c1; give the box an image"},
		"one that never parses": {files: map[string]string{".devcontainer.json": `{"image": `}, says: ".devcontainer.json does not parse"},
		"neither image nor build": {files: map[string]string{".devcontainer.json": `{"name": "x"}`},
			says: ".devcontainer.json declares neither image nor build.dockerfile"},
		"an image not in its registry": {files: map[string]string{".devcontainer.json": `{"image": "gone:1"}`},
			says: ".devcontainer.json names the image gone:1, which is not in its registry"},
		"a Dockerfile not there": {files: map[string]string{".devcontainer/devcontainer.json": `{"build": {"dockerfile": "Dockerfile"}}`},
			says: "builds from .devcontainer/Dockerfile, which the repository does not hold"},
		"a build from outside": {files: map[string]string{".devcontainer/devcontainer.json": `{"build": {"dockerfile": "../../Dockerfile"}}`},
			says: "builds from outside the repository"},
		"a context not there": {files: map[string]string{".devcontainer/devcontainer.json": `{"build": {"dockerfile": "Dockerfile", "context": "nowhere"}}`, ".devcontainer/Dockerfile": "FROM x\n"},
			says: "builds in .devcontainer/nowhere, which is no directory of the repository"},
		"an explicit image not there": {files: map[string]string{}, image: "gone:1", says: "the image gone:1 is not in its registry"},
		"an add-on tower lacks":       {files: map[string]string{}, image: "ubuntu:24.04", addon: "gpu", says: `tower has no add-on "gpu"; it has browser, desktop`},
	} {
		f, req := repo(t, tc.files)
		f.SetMissingImage("gone:1")
		if tc.image != "" {
			req = devcontainer.Request{Target: "t1", Image: tc.image}
		}
		if tc.addon != "" {
			req.Addons = []string{tc.addon}
		}
		_, err := r.Image(ctx, f, req)
		if !errors.Is(err, devcontainer.ErrUnresolvable) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %v, want it unresolvable, saying %q", what, err, tc.says)
		}
	}
	// A target that does not answer is no reason to give up.
	f, req := repo(t, map[string]string{".devcontainer.json": `{"image": "x"}`})
	f.SetUnavailable(true)
	if _, err := r.Image(ctx, f, req); errors.Is(err, devcontainer.ErrUnresolvable) || !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("a target that does not answer: %v", err)
	}
}

func TestAWarmCheckoutDependsOnItsToolchainAndLockfiles(t *testing.T) {
	r, ctx := devcontainer.Real{}, t.Context()
	warm := func(files map[string]string) devcontainer.Warm {
		t.Helper()
		f, req := repo(t, files)
		w, err := r.Warm(ctx, f, req.Mirror, req.Commit)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	base := warm(map[string]string{"go.sum": "a", "mise.toml": "x", "main.go": "1"})
	if len(base.Install) != 0 {
		t.Errorf("a warm checkout installs %v; every box sets itself up", base.Install)
	}
	if w := warm(map[string]string{"go.sum": "a", "mise.toml": "x", "main.go": "2"}); w.ToolchainHash != base.ToolchainHash || w.LockfilesHash != base.LockfilesHash {
		t.Error("a change to neither the toolchain nor a lockfile changed what a warm checkout depends on")
	}
	if w := warm(map[string]string{"go.sum": "b", "mise.toml": "x"}); w.LockfilesHash == base.LockfilesHash || w.ToolchainHash != base.ToolchainHash {
		t.Error("a lockfile's change is not told from the toolchain's")
	}
	if w := warm(map[string]string{"go.sum": "a", "mise.toml": "y"}); w.ToolchainHash == base.ToolchainHash || w.LockfilesHash != base.LockfilesHash {
		t.Error("the toolchain's change is not told from a lockfile's")
	}
}
