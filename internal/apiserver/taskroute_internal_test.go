package apiserver

import (
	"strings"
	"testing"
	"time"
)

// TestJudgeRuns: each of the draft's thresholds, at its edge
// (docs/decisions/boost.md, "Routing by task").
func TestJudgeRuns(t *testing.T) {
	t.Parallel()
	j := judge{set: settings{boostResources: defaultBoostResources, boostMax: defaultBoostMax}, box: boostSize{milliCPU: 1000, memory: 1000 << 20}}
	d := func(x time.Duration) *time.Duration { return &x }
	b := func(x int64) *int64 { return &x }
	yes := true
	inBox := ranOn{Kind: ranOnBox, CPU: "1", Memory: "2Gi"}
	onBoost := ranOn{Kind: ranOnBoost, CPU: "8", Memory: "8Gi"}
	light := taskRun{RanOn: inBox, Wall: time.Minute, CPUUsage: d(time.Second), MemoryPeak: b(100 << 20)}
	for _, tc := range []struct {
		name   string
		runs   []taskRun
		boost  bool
		memory uint64
		why    string
	}{
		{"a fifth waiting", []taskRun{{RanOn: inBox, Wall: 30 * time.Second, CPUWait: d(6 * time.Second)}}, true, 8 << 30, "waited for a CPU 20% of its 30s"},
		{"just under a fifth", []taskRun{{RanOn: inBox, Wall: 30 * time.Second, CPUWait: d(6*time.Second - time.Millisecond)}}, false, 0, "came near neither"},
		{"just under 30s", []taskRun{{RanOn: inBox, Wall: 30*time.Second - time.Millisecond, CPUWait: d(30 * time.Second)}}, false, 0, ""},
		{"all CPUs busy", []taskRun{{RanOn: inBox, Wall: 100 * time.Second, CPUUsage: d(90 * time.Second)}}, true, 8 << 30, "kept all the CPUs"},
		{"not quite all busy", []taskRun{{RanOn: inBox, Wall: 100 * time.Second, CPUUsage: d(90*time.Second - time.Millisecond)}}, false, 0, ""},
		{"busy on 2 of 4", []taskRun{{RanOn: ranOn{Kind: ranOnBox, CPU: "4"}, Wall: 100 * time.Second, CPUUsage: d(200 * time.Second)}}, false, 0, ""},
		{"a wait on a boost", []taskRun{{RanOn: onBoost, Wall: time.Minute, CPUWait: d(time.Minute), CPUUsage: d(time.Second), MemoryPeak: b(1)}}, false, 0, "its last run, on a boost, used 0.0 vCPU"},
		// The box of 1000Mi holds its processes to 500Mi (processMemoryMax):
		// 80% of that is 400Mi. A boost holding them to 1.5 times a peak T
		// has min(T+768Mi, 2T) of memory.
		{"at 80% of the limit", []taskRun{{RanOn: inBox, Wall: time.Second, MemoryPeak: b(400 << 20)}}, true, 1200 << 20, "peaked at 400Mi, of the 500Mi this box holds its processes to"},
		{"under 80% of the limit", []taskRun{{RanOn: inBox, Wall: time.Second, MemoryPeak: b(400<<20 - 1)}}, false, 0, ""},
		{"an OOM", []taskRun{{RanOn: inBox, Wall: time.Second, MemoryPeak: b(100 << 20), OOMKilled: &yes}}, true, 300 << 20, "ran out of memory at 100Mi"},
		{"an OOM with no peak", []taskRun{{RanOn: inBox, Wall: time.Second, OOMKilled: &yes}}, true, 2688 << 20, "ran out of memory at 1.2Gi"},
		{"the largest peak", []taskRun{
			{RanOn: inBox, MemoryPeak: b(1900 << 20)}, {RanOn: onBoost, MemoryPeak: b(4 << 30)}, {RanOn: inBox, MemoryPeak: b(1700 << 20)},
		}, true, 6912 << 20, "of its last 3 runs, one on a boost peaked at 4.0Gi"},
		{"past the last 3", []taskRun{{RanOn: inBox, Wall: time.Hour, CPUWait: d(time.Hour)}, light, light, light}, false, 0, "its last 3 runs came near neither"},
		{"within the last 3", []taskRun{light, {RanOn: inBox, Wall: time.Hour, CPUWait: d(time.Hour)}, light, light}, true, 8 << 30, "of its last 3 runs, one"},
		{"on a boost, fitting", []taskRun{{RanOn: onBoost, Wall: time.Minute, CPUUsage: d(time.Minute), MemoryPeak: b(300 << 20)}}, false, 0, "used 1.0 vCPU on average and peaked at 300Mi, which this 1 vCPU box, holding its processes to 500Mi, holds"},
		{"on a boost, wider than the box", []taskRun{{RanOn: onBoost, Wall: time.Minute, CPUUsage: d(time.Minute + time.Millisecond), MemoryPeak: b(300 << 20)}}, true, 8 << 30, "more than this box's 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := j.runs("make test", tc.runs)
			if r.boost != tc.boost || (tc.boost && r.size.memory != shapedMemory(j, tc.memory)) || !strings.Contains(r.why, tc.why) || !strings.HasPrefix(r.why, "make test: ") {
				t.Errorf("runs %+v routed %+v (%s), want boost %v of %d saying %q", tc.runs, r, r.size, tc.boost, tc.memory, tc.why)
			}
		})
	}
}

// shapedMemory is the memory of a boost asked for memory, as the
// target's settings shape it.
func shapedMemory(j judge, memory uint64) uint64 {
	if memory == j.set.boostResources.memory {
		return memory
	}
	size, _ := parseBoostSize("", (&boostSize{memory: memory}).resources().GetMemory(), j.set.boostResources)
	return size.shaped(j.set.boostMax).memory
}

// TestMemoryHolding: the memory found holds the measured processes to
// the limit asked for, and no less memory would.
func TestMemoryHolding(t *testing.T) {
	t.Parallel()
	for _, limit := range []uint64{100 << 20, 767 << 20, 768 << 20, 769 << 20, 1950 << 20, 6 << 30} {
		m := memoryHolding(limit)
		if got := uint64(processMemoryMax(int64(m))); got < limit {
			t.Errorf("memoryHolding(%d) = %d, holding its processes to %d", limit, m, got)
		}
		if got := uint64(processMemoryMax(int64(m - 1))); got >= limit {
			t.Errorf("memoryHolding(%d) = %d, but %d would hold them to %d", limit, m, m-1, got)
		}
	}
}

// TestHeavyTool: the heavy tools, and what is none.
func TestHeavyTool(t *testing.T) {
	t.Parallel()
	for tool, want := range map[string]bool{
		"go test": true, "go build": true, "go vet": true, "bun test": true, "cargo test": true, "cargo build": true, "tsc": true, "vitest": true, "jest": true,
		"vite build": true, "next build": true, "webpack": true, "docker build": true,
		"make": false, "pnpm": false, "just": false, "go fmt": false,
	} {
		if got := heavyTool(taskEntry{Tool: tool}); got != want {
			t.Errorf("heavyTool(%q) = %v, want %v", tool, got, want)
		}
	}
}

// TestCapped: a capped buffer keeps its first bytes and says it was given
// more.
func TestCapped(t *testing.T) {
	t.Parallel()
	c := &capped{max: 4}
	if n, err := c.Write([]byte("abc")); n != 3 || err != nil || c.over {
		t.Fatal(n, err, c.over)
	}
	if n, err := c.Write([]byte("def")); n != 3 || err != nil || !c.over || c.String() != "abcd" {
		t.Fatal(n, err, c.over, c.String())
	}
	if _, _ = c.Write([]byte("g")); c.String() != "abcd" {
		t.Fatal(c.String())
	}
}
