// Package devcontainer says what a box's image is made of: given a
// repository's checkout in a target's mirror, or an explicit base image,
// and the add-ons asked for, a Resolver returns the inputs that identify
// the image and the build context that builds it, and what a warm checkout
// of the repository depends on. Reading devcontainer.json, its features and
// Dockerfile is a Resolver's; this package holds the contract and Fake.
package devcontainer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Request is what an image is resolved from.
type Request struct {
	Target string
	// Mirror and Commit name the checkout whose devcontainer is read;
	// empty when Image is set.
	Mirror, Commit string
	// Image is an explicit base image, which skips the devcontainer.
	Image string
	// Build is the box's own build, which skips the devcontainer; nil
	// for none. At most one of Image and Build is set.
	Build *Build `json:",omitempty"`
	// Addons are layered onto the image, by name.
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
	files := map[string][]byte{"Dockerfile": []byte(b.Dockerfile + extra)}
	for name, data := range b.Files {
		files[name] = data
	}
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name]))}); err != nil {
			return nil, err
		}
		if _, err := w.Write(files[name]); err != nil {
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
	// Context is the build context, a tar with its Dockerfile.
	Context []byte
}

// Warm is what a warm checkout of a repository at a commit depends on.
type Warm struct {
	// ToolchainHash and LockfilesHash are BoxWarmInputs'.
	ToolchainHash, LockfilesHash string
	// Install installs the toolchain and the dependencies in the checkout.
	Install []string
}

// Resolver resolves images and warm checkouts.
type Resolver interface {
	Image(ctx context.Context, req Request) (Image, error)
	Warm(ctx context.Context, target, mirror, commit string) (Warm, error)
}

// Fake resolves from the request alone, as a repository whose
// devcontainer never changes would: every commit of one mirror is one
// image, and a warm checkout depends on its commit.
type Fake struct{}

func hash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (Fake) Image(_ context.Context, req Request) (Image, error) {
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
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	if err := w.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0o644, Size: int64(len(dockerfile))}); err != nil {
		return Image{}, err
	}
	if _, err := w.Write([]byte(dockerfile)); err != nil {
		return Image{}, err
	}
	if err := w.Close(); err != nil {
		return Image{}, err
	}
	img.Context = b.Bytes()
	return img, nil
}

func (Fake) Warm(_ context.Context, _, mirror, commit string) (Warm, error) {
	return Warm{
		ToolchainHash: hash("toolchain", mirror, commit), LockfilesHash: hash("lockfiles", mirror, commit),
		Install: []string{"install-dependencies"},
	}, nil
}
