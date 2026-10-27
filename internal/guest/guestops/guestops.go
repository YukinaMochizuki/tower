// Package guestops is a provider's side of tower-guest (internal/guest/
// guestbin): the operations done in a running guest (commands, files,
// copies and processes), each one call of tower-guest shim over a Runner:
// docker exec in a hostd container, ssh in a hostd VM, hangar's exec in a
// hangar machine. It turns the shim's exit codes into internal/guest's
// errors; a Runner's own errors pass through as they are.
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
	"strconv"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
	"github.com/YukinaMochizuki/tower/internal/guest/proclog"
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
		return fmt.Errorf("%s: %s: %w", op, msg, guest.ErrBadOffset)
	}
	return fmt.Errorf("%s: exit %d: %s", op, code, msg)
}

// Exec runs a command until it ends or ctx does, which kills it.
func (o Ops) Exec(ctx context.Context, spec guest.ExecSpec) (int, error) {
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

// Receive listens in the guest for a tar Send sends with token, and
// extracts it into dir. listening hears the port it listens on, every
// address of the guest's, once it does; it then waits for the sender until
// it is done or ctx is.
func (o Ops) Receive(ctx context.Context, dir, token string, listening func(port int)) error {
	pr, pw := io.Pipe()
	got := make(chan struct{})
	go func() {
		defer close(got)
		sc := bufio.NewScanner(pr)
		if sc.Scan() {
			if port, err := strconv.Atoi(strings.TrimSpace(sc.Text())); err == nil {
				listening(port)
			}
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	code, msg, err := o.call(ctx, guestbin.Header{Op: guestbin.OpReceive, Path: dir, Token: token}, nil, pw)
	_ = pw.Close()
	<-got
	return result("receiving into "+dir, code, msg, err)
}

// Send connects to a guest's Receive at addr and sends it a tar of path,
// its top name to, or path's own when empty.
func (o Ops) Send(ctx context.Context, path, addr, token, to string) error {
	code, msg, err := o.call(ctx, guestbin.Header{Op: guestbin.OpSend, Path: path, Addr: addr, Token: token, To: to}, nil, nil)
	return result("sending "+path, code, msg, err)
}

// HTTPTransfer is a tar taken from a server or posted to it: URL, which
// the credential lets the guest's request reach once, after Probe answers.
type HTTPTransfer struct {
	URL, Probe, Credential string
}

// Fetch GETs the transfer's URL and extracts the tar it answers into dir,
// and Post POSTs a tar of path to it; each returns how the server
// answered. A probe that fails is guest.ErrUnreachable, the request not
// made.
func (o Ops) Fetch(ctx context.Context, x HTTPTransfer, dir string) (guestbin.HTTPAnswer, error) {
	return o.http(ctx, guestbin.OpFetch, x, dir, "fetching into "+dir)
}

func (o Ops) Post(ctx context.Context, x HTTPTransfer, path string) (guestbin.HTTPAnswer, error) {
	return o.http(ctx, guestbin.OpPost, x, path, "posting "+path)
}

func (o Ops) http(ctx context.Context, op string, x HTTPTransfer, path, what string) (guestbin.HTTPAnswer, error) {
	var out bytes.Buffer
	h := guestbin.Header{Op: op, Path: path, URL: x.URL, Probe: x.Probe, Token: x.Credential}
	code, msg, err := o.call(ctx, h, nil, &out)
	if code == guestbin.ExitUnreachable && err == nil {
		return guestbin.HTTPAnswer{}, fmt.Errorf("%s: %s: %w", what, msg, guest.ErrUnreachable)
	}
	if err := result(what, code, msg, err); err != nil {
		return guestbin.HTTPAnswer{}, err
	}
	var a guestbin.HTTPAnswer
	if err := json.Unmarshal(out.Bytes(), &a); err != nil {
		return guestbin.HTTPAnswer{}, fmt.Errorf("%s: the guest answered %q: %w", what, out.String(), err)
	}
	return a, nil
}

// StartProcess starts a process under a supervisor of its own and returns
// its id.
func (o Ops) StartProcess(ctx context.Context, spec guest.ProcessSpec) (string, error) {
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
		return fmt.Errorf("process %s: %w", id, guest.ErrNoProcess)
	}
	return result("signalling "+id, code, msg, err)
}

func processInfo(in proclog.Info) guest.ProcessInfo {
	p := guest.ProcessInfo{ID: in.ID, Name: in.Name, Command: in.Command, Started: in.Started}
	if in.Exit != nil {
		p.Exit = &guest.ProcessExit{Code: in.Exit.Code, Signal: in.Exit.Signal}
	}
	return p
}

// ListProcesses lists the guest's processes, oldest first.
func (o Ops) ListProcesses(ctx context.Context) ([]guest.ProcessInfo, error) {
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
	out := make([]guest.ProcessInfo, 0, len(list))
	for _, in := range list {
		out = append(out, processInfo(in))
	}
	return out, nil
}

func logEnd(e proclog.End) guest.LogEnd {
	end := guest.LogEnd{HeldFrom: e.HeldFrom, Next: e.Next}
	if e.Exit != nil {
		end.Exit = &guest.ProcessExit{Code: e.Exit.Code, Signal: e.Exit.Signal}
	}
	return end
}

// ReadProcessLog gives out a process's log from from, then, with follow,
// what it writes until it ends or ctx is done.
func (o Ops) ReadProcessLog(ctx context.Context, id string, from int64, follow bool, out func(guest.LogChunk) error) (guest.LogEnd, error) {
	emit := func(c proclog.Chunk) error {
		return out(guest.LogChunk{Stderr: c.Stderr, Offset: c.Offset, Data: c.Data})
	}
	if o.HostProcRoot != "" {
		end, err := proclog.ReadLog(ctx, o.HostProcRoot, id, from, follow, emit)
		if errors.Is(err, proclog.ErrNotFound) {
			return guest.LogEnd{}, fmt.Errorf("process %s: %w", id, guest.ErrNoProcess)
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
		return guest.LogEnd{}, outErr
	}
	if code == guestbin.ExitNotExist && err == nil {
		return guest.LogEnd{}, fmt.Errorf("process %s: %w", id, guest.ErrNoProcess)
	}
	if err := result("reading the log of "+id, code, msg, err); err != nil {
		return guest.LogEnd{}, err
	}
	if scanErr != nil {
		return guest.LogEnd{}, scanErr
	}
	if end == nil {
		return guest.LogEnd{}, fmt.Errorf("reading the log of %s: it ended without its end", id)
	}
	if ctx.Err() != nil && end.Exit == nil {
		return logEnd(*end), ctx.Err()
	}
	return logEnd(*end), nil
}
