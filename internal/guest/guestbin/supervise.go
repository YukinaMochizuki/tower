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
	"sync"
	"syscall"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest/proclog"
)

// startSupervised starts a process's supervisor, tower-guest supervise,
// in a session of its own, so it outlives the call and the shim; the
// command, its environment and directory go to it through a pipe, never a
// file or a command line. The supervisor makes the process's directory,
// h.Root/ID, which its command line names: cgroup.Sweep reads it there
// to tell a supervisor that lives from one that died. It returns the
// process's id once the supervisor says the command runs.
func startSupervised(h Header) (string, error) {
	if err := os.MkdirAll(h.Root, 0o700); err != nil {
		return "", err
	}
	// A new id: not one of a process there; the supervisor makes none
	// over one there either.
	var id string
	for i := 0; ; i++ {
		id = proclog.NewID(time.Now().Add(time.Duration(i)))
		if _, err := os.Lstat(filepath.Join(h.Root, id)); errors.Is(err, os.ErrNotExist) {
			break
		}
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
	cmd := exec.Command(self, "supervise", filepath.Join(h.Root, id))
	cmd.ExtraFiles = []*os.File{specR, readyW} // fds 3 and 4
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	_ = specR.Close()
	_ = readyW.Close()
	spec, _ := json.Marshal(Header{Name: h.Name, Command: h.Command, Env: h.Env, Dir: h.Dir, LogCap: h.LogCap, Keep: h.Keep, Measure: h.Measure, MemoryMax: h.MemoryMax, As: h.As})
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

// supervise is tower-guest supervise DIR: it makes the process's
// directory, DIR, ROOT/ID, holds its lock, runs its command in a process
// group of its own with what fd 3 says, tells fd 4 once it runs,
// keeps its output in its capped log, and writes how it ended. A command
// that drops to a user (runas.go) starts as that user, the supervisor
// staying root, outside the command's process group: its files are
// root's, readable to the user (proclog.Readable).
func supervise(args []string) int {
	if len(args) != 1 {
		return 2
	}
	root, id := filepath.Dir(args[0]), filepath.Base(args[0])
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
	if len(h.Command) == 0 {
		return fail(errors.New("no command"))
	}
	as := dropTo(h.As)
	perm := proclog.Private
	if as != nil {
		perm = proclog.Readable
	}
	now := time.Now()
	dir, err := proclog.Create(root, id, perm)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = dir.Close() }()
	started := false
	defer func() {
		if !started {
			forget(root, id)
		}
	}()
	meta := proclog.Meta{ID: id, Name: h.Name, Command: h.Command, Dir: workingDir(h.Dir), Started: now.UTC(), LogCap: h.LogCap}
	if err := proclog.WriteMeta(dir, meta, perm); err != nil {
		return fail(err)
	}
	lock, err := proclog.Lock(dir, perm)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = lock.Close() }()
	// A hangup is not this process's end. Caught rather than ignored: an
	// ignored signal stays ignored in the command it runs.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGHUP)
	log, err := proclog.NewWriter(dir, meta.LogCap, perm)
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
		// A group of its own, the supervisor not in it: a signal its user
		// sends the group never reaches the supervisor, which writes its
		// exit however the command ends.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if as != nil {
			cmd.SysProcAttr.Credential = credential(as)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, outW, errW
		return cmd
	}
	cmd, group, err := startMeasured(newCmd, h, id)
	if err != nil {
		return fail(err)
	}
	startedAt := time.Now()
	if group != nil {
		// Removed once its exit is written, below.
		defer group.remove()
	}
	_ = outW.Close()
	_ = errW.Close()
	_ = devnull.Close()
	if err := proclog.WritePID(dir, cmd.Process.Pid, perm); err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		return fail(err)
	}
	started = true
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
	wall := time.Since(startedAt)
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
	if err := proclog.WriteExit(dir, exit, perm); err != nil {
		return 1
	}
	// The finished ones beyond what is kept go as it ends, the oldest
	// first: those killed with an earlier run of the guest among them.
	if h.Keep > 0 {
		_ = proclog.Prune(root, h.Keep)
	}
	return 0
}

// forget removes a process's directory that never ran, by its name under
// root, confined to root: one put in its place is removed, nothing
// outside it.
func forget(root, id string) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return
	}
	_ = r.RemoveAll(id)
	_ = r.Close()
}
