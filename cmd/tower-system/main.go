// Command tower-system is the program in a hangar target's system
// container (internal/system): `tower-system call` runs one operation,
// named by the header on its stdin, as tower's hangar provider asks it
// through hangar's exec; `tower-system serve` is the server boxes reach
// over the target's EndpointService, which the image starts beside
// buildkitd and tailscaled (deploy/system-image), and which says
// "listening on ADDR" on its stdout once it listens.
//
// It is configured by the container's environment: TOWER_SYSTEM_ROOT, the
// container's disk (default /var/lib/tower); TOWER_REGISTRY, tower's
// registry as the container pushes to it, with TOWER_REGISTRY_PLAIN_HTTP=1
// for one served over plain HTTP; BUILDKIT_HOST, buildkitd's address
// (default its socket); TAILSCALE_SOCKET, tailscaled's;
// TOWER_SYSTEM_LISTEN, where the server serves boxes (default :7480), and
// TOWER_SYSTEM_CONTROL, its control socket, which the calls reach it by.
package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/YukinaMochizuki/tower/internal/system"
)

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "call" && os.Args[1] != "serve") {
		fmt.Fprintln(os.Stderr, "usage: tower-system call|serve")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cfg := configOf(os.Getenv)
	if os.Args[1] == "serve" {
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		err := system.NewServer(cfg, log).Run(ctx, func(addr string) { fmt.Println("listening on " + addr) })
		if err != nil {
			log.Error("tower-system serve", "error", err)
			os.Exit(1)
		}
		return
	}
	os.Exit(system.Serve(ctx, cfg, os.Stdin, os.Stdout, os.Stderr))
}

// configOf is the configuration the environment getenv reads says.
func configOf(getenv func(string) string) system.Config {
	return system.Config{
		Root:              cmp.Or(getenv("TOWER_SYSTEM_ROOT"), "/var/lib/tower"),
		BuildKit:          cmp.Or(getenv("BUILDKIT_HOST"), "unix:///run/buildkit/buildkitd.sock"),
		Registry:          getenv("TOWER_REGISTRY"),
		RegistryPlainHTTP: getenv("TOWER_REGISTRY_PLAIN_HTTP") == "1",
		Tailscale:         []string{"tailscale", "--socket=" + cmp.Or(getenv("TAILSCALE_SOCKET"), "/var/run/tailscale/tailscaled.sock")},
		Listen:            getenv("TOWER_SYSTEM_LISTEN"),
		Control:           getenv("TOWER_SYSTEM_CONTROL"),
	}
}
