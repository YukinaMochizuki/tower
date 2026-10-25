package chvm

import (
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/node/driver"
)

// What a VM's directory, Dir/<machine uid>, holds. The logs outlive the VM;
// the rest is the running VM's, and goes with it. Everything in it is
// root's: Cloud Hypervisor, running as the VM's user, reads the boot disk
// and opens the root disk's node and the serial FIFO through the VM's
// group, and writes only in chDir.
const (
	// specFile is the launch: what the launcher starts, written by Start.
	specFile = "spec.json"
	// bootFile is the boot disk (internal/guest/bootdisk).
	bootFile = "boot.img"
	// blankFile is a file share's server's, or a converter's, vda, in place
	// of a root disk: blank, and never read, but by the kernel probing it.
	blankFile = "blank.img"
	// diskNode is a node of the root disk's device, made by the launcher
	// for Cloud Hypervisor to open, as the jailer makes one in its chroot.
	diskNode = "disk"
	// volumeNodePrefix and a number from 1 name a node of each volume's
	// device, made as diskNode is (volumeNode).
	volumeNodePrefix = "disk-"
	// chDir is the one directory Cloud Hypervisor can write in, for its
	// sockets; the launcher makes it afresh on each run.
	chDir = "ch"
	// apiFile is Cloud Hypervisor's API socket, which a firmware VM's stop
	// presses its power button through.
	apiFile = chDir + "/api"
	// vsockFile is the hybrid vsock socket Cloud Hypervisor listens on.
	vsockFile = chDir + "/vsock"
	// legacyVsockFile is the socket of a VM whose launcher has no chDir; it
	// is there until the VM is started again.
	legacyVsockFile = "vsock"
	// exitFile records how the VM's last run ended, written by the
	// launcher when Cloud Hypervisor exits by itself.
	exitFile = "exit.json"
	// stopFile marks a VM being stopped: once its guest powers off, the
	// launcher waits to be stopped instead of exiting, so the restart
	// policy never starts it again.
	stopFile = "stopping"
	// serialFIFO carries the guest's serial console from Cloud Hypervisor
	// to the launcher.
	serialFIFO = "serial.fifo"

	// ConsoleLog is the guest's framed console (internal/guest/console):
	// what the container wrote, and how it ended. It is rotated to
	// ConsoleLog.1.
	ConsoleLog = "console.log"
	// SerialLog is the guest kernel's console, rotated to SerialLog.1.
	SerialLog = "serial.log"
)

// volumeNode is the node of the spec's n-th volume, from 0.
func volumeNode(n int) string { return fmt.Sprintf("%s%d", volumeNodePrefix, n+1) }

// dataDisks are the devices of the disks a VM has after its boot disk, vdc
// on, each with a volumeNode: a file share's server's share, a converter's
// target, after its file, or the spec's volumes.
func dataDisks(s driver.VMSpec) []string {
	if s.FileShareServer != nil {
		return []string{s.FileShareServer.Volume.Path}
	}
	if s.Converter != nil {
		if s.Converter.Target == "" {
			return nil
		}
		return []string{s.Converter.Target}
	}
	out := make([]string, len(s.Disks))
	for n, v := range s.Disks {
		out[n] = v.Path
	}
	return out
}

// readFiles are the files of a VM's directory that Cloud Hypervisor reads.
func readFiles(s driver.VMSpec) []string {
	switch {
	case s.Firmware:
		return nil
	case s.FileShareServer != nil, s.Converter != nil:
		return []string{bootFile, blankFile}
	}
	return []string{bootFile}
}

// blankVDA says a VM has a blank file at vda, which it never reads, in
// place of a root disk.
func blankVDA(s driver.VMSpec) bool { return s.FileShareServer != nil || s.Converter != nil }

// blankSize is the size of a blank vda.
const blankSize = 1 << 20

// logFiles are what Stop keeps of a VM's directory.
var logFiles = []string{ConsoleLog, ConsoleLog + ".1", SerialLog, SerialLog + ".1"}

// Exit codes the launcher ends with when it has no exit code of the
// container's to end with. Either is a failure to the restart policy. A
// container may exit with either itself, which reads the same: the exit
// record's Error tells ExitFailed apart.
const (
	// ExitNoRecord means Cloud Hypervisor ended without the guest having
	// recorded how its process ended: the VM crashed, or was killed.
	ExitNoRecord = 253
	// ExitFailed means the launcher could not start the VM, such as for a
	// root disk that is not the machine's.
	ExitFailed = 252
)

// How long each step of a stop may take. A stop asks the guest to stop its
// process (stopAsk), waits for the guest to power off (the grace and the
// slack), then stops Cloud Hypervisor (killGrace) and reads the rest of
// its console (drainWait).
const (
	// DefaultStopSlack is how much longer than the grace a guest has to
	// power off, unless Config.StopSlack says otherwise.
	DefaultStopSlack = 5 * time.Second
	// stopAsk bounds asking the guest to stop: a guest that does not take
	// the stop within it is killed at once.
	stopAsk = 3 * time.Second
	// killGrace is how long Cloud Hypervisor has to exit once SIGTERMed
	// before it is killed, unless Launcher.KillGrace says otherwise.
	killGrace = 2 * time.Second
	// drainWait bounds reading the console once Cloud Hypervisor exits.
	drainWait = 5 * time.Second
)

// stopTimeout is the unit's TimeoutStopSec: a stop of the unit by
// systemd, as at host shutdown, takes each step of a stop at its longest,
// so systemd never kills the launcher before the guest has had its grace.
func stopTimeout(grace, slack time.Duration) time.Duration {
	return stopAsk + grace + slack + killGrace + drainWait
}

// launch is the spec file: the VM and what the launcher starts it with.
type launch struct {
	Spec driver.VMSpec
	// Hash is Spec.Hash() when it was written.
	Hash            string
	CloudHypervisor string
	Kernel          string
	// Initramfs is the platform's, or a converter VM's the converter
	// initramfs.
	Initramfs string
	Cmdline   string
	// Firmware is what a firmware VM (Spec.Firmware) boots, in place of
	// the kernel and initramfs.
	Firmware string `json:",omitempty"`
	// StopGrace and StopSlack are the driver's Config.StopGrace and
	// StopSlack, which the launcher gives a guest when systemd stops it;
	// zero, as in a spec written before they were, means the defaults.
	StopGrace, StopSlack time.Duration `json:",omitempty"`
	// VMUser is the uid, and the gid, Cloud Hypervisor runs as: the VM's
	// own, from the driver's range, held while this file is. Zero, as in a
	// spec written before it was, means Cloud Hypervisor runs as root.
	VMUser uint32 `json:",omitempty"`
}

// stopTimes are the grace and the slack the launch gives a guest to stop.
func (l launch) stopTimes() (grace, slack time.Duration) {
	grace, slack = l.StopGrace, l.StopSlack
	if grace <= 0 {
		grace = driver.StopGrace
	}
	if slack <= 0 {
		slack = DefaultStopSlack
	}
	return grace, slack
}

// exitRecord is the exit file.
type exitRecord struct {
	// Code is what the launcher exited with: the container's exit code, or
	// ExitNoRecord or ExitFailed.
	Code int
	// Signal is the signal that ended the container's process, if the
	// guest recorded one.
	Signal int `json:",omitempty"`
	// Error says why the launcher could not start the VM.
	Error string `json:",omitempty"`
	// Started and Ended bound the run.
	Started, Ended time.Time
}

// unitPrefix starts the name of every unit the driver makes, and of no
// other: a unit outside it is never touched.
const unitPrefix = "hangar-vm-"

func unitName(uid string) string { return unitPrefix + uid + ".service" }

// agentUnit is the node agent's unit as deploy/host installs it, which the
// VMs' units are ordered before so that a shutdown stops the agent first.
const agentUnit = "hangar-node.service"

// uidOfUnit returns the machine uid a unit's name is for, if it is one of
// the driver's names.
func uidOfUnit(name string) (string, bool) {
	uid, ok := strings.CutPrefix(name, unitPrefix)
	if !ok {
		return "", false
	}
	uid, ok = strings.CutSuffix(uid, ".service")
	return uid, ok && checkUID(uid) == nil
}

// uidPattern is what names a VM: a unit name and a directory name, short
// enough that the vsock socket's path fits a unix address.
var uidPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// maxUID is the longest uid uidPattern takes.
const maxUID = 64

func checkUID(uid string) error {
	if !uidPattern.MatchString(uid) {
		return fmt.Errorf("machine uid %q cannot name a VM", uid)
	}
	return nil
}

// writeAtomic writes a file whole or not at all.
func writeAtomic(path string, data []byte) error {
	return writeAtomicAs(path, data, nil)
}

// writeAtomicAs is writeAtomic, the file given to prepare before it is in
// place, as for its owner and mode.
func writeAtomicAs(path string, data []byte, prepare func(*os.File) error) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil && prepare != nil {
		err = prepare(f)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

func readLaunch(dir string) (launch, error) {
	var l launch
	b, err := os.ReadFile(filepath.Join(dir, specFile))
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return l, fmt.Errorf("%s: %w", filepath.Join(dir, specFile), err)
	}
	return l, nil
}

// readExit returns the exit record, if there is one.
func readExit(dir string) (exitRecord, bool, error) {
	var r exitRecord
	b, err := os.ReadFile(filepath.Join(dir, exitFile))
	if errors.Is(err, fs.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, false, fmt.Errorf("%s: %w", filepath.Join(dir, exitFile), err)
	}
	return r, true, nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func isSocket(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().Type() == fs.ModeSocket
}

// checkStatic refuses an executable that needs a dynamic loader, which a
// VM's unit, with an empty root, does not have.
func checkStatic(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("%s is dynamically linked; a VM's unit runs only static executables (CGO_ENABLED=0)", path)
		}
	}
	return nil
}
