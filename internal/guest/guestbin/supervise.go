package guestbin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest/proclog"
)

// startSupervised makes a process's directory and starts its supervisor,
// tower-guest supervise, in a session of its own, so it outlives the call
// and the shim; the command, its environment and directory go to it
// through a pipe, never a file or a command line. It returns the
// process's id once the supervisor says the command runs. One whose
// command drops to a user (h.As) has its directory given to that user.
func startSupervised(h Header) (string, error) {
	own := owner(dropTo(h.As))
	_, err := os.Lstat(h.Root)
	made := errors.Is(err, os.ErrNotExist)
	if err := os.MkdirAll(h.Root, 0o700); err != nil {
		return "", err
	}
	if made {
		if err := own.Own(h.Root); err != nil {
			return "", err
		}
	}
	var id, dir string
	for {
		id = proclog.NewID(time.Now())
		dir = filepath.Join(h.Root, id)
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	if err := own.Own(dir); err != nil {
		_ = os.Remove(dir)
		return "", err
	}
	meta := proclog.Meta{ID: id, Name: h.Name, Command: h.Command, Dir: workingDir(h.Dir), Started: time.Now().UTC(), LogCap: h.LogCap}
	if err := proclog.WriteMeta(dir, meta, own); err != nil {
		return "", err
	}
	specR, specW, err := os.Pipe()
	if err != nil {
		return "", err
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(self, "supervise", dir)
	cmd.ExtraFiles = []*os.File{specR, readyW} // fds 3 and 4
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	_ = specR.Close()
	_ = readyW.Close()
	spec, _ := json.Marshal(Header{Command: h.Command, Env: h.Env, Dir: h.Dir, Keep: h.Keep, Measure: h.Measure, MemoryMax: h.MemoryMax, As: h.As})
	_, werr := specW.Write(spec)
	_ = specW.Close()
	msg, rerr := io.ReadAll(readyR)
	_ = readyR.Close()
	// The supervisor runs on, a child of init once this shim exits.
	_ = cmd.Process.Release()
	if err := errors.Join(werr, rerr); err != nil {
		return "", err
	}
	if string(msg) != "ok" {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("starting %s: %s", h.Command[0], msg)
	}
	return id, nil
}

// workingDir is the directory a command asked to run in dir runs in: dir
// against this process's own, which the supervisor and the command keep;
// empty if that cannot be read.
func workingDir(dir string) string {
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Join(wd, dir)
}

// supervise is tower-guest supervise DIR: it holds the process's lock,
// runs its command in a process group of its own with what fd 3 says,
// tells fd 4 it runs, keeps its output in its capped log, and writes how
// it ended. A command that drops to a user (runas.go) starts as that
// user, and the files the supervisor makes are that user's.
func supervise(args []string) int {
	if len(args) != 1 {
		return 2
	}
	dir := args[0]
	// Neither pipe reaches the command: the ready pipe open in it would
	// keep the starting call waiting for its end.
	syscall.CloseOnExec(3)
	syscall.CloseOnExec(4)
	spec, ready := os.NewFile(3, "spec"), os.NewFile(4, "ready")
	fail := func(err error) int {
		_, _ = io.WriteString(ready, err.Error())
		_ = ready.Close()
		return 1
	}
	b, err := io.ReadAll(spec)
	_ = spec.Close()
	if err != nil {
		return fail(err)
	}
	var h Header
	if err := json.Unmarshal(b, &h); err != nil {
		return fail(err)
	}
	b, err = os.ReadFile(filepath.Join(dir, proclog.MetaFile))
	if err != nil {
		return fail(err)
	}
	var meta proclog.Meta
	if err := json.Unmarshal(b, &meta); err != nil {
		return fail(err)
	}
	as := dropTo(h.As)
	own := owner(as)
	lock, err := proclog.Lock(dir, own)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = lock.Close() }()
	// A hangup is not this process's end. Caught rather than ignored: an
	// ignored signal stays ignored in the command it runs.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGHUP)
	log, err := proclog.NewWriter(dir, meta.LogCap, own)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = log.Close() }()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return fail(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	newCmd := func() *exec.Cmd {
		cmd := exec.Command(h.Command[0], h.Command[1:]...)
		cmd.Env, cmd.Dir = commandEnv(h.Env, as), h.Dir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if as != nil {
			cmd.SysProcAttr.Credential = credential(as)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, outW, errW
		return cmd
	}
	cmd, group, err := startMeasured(newCmd, h, filepath.Base(dir))
	if err != nil {
		return fail(err)
	}
	started := time.Now()
	if group != nil {
		// Removed once its exit is written, below.
		defer group.remove()
	}
	_ = outW.Close()
	_ = errW.Close()
	_ = devnull.Close()
	pidFile := filepath.Join(dir, proclog.PIDFile)
	err = os.WriteFile(pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	if err == nil {
		err = own.Own(pidFile)
	}
	if err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return fail(err)
	}
	_, _ = io.WriteString(ready, "ok")
	_ = ready.Close()

	var mu sync.Mutex
	var wg sync.WaitGroup
	copyStream := func(r *os.File, stderr bool) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				mu.Lock()
				// A log that cannot be written loses output, never the
				// process.
				_ = log.Write(stderr, buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go copyStream(outR, false)
	go copyStream(errR, true)
	werr := cmd.Wait()
	wall := time.Since(started)
	// What it wrote is read to the end; a child it left holding its
	// output open is not waited for long.
	copied := make(chan struct{})
	go func() { wg.Wait(); close(copied) }()
	select {
	case <-copied:
	case <-time.After(500 * time.Millisecond):
		_ = outR.Close()
		_ = errR.Close()
		<-copied
	}
	var ee *exec.ExitError
	if werr != nil && !errors.As(werr, &ee) {
		return 1
	}
	code, sig := exitOf(cmd.ProcessState)
	exit := proclog.Exit{Code: code, Signal: sig}
	if group != nil {
		exit.Usage = group.usage(wall)
	}
	if err := proclog.WriteExit(dir, exit, own); err != nil {
		return 1
	}
	// The finished ones beyond what is kept go as it ends, the oldest
	// first: those killed with an earlier run of the guest among them.
	if h.Keep > 0 {
		_ = proclog.Prune(filepath.Dir(dir), h.Keep)
	}
	return 0
}
