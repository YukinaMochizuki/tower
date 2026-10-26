package proclog_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/guest/proclog"
)

// What hostd reads of a process's directory is written in the guest: the
// tests write it as a hostile guest would, and hostd reads nothing outside
// it, never blocks, and stops.

const secret = "host-secret-7Qz"

// guest makes root with one process, its log one record, and a file of the
// host's outside root holding secret, whose path it returns.
func guest(t *testing.T) (root, id, hostFile string) {
	t.Helper()
	base := t.TempDir()
	hostFile = filepath.Join(base, "host-file")
	if err := os.WriteFile(hostFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(base, "processes")
	id = proclog.NewID(time.Unix(1, 0))
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := proclog.WriteMeta(dir, proclog.Meta{ID: id, Command: []string{"x"}, LogCap: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	w, err := proclog.NewWriter(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(false, []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if err := proclog.WriteExit(dir, proclog.Exit{Code: 0}); err != nil {
		t.Fatal(err)
	}
	return root, id, hostFile
}

// readAll reads the log, failing the test if it takes over a few seconds.
func readAll(t *testing.T, root, id string, follow bool) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		var out bytes.Buffer
		_, err := proclog.ReadLog(ctx, root, id, 0, follow, func(c proclog.Chunk) error {
			out.Write(c.Data)
			return nil
		})
		done <- result{out.String(), err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("reading the log did not end")
		return "", nil
	}
}

func TestALogIsReadBack(t *testing.T) {
	root, id, _ := guest(t)
	if out, err := readAll(t, root, id, false); err != nil || out != "hello\n" {
		t.Fatalf("read %q, %v", out, err)
	}
}

func TestNoSymlinkLeadsOutOfTheDirectory(t *testing.T) {
	// The host's file holds what the guest's would, so a read through the
	// symlink would give it out.
	for name, content := range map[string]string{
		"meta.json":                `{"id":"x","name":"` + secret + `","command":["x"]}`,
		"log-00000000000000000000": "o\x00\x00\x00\x0f" + secret,
	} {
		t.Run(name, func(t *testing.T) {
			root, id, hostFile := guest(t)
			if err := os.WriteFile(hostFile, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(root, id, name)
			_ = os.Remove(p)
			if err := os.Symlink(hostFile, p); err != nil {
				t.Fatal(err)
			}
			out, err := readAll(t, root, id, false)
			info, _ := proclog.Read(root, id)
			if strings.Contains(out, secret) || strings.Contains(info.Name, secret) {
				t.Errorf("read the host's file through a symlink of %s: %q, %v; %+v", name, out, err, info)
			}
		})
	}
	t.Run("the process's directory", func(t *testing.T) {
		root, id, hostFile := guest(t)
		outside := filepath.Join(filepath.Dir(hostFile), "outside")
		if err := os.Rename(filepath.Join(root, id), outside); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, id)); err != nil {
			t.Fatal(err)
		}
		if _, err := proclog.Read(root, id); !errors.Is(err, proclog.ErrNotFound) {
			t.Errorf("a process's directory that is a symlink read: %v", err)
		}
		if list, err := proclog.List(root); err != nil || len(list) != 0 {
			t.Errorf("listed %v, %v", list, err)
		}
	})
}

func TestAFIFOBlocksNoRead(t *testing.T) {
	for _, name := range []string{"exit.json", "lock", "log-00000000000000000000"} {
		t.Run(name, func(t *testing.T) {
			root, id, _ := guest(t)
			p := filepath.Join(root, id, name)
			_ = os.Remove(p)
			if name != "exit.json" {
				_ = os.Remove(filepath.Join(root, id, "exit.json"))
			}
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Fatal(err)
			}
			_, _ = readAll(t, root, id, false) // ends, whatever it says
		})
	}
}

func TestAFileTooLargeIsNotRead(t *testing.T) {
	root, id, _ := guest(t)
	big := append([]byte(`{"id":"`+id+`","name":"`), bytes.Repeat([]byte("x"), 1<<20)...)
	big = append(big, []byte(`","command":["x"]}`)...)
	if err := os.WriteFile(filepath.Join(root, id, "meta.json"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := proclog.Read(root, id); err == nil {
		t.Error("a meta.json of a MiB was read")
	}
}

func TestOddSegmentsEndTheRead(t *testing.T) {
	t.Run("a segment named otherwise", func(t *testing.T) {
		root, id, _ := guest(t)
		if err := os.WriteFile(filepath.Join(root, id, "log-0"), []byte("o\x00\x00\x00\x02hi"), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := readAll(t, root, id, false); err != nil || out != "hello\n" {
			t.Errorf("read %q, %v", out, err)
		}
	})
	t.Run("a segment always missing", func(t *testing.T) {
		root, id, _ := guest(t)
		if err := os.Symlink("nowhere", filepath.Join(root, id, "log-00000000000000000099")); err != nil {
			t.Fatal(err)
		}
		if _, err := readAll(t, root, id, false); err == nil {
			t.Error("a log whose segment is never there read")
		}
	})
	t.Run("a record longer than any written", func(t *testing.T) {
		root, id, _ := guest(t)
		seg := filepath.Join(root, id, "log-00000000000000000000")
		if err := os.WriteFile(seg, []byte("o\xff\xff\xff\xffrest"), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := readAll(t, root, id, false); err != nil || out != "" {
			t.Errorf("read %q, %v", out, err)
		}
	})
	t.Run("a followed log stops with its context", func(t *testing.T) {
		root, id, _ := guest(t)
		_ = os.Remove(filepath.Join(root, id, "exit.json"))
		// Its lock held, as a supervisor alive holds it.
		lock, err := proclog.Lock(filepath.Join(root, id))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Close() }()
		if _, err := readAll(t, root, id, true); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a followed log ended with %v", err)
		}
	})
}
