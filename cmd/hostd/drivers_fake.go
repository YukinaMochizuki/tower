//go:build hostdfake

package main

import (
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/fake"
)

var fakeImages string

func driverFlags() {
	flag.StringVar(&fakeImages, "fake-images", "", "references by digest the fake registry holds from the start, comma-separated")
}

// drivers are a fake host's, kept in fake-host.json in the state
// directory: a build for tests and the demo, never a release.
func drivers(f flags, log *slog.Logger) (driver.Drivers, error) {
	if err := os.MkdirAll(f.stateDir, 0o700); err != nil {
		return driver.Drivers{}, err
	}
	path := filepath.Join(f.stateDir, "fake-host.json")
	var images []string
	if fakeImages != "" {
		images = strings.Split(fakeImages, ",")
	}
	h, err := fake.New(fake.Options{Path: path, Images: images})
	if err != nil {
		return driver.Drivers{}, err
	}
	log.Warn("running on a fake host: nothing on this machine is touched", "state", path)
	return h.Drivers(), nil
}
