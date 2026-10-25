package drivertest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// GuestKind is one guest driver, and how the suite makes its guests.
type GuestKind struct {
	Driver   driver.Guests
	DiskKind driver.DiskKind
	// Create makes a stopped guest of spec.
	Create func(t *testing.T, spec driver.GuestSpec) error
	// Workspace is a directory of the guest its disk holds.
	Workspace string
	// Keeps are directories of the guest where it keeps what it is told,
	// searched for what must never be kept.
	Keeps []string
}

// ContainersOf is the harness's container driver as a guest kind, for a
// harness that runs the suites one at a time.
func ContainersOf(h Harness) GuestKind { return h.containers() }

// VMsOf is the harness's VM driver as a guest kind.
func VMsOf(h Harness) GuestKind { return h.vms() }

func (h Harness) containers() GuestKind {
	c := h.Drivers.Containers
	return GuestKind{
		Driver: c, DiskKind: driver.Dataset, Workspace: "/workspace",
		Create: func(t *testing.T, spec driver.GuestSpec) error {
			mustNot(t, "pulling "+h.Image, h.Drivers.Images.Pull(t.Context(), h.Image))
			return c.Create(t.Context(), driver.ContainerSpec{GuestSpec: spec, Image: h.Image})
		},
	}
}

func (h Harness) vms() GuestKind {
	v := h.Drivers.VMs
	return GuestKind{
		Driver: v, DiskKind: driver.Volume, Workspace: "/home/tower", Keeps: []string{"/var/lib/tower"},
		Create: func(t *testing.T, spec driver.GuestSpec) error {
			return v.Create(t.Context(), driver.VMSpec{GuestSpec: spec, BaseImage: h.BaseImage})
		},
	}
}

// guest makes a running guest on the disk at diskPath, on no network,
// until the test ends, and returns its name.
func (k GuestKind) guest(t *testing.T, h Harness, what, diskPath string) string {
	t.Helper()
	n := name(t, what)
	mustNot(t, "making guest "+n, k.Create(t, driver.GuestSpec{Name: n, Disk: diskPath, CPUs: 1, Memory: 1 << 30, Labels: labels(t)}))
	t.Cleanup(func() { _ = k.Driver.Remove(context.WithoutCancel(t.Context()), n) })
	mustNot(t, "starting guest "+n, k.Driver.Start(t.Context(), n))
	return n
}

// guestWithDisk makes a running guest on a disk of its own.
func (k GuestKind) guestWithDisk(t *testing.T, h Harness, what string) string {
	t.Helper()
	d := disk(t, h, driver.DiskSpec{Name: name(t, what+"-disk"), Kind: k.DiskKind, Size: 1 << 30, Labels: labels(t)})
	return k.guest(t, h, what, d.Path)
}

func readFile(t *testing.T, g driver.Guests, name, path string) string {
	t.Helper()
	var b bytes.Buffer
	mustNot(t, "reading "+path, g.ReadFile(t.Context(), name, path, &b))
	return b.String()
}

// RunGuests runs a guest driver's suite.
func RunGuests(t *testing.T, h Harness, k GuestKind) {
	tests := []struct {
		name string
		fn   func(*testing.T, Harness, GuestKind)
	}{
		{"Lifecycle", testLifecycle},
		{"Networks", testNetworks},
		{"Exec", testGuestExec},
		{"EnvironmentIsPerCommand", testGuestEnv},
		{"ExecEndsWithItsContext", testExecEnds},
		{"Files", testGuestFiles},
		{"Copy", testGuestCopy},
		{"Dial", testDial},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, h, k) })
	}
}

func testLifecycle(t *testing.T, h Harness, k GuestKind) {
	g, ctx := k.Driver, t.Context()
	d := disk(t, h, driver.DiskSpec{Name: name(t, "disk"), Kind: k.DiskKind, Size: 1 << 30, Labels: labels(t)})
	n := name(t, "g")
	spec := driver.GuestSpec{Name: n, Disk: d.Path, CPUs: 1, Memory: 1 << 30, Labels: labels(t)}
	mustNot(t, "making the guest", k.Create(t, spec))
	t.Cleanup(func() { _ = g.Remove(context.WithoutCancel(ctx), n) })
	mustNot(t, "making the guest again", k.Create(t, spec))
	other := spec
	other.Disk = d.Path + "-other"
	is(t, "a guest of the name made otherwise", k.Create(t, other), driver.ErrConflict)

	got, err := g.Inspect(ctx, n)
	mustNot(t, "inspecting", err)
	if got.Running || got.Name != n || !maps.Equal(got.Labels, spec.Labels) || got.Disk != d.Path {
		t.Errorf("a guest made: %+v; want it stopped, with its spec", got)
	}
	recent(t, "a guest", got.Created)
	_, err = g.Exec(ctx, n, driver.ExecSpec{Command: []string{"true"}})
	is(t, "a command in a stopped guest", err, driver.ErrNotRunning)

	mustNot(t, "starting", g.Start(ctx, n))
	got, err = g.Inspect(ctx, n)
	mustNot(t, "inspecting", err)
	if !got.Running || got.Address == "" || got.StartedAt.IsZero() {
		t.Errorf("a guest started: %+v; want it running, with its address and start time", got)
	}
	mustNot(t, "starting again", g.Start(ctx, n))
	if again, _ := g.Inspect(ctx, n); !again.StartedAt.Equal(got.StartedAt) {
		t.Errorf("starting a running guest started it again: %v, then %v", got.StartedAt, again.StartedAt)
	}
	list, err := g.List(ctx)
	mustNot(t, "listing", err)
	if !slices.ContainsFunc(list, func(l driver.Guest) bool { return l.Name == n && maps.Equal(l.Labels, spec.Labels) }) {
		t.Errorf("the guest is not listed with its labels: %+v", list)
	}

	mustNot(t, "stopping", g.Stop(ctx, n))
	if got, _ := g.Inspect(ctx, n); got.Running {
		t.Errorf("a guest stopped: %+v", got)
	}
	mustNot(t, "stopping again", g.Stop(ctx, n))
	mustNot(t, "removing", g.Remove(ctx, n))
	_, err = g.Inspect(ctx, n)
	is(t, "inspecting a guest removed", err, driver.ErrNotFound)
	is(t, "starting a guest removed", g.Start(ctx, n), driver.ErrNotFound)
	mustNot(t, "removing a guest that is not there", g.Remove(ctx, n))
	if _, ok := listed(t, h, d.Name); !ok {
		t.Error("removing a guest took its disk")
	}
}

func testNetworks(t *testing.T, h Harness, k GuestKind) {
	g, ctx := k.Driver, t.Context()
	net := name(t, "net")
	mustNot(t, "making a network", g.EnsureNetwork(ctx, net, labels(t)))
	t.Cleanup(func() { _ = g.RemoveNetwork(context.WithoutCancel(ctx), net) })
	mustNot(t, "making the network again", g.EnsureNetwork(ctx, net, labels(t)))
	networks, err := g.Networks(ctx)
	mustNot(t, "listing networks", err)
	i := slices.IndexFunc(networks, func(n driver.Network) bool { return n.Name == net && maps.Equal(n.Labels, labels(t)) })
	if i < 0 {
		t.Fatalf("the network is not listed with its labels: %+v", networks)
	}
	recent(t, "a network", networks[i].Created)
	d := disk(t, h, driver.DiskSpec{Name: name(t, "disk"), Kind: k.DiskKind, Size: 1 << 30, Labels: labels(t)})
	n := name(t, "on")
	mustNot(t, "making a guest on it", k.Create(t, driver.GuestSpec{Name: n, Disk: d.Path, Network: net, Alias: "web", CPUs: 1, Memory: 1 << 30, Labels: labels(t)}))
	t.Cleanup(func() { _ = g.Remove(context.WithoutCancel(ctx), n) })
	if got, _ := g.Inspect(ctx, n); got.Network != net || got.Alias != "web" {
		t.Errorf("a guest on a network: %+v", got)
	}
	is(t, "removing a network a guest is on", g.RemoveNetwork(ctx, net), driver.ErrBusy)
	mustNot(t, "removing the guest", g.Remove(ctx, n))
	mustNot(t, "removing the network", g.RemoveNetwork(ctx, net))
	mustNot(t, "removing a network that is not there", g.RemoveNetwork(ctx, net))
	is(t, "a guest on a network that is not there", k.Create(t, driver.GuestSpec{Name: name(t, "lost"), Disk: d.Path, Network: net, Labels: labels(t)}), driver.ErrNotFound)
}

// run runs a command and returns its exit code and output.
func run(t *testing.T, g driver.Guests, n string, env map[string]string, command ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code, err := g.Exec(t.Context(), n, driver.ExecSpec{Command: command, Env: env, Stdout: &out, Stderr: io.Discard})
	mustNot(t, "running "+strings.Join(command, " "), err)
	return code, out.String()
}

func testGuestExec(t *testing.T, h Harness, k GuestKind) {
	g := k.Driver
	n := k.guestWithDisk(t, h, "exec")
	if code, out := run(t, g, n, nil, "echo", "hello", "world"); code != 0 || out != "hello world\n" {
		t.Errorf("echo: %d %q", code, out)
	}
	for cmd, want := range map[string]int{"true": 0, "false": 1} {
		if code, _ := run(t, g, n, nil, cmd); code != want {
			t.Errorf("%s exited %d, want %d", cmd, code, want)
		}
	}
	if code, _ := run(t, g, n, nil, "sh", "-c", "exit 3"); code != 3 {
		t.Errorf("sh -c 'exit 3' exited %d", code)
	}
	var mu sync.Mutex
	var got []string
	stream := func(name string) io.Writer {
		return writer(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			for line := range strings.Lines(string(p)) {
				got = append(got, name+":"+strings.TrimSuffix(line, "\n"))
			}
			return len(p), nil
		})
	}
	want := []string{"out:one", "err:two", "out:three"}
	code, err := g.Exec(t.Context(), n, driver.ExecSpec{Command: append([]string{"emit"}, want...), Stdout: stream("out"), Stderr: stream("err")})
	if err != nil || code != 0 || !slices.Equal(got, want) {
		t.Errorf("emit: %d, %v, %q; want %q in order", code, err, got, want)
	}
	var out bytes.Buffer
	code, err = g.Exec(t.Context(), n, driver.ExecSpec{Command: []string{"cat"}, Stdin: strings.NewReader("typed"), Stdout: &out})
	if err != nil || code != 0 || out.String() != "typed" {
		t.Errorf("cat of stdin: %d, %v, %q", code, err, out.String())
	}
	_, err = g.Exec(t.Context(), name(t, "absent"), driver.ExecSpec{Command: []string{"true"}})
	is(t, "a command in a guest that is not there", err, driver.ErrNotFound)
}

type writer func([]byte) (int, error)

func (f writer) Write(p []byte) (int, error) { return f(p) }

func testGuestEnv(t *testing.T, h Harness, k GuestKind) {
	g := k.Driver
	n := k.guestWithDisk(t, h, "env")
	if _, out := run(t, g, n, map[string]string{"DRIVERTEST": "given"}, "env"); !strings.Contains(out, "DRIVERTEST=given\n") {
		t.Errorf("the environment given a command is not its own: %q", out)
	}
	if _, out := run(t, g, n, nil, "env"); strings.Contains(out, "DRIVERTEST") {
		t.Errorf("the environment given one command reached the next: %q", out)
	}
}

func testExecEnds(t *testing.T, h Harness, k GuestKind) {
	g := k.Driver
	n := k.guestWithDisk(t, h, "ends")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := g.Exec(ctx, n, driver.ExecSpec{Command: []string{"sleep", "3600"}})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a command whose context ended returned no error")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a command whose context ended still runs")
	}
}

func testGuestFiles(t *testing.T, h Harness, k GuestKind) {
	g, ctx := k.Driver, t.Context()
	n := k.guestWithDisk(t, h, "files")
	p := k.Workspace + "/drivertest/a.txt"
	mustNot(t, "writing", g.WriteFile(ctx, n, p, 0o644, 0, strings.NewReader("cont")))
	mustNot(t, "writing a chunk", g.WriteFile(ctx, n, p, 0o644, 4, strings.NewReader("ents")))
	is(t, "a chunk not where the file ends", g.WriteFile(ctx, n, p, 0o644, 2, strings.NewReader("xx")), driver.ErrBadOffset)
	if got := readFile(t, g, n, p); got != "contents" {
		t.Errorf("read back %q", got)
	}
	if code, out := run(t, g, n, nil, "cat", p); code != 0 || out != "contents" {
		t.Errorf("the file as a command reads it: %d %q", code, out)
	}
	mustNot(t, "replacing", g.WriteFile(ctx, n, p, 0o644, 0, strings.NewReader("new")))
	if got := readFile(t, g, n, p); got != "new" {
		t.Errorf("a write at offset 0 left %q", got)
	}
	err := g.ReadFile(ctx, n, k.Workspace+"/drivertest/none", io.Discard)
	is(t, "reading a file that is not there", err, fs.ErrNotExist)
}

func testGuestCopy(t *testing.T, h Harness, k GuestKind) {
	g, ctx := k.Driver, t.Context()
	a, b := k.guestWithDisk(t, h, "copy-a"), k.guestWithDisk(t, h, "copy-b")
	mustNot(t, "writing", g.WriteFile(ctx, a, "/tmp/out/result.txt", 0o644, 0, strings.NewReader("built")))
	var tarball bytes.Buffer
	mustNot(t, "copying out", g.CopyOut(ctx, a, "/tmp/out", &tarball))
	if got := files(t, tarball.Bytes()); got["out/result.txt"] != "built" {
		t.Errorf("the tar of /tmp/out holds %v, want out/result.txt", got)
	}
	mustNot(t, "copying in", g.CopyIn(ctx, b, "/tmp/in", bytes.NewReader(tarball.Bytes())))
	if got := readFile(t, g, b, "/tmp/in/out/result.txt"); got != "built" {
		t.Errorf("the copy holds %q", got)
	}
	err := g.CopyOut(ctx, a, "/tmp/never", io.Discard)
	is(t, "copying out what is not there", err, fs.ErrNotExist)
}

func testDial(t *testing.T, h Harness, k GuestKind) {
	g, ctx := k.Driver, t.Context()
	n := k.guestWithDisk(t, h, "dial")
	conn, err := g.Dial(ctx, n, h.EchoPort)
	mustNot(t, "dialing", err)
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping\n" {
		t.Errorf("the echo answered %q, %v", buf, err)
	}
	mustNot(t, "stopping", g.Stop(ctx, n))
	_, err = g.Dial(ctx, n, h.EchoPort)
	if !errors.Is(err, driver.ErrNotRunning) {
		t.Errorf("dialing a stopped guest: %v, want ErrNotRunning", err)
	}
}
