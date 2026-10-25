package apiserver_test

import (
	"errors"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/hostdtest"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/providertest"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// TestHostdConformanceThroughTower holds tower's own provider of a hostd
// target, a real hostd over the fake host behind the tunnel it opens to
// this apiserver, to the provider contract: the seam a host's real drivers
// plug into.
func TestHostdConformanceThroughTower(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
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
