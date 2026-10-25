// Package images is hostd's image driver (driver.Images): it pulls from
// tower's registry into Docker, and builds a context with BuildKit,
// pushing the image to the registry (docs/decisions/images.md). The
// registry's credentials are hostd's own configuration, held in memory and
// handed to Docker with each pull and to BuildKit through its session;
// they are never written to a Docker config or anywhere else on the host.
package images

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/docker/cli/cli/config/types"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	gateway "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/moby/api/pkg/authconfig"
	"github.com/moby/moby/api/types/registry"
	dockerclient "github.com/moby/moby/client"
	"github.com/tonistiigi/fsutil"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
	"github.com/YukinaMochizuki/tower/internal/hostd/tarx"
)

// Config configures the driver.
type Config struct {
	// Docker is the client images are pulled into.
	Docker *dockerclient.Client
	// BuildKit is buildkitd's address, such as
	// unix:///run/buildkit/buildkitd.sock.
	BuildKit string
	// Registry is tower's registry, host[:port]; builds are pushed to
	// Registry/<repository>.
	Registry string
	// Username and Password sign in to Registry; empty is anonymous.
	Username, Password string
	// ScratchDir is a directory, root's alone, to unpack build contexts in.
	ScratchDir string
}

// Driver is the image driver. It is safe for concurrent use.
type Driver struct {
	cfg Config
}

var _ driver.Images = (*Driver)(nil)

// New makes the scratch directory.
func New(cfg Config) (*Driver, error) {
	if cfg.Registry == "" {
		return nil, errors.New("no registry")
	}
	if err := os.MkdirAll(cfg.ScratchDir, 0o700); err != nil {
		return nil, err
	}
	return &Driver{cfg: cfg}, nil
}

func (d *Driver) Version(ctx context.Context) (string, error) {
	c, err := client.New(ctx, d.cfg.BuildKit)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	info, err := c.Info(ctx)
	if err != nil {
		return "", err
	}
	return "buildkit " + info.BuildkitVersion.Version, nil
}

// registryOf is the registry host a reference names.
func registryOf(ref string) string {
	host, _, ok := strings.Cut(ref, "/")
	if !ok || !strings.ContainsAny(host, ".:") && host != "localhost" {
		return "docker.io"
	}
	return host
}

// Pull brings an image onto the host, with the registry's credentials if
// it is tower's. One there already is not pulled again.
func (d *Driver) Pull(ctx context.Context, ref string) error {
	if _, err := d.cfg.Docker.ImageInspect(ctx, ref); err == nil {
		return nil
	}
	opts := dockerclient.ImagePullOptions{}
	if registryOf(ref) == d.cfg.Registry && d.cfg.Username != "" {
		auth, err := authconfig.Encode(registry.AuthConfig{Username: d.cfg.Username, Password: d.cfg.Password, ServerAddress: d.cfg.Registry})
		if err != nil {
			return err
		}
		opts.RegistryAuth = auth
	}
	resp, err := d.cfg.Docker.ImagePull(ctx, ref, opts)
	if err == nil {
		err = resp.Wait(ctx)
		_ = resp.Close()
	}
	return pullError(ref, err)
}

// pullError is a pull's error as the driver's: what the registry lacks is
// ErrNotFound, a full disk ErrNoSpace.
func pullError(ref string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case cerrdefs.IsNotFound(err), strings.Contains(msg, "manifest unknown"), strings.Contains(msg, "not found"):
		return fmt.Errorf("pulling %s: %w: %w", ref, err, driver.ErrNotFound)
	case strings.Contains(msg, "no space left"):
		return fmt.Errorf("pulling %s: %w: %w", ref, err, driver.ErrNoSpace)
	}
	return fmt.Errorf("pulling %s: %w", ref, err)
}

// auth gives BuildKit the registry's credentials, and none for any other
// registry.
func (d *Driver) auth(_ context.Context, host string, _ []string, _ authprovider.ExpireCachedAuthCheck) (types.AuthConfig, error) {
	if host != d.cfg.Registry || d.cfg.Username == "" {
		return types.AuthConfig{}, nil
	}
	return types.AuthConfig{Username: d.cfg.Username, Password: d.cfg.Password, ServerAddress: host}, nil
}

// maxQuoted is how much of a failed build's output its error quotes.
const maxQuoted = 600

// inContext is a path of a build's tar, "" its root; one leaving it is
// none.
func inContext(p string) (string, error) {
	c := path.Clean(p)
	switch {
	case c == ".":
		return "", nil
	case path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../"):
		return "", fmt.Errorf("%q leaves the build context", p)
	}
	return c, nil
}

// within is the directory rel names in root, its symlinks followed, which
// must be a directory and stay in root.
func within(root, rel string) (string, error) {
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(base, filepath.FromSlash(rel)))
	if err != nil {
		return "", errors.New("not in the build context")
	}
	if resolved != base && !strings.HasPrefix(resolved, base+string(filepath.Separator)) {
		return "", errors.New("leads out of the build context")
	}
	if st, err := os.Stat(resolved); err != nil || !st.IsDir() {
		return "", errors.New("is no directory of the build context")
	}
	return resolved, nil
}

// Build builds a context with its Dockerfile and pushes the image to
// Registry/repository, returning its reference by digest. Timestamps are
// the epoch (SOURCE_DATE_EPOCH=0, rewritten in the layers), so the same
// context builds the same digest. The context is the tar's Dir; the
// Dockerfile, anywhere in the tar, is given to BuildKit apart from it, as
// docker build -f does.
func (d *Driver) Build(ctx context.Context, spec driver.BuildSpec) (string, error) {
	repository := spec.Repository
	if repository == "" || strings.ContainsAny(repository, "@: ") {
		return "", fmt.Errorf("%q is no repository", repository)
	}
	sub, err := inContext(spec.Dir)
	if err != nil {
		return "", err
	}
	dockerfile := spec.Dockerfile
	if dockerfile == "" {
		dockerfile = path.Join(sub, "Dockerfile")
	}
	if dockerfile, err = inContext(dockerfile); err != nil || dockerfile == "" {
		return "", fmt.Errorf("the Dockerfile: %w", cmp.Or(err, errors.New("names no file")))
	}
	dir, err := os.MkdirTemp(d.cfg.ScratchDir, "build-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := tarx.Extract(spec.Context, dir); err != nil {
		return "", fmt.Errorf("unpacking the build context: %w", err)
	}
	contextDir, err := within(dir, sub)
	if err != nil {
		return "", fmt.Errorf("the build context's directory %q: %w", spec.Dir, err)
	}
	dockerfileDir, err := within(dir, path.Dir(dockerfile))
	if err != nil {
		return "", fmt.Errorf("the Dockerfile %q: %w", dockerfile, err)
	}
	fs, err := fsutil.NewFS(contextDir)
	if err != nil {
		return "", err
	}
	dfs, err := fsutil.NewFS(dockerfileDir)
	if err != nil {
		return "", err
	}
	c, err := client.New(ctx, d.cfg.BuildKit)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	name := d.cfg.Registry + "/" + repository
	// What the build's steps write, the end of which a failure quotes: a
	// RUN that failed says why there, not in BuildKit's error.
	status := make(chan *client.SolveStatus)
	logged := make(chan string, 1)
	go func() {
		var tail []byte
		for s := range status {
			for _, l := range s.Logs {
				tail = append(tail, l.Data...)
				if len(tail) > 2*maxQuoted {
					tail = tail[len(tail)-maxQuoted:]
				}
			}
		}
		if len(tail) > maxQuoted {
			tail = tail[len(tail)-maxQuoted:]
		}
		logged <- strings.TrimSpace(string(tail))
	}()
	resp, err := c.Solve(ctx, nil, client.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: map[string]string{"build-arg:SOURCE_DATE_EPOCH": "0", "filename": path.Base(dockerfile)},
		LocalMounts:   map[string]fsutil.FS{"context": fs, "dockerfile": dfs},
		Exports: []client.ExportEntry{{Type: client.ExporterImage, Attrs: map[string]string{
			"name": name, "push": "true", "rewrite-timestamp": "true",
		}}},
		Session: []session.Attachable{authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{AuthConfigProvider: d.auth})},
	}, status)
	tail := <-logged // Solve closes status as it returns
	if err != nil {
		if strings.Contains(err.Error(), "no space left") {
			return "", fmt.Errorf("building %s: %w: %w", repository, err, driver.ErrNoSpace)
		}
		if tail != "" {
			return "", fmt.Errorf("building %s: %w; it last wrote: %s", repository, err, tail)
		}
		return "", fmt.Errorf("building %s: %w", repository, err)
	}
	digest := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	if digest == "" {
		return "", fmt.Errorf("building %s: BuildKit named no digest", repository)
	}
	return name + "@" + digest, nil
}

// Resolve asks BuildKit to resolve ref in its registry, with the
// registry's credentials for tower's, reading its manifest and config but
// none of its layers.
func (d *Driver) Resolve(ctx context.Context, ref string) (driver.ImageConfig, error) {
	c, err := client.New(ctx, d.cfg.BuildKit)
	if err != nil {
		return driver.ImageConfig{}, err
	}
	defer func() { _ = c.Close() }()
	var out driver.ImageConfig
	_, err = c.Build(ctx, client.SolveOpt{
		Session: []session.Attachable{authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{AuthConfigProvider: d.auth})},
	}, "tower-hostd", func(ctx context.Context, gw gateway.Client) (*gateway.Result, error) {
		_, dgst, config, err := gw.ResolveImageConfig(ctx, ref, sourceresolver.Opt{
			ImageOpt: &sourceresolver.ResolveImageOpt{ResolveMode: "pull"},
		})
		if err != nil {
			return nil, err
		}
		var img struct {
			Config struct{ User string }
		}
		if err := json.Unmarshal(config, &img); err != nil {
			return nil, fmt.Errorf("the config of %s: %w", ref, err)
		}
		out = driver.ImageConfig{Digest: dgst.String(), User: img.Config.User}
		return gateway.NewResult(), nil
	}, nil)
	if err != nil {
		return driver.ImageConfig{}, pullError(ref, err)
	}
	return out, nil
}
