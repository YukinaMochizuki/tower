//go:build !hostdfake

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/chvm"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/docker"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/gitmirror"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/hostinfo"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/images"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/zfsstore"
)

// The host's drivers' flags, as docs/runbook/deploy.md installs them.
var df struct {
	dataset, dockerHost, guestBinary         string
	cloudHypervisor, firmware, baseImages    string
	buildkit, registry, registryUser, pwFile string
	guestDNS                                 string
}

func driverFlags() {
	flag.StringVar(&df.dataset, "zfs-dataset", "tower/hostd", "the ZFS dataset the disks, goldens and mirrors are made in; it must exist")
	flag.StringVar(&df.dockerHost, "docker-host", "", "Docker's address; empty is Docker's default")
	flag.StringVar(&df.guestBinary, "guest-binary", "/usr/local/lib/tower/tower-guest", "tower-guest, statically linked, which runs in every guest")
	flag.StringVar(&df.cloudHypervisor, "cloud-hypervisor", "/usr/local/bin/cloud-hypervisor", "Cloud Hypervisor, statically linked")
	flag.StringVar(&df.firmware, "vm-firmware", "/usr/local/share/tower/CLOUDHV.fd", "the EDK2 firmware VMs boot through")
	flag.StringVar(&df.baseImages, "base-images", "/var/lib/tower-hostd/base-images", "the base images VMs boot, NAME.raw each (deploy/hostd/base-image.sh)")
	flag.StringVar(&df.buildkit, "buildkit", "unix:///run/buildkit/buildkitd.sock", "buildkitd's address")
	flag.StringVar(&df.registry, "registry", "", "tower's registry, host[:port], which images are pulled from and pushed to")
	flag.StringVar(&df.registryUser, "registry-user", "", "the registry user hostd signs in as; empty is anonymous")
	flag.StringVar(&df.pwFile, "registry-password-file", "", "file holding the registry user's password, only its owner may read it")
	flag.StringVar(&df.guestDNS, "guest-dns", "1.1.1.1,9.9.9.9", "the DNS servers every guest resolves through, comma-separated public addresses: the host's firewall drops what a guest sends to a private one (deploy/hostd/guest-network.nft)")
}

// parseDNS reads --guest-dns: one or more addresses, none of them private,
// loopback or link-local, which the guests' firewall would drop.
func parseDNS(s string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, f := range strings.Split(s, ",") {
		a, err := netip.ParseAddr(strings.TrimSpace(f))
		if err != nil {
			return nil, fmt.Errorf("--guest-dns: %w", err)
		}
		if a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() || netip.MustParsePrefix("100.64.0.0/10").Contains(a) {
			return nil, fmt.Errorf("--guest-dns: %s is not a public address, which a guest could not reach", a)
		}
		out = append(out, a)
	}
	return out, nil
}

// drivers are the host's: ZFS, Docker, Cloud Hypervisor, BuildKit and the
// registry, git.
func drivers(f flags, log *slog.Logger) (driver.Drivers, error) {
	ctx := context.Background()
	if df.registry == "" {
		return driver.Drivers{}, errors.New("--registry is required")
	}
	dns, err := parseDNS(df.guestDNS)
	if err != nil {
		return driver.Drivers{}, err
	}
	password := ""
	if df.pwFile != "" {
		var err error
		if password, err = readSecret(df.pwFile); err != nil {
			return driver.Drivers{}, fmt.Errorf("--registry-password-file: %w", err)
		}
	}
	cli, err := docker.Dial(df.dockerHost)
	if err != nil {
		return driver.Drivers{}, fmt.Errorf("docker: %w", err)
	}
	storage, err := zfsstore.New(ctx, zfsstore.Config{Dataset: df.dataset, MountRoot: filepath.Join(f.stateDir, "disks"), Log: log})
	if err != nil {
		return driver.Drivers{}, fmt.Errorf("storage: %w", err)
	}
	containers, err := docker.New(ctx, docker.Config{Client: cli, StateDir: f.stateDir, GuestBinary: df.guestBinary, DNS: dns})
	if err != nil {
		return driver.Drivers{}, fmt.Errorf("containers: %w", err)
	}
	vms, err := chvm.New(ctx, chvm.Config{
		Docker: cli, StateDir: f.stateDir, GuestBinary: df.guestBinary,
		CloudHypervisor: df.cloudHypervisor, Firmware: df.firmware, BaseImages: df.baseImages,
		DNS: dns,
	})
	if err != nil {
		return driver.Drivers{}, fmt.Errorf("VMs: %w", err)
	}
	imgs, err := images.New(images.Config{
		Docker: cli, BuildKit: df.buildkit, Registry: df.registry, Username: df.registryUser, Password: password,
		ScratchDir:  filepath.Join(f.stateDir, "scratch", "builds"),
		UnpushedDir: filepath.Join(f.stateDir, "images", "unpushed"),
	})
	if err != nil {
		return driver.Drivers{}, fmt.Errorf("images: %w", err)
	}
	git, err := gitmirror.New(ctx, gitmirror.Config{
		Dataset: df.dataset + "/mirrors", MountRoot: filepath.Join(f.stateDir, "mirrors"),
		ScratchDir: filepath.Join(f.stateDir, "scratch", "git"),
	})
	if err != nil {
		return driver.Drivers{}, fmt.Errorf("git: %w", err)
	}
	return driver.Drivers{Storage: storage, Containers: containers, VMs: vms, Images: imgs, Git: git, Host: hostinfo.Host{}}, nil
}
