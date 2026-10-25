package testutil

import (
	"fmt"
	"testing"
	"time"
)

// Wait is how long Eventually waits. It is a bound for a test that has gone
// wrong, not a measure of how fast anything should be: a condition that
// holds at all holds long before it.
const Wait = 10 * time.Second

// Cond is a condition a test waits for. The form that also returns a string
// says what it saw, which a failure reports.
type Cond interface {
	func() bool | func() (bool, string)
}

// Eventually waits until cond holds, polling it, and fails the test if it
// has not held within Wait. what names what it waits for.
func Eventually[C Cond](t testing.TB, what string, cond C) {
	t.Helper()
	EventuallyWithin(t, Wait, what, cond)
}

// EventuallyWithin is Eventually with a bound of its own, for a condition
// that waits on something slower than a process, such as a real VM booting.
// Both work in a t.Cleanup too, where t.Context is already done, so neither
// stops early for it: the bound alone ends the wait.
func EventuallyWithin[C Cond](t testing.TB, within time.Duration, what string, cond C) {
	t.Helper()
	check := asCheck(cond)
	deadline := time.Now().Add(within)
	for pause := time.Millisecond; ; pause = min(2*pause, 50*time.Millisecond) {
		ok, last := check()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			if last == "" {
				t.Fatalf("not within %v: %s", within, what)
			}
			t.Fatalf("not within %v: %s; last seen: %s", within, what, last)
		}
		<-time.After(pause)
	}
}

// Unchanged fails the test if version reports anything other than its first
// value at any point during window: for "this did not happen" checks. version
// reads the state of the one object the check is about, such as its
// resourceVersion, never a store-wide revision, which unrelated writes move.
// A window can only show that nothing happened within it, so prefer waiting
// for the background work to finish when the test can tell.
func Unchanged(t testing.TB, what string, window time.Duration, version func() (string, error)) {
	t.Helper()
	first, err := version()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		<-time.After(5 * time.Millisecond)
		now, err := version()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if now != first {
			t.Fatalf("%s changed: %s, then %s", what, first, now)
		}
	}
}

func asCheck[C Cond](cond C) func() (bool, string) {
	switch c := any(cond).(type) {
	case func() bool:
		return func() (bool, string) { return c(), "" }
	case func() (bool, string):
		return c
	}
	panic(fmt.Sprintf("testutil: unexpected condition type %T", cond))
}
