package drivertest

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// RunTailnet runs the container driver's tailnet cases: a container that
// joins a tailnet has a TUN device and its tailscaled alone NET_ADMIN, one
// that joins none has neither, and it joins only while its node has no
// state, which it keeps across a restart.
func RunTailnet(t *testing.T, h Harness) {
	tests := []struct {
		name string
		fn   func(*testing.T, Harness)
	}{
		{"OnlyWhenAsked", testTailnetOnlyWhenAsked},
		{"JoinsOnlyWithoutState", testTailnetJoinsOnlyWithoutState},
		{"KeyRejected", testTailnetKeyRejected},
		{"AddedAndRemoved", testTailnetAddedAndRemoved},
		{"NodeForgottenOnRemove", testTailnetNodeForgottenOnRemove},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, h) })
	}
}

// tailnetGuest makes a running container that joins a tailnet as
// hostname, on a disk of its own, until the test ends.
func tailnetGuest(t *testing.T, h Harness, what, hostname string) string {
	t.Helper()
	k := h.containers()
	k.Create = func(t *testing.T, spec driver.GuestSpec) error {
		return h.Drivers.Containers.Create(t.Context(), driver.ContainerSpec{
			GuestSpec: spec, Image: h.Local(t, h.Image), Tailnet: &driver.ContainerTailnet{Hostname: hostname},
		})
	}
	return k.guestWithDisk(t, h, what)
}

// netAdmin is what netadmin says of a container's processes: by name,
// whether any of that name holds NET_ADMIN in its bounding or effective
// set.
func netAdmin(t *testing.T, g driver.Guests, n string) map[string]bool {
	t.Helper()
	code, out := run(t, g, n, nil, "netadmin")
	if code != 0 {
		t.Fatalf("netadmin exited %d: %q", code, out)
	}
	held := map[string]bool{}
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		held[f[0]] = held[f[0]] || f[1] == "1" || f[2] == "1"
	}
	return held
}

func testTailnetOnlyWhenAsked(t *testing.T, h Harness) {
	c := h.Drivers.Containers
	plain := h.containers().guestWithDisk(t, h, "plain")
	if code, _ := run(t, c, plain, nil, "sh", "-c", "test -c /dev/net/tun"); code == 0 {
		t.Error("a container that joins no tailnet has a TUN device")
	}
	for name, held := range netAdmin(t, c, plain) {
		if held {
			t.Errorf("%s holds NET_ADMIN in a container that joins no tailnet", name)
		}
	}
	_, err := c.Tailnet(t.Context(), plain)
	is(t, "the tailnet of a container that joins none", err, driver.ErrNotFound)
	is(t, "a join of a container that joins no tailnet", c.JoinTailnet(t.Context(), plain, "unused"), driver.ErrNotFound)

	joins := tailnetGuest(t, h, "joins", name(t, ""))
	// Its tailscaled runs once it answers.
	tailnetReason(t, c, joins, "NoKey", "Joined")
	if code, _ := run(t, c, joins, nil, "sh", "-c", "test -c /dev/net/tun"); code != 0 {
		t.Error("a container that joins a tailnet has no TUN device")
	}
	held := netAdmin(t, c, joins)
	for name, has := range held {
		if has && name != "tailscaled" {
			t.Errorf("%s holds NET_ADMIN; only tailscaled may", name)
		}
	}
	if !held["tailscaled"] {
		t.Errorf("tailscaled does not run with NET_ADMIN: %v", held)
	}
}

// tailnetReason waits for a container's tailnet to say one of reasons,
// and returns its state.
func tailnetReason(t *testing.T, c driver.Containers, n string, reasons ...string) driver.TailnetState {
	t.Helper()
	var st driver.TailnetState
	// A real tailscaled answers, and a real control server, within
	// seconds; tailscale up gives up after two minutes.
	testutil.EventuallyWithin(t, 3*time.Minute, fmt.Sprintf("its tailnet to say one of %v", reasons), func() (bool, string) {
		var err error
		st, err = c.Tailnet(t.Context(), n)
		mustNot(t, "reading the tailnet's state", err)
		return slices.Contains(reasons, st.Reason), fmt.Sprintf("%+v", st)
	})
	return st
}

func testTailnetJoinsOnlyWithoutState(t *testing.T, h Harness) {
	if h.TailnetKey == "" {
		t.Skip("the harness has no key the tailnet accepts")
	}
	c, ctx := h.Drivers.Containers, t.Context()
	n := tailnetGuest(t, h, "node", name(t, ""))
	tailnetReason(t, c, n, "NoKey")
	mustNot(t, "joining", c.JoinTailnet(ctx, n, h.TailnetKey))
	tailnetReason(t, c, n, "Joined")
	// Its node's state outlives a restart: it comes back as itself, and a
	// key given again, even one the tailnet would refuse, is not used.
	mustNot(t, "stopping it", c.Stop(ctx, n))
	mustNot(t, "starting it again", c.Start(ctx, n))
	tailnetReason(t, c, n, "Joined")
	if h.TailnetRejectedKey != "" {
		mustNot(t, "joining again", c.JoinTailnet(ctx, n, h.TailnetRejectedKey))
		if st := tailnetReason(t, c, n, "Joined", "KeyRejected"); st.Reason != "Joined" {
			t.Errorf("a node with state joined again with another key: %+v", st)
		}
	}
	notInGuest(t, h, c, n, h.TailnetKey)
}

func testTailnetKeyRejected(t *testing.T, h Harness) {
	if h.TailnetRejectedKey == "" {
		t.Skip("the harness has no key the tailnet refuses")
	}
	c := h.Drivers.Containers
	n := tailnetGuest(t, h, "refused", name(t, ""))
	tailnetReason(t, c, n, "NoKey")
	mustNot(t, "joining", c.JoinTailnet(t.Context(), n, h.TailnetRejectedKey))
	st := tailnetReason(t, c, n, "KeyRejected")
	if strings.Contains(st.Message, h.TailnetRejectedKey) {
		t.Errorf("its state quotes the key: %+v", st)
	}
	notInGuest(t, h, c, n, h.TailnetRejectedKey)
}

// hasTun reports whether a running container has a TUN device.
func hasTun(t *testing.T, c driver.Containers, n string) bool {
	t.Helper()
	code, _ := run(t, c, n, nil, "sh", "-c", "test -c /dev/net/tun")
	return code == 0
}

// testTailnetAddedAndRemoved: a tailnet added to a running container makes
// it again, stopped, with its TUN device and its tailscaled alone holding
// NET_ADMIN once it starts, on the same disk; removed, it is made again
// without either. Inspect says the tailnet it joins.
func testTailnetAddedAndRemoved(t *testing.T, h Harness) {
	c, ctx := h.Drivers.Containers, t.Context()
	n := h.containers().guestWithDisk(t, h, "later")
	mustNot(t, "writing its workspace", c.WriteFile(ctx, n, "/workspace/kept", 0o644, 0, strings.NewReader("kept")))
	added := &driver.ContainerTailnet{Hostname: name(t, ""), AcceptRoutes: true}
	mustNot(t, "adding its tailnet", c.SetTailnet(ctx, n, added))
	g, err := c.Inspect(ctx, n)
	mustNot(t, "inspecting it", err)
	if g.Running || g.Tailnet == nil || *g.Tailnet != *added {
		t.Fatalf("after its tailnet was added: running %v, tailnet %+v; want it stopped, joining %+v", g.Running, g.Tailnet, added)
	}
	mustNot(t, "starting it", c.Start(ctx, n))
	tailnetReason(t, c, n, "NoKey", "Joined")
	if !hasTun(t, c, n) {
		t.Error("a container its tailnet was added to has no TUN device")
	}
	for name, has := range netAdmin(t, c, n) {
		if has && name != "tailscaled" {
			t.Errorf("%s holds NET_ADMIN; only tailscaled may", name)
		}
	}
	if got := readFile(t, c, n, "/workspace/kept"); got != "kept" {
		t.Errorf("its workspace holds %q after it was made again", got)
	}

	mustNot(t, "removing its tailnet", c.SetTailnet(ctx, n, nil))
	g, err = c.Inspect(ctx, n)
	mustNot(t, "inspecting it", err)
	if g.Running || g.Tailnet != nil {
		t.Fatalf("after its tailnet was removed: running %v, tailnet %+v", g.Running, g.Tailnet)
	}
	mustNot(t, "starting it", c.Start(ctx, n))
	if hasTun(t, c, n) {
		t.Error("a container its tailnet was removed from has a TUN device")
	}
	for name, has := range netAdmin(t, c, n) {
		if has {
			t.Errorf("%s holds NET_ADMIN after its tailnet was removed", name)
		}
	}
	_, err = c.Tailnet(ctx, n)
	is(t, "the tailnet of a container it was removed from", err, driver.ErrNotFound)
}

// testTailnetNodeForgottenOnRemove: a joined node takes a new hostname as
// it runs, with no restart; its tailnet removed and added again, it has no
// state and waits for a key again.
func testTailnetNodeForgottenOnRemove(t *testing.T, h Harness) {
	if h.TailnetKey == "" {
		t.Skip("the harness has no key the tailnet accepts")
	}
	c, ctx := h.Drivers.Containers, t.Context()
	n := tailnetGuest(t, h, "forgotten", name(t, ""))
	tailnetReason(t, c, n, "NoKey")
	mustNot(t, "joining", c.JoinTailnet(ctx, n, h.TailnetKey))
	tailnetReason(t, c, n, "Joined")
	before, err := c.Inspect(ctx, n)
	mustNot(t, "inspecting it", err)
	renamed := &driver.ContainerTailnet{Hostname: name(t, "renamed")}
	mustNot(t, "setting its hostname", c.SetTailnet(ctx, n, renamed))
	after, err := c.Inspect(ctx, n)
	mustNot(t, "inspecting it", err)
	if !after.Running || !after.StartedAt.Equal(before.StartedAt) || after.Tailnet == nil || *after.Tailnet != *renamed {
		t.Errorf("a new hostname: running %v, started %v (was %v), tailnet %+v; want it set as it runs", after.Running, after.StartedAt, before.StartedAt, after.Tailnet)
	}
	tailnetReason(t, c, n, "Joined")
	mustNot(t, "removing its tailnet", c.SetTailnet(ctx, n, nil))
	mustNot(t, "adding it again", c.SetTailnet(ctx, n, renamed))
	mustNot(t, "starting it", c.Start(ctx, n))
	tailnetReason(t, c, n, "NoKey")
}

// notInGuest fails the test if key is kept anywhere the host keeps what
// its drivers write, or in the container's files outside memory.
func notInGuest(t *testing.T, h Harness, c driver.Containers, n, key string) {
	t.Helper()
	if h.HostFiles != "" {
		notOnHost(t, h, key)
	}
	var out strings.Builder
	_, err := c.Exec(context.WithoutCancel(t.Context()), n, driver.ExecSpec{
		Command: []string{"sh", "-c", "grep -rlF -- \"$1\" /.tower /etc /root /tmp /workspace 2>/dev/null", "sh", key},
		Stdout:  &out, Stderr: io.Discard,
	})
	mustNot(t, "searching the container for the key", err)
	if s := strings.TrimSpace(out.String()); s != "" {
		t.Errorf("the key is kept in the container: %s", s)
	}
}
