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
	// Addons are layered onto the image, by name.
	Addons []string
}

// Image is a resolved box image.
type Image struct {
	// BaseImageDigest, DevcontainerHash and Addons (with their versions)
	// are BoxImageInputs.
	BaseImageDigest  string
	DevcontainerHash string
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
	base := req.Image
	if base == "" {
		base = "devcontainer:" + req.Mirror
	}
	var addons []string
	for _, a := range slices.Sorted(slices.Values(req.Addons)) {
		addons = append(addons, a+"@1")
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
