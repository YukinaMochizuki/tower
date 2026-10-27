package hangar_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hangarcompute "github.com/YukinaMochizuki/hangar/gen/go/hangar/compute/v1alpha1"

	"github.com/YukinaMochizuki/tower/internal/guest/guestbin"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/fake"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/hangartest"
	"github.com/YukinaMochizuki/tower/internal/system"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// behindDoor is a Front, and a Connect through it, to wireMux behind a
// front door of l.
func behindDoor(t *testing.T, l hangartest.DoorLimits, o hangar.FrontOptions) (*hangar.Connect, *hangartest.FrontDoor, *hangar.Front) {
	t.Helper()
	door := hangartest.NewFrontDoor(t, wireMux(t), l)
	o.TLS = door.TLS
	front := hangar.NewFront(o)
	t.Cleanup(front.CloseIdleConnections)
	return hangar.NewConnect(door.URL, "p", "tok", front.Client()), door, front
}

// execTrue execs true in the container c names, as the wire runs it.
func execTrue(ctx context.Context, c *hangar.Connect) error {
	_, err := c.Exec(ctx, hangar.ExecRequest{
		Plural: hangar.PluralContainers, Name: "c", Start: &hangarcompute.MachineExecStart{Command: []string{"true"}, User: "0"},
	})
	return err
}

// atOnce runs fn n times at once and returns their errors.
func atOnce(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs[i] = fn(i) })
	}
	wg.Wait()
	return errs
}

// keptPace fails t unless the connections accepted at those times kept
// to a rate and burst: none was opened before the bucket held a token for
// it, but for one of the clock's grain.
func keptPace(t *testing.T, accepted []time.Time, perSecond float64, burst int) {
	t.Helper()
	for i, at := range accepted {
		if allowed := float64(burst) + perSecond*at.Sub(accepted[0]).Seconds() + 1; float64(i+1) > allowed {
			t.Errorf("connection %d was opened %v after the first, past %v a second with a burst of %d", i+1, at.Sub(accepted[0]), perSecond, burst)
			return
		}
	}
}

// TestManyExecsAtOnceKeepToTheFrontDoorsPace: 60 execs at once, each a
// WebSocket and so a connection of its own, all succeed through a front
// door that refuses connections past 10 a second with a burst of 50, as
// hangar-edge does: none is refused, and none tried again.
func TestManyExecsAtOnceKeepToTheFrontDoorsPace(t *testing.T) {
	t.Parallel()
	c, door, front := behindDoor(t, hangartest.EdgeLimits, hangar.FrontOptions{})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for i, err := range atOnce(60, func(int) error { return execTrue(ctx, c) }) {
		if err != nil {
			t.Errorf("exec %d: %v", i, err)
		}
	}
	if n := door.Refused(); n != 0 {
		t.Errorf("the front door refused %d connections, want none", n)
	}
	if st := front.Stats(); st.Refusals != 0 {
		t.Errorf("%d requests refused and tried again, want none", st.Refusals)
	}
	accepted := door.Accepted()
	if len(accepted) != 60 {
		t.Errorf("%d connections for 60 execs, want one each", len(accepted))
	}
	keptPace(t, accepted, hangar.DefaultFrontLimits.ConnRate, hangar.DefaultFrontLimits.ConnBurst)
}

// TestConnectsOfOneFrontShareItsPace: two Connects through one Front,
// as two hangar targets of one endpoint are, keep its pace together.
func TestConnectsOfOneFrontShareItsPace(t *testing.T) {
	t.Parallel()
	c1, door, front := behindDoor(t, hangartest.EdgeLimits, hangar.FrontOptions{})
	c2 := hangar.NewConnect(door.URL, "p", "tok", front.Client())
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for i, err := range atOnce(60, func(i int) error { return execTrue(ctx, []*hangar.Connect{c1, c2}[i%2]) }) {
		if err != nil {
			t.Errorf("exec %d: %v", i, err)
		}
	}
	if n := door.Refused(); n != 0 {
		t.Errorf("the front door refused %d connections, want none", n)
	}
	keptPace(t, door.Accepted(), hangar.DefaultFrontLimits.ConnRate, hangar.DefaultFrontLimits.ConnBurst)
}

// TestManyCallsAtOnceKeepToTheFrontDoorsRequestRate: 120 calls at once
// get no 429 from a front door that answers one past 20 a second with a
// burst of 100.
func TestManyCallsAtOnceKeepToTheFrontDoorsRequestRate(t *testing.T) {
	t.Parallel()
	c, door, front := behindDoor(t, hangartest.EdgeLimits, hangar.FrontOptions{})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for i, err := range atOnce(120, func(int) error { _, err := c.Project(ctx); return err }) {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	if n := door.TooMany(); n != 0 {
		t.Errorf("the front door answered %d requests 429, want none", n)
	}
	if st := front.Stats(); st.Refusals != 0 {
		t.Errorf("%d requests refused and tried again, want none", st.Refusals)
	}
}

// TestARefusedHandshakeIsTriedAgain: a connection the front door closes
// as it accepts it, an EOF in the TLS handshake, is tried again after a
// backoff, and the exec succeeds.
func TestARefusedHandshakeIsTriedAgain(t *testing.T) {
	t.Parallel()
	l := hangartest.EdgeLimits
	l.RefuseFirst = 3
	c, door, front := behindDoor(t, l, hangar.FrontOptions{})
	start := time.Now()
	if err := execTrue(t.Context(), c); err != nil {
		t.Fatalf("an exec whose first connections were refused: %v", err)
	}
	if took, least := time.Since(start), (hangar.DefaultFrontBackoff*(1+2+4))/2; took < least {
		t.Errorf("three refusals tried again within %v, want a backoff of at least %v", took, least)
	}
	if n, accepted := door.Refused(), len(door.Accepted()); n != 3 || accepted != 1 {
		t.Errorf("%d connections refused and %d accepted, want 3 and 1", n, accepted)
	}
	if st := front.Stats(); st.Refusals != 3 {
		t.Errorf("%d refusals counted, want 3", st.Refusals)
	}
}

// TestAFrontDoorThatKeepsRefusingIsUnavailable: tried a bounded number
// of times, a refused exec, or call, is ErrUnavailable, which tower's
// controllers back off from, never a hard failure.
func TestAFrontDoorThatKeepsRefusingIsUnavailable(t *testing.T) {
	t.Parallel()
	l := hangartest.EdgeLimits
	l.RefuseFirst = 1 << 20
	c, door, _ := behindDoor(t, l, hangar.FrontOptions{Backoff: 10 * time.Millisecond})
	err := execTrue(t.Context(), c)
	if !errors.Is(hangar.ProviderError(err), provider.ErrUnavailable) {
		t.Errorf("an exec the front door kept refusing: %v, want ErrUnavailable", err)
	}
	if n := door.Refused(); n != hangar.DefaultFrontAttempts {
		t.Errorf("%d connections for one exec, want %d", n, hangar.DefaultFrontAttempts)
	}
	_, err = c.Project(t.Context())
	if !errors.Is(hangar.ProviderError(err), provider.ErrUnavailable) {
		t.Errorf("a call the front door kept refusing: %v, want ErrUnavailable", err)
	}
	if n := door.Refused(); n != 2*hangar.DefaultFrontAttempts {
		t.Errorf("%d connections for an exec and a call, want %d", n, 2*hangar.DefaultFrontAttempts)
	}
}

// TestA429FromTheFrontDoorIsWaitedOut: the front door's 429 is waited out
// as its Retry-After says, and the call sent again; hangar's own 429, its
// cap on calls to machines, is not.
func TestA429FromTheFrontDoorIsWaitedOut(t *testing.T) {
	t.Parallel()
	l := hangartest.EdgeLimits
	l.TooManyFirst = 1
	c, door, _ := behindDoor(t, l, hangar.FrontOptions{})
	start := time.Now()
	if _, err := c.Project(t.Context()); err != nil {
		t.Fatalf("a call answered 429 once: %v", err)
	}
	if took := time.Since(start); took < time.Second {
		t.Errorf("sent again after %v, before its Retry-After of 1s", took)
	}
	if n, all := door.TooMany(), door.Requests(); n != 1 || all != 2 {
		t.Errorf("%d answered 429 of %d requests, want 1 of 2", n, all)
	}
	_, err := c.Exec(t.Context(), hangar.ExecRequest{Plural: hangar.PluralContainers, Name: "busy", Start: &hangarcompute.MachineExecStart{Command: []string{"true"}}})
	if !errors.Is(hangar.ProviderError(err), provider.ErrUnavailable) {
		t.Errorf("hangar's cap on calls to machines: %v, want ErrUnavailable", err)
	}
	if all := door.Requests(); all != 3 {
		t.Errorf("hangar's own 429 sent %d times, want once", all-2)
	}
}

// TestCallsShareTheirConnections: Connect's calls reuse connections, as
// the front door counts each one: over HTTP/2 64 calls at once share a
// few, of 32 streams each, opened one at a time (two or three; under load
// a round's streams may still count as the next starts, so one more is
// opened for it), where each call would otherwise open one; over
// HTTP/1.1, as a hangar
// reached without its front door speaks, a few calls are in flight at
// once, each on a connection the next one takes up.
func TestCallsShareTheirConnections(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                 string
		plain                bool
		atOnce, rounds, most int
	}{
		{"HTTP/2", false, 64, 2, 8},
		{"HTTP/1.1", true, 16, 5, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := hangartest.EdgeLimits
			l.PlainHTTP = tc.plain
			c, door, _ := behindDoor(t, l, hangar.FrontOptions{})
			for range tc.rounds {
				for i, err := range atOnce(tc.atOnce, func(int) error { _, err := c.Project(t.Context()); return err }) {
					if err != nil {
						t.Fatalf("call %d: %v", i, err)
					}
				}
			}
			if n := len(door.Accepted()); n > tc.most {
				t.Errorf("%d rounds of %d calls opened %d connections, want at most %d", tc.rounds, tc.atOnce, n, tc.most)
			}
		})
	}
}

// overWire is the fake whose machines are reached over hangar's wire,
// through a front door: its exec and port forward cross the network, its
// other calls do not.
type overWire struct {
	*fake.Fake
	c *hangar.Connect
}

func (o overWire) Exec(ctx context.Context, req hangar.ExecRequest) (hangar.ExecExit, error) {
	return o.c.Exec(ctx, req)
}

func (o overWire) PortForward(ctx context.Context, plural, name string, port int32) (net.Conn, error) {
	return o.c.PortForward(ctx, plural, name, port)
}

// TestSixtyRepositoriesFetchAtOnce: 60 repositories placed at once on one
// hangar target, each fetch an exec in its system container, all fetch
// through a front door that refuses connections past 10 a second with a
// burst of 50: none is refused, and none tried again.
func TestSixtyRepositoriesFetchAtOnce(t *testing.T) {
	t.Parallel()
	var door *hangartest.FrontDoor
	var front *hangar.Front
	var f *fake.Fake
	w, _ := newWorld(t, func(c *hangar.Config) {
		c.API = overWire{Fake: f, c: hangar.NewConnect(door.URL, fake.Project, "tok", front.Client())}
	}, func(fk *fake.Fake) {
		f = fk
		door = hangartest.NewFrontDoor(t, f.Wire("tok"), hangartest.EdgeLimits)
		front = hangar.NewFront(hangar.FrontOptions{TLS: door.TLS})
		t.Cleanup(front.CloseIdleConnections)
	})
	h := w.Harness()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	before := len(door.Accepted())
	for i, err := range atOnce(60, func(i int) error {
		head, err := w.Target.Fetch(ctx, provider.FetchRequest{Mirror: fmt.Sprintf("p/r%02d", i), URL: h.Remote, Branch: h.Branch})
		if err == nil && head != h.RemoteHead {
			err = fmt.Errorf("fetched %s, want %s", head, h.RemoteHead)
		}
		return err
	}) {
		if err != nil {
			t.Errorf("repository %d: %v", i, err)
		}
	}
	if n := door.Refused(); n != 0 {
		t.Errorf("the front door refused %d connections, want none", n)
	}
	if st := front.Stats(); st.Refusals != 0 {
		t.Errorf("%d requests refused and tried again, want none", st.Refusals)
	}
	accepted := door.Accepted()
	// The target's own checks and reports may exec meanwhile.
	if n := len(accepted) - before; n < 60 || n > 70 {
		t.Errorf("%d connections for 60 fetches, want about one each", n)
	}
	keptPace(t, accepted, hangar.DefaultFrontLimits.ConnRate, hangar.DefaultFrontLimits.ConnBurst)
}

// gatedSystem holds the system container's fetches, once holding, until
// its gate opens, and counts how many it holds at once.
type gatedSystem struct {
	*fake.Fake
	gate          chan struct{}
	holding       atomic.Bool
	held, mostNow atomic.Int32
}

func (g *gatedSystem) Exec(ctx context.Context, req hangar.ExecRequest) (hangar.ExecExit, error) {
	cmd := req.Start.GetCommand()
	if len(cmd) != 2 || cmd[0] != "/usr/local/bin/tower-system" {
		return g.Fake.Exec(ctx, req)
	}
	// The call's header says what it is; the fake reads it after.
	typ, data, err := guestbin.ReadFrame(req.Stdin)
	if err != nil {
		return hangar.ExecExit{}, err
	}
	var head bytes.Buffer
	_ = guestbin.WriteFrame(&head, typ, data)
	req.Stdin = io.MultiReader(&head, req.Stdin)
	var h system.Header
	if json.Unmarshal(data, &h) == nil && h.Op == system.OpFetch && g.holding.Load() {
		n := g.held.Add(1)
		for {
			most := g.mostNow.Load()
			if n <= most || g.mostNow.CompareAndSwap(most, n) {
				break
			}
		}
		select {
		case <-g.gate:
		case <-ctx.Done():
		}
		defer g.held.Add(-1)
	}
	return g.Fake.Exec(ctx, req)
}

// TestMirrorOperationsTakeTurns: a target runs no more mirror operations
// at once than MirrorOps, those of a controller no more than three of four
// of them, the rest waiting their turn, while a box's operations do not
// wait on them.
func TestMirrorOperationsTakeTurns(t *testing.T) {
	t.Parallel()
	g := &gatedSystem{gate: make(chan struct{})}
	g.holding.Store(true)
	w, _ := newWorld(t, func(c *hangar.Config) {
		c.MirrorOps = 4
		c.API = g
	}, func(f *fake.Fake) { g.Fake = f })
	ref, _ := w.Harness().Start(t, "box", container("n1"))
	h := w.Harness()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	errs := make(chan error, 10)
	for i := range 10 {
		go func() {
			_, err := w.Target.Fetch(ctx, provider.FetchRequest{Mirror: fmt.Sprintf("p/r%d", i), URL: h.Remote, Branch: h.Branch})
			errs <- err
		}()
	}
	testutil.Eventually(t, "three mirror operations running", func() (bool, string) {
		return g.held.Load() == 3, fmt.Sprintf("%d running", g.held.Load())
	})
	// A box's exec is not a mirror operation: it runs while they wait.
	run, cancelRun := context.WithTimeout(ctx, testutil.Wait)
	_, err := w.Target.Exec(run, provider.ExecRequest{Instance: ref, Command: []string{"true"}})
	cancelRun()
	if err != nil {
		t.Errorf("a box's exec while the mirror operations wait: %v", err)
	}
	if most := g.mostNow.Load(); most != 3 {
		t.Errorf("%d mirror operations ran at once, want 3", most)
	}
	close(g.gate)
	for range 10 {
		if err := <-errs; err != nil {
			t.Errorf("a fetch: %v", err)
		}
	}
	if most := g.mostNow.Load(); most > 3 {
		t.Errorf("%d mirror operations ran at once, want at most 3", most)
	}
}

// TestAPushGoesBeforeQueuedFetches: a person's push, interactive, runs at
// once while a controller's 50 fetches hold all the operations they may
// and the rest wait.
func TestAPushGoesBeforeQueuedFetches(t *testing.T) {
	t.Parallel()
	g := &gatedSystem{gate: make(chan struct{})}
	w, _ := newWorld(t, func(c *hangar.Config) { c.API = g }, func(f *fake.Fake) { g.Fake = f })
	h := w.Harness()
	in, _ := h.Start(t, "push", container("n1"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	mirror := in.Project + "/pushed"
	head, err := w.Target.Fetch(ctx, provider.FetchRequest{Mirror: mirror, URL: h.Remote, Branch: h.Branch})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Target.Checkout(ctx, in, mirror, "", "/workspace"); err != nil {
		t.Fatal(err)
	}
	g.holding.Store(true)
	errs := make(chan error, 50)
	for i := range 50 {
		go func() {
			_, err := w.Target.Fetch(ctx, provider.FetchRequest{Mirror: fmt.Sprintf("%s/bg%02d", in.Project, i), URL: h.Remote, Branch: h.Branch})
			errs <- err
		}()
	}
	defer func() {
		close(g.gate)
		for range 50 {
			if err := <-errs; err != nil {
				t.Errorf("a background fetch: %v", err)
			}
		}
	}()
	testutil.Eventually(t, "the background fetches holding their share", func() (bool, string) {
		n := g.held.Load()
		return n >= hangar.DefaultMirrorOps*3/4, fmt.Sprintf("%d running", n)
	})
	push, cancelPush := context.WithTimeout(provider.Interactive(ctx), testutil.Wait)
	defer cancelPush()
	start := time.Now()
	if err := w.Target.Push(push, provider.PushRequest{Instance: in, Dir: "/workspace", Mirror: mirror, URL: h.Remote, Branch: h.Branch, Lease: head}); err != nil {
		t.Errorf("a push while 50 fetches wait: %v (after %v)", err, time.Since(start))
	}
}

// TestAConnectionThatCannotBeOpenedFailsAtOnce: no front door refused a
// connection to a closed port, so an exec, or a call, to it fails at once,
// as unavailable, after one dial, and is not tried again.
func TestAConnectionThatCannotBeOpenedFailsAtOnce(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	front := hangar.NewFront(hangar.FrontOptions{})
	c := hangar.NewConnect("http://"+addr, "p", "tok", front.Client())
	start := time.Now()
	if err := execTrue(t.Context(), c); !errors.Is(hangar.ProviderError(err), provider.ErrUnavailable) {
		t.Errorf("an exec to a closed port: %v, want ErrUnavailable", err)
	}
	if _, err := c.Project(t.Context()); !errors.Is(hangar.ProviderError(err), provider.ErrUnavailable) {
		t.Errorf("a call to a closed port: %v, want ErrUnavailable", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("an exec and a call to a closed port took %v, want them to fail at once", took)
	}
	if st := front.Stats(); st.Dials != 2 || st.Refusals != 0 {
		t.Errorf("%d dials and %d refusals for an exec and a call, want 2 and none", st.Dials, st.Refusals)
	}
}

// TestADialThatTimesOutCostsOneTimeout: a dial to an address that never
// answers costs the dial's own timeout once, not once a try.
func TestADialThatTimesOutCostsOneTimeout(t *testing.T) {
	t.Parallel()
	const timeout = 300 * time.Millisecond
	front := hangar.NewFront(hangar.FrontOptions{})
	front.SetDial(func(ctx context.Context, network, addr string) (net.Conn, error) {
		select {
		case <-time.After(timeout):
			return nil, &net.OpError{Op: "dial", Net: network, Err: os.ErrDeadlineExceeded}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	c := hangar.NewConnect("https://hangar.invalid", "p", "tok", front.Client())
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	if err := execTrue(ctx, c); !errors.Is(hangar.ProviderError(err), provider.ErrUnavailable) {
		t.Errorf("an exec whose dial timed out: %v, want ErrUnavailable", err)
	}
	if _, err := c.Project(ctx); !errors.Is(hangar.ProviderError(err), provider.ErrUnavailable) {
		t.Errorf("a call whose dial timed out: %v, want ErrUnavailable", err)
	}
	if took := time.Since(start); took > 4*timeout {
		t.Errorf("an exec and a call whose dials time out after %v took %v, want one timeout each", timeout, took)
	}
	if st := front.Stats(); st.Dials != 2 {
		t.Errorf("%d dials for an exec and a call, want 2", st.Dials)
	}
}

// TestAnInteractiveCallGoesBeforeQueuedOnes: a person's call, interactive,
// does not wait behind 120 calls of a controller queued for the rate: those
// take three quarters of it at most.
func TestAnInteractiveCallGoesBeforeQueuedOnes(t *testing.T) {
	t.Parallel()
	c, door, _ := behindDoor(t, hangartest.EdgeLimits, hangar.FrontOptions{})
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	for range 120 {
		wg.Go(func() {
			if _, err := c.Project(ctx); err != nil {
				t.Errorf("a background call: %v", err)
			}
		})
	}
	testutil.Eventually(t, "the background calls past their burst", func() (bool, string) {
		return door.Requests() >= 60, fmt.Sprint(door.Requests(), " requests")
	})
	start := time.Now()
	if _, err := c.Project(provider.Interactive(ctx)); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("an interactive call waited %v behind the background's", took)
	}
	if n := door.TooMany(); n != 0 {
		t.Errorf("the front door answered %d requests 429, want none", n)
	}
}

// TestTwoTargetsStayUnderTheFrontDoorsCaps: two Connects through one
// Front, as two targets of an endpoint are, keep fewer connections open,
// and requests in flight, than the front door takes from a client, however
// many WebSockets they hold; the calls keep places of their own.
func TestTwoTargetsStayUnderTheFrontDoorsCaps(t *testing.T) {
	t.Parallel()
	hang := func(ctx context.Context, c *hangar.Connect) error {
		_, err := c.Exec(ctx, hangar.ExecRequest{Plural: hangar.PluralContainers, Name: "c", Start: &hangarcompute.MachineExecStart{Command: []string{"hang"}}})
		return err
	}
	t.Run("connections open", func(t *testing.T) {
		t.Parallel()
		c1, door, front := behindDoor(t, hangartest.EdgeLimits, hangar.FrontOptions{})
		c2 := hangar.NewConnect(door.URL, "p", "tok", front.Client())
		ctx, cancel := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		for i := range 70 {
			wg.Go(func() { _ = hang(ctx, []*hangar.Connect{c1, c2}[i%2]) })
		}
		sockets := hangar.DefaultFrontLimits.MaxConns - 8
		testutil.Eventually(t, "the WebSockets open", func() (bool, string) {
			return door.Open() >= sockets, fmt.Sprint(door.Open(), " open")
		})
		call, cancelCall := context.WithTimeout(ctx, testutil.Wait)
		if _, err := c1.Project(call); err != nil {
			t.Errorf("a call while 70 WebSockets want to be open: %v", err)
		}
		cancelCall()
		cancel()
		wg.Wait()
		if n, most := door.Refused(), door.MostOpen(); n != 0 || most > hangar.DefaultFrontLimits.MaxConns {
			t.Errorf("the front door refused %d connections and saw %d open, want none and at most %d", n, most, hangar.DefaultFrontLimits.MaxConns)
		}
	})
	t.Run("requests in flight", func(t *testing.T) {
		t.Parallel()
		l := hangartest.EdgeLimits
		l.RequestsPerClient = 24
		limits := hangar.FrontLimits{MaxConns: 20, MaxInFlight: 24}
		c1, door, front := behindDoor(t, l, hangar.FrontOptions{Limits: limits})
		c2 := hangar.NewConnect(door.URL, "p", "tok", front.Client())
		slow := hangar.NewConnect(door.URL, "slow", "tok", front.Client())
		ctx, cancel := context.WithCancel(t.Context())
		var wg sync.WaitGroup
		for i := range 20 {
			wg.Go(func() { _ = hang(ctx, []*hangar.Connect{c1, c2}[i%2]) })
		}
		testutil.Eventually(t, "the WebSockets open", func() (bool, string) {
			return door.Open() >= limits.MaxConns-8, fmt.Sprint(door.Open(), " open")
		})
		for i, err := range atOnce(40, func(int) error { _, err := slow.Project(ctx); return err }) {
			if !errors.Is(err, hangar.ErrNotFound) {
				t.Errorf("slow call %d: %v, want hangar's not found", i, err)
			}
		}
		cancel()
		wg.Wait()
		if n, most := door.TooMany(), door.MostInFlight(); n != 0 || most > limits.MaxInFlight {
			t.Errorf("the front door answered %d requests 429 and saw %d in flight, want none and at most %d", n, most, limits.MaxInFlight)
		}
	})
}
