// Command tower-guest is the program hostd puts in each of its guests and
// runs everything there through (internal/hostd/guestbin), and that runs
// each VM as its container's first process (tower-guest vm,
// internal/hostd/driver/chvm/launcher). It is built static
// (CGO_ENABLED=0), as it runs in images and VMs of any libc.
package main

import (
	"os"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver/chvm/launcher"
	"github.com/YukinaMochizuki/tower/internal/hostd/guestbin"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "vm" {
		os.Exit(launcher.Main(os.Args[2:]))
	}
	os.Exit(guestbin.Main(os.Args[1:]))
}
