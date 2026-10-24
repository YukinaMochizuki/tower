// Package controllertest lets a test that runs controllers for real (the
// apiserver's, a node agent's) wait until they have finished reacting to
// everything written so far. It cannot see what reaches a controller outside
// the store, such as a Trigger from the host: a test waits for the visible
// effect of those first. See docs/decisions/testing-strategy.md.
package controllertest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/controller"
	"github.com/YukinaMochizuki/hangar/internal/testutil"
)

// Waiting is a key a controller checks again because it waits on something
// outside it, such as a VM booting: Reconcile returned controller.ErrWaiting.
type Waiting struct {
	Controller string
	Key        controller.Key
}

// Revision reads the store's current revision.
type Revision func(context.Context) (int64, error)

// Statuses reads what a process's controllers are doing, such as
// apiserver.Server.ControllerStatus.
type Statuses func() []controller.Status

// WaitSettled waits until the controllers have settled, and returns the
// keys still waiting on something outside them. Settled is two polls in a
// row, each reading the same store revision before and after the statuses,
// that find, for every controller, an empty queue, nothing in flight, no
// retry of a failure pending, and every change through that revision
// followed. It fails the test if they have not settled within
// testutil.Wait.
func WaitSettled(t testing.TB, revision Revision, statuses ...Statuses) []Waiting {
	t.Helper()
	deadline := time.Now().Add(testutil.Wait)
	var last int64 = -1
	var why string
	for pause := time.Millisecond; ; {
		waiting, rev, not := poll(t, revision, statuses)
		switch {
		case not != "":
			last, why = -1, fmt.Sprintf("at revision %d: %s", rev, not)
			pause = min(2*pause, 20*time.Millisecond)
		case rev == last:
			return waiting
		default:
			// The second poll follows at once: other writers, such as a
			// node's heartbeats, leave short quiet spells.
			last, why = rev, fmt.Sprintf("settled once at revision %d, never twice", rev)
			pause = time.Millisecond
		}
		if time.Now().After(deadline) {
			t.Fatalf("the controllers did not settle within %v; last seen %s", testutil.Wait, why)
		}
		select {
		case <-t.Context().Done():
			t.Fatal("test ended while waiting for the controllers to settle")
		case <-time.After(pause):
		}
	}
}

// poll reads the revision, every status, and the revision again, and says
// why they are not settled at it, or returns "" and the keys waiting.
func poll(t testing.TB, revision Revision, statuses []Statuses) ([]Waiting, int64, string) {
	t.Helper()
	read := func() int64 {
		rev, err := revision(t.Context())
		if err != nil {
			t.Fatalf("reading the store revision: %v", err)
		}
		return rev
	}
	rev := read()
	var st []controller.Status
	for _, s := range statuses {
		st = append(st, s()...)
	}
	if after := read(); after != rev {
		return nil, after, fmt.Sprintf("written while read, at %d", rev)
	}
	waiting, not := check(rev, st)
	return waiting, rev, not
}

// check says why the controllers have not settled at rev, or returns ""
// and the keys waiting.
func check(rev int64, statuses []controller.Status) ([]Waiting, string) {
	var waiting []Waiting
	seen := map[Waiting]bool{}
	var why []string
	for _, s := range statuses {
		var not []string
		if s.Queued > 0 {
			not = append(not, fmt.Sprintf("%d queued", s.Queued))
		}
		if s.InFlight > 0 {
			not = append(not, fmt.Sprintf("%d in flight", s.InFlight))
		}
		if s.Followed < rev {
			not = append(not, fmt.Sprintf("followed to %d", s.Followed))
		}
		for _, r := range s.Retries {
			w := Waiting{Controller: s.Name, Key: r.Key}
			switch {
			case r.Waiting && !seen[w]:
				seen[w] = true
				waiting = append(waiting, w)
			case !r.Waiting:
				not = append(not, fmt.Sprintf("retrying %s at %s", r.Key, r.Due.Format(time.StampMilli)))
			}
		}
		if len(not) > 0 {
			why = append(why, s.Name+": "+strings.Join(not, ", "))
		}
	}
	if len(why) > 0 {
		return nil, strings.Join(why, "; ")
	}
	return waiting, ""
}
