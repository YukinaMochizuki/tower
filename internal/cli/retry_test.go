package cli_test

import (
	"testing"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/cli"
	"github.com/YukinaMochizuki/tower/internal/client"
)

// TestRetry: retry names a FAILED object retried, and one that is not
// FAILED left as it is; one that is not there exits ExitNotFound.
func TestRetry(t *testing.T) {
	w := newWorld(t)
	w.asAdmin()
	w.must(manifests, "apply", "-f", "-", "-p", "web")
	sys := w.env.AsSystem()
	golden := func(name string, phase envv1alpha1.GoldenPhase) {
		t.Helper()
		if _, err := sys.Apply(t.Context(), registry.Golden, &envv1alpha1.Golden{
			Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
			Spec: &envv1alpha1.GoldenSpec{Target: "main", InputHash: name + "-hash", Inputs: &envv1alpha1.GoldenSpec_BoxImage{
				BoxImage: &envv1alpha1.BoxImageInputs{BaseImageDigest: "sha256:0"},
			}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := sys.UpdateStatus(t.Context(), registry.Golden, client.Key{Project: "web", Name: name}, &envv1alpha1.GoldenStatus{Phase: phase}, nil); err != nil {
			t.Fatal(err)
		}
	}
	golden("broken", envv1alpha1.GoldenPhase_GOLDEN_PHASE_FAILED)
	golden("fine", envv1alpha1.GoldenPhase_GOLDEN_PHASE_READY)

	if out := w.must("", "retry", "golden", "broken", "-p", "web"); out != "golden/broken retried: its failed work runs now\n" {
		t.Errorf("retrying a FAILED golden said %q", out)
	}
	if out := w.must("", "retry", "golden", "fine", "-p", "web"); out != "golden/fine is READY, not FAILED: nothing to retry\n" {
		t.Errorf("retrying a READY golden said %q", out)
	}
	if r := w.run("", "retry", "box", "absent", "-p", "web"); r.code != cli.ExitNotFound {
		t.Errorf("retrying an absent box: %+v", r)
	}
	if r := w.run("", "retry", "machine", "absent"); r.code != cli.ExitUsage {
		t.Errorf("retrying without a project: %+v", r)
	}
	if r := w.run("", "retry", "golden"); r.code != cli.ExitUsage {
		t.Errorf("retrying without a name: %+v", r)
	}
}
