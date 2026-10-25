//go:build testbed

// Package drivers runs the drivers' conformance suite
// (internal/hostd/driver/drivertest) against hostd's real drivers on the
// testbed: ZFS on its pool, Docker, Cloud Hypervisor, BuildKit and its
// registry, git (docs/runbook/testbed.md).
package drivers

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/drivertest"
	"github.com/YukinaMochizuki/tower/test/testbed/internal/testbed"
)

var env testbed.Env

func TestMain(m *testing.M) {
	var err error
	env, err = testbed.New("drivers")
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

// harness is the suite's harness over the real drivers.
func harness(t *testing.T) drivertest.Harness {
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
	reg, err := testbed.ReadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	remote, head, err := env.Remote()
	if err != nil {
		t.Fatal(err)
	}
	return drivertest.Harness{
		Drivers: d, Image: image, Remote: remote, Branch: "main", RemoteHead: head,
		MissingImage: reg.Addr + "/testbed/never-pushed@sha256:0000000000000000000000000000000000000000000000000000000000000000",
		BaseImage:    "drivertest", EchoPort: testbed.EchoPort,
		Reopen: func(t *testing.T) driver.Drivers {
			again, err := env.Drivers(context.WithoutCancel(t.Context()))
			if err != nil {
				t.Fatal(err)
			}
			return again
		},
		HostFiles: env.StateDir(),
	}
}

func TestStorage(t *testing.T) { drivertest.RunStorage(t, harness(t)) }

func TestImages(t *testing.T) { drivertest.RunImages(t, harness(t)) }

func TestContainers(t *testing.T) {
	h := harness(t)
	drivertest.RunGuests(t, h, drivertest.ContainersOf(h))
}

func TestContainerProcesses(t *testing.T) { drivertest.RunProcesses(t, harness(t)) }

func TestVMs(t *testing.T) {
	h := harness(t)
	drivertest.RunGuests(t, h, drivertest.VMsOf(h))
}

func TestVMProcesses(t *testing.T) { drivertest.RunVMProcesses(t, harness(t)) }

func TestGit(t *testing.T) { drivertest.RunGit(t, harness(t)) }
