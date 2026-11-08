package docker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// TestAGuestsStatusIsReadSafely: the status file a box writes is read as
// a regular file of its own directory alone: a link, out of it or in it,
// a FIFO or a file too large is never followed, waited on or read whole,
// and says it cannot be read.
func TestAGuestsStatusIsReadSafely(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "host.json")
	if err := os.WriteFile(outside, []byte(`{"reason":"Joined","message":"from the host"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, plant := range map[string]func(dir, p string) error{
		"a link out": func(_, p string) error { return os.Symlink(outside, p) },
		"a link in": func(dir, p string) error {
			_ = os.WriteFile(filepath.Join(dir, "x"), []byte(`{"reason":"Joined"}`), 0o600)
			return os.Symlink("x", p)
		},
		"a device": func(_, p string) error { return os.Symlink("/dev/zero", p) },
		"a FIFO":   func(_, p string) error { return syscall.Mkfifo(p, 0o600) },
		"a large file": func(_, p string) error {
			return os.WriteFile(p, []byte(`{"reason":"Joined","message":"`+strings.Repeat("x", maxTailnetStatus)+`"}`), 0o600)
		},
	} {
		dir := t.TempDir()
		if err := plant(dir, filepath.Join(dir, "status.json")); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		st := tailnetStatusIn(context.Background(), dir)
		if st.Reason != "Failed" || time.Since(start) > guestReadTimeout {
			t.Errorf("%s: %+v in %v; want it refused at once", name, st, time.Since(start))
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"reason":"Joined","message":"joined"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := tailnetStatusIn(context.Background(), dir); st.Reason != "Joined" {
		t.Errorf("a status tower-guest wrote: %+v", st)
	}
	if st := tailnetStatusIn(context.Background(), t.TempDir()); st.Reason != "Starting" {
		t.Errorf("no status: %+v", st)
	}
}

// TestNetAdminOnlyWhenAsked: only a container that joins a tailnet is
// made with NET_ADMIN, the TUN device, tailscale and its node's directory.
func TestNetAdminOnlyWhenAsked(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: "/var/lib/tower-hostd", GuestBinary: "/g", Tailscaled: "/ts/tailscaled", Tailscale: "/ts/tailscale"}}
	plain := d.hostConfig(driver.ContainerSpec{GuestSpec: driver.GuestSpec{Name: "c"}}, "net", "/procs")
	if len(plain.CapAdd) != 0 || len(plain.Devices) != 0 || len(plain.Mounts) != 3 {
		t.Errorf("a container that joins no tailnet: caps %v, devices %v, mounts %v", plain.CapAdd, plain.Devices, plain.Mounts)
	}
	if !slices.Contains(plain.CapDrop, "NET_RAW") || !slices.Contains(plain.SecurityOpt, "no-new-privileges") {
		t.Errorf("a container keeps NET_RAW or may gain privileges: %v %v", plain.CapDrop, plain.SecurityOpt)
	}
	joins := d.hostConfig(driver.ContainerSpec{GuestSpec: driver.GuestSpec{Name: "c"}, Tailnet: &driver.ContainerTailnet{Hostname: "h"}}, "net", "/procs")
	if !slices.Equal(joins.CapAdd, []string{"NET_ADMIN"}) || len(joins.Devices) != 1 || joins.Devices[0].PathOnHost != "/dev/net/tun" {
		t.Errorf("a container that joins a tailnet: caps %v, devices %v", joins.CapAdd, joins.Devices)
	}
	var targets []string
	for _, m := range joins.Mounts {
		targets = append(targets, m.Target)
		if (m.Target == guest.Tailscaled || m.Target == guest.Tailscale) && !m.ReadOnly {
			t.Errorf("%s is bound writable", m.Target)
		}
	}
	for _, m := range joins.Mounts {
		if m.Target == guest.TailnetControlDir && (!m.ReadOnly || m.Source == d.tailnetDir("c")) {
			t.Errorf("the box may write its driver's control directory: %+v", m)
		}
	}
	for _, want := range []string{guest.Tailscaled, guest.Tailscale, guest.TailnetDir, guest.TailnetControlDir} {
		if !slices.Contains(targets, want) {
			t.Errorf("no %s among %v", want, targets)
		}
	}
	if !slices.Contains(joins.SecurityOpt, "no-new-privileges") {
		t.Error("a container that joins a tailnet may gain privileges")
	}
}

// TestATailnetContainersShimsRunDropped: in a container that joins a
// tailnet, every shim is exec'd as root, told whom to become, so it drops
// NET_ADMIN first; in any other, as the image's user, told nothing.
func TestATailnetContainersShimsRunDropped(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir()}}
	c := container.InspectResponse{Config: &container.Config{User: "vscode", Env: []string{"PATH=/bin"}}}
	r := d.ops("c", spec{Tailnet: &driver.ContainerTailnet{Hostname: "h"}, User: "vscode"}, c).R.(runner)
	if r.user != "0" || !slices.Contains(r.env, guest.RunAsEnv+"=vscode") || !slices.Contains(r.env, guest.RunAsHomeEnv+"=1") {
		t.Errorf("a tailnet container's shim runs as %q with %v", r.user, r.env)
	}
	plain := d.ops("c", spec{}, c).R.(runner)
	if plain.user != "" || len(plain.env) > 0 {
		t.Errorf("a plain container's shim runs as %q with %v", plain.user, plain.env)
	}
}

// nodeLog records what a change of a container's tailnet does.
type nodeLog struct {
	did   []string
	state bool
}

func (n *nodeLog) hasState() bool { return n.state }
func (n *nodeLog) leaveStopped(context.Context) error {
	n.did = append(n.did, "leave-stopped")
	return nil
}
func (n *nodeLog) down(context.Context) error { n.did = append(n.did, "down"); return nil }
func (n *nodeLog) set(_ context.Context, t *driver.ContainerTailnet) error {
	n.did = append(n.did, "set "+t.Hostname)
	return nil
}
func (n *nodeLog) stop(context.Context) error   { n.did = append(n.did, "stop"); return nil }
func (n *nodeLog) remove(context.Context) error { n.did = append(n.did, "remove"); return nil }
func (n *nodeLog) forget() error                { n.did = append(n.did, "forget"); return nil }
func (n *nodeLog) create(_ context.Context, cs driver.ContainerSpec) error {
	n.did = append(n.did, fmt.Sprintf("create %v", cs.Tailnet != nil))
	return nil
}

// TestATailnetChangeDoesWhatItMust: joining makes the container again
// with its node forgotten; leaving logs its node out first, a stopped
// one's in leave mode, and only if it has state, then makes it again and
// forgets the node; a new hostname is set on a running node alone.
func TestATailnetChangeDoesWhatItMust(t *testing.T) {
	a, b := &driver.ContainerTailnet{Hostname: "a"}, &driver.ContainerTailnet{Hostname: "b"}
	for _, tt := range []struct {
		name       string
		have, want *driver.ContainerTailnet
		running    bool
		state      bool
		did        []string
	}{
		{"joins", nil, a, true, false, []string{"stop", "remove", "forget", "create true"}},
		{"leaves running", a, nil, true, true, []string{"down", "stop", "remove", "forget", "create false"}},
		{"leaves stopped", a, nil, false, true, []string{"leave-stopped", "stop", "remove", "forget", "create false"}},
		{"leaves stopped with no state", a, nil, false, false, []string{"stop", "remove", "forget", "create false"}},
		{"renamed running", a, b, true, true, []string{"set b"}},
		{"renamed stopped", a, b, false, true, nil},
		{"unchanged", a, a, true, true, nil},
		{"none", nil, nil, true, false, nil},
	} {
		n := &nodeLog{state: tt.state}
		if err := changeTailnet(context.Background(), n, tt.have, tt.want, tt.running, driver.ContainerSpec{Tailnet: tt.want}); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if !slices.Equal(n.did, tt.did) {
			t.Errorf("%s did %v, want %v", tt.name, n.did, tt.did)
		}
	}
}

// TestADeletedContainersNodeLogsOut: a container that joins a tailnet
// logs its node out as it goes, a stopped one's in leave mode, and only
// if it has state, and what the host keeps of its node goes with it; one
// that joins none just goes.
func TestADeletedContainersNodeLogsOut(t *testing.T) {
	for _, tt := range []struct {
		joins, running, state bool
		did                   []string
	}{
		{true, true, true, []string{"down", "remove", "forget"}},
		{true, false, true, []string{"leave-stopped", "remove", "forget"}},
		{true, false, false, []string{"remove", "forget"}},
		{false, true, false, []string{"remove", "forget"}},
	} {
		n := &nodeLog{state: tt.state}
		if err := retire(context.Background(), n, tt.joins, tt.running); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(n.did, tt.did) {
			t.Errorf("joins %v, running %v: did %v, want %v", tt.joins, tt.running, n.did, tt.did)
		}
	}
}

// TestStoppedNodeLeavesAsAsked: a stopped node has something to log out,
// and is started for it, only once its state file is there; the leave
// marker's directory is the driver's, apart from the one bound writable
// into the box.
func TestStoppedNodeLeavesAsAsked(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir()}}
	n := dockerNode{d, "c"}
	if n.hasState() {
		t.Error("a node with no state file has state")
	}
	if err := os.MkdirAll(d.tailnetDir("c"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.tailnetDir("c"), guest.TailnetStateFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !n.hasState() {
		t.Error("a node with its state file has none")
	}
	if !strings.HasPrefix(d.tailnetControl("c"), d.cfg.StateDir) || strings.HasPrefix(d.tailnetControl("c"), d.tailnetDir("c")) {
		t.Errorf("the control directory %s is not the driver's alone", d.tailnetControl("c"))
	}
}

// TestReposAreGivenByTowerGuest: /repos is made and given by tower-guest
// give-dir, by its absolute path, never by anything of the image's.
func TestReposAreGivenByTowerGuest(t *testing.T) {
	got := guestCommand(giveReposArgs(1000, 1001)...)
	want := []string{GuestBinary, "give-dir", "1000", "1001", ReposDir}
	if !slices.Equal(got, want) || !filepath.IsAbs(got[0]) {
		t.Errorf("/repos is given by %q, want %q", got, want)
	}
}
