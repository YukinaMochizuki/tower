package hostd_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver/apiservertest"
	"github.com/YukinaMochizuki/tower/internal/hostd/hostdtest"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// running waits until hostd reports n processes running in the instance.
func runningReported(t *testing.T, w *hostdtest.World, name string, n int32) {
	t.Helper()
	testutil.Eventually(t, "processes_running reported", func() (bool, string) {
		st := w.Instance(t, name).GetStatus()
		return st.GetProcessesRunning() == n, st.String()
	})
}

// TestProcessesAreReported: processes_running counts the instance's
// running processes as they start and end.
func TestProcessesAreReported(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	w.Apply(t, "p", container(""))
	w.WaitPhase(t, "p", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	ref := provider.Instance{Project: apiservertest.Project, Name: "p"}
	id, err := w.Host.StartProcess(t.Context(), provider.ProcessRequest{Instance: ref, Command: []string{"sleep"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Host.StartProcess(t.Context(), provider.ProcessRequest{Instance: ref, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	runningReported(t, w, "p", 1)
	if err := w.Host.SignalProcess(t.Context(), ref, id, provider.SignalTerm); err != nil {
		t.Fatal(err)
	}
	runningReported(t, w, "p", 0)
}

// TestARestartedHostdFindsItsProcesses: a process outlives the hostd that
// started it; another hostd on the host lists it, follows it and ends it.
func TestARestartedHostdFindsItsProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host.json")
	w := hostdtest.New(t, hostdtest.Options{StatePath: path})
	w.Apply(t, "r", container(""))
	w.WaitPhase(t, "r", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	ref := provider.Instance{Project: apiservertest.Project, Name: "r"}
	id, err := w.Host.StartProcess(t.Context(), provider.ProcessRequest{Instance: ref, Command: []string{"sleep"}, Name: "server"})
	if err != nil {
		t.Fatal(err)
	}
	w.Restart(t, hostdtest.Options{StatePath: path})
	list, err := w.Host.ListProcesses(t.Context(), ref)
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].Name != "server" || list[0].Exit != nil {
		t.Fatalf("after a restart, the processes: %+v, %v", list, err)
	}
	runningReported(t, w, "r", 1)
	type attached struct {
		end provider.Attached
		err error
	}
	done := make(chan attached, 1)
	go func() {
		end, err := w.Host.AttachProcess(t.Context(), ref, id, 0, true, func(provider.OutputChunk) error { return nil })
		done <- attached{end, err}
	}()
	if err := w.Host.SignalProcess(t.Context(), ref, id, provider.SignalKill); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-done:
		if a.err != nil || a.end.Exit == nil || a.end.Exit.Signal != provider.SignalKill {
			t.Errorf("following the process to its end: %+v, %v", a.end, a.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a follow did not end with the process")
	}
}

// TestAVMRunsProcesses: a VM's processes run through the VM driver, as a
// container's through the container driver, and are listed and read back.
func TestAVMRunsProcesses(t *testing.T) {
	w := hostdtest.New(t, hostdtest.Options{})
	w.Apply(t, "vm", &envv1alpha1.InstanceSpec{Target: target, Kind: envv1alpha1.InstanceKind_INSTANCE_KIND_VM, Image: "ubuntu-24.04"})
	w.WaitPhase(t, "vm", envv1alpha1.InstancePhase_INSTANCE_PHASE_RUNNING)
	ref := provider.Instance{Project: apiservertest.Project, Name: "vm"}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	id, err := w.Host.StartProcess(ctx, provider.ProcessRequest{Instance: ref, Command: []string{"emit", "out:in-the-vm"}, Name: "job"})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	end, err := w.Host.AttachProcess(ctx, ref, id, 0, true, func(c provider.OutputChunk) error {
		out.Write(c.Data)
		return nil
	})
	if err != nil || out.String() != "in-the-vm\n" || end.Exit == nil || end.Exit.Code != 0 {
		t.Errorf("attached %q, ended %+v, %v", out.String(), end, err)
	}
	list, err := w.Host.ListProcesses(ctx, ref)
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].Name != "job" {
		t.Errorf("listed %+v, %v", list, err)
	}
	// None of it went through the container driver.
	if _, err := w.Fake.Drivers().Containers.ListProcesses(ctx, "tower-"+w.Instance(t, "vm").GetMetadata().GetUid()); err == nil {
		t.Error("the VM's processes are a container's too")
	}
}
