package devcontainer_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
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
	want := "FROM mcr.microsoft.com/devcontainers/go:1@sha256:aa\nUSER vscode\n"
	if df := dockerfileOf(t, f, ref); df != want {
		t.Errorf("built with\n%s\nwant\n%s", df, want)
	}
	// Its containerEnv is the box's environment, not the image's.
	if want := map[string]string{"GOFLAGS": "-mod=mod", "WS": "/workspace"}; !maps.Equal(img.Env, want) {
		t.Errorf("the box's environment %v, want %v", img.Env, want)
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

// TestARepositoryWithoutADevcontainerFallsBack: a repository without a
// devcontainer is made of the image to fall back to, as workstream's
// setup "auto" makes a plain box of it, which sets nothing up; one with a
// devcontainer is made of that, whatever the fallback.
func TestARepositoryWithoutADevcontainerFallsBack(t *testing.T) {
	r, ctx := devcontainer.Real{}, t.Context()
	f, req := repo(t, map[string]string{"README": "plain", "mise.toml": "[tools]\ngo = '1.26'\n"})
	f.SetImage("ubuntu:24.04", provider.ImageConfig{Digest: "sha256:dd"})
	req.FallbackImage, req.Addons = "ubuntu:24.04", []string{"desktop"}
	img, err := r.Image(ctx, f, req)
	if err != nil || img.BaseImageDigest != "sha256:dd" || img.DevcontainerHash != "" || len(img.Addons) != 1 {
		t.Fatalf("resolved to %+v, %v", img, err)
	}
	// The same image as a box of it named explicitly.
	explicit, _ := r.Image(ctx, f, devcontainer.Request{Target: "t1", Image: "ubuntu:24.04", Addons: []string{"desktop"}})
	if !slices.Equal(explicit.Addons, img.Addons) || explicit.BaseImageDigest != img.BaseImageDigest || explicit.DevcontainerHash != img.DevcontainerHash {
		t.Errorf("a fallback resolved to %+v, the image named %+v", img, explicit)
	}
	ref, err := r.Build(ctx, f, req, img, "web/box")
	if err != nil || !strings.HasPrefix(dockerfileOf(t, f, ref), "FROM ubuntu:24.04@sha256:dd\nUSER root\n") {
		t.Errorf("a fallback built %q, %v", ref, err)
	}
	if steps, err := r.Setup(ctx, f, req); err != nil || steps != nil {
		t.Errorf("a fallback sets up %v, %v", steps, err)
	}

	withOne, wreq := repo(t, map[string]string{".devcontainer.json": `{"image": "node:22"}`})
	withOne.SetImage("node:22", provider.ImageConfig{Digest: "sha256:ee"})
	wreq.FallbackImage = "ubuntu:24.04"
	if img, err := r.Image(ctx, withOne, wreq); err != nil || img.BaseImageDigest != "sha256:ee" || img.DevcontainerHash == "" {
		t.Errorf("a devcontainer with a fallback resolved to %+v, %v", img, err)
	}

	missing, mreq := repo(t, map[string]string{"README": "plain"})
	missing.SetMissingImage("gone:1")
	mreq.FallbackImage = "gone:1"
	if _, err := r.Image(ctx, missing, mreq); !errors.Is(err, devcontainer.ErrUnresolvable) || !strings.Contains(err.Error(), "the fallback image gone:1 is not in its registry") {
		t.Errorf("a fallback image not in its registry: %v", err)
	}
}

// TestADevcontainerThatCannotBeMadeFallsBack: a devcontainer that cannot be
// made an image of is the image to fall back to when there is one, saying
// why, as workstream falls back; asked for, the fallback is the image
// whatever the devcontainer.
func TestADevcontainerThatCannotBeMadeFallsBack(t *testing.T) {
	r, ctx := devcontainer.Real{}, t.Context()
	f, req := repo(t, map[string]string{".devcontainer.json": `{"image": `, "README": "x"})
	f.SetImage("ubuntu:24.04", provider.ImageConfig{Digest: "sha256:dd"})
	if _, err := r.Image(ctx, f, req); !errors.Is(err, devcontainer.ErrUnresolvable) {
		t.Fatalf("with no fallback: %v", err)
	}
	req.FallbackImage = "ubuntu:24.04"
	img, err := r.Image(ctx, f, req)
	if err != nil || img.BaseImageDigest != "sha256:dd" || img.DevcontainerHash != "" || !strings.Contains(img.FellBack, ".devcontainer.json does not parse") {
		t.Errorf("fell back to %+v, %v", img, err)
	}
	if steps, err := r.Setup(ctx, f, req); err != nil || steps != nil {
		t.Errorf("one fallen back sets up %v, %v", steps, err)
	}
	good, greq := repo(t, map[string]string{".devcontainer.json": `{"image": "node:22", "postCreateCommand": "npm ci"}`})
	good.SetImage("ubuntu:24.04", provider.ImageConfig{Digest: "sha256:dd"})
	greq.FallbackImage, greq.UseFallback = "ubuntu:24.04", true
	if img, err := r.Image(ctx, good, greq); err != nil || img.BaseImageDigest != "sha256:dd" || img.DevcontainerHash != "" {
		t.Errorf("asked to fall back: %+v, %v", img, err)
	}
	// Without a devcontainer at all, it falls back saying nothing: that is
	// the plain box workstream makes.
	plain, preq := repo(t, map[string]string{"README": "x"})
	plain.SetImage("ubuntu:24.04", provider.ImageConfig{Digest: "sha256:dd"})
	preq.FallbackImage = "ubuntu:24.04"
	if img, err := r.Image(ctx, plain, preq); err != nil || img.FellBack != "" {
		t.Errorf("no devcontainer: %+v, %v", img, err)
	}
}

// noRegistry is a provider whose registry is never asked: each ask is
// counted and refused.
type noRegistry struct {
	*fake.Fake
	asked int
}

func (n *noRegistry) ResolveImage(context.Context, string) (provider.ImageConfig, error) {
	n.asked++
	return provider.ImageConfig{}, provider.ErrUnavailable
}

// TestABoxsSetupAsksNoRegistry: what a box sets up once checked out is
// read from its repository alone: the image is its golden's already.
func TestABoxsSetupAsksNoRegistry(t *testing.T) {
	r, ctx := devcontainer.Real{}, t.Context()
	f, req := repo(t, map[string]string{".devcontainer.json": `{"image": "node:22", "postCreateCommand": "npm ci"}`})
	p := &noRegistry{Fake: f}
	steps, err := r.Setup(ctx, p, req)
	if err != nil || len(steps) != 1 || p.asked != 0 {
		t.Errorf("set up with %v, %v, asking the registry %d times", steps, err, p.asked)
	}
	plain, preq := repo(t, map[string]string{"README": "x"})
	preq.FallbackImage = "ubuntu:24.04"
	q := &noRegistry{Fake: plain}
	if steps, err := r.Setup(ctx, q, preq); err != nil || steps != nil || q.asked != 0 {
		t.Errorf("a fallback set up with %v, %v, asking the registry %d times", steps, err, q.asked)
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
		"no devcontainer":       {files: map[string]string{"README": "x"}, says: "has no .devcontainer/devcontainer.json or .devcontainer.json at c1, and there is no image to fall back to"},
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
	if w := warm(map[string]string{"go.sum": "a", "mise.toml": "x", "main.go": "2"}); w.ToolchainHash != base.ToolchainHash || w.LockfilesHash != base.LockfilesHash || w.PrebuildHash != base.PrebuildHash {
		t.Error("a change to neither the toolchain nor a lockfile changed what a warm checkout depends on")
	}
	if w := warm(map[string]string{"go.sum": "b", "mise.toml": "x"}); w.LockfilesHash == base.LockfilesHash || w.ToolchainHash != base.ToolchainHash {
		t.Error("a lockfile's change is not told from the toolchain's")
	}
	if w := warm(map[string]string{"go.sum": "a", "mise.toml": "y"}); w.ToolchainHash == base.ToolchainHash || w.LockfilesHash != base.LockfilesHash {
		t.Error("the toolchain's change is not told from a lockfile's")
	}
}

// names are the names of a pre-build's steps, in order.
func names(steps []devcontainer.Step) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Name)
	}
	return out
}

// TestAWarmCheckoutInstallsWhatItsFilesCallFor: a warm checkout's
// pre-build installs by the files at its commit, each through bash -lc and
// skipped where the image lacks its tool; mise's first, its tools then on
// every later step's PATH.
func TestAWarmCheckoutInstallsWhatItsFilesCallFor(t *testing.T) {
	r, ctx := devcontainer.Real{}, t.Context()
	for what, tc := range map[string]struct {
		files map[string]string
		want  []string
	}{
		"nothing to install": {files: map[string]string{"README": "a"}},
		"go":                 {files: map[string]string{"go.mod": "module a\n", "go.sum": "x"}, want: []string{"go mod download"}},
		"npm":                {files: map[string]string{"package-lock.json": "{}"}, want: []string{"npm ci"}},
		"pnpm":               {files: map[string]string{"pnpm-lock.yaml": "x"}, want: []string{"pnpm install"}},
		"yarn":               {files: map[string]string{"yarn.lock": "x"}, want: []string{"yarn install"}},
		"uv":                 {files: map[string]string{"uv.lock": "x"}, want: []string{"uv sync"}},
		"mise and the rest": {
			files: map[string]string{".tool-versions": "go 1.26", "go.mod": "module a\n", "package-lock.json": "{}", "uv.lock": "x"},
			want:  []string{"mise install", "go mod download", "npm ci", "uv sync"},
		},
		"a lockfile with no install of its own": {files: map[string]string{"Cargo.lock": "x"}},
	} {
		f, req := repo(t, tc.files)
		w, err := r.Warm(ctx, f, req.Mirror, req.Commit)
		if err != nil {
			t.Fatal(err)
		}
		if got := names(w.Install); !slices.Equal(got, tc.want) {
			t.Errorf("%s: the pre-build %q, want %q", what, got, tc.want)
		}
		for _, s := range w.Install {
			if len(s.Command) != 3 || s.Command[0] != "bash" || s.Command[1] != "-lc" {
				t.Errorf("%s: %s runs %q, not through bash -lc", what, s.Name, s.Command)
				continue
			}
			script := s.Command[2]
			if tool, _, _ := strings.Cut(s.Name, " "); !strings.Contains(script, "command -v "+tool+" >/dev/null 2>&1 || {") {
				t.Errorf("%s: %s is not skipped without %s:\n%s", what, s.Name, tool, script)
			}
			if mise := strings.Contains(script, "mise env"); mise != (tc.files[".tool-versions"] != "" && s.Name != "mise install") {
				t.Errorf("%s: %s has mise's tools on its PATH: %t:\n%s", what, s.Name, mise, script)
			}
		}
	}
}

// TestAWarmCheckoutsBuildIsItsUpdateContentCommandElseMakeBuild: after its
// installs, a warm checkout runs its devcontainer's updateContentCommand,
// written any way a devcontainer command is; else make build, if the root
// Makefile make reads has a build target; else nothing. A change to either
// changes what the warm checkout depends on.
func TestAWarmCheckoutsBuildIsItsUpdateContentCommandElseMakeBuild(t *testing.T) {
	r, ctx := devcontainer.Real{}, t.Context()
	const makefile = "GO ?= go\nbuild: gen\n\t$(GO) build ./...\n"
	warm := func(files map[string]string) devcontainer.Warm {
		t.Helper()
		f, req := repo(t, files)
		w, err := r.Warm(ctx, f, req.Mirror, req.Commit)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	last := func(w devcontainer.Warm) string {
		if len(w.Install) == 0 {
			return ""
		}
		s := w.Install[len(w.Install)-1]
		lines := strings.Split(s.Command[len(s.Command)-1], "\n")
		return s.Name + ": " + lines[len(lines)-1]
	}
	for what, tc := range map[string]struct {
		files map[string]string
		want  string
	}{
		"updateContentCommand over make build": {files: map[string]string{
			".devcontainer/devcontainer.json": `{"image": "x", "updateContentCommand": "npm run prepare"}`, "Makefile": makefile,
		}, want: "updateContent: npm run prepare"},
		"an array": {files: map[string]string{
			".devcontainer.json": `{"image": "x", "updateContentCommand": ["make", "gen"]}`,
		}, want: "updateContent: make gen"},
		"named commands": {files: map[string]string{
			".devcontainer.json": `{"image": "x", "updateContentCommand": {"deps": "go mod download", "gen": "go generate ./..."}}`,
		}, want: "updateContent: go mod download && go generate ./..."},
		"make build":                        {files: map[string]string{".devcontainer.json": `{"image": "x"}`, "Makefile": makefile}, want: "make build: make build"},
		"make build among other targets":    {files: map[string]string{"Makefile": "all test build: x\n"}, want: "make build: make build"},
		"GNUmakefile, read before Makefile": {files: map[string]string{"GNUmakefile": "build:\n", "Makefile": "test:\n"}, want: "make build: make build"},
		"makefile without build":            {files: map[string]string{"Makefile": "BUILD := build: x\ntest:\n\tgo test ./...\n# build:\n"}},
		"a build that is a variable":        {files: map[string]string{"Makefile": "build:=x\n"}},
		"neither":                           {files: map[string]string{"go.mod": "module a\n"}, want: "go mod download: go mod download"},
	} {
		if got := last(warm(tc.files)); got != tc.want {
			t.Errorf("%s: the pre-build ends with %q, want %q", what, got, tc.want)
		}
	}

	base := warm(map[string]string{"Makefile": makefile, "main.go": "1"})
	if w := warm(map[string]string{"Makefile": makefile + "\ntest:\n", "main.go": "1"}); w.PrebuildHash == base.PrebuildHash {
		t.Error("a change to the Makefile did not change the pre-build's hash")
	}
	dc := func(cmd string) map[string]string {
		return map[string]string{".devcontainer.json": `{"image": "x", "updateContentCommand": "` + cmd + `"}`}
	}
	if a, b := warm(dc("make a")), warm(dc("make b")); a.PrebuildHash == b.PrebuildHash {
		t.Error("a change to updateContentCommand did not change the pre-build's hash")
	}
	if a, b := warm(dc("make a")), warm(map[string]string{".devcontainer.json": `{"image": "y", "updateContentCommand": "make a"}`}); a.PrebuildHash != b.PrebuildHash {
		t.Error("a change to the devcontainer that is not its pre-build changed the pre-build's hash")
	}
}
