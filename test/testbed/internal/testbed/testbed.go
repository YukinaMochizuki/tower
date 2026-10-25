//go:build testbed

// Package testbed is what the real-host tests share (docs/runbook/
// testbed.md): hostd's real drivers on the testbed, made over a dataset
// and directories of a test package's own, swept as it starts and ends,
// and the image drivertest's guests run, built and pushed to the
// testbed's registry. Not tests itself.
package testbed

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/docker"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/images"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/zfsstore"
)

// Pool is the testbed's ZFS pool (test/testbed/setup.sh).
const Pool = "tower"

// BusyBox is the base of the drivertest image, pinned.
const BusyBox = "docker.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// EchoPort is where the drivertest image's guests echo what they read.
const EchoPort = 7777

// Repo is the checkout's root.
func Repo() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../../../.."))
}

// Registry is the testbed's registry and its user.
type Registry struct{ Addr, User, Password string }

// ReadRegistry reads the testbed's registry and its user.
func ReadRegistry() (Registry, error) {
	b, err := os.ReadFile("/etc/tower/testbed-registry.env")
	if err != nil {
		return Registry{}, fmt.Errorf("the testbed's registry (test/testbed/setup.sh): %w", err)
	}
	env := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			env[k] = v
		}
	}
	return Registry{env["TOWER_REGISTRY"], env["TOWER_REGISTRY_USER"], env["TOWER_REGISTRY_PASSWORD"]}, nil
}

// Env is one test package's place on the testbed: its dataset, its state
// directory and its disks' mount root, all named for it.
type Env struct {
	Name, Dataset, Dir string
}

// StateDir is hostd's state directory in it.
func (e Env) StateDir() string { return filepath.Join(e.Dir, "state") }

// GuestBinary is tower-guest, built static into it.
func (e Env) GuestBinary() string { return filepath.Join(e.Dir, "bin", "tower-guest") }

// New sweeps what an earlier run of the package left, makes its place,
// and builds tower-guest there.
func New(name string) (Env, error) {
	e := Env{Name: name, Dataset: Pool + "/testbed-" + name, Dir: "/var/tmp/tower-testbed-" + name}
	if err := e.Sweep(); err != nil {
		return e, err
	}
	if out, err := exec.Command("zfs", "create", "-o", "mountpoint=none", e.Dataset).CombinedOutput(); err != nil {
		return e, fmt.Errorf("zfs create %s: %w: %s", e.Dataset, err, out)
	}
	if err := os.MkdirAll(filepath.Dir(e.GuestBinary()), 0o755); err != nil {
		return e, err
	}
	build := exec.Command("go", "build", "-o", e.GuestBinary(), "./cmd/tower-guest")
	build.Dir = Repo()
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return e, fmt.Errorf("building tower-guest: %w: %s", err, out)
	}
	return e, nil
}

// Sweep removes the package's containers, networks, dataset and
// directory, whatever a run killed outright left.
func (e Env) Sweep() error {
	label := "tower.hangarlab.cloud/instance"
	// drivertest's labels name the test; every one of them starts dt-.
	out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label="+docker.LabelDriver).Output()
	for _, id := range strings.Fields(string(out)) {
		l, _ := exec.Command("docker", "inspect", "--format", "{{index .Config.Labels \""+label+"\"}}", id).Output()
		if strings.HasPrefix(strings.TrimSpace(string(l)), "dt-") || strings.HasPrefix(strings.TrimSpace(string(l)), e.Name) {
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
	}
	out, _ = exec.Command("docker", "network", "ls", "-q", "--filter", "label="+docker.LabelDriver+"=network").Output()
	for _, id := range strings.Fields(string(out)) {
		n, _ := exec.Command("docker", "network", "inspect", "--format", "{{.Name}}", id).Output()
		if strings.HasPrefix(strings.TrimSpace(string(n)), "dt-") {
			_ = exec.Command("docker", "network", "rm", id).Run()
		}
	}
	if exec.Command("zfs", "list", e.Dataset).Run() == nil {
		if out, err := exec.Command("zfs", "destroy", "-R", e.Dataset).CombinedOutput(); err != nil {
			return fmt.Errorf("zfs destroy -R %s: %w: %s", e.Dataset, err, out)
		}
	}
	return os.RemoveAll(e.Dir)
}

// Drivers opens hostd's real drivers over the place, but VMs and git.
func (e Env) Drivers(ctx context.Context) (driver.Drivers, error) {
	cli, err := docker.Dial("")
	if err != nil {
		return driver.Drivers{}, err
	}
	storage, err := zfsstore.New(ctx, zfsstore.Config{Dataset: e.Dataset, MountRoot: filepath.Join(e.Dir, "disks")})
	if err != nil {
		return driver.Drivers{}, err
	}
	containers, err := docker.New(ctx, docker.Config{Client: cli, StateDir: e.StateDir(), GuestBinary: e.GuestBinary()})
	if err != nil {
		return driver.Drivers{}, err
	}
	reg, err := ReadRegistry()
	if err != nil {
		return driver.Drivers{}, err
	}
	imgs, err := images.New(images.Config{
		Docker: cli, BuildKit: "unix:///run/buildkit/buildkitd.sock", Registry: reg.Addr, Username: reg.User, Password: reg.Password,
		ScratchDir: filepath.Join(e.StateDir(), "scratch"),
	})
	if err != nil {
		return driver.Drivers{}, err
	}
	return driver.Drivers{Storage: storage, Containers: containers, Images: imgs}, nil
}

// DrivertestImage builds the image drivertest's guests run, with
// test/testbed/guestbin on its PATH and an echo on EchoPort, pushes it to
// the testbed's registry, and returns its reference by digest.
func DrivertestImage(ctx context.Context, im driver.Images) (string, error) {
	dir, err := os.MkdirTemp("", "drivertest-image-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.CopyFS(filepath.Join(dir, "guestbin"), os.DirFS(filepath.Join(Repo(), "test/testbed/guestbin"))); err != nil {
		return "", err
	}
	dockerfile := "FROM " + BusyBox + "\nCOPY --chmod=0755 guestbin/ /usr/local/bin/\n" +
		fmt.Sprintf("CMD [\"nc\", \"-lk\", \"-p\", \"%d\", \"-e\", \"/bin/cat\"]\n", EchoPort)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return "", err
	}
	ctxTar, err := ContextTar(dir)
	if err != nil {
		return "", err
	}
	return im.Build(ctx, "testbed/drivertest", bytes.NewReader(ctxTar))
}

// ContextTar is a tar of what dir holds, its names relative to dir: a
// build context.
func ContextTar(dir string) ([]byte, error) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil || !info.Mode().IsRegular() {
			return err
		}
		data, err := os.ReadFile(p)
		if err == nil {
			_, err = tw.Write(data)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	err = tw.Close()
	return b.Bytes(), err
}
