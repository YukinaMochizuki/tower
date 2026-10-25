// Package launcher runs one VM in the foreground, as the first process of
// the VM's container (tower-guest vm): the container is what Docker
// starts, stops and puts on the VM's workstream network, under its alias,
// and the launcher makes its network namespace the VM's
// (internal/hostd/driver/chvm). It opens a tap there for the VM, serves
// the VM its address by DHCP, its resolver and cloud-init's metadata,
// forwards the container's address to the VM, and runs Cloud Hypervisor
// on the VM's disk and tap, keeping its serial console in a capped log.
// Stopped (SIGTERM, as docker stop sends), it presses the VM's power
// button and gives the guest its grace before it kills Cloud Hypervisor.
//
// It started as hangar's internal/node/driver/chvm launcher, run there by a
// systemd unit; here the container is the unit.
package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// Paths in a VM's container.
const (
	// Dir holds the VM's spec, console log, API socket and exit record:
	// the VM's directory on the host.
	Dir = "/vm"
	// CloudHypervisor and Firmware are bound in from the host.
	CloudHypervisor = "/cloud-hypervisor"
	Firmware        = "/CLOUDHV.fd"
	// Disk is the VM's disk, its zvol's device.
	Disk = "/dev/tower-disk"
)

// Files of Dir.
const (
	SpecFile   = "spec.json"
	ConsoleLog = "console.log"
	ExitFile   = "exit.json"
	apiFile    = "api.sock"
)

// Spec is what the launcher runs, written into Dir by the driver.
type Spec struct {
	Name      string `json:"name"`
	CPUs      int    `json:"cpus"`
	MemoryMiB int64  `json:"memoryMiB"`
	// MAC is the VM's interface's.
	MAC string `json:"mac"`
	// InstanceID and Hostname are cloud-init's meta-data; UserData its
	// user-data.
	InstanceID string `json:"instanceID"`
	Hostname   string `json:"hostname"`
	UserData   string `json:"userData"`
	// StopGrace is how long the guest has to power off once its power
	// button is pressed.
	StopGrace time.Duration `json:"stopGrace"`
}

// Exit is the exit record: how the launcher's last run ended.
type Exit struct {
	Code  int       `json:"code"`
	Error string    `json:"error,omitempty"`
	Ended time.Time `json:"ended"`
}

// The launcher's own exit codes.
const (
	ExitFailed = 125
)

// consoleMax bounds each file of the console log.
const consoleMax = 4 << 20

// killGrace bounds how long Cloud Hypervisor has to exit once killed.
const killGrace = 5 * time.Second

// Main is tower-guest vm: it runs the VM of Dir until it ends, returning
// the launcher's exit code.
func Main(_ []string) int {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	code, err := run(ctx, log)
	rec := Exit{Code: code, Ended: time.Now().UTC()}
	if err != nil {
		log.Error("the VM did not run", "error", err)
		rec.Error = err.Error()
	}
	if b, merr := json.Marshal(rec); merr == nil {
		_ = os.WriteFile(filepath.Join(Dir, ExitFile), b, 0o600)
	}
	return code
}

func readSpec() (Spec, error) {
	var s Spec
	b, err := os.ReadFile(filepath.Join(Dir, SpecFile))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

func run(ctx context.Context, log *slog.Logger) (int, error) {
	s, err := readSpec()
	if err != nil {
		return ExitFailed, fmt.Errorf("the VM's spec: %w", err)
	}
	_ = os.Remove(filepath.Join(Dir, ExitFile))
	_ = os.Remove(filepath.Join(Dir, apiFile))
	tap, err := setupNetwork(ctx, s, log)
	if err != nil {
		return ExitFailed, fmt.Errorf("the VM's network: %w", err)
	}
	defer func() { _ = tap.Close() }()
	clog, err := openCapped(filepath.Join(Dir, ConsoleLog), consoleMax)
	if err != nil {
		return ExitFailed, err
	}
	defer func() { _ = clog.Close() }()
	_, _ = fmt.Fprintf(clog, "\n--- tower: %s boots at %s ---\n", s.Name, time.Now().UTC().Format(time.RFC3339))

	api := filepath.Join(Dir, apiFile)
	cmd := exec.Command(CloudHypervisor, chArgs(s, Firmware, Disk, api)...)
	outR, outW, err := os.Pipe()
	if err != nil {
		return ExitFailed, err
	}
	// Its stdin, the serial console's input, is held open and never
	// written.
	inR, inW, err := os.Pipe()
	if err != nil {
		return ExitFailed, err
	}
	defer func() { _ = inW.Close() }()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, outW
	cmd.ExtraFiles = []*os.File{tap} // fd 3
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		return ExitFailed, fmt.Errorf("starting Cloud Hypervisor: %w", err)
	}
	_ = inR.Close()
	_ = outW.Close()
	pumped := make(chan struct{})
	go func() {
		pumpRaw(outR, clog, func(err error) { log.Warn("the console log fails", "error", err) })
		close(pumped)
	}()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var werr error
	select {
	case werr = <-waited:
	case <-ctx.Done():
		werr = stopGuest(cmd, api, s.StopGrace, waited, log)
	}
	<-pumped
	var ee *exec.ExitError
	switch {
	case werr == nil:
		return 0, nil
	case errors.As(werr, &ee):
		if ctx.Err() != nil {
			// Stopped: as a VM powered off is.
			return 0, nil
		}
		return ee.ExitCode(), fmt.Errorf("cloud-hypervisor exited: %w", werr)
	}
	return ExitFailed, werr
}

// stopGuest presses the VM's power button and waits its grace for Cloud
// Hypervisor to exit, as the guest powers off, then kills it.
func stopGuest(cmd *exec.Cmd, api string, grace time.Duration, waited <-chan error, log *slog.Logger) error {
	if grace <= 0 {
		grace = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err := pressPowerButton(ctx, api)
	cancel()
	if err != nil {
		log.Warn("the power button did not press; the VM is killed", "error", err)
	} else {
		select {
		case err := <-waited:
			return err
		case <-time.After(grace):
			log.Warn("the guest did not power off in its grace; the VM is killed", "grace", grace)
		}
	}
	_ = cmd.Process.Kill()
	select {
	case err := <-waited:
		return err
	case <-time.After(killGrace):
		return errors.New("cloud-hypervisor did not exit once killed")
	}
}
