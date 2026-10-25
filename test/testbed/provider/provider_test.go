//go:build testbed

// Package provider runs the provider contract (internal/provider/
// providertest) against hostd over the testbed's real drivers: in process,
// and through the tunnel hostd opens to tower, as tower's own provider of
// a hostd target reaches it (docs/runbook/testbed.md). The fake runs the
// same contract in CI.
package provider

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/hostdtest"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
	"github.com/YukinaMochizuki/tower/test/testbed/internal/testbed"
)

var env testbed.Env

func TestMain(m *testing.M) {
	var err error
	env, err = testbed.New("provider")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Sweep(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// world is tower with a hostd target, whose hostd runs in the test over
// the testbed's real drivers.
func world(t *testing.T) *hostdtest.World {
	t.Helper()
	ctx := t.Context()
	d, err := env.Drivers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	image, err := testbed.DrivertestImage(ctx, d.Images)
	if err != nil {
		t.Fatalf("the drivertest image: %v", err)
	}
	// A Dockerfile at the remote's head, building from the image with the
	// repository's root as its context.
	remote, head, err := env.RemoteWith(map[string]string{"docker/Dockerfile": "FROM " + image + "\nCOPY go.mod /go.mod\n"})
	if err != nil {
		t.Fatal(err)
	}
	return hostdtest.New(t, hostdtest.Options{Real: &hostdtest.Real{
		Drivers: d, Image: image, BaseImage: "drivertest",
		Remote: remote, Branch: "main", RemoteHead: head, RemoteDockerfile: "docker/Dockerfile",
	}})
}

// TestConformance holds hostd itself, over the real drivers, to the
// provider contract.
func TestConformance(t *testing.T) {
	w := world(t)
	providertest.Run(t, w.Harness(w.Host))
}

// TestConformanceThroughATunnel holds tower's provider of a hostd target
// to the contract: hostd over the real drivers, behind the tunnel it opens
// to tower.
func TestConformanceThroughATunnel(t *testing.T) {
	w := world(t)
	w.Tunnel(t)
	p, err := w.Env.Server.ProviderFor(hostdtest.Target)
	if err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the host answering over its tunnel", func() bool {
		// No such mirror, says the host once it answers.
		_, err := p.CommitsBehind(t.Context(), "never-fetched", "c1", "main")
		return errors.Is(err, provider.ErrNotFound)
	})
	providertest.Run(t, w.Harness(p))
}
