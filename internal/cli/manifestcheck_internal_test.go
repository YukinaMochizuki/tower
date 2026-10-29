package cli

import (
	"strings"
	"testing"
)

const boxHead = "apiVersion: environments.tower.hangarlab.cloud/v1alpha1\nkind: Box\nmetadata: {name: web}\n"

// TestManifestErrorsSayWhatIsRight checks that a mistake in a manifest is
// reported at its line and field, with what the field would take.
func TestManifestErrorsSayWhatIsRight(t *testing.T) {
	for _, tt := range []struct {
		name string
		doc  string
		want []string
	}{
		{
			"unknown field",
			boxHead + "spec:\n  workstream: w\n  resources:\n    cpus: 2\n",
			[]string{"document 1, line 7: spec.resources.cpus: unknown field", "did you mean cpu?", "spec.resources has cpu, memory, disk"},
		},
		{
			"unknown top-level field",
			boxHead + "specs: {workstream: w}\n",
			[]string{"line 4: specs: unknown field; did you mean spec?", "the object has "},
		},
		{
			"enum value by its last word",
			boxHead + "spec: {workstream: w, runState: running}\n",
			[]string{"line 4: spec.runState: \"running\" is not a RunState", "want one of RUN_STATE_RUNNING, RUN_STATE_STOPPED", "did you mean RUN_STATE_RUNNING?"},
		},
		{
			"field in a list item",
			boxHead + "spec:\n  workstream: w\n  checkouts:\n  - repository: api\n    reff: main\n",
			[]string{"line 8: spec.checkouts[0].reff: unknown field; did you mean ref?"},
		},
		{
			"word for a bool",
			boxHead + "spec:\n  workstream: w\n  keep: maybe\n",
			[]string{"line 6: spec.keep: \"maybe\" is not true or false"},
		},
		{
			"object for a list",
			boxHead + "spec:\n  workstream: w\n  checkouts: {repository: api}\n",
			[]string{"line 6: spec.checkouts: want a list"},
		},
		{
			"value in a map",
			"apiVersion: environments.tower.hangarlab.cloud/v1alpha1\nkind: Box\nmetadata:\n  name: web\n  labels:\n    app: [a]\n",
			[]string{"line 6: metadata.labels[app]: want a single value"},
		},
		{
			"line in a later document",
			boxHead + "spec: {workstream: w}\n---\n" + boxHead + "spec: {workstream: w, colour: red}\n",
			[]string{"document 2, line 9: spec.colour: unknown field"},
		},
		{
			"wrong apiVersion",
			"apiVersion: environments.tower.hangarlab.cloud/v1\nkind: Box\n",
			[]string{"kind Box is in apiVersion environments.tower.hangarlab.cloud/v1alpha1, not \"environments.tower.hangarlab.cloud/v1\""},
		},
		{
			"kind in the wrong case",
			"apiVersion: environments.tower.hangarlab.cloud/v1alpha1\nkind: box\n",
			[]string{"unknown kind \"box\"; did you mean Box?", "environments.tower.hangarlab.cloud/v1alpha1: Boost, Box"},
		},
		{
			"no kind",
			"apiVersion: environments.tower.hangarlab.cloud/v1alpha1\nmetadata: {name: x}\n",
			[]string{"apiVersion and kind are required", "targets.tower.hangarlab.cloud/v1alpha1: Placement, Target"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeManifests("f.yaml", strings.NewReader(tt.doc))
			if err == nil {
				t.Fatal("decoded")
			}
			if code := exitCode(err); code != ExitUsage {
				t.Errorf("exit %d, want %d", code, ExitUsage)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not say %q", err, w)
				}
			}
		})
	}
}

// TestNumbersAreTheirText: a number written where the API has a string is
// sent as written.
func TestNumbersAreTheirText(t *testing.T) {
	ms, err := decodeManifests("f.yaml", strings.NewReader(boxHead+"spec:\n  workstream: w\n  resources: {cpu: 2, memory: 1.10}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := ms[0].obj.(interface{ String() string }).String(); !strings.Contains(got, `cpu:"2"`) || !strings.Contains(got, `memory:"1.10"`) {
		t.Errorf("decoded %s", got)
	}
}
