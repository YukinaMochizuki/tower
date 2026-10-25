//go:build !hostdfake

package main

import (
	"errors"
	"log/slog"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

func driverFlags() {}

// drivers are the host's. The real ones (ZFS, Docker, QEMU, git) need a
// testbed and are not written yet.
func drivers(flags, *slog.Logger) (driver.Drivers, error) {
	return driver.Drivers{}, errors.New("this hostd has no drivers for a real host yet; build it with -tags hostdfake to run on a fake host")
}
