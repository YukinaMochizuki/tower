package launcher

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// A VM boots its own kernel through EDK2 (CLOUDHV.fd), as hangar's
// VirtualMachines do (hangar: docs/decisions/vm-boot.md): nothing of
// tower runs in it until hostd copies tower-guest in over ssh. Its serial
// console is its console log, and stopping it presses its power button.

// NoCloudSerial is the SMBIOS system serial number a VM is given:
// cloud-init reads it as where its NoCloud datasource is, the metadata
// service the launcher serves. Its dsmode says the seed is read once the
// network is up (hangar: internal/node/driver/chvm/firmware.go).
const NoCloudSerial = "ds=nocloud;s=http://" + MetadataAddr + "/nocloud/;dsmode=net"

// freePageReporting gives memory the guest frees back to the host.
const freePageReporting = "size=0,free_page_reporting=on"

// pressPowerButton presses the power button of the VM whose Cloud
// Hypervisor serves its API at socket: an ACPI event the guest's operating
// system shuts down on.
func pressPowerButton(ctx context.Context, socket string) error {
	c := http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	defer c.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost/api/v1/vm.power-button", nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("pressing the power button: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("pressing the power button: %s: %s", resp.Status, b)
	}
	return nil
}

// chArgs is Cloud Hypervisor's command line for a VM: the firmware, its
// CPUs and memory, the balloon for free page reporting, the serial number
// pointing cloud-init at the metadata service, its disk, its tap on fd 3,
// its serial console on its stdout, no virtio console, and its API socket.
func chArgs(s Spec, firmware, disk, apiSocket string) []string {
	return []string{
		"--firmware", firmware,
		"--cpus", fmt.Sprintf("boot=%d", max(s.CPUs, 1)),
		"--memory", fmt.Sprintf("size=%dM", max(s.MemoryMiB, 128)),
		"--balloon", freePageReporting,
		"--platform", "system_serial_number=" + quoteOption(NoCloudSerial),
		"--disk", "path=" + disk + ",image_type=raw",
		"--net", "fd=3,mac=" + s.MAC,
		"--api-socket", "path=" + apiSocket,
		"--console", "off",
		"--serial", "tty",
	}
}

// quoteOption quotes v as a value of Cloud Hypervisor's comma-separated
// options, which takes everything between double quotes as it is, a doubled
// quote as one, and trims what is outside them.
func quoteOption(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}
