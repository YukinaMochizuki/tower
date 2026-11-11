package apiserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/provider"
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
		// The box of 1000Mi holds its processes to 500Mi (processMemoryMax).
		// A peak alone, even at that limit, is the page cache a build
		// fills, not a need: only a run killed for its memory moves the next.
		// A boost holding them to 1.5 times a peak T has min(T+768Mi, 2T).
		{"a peak at the limit", []taskRun{{RanOn: inBox, Wall: time.Second, MemoryPeak: b(500 << 20)}}, false, 0, "came near neither"},
		{"an OOM", []taskRun{{RanOn: inBox, Wall: time.Second, MemoryPeak: b(100 << 20), OOMKilled: &yes}}, true, 300 << 20, "ran out of memory at 100Mi, of the 500Mi this box holds its processes to"},
		{"an OOM with no peak", []taskRun{{RanOn: inBox, Wall: time.Second, OOMKilled: &yes}}, true, 2688 << 20, "ran out of memory at 1.2Gi"},
		{"the largest OOM", []taskRun{
			{RanOn: inBox, MemoryPeak: b(1900 << 20), OOMKilled: &yes}, {RanOn: onBoost, MemoryPeak: b(4 << 30), OOMKilled: &yes}, {RanOn: inBox, MemoryPeak: b(1700 << 20), OOMKilled: &yes},
		}, true, 6912 << 20, "of its last 3 runs, one on a boost ran out of memory at 4.0Gi"},
		{"a larger peak not killed", []taskRun{
			{RanOn: inBox, MemoryPeak: b(100 << 20), OOMKilled: &yes}, {RanOn: onBoost, MemoryPeak: b(4 << 30)},
		}, true, 300 << 20, "of its last 2 runs, one in this box ran out of memory at 100Mi"},
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

// readsAs is a provider whose ReadFile answers as read does; nothing else
// of it is called.
type readsAs struct {
	provider.Provider
	read  func(ctx context.Context, w io.Writer) error
	calls atomic.Int32
	// taken counts the bytes the reader took.
	taken atomic.Int64
}

func (r *readsAs) ReadFile(ctx context.Context, _ provider.Instance, _ string, w io.Writer) error {
	r.calls.Add(1)
	return r.read(ctx, counting{w, &r.taken})
}

// counting counts what w takes.
type counting struct {
	w io.Writer
	n *atomic.Int64
}

func (c counting) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n.Add(int64(n))
	return n, err
}

// TestIsDir: a directory is told by its error alone (syscall.EISDIR),
// never by its words; a file is read no further than its first byte, so
// one that never ends answers at once.
func TestIsDir(t *testing.T) {
	t.Parallel()
	endless := func(ctx context.Context, w io.Writer) error {
		for {
			if _, err := w.Write(make([]byte, 4096)); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	for _, tc := range []struct {
		name string
		read func(context.Context, io.Writer) error
		want bool
	}{
		{"a directory", func(context.Context, io.Writer) error {
			return fmt.Errorf("reading /workspace/web: %w: %w", syscall.EISDIR, provider.ErrFailed)
		}, true},
		{"words alone", func(context.Context, io.Writer) error {
			return errors.New("reading /workspace/web: /workspace/web is a directory")
		}, false},
		{"not there", func(context.Context, io.Writer) error { return fs.ErrNotExist }, false},
		{"an empty file", func(context.Context, io.Writer) error { return nil }, false},
		{"a file that never ends", endless, false},
		// As an older guest's read of a FIFO did: bounded by isDirTimeout.
		{"a read that never answers", func(ctx context.Context, _ io.Writer) error { <-ctx.Done(); return ctx.Err() }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := &readsAs{read: tc.read}
			pk := (&Server{}).packagesIn(t.Context(), &environment{p: p})
			start := time.Now()
			if got := pk.isDir("web"); got != tc.want {
				t.Errorf("isDir = %v, want %v", got, tc.want)
			}
			if took := time.Since(start); took > isDirTimeout+time.Second {
				t.Errorf("isDir took %v", took)
			}
			pk.isDir("web")
			if n := p.calls.Load(); n != 1 {
				t.Errorf("read %d times, want once", n)
			}
			if n := p.taken.Load(); n != 0 {
				t.Errorf("took %d bytes of a file, want none", n)
			}
		})
	}
}

// TestNarrowScope: go's and cargo's over named packages, and what is
// not.
func TestNarrowScope(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		task taskEntry
		want bool
	}{
		{taskEntry{Tool: "go test", Name: "./..."}, false},
		{taskEntry{Tool: "go test", Name: "./internal/a/ ./internal/b/..."}, false},
		{taskEntry{Tool: "go vet", Name: "./internal/a/"}, true},
		{taskEntry{Tool: "go build", Name: "."}, true},
		{taskEntry{Tool: "cargo test"}, false},
		{taskEntry{Tool: "cargo build", Name: "core"}, true},
		{taskEntry{Tool: "vitest"}, false},
		{taskEntry{Tool: "tsc", Name: "x"}, false},
	} {
		if got := narrowScope(tc.task); got != tc.want {
			t.Errorf("narrowScope(%+v) = %v, want %v", tc.task, got, tc.want)
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
