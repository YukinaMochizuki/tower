package apiserver

import (
	"sync"
	"testing"
)

// TestKeyLocks: one key's holders take turns, other keys do not wait, and
// a lock no one wants is dropped.
func TestKeyLocks(t *testing.T) {
	t.Parallel()
	var k keyLocks
	var wg sync.WaitGroup
	counts := map[string]int{}
	var mu sync.Mutex // guards counts across keys; each key's lock guards its own updates
	for range 50 {
		for _, key := range []string{"a", "b"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				unlock := k.lock(key)
				defer unlock()
				mu.Lock()
				n := counts[key]
				mu.Unlock()
				mu.Lock()
				counts[key] = n + 1
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	if counts["a"] != 50 || counts["b"] != 50 {
		t.Errorf("counts %v, want 50 each", counts)
	}
	// Another key is not held up by a held one.
	unlockA := k.lock("a")
	k.lock("b")()
	unlockA()
	if len(k.locks) != 0 {
		t.Errorf("%d locks left with no one holding them", len(k.locks))
	}
}
