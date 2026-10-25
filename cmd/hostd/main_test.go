package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDirectListenIsRequired: hostd binds its direct streams only where it
// is told, never every interface by default.
func TestDirectListenIsRequired(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("twr_x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := flags{tower: "http://127.0.0.1:1", target: "t1", tokenFile: token}
	err := run(t.Context(), f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err == nil || !strings.Contains(err.Error(), "--direct-listen") {
		t.Errorf("hostd with no --direct-listen: %v, want it refused", err)
	}
}
