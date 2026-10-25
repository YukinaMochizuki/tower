// Package tarx writes and extracts the tars files cross hostd's guests
// and host in, as docker cp does: a path named from its parent, regular
// files, directories and links with their modes. Extracting is confined
// to its directory.
package tarx

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Write writes a tar of p, a file or a directory with what it holds,
// its names relative to p's parent, as docker cp does: regular files,
// directories and links, with their modes.
func Write(w io.Writer, p string) error {
	tw := tar.NewWriter(w)
	parent := filepath.Dir(filepath.Clean(p))
	err := filepath.WalkDir(p, func(file string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(file); err != nil {
				return err
			}
		}
		if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&fs.ModeSymlink == 0 {
			return nil // sockets, devices and pipes are not copied
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(parent, file)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		// Who owns it, by name, means nothing to the guest it goes to.
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.CopyN(tw, f, hdr.Size)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// Extract extracts a tar into dir, making it, confined to it: no entry
// is written outside dir, through a link or a name that climbs out.
// Regular files, directories and links are made, with their modes; files
// replace what is there.
func Extract(r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "/"))
		if name == "." {
			continue
		}
		if name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("%q is outside the directory", hdr.Name)
		}
		if err := mkdirAll(root, path.Dir(name)); err != nil {
			return err
		}
		mode := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(name, mode|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			_ = root.Chmod(name, mode|0o700)
		case tar.TypeReg:
			_ = root.Remove(name)
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			_ = root.Chmod(name, mode)
		case tar.TypeSymlink:
			_ = root.Remove(name)
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
		default:
			// Hard links, devices and the rest are not made.
		}
	}
}

// mkdirAll makes a directory of root and its parents.
func mkdirAll(root *os.Root, dir string) error {
	if dir == "." || dir == "/" {
		return nil
	}
	if err := mkdirAll(root, path.Dir(dir)); err != nil {
		return err
	}
	if err := root.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}
