// Package images is hostd's image driver (driver.Images): it pulls from
// tower's registry into Docker, and builds a context with BuildKit into
// Docker, holding the image until it is pushed to the registry
// (docs/decisions/images.md). BuildKit exports a build to an OCI layout
// the driver holds; it is loaded into Docker from there, read by a build
// FROM it, and pushed from there, so what runs here is what the registry
// gets, byte for byte. The registry's credentials are hostd's own
// configuration, held in memory and handed to Docker with each pull, to
// BuildKit through its session and to the push; they are never written to
// a Docker config or anywhere else on the host.
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
	"slices"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/core/content"
	contentlocal "github.com/containerd/containerd/v2/plugins/content/local"
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
	"github.com/opencontainers/go-digest"
	"github.com/tonistiigi/fsutil"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/YukinaMochizuki/tower/internal/guest/tarx"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
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
	// UnpushedDir is a directory, root's alone, where what was built is
	// held until it is pushed; it must outlive a restart (held.go).
	UnpushedDir string
}

// Driver is the image driver. It is safe for concurrent use.
type Driver struct {
	cfg Config
	// held guards UnpushedDir's entries: what reads one (a build FROM it,
	// Resolve) holds it shared, what puts one in place or removes it
	// alone, so a push ending never removes what a build reads.
	held sync.RWMutex
}

var _ driver.Images = (*Driver)(nil)

// New makes the scratch and unpushed directories, removing what a build
// cut short left in the latter.
func New(cfg Config) (*Driver, error) {
	if cfg.Registry == "" {
		return nil, errors.New("no registry")
	}
	if cfg.UnpushedDir == "" {
		return nil, errors.New("no directory to hold images until they are pushed")
	}
	for _, dir := range []string{cfg.ScratchDir, cfg.UnpushedDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(cfg.UnpushedDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), partialDir) {
			if err := os.RemoveAll(filepath.Join(cfg.UnpushedDir, e.Name())); err != nil {
				return nil, err
			}
		}
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
// it is tower's. One there already, pulled or built here, is not pulled
// again.
func (d *Driver) Pull(ctx context.Context, ref string) error {
	if _, err := d.Local(ctx, ref); err == nil {
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

// Local is ref itself if Docker knows it by it, as one pulled; else the
// name a build here loaded it as (localName).
func (d *Driver) Local(ctx context.Context, ref string) (string, error) {
	names := []string{ref}
	if l, ok := localName(ref); ok {
		names = append(names, l)
	}
	for _, n := range names {
		_, err := d.cfg.Docker.ImageInspect(ctx, n)
		if err == nil {
			return n, nil
		}
		if !cerrdefs.IsNotFound(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("image %s is not on the host: %w", ref, driver.ErrNotFound)
}

// List lists the images Docker holds that every reference of names tower's
// registry: what hostd pulled or built. Another's, as the VMs' empty image
// or one pulled by hand, is not listed. One built here is listed by the
// reference by digest its tag stands for.
func (d *Driver) List(ctx context.Context) ([]driver.HostImage, error) {
	res, err := d.cfg.Docker.ImageList(ctx, dockerclient.ImageListOptions{})
	if err != nil {
		return nil, err
	}
	var out []driver.HostImage
	for _, im := range res.Items {
		refs := append(slices.Clone(im.RepoDigests), im.RepoTags...)
		ours := len(refs) > 0
		for _, r := range refs {
			ours = ours && registryOf(r) == d.cfg.Registry
		}
		if !ours {
			continue
		}
		byDigest := slices.Clone(im.RepoDigests)
		for _, tag := range im.RepoTags {
			if r, ok := builtRef(tag); ok && !slices.Contains(byDigest, r) {
				byDigest = append(byDigest, r)
			}
		}
		out = append(out, driver.HostImage{ID: im.ID, Refs: byDigest})
	}
	return out, nil
}

// Remove removes a reference, and the name a build here gave it, without
// forcing them: Docker refuses one a container is made of. What is held
// to be pushed stays.
func (d *Driver) Remove(ctx context.Context, ref string) error {
	names := []string{ref}
	if l, ok := localName(ref); ok {
		names = append(names, l)
	}
	for _, n := range names {
		_, err := d.cfg.Docker.ImageRemove(ctx, n, dockerclient.ImageRemoveOptions{PruneChildren: true})
		switch {
		case err == nil || cerrdefs.IsNotFound(err):
		case cerrdefs.IsConflict(err):
			return fmt.Errorf("image %s: %w: %w", ref, err, driver.ErrBusy)
		default:
			return err
		}
	}
	return nil
}

// DiskUsage is what Docker says its images hold, and what is held to be
// pushed.
func (d *Driver) DiskUsage(ctx context.Context) (uint64, error) {
	res, err := d.cfg.Docker.DiskUsage(ctx, dockerclient.DiskUsageOptions{Images: true})
	if err != nil {
		return 0, err
	}
	held, err := d.heldSize()
	if err != nil {
		return 0, err
	}
	return uint64(max(res.Images.TotalSize, 0)) + held, nil
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

// Build builds a context with its Dockerfile into an OCI layout it holds
// to be pushed to Registry/repository (Push), loads that into Docker, and
// returns its reference by digest. Timestamps are the epoch
// (SOURCE_DATE_EPOCH=0, rewritten in the layers), so the same context
// builds the same digest. The context is the tar's Dir; the Dockerfile,
// anywhere in the tar, is given to BuildKit apart from it, as docker build
// -f does. An image held, not pushed yet, that the Dockerfile names is
// given to BuildKit from its layout, as a named context of its reference,
// so a build FROM it reads it from the host rather than the registry.
//
// One export, to an OCI layout, rather than BuildKit's image and docker
// exporters at once: the layout is the one copy of the image, and Docker
// and the registry get it byte for byte, the registry after the build,
// when the push is apart from it, and from a layout on disk, which
// outlives the build session and a restart of hostd or BuildKit.
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
	attrs := map[string]string{"build-arg:SOURCE_DATE_EPOCH": "0", "filename": path.Base(dockerfile)}
	from, release, err := d.heldFrom(filepath.Join(dockerfileDir, path.Base(dockerfile)), attrs)
	if err != nil {
		return "", err
	}
	defer release()
	partial, err := os.MkdirTemp(d.cfg.UnpushedDir, partialDir)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(partial) }()
	layout := filepath.Join(partial, entryLayout)
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
		FrontendAttrs: attrs,
		LocalMounts:   map[string]fsutil.FS{"context": fs, "dockerfile": dfs},
		OCIStores:     from,
		Exports: []client.ExportEntry{{Type: client.ExporterOCI, OutputDir: layout, Attrs: map[string]string{
			"name": name, "tar": "false", "oci-mediatypes": "true", "rewrite-timestamp": "true",
		}}},
		Session: []session.Attachable{authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{AuthConfigProvider: d.auth})},
	}, status)
	release()
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
	dgst := resp.ExporterResponse[exptypes.ExporterImageDigestKey]
	if dgst == "" {
		return "", fmt.Errorf("building %s: BuildKit named no digest", repository)
	}
	ref := name + "@" + dgst
	local, ok := localName(ref)
	if !ok {
		return "", fmt.Errorf("building %s: BuildKit named the digest %q", repository, dgst)
	}
	if err := d.load(ctx, layout, digest.Digest(dgst), local); err != nil {
		if strings.Contains(err.Error(), "no space left") {
			return "", fmt.Errorf("building %s: %w: %w", repository, err, driver.ErrNoSpace)
		}
		return "", fmt.Errorf("building %s: %w", repository, err)
	}
	if err := d.keep(partial, ref); err != nil {
		return "", fmt.Errorf("building %s: holding it to be pushed: %w", repository, err)
	}
	return ref, nil
}

// heldFrom gives a build the held images its Dockerfile names, each as
// the named context of its reference (attrs) from its layout (the
// stores), and holds them until release, which may be called more than
// once. A Dockerfile not a file names none.
func (d *Driver) heldFrom(dockerfile string, attrs map[string]string) (map[string]content.Store, func(), error) {
	none := func() {}
	text := regularFile(dockerfile)
	if text == "" {
		return nil, none, nil
	}
	d.held.RLock()
	release := sync.OnceFunc(d.held.RUnlock)
	list, err := d.heldImages()
	if err != nil {
		release()
		return nil, none, err
	}
	stores := map[string]content.Store{}
	for i, h := range list {
		if !strings.Contains(text, h.ref) {
			continue
		}
		store, err := contentlocal.NewStore(h.layout())
		if err != nil {
			release()
			return nil, none, err
		}
		id := fmt.Sprintf("held%d", i)
		stores[id] = store
		attrs["context:"+h.ref] = "oci-layout://" + id + "@" + h.digest().String()
	}
	if len(stores) == 0 {
		release()
		return nil, none, nil
	}
	return stores, release, nil
}

// regularFile is what the regular file at p holds, "" for anything else
// or what cannot be read: BuildKit says what is wrong with a Dockerfile.
func regularFile(p string) string {
	if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() {
		return ""
	}
	b, _ := os.ReadFile(p)
	return string(b)
}

// Resolve reads a held image from its layout. Another it asks BuildKit to
// resolve in its registry, with the registry's credentials for tower's,
// reading its manifest and config but none of its layers.
func (d *Driver) Resolve(ctx context.Context, ref string) (driver.ImageConfig, error) {
	d.held.RLock()
	h, ok, err := d.heldOf(ref)
	if err == nil && ok {
		var cfg driver.ImageConfig
		cfg, err = configOf(h)
		d.held.RUnlock()
		return cfg, err
	}
	d.held.RUnlock()
	if err != nil {
		return driver.ImageConfig{}, err
	}
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
