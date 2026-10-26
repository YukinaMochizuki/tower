//go:build testbed

package drivers

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/drivertest"
)

// What confines a guest, read from the host: Docker's record of its
// container and the kernel's of its processes, which no fake has.

// guestOf makes a running guest of kind on a disk of its own, removed as
// the test ends, and returns its name.
func guestOf(t *testing.T, h drivertest.Harness, kind string) string {
	t.Helper()
	ctx := t.Context()
	name := "confined-" + kind
	labels := driver.Labels{driver.LabelOwner: "drivertest", driver.LabelInstance: name}
	diskKind, guests := driver.Dataset, driver.Guests(h.Drivers.Containers)
	if kind == "vm" {
		diskKind, guests = driver.Volume, h.Drivers.VMs
	}
	d, err := h.Drivers.Storage.EnsureDisk(ctx, driver.DiskSpec{Name: name + "-disk", Kind: diskKind, Size: 1 << 30, Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Drivers.Storage.DestroyDisk(context.WithoutCancel(ctx), name+"-disk") })
	spec := driver.GuestSpec{Name: name, Disk: d.Path, CPUs: 1, Memory: 1 << 30, Labels: labels}
	if kind == "vm" {
		err = h.Drivers.VMs.Create(ctx, driver.VMSpec{GuestSpec: spec, BaseImage: h.BaseImage})
	} else {
		err = h.Drivers.Containers.Create(ctx, driver.ContainerSpec{GuestSpec: spec, Image: h.Local(t, h.Image)})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = guests.Remove(context.WithoutCancel(ctx), name) })
	if err := guests.Start(ctx, name); err != nil {
		t.Fatal(err)
	}
	return name
}

// inspect is a field of a container as docker inspect formats it.
func inspect(t *testing.T, name, format string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", format, name).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", name, err)
	}
	return strings.TrimSpace(string(out))
}

// status is a line of a process's /proc status, by its key.
func status(t *testing.T, pid int, key string) string {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && k == key {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("process %d's status has no %s", pid, key)
	return ""
}

func capSet(t *testing.T, hex string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(hex, 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestAVMsHypervisorIsConfined: a VM's Cloud Hypervisor runs as a user of
// its own, with no capability; its launcher, once that runs, keeps
// DAC_OVERRIDE and KILL alone; its container drops Docker's default
// capabilities, gains no privilege through setuid, and runs a bounded
// number of processes.
func TestAVMsHypervisorIsConfined(t *testing.T) {
	h := harness(t)
	name := guestOf(t, h, "vm")
	if got := inspect(t, name, "{{json .HostConfig.CapDrop}}"); !strings.Contains(got, "ALL") {
		t.Errorf("its capabilities dropped: %s", got)
	}
	if got := inspect(t, name, "{{json .HostConfig.SecurityOpt}}"); !strings.Contains(got, "no-new-privileges") {
		t.Errorf("its security options: %s", got)
	}
	if got := inspect(t, name, "{{.HostConfig.PidsLimit}}"); got == "" || got == "0" || got == "<nil>" {
		t.Errorf("its pids limit: %s", got)
	}
	launcher, err := strconv.Atoi(inspect(t, name, "{{.State.Pid}}"))
	if err != nil {
		t.Fatal(err)
	}
	keeps := uint64(1)<<unix.CAP_DAC_OVERRIDE | 1<<unix.CAP_KILL
	for _, key := range []string{"CapEff", "CapPrm", "CapBnd"} {
		if got := capSet(t, status(t, launcher, key)); got != keeps {
			t.Errorf("its launcher's %s is %x, want %x", key, got, keeps)
		}
	}
	// Each thread's children are its own: Go forks on any of them.
	threads, err := os.ReadDir("/proc/" + strconv.Itoa(launcher) + "/task")
	if err != nil {
		t.Fatal(err)
	}
	var fields []string
	for _, th := range threads {
		children, err := os.ReadFile("/proc/" + strconv.Itoa(launcher) + "/task/" + th.Name() + "/children")
		if err != nil {
			t.Fatal(err)
		}
		fields = append(fields, strings.Fields(string(children))...)
	}
	if len(fields) != 1 {
		t.Fatalf("the launcher's children: %q", fields)
	}
	ch, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatal(err)
	}
	uids := strings.Fields(status(t, ch, "Uid"))
	if len(uids) == 0 || uids[0] == "0" || uids[1] == "0" {
		t.Errorf("Cloud Hypervisor runs as %v", uids)
	}
	if got := capSet(t, status(t, ch, "CapEff")); got != 0 {
		t.Errorf("Cloud Hypervisor's CapEff is %x", got)
	}
}

// TestABoxContainerIsConfined: a box's container has no raw sockets, gains
// no privilege through setuid, and runs a bounded number of processes.
func TestABoxContainerIsConfined(t *testing.T) {
	h := harness(t)
	name := guestOf(t, h, "box")
	if got := inspect(t, name, "{{json .HostConfig.CapDrop}}"); !strings.Contains(got, "NET_RAW") {
		t.Errorf("its capabilities dropped: %s", got)
	}
	if got := inspect(t, name, "{{json .HostConfig.SecurityOpt}}"); !strings.Contains(got, "no-new-privileges") {
		t.Errorf("its security options: %s", got)
	}
	if got := inspect(t, name, "{{.HostConfig.PidsLimit}}"); got == "" || got == "0" || got == "<nil>" {
		t.Errorf("its pids limit: %s", got)
	}
	pid, err := strconv.Atoi(inspect(t, name, "{{.State.Pid}}"))
	if err != nil {
		t.Fatal(err)
	}
	if bnd := capSet(t, status(t, pid, "CapBnd")); bnd&(1<<unix.CAP_NET_RAW) != 0 {
		t.Errorf("its init may yet take CAP_NET_RAW: CapBnd %x", bnd)
	}
	if got := status(t, pid, "NoNewPrivs"); got != "1" {
		t.Errorf("its init has NoNewPrivs %s", got)
	}
}
