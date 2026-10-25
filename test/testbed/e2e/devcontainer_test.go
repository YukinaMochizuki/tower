//go:build testbed

package e2e

import (
	"strings"
	"testing"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/testutil"
	"github.com/YukinaMochizuki/tower/test/testbed/internal/testbed"
)

// devcontainerRepos are public repositories whose devcontainers are what
// workstream makes boxes of, each at a commit of its own: one built from
// its Dockerfile beside its devcontainer.json, one of an image with a
// remoteUser and a feature tower does not install, and one of an image
// whose postCreateCommand installs its dependencies.
var devcontainerRepos = []struct{ name, url, branch, commit string }{
	{"cpp", "https://github.com/microsoft/vscode-remote-try-cpp.git", "main", "a673af1023cdf1680a09593e1b445baede839247"},
	{"starter", "https://github.com/devcontainers/feature-starter.git", "main", "67a45f05bb47843fef96f53ab09eab04680f6d95"},
	{"node", "https://github.com/microsoft/vscode-remote-try-node.git", "main", "3b9986fe3c4bcd5a288b92bda320d763a0285954"},
}

// TestBoxesOfDevcontainers: boxes of real repositories are made of their
// devcontainers as workstream makes them, on a real host: a Dockerfile
// built from the mirror, an image's remoteUser owning /workspace with the
// browser add-on layered on, a postCreateCommand run once checked out; a
// repository without a devcontainer is made of the image to fall back to,
// and with none fails its box, saying why.
func TestBoxesOfDevcontainers(t *testing.T) {
	w := newWorld(t)
	m := `apiVersion: tenancy.tower.hangarlab.cloud/v1alpha1
kind: Project
metadata: {name: web}
---
apiVersion: targets.tower.hangarlab.cloud/v1alpha1
kind: Target
metadata: {name: main}
spec:
  hostd: {}
  settings: {warmBudget: 1Gi}
---
apiVersion: targets.tower.hangarlab.cloud/v1alpha1
kind: Placement
metadata: {name: web}
spec: {target: main}
---
apiVersion: environments.tower.hangarlab.cloud/v1alpha1
kind: Repository
metadata: {name: hello}
spec: {url: "` + remote + `", defaultBranch: ` + remoteBranch + `}
---
apiVersion: environments.tower.hangarlab.cloud/v1alpha1
kind: Box
metadata: {name: hello}
spec: {workstream: ws1, checkouts: [{repository: hello}]}
---
apiVersion: environments.tower.hangarlab.cloud/v1alpha1
kind: Box
metadata: {name: fallback}
spec: {workstream: ws1, checkouts: [{repository: hello}], fallbackImage: "` + testbed.BusyBox + `"}
`
	for _, r := range devcontainerRepos {
		addons := ""
		if r.name == "starter" {
			addons = ", addons: [browser]"
		}
		m += `---
apiVersion: environments.tower.hangarlab.cloud/v1alpha1
kind: Repository
metadata: {name: ` + r.name + `}
spec: {url: "` + r.url + `", defaultBranch: ` + r.branch + `}
---
apiVersion: environments.tower.hangarlab.cloud/v1alpha1
kind: Box
metadata: {name: ` + r.name + `}
spec: {workstream: ws1, checkouts: [{repository: ` + r.name + `, ref: ` + r.commit + `}]` + addons + `}
`
	}
	w.admin.must(m, "apply", "-f", "-", "-p", "web")
	w.host = w.runHostd()
	ctx := t.Context()

	// A repository without a devcontainer: its box fails at once.
	testutil.EventuallyWithin(t, slow, "the box of a repository without a devcontainer failed", func() (bool, string) {
		st := w.box("hello").GetStatus()
		return st.GetPhase() == envv1alpha1.BoxPhase_BOX_PHASE_FAILED, st.String()
	})
	if c := conditions.Find(w.box("hello").GetStatus().GetConditions(), "Ready"); c.GetReason() != "DevcontainerUnresolvable" ||
		!strings.Contains(c.GetMessage(), "the repository has no .devcontainer/devcontainer.json or .devcontainer.json at ") || !strings.Contains(c.GetMessage(), "there is no image to fall back to") {
		t.Errorf("the box of a repository without a devcontainer: %v", c)
	}

	run := func(box, script string, want ...string) {
		t.Helper()
		out, code, err := execIn(ctx, w.envs, boxEnv(box), "sh", "-c", script)
		if err != nil || code != 0 {
			t.Errorf("%s: %q exited %d, %v: %s", box, script, code, err, out)
			return
		}
		for _, s := range want {
			if !strings.Contains(out, s) {
				t.Errorf("%s: %q wrote %q, without %q", box, script, out, s)
			}
		}
	}
	for _, r := range devcontainerRepos {
		w.admin.must("", "wait", "box", r.name, "-p", "web", "--for", "condition=Ready", "--timeout", "30m")
		if c := conditions.Find(w.box(r.name).GetStatus().GetConditions(), "SetUp"); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
			t.Errorf("%s set up: %v", r.name, c)
		}
	}
	// Built from its Dockerfile, which COPYs from its context, as root.
	run("cpp", "cmake --version && test -f /workspace/main.cpp && id -un && git -C /workspace rev-parse HEAD",
		"cmake version", "root", devcontainerRepos[0].commit)
	// Its remoteUser runs it and owns /workspace; the browser is layered on;
	// docker-in-docker, a feature, is not installed.
	run("starter", `id -un; stat -c %U /workspace; touch /workspace/written && echo writable; echo "$CHROME_PATH"; chromium --version; command -v dockerd || echo no-dockerd`,
		// Playwright's build of Chromium calls itself Chrome for Testing.
		"node\nnode\nwritable\n/usr/local/bin/chromium\n", "Chrome for Testing", "no-dockerd")
	// Its postCreateCommand installed its dependencies.
	run("node", "test -d /workspace/node_modules && echo installed", "installed")
	// A repository without a devcontainer, of the image to fall back to.
	w.admin.must("", "wait", "box", "fallback", "-p", "web", "--for", "condition=Ready", "--timeout", "30m")
	run("fallback", "test -f /workspace/README && busybox | head -1", "BusyBox")

	w.admin.must("", "delete", "project", "web")
	testutil.EventuallyWithin(t, slow, "the host empty", hostEmpty)
}
