//go:build testbed

package drivers

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/docker"
	"github.com/YukinaMochizuki/tower/test/testbed/internal/testbed"
)

// TestAGuestReachesNothingOfTheHost: a box's network is fenced
// (deploy/hostd/guest-network.nft, which install-deps.sh loads): its
// bridge is named for the fence, and a box reaches the internet, by the
// public DNS it is given, and a peer on its network by alias, and nothing
// listening on the host, at any of its addresses, nor its bridge's
// gateway. Each host address is first shown reachable from the host, so
// what the box cannot reach is there.
func TestAGuestReachesNothingOfTheHost(t *testing.T) {
	h := harness(t)
	ctx := t.Context()
	cs := h.Drivers.Containers

	const netName = "dt-guestnet-net"
	labels := func(n string) driver.Labels {
		return driver.Labels{driver.LabelOwner: "drivertest", driver.LabelInstance: n}
	}
	if err := cs.EnsureNetwork(ctx, netName, labels(netName)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.RemoveNetwork(context.WithoutCancel(ctx), netName) })
	if out, err := exec.Command("ip", "link", "show", docker.BridgeName(netName)).CombinedOutput(); err != nil {
		t.Fatalf("the network's bridge is not %s, which the fence matches: %v: %s", docker.BridgeName(netName), err, out)
	}
	if err := h.Drivers.Images.Pull(ctx, h.Image); err != nil {
		t.Fatal(err)
	}
	box := func(name, alias string) {
		d, err := h.Drivers.Storage.EnsureDisk(ctx, driver.DiskSpec{Name: name + "-disk", Kind: driver.Dataset, Size: 1 << 30, Labels: labels(name)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Drivers.Storage.DestroyDisk(context.WithoutCancel(ctx), name+"-disk") })
		spec := driver.GuestSpec{Name: name, Disk: d.Path, Network: netName, Alias: alias, CPUs: 1, Memory: 256 << 20, Labels: labels(name)}
		if err := cs.Create(ctx, driver.ContainerSpec{GuestSpec: spec, Image: h.Image}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cs.Remove(context.WithoutCancel(ctx), name) })
		if err := cs.Start(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	box("dt-guestnet-box", "box")
	box("dt-guestnet-peer", "peer")

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
	var targets []string
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

	// The box tries each, and its gateway, and fails saying what it
	// reached; then its peer and the internet, failing saying what it did
	// not.
	var script strings.Builder
	script.WriteString("fail=0; gw=$(ip route | awk '/^default/ {print $3}'); ")
	fmt.Fprintf(&script, "for a in %s \"$gw:%s\"; do ", strings.Join(targets, " "), port)
	script.WriteString("if echo hi | nc -w 3 \"${a%:*}\" \"${a##*:}\" >/dev/null 2>&1; then echo \"a guest reached $a\" >&2; fail=1; fi; done; ")
	fmt.Fprintf(&script, "echo hi | nc -w 3 peer %d >/dev/null 2>&1 || { echo 'a guest did not reach its peer' >&2; fail=1; }; ", testbed.EchoPort)
	script.WriteString("wget -q -T 10 -O /dev/null http://deb.debian.org/debian/ || { echo 'a guest reached no internet' >&2; fail=1; }; ")
	script.WriteString("exit $fail")
	var stderr bytes.Buffer
	code, err := cs.Exec(ctx, "dt-guestnet-box", driver.ExecSpec{Command: []string{"sh", "-c", script.String()}, Stderr: &stderr})
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Errorf("the box (it tried %v): %s", targets, stderr.String())
	}
}
