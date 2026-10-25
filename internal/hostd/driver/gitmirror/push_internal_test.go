package gitmirror

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestAPushTakesObjectsAndRefsAlone: of a checkout's .git, a push takes
// its HEAD, refs and objects into the driver's own repository, and nothing
// the guest could have git run on the host: no config, no hooks, nothing
// outside .git, no link.
func TestAPushTakesObjectsAndRefsAlone(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	add := func(name, body string, typ byte) {
		t.Helper()
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: typ}
		if typ == tar.TypeSymlink {
			hdr.Size, hdr.Linkname = 0, "/etc/passwd"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	add(".git/HEAD", "ref: refs/heads/main\n", tar.TypeReg)
	add(".git/refs/heads/main", "abc\n", tar.TypeReg)
	add(".git/packed-refs", "", tar.TypeReg)
	add(".git/objects/pack/pack-1.pack", "PACK", tar.TypeReg)
	add(".git/config", "[core]\n\tfsmonitor = touch /tmp/pwned\n", tar.TypeReg)
	add(".git/hooks/post-checkout", "#!/bin/sh\n", tar.TypeReg)
	add(".git/objects/info/alternates", "", tar.TypeSymlink)
	add("README", "outside .git", tar.TypeReg)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := takeObjects(&b, repo); err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"HEAD", "refs/heads/main", "packed-refs", "objects/pack/pack-1.pack"} {
		if _, err := os.Stat(filepath.Join(repo, kept)); err != nil {
			t.Errorf("%s was not taken: %v", kept, err)
		}
	}
	for _, dropped := range []string{"config", "hooks/post-checkout", "objects/info/alternates", "README"} {
		if _, err := os.Lstat(filepath.Join(repo, dropped)); err == nil {
			t.Errorf("%s was taken", dropped)
		}
	}
}

// TestATokenIsAHeader: a token reaches git as the environment's header
// alone, and an error quoting it is redacted.
func TestATokenIsAHeader(t *testing.T) {
	env := tokenEnv("tok-1234")
	found := false
	for _, e := range env {
		if e == "GIT_CONFIG_KEY_1=http.extraHeader" {
			found = true
		}
	}
	if !found {
		t.Errorf("the token's environment: %q", env)
	}
	err := redact(errBox("fetch https://x-access-token:tok-1234@example/r failed"), "tok-1234")
	if bytes.Contains([]byte(err.Error()), []byte("tok-1234")) {
		t.Errorf("an error still quotes the token: %v", err)
	}
}

type errBox string

func (e errBox) Error() string { return string(e) }
