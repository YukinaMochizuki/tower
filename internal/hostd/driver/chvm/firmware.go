package chvm

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
)

// A firmware VM boots its own kernel through EDK2 (CLOUDHV.fd), as a
// VirtualMachine does (docs/decisions/vm-boot.md). Nothing of the platform
// runs in it: its serial console is its log, and stopping it presses its
// power button.

// DefaultFirmwareStopGrace is how long a firmware VM's guest has to power
// off once its power button is pressed, unless Config.FirmwareStopGrace
// says otherwise: an operating system shuts down slower than a container's
// process, Ubuntu 24.04 in about 30 s while cloud-init still runs.
const DefaultFirmwareStopGrace = 60 * time.Second

// NoCloudSerial is the SMBIOS system serial number a firmware VM is given
// unless its spec names one:
// cloud-init reads it as where its NoCloud datasource is, the metadata
// service every network of the VM's has. Its dsmode says the seed is read
// once the network is up: without it, cloud-init 22.4 (Debian 12's) takes
// ds=nocloud for a local seed and drops what its network stage fetched;
// 24.4 and later take net from the http URL, and keep a dsmode given.
const NoCloudSerial = "ds=nocloud;s=http://169.254.169.254/nocloud/;dsmode=net"

// pressPowerButton presses the power button of the VM whose Cloud
// Hypervisor serves its API at socket: an ACPI event the guest's operating
// system shuts down on. The socket is in a directory Cloud Hypervisor can
// write in, so it is dialled as the vsock socket is, never through a link.
func pressPowerButton(ctx context.Context, socket string) error {
	c := http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return chguest.DialSocket(ctx, socket)
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

// firmwareArgs is Cloud Hypervisor's command line for a firmware VM: the
// firmware, the balloon for free page reporting, its serial number, its
// volumes at their nodes in dir with their serials, the root first, the
// taps on fds 3 and on, its serial console to the FIFO, no virtio console,
// and its API socket.
func firmwareArgs(ln launch, dir string, taps int) []string {
	s := ln.Spec
	args := []string{
		"--firmware", ln.Firmware,
		"--cpus", fmt.Sprintf("boot=%d", s.VCPUs),
		"--memory", fmt.Sprintf("size=%dM", s.MemoryMiB),
		"--balloon", freePageReporting,
		"--platform", "system_serial_number=" + quoteOption(cmp.Or(s.SystemSerialNumber, NoCloudSerial)),
		"--disk",
	}
	for n, v := range s.Disks {
		disk := "path=" + filepath.Join(dir, volumeNode(n)) + ",image_type=raw"
		if v.Serial != "" {
			disk += ",serial=" + v.Serial
		}
		args = append(args, disk)
	}
	if taps > 0 {
		args = append(args, "--net")
		for n, i := range s.Interfaces[:taps] {
			args = append(args, fmt.Sprintf("fd=%d,mac=%s", 3+n, i.MAC))
		}
	}
	return append(args,
		"--api-socket", "path="+filepath.Join(dir, apiFile),
		"--console", "off",
		"--serial", "file="+filepath.Join(dir, serialFIFO),
	)
}

// quoteOption quotes v as a value of Cloud Hypervisor's comma-separated
// options, which takes everything between double quotes as it is, a doubled
// quote as one, and trims what is outside them.
func quoteOption(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}

// serialFramer frames what a firmware VM's serial console writes into its
// console log as Stdout records, timed by the host as they come, so the log
// is read as a container's is.
type serialFramer struct {
	log    io.Writer
	logErr func(error)
	now    func() time.Time

	// rec and failing are pump's own.
	rec     []byte
	failing bool

	mu       sync.Mutex
	lastRead time.Time
}

// Coalescing a serial console: a guest's UART is read a few bytes at a
// time, so what has come is framed as far as its last line end, in records
// of at most console.MaxPayload. The rest of a line waits for its end, or
// is framed once it holds maxSerialRecord bytes or more, or once the
// console has been quiet for serialFlush, as a login prompt is.
const (
	maxSerialRecord = 4 << 10
	serialFlush     = 50 * time.Millisecond
)

// pump frames r into the log until r ends.
func (f *serialFramer) pump(r io.Reader) {
	chunks := make(chan []byte, 64)
	go func() {
		defer close(chunks)
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				f.mu.Lock()
				f.lastRead = f.now()
				f.mu.Unlock()
				chunks <- bytes.Clone(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	var pending []byte
	flush := time.NewTimer(serialFlush)
	flush.Stop()
	for {
		select {
		case c, ok := <-chunks:
			if !ok {
				f.write(pending)
				return
			}
			pending = append(pending, c...)
			if i := bytes.LastIndexByte(pending, '\n'); i >= 0 {
				f.write(pending[:i+1])
				pending = append([]byte(nil), pending[i+1:]...)
			}
			if len(pending) >= maxSerialRecord {
				f.write(pending)
				pending = nil
			}
			if len(pending) > 0 {
				flush.Reset(serialFlush)
			}
		case <-flush.C:
			f.write(pending)
			pending = nil
		}
	}
}

// write frames p into the log as Stdout records.
func (f *serialFramer) write(p []byte) {
	for len(p) > 0 {
		k := min(len(p), console.MaxPayload)
		f.rec = console.AppendRecord(f.rec[:0], console.Record{Stream: console.Stdout, Time: f.now(), Payload: p[:k]})
		if _, err := f.log.Write(f.rec); err != nil {
			if !f.failing && f.logErr != nil {
				f.logErr(err)
			}
			f.failing = true
		} else {
			f.failing = false
		}
		p = p[k:]
	}
}

// quiet waits until the console has written nothing for a while, or for at
// most drainWait: what the guest wrote as it powered off is in the log.
func (f *serialFramer) quiet() {
	deadline := time.Now().Add(drainWait)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		last := f.lastRead
		f.mu.Unlock()
		if time.Since(last) > 100*time.Millisecond {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
