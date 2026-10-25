// Package guestops is hostd's side of tower-guest (internal/hostd/
// guestbin): the operations the container and VM drivers both do in a
// running guest (commands, files, copies and processes), each one call of
// tower-guest shim over a Runner, docker exec for a container and ssh for
// a VM. It turns the shim's exit codes into the drivers' errors.
package guestops

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/guestbin"
	"github.com/YukinaMochizuki/tower/internal/hostd/proclog"
)

// Runner runs tower-guest shim in a guest, its stdin from stdin and its
// output to stdout and stderr, until it exits, and returns its exit code.
// ctx ending ends the call, which ends what it runs.
type Runner interface {
	RunShim(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}

// Ops are the operations in one guest.
type Ops struct {
	R Runner
	// ProcRoot is where the guest keeps its processes, in the guest.
	ProcRoot string
	// HostProcRoot, if set, is the same directory as the host sees it, a
	// container's, which lists and logs are read from directly: so they
	// are read while the guest is stopped too.
	HostProcRoot string
}

// call runs one operation: its header, then what in gives as data frames
// and the end frame. Its stderr is returned with the exit code.
func (o Ops) call(ctx context.Context, h guestbin.Header, in io.Reader, stdout io.Writer) (int, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	go func() {
		err := guestbin.WriteHeader(pw, h)
		if err == nil && in != nil {
			fw := guestbin.FrameWriter{W: pw}
			if _, err = io.Copy(fw, in); err == nil {
				err = fw.Close()
			}
		} else if err == nil {
			err = guestbin.WriteFrame(pw, guestbin.FrameEOF, nil)
		}
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		// The stdin stays open, without more, until the call ends: its end
		// without the end frame would kill what runs.
		<-ctx.Done()
		pw.CloseWithError(ctx.Err())
	}()
	var stderr bytes.Buffer
	if stdout == nil {
		stdout = io.Discard
	}
	code, err := o.R.RunShim(ctx, pr, stdout, &limited{w: &stderr, n: 64 << 10})
	_ = pr.Close()
	return code, strings.TrimSpace(stderr.String()), err
}

// limited writes the first n bytes to w and drops the rest.
type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(l.n, len(p))
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// result is an operation's error from its exit code: what is not there,
// a bad offset, or a failure with its message.
func result(op string, code int, msg string, err error) error {
	switch {
	case err != nil:
		return err
	case code == 0:
		return nil
	case code == guestbin.ExitNotExist:
		return fmt.Errorf("%s: %s: %w", op, msg, fs.ErrNotExist)
	case code == guestbin.ExitBadOffset:
		return fmt.Errorf("%s: %s: %w", op, msg, driver.ErrBadOffset)
	}
	return fmt.Errorf("%s: exit %d: %s", op, code, msg)
}

// Exec runs a command until it ends or ctx does, which kills it.
func (o Ops) Exec(ctx context.Context, spec driver.ExecSpec) (int, error) {
	stdin := spec.Stdin
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	stdout, stderr := spec.Stdout, spec.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	h := guestbin.Header{Op: guestbin.OpExec, Command: spec.Command, Env: spec.Env, Dir: spec.Dir}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	go func() {
		err := guestbin.WriteHeader(pw, h)
		if err == nil {
			fw := guestbin.FrameWriter{W: pw}
			if _, err = io.Copy(fw, stdin); err == nil {
				err = fw.Close()
			}
		}
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		<-ctx.Done()
		pw.CloseWithError(ctx.Err())
	}()
	code, err := o.R.RunShim(ctx, pr, stdout, stderr)
	_ = pr.Close()
	return code, err
}

// ReadFile streams a file to w.
func (o Ops) ReadFile(ctx context.Context, path string, w io.Writer) error {
	code, msg, err := o.call(ctx, guestbin.Header{Op: guestbin.OpRead, Path: path}, nil, w)
	return result("reading "+path, code, msg, err)
}

// WriteFile writes a file from r: offset 0 replaces it, any other must be
// where it ends.
func (o Ops) WriteFile(ctx context.Context, path string, mode fs.FileMode, offset int64, r io.Reader) error {
	code, msg, err := o.call(ctx, guestbin.Header{Op: guestbin.OpWrite, Path: path, Mode: uint32(mode.Perm()), Offset: offset}, r, nil)
	return result("writing "+path, code, msg, err)
}

// CopyOut writes a tar of a path.
func (o Ops) CopyOut(ctx context.Context, path string, w io.Writer) error {
	code, msg, err := o.call(ctx, guestbin.Header{Op: guestbin.OpCopyOut, Path: path}, nil, w)
	return result("copying out "+path, code, msg, err)
}

// CopyIn extracts a tar into dir.
func (o Ops) CopyIn(ctx context.Context, dir string, r io.Reader) error {
	code, msg, err := o.call(ctx, guestbin.Header{Op: guestbin.OpCopyIn, Path: dir}, r, nil)
	return result("copying into "+dir, code, msg, err)
}

// StartProcess starts a process under a supervisor of its own and returns
// its id.
func (o Ops) StartProcess(ctx context.Context, spec driver.ProcessSpec) (string, error) {
	if len(spec.Command) == 0 {
		return "", errors.New("a process needs a command")
	}
	var out bytes.Buffer
	h := guestbin.Header{Op: guestbin.OpStart, Root: o.ProcRoot, Name: spec.Name, Command: spec.Command, Env: spec.Env, Dir: spec.Dir, LogCap: spec.LogCap, Keep: spec.Keep}
	// Started apart from ctx: a call ended once the process runs ends
	// nothing of it, and one ended before has the shim kill nothing it
	// made but what it was told.
	code, msg, err := o.call(context.WithoutCancel(ctx), h, nil, &out)
	if err := result("starting "+spec.Command[0], code, msg, err); err != nil {
		return "", err
	}
	id := strings.TrimSpace(out.String())
	if !proclog.ValidID(id) {
		return "", fmt.Errorf("starting %s: the guest answered %q", spec.Command[0], id)
	}
	return id, nil
}

// SignalProcess sends sig to a process's group; one ended is no error.
func (o Ops) SignalProcess(ctx context.Context, id string, sig int) error {
	code, msg, err := o.call(ctx, guestbin.Header{Op: guestbin.OpSignal, Root: o.ProcRoot, ID: id, Signal: sig}, nil, nil)
	if code == guestbin.ExitNotExist && err == nil {
		return fmt.Errorf("process %s: %w", id, driver.ErrNotFound)
	}
	return result("signalling "+id, code, msg, err)
}

func processInfo(in proclog.Info) driver.ProcessInfo {
	p := driver.ProcessInfo{ID: in.ID, Name: in.Name, Command: in.Command, Started: in.Started}
	if in.Exit != nil {
		p.Exit = &driver.ProcessExit{Code: in.Exit.Code, Signal: in.Exit.Signal}
	}
	return p
}

// ListProcesses lists the guest's processes, oldest first.
func (o Ops) ListProcesses(ctx context.Context) ([]driver.ProcessInfo, error) {
	var list []proclog.Info
	if o.HostProcRoot != "" {
		var err error
		if list, err = proclog.List(o.HostProcRoot); err != nil {
			return nil, err
		}
	} else {
		var out bytes.Buffer
		code, msg, err := o.call(ctx, guestbin.Header{Op: guestbin.OpList, Root: o.ProcRoot}, nil, &out)
		if err := result("listing processes", code, msg, err); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(out.Bytes(), &list); err != nil {
			return nil, fmt.Errorf("listing processes: %w", err)
		}
	}
	out := make([]driver.ProcessInfo, 0, len(list))
	for _, in := range list {
		out = append(out, processInfo(in))
	}
	return out, nil
}

func logEnd(e proclog.End) driver.LogEnd {
	end := driver.LogEnd{HeldFrom: e.HeldFrom, Next: e.Next}
	if e.Exit != nil {
		end.Exit = &driver.ProcessExit{Code: e.Exit.Code, Signal: e.Exit.Signal}
	}
	return end
}

// ReadProcessLog gives out a process's log from from, then, with follow,
// what it writes until it ends or ctx is done.
func (o Ops) ReadProcessLog(ctx context.Context, id string, from int64, follow bool, out func(driver.LogChunk) error) (driver.LogEnd, error) {
	emit := func(c proclog.Chunk) error {
		return out(driver.LogChunk{Stderr: c.Stderr, Offset: c.Offset, Data: c.Data})
	}
	if o.HostProcRoot != "" {
		end, err := proclog.ReadLog(ctx, o.HostProcRoot, id, from, follow, emit)
		if errors.Is(err, proclog.ErrNotFound) {
			return driver.LogEnd{}, fmt.Errorf("process %s: %w", id, driver.ErrNotFound)
		}
		return logEnd(end), err
	}
	pr, pw := io.Pipe()
	var outErr error
	var end *proclog.End
	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 8<<20)
		for sc.Scan() {
			var line guestbin.LogLine
			if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
				done <- err
				_ = pr.CloseWithError(err)
				return
			}
			switch {
			case line.Chunk != nil && outErr == nil:
				if outErr = emit(*line.Chunk); outErr != nil {
					_ = pr.CloseWithError(outErr)
				}
			case line.End != nil:
				end = line.End
			}
		}
		done <- sc.Err()
	}()
	code, msg, err := o.call(ctx, guestbin.Header{Op: guestbin.OpLog, Root: o.ProcRoot, ID: id, From: from, Follow: follow}, nil, pw)
	_ = pw.Close()
	scanErr := <-done
	if outErr != nil {
		return driver.LogEnd{}, outErr
	}
	if code == guestbin.ExitNotExist && err == nil {
		return driver.LogEnd{}, fmt.Errorf("process %s: %w", id, driver.ErrNotFound)
	}
	if err := result("reading the log of "+id, code, msg, err); err != nil {
		return driver.LogEnd{}, err
	}
	if scanErr != nil {
		return driver.LogEnd{}, scanErr
	}
	if end == nil {
		return driver.LogEnd{}, fmt.Errorf("reading the log of %s: it ended without its end", id)
	}
	if ctx.Err() != nil && end.Exit == nil {
		return logEnd(*end), ctx.Err()
	}
	return logEnd(*end), nil
}
