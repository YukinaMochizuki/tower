package images

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	contentlocal "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/platforms"
	dockerclient "github.com/moby/moby/client"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// What a build made is held in UnpushedDir until it is pushed, an entry a
// directory of its own: ref, the reference by digest it was built as, and
// layout, the OCI image layout BuildKit exported it to, which is what is
// pushed, byte for byte, and what a build FROM it reads. An entry is made
// whole under a name starting with ".build-" and renamed into place, so
// one whose name has no dot is complete; one with a dot is a build's
// leftover, which New removes.
const (
	entryRef    = "ref"
	entryLayout = "layout"
	partialDir  = ".build-"
)

// held is an image built here and not pushed yet.
type held struct {
	ref, dir string
	built    time.Time
}

func (h held) layout() string { return filepath.Join(h.dir, entryLayout) }

func (h held) digest() digest.Digest {
	_, d, _ := strings.Cut(h.ref, "@")
	return digest.Digest(d)
}

// entryDir is where ref is held, whether it is or not.
func (d *Driver) entryDir(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return filepath.Join(d.cfg.UnpushedDir, hex.EncodeToString(sum[:16]))
}

// heldImages are the images held, oldest first. The caller holds d.held.
func (d *Driver) heldImages() ([]held, error) {
	entries, err := os.ReadDir(d.cfg.UnpushedDir)
	if err != nil {
		return nil, err
	}
	var out []held
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(d.cfg.UnpushedDir, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, entryRef))
		if err != nil {
			return nil, fmt.Errorf("held image %s: %w", e.Name(), err)
		}
		st, err := os.Stat(filepath.Join(dir, entryRef))
		if err != nil {
			return nil, err
		}
		out = append(out, held{ref: strings.TrimSpace(string(b)), dir: dir, built: st.ModTime()})
	}
	slices.SortStableFunc(out, func(a, b held) int { return a.built.Compare(b.built) })
	return out, nil
}

// heldOf is ref's entry, if it is held. The caller holds d.held.
func (d *Driver) heldOf(ref string) (held, bool, error) {
	dir := d.entryDir(ref)
	b, err := os.ReadFile(filepath.Join(dir, entryRef))
	if errors.Is(err, fs.ErrNotExist) {
		return held{}, false, nil
	}
	if err != nil {
		return held{}, false, err
	}
	if strings.TrimSpace(string(b)) != ref {
		return held{}, false, fmt.Errorf("held image %s holds %s", dir, b)
	}
	return held{ref: ref, dir: dir}, true, nil
}

// keep moves a build's partial entry into place as ref's, which it is
// unless ref is held already, when what it holds is the same.
func (d *Driver) keep(partial, ref string) error {
	if err := os.WriteFile(filepath.Join(partial, entryRef), []byte(ref+"\n"), 0o600); err != nil {
		return err
	}
	d.held.Lock()
	defer d.held.Unlock()
	if _, ok, err := d.heldOf(ref); err != nil || ok {
		return err
	}
	return os.Rename(partial, d.entryDir(ref))
}

func (d *Driver) Unpushed(context.Context) ([]string, error) {
	if d.cfg.NoDocker {
		return nil, nil
	}
	d.held.RLock()
	defer d.held.RUnlock()
	list, err := d.heldImages()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, h := range list {
		out = append(out, h.ref)
	}
	return out, nil
}

// blob reads a blob of an OCI layout.
func blob(layout string, d digest.Digest) ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(layout, "blobs", d.Algorithm().String(), d.Encoded()))
}

// manifestOf reads an image's manifest from an OCI layout, and its
// descriptor. An index, as of several platforms, is none: hostd builds
// for its own.
func manifestOf(layout string, d digest.Digest) (ocispec.Manifest, ocispec.Descriptor, error) {
	b, err := blob(layout, d)
	if err != nil {
		return ocispec.Manifest{}, ocispec.Descriptor{}, fmt.Errorf("the manifest of %s: %w", d, err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return ocispec.Manifest{}, ocispec.Descriptor{}, fmt.Errorf("the manifest of %s: %w", d, err)
	}
	switch m.MediaType {
	case ocispec.MediaTypeImageManifest, "application/vnd.docker.distribution.manifest.v2+json":
	default:
		return ocispec.Manifest{}, ocispec.Descriptor{}, fmt.Errorf("%s is a %q, not an image manifest", d, m.MediaType)
	}
	return m, ocispec.Descriptor{MediaType: m.MediaType, Digest: d, Size: int64(len(b))}, nil
}

// configOf reads what a held image says of itself.
func configOf(h held) (driver.ImageConfig, error) {
	m, _, err := manifestOf(h.layout(), h.digest())
	if err != nil {
		return driver.ImageConfig{}, err
	}
	b, err := blob(h.layout(), m.Config.Digest)
	if err != nil {
		return driver.ImageConfig{}, fmt.Errorf("the config of %s: %w", h.ref, err)
	}
	var img struct {
		Config struct{ User string }
	}
	if err := json.Unmarshal(b, &img); err != nil {
		return driver.ImageConfig{}, fmt.Errorf("the config of %s: %w", h.ref, err)
	}
	return driver.ImageConfig{Digest: h.digest().String(), User: img.Config.User}, nil
}

// localName is the name Docker knows an image built here by: its
// repository, tagged with its digest. Docker's own image store names an
// image loaded into it by its tags alone, never by the digest of a
// manifest it did not pull, so the tag stands for it; it is the tag the
// image is pushed with too.
func localName(ref string) (string, bool) {
	name, d, ok := strings.Cut(ref, "@")
	if !ok || digest.Digest(d).Validate() != nil {
		return "", false
	}
	return name + ":" + digestTag(digest.Digest(d)), true
}

func digestTag(d digest.Digest) string { return d.Algorithm().String() + "-" + d.Encoded() }

// builtRef is the reference by digest a name of localName's stands for.
func builtRef(name string) (string, bool) {
	i := strings.LastIndex(name, ":")
	if i < 0 || strings.Contains(name[i:], "/") {
		return "", false
	}
	algo, enc, ok := strings.Cut(name[i+1:], "-")
	if !ok {
		return "", false
	}
	d := digest.NewDigestFromEncoded(digest.Algorithm(algo), enc)
	if d.Validate() != nil {
		return "", false
	}
	return name[:i] + "@" + d.String(), true
}

// load loads the image an OCI layout holds into Docker as name: a tar of
// the layout, which Docker's containerd image store reads, with the
// manifest.json its own store reads beside it, as BuildKit's docker
// exporter writes them. What it loads is what is pushed.
func (d *Driver) load(ctx context.Context, layout string, dgst digest.Digest, name string) error {
	m, desc, err := manifestOf(layout, dgst)
	if err != nil {
		return err
	}
	_, tag, _ := strings.Cut(name[strings.LastIndex(name, "/")+1:], ":")
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeArchive(pw, layout, m, desc, name, tag)) }()
	res, err := d.cfg.Docker.ImageLoad(ctx, pr, dockerclient.ImageLoadWithQuiet(true))
	if err != nil {
		_ = pr.CloseWithError(err)
		return fmt.Errorf("loading %s into Docker: %w", name, err)
	}
	out, rerr := io.ReadAll(res)
	_ = res.Close()
	_ = pr.CloseWithError(errors.New("load ended"))
	if msg := loadError(out); msg != "" {
		return fmt.Errorf("loading %s into Docker: %s", name, msg)
	}
	if rerr != nil {
		return fmt.Errorf("loading %s into Docker: %w", name, rerr)
	}
	if _, err := d.cfg.Docker.ImageInspect(ctx, name); err != nil {
		return fmt.Errorf("%s is not in Docker after its load: %w", name, err)
	}
	return nil
}

// loadError is the error Docker's answer to a load holds, "" for none.
func loadError(out []byte) string {
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail *struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err != nil {
			// The end, or what is no message; the inspect after says
			// whether the image is there.
			return ""
		}
		if msg.ErrorDetail != nil && msg.ErrorDetail.Message != "" {
			return msg.ErrorDetail.Message
		}
		if msg.Error != "" {
			return msg.Error
		}
	}
}

// writeArchive writes the image of m as a tar Docker loads.
func writeArchive(w io.Writer, layout string, m ocispec.Manifest, desc ocispec.Descriptor, name, tag string) error {
	tw := tar.NewWriter(w)
	file := func(p string, b []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: p, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := tw.Write(b)
		return err
	}
	blobPath := func(d digest.Digest) string { return "blobs/" + d.Algorithm().String() + "/" + d.Encoded() }
	desc.Annotations = map[string]string{"io.containerd.image.name": name, ocispec.AnnotationRefName: tag}
	index, err := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{desc},
	})
	if err != nil {
		return err
	}
	type dockerManifest struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	dm := dockerManifest{Config: blobPath(m.Config.Digest), RepoTags: []string{name}}
	blobs := []digest.Digest{desc.Digest, m.Config.Digest}
	for _, l := range m.Layers {
		dm.Layers = append(dm.Layers, blobPath(l.Digest))
		blobs = append(blobs, l.Digest)
	}
	manifests, err := json.Marshal([]dockerManifest{dm})
	if err != nil {
		return err
	}
	for _, dir := range []string{"blobs/", "blobs/" + desc.Digest.Algorithm().String() + "/"} {
		if err := tw.WriteHeader(&tar.Header{Name: dir, Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
			return err
		}
	}
	if err := file(ocispec.ImageLayoutFile, []byte(`{"imageLayoutVersion":"`+ocispec.ImageLayoutVersion+`"}`)); err != nil {
		return err
	}
	if err := file("index.json", index); err != nil {
		return err
	}
	if err := file("manifest.json", manifests); err != nil {
		return err
	}
	seen := map[digest.Digest]bool{}
	for _, b := range blobs {
		if seen[b] {
			continue
		}
		seen[b] = true
		f, err := os.Open(filepath.Join(layout, filepath.FromSlash(blobPath(b))))
		if err != nil {
			return err
		}
		st, err := f.Stat()
		if err == nil {
			err = tw.WriteHeader(&tar.Header{Name: blobPath(b), Mode: 0o644, Size: st.Size(), Typeflag: tar.TypeReg})
		}
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// Push pushes a held image's layout to the registry as it is, so the
// registry holds the digest Build returned, tagged with it (localName) so
// that the registry's collection of untagged manifests never takes it;
// then it is no longer held. A blob the registry holds already is not
// sent again, so a push cut short goes on where it ended.
func (d *Driver) Push(ctx context.Context, ref string) error {
	if d.cfg.NoDocker {
		// Build pushed it.
		return nil
	}
	d.held.RLock()
	h, ok, err := d.heldOf(ref)
	d.held.RUnlock()
	if err != nil || !ok {
		return err
	}
	store, err := contentlocal.NewStore(h.layout())
	if err != nil {
		return err
	}
	_, desc, err := manifestOf(h.layout(), h.digest())
	if err != nil {
		return err
	}
	name, _, _ := strings.Cut(ref, "@")
	resolver := docker.NewResolver(docker.ResolverOptions{Hosts: d.hosts(ctx)})
	pusher, err := resolver.Pusher(ctx, name+":"+digestTag(desc.Digest)+"@"+desc.Digest.String())
	if err != nil {
		return fmt.Errorf("pushing %s: %w", ref, err)
	}
	if err := remotes.PushContent(ctx, pusher, desc, store, nil, platforms.All, nil); err != nil {
		return fmt.Errorf("pushing %s: %w", ref, err)
	}
	d.held.Lock()
	defer d.held.Unlock()
	return os.RemoveAll(h.dir)
}

// hosts reach the registry as Docker does: over HTTPS, falling back to
// plain HTTP for a registry Docker is told is insecure, as a testbed's is
// (deploy/hostd/install-deps.sh); with the registry's credentials and no
// other.
func (d *Driver) hosts(ctx context.Context) docker.RegistryHosts {
	var tr http.RoundTripper = docker.DefaultHTTPTransport(nil)
	if d.insecure(ctx) {
		tr = docker.NewHTTPFallback(tr)
	}
	c := &http.Client{Transport: tr}
	auth := docker.NewDockerAuthorizer(docker.WithAuthClient(c), docker.WithAuthCreds(func(host string) (string, string, error) {
		if host != d.cfg.Registry || d.cfg.Username == "" {
			return "", "", nil
		}
		return d.cfg.Username, d.cfg.Password, nil
	}))
	return docker.ConfigureDefaultRegistries(docker.WithClient(c), docker.WithAuthorizer(auth))
}

// insecure reports whether Docker is told the registry is insecure: named
// among its insecure registries, or at an address of their ranges.
func (d *Driver) insecure(ctx context.Context) bool {
	res, err := d.cfg.Docker.Info(ctx, dockerclient.InfoOptions{})
	if err != nil || res.Info.RegistryConfig == nil {
		return false
	}
	rc := res.Info.RegistryConfig
	if ic := rc.IndexConfigs[d.cfg.Registry]; ic != nil {
		return !ic.Secure
	}
	host := d.cfg.Registry
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, p := range rc.InsecureRegistryCIDRs {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// heldSize is what the held images hold on disk.
func (d *Driver) heldSize() (uint64, error) {
	var n uint64
	err := filepath.WalkDir(d.cfg.UnpushedDir, func(_ string, e fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || e.IsDir() {
			return err
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err == nil {
			n += uint64(max(info.Size(), 0))
		}
		return err
	})
	return n, err
}
