//go:build testbed

package drivers

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/test/testbed/internal/testbed"
)

// TestAnImageRunsWhereItWasBuiltWithTheRegistryDown: with the registry
// stopped from before the build to after the container runs, an image is
// built (BuildKit's OCI layout, loaded into Docker), another is built FROM
// it, and a container of the second is made and runs, all from the host
// alone (docs/decisions/images.md). Its push fails and keeps it held;
// once the registry is back the push puts the digest it was built as
// there.
func TestAnImageRunsWhereItWasBuiltWithTheRegistryDown(t *testing.T) {
	h := harness(t)
	ctx := t.Context()
	im := h.Drivers.Images

	start, err := testbed.StopRegistry()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := start(); err != nil {
			t.Error(err)
		}
	})

	stamp := time.Now().Format("20060102150405.000000")
	buildOf := func(dockerfile string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
			t.Fatal(err)
		}
		ctxTar, err := testbed.ContextTar(dir)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := im.Build(ctx, driver.BuildSpec{Repository: "testbed/localfirst", Context: bytes.NewReader(ctxTar)})
		if err != nil {
			t.Fatalf("building with the registry down: %v", err)
		}
		return ref
	}
	// FROM busybox of Docker Hub, which BuildKit reaches; never tower's
	// registry.
	base := buildOf("FROM " + testbed.BusyBox + "\nLABEL localfirst=" + stamp + "\nCMD [\"sleep\", \"3600\"]\n")
	layer := buildOf("FROM " + base + "\nLABEL localfirst-layer=" + stamp + "\n")

	left, err := im.Unpushed(ctx)
	if err != nil || !slices.Contains(left, base) || !slices.Contains(left, layer) {
		t.Fatalf("left to push: %v, %v; want %s and %s", left, err, base, layer)
	}
	if err := im.Pull(ctx, layer); err != nil {
		t.Errorf("pulling what was built here, with the registry down: %v", err)
	}
	if cfg, err := im.Resolve(ctx, layer); err != nil || !strings.HasSuffix(layer, "@"+cfg.Digest) {
		t.Errorf("resolving %s with the registry down: %+v, %v", layer, cfg, err)
	}

	const name = "dt-localfirst"
	labels := driver.Labels{driver.LabelOwner: "drivertest", driver.LabelInstance: name}
	d, err := h.Drivers.Storage.EnsureDisk(ctx, driver.DiskSpec{Name: name + "-disk", Kind: driver.Dataset, Size: 1 << 30, Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Drivers.Storage.DestroyDisk(context.WithoutCancel(ctx), name+"-disk") })
	cs := h.Drivers.Containers
	spec := driver.GuestSpec{Name: name, Disk: d.Path, CPUs: 1, Memory: 256 << 20, Labels: labels}
	if err := cs.Create(ctx, driver.ContainerSpec{GuestSpec: spec, Image: h.Local(t, layer)}); err != nil {
		t.Fatalf("making a container of %s with the registry down: %v", layer, err)
	}
	t.Cleanup(func() { _ = cs.Remove(context.WithoutCancel(ctx), name) })
	if err := cs.Start(ctx, name); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := cs.Exec(ctx, name, driver.ExecSpec{Command: []string{"true"}, Stdout: &out, Stderr: &out})
	if err != nil || code != 0 {
		t.Fatalf("a command in the container: %d, %v: %s", code, err, out.String())
	}

	if err := im.Push(ctx, layer); err == nil {
		t.Fatal("a push to a registry that is down succeeded")
	}
	if left, _ := im.Unpushed(ctx); !slices.Contains(left, layer) {
		t.Fatalf("an image whose push failed is not left to push: %v", left)
	}

	if err := start(); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{base, layer} {
		var perr error
		for range 30 { // zot takes a moment to listen again
			if perr = im.Push(ctx, ref); perr == nil {
				break
			}
			time.Sleep(time.Second)
		}
		if perr != nil {
			t.Fatalf("pushing %s once the registry is back: %v", ref, perr)
		}
		cfg, err := im.Resolve(ctx, ref)
		if err != nil || !strings.HasSuffix(ref, "@"+cfg.Digest) {
			t.Errorf("the registry has %s as %+v, %v; want the digest it was built as", ref, cfg, err)
		}
	}
	if left, _ := im.Unpushed(ctx); slices.Contains(left, base) || slices.Contains(left, layer) {
		t.Errorf("left to push after the pushes: %v", left)
	}
}
