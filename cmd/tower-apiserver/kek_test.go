package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/secrets"
)

// TestLoadKeyring: the key-encryption keys as the host passes them: a
// missing current key is no keyring, a missing previous one is skipped, a
// previous one without a current one or a file others can read refuses to
// start.
func TestLoadKeyring(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	cur, prev := filepath.Join(dir, "secrets-kek"), filepath.Join(dir, "secrets-kek.previous")
	write := func(path string, k secrets.KEK, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(k.String()), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}

	if k, err := loadKeyring("", nil, log); k != nil || err != nil {
		t.Errorf("no flag: %v %v", k, err)
	}
	if _, err := loadKeyring("", []string{prev}, log); err == nil {
		t.Error("a previous key without --secrets-kek was taken")
	}
	if k, err := loadKeyring(cur, []string{prev}, log); k != nil || err != nil {
		t.Errorf("neither file there: %v %v", k, err)
	}
	write(prev, secrets.NewKEK("old"), 0o600)
	if _, err := loadKeyring(cur, []string{prev}, log); err == nil {
		t.Error("a previous key without the current one was taken")
	}
	write(cur, secrets.NewKEK("new"), 0o600)
	if k, err := loadKeyring(cur, []string{prev}, log); err != nil || k.CurrentID() != "new" {
		t.Errorf("a rotation: %v %v", k, err)
	}
	if err := os.Remove(prev); err != nil {
		t.Fatal(err)
	}
	if k, err := loadKeyring(cur, []string{prev}, log); err != nil || k == nil {
		t.Errorf("the previous key gone: %v %v", k, err)
	}
	write(cur, secrets.NewKEK("new"), 0o644)
	if _, err := loadKeyring(cur, nil, log); err == nil {
		t.Error("a key others can read was taken")
	}
}
