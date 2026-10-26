package images

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	dockerclient "github.com/moby/moby/client"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestLocalNameStandsForTheDigest(t *testing.T) {
	ref := "reg.example:5000/p/box@sha256:" + strings.Repeat("ab", 32)
	name, ok := localName(ref)
	if !ok || name != "reg.example:5000/p/box:sha256-"+strings.Repeat("ab", 32) {
		t.Fatalf("localName(%q) = %q, %v", ref, name, ok)
	}
	if back, ok := builtRef(name); !ok || back != ref {
		t.Errorf("builtRef(%q) = %q, %v; want %q", name, back, ok, ref)
	}
	for _, n := range []string{"reg.example:5000/p/box:latest", "reg.example:5000/p/box", "reg.example:5000/p/box:sha256-zz"} {
		if r, ok := builtRef(n); ok {
			t.Errorf("builtRef(%q) = %q; it stands for no digest", n, r)
		}
	}
	if _, ok := localName("reg.example:5000/p/box:latest"); ok {
		t.Error("a reference by tag has a local name")
	}
}

// writeBlob puts b in the OCI layout at dir.
func writeBlob(t *testing.T, dir string, b []byte) ocispec.Descriptor {
	t.Helper()
	d := digest.FromBytes(b)
	p := filepath.Join(dir, "blobs", "sha256", d.Encoded())
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return ocispec.Descriptor{Digest: d, Size: int64(len(b))}
}

// heldImage makes an image of one layer held for ref's repository on the
// driver, as a build leaves it, and returns its reference.
func heldImage(t *testing.T, d *Driver, repository, user string) string {
	t.Helper()
	layout := filepath.Join(t.TempDir(), "layout")
	cfg, err := json.Marshal(map[string]any{"architecture": "amd64", "os": "linux", "config": map[string]string{"User": user}})
	if err != nil {
		t.Fatal(err)
	}
	cdesc := writeBlob(t, layout, cfg)
	cdesc.MediaType = ocispec.MediaTypeImageConfig
	ldesc := writeBlob(t, layout, []byte("a layer of "+repository))
	ldesc.MediaType = ocispec.MediaTypeImageLayerGzip
	m, err := json.Marshal(ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, Config: cdesc, Layers: []ocispec.Descriptor{ldesc}})
	if err != nil {
		t.Fatal(err)
	}
	mdesc := writeBlob(t, layout, m)
	ref := d.cfg.Registry + "/" + repository + "@" + mdesc.Digest.String()
	partial, err := os.MkdirTemp(d.cfg.UnpushedDir, partialDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(layout, filepath.Join(partial, entryLayout)); err != nil {
		t.Fatal(err)
	}
	if err := d.keep(partial, ref); err != nil {
		t.Fatal(err)
	}
	return ref
}

// fakeRegistry is the part of the registry API a push takes, behind basic
// auth, over plain HTTP; failing, while set, fails every blob upload.
type fakeRegistry struct {
	user, password string
	mu             sync.Mutex
	blobs          map[digest.Digest][]byte
	// manifests are by repository and tag or digest.
	manifests map[string][]byte
	failing   bool
	uploads   int
}

func (r *fakeRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if u, p, ok := req.BasicAuth(); !ok || u != r.user || p != r.password {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p := strings.TrimPrefix(req.URL.Path, "/v2/")
	switch {
	case req.URL.Path == "/v2/" || req.URL.Path == "/v2":
		w.WriteHeader(http.StatusOK)
	case strings.Contains(p, "/blobs/uploads/"):
		switch req.Method {
		case http.MethodPost:
			r.uploads++
			w.Header().Set("Location", "/v2/"+p+fmt.Sprint(r.uploads))
			w.WriteHeader(http.StatusAccepted)
		case http.MethodPut, http.MethodPatch:
			if r.failing {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			b, _ := io.ReadAll(req.Body)
			d := digest.Digest(req.URL.Query().Get("digest"))
			if req.Method == http.MethodPatch {
				w.Header().Set("Location", req.URL.Path)
				w.WriteHeader(http.StatusAccepted)
				return
			}
			if d.Validate() != nil || digest.FromBytes(b) != d {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			r.blobs[d] = b
			w.Header().Set("Docker-Content-Digest", d.String())
			w.WriteHeader(http.StatusCreated)
		}
	case strings.Contains(p, "/blobs/"):
		d := digest.Digest(p[strings.LastIndex(p, "/")+1:])
		b, ok := r.blobs[d]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		w.Header().Set("Docker-Content-Digest", d.String())
		w.WriteHeader(http.StatusOK)
	case strings.Contains(p, "/manifests/"):
		i := strings.LastIndex(p, "/manifests/")
		key := p[:i] + ":" + p[i+len("/manifests/"):]
		switch req.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(req.Body)
			r.manifests[key] = b
			w.Header().Set("Docker-Content-Digest", digest.FromBytes(b).String())
			w.WriteHeader(http.StatusCreated)
		default:
			b, ok := r.manifests[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", ocispec.MediaTypeImageManifest)
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			w.Header().Set("Docker-Content-Digest", digest.FromBytes(b).String())
			w.WriteHeader(http.StatusOK)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// pushWorld is a driver whose registry is reg, over plain HTTP, which a
// Docker that answers /info alone says is insecure.
func pushWorld(t *testing.T) (*Driver, *fakeRegistry) {
	t.Helper()
	reg := &fakeRegistry{user: "hostd", password: "s3cret", blobs: map[digest.Digest][]byte{}, manifests: map[string][]byte{}}
	rs := httptest.NewServer(reg)
	t.Cleanup(rs.Close)
	host := strings.TrimPrefix(rs.URL, "http://")
	docker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/info") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"RegistryConfig": map[string]any{"IndexConfigs": map[string]any{host: map[string]any{"Name": host, "Secure": false}}},
		})
	}))
	t.Cleanup(docker.Close)
	u, err := url.Parse(docker.URL)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := dockerclient.New(dockerclient.WithHost("tcp://"+u.Host), dockerclient.WithAPIVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(Config{
		Docker: cli, Registry: host, Username: reg.user, Password: reg.password,
		ScratchDir: filepath.Join(t.TempDir(), "scratch"), UnpushedDir: filepath.Join(t.TempDir(), "unpushed"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, reg
}

// TestAHeldImageIsPushedAsItWasBuilt: a push puts the held layout in the
// registry byte for byte, its manifest by digest and tagged with it, and
// lets it go; one that failed leaves it held, and is pushed when tried
// again. Before its push, it resolves from what is held.
func TestAHeldImageIsPushedAsItWasBuilt(t *testing.T) {
	t.Parallel()
	d, reg := pushWorld(t)
	ctx := t.Context()
	ref := heldImage(t, d, "p/box", "4242")
	other := heldImage(t, d, "p/other", "")
	if list, err := d.Unpushed(ctx); err != nil || !slices.Equal(list, []string{ref, other}) {
		t.Fatalf("left to push: %v, %v; want %s then %s", list, err, ref, other)
	}
	if cfg, err := d.Resolve(ctx, ref); err != nil || cfg.User != "4242" || ref != d.cfg.Registry+"/p/box@"+cfg.Digest {
		t.Errorf("%s, held, resolved to %+v, %v", ref, cfg, err)
	}

	reg.mu.Lock()
	reg.failing = true
	reg.mu.Unlock()
	if err := d.Push(ctx, ref); err == nil {
		t.Fatal("a push the registry refused succeeded")
	}
	if list, _ := d.Unpushed(ctx); !slices.Contains(list, ref) {
		t.Fatalf("an image whose push failed is not left to push: %v", list)
	}

	reg.mu.Lock()
	reg.failing = false
	reg.mu.Unlock()
	if err := d.Push(ctx, ref); err != nil {
		t.Fatalf("pushing again: %v", err)
	}
	if list, _ := d.Unpushed(ctx); !slices.Equal(list, []string{other}) {
		t.Errorf("left to push after the push: %v; want %s alone", list, other)
	}
	if err := d.Push(ctx, ref); err != nil {
		t.Errorf("pushing what is pushed: %v", err)
	}
	_, dgst, _ := strings.Cut(ref, "@")
	reg.mu.Lock()
	defer reg.mu.Unlock()
	tagged := reg.manifests["p/box:"+digestTag(digest.Digest(dgst))]
	if tagged == nil || digest.FromBytes(tagged).String() != dgst {
		t.Fatalf("the registry holds %v; want p/box tagged %s with the manifest %s", keys(reg.manifests), digestTag(digest.Digest(dgst)), dgst)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(tagged, &m); err != nil {
		t.Fatal(err)
	}
	for _, b := range append([]ocispec.Descriptor{m.Config}, m.Layers...) {
		if _, ok := reg.blobs[b.Digest]; !ok {
			t.Errorf("the registry lacks the blob %s", b.Digest)
		}
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestTheArchiveDockerLoadsNamesTheImage: what Docker loads holds the
// image's blobs as they are held, its manifest.json naming it by its local
// name, and an index naming it too.
func TestTheArchiveDockerLoadsNamesTheImage(t *testing.T) {
	t.Parallel()
	d, _ := pushWorld(t)
	ref := heldImage(t, d, "p/box", "")
	d.held.RLock()
	h, ok, err := d.heldOf(ref)
	d.held.RUnlock()
	if err != nil || !ok {
		t.Fatalf("%s is not held: %v", ref, err)
	}
	m, desc, err := manifestOf(h.layout(), h.digest())
	if err != nil {
		t.Fatal(err)
	}
	name, _ := localName(ref)
	var b bytes.Buffer
	if err := writeArchive(&b, h.layout(), m, desc, name, digestTag(h.digest())); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	tr := tar.NewReader(&b)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name], _ = io.ReadAll(tr)
	}
	var manifests []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	if err := json.Unmarshal(files["manifest.json"], &manifests); err != nil || len(manifests) != 1 || !slices.Equal(manifests[0].RepoTags, []string{name}) {
		t.Fatalf("manifest.json: %s, %v", files["manifest.json"], err)
	}
	for _, p := range append([]string{manifests[0].Config, "blobs/sha256/" + h.digest().Encoded()}, manifests[0].Layers...) {
		sum := sha256.Sum256(files[p])
		if files[p] == nil || fmt.Sprintf("%x", sum) != filepath.Base(p) {
			t.Errorf("the archive's %s is not the blob it names", p)
		}
	}
	var index ocispec.Index
	if err := json.Unmarshal(files["index.json"], &index); err != nil || len(index.Manifests) != 1 ||
		index.Manifests[0].Digest != h.digest() || index.Manifests[0].Annotations["io.containerd.image.name"] != name {
		t.Errorf("index.json: %s, %v", files["index.json"], err)
	}
}
