package hangar

// Locked reports whether anyone holds the target's lock, as a reconcile,
// makeSystem or Teardown does.
func (t *Target) Locked() bool {
	if t.mu.TryLock() {
		t.mu.Unlock()
		return false
	}
	return true
}
