package hangar_test

import (
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	"github.com/YukinaMochizuki/tower/internal/apimachinery/conditions"
	"github.com/YukinaMochizuki/tower/internal/guest"
	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/fake"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

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

// identities are who the container's shims were asked they run as: the
// users they ran as, a root one refused among them.
func identities(f *fake.Fake) []string {
	var out []string
	for _, e := range f.ExecsAs() {
		if e.Op == guestbin.OpIdentity || e.Op == "" {
			out = append(out, e.User)
		}
	}
	slices.Sort(out)
	return out
}

// measured waits for the instance's Measured condition to be status, and
// returns it.
func measured(t *testing.T, get func() []*metav1.Condition, status metav1.ConditionStatus) *metav1.Condition {
	t.Helper()
	var c *metav1.Condition
	testutil.Eventually(t, "its Measured condition "+status.String(), func() (bool, string) {
		c = conditions.Find(get(), hangar.InstanceMeasured)
		return c.GetStatus() == status, c.String()
	})
	return c
}

// TestOnlyAStartRunsAsRoot: a container's starts run tower-guest as root,
// each naming the container's user, as its own shim says it is, to start
// the command as; every other operation, a process's signal, list and log
// among them, runs as that user. Who that user is is read at the first
// start, and once: not for an exec, which pays nothing for it.
func TestOnlyAStartRunsAsRoot(t *testing.T) {
	w, f := newWorld(t, nil, func(f *fake.Fake) { f.ContainerUser(vscode) })
	ref, _ := w.Harness().Start(t, "box", container("ws1"))
	before := len(f.ExecsAs())
	if _, err := w.Target.Exec(t.Context(), provider.ExecRequest{Instance: ref, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if got := identities(f); len(got) != 0 {
		t.Errorf("who runs in the container was read as %q before any start", got)
	}
	if got := runProcess(t, w.Target, ref, "sh", "-c", "echo ran"); strings.TrimSpace(got) != "ran" {
		t.Errorf("the process wrote %q", got)
	}
	runProcess(t, w.Target, ref, "true")

	for _, e := range f.ExecsAs()[before:] {
		switch {
		case e.Op == guestbin.OpIdentity:
		case e.Op == guestbin.OpStart && e.User != "0":
			t.Errorf("a start ran as %q, want root", e.User)
		case e.Op != guestbin.OpStart && e.User != "":
			t.Errorf("%s ran as %q, want the container's user", e.Op, e.User)
		}
		if e.Op == guestbin.OpStart && (e.As == nil || !reflect.DeepEqual(*e.As, vscode)) {
			t.Errorf("a start runs its command as %+v, want the container's user %+v", e.As, vscode)
		}
	}
	if got := identities(f); !slices.Equal(got, []string{"", "0"}) {
		t.Errorf("who runs in the container was read as %q, want once as its user and once as root", got)
	}
	in := func() []*metav1.Condition { return w.Instance(t, "box").GetStatus().GetConditions() }
	measured(t, in, metav1.ConditionStatus_CONDITION_STATUS_TRUE)
}

// TestARootImagesProcessesStartAsRoot: in a container whose user is root
// itself, by uid, gid and groups, a start runs as root and names no user;
// one whose user is uid 0 with groups of its own names it.
func TestARootImagesProcessesStartAsRoot(t *testing.T) {
	for what, tc := range map[string]struct {
		user guest.Identity
		drop bool
	}{
		"root":                  {guest.Identity{Groups: []uint32{0}, Home: "/root"}, false},
		"uid 0 in other groups": {guest.Identity{Groups: []uint32{0, 27}, Home: "/root"}, true},
	} {
		t.Run(what, func(t *testing.T) {
			w, f := newWorld(t, nil, func(f *fake.Fake) { f.ContainerUser(tc.user) })
			ref, _ := w.Harness().Start(t, "box", container("ws1"))
			before := len(f.ExecsAs())
			runProcess(t, w.Target, ref, "true")
			for _, e := range f.ExecsAs()[before:] {
				if e.Op == guestbin.OpStart && (e.User != "0" || (e.As != nil) != tc.drop) {
					t.Errorf("a start ran as %q naming %+v; want root, naming a user %v", e.User, e.As, tc.drop)
				}
			}
		})
	}
}

// TestWithoutRootProcessesRunAsTheUser: in a container whose shim does
// not run as root, or runs as another when asked for root, every process
// operation runs as before, as the user, unmeasured; its Measured
// condition says why; root is tried again after a while, not at each
// start, and the failure is logged once.
func TestWithoutRootProcessesRunAsTheUser(t *testing.T) {
	for what, setup := range map[string]func(*fake.Fake){
		"no root":                (*fake.Fake).NoRoot,
		"a root that is no root": func(f *fake.Fake) { f.RootRunsAs(&vscode) },
	} {
		t.Run(what, func(t *testing.T) {
			var logs syncBuffer
			w, f := newWorld(t, func(c *hangar.Config) {
				c.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			}, func(f *fake.Fake) { f.ContainerUser(vscode); setup(f) })
			ref, _ := w.Harness().Start(t, "box", container("ws1"))
			before := len(f.ExecsAs())
			if got := runProcess(t, w.Target, ref, "sh", "-c", "echo ran"); strings.TrimSpace(got) != "ran" {
				t.Errorf("the process wrote %q", got)
			}
			runProcess(t, w.Target, ref, "true")
			for _, e := range f.ExecsAs()[before:] {
				if e.Op != guestbin.OpIdentity && e.Op != "" && (e.User != "" || e.As != nil) {
					t.Errorf("%s ran as %q naming %+v, want as the user, as before", e.Op, e.User, e.As)
				}
			}
			if got := identities(f); !slices.Equal(got, []string{"", "0"}) {
				t.Errorf("who runs in the container was read as %q over two starts, want once", got)
			}
			in := func() []*metav1.Condition { return w.Instance(t, "box").GetStatus().GetConditions() }
			if c := measured(t, in, metav1.ConditionStatus_CONDITION_STATUS_FALSE); c.GetReason() != "NoRoot" || !strings.Contains(c.GetMessage(), "root") {
				t.Errorf("Measured is %v, want NoRoot saying why", c)
			}

			// Past the retry, a start asks again; the failure is not logged
			// again.
			w.Target.SetRootRetry(time.Millisecond)
			testutil.Eventually(t, "root asked for again", func() (bool, string) {
				runProcess(t, w.Target, ref, "true")
				got := identities(f)
				return len(got) > 2, strings.Join(got, ",")
			})
			if n := strings.Count(logs.String(), "no root shim runs"); n != 1 {
				t.Errorf("the failure was logged %d times, want once:\n%s", n, logs.String())
			}
		})
	}
}
