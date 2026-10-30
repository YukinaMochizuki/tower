package guestbin

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A boost's file sync (docs/decisions/boost.md): /workspace, but .git at
// any depth and the top /.ws-cache, moves file by file between a box and
// its boost, ignored files too, each way decided from manifests: one
// record a path, compared by type, mode, size and mtime, which a move
// keeps.

// fileRec is one path of a manifest. Ino and CTime are its side's alone:
// what says a file changed on that side though its size and mtime did not.
type fileRec struct {
	Path  string
	Type  byte // 'f' regular file, 'd' directory, 'l' symlink
	Mode  fs.FileMode
	Size  int64
	MTime int64 // nanoseconds
	Link  string
	Ino   uint64
	CTime int64
}

// sameAs says a and b are the same across instances: their type and mode,
// and a file's size and mtime, a link's target. A directory's mtime moves
// with what it holds, and is not compared.
func (a fileRec) sameAs(b fileRec) bool {
	if a.Type != b.Type || a.Mode != b.Mode {
		return false
	}
	switch a.Type {
	case 'f':
		return a.Size == b.Size && a.MTime == b.MTime
	case 'l':
		return a.Link == b.Link
	}
	return true
}

// sameHere is sameAs on one instance: a file or link also kept its inode
// and its ctime, which any write moves, so one written back to its size
// and mtime still differs.
func (a fileRec) sameHere(b fileRec) bool {
	if !a.sameAs(b) {
		return false
	}
	return a.Type == 'd' || (a.Ino == b.Ino && a.CTime == b.CTime)
}

// excluded says p, relative to the checkout, is never synced: .git at any
// depth (a checkout's own git state, a submodule's or a nested
// repository's), the top .ws-cache (each instance's own caches), and a
// file a sync is writing beside its path (tempPrefix).
func excluded(p string) bool {
	if p == ".ws-cache" || strings.HasPrefix(p, ".ws-cache/") {
		return true
	}
	if strings.HasPrefix(path.Base(p), tempPrefix) {
		return true
	}
	return slices.Contains(strings.Split(p, "/"), ".git")
}

// tempPrefix begins the name of a file a sync writes beside its path
// before it renames it on.
const tempPrefix = ".tower-boost-"

// checkPath refuses a path that is no path in the checkout, or one never
// synced.
func checkPath(p string) error {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("%q is no path in the checkout", p)
	}
	if excluded(p) {
		return fmt.Errorf("%q is never synced", p)
	}
	return nil
}

// recOf is info as a record of p.
func recOf(p string, info fs.FileInfo, link string) (fileRec, bool) {
	r := fileRec{Path: p, Mode: info.Mode().Perm(), MTime: info.ModTime().UnixNano()}
	switch {
	case info.Mode().IsRegular():
		r.Type, r.Size = 'f', info.Size()
	case info.IsDir():
		r.Type = 'd'
	case info.Mode()&fs.ModeSymlink != 0:
		r.Type, r.Link, r.Mode, r.MTime = 'l', link, 0, 0
	default:
		// Sockets, pipes and devices are not synced.
		return fileRec{}, false
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		r.Ino, r.CTime = st.Ino, st.Ctim.Nano()
	}
	return r, true
}

// walk is the manifest of dir: every path it holds but those never
// synced, sorted, never following a link.
func walk(dir string) ([]fileRec, error) {
	var out []fileRec
	err := filepath.WalkDir(dir, func(file string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Gone since it was listed.
				return nil
			}
			return err
		}
		if file == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, file)
		if err != nil {
			return err
		}
		p := filepath.ToSlash(rel)
		if excluded(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			sweepTemp(file, d)
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(file); err != nil {
				return err
			}
		}
		if r, ok := recOf(p, info, link); ok {
			out = append(out, r)
		}
		return nil
	})
	slices.SortFunc(out, func(a, b fileRec) int { return strings.Compare(a.Path, b.Path) })
	return out, err
}

// sweepTemp removes a file a sync was writing beside its path that a
// crash left, once its ctime, which a sync's own writes move, is older
// than boostKeepFor.
func sweepTemp(file string, d fs.DirEntry) {
	if !strings.HasPrefix(d.Name(), tempPrefix) || !d.Type().IsRegular() {
		return
	}
	info, err := d.Info()
	if err != nil {
		return
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && time.Since(time.Unix(0, st.Ctim.Nano())) > boostKeepFor {
		_ = os.Remove(file)
	}
}

// lstatRec is p's record now in root, false if it is not there.
func lstatRec(root *os.Root, p string) (fileRec, bool, error) {
	info, err := root.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return fileRec{}, false, nil
	}
	if err != nil {
		return fileRec{}, false, err
	}
	link := ""
	if info.Mode()&fs.ModeSymlink != 0 {
		if link, err = root.Readlink(p); err != nil {
			return fileRec{}, false, err
		}
	}
	r, ok := recOf(p, info, link)
	return r, ok, nil
}

// writeManifest writes recs to file, gzipped, each field NUL-ended.
func writeManifest(file string, recs []fileRec) error {
	tmp := file + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	w := bufio.NewWriter(zw)
	for _, r := range recs {
		for _, field := range []string{r.Path, string(r.Type), strconv.FormatUint(uint64(r.Mode), 8), strconv.FormatInt(r.Size, 10),
			strconv.FormatInt(r.MTime, 10), r.Link, strconv.FormatUint(r.Ino, 10), strconv.FormatInt(r.CTime, 10)} {
			_, _ = w.WriteString(field)
			_ = w.WriteByte(0)
		}
	}
	err = errors.Join(w.Flush(), zw.Close(), f.Close())
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, file)
}

// readManifest reads a manifest writeManifest wrote.
func readManifest(file string) ([]fileRec, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", file, err)
	}
	r := bufio.NewReader(zr)
	var out []fileRec
	for {
		var fields [8]string
		for i := range fields {
			s, err := r.ReadString(0)
			if errors.Is(err, io.EOF) && i == 0 && s == "" {
				return out, nil
			}
			if err != nil {
				return nil, fmt.Errorf("manifest %s: %w", file, err)
			}
			fields[i] = s[:len(s)-1]
		}
		mode, err1 := strconv.ParseUint(fields[2], 8, 32)
		size, err2 := strconv.ParseInt(fields[3], 10, 64)
		mtime, err3 := strconv.ParseInt(fields[4], 10, 64)
		ino, err4 := strconv.ParseUint(fields[6], 10, 64)
		ctime, err5 := strconv.ParseInt(fields[7], 10, 64)
		if err := errors.Join(err1, err2, err3, err4, err5); err != nil || len(fields[1]) != 1 {
			return nil, fmt.Errorf("manifest %s: a record of %q is not one", file, fields[0])
		}
		out = append(out, fileRec{Path: fields[0], Type: fields[1][0], Mode: fs.FileMode(mode), Size: size, MTime: mtime,
			Link: fields[5], Ino: ino, CTime: ctime})
	}
}

// fileChange is one path that differs: A added, M changed, D deleted; rec
// is the path as it is to be, or as it was for D.
type fileChange struct {
	Status byte
	Rec    fileRec
}

// diffRecs is what differs from old to now, in path order: same says two
// records of a path are the same.
func diffRecs(old, now []fileRec, same func(a, b fileRec) bool) []fileChange {
	var out []fileChange
	i, j := 0, 0
	for i < len(old) || j < len(now) {
		switch {
		case j == len(now) || (i < len(old) && old[i].Path < now[j].Path):
			out = append(out, fileChange{'D', old[i]})
			i++
		case i == len(old) || now[j].Path < old[i].Path:
			out = append(out, fileChange{'A', now[j]})
			j++
		default:
			if !same(old[i], now[j]) {
				out = append(out, fileChange{'M', now[j]})
			}
			i++
			j++
		}
	}
	return out
}

func indexRecs(recs []fileRec) map[string]fileRec {
	m := make(map[string]fileRec, len(recs))
	for _, r := range recs {
		m[r.Path] = r
	}
	return m
}

// OverCap is a file not moved, past what one move takes.
type OverCap struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// packed is what writeFiles wrote.
type packed struct {
	files int
	bytes int64
	sent  []fileRec
	over  []OverCap
}

// writeFiles writes a tar of the records' paths in root as they are now,
// in order, each with its mode and mtime (in nanoseconds), a link with
// its target, a directory as one; a file that would take what it holds
// past limit is left out, and said over. A path gone since it was listed
// is left out; one that changed is written as it is, and said so in sent.
func writeFiles(root *os.Root, file string, recs []fileRec, limit int64) (packed, error) {
	var pk packed
	f, err := os.Create(file)
	if err != nil {
		return pk, err
	}
	defer func() { _ = f.Close() }()
	buf := bufio.NewWriterSize(f, 1<<20)
	bw := tar.NewWriter(buf)
	for _, want := range recs {
		r, ok, err := lstatRec(root, want.Path)
		if err != nil {
			return pk, err
		}
		if !ok {
			continue
		}
		hdr := &tar.Header{Name: r.Path, Mode: int64(r.Mode), ModTime: time.Unix(0, r.MTime), Format: tar.FormatPAX}
		switch r.Type {
		case 'd':
			hdr.Typeflag, hdr.Name = tar.TypeDir, r.Path+"/"
		case 'l':
			hdr.Typeflag, hdr.Linkname, hdr.Mode, hdr.ModTime = tar.TypeSymlink, r.Link, 0o777, time.Unix(0, 0)
		case 'f':
			if pk.bytes+r.Size > limit {
				pk.over = append(pk.over, OverCap{Path: r.Path, Bytes: r.Size})
				continue
			}
			hdr.Typeflag, hdr.Size = tar.TypeReg, r.Size
		}
		if r.Type != 'f' {
			if err := bw.WriteHeader(hdr); err != nil {
				return pk, err
			}
			pk.sent = append(pk.sent, r)
			continue
		}
		src, err := root.Open(r.Path)
		if err != nil {
			return pk, err
		}
		// What is written is what the open file holds: its size and mtime
		// from it, so a file rewritten since its record was taken goes as it
		// is now.
		info, err := src.Stat()
		if err == nil && info.Mode().IsRegular() {
			r, _ = recOf(r.Path, info, "")
			hdr.Size, hdr.Mode, hdr.ModTime = r.Size, int64(r.Mode), time.Unix(0, r.MTime)
			if pk.bytes+r.Size > limit {
				_ = src.Close()
				pk.over = append(pk.over, OverCap{Path: r.Path, Bytes: r.Size})
				continue
			}
			if err = bw.WriteHeader(hdr); err == nil {
				var n int64
				n, err = io.CopyN(bw, src, r.Size)
				if err != nil && n < r.Size {
					err = fmt.Errorf("%s shrank while it was sent: %w", r.Path, err)
				}
			}
		} else if err == nil {
			err = fmt.Errorf("%s is no longer a regular file", r.Path)
		}
		_ = src.Close()
		if err != nil {
			return pk, err
		}
		pk.files++
		pk.bytes += r.Size
		pk.sent = append(pk.sent, r)
	}
	if err := errors.Join(bw.Close(), buf.Flush()); err != nil {
		return pk, err
	}
	return pk, f.Close()
}

// writeNUL writes paths to file, each NUL-ended.
func writeNUL(file string, paths []string) error {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte(0)
	}
	return os.WriteFile(file, []byte(b.String()), 0o644)
}

// readNUL reads what writeNUL wrote; a file not there holds nothing.
func readNUL(file string) ([]string, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return splitNUL([]byte(strings.TrimSuffix(string(b), "\x00"))), nil
}

// applyEntry makes one tar entry at its path in root, as it is in the
// tar: a file written whole beside its path and renamed onto it, with
// its mode and mtime; a link with its target, verbatim, wherever it
// points; a directory with its mode. What is at the path, of another
// type, goes first, with what it holds. Nothing is ever written through
// a link: root refuses a path that leads out of it.
func applyEntry(root *os.Root, hdr *tar.Header, body io.Reader) error {
	p := strings.TrimSuffix(hdr.Name, "/")
	if err := checkPath(p); err != nil {
		return err
	}
	mode := fs.FileMode(hdr.Mode).Perm()
	if d := path.Dir(p); d != "." {
		if err := mkdirs(root, d); err != nil {
			return err
		}
	}
	info, err := root.Lstat(p)
	exists := err == nil
	switch hdr.Typeflag {
	case tar.TypeDir:
		if exists && !info.IsDir() {
			if err := root.Remove(p); err != nil {
				return err
			}
			exists = false
		}
		if !exists {
			if err := root.Mkdir(p, mode); err != nil {
				return err
			}
		}
		return root.Chmod(p, mode)
	case tar.TypeSymlink:
		if exists {
			if err := root.RemoveAll(p); err != nil {
				return err
			}
		}
		return root.Symlink(hdr.Linkname, p)
	case tar.TypeReg:
		if exists && info.IsDir() {
			if err := root.RemoveAll(p); err != nil {
				return err
			}
		}
		tmp := path.Join(path.Dir(p), tempPrefix+strconv.FormatInt(time.Now().UnixNano(), 36)+"-"+path.Base(p))
		if len(path.Base(tmp)) > 255 {
			tmp = path.Join(path.Dir(p), tempPrefix+strconv.FormatInt(time.Now().UnixNano(), 36))
		}
		f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = root.Chmod(tmp, mode)
		}
		if err == nil {
			err = root.Chtimes(tmp, hdr.ModTime, hdr.ModTime)
		}
		if err == nil {
			err = root.Rename(tmp, p)
		}
		if err != nil {
			_ = root.Remove(tmp)
		}
		return err
	}
	return fmt.Errorf("%s: a tar entry of type %q is not synced", p, hdr.Typeflag)
}

// mkdirs makes d and its parents in root, a path of another type in its
// way removed: what the tar holds below it says it is a directory.
func mkdirs(root *os.Root, d string) error {
	info, err := root.Lstat(d)
	if err == nil && info.IsDir() {
		return nil
	}
	if err == nil {
		if err := root.Remove(d); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if p := path.Dir(d); p != "." {
		if err := mkdirs(root, p); err != nil {
			return err
		}
	}
	err = root.Mkdir(d, 0o755)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	return err
}

// deepestFirst orders paths so each comes before its parents.
func deepestFirst(paths []string) []string {
	out := slices.Clone(paths)
	slices.SortFunc(out, func(a, b string) int { return strings.Compare(b, a) })
	return out
}
