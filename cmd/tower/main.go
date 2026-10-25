// Command tower is tower's CLI (internal/cli).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/YukinaMochizuki/tower/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], cli.IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Env: os.Getenv}, cli.Options{})
	stop()
	os.Exit(code)
}
