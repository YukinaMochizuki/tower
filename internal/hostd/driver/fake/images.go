package fake

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// Registry is where the fake's images are pushed.
const Registry = "registry.tower.internal"

type images struct{ h *Host }

func (i images) Version(context.Context) (string, error) { return Version, nil }

func (i images) Pull(ctx context.Context, ref string) error {
	if !strings.Contains(ref, "@sha256:") {
		return fmt.Errorf("%s is no reference by digest", ref)
	}
	i.h.mu.Lock()
	hold := i.h.holdPulls[ref]
	i.h.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return i.h.change("images.Pull", func() error {
		if i.h.st.Pulled[ref] {
			return nil
		}
		if !i.h.st.Registry[ref] {
			return fmt.Errorf("image %s: %w", ref, driver.ErrNotFound)
		}
		if err := i.h.room(ImageSize); err != nil {
			return err
		}
		i.h.st.Pulled[ref] = true
		return nil
	})
}

// inTar is a path of a build's tar, "" its root; one leaving it is none.
func inTar(p string) (string, error) {
	c := path.Clean(p)
	switch {
	case c == ".":
		return "", nil
	case path.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../"):
		return "", fmt.Errorf("%q leaves the build context", p)
	}
	return c, nil
}

// Build reads the context's Dockerfile, which must start with FROM, and
// names the image by the hash of the whole context and where in it the
// build is. The image runs as the user its last USER names, else as what
// it is FROM runs as. It is on the host at once, as pulled, and the
// registry holds it once it is pushed.
func (i images) Build(_ context.Context, spec driver.BuildSpec) (string, error) {
	b, err := io.ReadAll(spec.Context)
	if err != nil {
		return "", err
	}
	files, err := untar(b)
	if err != nil {
		return "", fmt.Errorf("the build context: %w", err)
	}
	dir, err := inTar(spec.Dir)
	if err != nil {
		return "", err
	}
	if dir != "" && !slices.ContainsFunc(slices.Collect(maps.Keys(files)), func(f string) bool { return strings.HasPrefix(f, dir+"/") }) {
		return "", fmt.Errorf("the build context holds no directory %s", dir)
	}
	name := spec.Dockerfile
	if name == "" {
		name = path.Join(dir, "Dockerfile")
	}
	if name, err = inTar(name); err != nil {
		return "", err
	}
	dockerfile, ok := files[name]
	if !ok {
		return "", fmt.Errorf("the build context holds no %s", name)
	}
	lines := strings.Split(strings.TrimSpace(string(dockerfile)), "\n")
	from, ok := strings.CutPrefix(lines[0], "FROM ")
	if !ok {
		return "", errors.New("step 1: the Dockerfile does not start with FROM")
	}
	sum := sha256.Sum256(append(b, []byte("\x00"+dir+"\x00"+name)...))
	ref := Registry + "/" + spec.Repository + "@sha256:" + hex.EncodeToString(sum[:])
	err = i.h.change("images.Build", func() error {
		if !i.h.st.Pulled[ref] {
			if err := i.h.room(ImageSize); err != nil {
				return err
			}
		}
		i.h.st.Pulled[ref] = true
		if !i.h.st.Registry[ref] && !slices.Contains(i.h.st.Unpushed, ref) {
			i.h.st.Unpushed = append(i.h.st.Unpushed, ref)
		}
		user := i.h.st.Users[strings.TrimSpace(from)]
		for _, l := range lines {
			if u, ok := strings.CutPrefix(strings.TrimSpace(l), "USER "); ok {
				user = strings.TrimSpace(u)
			}
		}
		if i.h.st.Users == nil {
			i.h.st.Users = map[string]string{}
		}
		i.h.st.Users[ref] = user
		return nil
	})
	return ref, err
}

// Push puts an image built here in the registry.
func (i images) Push(_ context.Context, ref string) error {
	return i.h.change("images.Push", func() error {
		n := slices.Index(i.h.st.Unpushed, ref)
		if n < 0 {
			return nil
		}
		i.h.st.Registry[ref] = true
		i.h.st.Unpushed = slices.Delete(i.h.st.Unpushed, n, n+1)
		return nil
	})
}

func (i images) Unpushed(context.Context) ([]string, error) {
	var out []string
	err := i.h.read("images.Unpushed", func() error {
		out = slices.Clone(i.h.st.Unpushed)
		return nil
	})
	return out, err
}

// Local is ref itself, pulled or built.
func (i images) Local(_ context.Context, ref string) (string, error) {
	err := i.h.read("images.Local", func() error {
		if !i.h.st.Pulled[ref] {
			return fmt.Errorf("image %s is not on the host: %w", ref, driver.ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return ref, nil
}

// List lists what was pulled or built, an image a reference.
func (i images) List(context.Context) ([]driver.HostImage, error) {
	var out []driver.HostImage
	err := i.h.read("images.List", func() error {
		for _, ref := range slices.Sorted(maps.Keys(i.h.st.Pulled)) {
			sum := sha256.Sum256([]byte(ref))
			out = append(out, driver.HostImage{ID: "sha256:" + hex.EncodeToString(sum[:]), Refs: []string{ref}})
		}
		return nil
	})
	return out, err
}

// Remove forgets a pull or a build, but of an image a container is made
// of; what is to be pushed stays to be.
func (i images) Remove(_ context.Context, ref string) error {
	return i.h.change("images.Remove", func() error {
		for name, g := range i.h.st.Guests {
			if strings.HasPrefix(name, "container/") && g.Image == ref {
				return fmt.Errorf("image %s: container %s is made of it: %w", ref, g.Spec.Name, driver.ErrBusy)
			}
		}
		delete(i.h.st.Pulled, ref)
		return nil
	})
}

// DiskUsage is ImageSize an image pulled.
func (i images) DiskUsage(context.Context) (uint64, error) {
	var n uint64
	err := i.h.read("images.DiskUsage", func() error {
		n = uint64(len(i.h.st.Pulled)) * ImageSize
		return nil
	})
	return n, err
}

// Resolve answers for what tower's registry holds or is to be pushed to
// it, and for any image of another registry, as a public one holds them: a
// reference by digest, its digest, a tag the hash of its name.
func (i images) Resolve(_ context.Context, ref string) (driver.ImageConfig, error) {
	var cfg driver.ImageConfig
	err := i.h.read("images.Resolve", func() error {
		if !i.h.st.Registry[ref] && !slices.Contains(i.h.st.Unpushed, ref) && strings.HasPrefix(ref, Registry+"/") {
			return fmt.Errorf("image %s: %w", ref, driver.ErrNotFound)
		}
		cfg.User = i.h.st.Users[ref]
		if _, digest, ok := strings.Cut(ref, "@"); ok {
			cfg.Digest = digest
			return nil
		}
		sum := sha256.Sum256([]byte(ref))
		cfg.Digest = "sha256:" + hex.EncodeToString(sum[:])
		return nil
	})
	return cfg, err
}

// untar reads a tar's regular files by their cleaned names.
func untar(b []byte) (map[string][]byte, error) {
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		out[strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")] = data
	}
}

// writeTar writes files as a tar, in name order.
func writeTar(w io.Writer, files map[string][]byte) error {
	tw := tar.NewWriter(w)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(files[name]); err != nil {
			return err
		}
	}
	return tw.Close()
}
