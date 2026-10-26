package cli_test

import (
	"strings"
	"testing"

	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/cli"
	"github.com/YukinaMochizuki/tower/internal/client"
)

// TestTargetAbandon: abandoning a target not being deleted fails; one
// being deleted goes, and what its teardown last found left is printed,
// or that it never listed the target, which leaves what is there unknown.
func TestTargetAbandon(t *testing.T) {
	w := newWorld(t)
	w.asAdmin()
	w.must(manifests, "apply", "-f", "-", "-p", "web")
	if r := w.run("", "target", "abandon", "main"); r.code != cli.ExitError || !strings.Contains(r.stderr, "not being deleted") {
		t.Errorf("abandoning a target not being deleted: %+v", r)
	}
	// No controller runs here: the target stays, being deleted.
	w.must("", "delete", "target", "main")
	if _, err := w.env.AsSystem().UpdateStatus(t.Context(), registry.Target, client.Key{Name: "main"}, &targetsv1alpha1.TargetStatus{
		Remaining: []string{"golden web/g1"}, RemainingKnown: true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	want := "target/main abandoned: its teardown was not confirmed\n" +
		"its teardown last found left, which may still be on the target:\n" +
		"  golden web/g1\n"
	if out := w.must("", "target", "abandon", "main"); out != want {
		t.Errorf("abandon said %q, want %q", out, want)
	}
	if r := w.run("", "get", "target", "main"); r.code != cli.ExitNotFound {
		t.Errorf("the target abandoned: %+v", r)
	}
	// A teardown that never listed the target, as one refusing it: no
	// controller here writes its status.
	w.must("apiVersion: targets.tower.hangarlab.cloud/v1alpha1\nkind: Target\nmetadata: {name: refusing}\nspec: {hostd: {}}\n", "apply", "-f", "-")
	w.must("", "delete", "target", "refusing")
	want = "target/refusing abandoned: its teardown was not confirmed\n" +
		"its teardown never listed the target: what is left on it is unknown\n"
	if out := w.must("", "target", "abandon", "refusing"); out != want {
		t.Errorf("abandon said %q, want %q", out, want)
	}
	if r := w.run("", "target", "abandon"); r.code != cli.ExitUsage {
		t.Errorf("abandoning without a name: %+v", r)
	}
}
