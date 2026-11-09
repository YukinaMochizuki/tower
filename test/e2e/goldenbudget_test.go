//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/secrets"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// TestAGoldenOverBudgetIsEvictedOnTheHost: a machine's golden is kept while
// the machine is made from it, and once nothing references it, a target
// whose golden_budget it is over evicts it, which tower logs, and hostd
// deletes it from the host.
func TestAGoldenOverBudgetIsEvictedOnTheHost(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	binaries(t, bin)
	dir, err := os.MkdirTemp("", "tower-e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	kek := filepath.Join(dir, "secrets-kek")
	if err := os.WriteFile(kek, []byte(secrets.NewKEK("e2e").String()), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "admin.sock")
	tower := start(t, filepath.Join(bin, "tower-apiserver"), "--dev", "--listen", "127.0.0.1:0",
		"--db", filepath.Join(dir, "tower.db"), "--admin-socket", sock, "--secrets-kek", kek)
	url := "http://" + tower.find(`serving the public API" address=(\S+)`)
	tower.find(`(serving the admin socket)`)
	root := cli{t: t, bin: filepath.Join(bin, "tower"), env: []string{"TOWER_SERVER=unix://" + sock}}
	root.must("", "token", "create", "admin", "--role", "admin", "--secret-file", filepath.Join(dir, "admin.token"))
	admin := cli{t: t, bin: root.bin, env: []string{"TOWER_SERVER=" + url, "TOWER_TOKEN_FILE=" + filepath.Join(dir, "admin.token")}}
	admin.must("", "token", "create", "hostd-main", "--role", "hostd", "--target", "main", "--secret-file", filepath.Join(dir, "hostd.token"))
	// A budget under one fake golden: what nothing references goes.
	manifests := strings.Replace(machineManifests("2", "2Gi", "10Gi"), "  hostd: {}\n",
		"  hostd: {}\n  settings: {goldenBudget: 512Ki, goldenBudgetLow: \"0\"}\n", 1)
	admin.must(manifests, "apply", "-f", "-", "-p", "lab")
	host := start(t, filepath.Join(bin, "hostd"), "--tower", url, "--target", "main",
		"--token-file", filepath.Join(dir, "hostd.token"), "--ticket-public-key", filepath.Join(dir, "ticket-key.pub"),
		"--state-dir", filepath.Join(dir, "host"), "--direct-listen", "127.0.0.1:0", "--report-every", "500ms")
	host.find(`(serving direct streams)`)

	var golden string
	testutil.EventuallyWithin(t, slow, "machine m1 READY", func() (bool, string) {
		var m envv1alpha1.Machine
		if err := protojson.Unmarshal([]byte(admin.must("", "get", "machine", "m1", "-p", "lab", "-o", "json")), &m); err != nil {
			t.Fatal(err)
		}
		golden = m.GetStatus().GetGolden()
		return m.GetStatus().GetPhase() == envv1alpha1.MachinePhase_MACHINE_PHASE_READY, m.GetStatus().String()
	})
	if out := admin.must("", "get", "goldens", "-p", "lab"); !strings.Contains(out, golden) {
		t.Fatalf("the golden of a machine over the budget was evicted while the machine is made from it:\n%s", out)
	}
	admin.must("", "delete", "machine", "m1", "-p", "lab")
	testutil.EventuallyWithin(t, slow, "golden "+golden+" evicted", func() (bool, string) {
		out := admin.must("", "get", "goldens", "-p", "lab")
		return !strings.Contains(out, golden), out
	})
	line := regexp.MustCompile(`evicted a golden.*name=` + regexp.QuoteMeta(golden) + ` .*reason="over budget"`)
	if !line.MatchString(tower.logged()) {
		t.Errorf("tower did not log the eviction of %s over budget", golden)
	}
}
