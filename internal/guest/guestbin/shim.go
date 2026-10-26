package guestbin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/YukinaMochizuki/tower/internal/guest/proclog"
	"github.com/YukinaMochizuki/tower/internal/guest/tarx"
)

// Main runs tower-guest with its arguments, returning its exit code.
func Main(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: tower-guest shim|init|supervise ...")
		return 2
	}
	switch args[0] {
	case "shim":
		return shim(os.Stdin, os.Stdout, os.Stderr)
	case "init":
		return initMain(args[1:])
	case "supervise":
		return supervise(args[1:])
	case "version":
		fmt.Println(Version)
		return 0
	}
	fmt.Fprintf(os.Stderr, "tower-guest: no command %q\n", args[0])
	return 2
}

// Version is tower-guest's, which hostd compares with its own to copy it
// into a VM again.
var Version = "dev"

// failed writes a shim's failure and returns its exit code.
func failed(stderr io.Writer, code int, err error) int {
	_, _ = fmt.Fprintf(stderr, "tower-guest: %v\n", err)
	return code
}

// shim runs the one operation its stdin's header names.
func shim(stdin io.Reader, stdout, stderr io.Writer) int {
	in := bufio.NewReader(stdin)
	h, err := readHeader(in)
	if err != nil {
		return failed(stderr, ExitFailed, fmt.Errorf("reading the header: %w", err))
	}
	switch h.Op {
	case OpExec:
		return execOp(h, in, stderr)
	case OpRead:
		return readOp(h, stdout, stderr)
	case OpWrite:
		return writeOp(h, in, stderr)
	case OpCopyOut:
		return copyOutOp(h, stdout, stderr)
	case OpCopyIn:
		return copyInOp(h, in, stderr)
	case OpStart:
		return startOp(h, stdout, stderr)
	case OpSignal:
		return signalOp(h, stderr)
	case OpList:
		return listOp(h, stdout, stderr)
	case OpLog:
		return logOp(h, in, stdout, stderr)
	}
	return failed(stderr, ExitFailed, fmt.Errorf("no operation %q", h.Op))
}

// environ is the shim's environment with env over it.
func environ(env map[string]string) []string {
	out := os.Environ()
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = slices.DeleteFunc(out, func(e string) bool { return strings.HasPrefix(e, k+"=") })
		out = append(out, k+"="+env[k])
	}
	return out
}

// exitOf is how a command ended: its code, or 128 plus the signal that
// ended it.
func exitOf(ps *os.ProcessState) (code, sig int) {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), int(ws.Signal())
	}
	return ps.ExitCode(), 0
}

// execOp runs a command in a process group of its own, its output the
// shim's, until it ends; the data frames are its stdin, a signal frame is
// sent to its group, and a stdin that ends without its end frame kills
// the group, as when the call that runs it is gone.
func execOp(h Header, in io.Reader, stderr io.Writer) int {
	if len(h.Command) == 0 {
		return failed(stderr, ExitCannotStart, errors.New("no command"))
	}
	cmd := exec.Command(h.Command[0], h.Command[1:]...)
	cmd.Env, cmd.Dir = environ(h.Env), h.Dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return failed(stderr, ExitCannotStart, err)
	}
	cmd.Stdin = stdinR
	if err := cmd.Start(); err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return failed(stderr, ExitCannotStart, err)
	}
	_ = stdinR.Close()
	pgid := cmd.Process.Pid
	go func() {
		defer func() { _ = stdinW.Close() }()
		for {
			typ, data, err := ReadFrame(in)
			if err != nil {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
				return
			}
			switch typ {
			case FrameData:
				// A command that does not read its stdin is no error.
				_, _ = stdinW.Write(data)
			case FrameEOF:
				_ = stdinW.Close()
			case FrameSignal:
				if len(data) == 1 {
					_ = syscall.Kill(-pgid, syscall.Signal(data[0]))
				}
			}
		}
	}()
	err = cmd.Wait()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return failed(stderr, ExitFailed, err)
	}
	code, _ := exitOf(cmd.ProcessState)
	return code
}

func readOp(h Header, stdout, stderr io.Writer) int {
	f, err := os.Open(h.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return failed(stderr, ExitNotExist, err)
	}
	if err != nil {
		return failed(stderr, ExitFailed, err)
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err == nil && st.IsDir() {
		return failed(stderr, ExitFailed, fmt.Errorf("%s is a directory", h.Path))
	}
	if _, err := io.Copy(stdout, f); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	return 0
}

// dataFrames reads the data frames to w until the end frame; a stdin that
// ends without it is an error.
func dataFrames(in io.Reader, w io.Writer) error {
	for {
		typ, data, err := ReadFrame(in)
		if err != nil {
			return fmt.Errorf("the data ended without its end: %w", err)
		}
		switch typ {
		case FrameData:
			if _, err := w.Write(data); err != nil {
				return err
			}
		case FrameEOF:
			return nil
		}
	}
}

// writeOp writes a file from the data frames, making its directory: at
// offset 0 into a new file renamed over it, at any other where the file
// ends.
func writeOp(h Header, in io.Reader, stderr io.Writer) int {
	if !filepath.IsAbs(h.Path) {
		return failed(stderr, ExitFailed, fmt.Errorf("%q is not an absolute path", h.Path))
	}
	mode := fs.FileMode(h.Mode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	if err := os.MkdirAll(filepath.Dir(h.Path), 0o755); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	if h.Offset != 0 {
		st, err := os.Stat(h.Path)
		if err != nil || st.Size() != h.Offset {
			// Nothing is read: the caller's write fails as a whole.
			return failed(stderr, ExitBadOffset, fmt.Errorf("%s does not end at %d", h.Path, h.Offset))
		}
		f, err := os.OpenFile(h.Path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return failed(stderr, ExitFailed, err)
		}
		err = dataFrames(in, f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return failed(stderr, ExitFailed, err)
		}
		return 0
	}
	f, err := os.CreateTemp(filepath.Dir(h.Path), "."+filepath.Base(h.Path)+".tower-*")
	if err != nil {
		return failed(stderr, ExitFailed, err)
	}
	err = dataFrames(in, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), mode)
	}
	if err == nil {
		err = os.Rename(f.Name(), h.Path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return failed(stderr, ExitFailed, err)
	}
	return 0
}

func copyOutOp(h Header, stdout, stderr io.Writer) int {
	if _, err := os.Lstat(h.Path); errors.Is(err, fs.ErrNotExist) {
		return failed(stderr, ExitNotExist, err)
	}
	if err := tarx.Write(stdout, h.Path); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	return 0
}

func copyInOp(h Header, in io.Reader, stderr io.Writer) int {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(dataFrames(in, pw)) }()
	if err := tarx.Extract(pr, h.Path); err != nil {
		_ = pr.CloseWithError(err)
		return failed(stderr, ExitFailed, err)
	}
	// What follows the tar's end, up to the end frame, is read and dropped.
	_, _ = io.Copy(io.Discard, pr)
	return 0
}

// startOp starts a process under a supervisor of its own, detached from
// this call, and writes its id once it runs.
func startOp(h Header, stdout, stderr io.Writer) int {
	if len(h.Command) == 0 || h.Root == "" {
		return failed(stderr, ExitFailed, errors.New("a process needs a command and its root"))
	}
	id, err := startSupervised(h)
	if err != nil {
		return failed(stderr, ExitFailed, err)
	}
	if _, err := fmt.Fprintln(stdout, id); err != nil {
		return ExitFailed
	}
	return 0
}

func signalOp(h Header, stderr io.Writer) int {
	in, err := proclog.Read(h.Root, h.ID)
	if errors.Is(err, proclog.ErrNotFound) {
		return failed(stderr, ExitNotExist, err)
	}
	if err != nil {
		return failed(stderr, ExitFailed, err)
	}
	if in.Exit != nil {
		return 0
	}
	b, err := os.ReadFile(filepath.Join(h.Root, h.ID, proclog.PIDFile))
	if err != nil {
		return failed(stderr, ExitFailed, err)
	}
	var pid int
	if _, err := fmt.Sscan(string(b), &pid); err != nil || pid <= 1 {
		return failed(stderr, ExitFailed, fmt.Errorf("process %s has no pid", h.ID))
	}
	if err := syscall.Kill(-pid, syscall.Signal(h.Signal)); err != nil && !errors.Is(err, syscall.ESRCH) {
		return failed(stderr, ExitFailed, err)
	}
	return 0
}

func listOp(h Header, stdout, stderr io.Writer) int {
	list, err := proclog.List(h.Root)
	if err != nil {
		return failed(stderr, ExitFailed, err)
	}
	if list == nil {
		list = []proclog.Info{}
	}
	if err := json.NewEncoder(stdout).Encode(list); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	return 0
}

// LogLine is one line of a log op's output: a chunk, or how the read
// ended, last.
type LogLine struct {
	Chunk *proclog.Chunk `json:"chunk,omitempty"`
	End   *proclog.End   `json:"end,omitempty"`
}

// logOp reads a process's log as LogLines, following it until it ends or
// the call is gone, as its stdin ending says.
func logOp(h Header, in io.Reader, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, in)
		cancel()
	}()
	enc := json.NewEncoder(stdout)
	end, err := proclog.ReadLog(ctx, h.Root, h.ID, h.From, h.Follow, func(c proclog.Chunk) error {
		return enc.Encode(LogLine{Chunk: &c})
	})
	if errors.Is(err, proclog.ErrNotFound) {
		return failed(stderr, ExitNotExist, err)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return failed(stderr, ExitFailed, err)
	}
	if err := enc.Encode(LogLine{End: &end}); err != nil {
		return failed(stderr, ExitFailed, err)
	}
	return 0
}
