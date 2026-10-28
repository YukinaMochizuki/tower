package apiserver

import (
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	envv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/environments/v1alpha1"
	metav1 "github.com/YukinaMochizuki/tower/gen/go/tower/meta/v1"
	targetsv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/targets/v1alpha1"
)

// TestWarmStalenessDefaults: unset, a warm golden is rebuilt 20 commits
// behind, 30 minutes after its inputs change, at any time of day.
func TestWarmStalenessDefaults(t *testing.T) {
	t.Parallel()
	set := settingsOf(&targetsv1alpha1.Target{})
	if set.warmMaxCommitsBehind != 20 || set.warmSettle != 30*time.Minute || set.warmOffPeak != nil {
		t.Errorf("defaults: %d commits behind, settle %v, off-peak %v; want 20, 30m, none", set.warmMaxCommitsBehind, set.warmSettle, set.warmOffPeak)
	}
}

// TestAnOffPeakWindowIsWallClockTime: a window is read in its time zone,
// its end excluded, and one ending before it starts spans midnight.
func TestAnOffPeakWindowIsWallClockTime(t *testing.T) {
	t.Parallel()
	at := func(hhmm string) time.Time {
		d, err := time.Parse("15:04", hhmm)
		if err != nil {
			t.Fatal(err)
		}
		return time.Date(2026, 10, 29, d.Hour(), d.Minute(), 0, 0, time.UTC)
	}
	for _, tt := range []struct {
		window *targetsv1alpha1.TimeWindow
		utc    string
		in     bool
	}{
		{&targetsv1alpha1.TimeWindow{Start: "02:00", End: "06:00"}, "01:59", false},
		{&targetsv1alpha1.TimeWindow{Start: "02:00", End: "06:00"}, "02:00", true},
		{&targetsv1alpha1.TimeWindow{Start: "02:00", End: "06:00"}, "06:00", false},
		// Asia/Taipei is UTC+8: 02:00-06:00 there is 18:00-22:00 UTC.
		{&targetsv1alpha1.TimeWindow{Start: "02:00", End: "06:00", TimeZone: "Asia/Taipei"}, "03:00", false},
		{&targetsv1alpha1.TimeWindow{Start: "02:00", End: "06:00", TimeZone: "Asia/Taipei"}, "19:30", true},
		{&targetsv1alpha1.TimeWindow{Start: "22:00", End: "03:00"}, "23:00", true},
		{&targetsv1alpha1.TimeWindow{Start: "22:00", End: "03:00"}, "01:00", true},
		{&targetsv1alpha1.TimeWindow{Start: "22:00", End: "03:00"}, "12:00", false},
	} {
		w, err := parseDailyWindow(tt.window)
		if err != nil {
			t.Fatal(err)
		}
		if got := w.contains(at(tt.utc)); got != tt.in {
			t.Errorf("%v at %s UTC: in %v, want %v", tt.window, tt.utc, got, tt.in)
		}
	}
	var none *dailyWindow
	if !none.contains(at("12:00")) {
		t.Error("no window does not hold all day")
	}
}

// TestWarmRankingHasHysteresis: a holder keeps its rank for its minimum
// hold whatever its score; after it, a challenger takes a place only with
// a score more than WarmChallengeMargin above the holder's.
func TestWarmRankingHasHysteresis(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	candidate := func(name string, seconds int64, heldFor time.Duration) warmCandidate {
		c := warmCandidate{project: "web", repo: &envv1alpha1.Repository{
			Metadata: &metav1.ObjectMeta{Project: "web", Name: name},
			Status:   &envv1alpha1.RepositoryStatus{Use: &envv1alpha1.RepositoryUse{BoxSeconds: seconds}},
		}}
		if heldFor > 0 {
			c.goldens = []*envv1alpha1.Golden{{Metadata: &metav1.ObjectMeta{Name: "warm-" + name, CreateTime: timestamppb.New(now.Add(-heldFor))}}}
		}
		return c
	}
	order := func(cs ...warmCandidate) []string {
		rankWarm(cs, now, DefaultWarmMinHold)
		var names []string
		for _, c := range cs {
			names = append(names, c.name())
		}
		return names
	}
	for _, tt := range []struct {
		name string
		cs   []warmCandidate
		want []string
	}{
		{"within its hold a holder ranks first", []warmCandidate{candidate("new", 10000, 0), candidate("held", 100, time.Hour)}, []string{"held", "new"}},
		{"the longest held first", []warmCandidate{candidate("later", 900, time.Hour), candidate("earlier", 100, 2*time.Hour)}, []string{"earlier", "later"}},
		{"past its hold, a challenger 19% above stays below", []warmCandidate{candidate("challenger", 1190, 0), candidate("holder", 1000, 48*time.Hour)}, []string{"holder", "challenger"}},
		{"past its hold, a challenger 21% above takes its place", []warmCandidate{candidate("holder", 1000, 48*time.Hour), candidate("challenger", 1210, 0)}, []string{"challenger", "holder"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := order(tt.cs...); !slices.Equal(got, tt.want) {
				t.Errorf("ranked %v, want %v", got, tt.want)
			}
		})
	}
}
