package chvm

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEachVMHasAUserOfItsOwn: each VM's Cloud Hypervisor runs as an id of
// the range no other VM has, the same at each start; a range a host user
// has an id in is refused.
func TestEachVMHasAUserOfItsOwn(t *testing.T) {
	d := &Driver{cfg: Config{StateDir: t.TempDir(), VMUsers: UserRange{First: 300000, Count: 2}}}
	if err := os.MkdirAll(filepath.Join(d.cfg.StateDir, "vms"), 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := d.vmUser("a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.vmUser("b")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := d.vmUser("a"); a == b || again != a || !d.cfg.VMUsers.has(a) || !d.cfg.VMUsers.has(b) {
		t.Errorf("VMs a and b run as %d and %d; a again as %d", a, b, again)
	}
	if _, err := d.vmUser("c"); err == nil {
		t.Error("a third VM was given an id of a range of two")
	}

	etc := t.TempDir()
	for name, content := range map[string]string{"passwd": "root:x:0:0::/root:/bin/sh\nalice:x:300001:300001::/home/alice:/bin/sh\n", "group": "root:x:0:\n"} {
		if err := os.WriteFile(filepath.Join(etc, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := (UserRange{First: 300000, Count: 2}).checkFree(etc); err == nil {
		t.Error("a range a host user has an id in was taken")
	}
	if err := (UserRange{First: 400000, Count: 2}).checkFree(etc); err != nil {
		t.Errorf("a free range was refused: %v", err)
	}
}
