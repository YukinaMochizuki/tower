//go:build testbed

package e2e

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// runAsPlaybook is a blueprint written to be run by a user with
// passwordless sudo, as workstream's are: a become task writes as root,
// the others write into ~, in its golden phase and in its configure
// phase.
const runAsPlaybook = `- hosts: all
  gather_facts: false
  tasks:
    - name: as root, through sudo
      become: true
      ansible.builtin.shell: id -un > /etc/run-as-become
    - name: into the user's home, in the golden
      become: false
      ansible.builtin.shell: id -un > ~/golden
    - name: into the user's home, in each machine
      tags: [configure]
      become: false
      ansible.builtin.shell: id -un > ~/configure
`

// TestAMachineRunsAsItsRunAsUser: a blueprint naming a run_as user
// converges on a Cloud Hypervisor VM as that user, made by tower in the
// golden's builder: its become task runs as root through sudo, its other
// tasks of both phases and its ready check write and read that user's
// home, and root's home holds none of it.
func TestAMachineRunsAsItsRunAsUser(t *testing.T) {
	w := newWorld(t)
	m := &envv1alpha1.Machine{
		ApiVersion: "environments.tower.hangarlab.cloud/v1alpha1", Kind: "Machine",
		Metadata: &metav1.ObjectMeta{Name: "runas"},
		Spec: &envv1alpha1.MachineSpec{
			Workstream: "ws1",
			Blueprint: &envv1alpha1.Blueprint{
				Name: "runas", Content: runAsPlaybook, BaseImage: "ubuntu-24.04",
				RunAs:      &envv1alpha1.RunAs{User: "tbuser"},
				ReadyCheck: &envv1alpha1.ReadyCheck{Command: `test "$(cat ~/configure)" = tbuser`},
			},
			Resources: &envv1alpha1.Resources{Cpu: "2", Memory: "2Gi", Disk: "10Gi"},
		},
	}
	mJSON, err := protojson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	w.admin.must(manifests(t)+"---\n"+string(mJSON)+"\n", "apply", "-f", "-", "-p", "web")
	w.host = w.runHostd()
	w.admin.must("", "wait", "machine", "runas", "-p", "web", "--for", "condition=Ready", "--timeout", "30m")

	// Read as root, through tower's exec, which runs as root.
	const script = `set -e
h=$(getent passwd tbuser | cut -d: -f6)
echo "home=$h"
echo "become=$(cat /etc/run-as-become)"
echo "golden=$(cat "$h/golden") $(stat -c %U "$h/golden")"
echo "configure=$(cat "$h/configure") $(stat -c %U "$h/configure")"
test ! -e /root/golden && test ! -e /root/configure && echo "root-home=clean"
sudo -n -u tbuser sudo -n true && echo "sudo=ok"`
	out, code, err := execIn(t.Context(), w.envs, machineEnv("runas"), "sh", "-c", script)
	if err != nil || code != 0 {
		t.Fatalf("reading the machine: %q, exit %d, %v", out, code, err)
	}
	for _, want := range []string{"home=/home/tbuser", "become=root", "golden=tbuser tbuser", "configure=tbuser tbuser", "root-home=clean", "sudo=ok"} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("the machine says %q, not %q", out, want)
		}
	}

	w.admin.must("", "delete", "project", "web")
	testutil.EventuallyWithin(t, slow, "the host empty", hostEmpty)
}
