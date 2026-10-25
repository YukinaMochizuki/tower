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

func (i images) Pull(_ context.Context, ref string) error {
	if !strings.Contains(ref, "@sha256:") {
		return fmt.Errorf("%s is no reference by digest", ref)
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

// Build reads the context's Dockerfile, which must start with FROM, and
// names the image by the hash of the whole context.
func (i images) Build(_ context.Context, repository string, buildContext io.Reader) (string, error) {
	b, err := io.ReadAll(buildContext)
	if err != nil {
		return "", err
	}
	files, err := untar(b)
	if err != nil {
		return "", fmt.Errorf("the build context: %w", err)
	}
	dockerfile, ok := files["Dockerfile"]
	if !ok {
		return "", errors.New("the build context holds no Dockerfile")
	}
	if !strings.HasPrefix(strings.TrimSpace(string(dockerfile)), "FROM ") {
		return "", errors.New("step 1: the Dockerfile does not start with FROM")
	}
	sum := sha256.Sum256(b)
	ref := Registry + "/" + repository + "@sha256:" + hex.EncodeToString(sum[:])
	err = i.h.change("images.Build", func() error {
		if !i.h.st.Registry[ref] {
			if err := i.h.room(ImageSize); err != nil {
				return err
			}
		}
		i.h.st.Registry[ref] = true
		return nil
	})
	return ref, err
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
