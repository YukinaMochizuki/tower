package chvm

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/YukinaMochizuki/hangar/internal/guest/console"
	"github.com/YukinaMochizuki/hangar/internal/node/driver"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/chguest"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/hostnet"
	"github.com/YukinaMochizuki/hangar/internal/node/driver/zfsstore"
)

// Log sizes: a file of the guest's console holds this much before it is
// rotated, so a VM keeps at most twice as much of either.
const (
	DefaultConsoleMax = 16 << 20
	DefaultSerialMax  = 1 << 20
)

// Launcher runs one VM in the foreground: it is hangar-node vmm, the main
// process of the VM's unit, started again by systemd as the restart policy
// says. It needs nothing of the node agent: what it runs is in the VM's
// directory. It opens the VM's disks, each checked to be what the spec
// names, and its taps, and runs Cloud Hypervisor on them as the VM's user,
// with no capabilities and only the taps' fds (ExecMain); it keeps the
// guest's console in a log of bounded size, and exits as the container's
// process did, so systemd's restart policy and restart count see the
// container. Stopped (ctx ends, on SIGTERM), it stops Cloud Hypervisor and
// exits, recording no exit: stopped by the driver, which has stopped the
// guest already, at once; stopped by systemd alone, as at host shutdown,
// once it has stopped the guest as the driver's Stop does, so the container
// gets its SIGTERM and grace. Either way the VM reads as stopped or
// interrupted, never as exited, however its process ended.
type Launcher struct {
	// Dir holds the VMs' directories, as the driver's Config.Dir.
	Dir string
	// OpenDisk opens a disk at path for the VM, the root disk or a volume,
	// refusing one that is not what path names. Nil means OpenZvol.
	OpenDisk func(path string) (*os.File, error)
	// OpenTap opens an interface's tap. Nil means hostnet.OpenTap.
	OpenTap func(driver.Interface) (*os.File, error)
	// ConsoleMax and SerialMax bound each file of the logs; zero means
	// DefaultConsoleMax and DefaultSerialMax.
	ConsoleMax, SerialMax int64
	// KillGrace bounds how long Cloud Hypervisor has to exit once
	// SIGTERMed before it is killed; zero means 2 seconds.
	KillGrace time.Duration
	// Guest reaches the guest's agent. Nil means chguest's, at the VM's
	// vsock socket.
	Guest driver.Guest
	// PowerButton presses the power button of a firmware VM, whose Cloud
	// Hypervisor serves its API at socket. Nil means pressPowerButton.
	PowerButton func(ctx context.Context, socket string) error
	Log         *slog.Logger

	// VMUser is the id the unit runs Cloud Hypervisor as, which the spec
	// file must hold too. Zero is a unit that runs it as root, as a driver
	// without VM users started them: such a VM runs as it did until it is
	// started again, in a unit of its user's.
	VMUser uint32
	// MakeNode makes at path a node of the device f is open on, the
	// group's to read and write. Nil means mknodOf.
	MakeNode func(f *os.File, path string, gid int) error
	// Exec runs Cloud Hypervisor as the VM's user: hangar-node vmm-exec,
	// to which the launcher adds the flags of ExecMain. Nil means
	// /proc/self/exe vmm-exec.
	Exec []string
	// KVM is the KVM device, whose group Cloud Hypervisor gets if it
	// needs it. Empty means /dev/kvm.
	KVM string
	// UnmountNetns removes the networks' namespaces from the unit's mount
	// namespace once the taps are open, so Cloud Hypervisor never sees
	// them. Nil means unmountNetns.
	UnmountNetns func() error
	// DropCaps leaves the launcher only what it needs once Cloud
	// Hypervisor runs, with a VM user. Nil means dropCaps(launcherKeeps).
	DropCaps func() error
}

// LauncherMain is hangar-node vmm [--dir DIR] UID, returning its exit code.
func LauncherMain(args []string) int {
	fs := flag.NewFlagSet("vmm", flag.ContinueOnError)
	dir := fs.String("dir", DefaultDir, "the directory of the VMs' directories")
	user := fs.Uint("vm-user", 0, "the id Cloud Hypervisor runs as; 0, or none, runs it as root")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 || *user > 1<<31 {
		fmt.Fprintln(os.Stderr, "usage: hangar-node vmm [--dir DIR] [--vm-user ID] MACHINE_UID")
		return ExitFailed
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	l := &Launcher{Dir: *dir, VMUser: uint32(*user), Log: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	return l.Run(ctx, fs.Arg(0))
}

// OpenZvol opens the zvol a /dev/zvol link names, refusing what it leads to
// unless the device says, by BLKZNAME, that it is that zvol: a stale link
// can lead to another machine's disk (docs/decisions/storage-zfs.md).
func OpenZvol(path string) (*os.File, error) {
	want, ok := strings.CutPrefix(path, "/dev/zvol/")
	if !ok || want == "" || filepath.Clean(path) != path {
		return nil, fmt.Errorf("%s is not a zvol's link", path)
	}
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_EXCL, 0)
	if err != nil {
		return nil, err
	}
	got, err := zfsstore.VolumeOf(f)
	if err == nil && got != want {
		err = fmt.Errorf("%s leads to the zvol %s", path, got)
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("refusing %s: %w", path, err)
	}
	return f, nil
}

func (l *Launcher) defaults() {
	if l.OpenDisk == nil {
		l.OpenDisk = OpenZvol
	}
	if l.OpenTap == nil {
		l.OpenTap = hostnet.OpenTap
	}
	if l.ConsoleMax == 0 {
		l.ConsoleMax = DefaultConsoleMax
	}
	if l.SerialMax == 0 {
		l.SerialMax = DefaultSerialMax
	}
	if l.KillGrace == 0 {
		l.KillGrace = killGrace
	}
	if l.Guest == nil {
		l.Guest = chguest.Guest{Socket: func(uid string) string { return filepath.Join(l.Dir, uid, vsockFile) }}
	}
	if l.PowerButton == nil {
		l.PowerButton = pressPowerButton
	}
	if l.Log == nil {
		l.Log = slog.New(slog.DiscardHandler)
	}
	if l.MakeNode == nil {
		l.MakeNode = mknodOf
	}
	if l.Exec == nil {
		l.Exec = []string{"/proc/self/exe", "vmm-exec"}
	}
	if l.KVM == "" {
		l.KVM = "/dev/kvm"
	}
	if l.UnmountNetns == nil {
		l.UnmountNetns = unmountNetns
	}
	if l.DropCaps == nil {
		l.DropCaps = func() error { return dropCaps(launcherKeeps) }
	}
}

// Run runs the machine's VM until it powers off, or ctx ends, and returns
// the code to exit with.
func (l *Launcher) Run(ctx context.Context, uid string) int {
	l.defaults()
	// No fd the launcher was started with reaches Cloud Hypervisor: it
	// gets only what the launcher passes it (command).
	if err := unix.CloseRange(3, ^uint(0), unix.CLOSE_RANGE_CLOEXEC); err != nil {
		l.Log.Error("vmm", "error", fmt.Errorf("close_range: %w", err))
		return ExitFailed
	}
	if err := checkUID(uid); err != nil {
		l.Log.Error("vmm", "error", err)
		return ExitFailed
	}
	dir := filepath.Join(l.Dir, uid)
	started := time.Now()
	// A record of an earlier run would say this one ended.
	if err := os.Remove(filepath.Join(dir, exitFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		l.Log.Error("vmm: removing the last exit record", "error", err)
	}
	code, err := l.run(ctx, uid, dir, started)
	if err != nil {
		l.Log.Error("vmm: the VM did not start", "machine_uid", uid, "error", err)
		l.record(dir, exitRecord{Code: ExitFailed, Error: err.Error(), Started: started, Ended: time.Now()})
		code = ExitFailed
	}
	if code < 0 { // stopped: no record, and no restart either way
		return 0
	}
	// A VM being stopped is not started again: the launcher waits for the
	// stop, so systemd sees no exit to restart it for.
	if exists(filepath.Join(dir, stopFile)) {
		l.Log.Info("vmm: the guest is off, waiting to be stopped", "machine_uid", uid)
		<-ctx.Done()
	}
	return code
}

// run runs the VM. It returns the code to exit with, or -1 if ctx ended,
// and an error if the VM could not be started.
func (l *Launcher) run(ctx context.Context, uid, dir string, started time.Time) (int, error) {
	ln, err := readLaunch(dir)
	if err != nil {
		return 0, err
	}
	if ln.Spec.MachineUID != uid {
		return 0, fmt.Errorf("%s is machine %q's, not %q's", filepath.Join(dir, specFile), ln.Spec.MachineUID, uid)
	}
	if ln.VMUser != l.VMUser {
		return 0, fmt.Errorf("the unit runs Cloud Hypervisor as %d, %s says %d", l.VMUser, filepath.Join(dir, specFile), ln.VMUser)
	}
	if err := checkOwn(filepath.Join(dir, specFile)); err != nil {
		return 0, err
	}
	// The VM's group; root's, with no VM user.
	gid := int(ln.VMUser)
	if err := prepareDir(dir, gid, readFiles(ln.Spec)); err != nil {
		return 0, err
	}
	// Held open while the VM runs, O_EXCL: no other zvol can take its
	// device's number, which the node names. A firmware VM boots its first
	// volume instead, and a file share's server and a converter have a
	// blank file.
	if !ln.Spec.Firmware && !blankVDA(ln.Spec) {
		root, err := l.OpenDisk(ln.Spec.Root.Path)
		if err != nil {
			return 0, err
		}
		defer func() { _ = root.Close() }()
		if err := l.MakeNode(root, filepath.Join(dir, diskNode), gid); err != nil {
			return 0, fmt.Errorf("the root disk's node: %w", err)
		}
	}
	if c := ln.Spec.Converter; c != nil {
		if err := checkConverterFile(c.File, gid); err != nil {
			return 0, err
		}
	}
	var vols []*os.File
	defer func() {
		for _, f := range vols {
			_ = f.Close()
		}
	}()
	for n, path := range dataDisks(ln.Spec) {
		f, err := l.OpenDisk(path)
		if err != nil {
			return 0, fmt.Errorf("disk %s: %w", path, err)
		}
		vols = append(vols, f)
		if err := l.MakeNode(f, filepath.Join(dir, volumeNode(n)), gid); err != nil {
			return 0, fmt.Errorf("disk %s's node: %w", path, err)
		}
	}
	var taps []*os.File
	defer func() {
		for _, f := range taps {
			_ = f.Close()
		}
	}()
	for _, i := range ln.Spec.Interfaces {
		tap, err := l.OpenTap(i)
		if err != nil {
			return 0, err
		}
		taps = append(taps, tap)
	}
	if ln.VMUser != 0 {
		if err := l.UnmountNetns(); err != nil {
			return 0, fmt.Errorf("unmounting the networks' namespaces: %w", err)
		}
	}

	clog, err := openCapped(filepath.Join(dir, ConsoleLog), l.ConsoleMax)
	if err != nil {
		return 0, err
	}
	defer func() { _ = clog.Close() }()
	l.markBoot(clog, started)
	// A firmware VM's serial console is its console log; a container's
	// is the guest kernel's, kept apart.
	var serialLog io.Writer = clog
	if !ln.Spec.Firmware {
		raw, err := openCapped(filepath.Join(dir, SerialLog), l.SerialMax)
		if err != nil {
			return 0, err
		}
		defer func() { _ = raw.Close() }()
		serialLog = raw
	}
	serial, err := openFIFO(filepath.Join(dir, serialFIFO), gid)
	if err != nil {
		return 0, err
	}
	defer func() { _ = serial.Close() }()

	// The console comes on Cloud Hypervisor's stdout; its stdin, the
	// console's input, is held open and never written.
	outR, outW, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer func() { _ = outR.Close() }()
	inR, inW, err := os.Pipe()
	if err != nil {
		_ = outW.Close()
		return 0, err
	}
	defer func() { _ = inW.Close() }()
	cmd, err := l.command(ln, dir, len(taps))
	if err != nil {
		_ = outW.Close()
		_ = inR.Close()
		return 0, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, os.Stderr
	cmd.ExtraFiles = taps // fds 3 and on, and nothing else
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	err = cmd.Start()
	_ = inR.Close()
	_ = outW.Close()
	if err != nil {
		return 0, err
	}
	if ln.VMUser != 0 {
		if err := l.DropCaps(); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return 0, fmt.Errorf("giving up root's capabilities: %w", err)
		}
	}
	l.Log.Info("vmm: started", "machine_uid", uid, "machine", ln.Spec.Machine, "pid", cmd.Process.Pid, "spec", ln.Hash)

	consoleDone := make(chan *console.ExitStatus, 1)
	logErr := func(err error) { l.Log.Error("vmm: the console log fails; dropping records", "error", err) }
	if ln.Spec.Firmware {
		// No virtio console: its stdout carries nothing of the guest's.
		go func() {
			_, _ = io.Copy(io.Discard, outR)
			consoleDone <- nil
		}()
	} else {
		go func() { consoleDone <- pumpConsole(outR, clog, logErr) }()
	}
	serialDone := make(chan struct{})
	framer := &serialFramer{log: clog, logErr: logErr, now: time.Now}
	go func() {
		defer close(serialDone)
		if ln.Spec.Firmware {
			framer.pump(serial)
			return
		}
		pumpRaw(serial, serialLog, func(err error) {
			l.Log.Error("vmm: the serial log fails; dropping output", "error", err)
		})
	}()
	var endSerial sync.Once
	stopSerial := func() {
		endSerial.Do(func() {
			if ln.Spec.Firmware {
				framer.quiet()
			}
			_ = serial.Close()
			<-serialDone
		})
	}
	defer stopSerial()
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		// The driver marks a VM before it stops the guest: without the
		// mark, systemd stops the unit on its own.
		poweredOff := !exists(filepath.Join(dir, stopFile)) && l.stopGuest(uid, ln, waited)
		if !poweredOff {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-waited:
			case <-time.After(l.KillGrace):
				_ = cmd.Process.Kill()
				<-waited
			}
		}
		// The log, not exit.json: a stop is no exit to the driver. A
		// firmware VM's boot ends as the platform's, by the driver's stop
		// or the host's shutdown alike.
		st, drained := l.drain(consoleDone)
		switch {
		case ln.Spec.Firmware:
			stopSerial()
			endBoot(clog, poweredOff, true)
		case st == nil && drained:
			noExit(clog)
		}
		l.Log.Info("vmm: stopped", "machine_uid", uid)
		return -1, nil
	case err := <-waited:
		st, drained := l.drain(consoleDone)
		code := exitCode(st)
		switch {
		case ln.Spec.Firmware:
			// A firmware VM records no exit: Cloud Hypervisor ending well
			// is its guest powering off, on its own unless the driver
			// pressed its power button, marking it as being stopped.
			stopSerial()
			code = endBoot(clog, err == nil, exists(filepath.Join(dir, stopFile)))
		case st == nil && drained:
			noExit(clog)
		}
		rec := exitRecord{Code: code, Started: started, Ended: time.Now()}
		if st != nil {
			rec.Signal = int(st.Signal)
		}
		l.record(dir, rec)
		l.Log.Info("vmm: the VM ended", "machine_uid", uid, "cloud_hypervisor", errString(err), "exit_code", code, "recorded", st != nil)
		return code, nil
	}
}

// stopGuest asks the guest to stop its process with the launch's grace, or
// presses a firmware VM's power button, as the driver's Stop does, and
// waits for the guest to power off, Cloud Hypervisor exiting, for the grace
// and the slack. It reports whether Cloud Hypervisor has exited, and
// returns at once if the guest does not take the stop.
func (l *Launcher) stopGuest(uid string, ln launch, waited <-chan error) bool {
	grace, slack := ln.stopTimes()
	l.Log.Info("vmm: stopped by systemd; stopping the guest", "machine_uid", uid, "grace", grace)
	ctx, cancel := context.WithTimeout(context.Background(), stopAsk)
	var err error
	if ln.Spec.Firmware {
		err = l.PowerButton(ctx, filepath.Join(l.Dir, uid, apiFile))
	} else {
		err = l.Guest.Stop(ctx, uid, grace)
	}
	cancel()
	if err != nil {
		l.Log.Error("vmm: the guest did not take the stop", "machine_uid", uid, "error", err)
		return false
	}
	deadline := time.NewTimer(grace + slack)
	defer deadline.Stop()
	select {
	case <-waited:
		return true
	case <-deadline.C:
		l.Log.Error("vmm: the guest did not power off in its grace", "machine_uid", uid)
		return false
	}
}

// endBoot ends a firmware VM's boot in its console log, as a container's
// exit does, and returns the code it ended with: 0 if the guest powered
// off, ExitNoRecord if not. A boot the platform ended, stopped, has
// driver.StoppedSignal too.
func endBoot(clog *cappedLog, poweredOff, stopped bool) int {
	st := console.ExitStatus{Code: ExitNoRecord}
	if poweredOff {
		st.Code = 0
	}
	if stopped {
		st.Signal = driver.StoppedSignal
	}
	_, _ = clog.Write(console.AppendRecord(nil, console.ExitRecord(st, time.Now())))
	return int(st.Code)
}

// noExit ends a boot whose guest recorded no exit, as a crash or a kill
// leaves it, with an exit of ExitNoRecord in the console log, so the log
// shows every boot's end.
func noExit(clog *cappedLog) {
	_, _ = clog.Write(console.AppendRecord(nil, console.ExitRecord(console.ExitStatus{Code: ExitNoRecord}, time.Now())))
}

// markBoot numbers this boot in the console log, one past the last there,
// and starts each file the log rotates to with the same number, so a
// reader numbers boots alike whichever files it reads.
func (l *Launcher) markBoot(clog *cappedLog, started time.Time) {
	last, err := lastBoot(clog.path)
	if err != nil {
		l.Log.Error("vmm: reading the last boot's number; numbering from it anyway", "error", err)
	}
	mark := console.AppendRecord(nil, console.BootRecord(last+1, started))
	clog.head = mark
	if _, err := clog.Write(mark); err != nil {
		l.Log.Error("vmm: marking the boot in the console log", "error", err)
	}
}

// drain waits a little for the rest of the console once Cloud Hypervisor
// has exited, and reports whether the console ended.
func (l *Launcher) drain(done <-chan *console.ExitStatus) (*console.ExitStatus, bool) {
	select {
	case st := <-done:
		return st, true
	case <-time.After(drainWait):
		l.Log.Error("vmm: the console did not end with Cloud Hypervisor")
		return nil, false
	}
}

func errString(err error) string {
	if err == nil {
		return "exited 0"
	}
	return err.Error()
}

func (l *Launcher) record(dir string, r exitRecord) {
	b, _ := json.Marshal(r)
	if err := writeAtomic(filepath.Join(dir, exitFile), b); err != nil {
		l.Log.Error("vmm: recording the exit", "error", err)
	}
}

// openFIFO makes a FIFO at path, afresh, for the group gid to write to, and
// opens it to read from. It is opened for writing too, so it never reads as
// ended between writers.
func openFIFO(path string, gid int) (*os.File, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if gid != 0 {
		if err := setGroup(f, gid, 0o620); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}

// prepareDir makes the VM's directory and reads, what Cloud Hypervisor
// reads in it, the group gid's, and chDir afresh, where it writes. With no
// VM user (gid 0) they are root's alone. Nothing in the directory is Cloud
// Hypervisor's to remove or replace, nor to read but what it needs.
func prepareDir(dir string, gid int, reads []string) error {
	dirMode, readMode, chMode := os.FileMode(0o700), readableMode(gid), os.FileMode(0o700)
	if gid != 0 {
		dirMode, chMode = 0o710, 0o770
	}
	type file struct {
		name string
		mode os.FileMode
	}
	files := []file{{"", dirMode}}
	for _, name := range reads {
		files = append(files, file{name, readMode})
	}
	for _, p := range files {
		f, err := os.OpenFile(filepath.Join(dir, p.name), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		err = setGroup(f, gid, p.mode)
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	nodes, err := filepath.Glob(filepath.Join(dir, volumeNodePrefix+"*"))
	if err != nil {
		return err
	}
	for _, path := range append(nodes, filepath.Join(dir, legacyVsockFile), filepath.Join(dir, diskNode)) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	ch := filepath.Join(dir, chDir)
	if err := os.RemoveAll(ch); err != nil {
		return err
	}
	if err := os.Mkdir(ch, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(ch, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return setGroup(f, gid, chMode)
}

// checkConverterFile refuses a converter's file unless it is a regular
// file of root's, which the VM's group may read and no one but root write,
// as the driver left it: Cloud Hypervisor opens it by its path.
func checkConverterFile(path string, gid int) error {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return fmt.Errorf("the converter's file: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != os.Geteuid() || int(st.Gid) != gid || st.Mode&0o027 != 0 {
		return fmt.Errorf("%s is not a file of the launcher's user's, the VM's group's to read alone", path)
	}
	return nil
}

// readableMode is the mode of what Cloud Hypervisor reads in the VM's
// directory: the group gid's to read, or root's alone for gid 0.
func readableMode(gid int) os.FileMode {
	if gid != 0 {
		return 0o640
	}
	return 0o600
}

// checkOwn refuses a file the launcher would run what it says of, unless it
// is a regular file the launcher's user alone can write.
func checkOwn(path string) error {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != os.Geteuid() || st.Mode&0o022 != 0 {
		return fmt.Errorf("%s is not a file of the launcher's user's alone", path)
	}
	return nil
}

// setGroup gives f to root and the group gid, with mode. It is root's
// already, or the launcher's when it is not root, as in tests.
func setGroup(f *os.File, gid int, mode os.FileMode) error {
	// Not root, a test cannot give a file to root's group.
	if err := f.Chown(os.Geteuid(), gid); err != nil && (os.Geteuid() == 0 || gid != 0) {
		return err
	}
	return f.Chmod(mode)
}

// mknodOf makes at path a block device node of the device f is open on,
// root's and the group gid's to read and write.
func mknodOf(f *os.File, path string, gid int) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFBLK {
		return fmt.Errorf("%s is no block device", f.Name())
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := unix.Mknod(path, unix.S_IFBLK|0o600, int(st.Rdev)); err != nil {
		return err
	}
	// By path: nothing but root writes in the VM's directory.
	if err := os.Lchown(path, 0, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o660)
}

// unmountNetns detaches /run/netns from the unit's mount namespace, and
// only from a VM unit's: one whose root is the empty tmpfs it has.
func unmountNetns() error {
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err != nil {
		return err
	}
	if st.Type != unix.TMPFS_MAGIC {
		return errors.New("the root is the host's: this is no VM unit's mount namespace")
	}
	err := unix.Unmount("/run/netns", unix.MNT_DETACH)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
		return nil // not mounted: the host has no network
	}
	return err
}

// command is what runs Cloud Hypervisor for the launch: with a VM user,
// ExecMain, which runs it as that user with the KVM device's group if it
// needs one; with none, Cloud Hypervisor itself, as root.
func (l *Launcher) command(ln launch, dir string, taps int) (*exec.Cmd, error) {
	args := chArgs(ln, dir, taps)
	if ln.Spec.Firmware {
		args = firmwareArgs(ln, dir, taps)
	}
	if ln.VMUser == 0 {
		return exec.Command(ln.CloudHypervisor, args...), nil
	}
	groups, err := kvmGroups(l.KVM)
	if err != nil {
		return nil, err
	}
	id := fmt.Sprint(ln.VMUser)
	argv := append(slices.Clone(l.Exec[1:]), "--uid", id, "--gid", id, "--groups", groups, "--fds", fmt.Sprint(taps), "--", ln.CloudHypervisor)
	return exec.Command(l.Exec[0], append(argv, args...)...), nil
}

// kvmGroups is the group the KVM device needs of its users, if any.
func kvmGroups(kvm string) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(kvm, &st); err != nil {
		return "", err
	}
	switch {
	case st.Mode&0o006 == 0o006:
		return "", nil
	case st.Mode&0o060 == 0o060 && st.Gid != 0:
		return fmt.Sprint(st.Gid), nil
	}
	return "", fmt.Errorf("%s is open to neither its group nor everyone", kvm)
}

// freePageReporting is the balloon every VM has: never inflated, it only
// carries the pages the guest reports free, which Cloud Hypervisor gives
// back to the host. Without it a guest's memory, once touched, stays in use
// on the host until the VM stops (docs/decisions/node-agent.md).
const freePageReporting = "size=0,free_page_reporting=on"

// chArgs is Cloud Hypervisor's command line for the launch: the root disk,
// or a blank file, the boot disk and the volumes, the server's share, or a
// converter's file, read-only and raw whatever it holds, and its target,
// at their nodes in dir, and the taps on fds 3 and on, as the launcher
// passes them.
func chArgs(ln launch, dir string, taps int) []string {
	s := ln.Spec
	vda := "path=" + filepath.Join(dir, diskNode) + ",image_type=raw"
	if blankVDA(s) {
		vda = "path=" + filepath.Join(dir, blankFile) + ",image_type=raw,readonly=on"
	}
	args := []string{
		"--kernel", ln.Kernel,
		"--initramfs", ln.Initramfs,
		"--cmdline", ln.Cmdline,
		"--cpus", fmt.Sprintf("boot=%d", s.VCPUs),
		"--memory", fmt.Sprintf("size=%dM", s.MemoryMiB),
		"--balloon", freePageReporting,
		"--disk",
		vda,
		"path=" + filepath.Join(dir, bootFile) + ",image_type=raw,readonly=on",
	}
	if c := s.Converter; c != nil {
		// The unit binds the file read-only; Cloud Hypervisor parses no
		// format of it.
		args = append(args, "path="+c.File+",image_type=raw,readonly=on")
	}
	for n := range dataDisks(s) {
		args = append(args, "path="+filepath.Join(dir, volumeNode(n))+",image_type=raw")
	}
	if taps > 0 {
		args = append(args, "--net")
		for n, i := range s.Interfaces[:taps] {
			args = append(args, fmt.Sprintf("fd=%d,mac=%s", 3+n, i.MAC))
		}
	}
	return append(args,
		"--vsock", "cid=3,socket="+filepath.Join(dir, vsockFile),
		"--console", "tty",
		"--serial", "file="+filepath.Join(dir, serialFIFO),
	)
}
