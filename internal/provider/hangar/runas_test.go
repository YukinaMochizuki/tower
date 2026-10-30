package hangar_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/fake"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// processOps are the process operations of tower-guest.
var processOps = []string{guestbin.OpStart, guestbin.OpSignal, guestbin.OpList, guestbin.OpLog}

// runProcess starts a process in ref, lists it, reads its log until it
// ends and signals it, as tower does, and returns its output.
func runProcess(t *testing.T, target provider.Provider, ref provider.Instance, command ...string) string {
	t.Helper()
	id, err := target.StartProcess(t.Context(), provider.ProcessRequest{Instance: ref, Command: command})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	testutil.Eventually(t, "the process ended", func() (bool, string) {
		list, err := target.ListProcesses(t.Context(), ref)
		if err != nil || len(list) == 0 {
			return false, ""
		}
		i := slices.IndexFunc(list, func(p provider.Process) bool { return p.ID == id })
		return i >= 0 && list[i].Exit != nil, ""
	})
	if _, err := target.AttachProcess(t.Context(), ref, id, 0, false, func(c provider.OutputChunk) error {
		out.Write(c.Data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := target.SignalProcess(t.Context(), ref, id, provider.SignalTerm); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// vscode is a devcontainer's user, as its shim says it runs.
var vscode = guest.Identity{UID: 1000, GID: 1000, Groups: []uint32{1000, 999}, Home: "/home/vscode"}

// TestProcessesRunAsRootStartingTheContainersUser: a container's process
// operations run tower-guest as root, each start naming the container's
// user, as its own shim reads it once, to start the command as; every
// other operation runs as that user, as before.
func TestProcessesRunAsRootStartingTheContainersUser(t *testing.T) {
	w, f := newWorld(t, nil, func(f *fake.Fake) { f.ContainerUser(vscode) })
	ref, _ := w.Harness().Start(t, "box", container("ws1"))
	before := len(f.ExecsAs())
	if _, err := w.Target.Exec(t.Context(), provider.ExecRequest{Instance: ref, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if got := runProcess(t, w.Target, ref, "sh", "-c", "echo ran"); strings.TrimSpace(got) != "ran" {
		t.Errorf("the process wrote %q", got)
	}
	runProcess(t, w.Target, ref, "true")

	var identities []string
	for _, e := range f.ExecsAs() {
		if e.Op == guestbin.OpIdentity {
			identities = append(identities, e.User)
		}
	}
	for _, e := range f.ExecsAs()[before:] {
		switch {
		case e.Op == guestbin.OpIdentity:
		case slices.Contains(processOps, e.Op) && e.User != "0":
			t.Errorf("%s ran as %q, want root", e.Op, e.User)
		case !slices.Contains(processOps, e.Op) && e.User != "":
			t.Errorf("%s ran as %q, want the container's user", e.Op, e.User)
		}
		if e.Op == guestbin.OpStart && (e.As == nil || !reflect.DeepEqual(*e.As, vscode)) {
			t.Errorf("a start runs its command as %+v, want the container's user %+v", e.As, vscode)
		}
	}
	// Read once, as the user and as root, for every call after: its
	// reconcile's count among them.
	slices.Sort(identities)
	if !slices.Equal(identities, []string{"", "0"}) {
		t.Errorf("who runs in the container was read as %q, want once as its user and once as root", identities)
	}
}

// TestARootImagesProcessesStartAsRoot: in a container whose user is root
// the process operations run as root, and a start names no user to start
// its command as.
func TestARootImagesProcessesStartAsRoot(t *testing.T) {
	w, f := newWorld(t, nil, func(f *fake.Fake) { f.ContainerUser(guest.Identity{Home: "/root"}) })
	ref, _ := w.Harness().Start(t, "box", container("ws1"))
	before := len(f.ExecsAs())
	runProcess(t, w.Target, ref, "true")
	for _, e := range f.ExecsAs()[before:] {
		if slices.Contains(processOps, e.Op) && (e.User != "0" || e.As != nil) {
			t.Errorf("%s ran as %q starting its command as %+v, want root starting it as root", e.Op, e.User, e.As)
		}
	}
}

// TestWithoutRootProcessesRunAsTheUser: a container whose shim cannot
// run as root runs its processes as before: as its user, unmeasured.
func TestWithoutRootProcessesRunAsTheUser(t *testing.T) {
	w, f := newWorld(t, nil, func(f *fake.Fake) { f.ContainerUser(vscode); f.NoRoot() })
	ref, _ := w.Harness().Start(t, "box", container("ws1"))
	before := len(f.ExecsAs())
	if got := runProcess(t, w.Target, ref, "sh", "-c", "echo ran"); strings.TrimSpace(got) != "ran" {
		t.Errorf("the process wrote %q", got)
	}
	for _, e := range f.ExecsAs()[before:] {
		if slices.Contains(processOps, e.Op) && (e.User != "" || e.As != nil) {
			t.Errorf("%s ran as %q starting its command as %+v, want as the user, as before", e.Op, e.User, e.As)
		}
	}
}
