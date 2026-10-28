package images

import "testing"

// TestAShortNameIsResolvedOnItsRegistry holds what BuildKit's gateway is
// given to resolve: a reference with its registry named, since it takes a
// short name's first part for a host (https://ardge/v2/... for
// ardge/devcontainer-base, which is on Docker Hub).
func TestAShortNameIsResolvedOnItsRegistry(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string]string{
		"ardge/devcontainer-base:26.10.5": "docker.io/ardge/devcontainer-base:26.10.5",
		"golang:bookworm":                 "docker.io/library/golang:bookworm",
		"ubuntu":                          "docker.io/library/ubuntu:latest",
		"mcr.microsoft.com/devcontainers/base:ubuntu":          "mcr.microsoft.com/devcontainers/base:ubuntu",
		"registry.tower.internal/box@sha256:" + sixtyFourZeros: "registry.tower.internal/box@sha256:" + sixtyFourZeros,
		"localhost:5000/x:1":                                   "localhost:5000/x:1",
		"ghcr.io/acme/ops:1@sha256:" + sixtyFourZeros:          "ghcr.io/acme/ops:1@sha256:" + sixtyFourZeros,
	} {
		got, err := normalizedRef(ref)
		if err != nil {
			t.Errorf("%s: %v", ref, err)
			continue
		}
		if got != want {
			t.Errorf("%s: resolved as %s, want %s", ref, got, want)
		}
	}
	if _, err := normalizedRef("Not A/Ref"); err == nil {
		t.Error("an invalid reference was taken")
	}
}

const sixtyFourZeros = "0000000000000000000000000000000000000000000000000000000000000000"
