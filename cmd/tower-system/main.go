// Command tower-system is the program in a hangar target's system
// container (internal/system): `tower-system call` runs one operation,
// named by the header on its stdin, as tower's hangar provider asks it
// through hangar's exec. The container's image starts buildkitd and
// tailscaled beside it (deploy/system-image).
//
// It is configured by the container's environment: TOWER_SYSTEM_ROOT, the
// container's disk (default /var/lib/tower); TOWER_REGISTRY, tower's
// registry as the container pushes to it; BUILDKIT_HOST, buildkitd's
// address (default its socket); TAILSCALE_SOCKET, tailscaled's.
package main

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/YukinaMochizuki/tower/internal/system"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "call" {
		fmt.Fprintln(os.Stderr, "usage: tower-system call")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cfg := system.Config{
		Root:      cmp.Or(os.Getenv("TOWER_SYSTEM_ROOT"), "/var/lib/tower"),
		BuildKit:  cmp.Or(os.Getenv("BUILDKIT_HOST"), "unix:///run/buildkit/buildkitd.sock"),
		Registry:  os.Getenv("TOWER_REGISTRY"),
		Tailscale: []string{"tailscale", "--socket=" + cmp.Or(os.Getenv("TAILSCALE_SOCKET"), "/var/run/tailscale/tailscaled.sock")},
	}
	os.Exit(system.Serve(ctx, cfg, os.Stdin, os.Stdout, os.Stderr))
}
