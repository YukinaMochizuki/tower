package guestops

import "time"

// SetReceiverGrace sets receiverGrace for a test, and puts it back once
// the test ends. Tests that set it do not run in parallel.
func SetReceiverGrace(t interface{ Cleanup(func()) }, d time.Duration) {
	was := receiverGrace
	receiverGrace = d
	t.Cleanup(func() { receiverGrace = was })
}
