// Package devcontainer says what a box's image is made of: given a
// repository's checkout in a target's mirror, or an explicit base image,
// or a box's own build, and the add-ons asked for, a Resolver returns the
// inputs that identify the image, builds it on the target, and says what a
// box made of it runs once it is checked out and what a warm checkout of
// the repository depends on. Real reads devcontainer.json as workstream
// does; Fake answers from the request alone, for tests.
package devcontainer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/provider"
)

// ErrUnresolvable is a box's image that cannot be made of what it names, a
// devcontainer tower does not support or a repository without one; its
// message says why. It is no use asking again.
var ErrUnresolvable = errors.New("devcontainer: unresolvable")

// Request is what an image is resolved from.
type Request struct {
	Target string
	// Mirror and Commit name the checkout whose devcontainer is read;
	// empty when Image or Build is set. Commit may be any ref of the
	// mirror.
	Mirror, Commit string
	// Image is an explicit base image, which skips the devcontainer.
	Image string
	// FallbackImage is the image when the checkout has no devcontainer, or
	// one that cannot be made an image of; empty fails such a request.
	FallbackImage string `json:",omitempty"`
	// UseFallback asks for FallbackImage whatever the devcontainer: its
	// image did not build.
	UseFallback bool `json:",omitempty"`
	// Build is the box's own build, which skips the devcontainer; nil
	// for none. At most one of Image and Build is set.
	Build *Build `json:",omitempty"`
	// Addons are layered onto the image, by name, in the order asked.
	Addons []string
}

// Build is a box image's own build (BoxSpec.image_build): a Dockerfile
// and the files of its context beside it.
type Build struct {
	Dockerfile string
	Files      map[string][]byte `json:",omitempty"`
}

// Hash is a hash of the build's Dockerfile and files, each part written
// with its length, so no two builds share one however their bytes run.
func (b Build) Hash() string {
	h := sha256.New()
	write := func(p []byte) {
		_, _ = fmt.Fprintf(h, "%d:", len(p))
		_, _ = h.Write(p)
	}
	write([]byte(b.Dockerfile))
	for _, name := range slices.Sorted(maps.Keys(b.Files)) {
		write([]byte(name))
		write(b.Files[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Context is a tar of the build's context: its files and its Dockerfile,
// which extra, if any, is added to the end of.
func (b Build) Context(extra string) ([]byte, error) {
	files := map[string]file{"Dockerfile": {data: []byte(b.Dockerfile + extra), mode: 0o644}}
	for name, data := range b.Files {
		files[name] = file{data: data, mode: 0o644}
	}
	return tarOf(files)
}

// file is a file of a build context.
type file struct {
	data []byte
	mode int64
}

// tarOf is files as a tar, in name order.
func tarOf(files map[string]file) ([]byte, error) {
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f := files[name]
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: f.mode, Size: int64(len(f.data))}); err != nil {
			return nil, err
		}
		if _, err := w.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Image is a resolved box image.
type Image struct {
	// BaseImageDigest, DevcontainerHash, BuildHash and Addons (with their
	// versions) are BoxImageInputs.
	BaseImageDigest  string
	DevcontainerHash string
	BuildHash        string
	Addons           []string
	// Context is Fake's build context, a tar with its Dockerfile.
	Context []byte
	// Commit is the commit the devcontainer was read at, "" for none: what
	// its image is built from, though the ref asked for moves.
	Commit string
	// Env is the devcontainer's containerEnv, which a box of the image is
	// given as its environment (InstanceSpec.env), each value as written.
	Env map[string]string
	// FellBack, when set, is why the image is the fallback, not the
	// devcontainer's.
	FellBack string
}

// Step is a command a box runs once, after its repositories are checked
// out, in /workspace: one of a devcontainer's.
type Step struct {
	// Name is for people, such as "postCreate".
	Name    string
	Command []string
}

// Warm is what a warm checkout of a repository at a commit depends on.
type Warm struct {
	// ToolchainHash and LockfilesHash are BoxWarmInputs'.
	ToolchainHash, LockfilesHash string
	// Install, if any, installs the toolchain and the dependencies in the
	// checkout.
	Install []string
}

// Resolver resolves images and warm checkouts, reading and building
// through the target's provider.
type Resolver interface {
	// Image resolves an image's inputs.
	Image(ctx context.Context, p provider.Provider, req Request) (Image, error)
	// Build builds the image Image resolved, pushing it to repository in
	// the registry, and returns its reference by digest.
	Build(ctx context.Context, p provider.Provider, req Request, img Image, repository string) (string, error)
	// Setup is what a box of the image runs once it is checked out.
	Setup(ctx context.Context, p provider.Provider, req Request) ([]Step, error)
	Warm(ctx context.Context, p provider.Provider, mirror, commit string) (Warm, error)
}

// Fake resolves from the request alone, as a repository whose
// devcontainer never changes would: every commit of one mirror is one
// image, and a warm checkout depends on its commit. Steps are what a box
// of a devcontainer runs.
type Fake struct {
	Steps []Step
}

func hash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (Fake) Image(_ context.Context, _ provider.Provider, req Request) (Image, error) {
	var addons []string
	for _, a := range slices.Sorted(slices.Values(req.Addons)) {
		addons = append(addons, a+"@1")
	}
	if b := req.Build; b != nil {
		// Its own Dockerfile, the add-ons written after it.
		ctx, err := b.Context(fmt.Sprintf("\n# addons %s\n", strings.Join(addons, ",")))
		if err != nil {
			return Image{}, err
		}
		return Image{BuildHash: b.Hash(), Addons: addons, Context: ctx}, nil
	}
	base := req.Image
	if base == "" {
		base = "devcontainer:" + req.Mirror
	}
	img := Image{
		BaseImageDigest:  "sha256:" + hash("base", base),
		DevcontainerHash: hash("devcontainer", req.Mirror, req.Image),
		Addons:           addons,
	}
	dockerfile := fmt.Sprintf("FROM %s\n# %s\n# addons %s\n", img.BaseImageDigest, img.DevcontainerHash, strings.Join(addons, ","))
	ctx, err := tarOf(map[string]file{"Dockerfile": {data: []byte(dockerfile), mode: 0o644}})
	if err != nil {
		return Image{}, err
	}
	img.Context = ctx
	return img, nil
}

// Build builds the context Image made.
func (f Fake) Build(ctx context.Context, p provider.Provider, req Request, _ Image, repository string) (string, error) {
	img, err := f.Image(ctx, p, req)
	if err != nil {
		return "", err
	}
	return p.BuildImage(ctx, provider.BuildRequest{Context: bytes.NewReader(img.Context), Repository: repository})
}

func (f Fake) Setup(_ context.Context, _ provider.Provider, req Request) ([]Step, error) {
	if req.Image != "" || req.Build != nil {
		return nil, nil
	}
	return f.Steps, nil
}

func (Fake) Warm(_ context.Context, _ provider.Provider, mirror, commit string) (Warm, error) {
	return Warm{
		ToolchainHash: hash("toolchain", mirror, commit), LockfilesHash: hash("lockfiles", mirror, commit),
		Install: []string{"install-dependencies"},
	}, nil
}
