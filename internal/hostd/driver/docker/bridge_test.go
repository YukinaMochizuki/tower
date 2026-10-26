package docker_test

import (
	"strings"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver/docker"
)

// TestABridgeIsNamedForTheFirewall: a workstream network's bridge is
// named with BridgePrefix, which deploy/hostd/guest-network.nft matches,
// within the 15 bytes Linux allows an interface name, the same for the
// same network and apart for two.
func TestABridgeIsNamedForTheFirewall(t *testing.T) {
	long := strings.Repeat("w", 200)
	for _, n := range []string{"ws-a", long} {
		b := docker.BridgeName(n)
		if !strings.HasPrefix(b, docker.BridgePrefix) {
			t.Errorf("%q's bridge is %q, without %q", n, b, docker.BridgePrefix)
		}
		if len(b) > 15 {
			t.Errorf("%q's bridge %q is %d bytes, more than an interface name holds", n, b, len(b))
		}
		if docker.BridgeName(n) != b {
			t.Errorf("%q's bridge is named two ways", n)
		}
	}
	if docker.BridgeName("ws-a") == docker.BridgeName("ws-b") {
		t.Error("two networks have one bridge name")
	}
	if strings.HasPrefix("tower-build0", docker.BridgePrefix) {
		t.Error("the build steps' bridge would be taken for a guest's")
	}
}
