package apiserver_test

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/fake"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// raisesLimits reports whether a command run in an instance is the one
// that raises its inotify limits, to the values tower sets.
func raisesLimits(e fake.Exec) bool {
	return e.AsRoot && len(e.Command) == 3 && e.Command[0] == "sh" &&
		strings.Contains(e.Command[2], "max_user_watches 524288") && strings.Contains(e.Command[2], "max_user_instances 1024")
}

// limitRaises are the raises of the inotify limits run in the instance,
// and the index in the fake's commands of the first.
func limitRaises(f *fake.Fake, ref provider.Instance) (int, int) {
	n, first := 0, -1
	for i, e := range f.Execs() {
		if e.Instance == ref && raisesLimits(e) {
			if n++; first < 0 {
				first = i
			}
		}
	}
	return n, first
}

// TestABoxsKernelLimitsAreRaisedAtEachStartOnHangar: a box on a hangar
// target has its inotify limits raised as root once its instance runs,
// before its checkout, and again after an idle stop and a wake; its
// KernelLimits condition says so.
func TestABoxsKernelLimitsAreRaisedAtEachStartOnHangar(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{hangar: true, settings: &targetsv1alpha1.TargetSettings{IdleStopAfter: durationpb.New(time.Hour)}})
	b := w.readyBox("dev")
	ref := provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}
	n, first := limitRaises(w.fake, ref)
	if n != 1 {
		t.Fatalf("the limits were raised %d times in a box just made, want once", n)
	}
	checkout := slices.IndexFunc(w.fake.Execs(), func(e fake.Exec) bool {
		return e.Instance == ref && len(e.Command) == 3 && strings.Contains(e.Command[2], ".git/info/exclude")
	})
	if checkout < 0 || checkout < first {
		t.Errorf("the limits were raised at command %d, its checkout set up at %d: want them first", first, checkout)
	}
	if c := condition(b.GetStatus().GetConditions(), apiserver.BoxKernelLimits); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("KernelLimits is %v", c)
	}

	w.advance(61 * time.Minute)
	w.waitBox("dev", envv1alpha1.BoxPhase_BOX_PHASE_STOPPED)
	wctx, cancel := context.WithTimeout(t.Context(), testutil.Wait)
	defer cancel()
	if err := w.srv.Wake(wctx, "web", "dev"); err != nil {
		t.Fatal(err)
	}
	if n, _ := limitRaises(w.fake, ref); n != 2 {
		t.Errorf("after a wake the limits were raised %d times in all, want twice", n)
	}
	if c := condition(w.box("dev").GetStatus().GetConditions(), apiserver.BoxKernelLimits); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("KernelLimits after a wake is %v", c)
	}
}

// TestKernelLimitsThatCannotBeRaisedLeaveTheBoxReady: a raise that fails
// leaves the box READY, its KernelLimits condition saying why.
func TestKernelLimitsThatCannotBeRaisedLeaveTheBoxReady(t *testing.T) {
	t.Parallel()
	w := newWorld(t, worldOption{hangar: true})
	w.fake.OnExec(func(req provider.ExecRequest) (int, bool) {
		if !req.AsRoot {
			return 0, false
		}
		if req.Stderr != nil {
			_, _ = io.WriteString(req.Stderr, "sh: can't create /proc/sys/fs/inotify/max_user_watches: Read-only file system\n")
		}
		return 2, true
	})
	b := w.readyBox("dev")
	c := condition(b.GetStatus().GetConditions(), apiserver.BoxKernelLimits)
	if c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_FALSE || c.GetReason() != "NotRaised" ||
		!strings.Contains(c.GetMessage(), "exit 2") || !strings.Contains(c.GetMessage(), "Read-only file system") {
		t.Errorf("KernelLimits is %v; want NotRaised, saying why", c)
	}
	if n, _ := limitRaises(w.fake, provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}); n != 1 {
		t.Errorf("a raise that failed was tried %d times in one run, want once", n)
	}
}

// TestNoKernelLimitsAreRaisedOnHostd: a hostd target's kernel is the
// host's: nothing runs as root in its boxes, and they say no KernelLimits.
func TestNoKernelLimitsAreRaisedOnHostd(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	b := w.readyBox("dev")
	for _, e := range w.fake.Execs() {
		if e.AsRoot {
			t.Errorf("%v ran as root in a box on hostd", e.Command)
		}
	}
	if c := condition(b.GetStatus().GetConditions(), apiserver.BoxKernelLimits); c != nil {
		t.Errorf("a box on hostd says %v", c)
	}
}

// TestABoostsKernelLimitsAreRaised: a boost's instance has its inotify
// limits raised as its box's does.
func TestABoostsKernelLimitsAreRaised(t *testing.T) {
	t.Parallel()
	w := boostWorld(t, nil)
	w.readyBox("dev")
	w.startBoosted(boostOn("dev", "sleep"))
	b := w.waitBoost("dev", envv1alpha1.BoostPhase_BOOST_PHASE_READY)
	ref := provider.Instance{Project: "web", Name: b.GetStatus().GetInstance()}
	if n, _ := limitRaises(w.fake, ref); n != 1 {
		t.Errorf("the limits were raised %d times in the boost, want once", n)
	}
	if c := condition(b.GetStatus().GetConditions(), apiserver.BoostKernelLimits); c.GetStatus() != metav1.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Errorf("the boost's KernelLimits is %v", c)
	}
}
