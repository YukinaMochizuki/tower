// Command tower-guest is the program hostd puts in each of its guests and
// runs everything there through (internal/hostd/guestbin). It is built
// static (CGO_ENABLED=0), as it runs in images and VMs of any libc.
package main

import (
	"os"

	"github.com/YukinaMochizuki/tower/internal/hostd/guestbin"
)

func main() { os.Exit(guestbin.Main(os.Args[1:])) }
