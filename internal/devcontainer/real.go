package devcontainer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/provider"
)

// layerVersion is part of every devcontainer's hash, so a change to how
// tower builds the layer builds every image again.
const layerVersion = "tower-layer-2"

// lockfiles are what a warm checkout's dependencies are pinned by.
var lockfiles = []string{
	"go.sum", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb",
	"uv.lock", "poetry.lock", "Pipfile.lock", "Cargo.lock", "Gemfile.lock", "composer.lock",
}

// Real resolves a box's image as workstream does from the same repository
// (workstream: src/box/devcontainer.ts, src/box/docker.ts, src/box/
// addon-image.ts), reading the repository at the box's ref from the
// target's mirror and building on the target. What workstream reads it
// reads: image, or build.dockerfile with build.context; containerEnv,
// with ${containerWorkspaceFolder}, ${localWorkspaceFolder} and
// ${localEnv:...} resolved; remoteUser, else containerUser;
// onCreateCommand and postCreateCommand; and whether the repository
// declares a mise toolchain. It skips what workstream skips, and so builds
// none of a devcontainer's features, nor its build.args; workstream's
// docker-in-docker, a daemon in a privileged container, has no place on
// tower, whose instances are never privileged. A devcontainer it cannot
// make an image of is ErrUnresolvable, saying why: where workstream falls
// back to a plain image, tower fails the box, which asked for this one.
type Real struct{}

var _ Resolver = Real{}

// resolved is what a request resolves to.
type resolved struct {
	addons []addon
	// cfg is the devcontainer's, nil for an explicit image or build.
	cfg *config
	// commit is the devcontainer's commit.
	commit string
	// base is the image the layer is FROM when it is not built: an
	// explicit image or the devcontainer's, by digest.
	base string
	// dockerfile and contextDir are a devcontainer's build, in the work
	// tree.
	dockerfile, contextDir string
	// hasMise says the repository declares a mise toolchain.
	hasMise bool
	img     Image
}

// unresolvable is ErrUnresolvable, saying why.
func unresolvable(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), ErrUnresolvable)
}

// byDigest is ref named by digest: its tag, if any, kept for people.
func byDigest(ref, digest string) string {
	if strings.Contains(ref, "@") {
		return ref
	}
	return ref + "@" + digest
}

// inTree joins rel to dir in a work tree; one leaving it is no path.
func inTree(dir, rel string) (string, bool) {
	p := path.Clean(path.Join(dir, rel))
	if p == "." {
		return "", true
	}
	if p == ".." || strings.HasPrefix(p, "../") || path.IsAbs(p) {
		return "", false
	}
	return p, true
}

// resolve resolves req. For a box's setup alone, forSetup, it asks no
// registry: the image is the golden's already, and its setup is read from
// the repository.
func (re Real) resolve(ctx context.Context, p provider.Provider, req Request, forSetup bool) (resolved, error) {
	var r resolved
	for _, id := range req.Addons {
		a, err := loadAddon(id)
		if err != nil {
			return resolved{}, err
		}
		r.addons = append(r.addons, a)
		r.img.Addons = append(r.img.Addons, a.version())
	}
	switch {
	case req.Build != nil:
		r.img.BuildHash = req.Build.Hash()
		return r, nil
	case req.Image != "":
		err := r.explicit(ctx, p, req.Image, "the image")
		return r, err
	}
	// A box whose devcontainer's image did not build has asked for the
	// image to fall back to.
	if req.UseFallback && req.FallbackImage != "" {
		return r.fallBack(ctx, p, req.FallbackImage, forSetup)
	}
	d, err := re.devcontainer(ctx, p, req, forSetup, r)
	switch {
	case err == nil:
		return d, nil
	case req.FallbackImage == "":
		return resolved{}, err
	case errors.Is(err, errNoDevcontainer):
		// What workstream's setup "auto" does: the image it falls back
		// to, which sets nothing up.
		return r.fallBack(ctx, p, req.FallbackImage, forSetup)
	case errors.Is(err, ErrUnresolvable):
		// What workstream does of a devcontainer it cannot make a box of,
		// saying why.
		f, ferr := r.fallBack(ctx, p, req.FallbackImage, forSetup)
		f.img.FellBack = Reason(err)
		return f, ferr
	}
	return resolved{}, err
}

// errNoDevcontainer is a repository without a devcontainer.
var errNoDevcontainer = fmt.Errorf("no devcontainer: %w", ErrUnresolvable)

// Reason is an ErrUnresolvable's reason, for people.
func Reason(err error) string {
	msg := strings.TrimSuffix(err.Error(), ": "+errNoDevcontainer.Error())
	return strings.TrimSuffix(msg, ": "+ErrUnresolvable.Error())
}

// fallBack makes ref the image, as an explicit one is, without a
// devcontainer and with nothing to set up.
func (r resolved) fallBack(ctx context.Context, p provider.Provider, ref string, forSetup bool) (resolved, error) {
	if forSetup {
		return r, nil
	}
	err := r.explicit(ctx, p, ref, "the fallback image")
	return r, err
}

// devcontainer resolves the checkout's devcontainer onto r.
func (Real) devcontainer(ctx context.Context, p provider.Provider, req Request, forSetup bool, r resolved) (resolved, error) {
	src := provider.MirrorSource{Mirror: req.Mirror, Ref: req.Commit}
	read, err := p.ReadMirror(ctx, provider.MirrorReadRequest{MirrorSource: src, Paths: append(slices.Clone(configCandidates), miseFiles...), Content: true})
	if errors.Is(err, provider.ErrNotFound) && req.Commit != "" {
		return resolved{}, unresolvable("the repository has no %s", req.Commit)
	}
	if err != nil {
		return resolved{}, err
	}
	r.commit = read.Commit
	var cfg config
	found := false
	for _, c := range configCandidates {
		e, ok := read.Entries[c]
		if !ok || e.Dir {
			continue
		}
		if e.Content == nil {
			return resolved{}, unresolvable("%s is larger than %d bytes", c, provider.MaxMirrorContent)
		}
		if cfg, err = parseConfig(c, e.Content); err != nil {
			return resolved{}, unresolvable("%v", err)
		}
		found = true
		break
	}
	if !found {
		return resolved{}, fmt.Errorf("the repository has no %s at %s, and there is no image to fall back to: %w", strings.Join(configCandidates, " or "), short(r.commit), errNoDevcontainer)
	}
	r.cfg = &cfg
	r.img.Env = cfg.Env
	r.hasMise = cfg.Env["MISE_CONFIG_FILE"] != ""
	for _, f := range miseFiles {
		if e, ok := read.Entries[f]; ok && !e.Dir {
			r.hasMise = true
		}
	}
	// What the devcontainer is: its file, and what it builds from.
	h := sha256.New()
	part := func(s string) { _, _ = fmt.Fprintf(h, "%d:%s", len(s), s) }
	part(layerVersion)
	part(cfg.Path)
	part(string(read.Entries[cfg.Path].Content))
	switch {
	case cfg.Image != "" && forSetup:
	case cfg.Image != "":
		ic, err := p.ResolveImage(ctx, cfg.Image)
		if errors.Is(err, provider.ErrNotFound) {
			return resolved{}, unresolvable("%s names the image %s, which is not in its registry", cfg.Path, cfg.Image)
		}
		if err != nil {
			return resolved{}, err
		}
		r.base, r.img.BaseImageDigest = byDigest(cfg.Image, ic.Digest), ic.Digest
	case cfg.Dockerfile != "":
		var ok1, ok2 bool
		r.dockerfile, ok1 = inTree(cfg.Dir, cfg.Dockerfile)
		r.contextDir, ok2 = inTree(cfg.Dir, cmpOr(cfg.Context, "."))
		if !ok1 || !ok2 || r.dockerfile == "" {
			return resolved{}, unresolvable("%s builds from outside the repository", cfg.Path)
		}
		built, err := p.ReadMirror(ctx, provider.MirrorReadRequest{MirrorSource: provider.MirrorSource{Mirror: req.Mirror, Ref: r.commit}, Paths: []string{r.dockerfile, r.contextDir}})
		if err != nil {
			return resolved{}, err
		}
		df, ok := built.Entries[r.dockerfile]
		if !ok || df.Dir {
			return resolved{}, unresolvable("%s builds from %s, which the repository does not hold", cfg.Path, r.dockerfile)
		}
		dir, ok := built.Entries[r.contextDir]
		if !ok || !dir.Dir {
			return resolved{}, unresolvable("%s builds in %s, which is no directory of the repository", cfg.Path, cmpOr(r.contextDir, "."))
		}
		// The Dockerfile and its context by their git objects: a change to
		// any file under the context is another image.
		part(r.dockerfile + "=" + df.Object)
		part(r.contextDir + "=" + dir.Object)
	default:
		return resolved{}, unresolvable("%s declares neither image nor build.dockerfile", cfg.Path)
	}
	r.img.DevcontainerHash = hex.EncodeToString(h.Sum(nil))
	return r, nil
}

// explicit makes ref, an image named rather than a devcontainer's, the
// base, by its digest.
func (r *resolved) explicit(ctx context.Context, p provider.Provider, ref, what string) error {
	cfg, err := p.ResolveImage(ctx, ref)
	if errors.Is(err, provider.ErrNotFound) {
		return unresolvable("%s %s is not in its registry", what, ref)
	}
	if err != nil {
		return err
	}
	r.base, r.img.BaseImageDigest = byDigest(ref, cfg.Digest), cfg.Digest
	return nil
}

func cmpOr(s, or string) string {
	if s == "" {
		return or
	}
	return s
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// Image resolves the image's inputs.
func (re Real) Image(ctx context.Context, p provider.Provider, req Request) (Image, error) {
	r, err := re.resolve(ctx, p, req, false)
	r.img.Commit = r.commit
	return r.img, err
}

// Build builds the image: the box's own build, or the devcontainer's from
// the mirror, or neither for an image by digest; then the layer on it, of
// the add-ons, the containerEnv and the remoteUser, which an image that
// needs none of them and is tower's already goes without. The base of an
// explicit or a devcontainer image is the one img names by digest, which
// is what the golden was made for.
func (re Real) Build(ctx context.Context, p provider.Provider, req Request, img Image, repository string) (string, error) {
	r, err := re.resolve(ctx, p, req, false)
	if err != nil {
		return "", err
	}
	l := layer{Addons: r.addons}
	if r.cfg != nil {
		l.RemoteUser = r.cfg.User
	}
	switch {
	case req.Build != nil:
		ctxTar, err := req.Build.Context("")
		if err != nil {
			return "", err
		}
		if l.Base, err = p.BuildImage(ctx, provider.BuildRequest{Context: bytesReader(ctxTar), Repository: repository}); err != nil {
			return "", err
		}
		if len(l.Addons) == 0 {
			return l.Base, nil
		}
	case r.dockerfile != "":
		if l.Base, err = p.BuildImage(ctx, provider.BuildRequest{
			Mirror: &provider.MirrorSource{Mirror: req.Mirror, Ref: r.commit}, Dir: r.contextDir, Dockerfile: r.dockerfile, Repository: repository,
		}); err != nil {
			return "", err
		}
	default:
		l.Base = r.base
		if img.BaseImageDigest != "" {
			name, _, _ := strings.Cut(r.base, "@")
			l.Base = name + "@" + img.BaseImageDigest
		}
	}
	if len(l.Addons) > 0 {
		cfg, err := p.ResolveImage(ctx, l.Base)
		if err != nil {
			return "", err
		}
		l.BaseUser = cfg.User
	}
	layerTar, err := l.context()
	if err != nil {
		return "", err
	}
	return p.BuildImage(ctx, provider.BuildRequest{Context: bytesReader(layerTar), Repository: repository})
}

// Setup is the devcontainer's steps, which every box made of it runs once
// after its checkouts, in /workspace, as the image's user, as workstream
// runs them: mise's toolchain if the repository declares one, then
// onCreateCommand, then postCreateCommand, each through bash -lc. A step
// that fails does not fail the box; workstream keeps it, with a warning.
// An explicit image or build has none.
func (re Real) Setup(ctx context.Context, p provider.Provider, req Request) ([]Step, error) {
	if req.Image != "" || req.Build != nil {
		return nil, nil
	}
	r, err := re.resolve(ctx, p, Request{Target: req.Target, Mirror: req.Mirror, Commit: req.Commit, FallbackImage: req.FallbackImage}, true)
	if err != nil || r.cfg == nil {
		return nil, err
	}
	var steps []Step
	if r.hasMise {
		steps = append(steps, Step{Name: "mise install", Command: []string{"bash", "-lc", "command -v mise >/dev/null 2>&1 && mise trust && mise install || true"}})
	}
	if c := r.cfg.OnCreate; c != "" {
		steps = append(steps, Step{Name: "onCreate", Command: []string{"bash", "-lc", c}})
	}
	if c := r.cfg.PostCreate; c != "" {
		steps = append(steps, Step{Name: "postCreate", Command: []string{"bash", "-lc", c}})
	}
	return steps, nil
}

// Warm is what a warm checkout at commit depends on, and its pre-build:
// the installs its files call for, then updateContentCommand, else make
// build (docs/decisions/devcontainers.md).
func (Real) Warm(ctx context.Context, p provider.Provider, mirror, commit string) (Warm, error) {
	src := provider.MirrorSource{Mirror: mirror, Ref: commit}
	read, err := p.ReadMirror(ctx, provider.MirrorReadRequest{MirrorSource: src, Paths: append(append(slices.Clone(miseFiles), lockfiles...), "go.mod")})
	if err != nil {
		return Warm{}, err
	}
	// What the pre-build's last step is read from, by content.
	cmds, err := p.ReadMirror(ctx, provider.MirrorReadRequest{MirrorSource: src, Paths: append(slices.Clone(configCandidates), makefiles...), Content: true})
	if err != nil {
		return Warm{}, err
	}
	hashOf := func(names []string) string {
		h := sha256.New()
		for _, n := range names {
			if e, ok := read.Entries[n]; ok {
				_, _ = fmt.Fprintf(h, "%s=%s\n", n, e.Object)
			}
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	has := func(n string) bool { e, ok := read.Entries[n]; return ok && !e.Dir }
	var cfg *config
	for _, c := range configCandidates {
		if e, ok := cmds.Entries[c]; ok && !e.Dir && e.Content != nil {
			// One that does not parse makes no image either: its boxes fail,
			// saying why, and its pre-build is what a repository without one
			// has.
			if c, err := parseConfig(c, e.Content); err == nil {
				cfg = &c
			}
			break
		}
	}
	mise := slices.ContainsFunc(miseFiles, has) || cfg != nil && cfg.Env["MISE_CONFIG_FILE"] != ""
	var steps []Step
	if mise {
		steps = append(steps, Step{Name: "mise install", Command: prebuildStep("mise", false, "mise trust && mise install")})
	}
	for _, i := range installs {
		if has(i.file) {
			steps = append(steps, Step{Name: i.name, Command: prebuildStep(i.tool, mise, i.command)})
		}
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\n", prebuildVersion)
	for _, m := range makefiles {
		if e, ok := cmds.Entries[m]; ok {
			_, _ = fmt.Fprintf(h, "%s=%s\n", m, e.Object)
		}
	}
	switch {
	case cfg != nil && cfg.UpdateContent != "":
		steps = append(steps, Step{Name: "updateContent", Command: prebuildStep("", mise, cfg.UpdateContent)})
	case hasBuildTarget(cmds.Entries):
		steps = append(steps, Step{Name: "make build", Command: prebuildStep("make", mise, "make build")})
	}
	for _, s := range steps {
		_, _ = fmt.Fprintf(h, "%d:%s\n", len(s.Command[len(s.Command)-1]), s.Command[len(s.Command)-1])
	}
	return Warm{
		ToolchainHash: hashOf(miseFiles), LockfilesHash: hashOf(lockfiles),
		PrebuildHash: hex.EncodeToString(h.Sum(nil)), Install: steps,
	}, nil
}

// prebuildVersion is part of every pre-build's hash, so a change to what
// tower runs builds every warm golden again at its next rebuild.
const prebuildVersion = "tower-prebuild-1"

// installs are the dependency installs of a warm checkout, in order: each
// runs when the repository holds its file.
var installs = []struct{ file, name, tool, command string }{
	{"go.mod", "go mod download", "go", "go mod download"},
	{"package-lock.json", "npm ci", "npm", "npm ci"},
	{"pnpm-lock.yaml", "pnpm install", "pnpm", "pnpm install --frozen-lockfile"},
	{"yarn.lock", "yarn install", "yarn", "yarn install --frozen-lockfile"},
	{"uv.lock", "uv sync", "uv", "uv sync"},
}

// makefiles are the names make reads a repository's root Makefile by.
var makefiles = []string{"GNUmakefile", "makefile", "Makefile"}

// prebuildStep is script through bash -lc, skipped without tool ("" for
// none), with mise's tools on its PATH when mise is set.
func prebuildStep(tool string, mise bool, script string) []string {
	var pre string
	if mise {
		pre = `if command -v mise >/dev/null 2>&1; then eval "$(mise env -s bash)"; fi` + "\n"
	}
	if tool != "" {
		pre += fmt.Sprintf("command -v %s >/dev/null 2>&1 || { echo '%s is not installed: skipped'; exit 0; }\n", tool, tool)
	}
	return []string{"bash", "-lc", pre + script}
}

// hasBuildTarget reports whether the root Makefile make reads declares a
// build target; one too large to read has none.
func hasBuildTarget(entries map[string]provider.MirrorEntry) bool {
	for _, m := range makefiles {
		e, ok := entries[m]
		if !ok || e.Dir {
			continue
		}
		for _, line := range strings.Split(string(e.Content), "\n") {
			if line == "" || line[0] == '\t' || line[0] == '#' {
				continue
			}
			targets, rest, ok := strings.Cut(line, ":")
			// A variable's assignment, not a rule.
			if !ok || strings.HasPrefix(rest, "=") || strings.ContainsAny(targets, "=$") {
				continue
			}
			if slices.Contains(strings.Fields(targets), "build") {
				return true
			}
		}
		return false
	}
	return false
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
