package apiserver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// TestUnpackScriptPutsTheCopyInPlace: the script a scanned copy unpacks
// with, run by a real shell on a tar real tar made as tower asks it to,
// puts a file or a directory at the destination, merges a directory into
// one there, refuses a file over a directory, and leaves nothing beside.
func TestUnpackScriptPutsTheCopyInPlace(t *testing.T) {
	for _, tool := range []string{"sh", "tar", "cp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s: %v", tool, err)
		}
	}
	root := t.TempDir()
	at := func(p string) string { return filepath.Join(root, p) }
	write := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(at(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(at(p), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string {
		b, _ := os.ReadFile(at(p))
		return string(b)
	}
	// copyPath copies as copyScanned does, the stream through a file.
	copyPath := func(from, to string) error {
		t.Helper()
		part := at(to) + ".part-test"
		out, err := os.Create(part)
		if err != nil {
			t.Fatal(err)
		}
		tarCmd := exec.Command("tar", "-cf", "-", "-C", filepath.Dir(at(from)), "--", filepath.Base(from))
		tarCmd.Stdout = out
		if err := tarCmd.Run(); err != nil {
			t.Fatal(err)
		}
		_ = out.Close()
		return exec.Command("sh", "-c", unpackScript, "sh", part, at(to), filepath.Base(from)).Run()
	}
	write("src/one.txt", "one")
	write("src/dir/two.txt", "two")
	write("dst/dir/old.txt", "old")
	write("dst/file.txt", "was here")

	if err := copyPath("src/one.txt", "dst/new.txt"); err != nil || read("dst/new.txt") != "one" {
		t.Errorf("a file to a new path: %v, %q", err, read("dst/new.txt"))
	}
	if fi, err := os.Stat(at("dst/new.txt")); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("the file's mode: %v, %v", fi, err)
	}
	if err := copyPath("src/one.txt", "dst/file.txt"); err != nil || read("dst/file.txt") != "one" {
		t.Errorf("a file over a file: %v, %q", err, read("dst/file.txt"))
	}
	if err := copyPath("src/dir", "dst/fresh"); err != nil || read("dst/fresh/two.txt") != "two" {
		t.Errorf("a directory to a new path: %v, %q", err, read("dst/fresh/two.txt"))
	}
	if err := copyPath("src/dir", "dst/dir"); err != nil || read("dst/dir/two.txt") != "two" || read("dst/dir/old.txt") != "old" {
		t.Errorf("a directory into a directory: %v", err)
	}
	if err := copyPath("src/one.txt", "dst/dir"); err == nil {
		t.Error("a file over a directory went")
	}
	// A link at the destination is replaced, whatever it names: never
	// written through.
	write("elsewhere/kept.txt", "kept")
	for _, link := range []string{"dst/link-to-dir", "dst/link-to-file"} {
		target := at("elsewhere")
		if link == "dst/link-to-file" {
			target = at("elsewhere/kept.txt")
		}
		if err := os.Symlink(target, at(link)); err != nil {
			t.Fatal(err)
		}
		if err := copyPath("src/dir", link); err != nil {
			t.Errorf("a directory onto %s: %v", link, err)
		}
		if fi, err := os.Lstat(at(link)); err != nil || !fi.IsDir() || read(link+"/two.txt") != "two" {
			t.Errorf("%s after the copy: %v, %v", link, fi, err)
		}
	}
	if got, _ := filepath.Glob(at("elsewhere/*")); len(got) != 1 || read("elsewhere/kept.txt") != "kept" {
		t.Errorf("a copy wrote through a link: %v, %q", got, read("elsewhere/kept.txt"))
	}
	// A compressed part is not unpacked, though tar would unpack it from a
	// file it reads itself.
	var tarred, zipped bytes.Buffer
	tarCmd := exec.Command("tar", "-cf", "-", "-C", at("src"), "--", "one.txt")
	tarCmd.Stdout = &tarred
	if err := tarCmd.Run(); err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(&zipped)
	_, _ = zw.Write(tarred.Bytes())
	_ = zw.Close()
	if err := os.WriteFile(at("dst/zipped.part-test"), zipped.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("sh", "-c", unpackScript, "sh", at("dst/zipped.part-test"), at("dst/zipped"), "one.txt").Run(); err == nil || read("dst/zipped") != "" {
		t.Errorf("a compressed part was unpacked: %v, %q", err, read("dst/zipped"))
	}
	_ = os.RemoveAll(at("dst/zipped.part-test"))
	_ = os.RemoveAll(at("dst/zipped.part-test.d"))
	left, _ := filepath.Glob(at("dst/*.part*"))
	left2, _ := filepath.Glob(at("dst/*/*.part*"))
	if len(left)+len(left2) > 0 {
		t.Errorf("left beside: %v %v", left, left2)
	}
}

// tarOfEntries is a tar of hdrs, each regular one holding its name.
func tarOfEntries(t *testing.T, hdrs ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range hdrs {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(h.Name))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(h.Name))
		}
	}
	_ = tw.Close()
	return b.Bytes()
}

// TestTheTarCheckPassesAPlainTarOfThePathAlone: a scanned copy unpacks
// only a plain tar of the path asked for: not a compressed one, nor one
// with an entry outside the path, through a link it made, or of a device.
func TestTheTarCheckPassesAPlainTarOfThePathAlone(t *testing.T) {
	reg := func(name string) *tar.Header { return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644} }
	plain := tarOfEntries(t, &tar.Header{Name: "out/", Typeflag: tar.TypeDir, Mode: 0o755}, reg("out/a.txt"),
		&tar.Header{Name: "out/l", Typeflag: tar.TypeSymlink, Linkname: "/etc"}, &tar.Header{Name: "out/h", Typeflag: tar.TypeLink, Linkname: "out/a.txt"})
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(plain)
	_ = zw.Close()
	for _, tt := range []struct {
		name string
		in   []byte
		ok   bool
	}{
		{"plain", plain, true},
		{"plain, padded", append(slices.Clone(plain), make([]byte, 10240)...), true},
		{"gzip", gz.Bytes(), false},
		{"empty", nil, false},
		{"outside the path", tarOfEntries(t, reg("out/a"), reg("other/b")), false},
		{"up and out", tarOfEntries(t, reg("out/../etc/passwd")), false},
		{"absolute", tarOfEntries(t, reg("/out/a")), false},
		{"through a link", tarOfEntries(t, &tar.Header{Name: "out/l", Typeflag: tar.TypeSymlink, Linkname: "/etc"}, reg("out/l/passwd")), false},
		{"a hard link out", tarOfEntries(t, &tar.Header{Name: "out/h", Typeflag: tar.TypeLink, Linkname: "etc/shadow"}), false},
		{"a device", tarOfEntries(t, &tar.Header{Name: "out/dev", Typeflag: tar.TypeChar}), false},
	} {
		for _, n := range []int{1, 100, 1 << 20} {
			c := newTarCheck("out")
			var werr error
			for b := tt.in; len(b) > 0 && werr == nil; b = b[min(n, len(b)):] {
				_, werr = c.Write(b[:min(n, len(b))])
			}
			err := c.Close()
			if got := werr == nil && err == nil; got != tt.ok {
				t.Errorf("%s in writes of %d: write %v, close %v; want ok %v", tt.name, n, werr, err, tt.ok)
			}
		}
	}
}
