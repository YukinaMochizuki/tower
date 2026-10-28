package hangar_test

import (
	"testing"

	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/proto"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"
	hangarstorage "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"

	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/fake"
)

// TestTheSystemContainersDisksStartSmallAndGrow: the system container's
// Disk and its root start small and grow as they fill, and a check of a
// system container as made applies nothing again, which would restart it.
func TestTheSystemContainersDisksStartSmallAndGrow(t *testing.T) {
	f := fake.New(towerGuest, t.TempDir(), clockwork.NewRealClock())
	f.System = towerSystem
	t.Cleanup(f.Close)
	a := &flakyAPI{Fake: f}
	target := hangar.New(hangar.Config{Target: "t1", API: a, SystemImage: "registry.tower.internal/tower/system@sha256:01"})
	ctx := t.Context()
	if err := target.EnsureSystem(ctx); err != nil {
		t.Fatal(err)
	}
	sys := systemName(f)
	d, err := f.Disks().Get(ctx, sys)
	if err != nil {
		t.Fatal(err)
	}
	wantDisk := &hangarstorage.DiskSpec{Size: hangar.DefaultSystemDisk, Grow: &hangarstorage.DiskGrow{Max: "50Gi", Threshold: "80%", Step: "2x"}}
	if !proto.Equal(d.GetSpec(), wantDisk) {
		t.Errorf("the system disk: %v, want %v", d.GetSpec(), wantDisk)
	}
	ct, err := f.Containers().Get(ctx, sys)
	if err != nil {
		t.Fatal(err)
	}
	r := ct.GetSpec().GetResources()
	wantGrow := &hangarcompute.DiskGrow{Max: "10Gi", Threshold: "80%", Step: "2x"}
	if r.GetDisk() != "2Gi" || !proto.Equal(r.GetDiskGrow(), wantGrow) {
		t.Errorf("the system container's root: disk %q, grow %v; want 2Gi, %v", r.GetDisk(), r.GetDiskGrow(), wantGrow)
	}
	a.mu.Lock()
	a.ctApplies = 0
	a.mu.Unlock()
	if err := target.EnsureSystem(ctx); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	applies := a.ctApplies
	a.mu.Unlock()
	if applies != 0 {
		t.Errorf("the system container was applied %d times again, as it was made", applies)
	}
}
