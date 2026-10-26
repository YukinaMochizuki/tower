package tarx

import (
	"archive/tar"
	"errors"
	"io"
	"strings"
)

// Rename is the tar r with its top name from changed to to: a tar Write
// made of a path, extracted under another name.
func Rename(r io.Reader, from, to string) io.Reader {
	if from == to {
		return r
	}
	pr, pw := io.Pipe()
	go func() {
		tr, tw := tar.NewReader(r), tar.NewWriter(pw)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				pw.CloseWithError(tw.Close())
				return
			}
			if err != nil {
				pw.CloseWithError(err)
				return
			}
			if rest, ok := strings.CutPrefix(hdr.Name, from); ok && (rest == "" || rest[0] == '/') {
				hdr.Name = to + rest
			}
			if err := tw.WriteHeader(hdr); err != nil {
				pw.CloseWithError(err)
				return
			}
			if _, err := io.Copy(tw, tr); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
	}()
	return pr
}
