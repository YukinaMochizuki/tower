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

// ApplyRefused is what an apply hangar refused with err makes of an
// instance: its reason, and whether it waits.
func ApplyRefused(err error) (reason string, waits bool) {
	o, _ := (&Target{}).applyFailed(err)
	return o.reason, o.wait
}

// VMConnections is how many live connections the provider holds to its
// VMs.
func (t *Target) VMConnections() int {
	t.vms.mu.Lock()
	defer t.vms.mu.Unlock()
	n := 0
	for _, c := range t.vms.conns {
		if !c.ended() {
			n++
		}
	}
	return n
}

// CloseVMConnections closes every connection to the target's VMs, as its
// Run returning does.
func (t *Target) CloseVMConnections() { t.vms.closeAll() }
