//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/secrets"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// machineManifests are a project on a hostd target and a machine on it, at
// the size the test gives.
func machineManifests(cpu, memory, disk string) string {
	return `apiVersion: tenancy.tower.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: lab}
---
apiVersion: targets.tower.hangarlab.cloud/v1alpha1
kind: Target
metadata: {name: main}
spec:
  hostd: {}
---
apiVersion: targets.tower.hangarlab.cloud/v1alpha1
kind: Placement
metadata: {name: lab}
spec: {target: main}
---
apiVersion: environments.tower.hangarlab.cloud/v1alpha1
kind: Machine
metadata: {name: m1}
spec:
  workstream: ws1
  blueprint: {name: bare, content: "- hosts: localhost\n  tasks: []\n", baseImage: ubuntu-24.04}
  resources: {cpu: "` + cpu + `", memory: ` + memory + `, disk: ` + disk + `}
`
}

// TestAMachineIsResizedOnTheHost: a machine's new size goes from its spec,
// through tower's machine controller, to hostd on the host, which restarts
// its VM at it and reports the size it applied, which the machine says.
func TestAMachineIsResizedOnTheHost(t *testing.T) {
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
	admin.must(machineManifests("2", "2Gi", "10Gi"), "apply", "-f", "-", "-p", "lab")
	host := start(t, filepath.Join(bin, "hostd"), "--tower", url, "--target", "main",
		"--token-file", filepath.Join(dir, "hostd.token"), "--ticket-public-key", filepath.Join(dir, "ticket-key.pub"),
		"--state-dir", filepath.Join(dir, "host"), "--direct-listen", "127.0.0.1:0", "--report-every", "500ms")
	host.find(`(serving direct streams)`)

	machine := func() *envv1alpha1.Machine {
		var m envv1alpha1.Machine
		if err := protojson.Unmarshal([]byte(admin.must("", "get", "machine", "m1", "-p", "lab", "-o", "json")), &m); err != nil {
			t.Fatal(err)
		}
		return &m
	}
	// at waits until the machine is READY at size, its Resized True for its
	// generation, and returns it.
	at := func(size string) *envv1alpha1.Machine {
		t.Helper()
		var m *envv1alpha1.Machine
		testutil.EventuallyWithin(t, slow, "machine m1 READY at "+size, func() (bool, string) {
			m = machine()
			st := m.GetStatus()
			r := st.GetResources()
			c := conditions.Find(st.GetConditions(), "Resized")
			return st.GetPhase() == envv1alpha1.MachinePhase_MACHINE_PHASE_READY && r.GetCpu()+"/"+r.GetMemory()+"/"+r.GetDisk() == size &&
				c.GetStatus() == metav1.ConditionStatus_CONDITION_STATUS_TRUE && c.GetObservedGeneration() == m.GetMetadata().GetGeneration(), st.String()
		})
		return m
	}
	first := at("2/2048Mi/10Gi")

	admin.must(machineManifests("4", "4Gi", "20Gi"), "apply", "-f", "-", "-p", "lab")
	resized := at("4/4096Mi/20Gi")
	var in envv1alpha1.Instance
	if err := protojson.Unmarshal([]byte(admin.must("", "get", "instance", resized.GetStatus().GetInstance(), "-p", "lab", "-o", "json")), &in); err != nil {
		t.Fatal(err)
	}
	if r := in.GetStatus().GetResources(); r.GetCpu() != "4" || r.GetMemory() != "4096Mi" || r.GetDisk() != "20Gi" {
		t.Errorf("hostd reports the instance at %v", r)
	}
	if !in.GetStatus().GetStartTime().AsTime().After(first.GetMetadata().GetCreateTime().AsTime()) || in.GetMetadata().GetUid() == "" {
		t.Errorf("the instance resized: %v", in.GetStatus())
	}
	if !strings.Contains(host.logged(), "resizing a VM") {
		t.Error("hostd did not say it resized the VM")
	}
}
