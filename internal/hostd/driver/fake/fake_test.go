package fake_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/drivertest"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/fake"
)

const image = fake.Registry + "/base@sha256:00"

func harness(t *testing.T, h *fake.Host) drivertest.Harness {
	t.Helper()
	h.SetRemote("https://git.example.com/drivertest.git", "main", map[string]string{"go.mod": "module x\n"}, "c1", "c2")
	return drivertest.Harness{
		Drivers: h.Drivers(), Image: image, MissingImage: fake.Registry + "/base@sha256:ff", BaseImage: "ubuntu-24.04", EchoPort: 7,
		Remote: "https://git.example.com/drivertest.git", Branch: "main", RemoteHead: "c2",
	}
}

func newHost(t *testing.T, path string) *fake.Host {
	t.Helper()
	h, err := fake.New(fake.Options{Path: path, Images: []string{image}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestConformance holds the fake host to the drivers' contract.
func TestConformance(t *testing.T) {
	h := newHost(t, "")
	drivertest.Run(t, harness(t, h))
	if s := h.State(); !s.Empty() {
		t.Errorf("the suites left %+v on the host", s)
	}
}

// TestConformanceKeptInAFile: the same, on a host kept in its state file.
func TestConformanceKeptInAFile(t *testing.T) {
	drivertest.Run(t, harness(t, newHost(t, filepath.Join(t.TempDir(), "host.json"))))
}

// TestTheHostOutlivesItsProcess: a host made again from its state file
// holds what it held, its guests running as they were; another process
// reads it.
func TestTheHostOutlivesItsProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.json")
	d := newHost(t, path).Drivers()
	ctx := t.Context()
	disk, err := d.Storage.EnsureDisk(ctx, driver.DiskSpec{Name: "d1", Kind: driver.Dataset, Labels: driver.Labels{driver.LabelOwner: "t1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Images.Pull(ctx, image); err != nil {
		t.Fatal(err)
	}
	if err := d.Containers.Create(ctx, driver.ContainerSpec{GuestSpec: driver.GuestSpec{Name: "c1", Disk: disk.Path}, Image: image}); err != nil {
		t.Fatal(err)
	}
	if err := d.Containers.Start(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := d.Containers.WriteFile(ctx, "c1", "/workspace/kept", 0o644, 0, strings.NewReader("yes")); err != nil {
		t.Fatal(err)
	}

	again := newHost(t, path).Drivers()
	g, err := again.Containers.Inspect(ctx, "c1")
	if err != nil || !g.Running {
		t.Fatalf("the container after the host was made again: %+v, %v", g, err)
	}
	var b bytes.Buffer
	if err := again.Containers.ReadFile(ctx, "c1", "/workspace/kept", &b); err != nil || b.String() != "yes" {
		t.Errorf("its file: %q, %v", b.String(), err)
	}
	st, err := fake.ReadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Containers) != 1 || len(st.Disks) != 1 || st.Empty() {
		t.Errorf("the state read: %+v", st)
	}
}

// TestProcessesOutliveTheirProcess: a host made again from its state file
// lists a process still running and ends it when signalled; the file never
// holds a process's environment.
func TestProcessesOutliveTheirProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.json")
	d := newHost(t, path).Drivers()
	ctx := t.Context()
	disk, err := d.Storage.EnsureDisk(ctx, driver.DiskSpec{Name: "d1", Kind: driver.Dataset})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Images.Pull(ctx, image); err != nil {
		t.Fatal(err)
	}
	if err := d.Containers.Create(ctx, driver.ContainerSpec{GuestSpec: driver.GuestSpec{Name: "c1", Disk: disk.Path}, Image: image}); err != nil {
		t.Fatal(err)
	}
	if err := d.Containers.Start(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	id, err := d.Containers.StartProcess(ctx, "c1", driver.ProcessSpec{
		Command: []string{"sleep"}, Env: map[string]string{"TOKEN": "s3cr3t-value"}, LogCap: 1 << 10, Keep: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || bytes.Contains(b, []byte("s3cr3t-value")) {
		t.Errorf("the state file holds a process's environment: %v", err)
	}

	again := newHost(t, path).Drivers()
	list, err := again.Containers.ListProcesses(ctx, "c1")
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].Exit != nil {
		t.Fatalf("the process after the host was made again: %+v, %v", list, err)
	}
	if err := again.Containers.SignalProcess(ctx, "c1", id, 9); err != nil {
		t.Fatal(err)
	}
	end, err := again.Containers.ReadProcessLog(ctx, "c1", id, 0, true, func(driver.LogChunk) error { return nil })
	if err != nil || end.Exit == nil || end.Exit.Code != 137 {
		t.Errorf("the process killed: %+v, %v", end, err)
	}
}

// TestFaults: what a test injects is what the drivers do.
func TestFaults(t *testing.T) {
	h := newHost(t, "")
	d, ctx := h.Drivers(), t.Context()
	boom := errors.New("boom")
	h.Fail("storage.Disks", boom)
	if _, err := d.Storage.Disks(ctx); !errors.Is(err, boom) {
		t.Errorf("a failed list: %v", err)
	}
	h.Fail("storage.Disks", nil)
	if _, err := d.Storage.Disks(ctx); err != nil {
		t.Errorf("a list cleared of its fault: %v", err)
	}
	free := uint64(0)
	h.SetFree(&free)
	if _, err := d.Storage.EnsureDisk(ctx, driver.DiskSpec{Name: "full", Kind: driver.Dataset}); !errors.Is(err, driver.ErrNoSpace) {
		t.Errorf("a disk on a full pool: %v", err)
	}
	h.SetFree(nil)
	disk, _ := d.Storage.EnsureDisk(ctx, driver.DiskSpec{Name: "d", Kind: driver.Dataset})
	_ = d.Images.Pull(ctx, image)
	_ = d.Containers.Create(ctx, driver.ContainerSpec{GuestSpec: driver.GuestSpec{Name: "c", Disk: disk.Path}, Image: image})
	h.FailStart("c", "no kernel")
	if err := d.Containers.Start(ctx, "c"); !errors.Is(err, driver.ErrStartFailed) || !strings.Contains(err.Error(), "no kernel") {
		t.Errorf("a start that fails: %v", err)
	}
	h.FailStart("c", "")
	if err := d.Containers.Start(ctx, "c"); err != nil {
		t.Errorf("a start cleared of its fault: %v", err)
	}
	h.Lose("container", "c")
	if _, err := d.Containers.Inspect(ctx, "c"); !errors.Is(err, driver.ErrNotFound) {
		t.Errorf("a container lost: %v", err)
	}
	if disks, _ := d.Storage.Disks(ctx); len(disks) != 0 {
		t.Errorf("a container lost kept its disk: %+v", disks)
	}
	h.SetHealth("DEGRADED")
	if p, _ := d.Storage.Pool(ctx); p.Health != "DEGRADED" {
		t.Errorf("pool health: %q", p.Health)
	}
}
