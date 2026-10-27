package system

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/gitmirror"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/images"
)

var errNoHeader = errors.New("the call's first frame is no header")

// Config is where the container keeps what it keeps.
type Config struct {
	// Root is the container's disk: the mirrors, scratch space and the
	// tailnet's state are under it.
	Root string
	// BuildKit is buildkitd's address, such as
	// unix:///run/buildkit/buildkitd.sock.
	BuildKit string
	// Registry is tower's registry as the container pushes to it,
	// host[:port], over the tailnet.
	Registry string
	// RegistryPlainHTTP says Registry is served over plain HTTP.
	RegistryPlainHTTP bool
	// Tailscale is the tailscale command, talking to the container's
	// tailscaled.
	Tailscale []string
	// Listen is where the server serves boxes, ":7480" when empty;
	// Control its control socket, DefaultControl when empty.
	Listen, Control string
	// GitProxy is the HTTP proxy git reaches repositories through:
	// tailscaled's, as the tailnet's repositories, a workstream's own
	// among them, are reached through it alone. None when empty.
	GitProxy string
}

func (c Config) control() string {
	if c.Control == "" {
		return DefaultControl
	}
	return c.Control
}

// mirrors is the git driver over the container's mirrors.
func mirrors(ctx context.Context, cfg Config) (*gitmirror.Driver, error) {
	return gitmirror.New(ctx, gitmirror.Config{
		Root: filepath.Join(cfg.Root, "mirrors"), ScratchDir: filepath.Join(cfg.Root, "scratch", "git"), Proxy: cfg.GitProxy,
	})
}

// checkoutFile writes a tar of a clone of a mirror at ref into a scratch
// file, made whole before it is sent: a checkout that fails says so, not
// a tar cut short. It returns the commit checked out, and the file, which
// the caller closes.
func checkoutFile(ctx context.Context, cfg Config, mirror, ref string) (string, *os.File, error) {
	g, err := mirrors(ctx, cfg)
	if err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp(filepath.Join(cfg.Root, "scratch"), "checkout-")
	if err != nil {
		return "", nil, err
	}
	_ = os.Remove(f.Name())
	commit, err := g.Checkout(ctx, mirror, ref, f)
	if err != nil {
		_ = f.Close()
		return "", nil, err
	}
	return commit, f, nil
}

// originFile writes, as checkoutFile does, the tar that brings a
// checkout's origin/* up to its mirror.
func originFile(ctx context.Context, cfg Config, mirror string) (*os.File, error) {
	g, err := mirrors(ctx, cfg)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Join(cfg.Root, "scratch"), "origin-")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(f.Name())
	if err := g.Origin(ctx, mirror, f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// Serve runs the one operation its stdin's header names, and returns its
// exit code.
func Serve(ctx context.Context, cfg Config, stdin io.Reader, stdout, stderr io.Writer) int {
	in := bufio.NewReader(stdin)
	h, err := readHeader(in)
	if err != nil {
		return failed(stderr, fmt.Errorf("reading the header: %w", err))
	}
	res, tail, err := serve(ctx, cfg, h, in, stderr)
	if err != nil {
		return failed(stderr, err)
	}
	enc, err := json.Marshal(res)
	if err != nil {
		return failed(stderr, err)
	}
	w := bufio.NewWriter(stdout)
	if _, err := w.Write(append(enc, '\n')); err != nil {
		return ExitFailed
	}
	if tail != nil {
		if err := tail(w); err != nil {
			return failed(stderr, err)
		}
	}
	if err := w.Flush(); err != nil {
		return ExitFailed
	}
	return 0
}

// failed writes a call's failure and returns its exit code.
func failed(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "tower-system: %v\n", err)
	switch {
	case errors.Is(err, driver.ErrNotFound):
		return ExitNotFound
	case errors.Is(err, driver.ErrLeaseLost):
		return ExitLeaseLost
	case errors.Is(err, driver.ErrNoSpace):
		return ExitNoSpace
	}
	return ExitFailed
}

// builderConfig is the image driver's for the call h: without Docker,
// which the container has not, so buildkitd pushes each build itself,
// through its proxy, as it exports it, and nothing is held between calls.
func builderConfig(cfg Config, h Header) images.Config {
	return images.Config{
		BuildKit: cfg.BuildKit, Registry: cfg.Registry, ScratchDir: filepath.Join(cfg.Root, "scratch", "build"),
		NoDocker: true, PlainHTTP: cfg.RegistryPlainHTTP,
		Username: h.Env[EnvRegistryUsername], Password: h.Env[EnvRegistryPassword],
	}
}

// serve runs h, and returns its result and, for a checkout, what writes
// its tar after it.
func serve(ctx context.Context, cfg Config, h Header, in io.Reader, stderr io.Writer) (Result, func(io.Writer) error, error) {
	git := func() (*gitmirror.Driver, error) { return mirrors(ctx, cfg) }
	builder := func() (*images.Driver, error) { return images.New(builderConfig(cfg, h)) }
	switch h.Op {
	case OpFetch:
		g, err := git()
		if err != nil {
			return Result{}, nil, err
		}
		head, err := g.Fetch(ctx, driver.FetchSpec{Mirror: h.Mirror, URL: h.URL, Token: h.Env[EnvToken], Branch: h.Branch})
		return Result{Commit: head}, nil, err
	case OpCheckout:
		commit, f, err := checkoutFile(ctx, cfg, h.Mirror, h.Ref)
		if err != nil {
			return Result{}, nil, err
		}
		return Result{Commit: commit}, sendFile(f), nil
	case OpOrigin:
		f, err := originFile(ctx, cfg, h.Mirror)
		if err != nil {
			return Result{}, nil, err
		}
		return Result{}, sendFile(f), nil
	case OpPush:
		g, err := git()
		if err != nil {
			return Result{}, nil, err
		}
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(guestbin.ReadData(in, pw)) }()
		err = g.Push(ctx, driver.PushSpec{Mirror: h.Mirror, URL: h.URL, Token: h.Env[EnvToken], Branch: h.Branch, Lease: h.Lease}, pr)
		_ = pr.CloseWithError(errors.New("push ended"))
		return Result{}, nil, err
	case OpBehind:
		g, err := git()
		if err != nil {
			return Result{}, nil, err
		}
		n, err := g.CommitsBehind(ctx, h.Mirror, h.Commit, h.Branch)
		return Result{Behind: n}, nil, err
	case OpRead:
		g, err := git()
		if err != nil {
			return Result{}, nil, err
		}
		commit, entries, err := g.Read(ctx, h.Mirror, h.Ref, h.Paths, h.MaxContent)
		return Result{Commit: commit, Entries: entries}, nil, err
	case OpBuild:
		b, err := builder()
		if err != nil {
			return Result{}, nil, err
		}
		pr, pw := io.Pipe()
		if h.Mirror != "" {
			g, err := git()
			if err != nil {
				return Result{}, nil, err
			}
			go func() {
				_, err := g.Archive(ctx, h.Mirror, h.Ref, pw)
				pw.CloseWithError(err)
			}()
		} else {
			go func() { pw.CloseWithError(guestbin.ReadData(in, pw)) }()
		}
		image, err := b.Build(ctx, driver.BuildSpec{Repository: h.Repository, Context: pr, Dir: h.Dir, Dockerfile: h.Dockerfile})
		_ = pr.CloseWithError(errors.New("build ended"))
		return Result{Image: image}, nil, err
	case OpResolve:
		b, err := builder()
		if err != nil {
			return Result{}, nil, err
		}
		c, err := b.Resolve(ctx, h.Image)
		return Result{Digest: c.Digest, User: c.User}, nil, err
	case OpStatus:
		var res Result
		b, err := builder()
		if err != nil {
			return Result{}, nil, fmt.Errorf("the builder: %w", err)
		}
		if v, err := b.Version(ctx); err == nil {
			res.BuildKit = v
		} else {
			_, _ = fmt.Fprintf(stderr, "buildkitd: %v\n", err)
		}
		res.Tailnet = tailnetUp(ctx, cfg)
		if err := control(ctx, cfg, http.MethodGet, controlStatusPath, nil); err == nil {
			res.Serving = true
		} else {
			_, _ = fmt.Fprintf(stderr, "the server: %v\n", err)
		}
		return res, nil, nil
	case OpTailnetUp:
		return Result{Tailnet: true}, nil, joinTailnet(ctx, cfg, h)
	case OpGrant:
		return Result{}, nil, control(ctx, cfg, http.MethodPost, controlGrantPath, controlGrant{
			Credential: h.Env[EnvCredential], Op: h.Grant, Mirror: h.Mirror, Ref: h.Ref, URL: h.URL, Branch: h.Branch, Lease: h.Lease,
			Token: h.Env[EnvToken], Network: h.Network, TTL: h.TTL, Sources: h.Sources,
		})
	case OpRevoke:
		return Result{}, nil, control(ctx, cfg, http.MethodDelete, controlGrantPath, controlGrant{Credential: h.Env[EnvCredential]})
	case OpSources:
		srcs := h.Sources
		if srcs == nil {
			srcs = []Source{}
		}
		return Result{}, nil, control(ctx, cfg, http.MethodPut, controlSourcesPath, srcs)
	}
	return Result{}, nil, fmt.Errorf("no operation %q", h.Op)
}

// sendFile writes f, from its start, and closes it.
func sendFile(f *os.File) func(io.Writer) error {
	return func(w io.Writer) error {
		defer func() { _ = f.Close() }()
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		_, err := io.Copy(w, f)
		return err
	}
}

// tailnetUp reports whether the container's tailscaled is on the tailnet.
func tailnetUp(ctx context.Context, cfg Config) bool {
	if len(cfg.Tailscale) == 0 {
		return false
	}
	out, err := exec.CommandContext(ctx, cfg.Tailscale[0], append(cfg.Tailscale[1:], "status", "--json")...).Output()
	if err != nil {
		return false
	}
	var st struct{ BackendState string }
	return json.Unmarshal(out, &st) == nil && st.BackendState == "Running"
}

// joinTailnet joins the container to the tailnet with the call's auth
// key, which tailscale reads from its stdin: never its command line, nor a
// file that a call cut short would leave behind.
func joinTailnet(ctx context.Context, cfg Config, h Header) error {
	key := h.Env[EnvTailnetAuthKey]
	if len(cfg.Tailscale) == 0 || key == "" {
		return errors.New("joining the tailnet needs tailscale and an auth key")
	}
	args := append(cfg.Tailscale[1:], "up", "--auth-key=file:/dev/stdin")
	if h.Hostname != "" {
		args = append(args, "--hostname="+h.Hostname)
	}
	cmd := exec.CommandContext(ctx, cfg.Tailscale[0], args...)
	cmd.Stdin = strings.NewReader(key)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tailscale up: %w: %s", err, strings.ReplaceAll(strings.TrimSpace(string(out)), key, "[the key]"))
	}
	return nil
}
