//go:build testbed

package drivers

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/test/testbed/internal/testbed"
)

// TestABuildStepReachesNothingOfTheHost: a build step's network is its
// own (deploy/hostd/build-network.nft): it reaches the internet, and
// neither a box on a workstream network nor anything listening on the
// host, at its loopback or any of its addresses. Each is first shown
// reachable from the host, so what the step cannot reach is there.
func TestABuildStepReachesNothingOfTheHost(t *testing.T) {
	h := harness(t)
	ctx := t.Context()

	// A box on a workstream network, echoing on EchoPort.
	const netName, boxName = "dt-buildnet-net", "dt-buildnet-box"
	labels := driver.Labels{driver.LabelOwner: "drivertest", driver.LabelInstance: boxName}
	cs := h.Drivers.Containers
	if err := cs.EnsureNetwork(ctx, netName, labels); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.RemoveNetwork(context.WithoutCancel(ctx), netName) })
	d, err := h.Drivers.Storage.EnsureDisk(ctx, driver.DiskSpec{Name: boxName + "-disk", Kind: driver.Dataset, Size: 1 << 30, Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Drivers.Storage.DestroyDisk(context.WithoutCancel(ctx), boxName+"-disk") })
	spec := driver.GuestSpec{Name: boxName, Disk: d.Path, Network: netName, Alias: "box", CPUs: 1, Memory: 256 << 20, Labels: labels}
	if err := cs.Create(ctx, driver.ContainerSpec{GuestSpec: spec, Image: h.Local(t, h.Image)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Remove(context.WithoutCancel(ctx), boxName) })
	if err := cs.Start(ctx, boxName); err != nil {
		t.Fatal(err)
	}
	g, err := cs.Inspect(ctx, boxName)
	if err != nil {
		t.Fatal(err)
	}
	box := net.JoinHostPort(g.Address, strconv.Itoa(testbed.EchoPort))

	// A listener on every address of the host's.
	l, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	targets := []string{box, net.JoinHostPort("127.0.0.1", port)}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			targets = append(targets, net.JoinHostPort(ipn.IP.String(), port))
		}
	}
	for _, a := range targets {
		c, err := net.DialTimeout("tcp", a, 3*time.Second)
		if err != nil {
			t.Fatalf("%s is not reachable from the host, so the test shows nothing of it: %v", a, err)
		}
		_ = c.Close()
	}

	// The step tries each, and its own gateway (the host's address on its
	// bridge), and the internet; it fails saying what it reached, or what
	// it did not.
	var script strings.Builder
	fmt.Fprintf(&script, "echo %d; fail=0; ", time.Now().UnixNano())
	fmt.Fprintf(&script, "gw=$(ip route | awk '/^default/ {print $3}'); ip -4 addr; ")
	fmt.Fprintf(&script, "for a in %s \"$gw:%s\"; do ", strings.Join(targets, " "), port)
	script.WriteString("if echo hi | nc -w 3 \"${a%:*}\" \"${a##*:}\" >/dev/null 2>&1; then echo \"a build step reached $a\" >&2; fail=1; fi; done; ")
	script.WriteString("wget -q -T 10 -O /dev/null http://deb.debian.org/debian/ || { echo 'a build step reached no internet' >&2; fail=1; }; ")
	script.WriteString("exit $fail")
	dockerfile := "FROM " + h.Image + "\nRUN " + script.String() + "\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	ctxTar, err := testbed.ContextTar(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Drivers.Images.Build(ctx, driver.BuildSpec{Repository: "drivertest/buildnet", Context: bytes.NewReader(ctxTar)}); err != nil {
		t.Errorf("the build step (it tried %v): %v", targets, err)
	}
}
