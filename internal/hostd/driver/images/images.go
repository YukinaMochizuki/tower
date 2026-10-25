// Package images is hostd's image driver (driver.Images): it pulls from
// tower's registry into Docker, and builds a context with BuildKit,
// pushing the image to the registry (docs/decisions/images.md). The
// registry's credentials are hostd's own configuration, held in memory and
// handed to Docker with each pull and to BuildKit through its session;
// they are never written to a Docker config or anywhere else on the host.
package images

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/docker/cli/cli/config/types"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
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

// Build builds a context with its Dockerfile and pushes the image to
// Registry/repository, returning its reference by digest. Timestamps are
// the epoch (SOURCE_DATE_EPOCH=0, rewritten in the layers), so the same
// context builds the same digest.
func (d *Driver) Build(ctx context.Context, repository string, buildContext io.Reader) (string, error) {
	if repository == "" || strings.ContainsAny(repository, "@: ") {
		return "", fmt.Errorf("%q is no repository", repository)
	}
	dir, err := os.MkdirTemp(d.cfg.ScratchDir, "build-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := tarx.Extract(buildContext, dir); err != nil {
		return "", fmt.Errorf("unpacking the build context: %w", err)
	}
	fs, err := fsutil.NewFS(dir)
	if err != nil {
		return "", err
	}
	c, err := client.New(ctx, d.cfg.BuildKit)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	name := d.cfg.Registry + "/" + repository
	resp, err := c.Solve(ctx, nil, client.SolveOpt{
		Frontend:      "dockerfile.v0",
		FrontendAttrs: map[string]string{"build-arg:SOURCE_DATE_EPOCH": "0"},
		LocalMounts:   map[string]fsutil.FS{"context": fs, "dockerfile": fs},
		Exports: []client.ExportEntry{{Type: client.ExporterImage, Attrs: map[string]string{
			"name": name, "push": "true", "rewrite-timestamp": "true",
		}}},
		Session: []session.Attachable{authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{AuthConfigProvider: d.auth})},
	}, nil)
	if err != nil {
		if strings.Contains(err.Error(), "no space left") {
			return "", fmt.Errorf("building %s: %w: %w", repository, err, driver.ErrNoSpace)
		}
		return "", fmt.Errorf("building %s: %w", repository, err)
	}
	digest := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	if digest == "" {
		return "", fmt.Errorf("building %s: BuildKit named no digest", repository)
	}
	return name + "@" + digest, nil
}
