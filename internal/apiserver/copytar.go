package apiserver

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// tarMagicAt is where a ustar header, POSIX or GNU, says "ustar".
const tarMagicAt = 257

// tarCheck is a writer reading what it is written as a tar, and failing
// the write once it is not a plain tar of base: one the destination's tar
// would unpack as just what the scanner read, with nothing compressed,
// nothing outside base, and nothing written through a link it holds.
type tarCheck struct {
	base  string
	pw    *io.PipeWriter
	done  chan error
	first []byte
	err   error
}

func newTarCheck(base string) *tarCheck {
	pr, pw := io.Pipe()
	c := &tarCheck{base: base, pw: pw, done: make(chan error, 1)}
	go func() {
		err := checkTar(pr, base)
		if err != nil {
			pr.CloseWithError(err)
		} else {
			// What follows the end of the archive is read and dropped.
			_, err = io.Copy(io.Discard, pr)
		}
		c.done <- err
	}()
	return c
}

func (c *tarCheck) Write(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if len(c.first) < tarMagicAt+5 {
		c.first = append(c.first, p[:min(len(p), tarMagicAt+5-len(c.first))]...)
		if len(c.first) == tarMagicAt+5 && !bytes.Equal(c.first[tarMagicAt:], []byte("ustar")) {
			c.err = errors.New("not a ustar archive")
			return 0, c.err
		}
	}
	if _, err := c.pw.Write(p); err != nil {
		c.err = err
		return 0, err
	}
	return len(p), nil
}

// Close ends the stream and says whether all of it was a plain tar of
// base.
func (c *tarCheck) Close() error {
	_ = c.pw.Close()
	err := <-c.done
	switch {
	case c.err != nil:
		return c.err
	case len(c.first) < tarMagicAt+5:
		return errors.New("too short for a tar")
	}
	return err
}

// checkTar reads r as a tar of base to its end: regular files,
// directories, symbolic links, and hard links to what it holds.
func checkTar(r io.Reader, base string) error {
	tr := tar.NewReader(r)
	links := map[string]bool{}
	for n := 0; ; n++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) && n > 0 {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := tarName(hdr.Name, base, links)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeReg, '\x00', tar.TypeDir:
		case tar.TypeSymlink:
			links[name] = true
		case tar.TypeLink:
			if _, err := tarName(hdr.Linkname, base, links); err != nil {
				return fmt.Errorf("a hard link to %w", err)
			}
		default:
			return fmt.Errorf("an entry of type %q", hdr.Typeflag)
		}
		if _, err := io.Copy(io.Discard, tr); err != nil {
			return err
		}
	}
}

// tarName is an entry's name, cleaned, if it is base or under it and not
// under a link the tar made before.
func tarName(name, base string, links map[string]bool) (string, error) {
	clean := path.Clean(name)
	if strings.HasPrefix(name, "/") || (clean != base && !strings.HasPrefix(clean, base+"/")) {
		return "", errors.New("an entry outside the path")
	}
	for p := path.Dir(clean); p != "." && p != "/"; p = path.Dir(p) {
		if links[p] {
			return "", errors.New("an entry through a link")
		}
	}
	return clean, nil
}
