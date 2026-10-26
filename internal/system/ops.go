package system

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// Runner runs tower-system in the container, its stdin from stdin and its
// output to stdout and stderr, until it exits, and returns its exit code.
// ctx ending ends the call, which ends what it runs.
type Runner interface {
	RunSystem(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error)
}

// Ops are the operations of one system container. A call's error is the
// git and image drivers' (internal/hostd/driver): ErrNotFound,
// ErrLeaseLost, ErrNoSpace, or a failure with tower-system's message; a
// Runner's own error passes through.
type Ops struct{ R Runner }

// call runs one operation: its header, then what in gives as data frames
// and the end frame. Its answer is decoded, and what follows it written to
// tail.
func (o Ops) call(ctx context.Context, h Header, in io.Reader, tail io.Writer) (Result, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	go func() {
		err := writeHeader(pw, h)
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
		<-ctx.Done()
		pw.CloseWithError(ctx.Err())
	}()
	or, ow := io.Pipe()
	var res Result
	decoded := make(chan error, 1)
	go func() {
		br := bufio.NewReaderSize(or, 64<<10)
		line, err := br.ReadBytes('\n')
		if err == nil {
			err = json.Unmarshal(line, &res)
		}
		if err == nil && tail != nil {
			_, err = io.Copy(tail, br)
		}
		_, _ = io.Copy(io.Discard, br)
		decoded <- err
	}()
	var stderr bytes.Buffer
	code, err := o.R.RunSystem(ctx, pr, ow, &limited{w: &stderr, n: 64 << 10})
	_ = pr.Close()
	_ = ow.Close()
	derr := <-decoded
	if err != nil {
		return Result{}, err
	}
	msg := strings.TrimSpace(stderr.String())
	switch code {
	case 0:
	case ExitNotFound:
		return Result{}, fmt.Errorf("%s: %w", msg, driver.ErrNotFound)
	case ExitLeaseLost:
		return Result{}, fmt.Errorf("%s: %w", msg, driver.ErrLeaseLost)
	case ExitNoSpace:
		return Result{}, fmt.Errorf("%s: %w", msg, driver.ErrNoSpace)
	default:
		return Result{}, fmt.Errorf("tower-system %s: exit %d: %s", h.Op, code, msg)
	}
	if derr != nil {
		return Result{}, fmt.Errorf("tower-system %s: its answer: %w", h.Op, derr)
	}
	return res, nil
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

// Fetch brings a branch into a mirror and returns the commit it is at.
func (o Ops) Fetch(ctx context.Context, spec driver.FetchSpec) (string, error) {
	res, err := o.call(ctx, Header{Op: OpFetch, Mirror: spec.Mirror, URL: spec.URL, Branch: spec.Branch, Env: secret(EnvToken, spec.Token)}, nil, nil)
	return res.Commit, err
}

// Checkout writes a tar of a clone of a mirror at ref to w, and returns
// the commit checked out.
func (o Ops) Checkout(ctx context.Context, mirror, ref string, w io.Writer) (string, error) {
	res, err := o.call(ctx, Header{Op: OpCheckout, Mirror: mirror, Ref: ref}, nil, w)
	return res.Commit, err
}

// Origin writes a tar that brings a checkout's origin/* up to its mirror.
func (o Ops) Origin(ctx context.Context, mirror string, w io.Writer) error {
	_, err := o.call(ctx, Header{Op: OpOrigin, Mirror: mirror}, nil, w)
	return err
}

// Push takes a checkout's .git, a tar from gitDir, into a mirror and pushes
// its HEAD to the branch, with --force-with-lease on the lease.
func (o Ops) Push(ctx context.Context, spec driver.PushSpec, gitDir io.Reader) error {
	_, err := o.call(ctx, Header{Op: OpPush, Mirror: spec.Mirror, URL: spec.URL, Branch: spec.Branch, Lease: spec.Lease, Env: secret(EnvToken, spec.Token)}, gitDir, nil)
	return err
}

// CommitsBehind counts the commits on a mirror's branch since commit.
func (o Ops) CommitsBehind(ctx context.Context, mirror, commit, branch string) (int, error) {
	res, err := o.call(ctx, Header{Op: OpBehind, Mirror: mirror, Commit: commit, Branch: branch}, nil, nil)
	return res.Behind, err
}

// Read reads paths of a mirror's work tree at ref.
func (o Ops) Read(ctx context.Context, mirror, ref string, paths []string, maxContent int64) (string, map[string]driver.MirrorEntry, error) {
	res, err := o.call(ctx, Header{Op: OpRead, Mirror: mirror, Ref: ref, Paths: paths, MaxContent: maxContent}, nil, nil)
	return res.Commit, res.Entries, err
}

// Login is a registry login.
type Login struct{ Username, Password string }

func (l Login) env() map[string]string {
	if l.Username == "" {
		return nil
	}
	return map[string]string{EnvRegistryUsername: l.Username, EnvRegistryPassword: l.Password}
}

// Build builds a context, a tar, or with mirror set the mirror's work tree
// at ref, and pushes the image to the registry with login, returning its
// reference by digest.
func (o Ops) Build(ctx context.Context, spec driver.BuildSpec, mirror, ref string, login Login) (string, error) {
	h := Header{Op: OpBuild, Repository: spec.Repository, Dir: spec.Dir, Dockerfile: spec.Dockerfile, Mirror: mirror, Ref: ref, Env: login.env()}
	in := spec.Context
	if mirror != "" {
		in = nil
	} else if in == nil {
		return "", errors.New("a build needs a context or a mirror")
	}
	res, err := o.call(ctx, h, in, nil)
	return res.Image, err
}

// Resolve reads what an image reference names from its registry.
func (o Ops) Resolve(ctx context.Context, ref string, login Login) (driver.ImageConfig, error) {
	res, err := o.call(ctx, Header{Op: OpResolve, Image: ref, Env: login.env()}, nil, nil)
	return driver.ImageConfig{Digest: res.Digest, User: res.User}, err
}

// Status says whether the container's buildkitd answers, and whether it is
// on the tailnet.
func (o Ops) Status(ctx context.Context) (Result, error) {
	return o.call(ctx, Header{Op: OpStatus}, nil, nil)
}

// JoinTailnet joins the container to the tailnet as hostname, with key.
func (o Ops) JoinTailnet(ctx context.Context, hostname, key string) error {
	_, err := o.call(ctx, Header{Op: OpTailnetUp, Hostname: hostname, Env: secret(EnvTailnetAuthKey, key)}, nil, nil)
	return err
}

func secret(name, value string) map[string]string {
	if value == "" {
		return nil
	}
	return map[string]string{name: value}
}
