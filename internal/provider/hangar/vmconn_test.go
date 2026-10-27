package hangar_test

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/gen/go/tower/registry"
	"github.com/YukinaMochizuki/tower/internal/client"
	"github.com/YukinaMochizuki/tower/internal/provider"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/fake"
	"github.com/YukinaMochizuki/tower/internal/provider/hangar/hangartest"
	"github.com/YukinaMochizuki/tower/internal/streams"
	"github.com/YukinaMochizuki/tower/internal/testutil"
	"github.com/YukinaMochizuki/tower/internal/ticket"
)

// startVM starts a VM instance and returns it and its VirtualMachine's
// name.
func startVM(t *testing.T, w *hangartest.World, name string) (provider.Instance, string) {
	t.Helper()
	ref, _ := w.Harness().Start(t, name, vmSpec("ws1", name))
	return ref, "t-" + w.Instance(t, name).GetMetadata().GetUid()
}

func run1(t *testing.T, p provider.Provider, ref provider.Instance, command ...string) error {
	t.Helper()
	code, err := p.Exec(t.Context(), provider.ExecRequest{Instance: ref, Command: command})
	if err == nil && code != 0 {
		err = fmt.Errorf("%v exited %d", command, code)
	}
	return err
}

// TestAProviderLeavesNoConnectionToItsVMs: a provider that ends, as tower
// makes one again when its target's secrets change, closes every
// connection it holds to its VMs; so does releasing a VM's instance, and
// tearing the target down, while hangar still holds the VM.
func TestAProviderLeavesNoConnectionToItsVMs(t *testing.T) {
	w, f := newWorld(t, nil)
	ref, name := startVM(t, w, "vm")

	p := hangar.New(w.Config)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	if err := run1(t, p, ref, "true"); err != nil {
		t.Fatal(err)
	}
	if n := f.VMConnections(name); n != 1 {
		t.Fatalf("the VM holds %d connections, want the provider's one", n)
	}
	cancel()
	<-done
	testutil.Eventually(t, "the ended provider's connection closed", func() (bool, string) {
		n := f.VMConnections(name)
		return n == 0, fmt.Sprint(n, " open")
	})

	// Its instance deleted, while hangar holds the VM being deleted.
	if err := run1(t, w.Target, ref, "true"); err != nil {
		t.Fatal(err)
	}
	f.HoldDeletes()
	if _, err := w.Env.AsSystem().Delete(t.Context(), registry.Instance, client.Key{Project: ref.Project, Name: ref.Name}); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a released VM's connection closed", func() (bool, string) {
		n := f.VMConnections(name)
		return n == 0 && w.Target.VMConnections() == 0, fmt.Sprint(n, " open; the provider holds ", w.Target.VMConnections())
	})
	f.ReleaseDeletes()

	// Torn down, while hangar holds the VM being deleted.
	ref2, name2 := startVM(t, w, "vm2")
	if err := run1(t, w.Target, ref2, "true"); err != nil {
		t.Fatal(err)
	}
	f.HoldDeletes()
	if _, err := w.Target.Teardown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := w.Target.VMConnections(); n != 0 {
		t.Errorf("after a teardown pass the provider holds %d connections", n)
	}
	// The VM's sshd hears of the close in its own time.
	testutil.Eventually(t, "a torn down VM's connection closed", func() (bool, string) {
		n := f.VMConnections(name2)
		return n == 0, fmt.Sprint(n, " open")
	})
	f.ReleaseDeletes()
}

// TestASlowVMHoldsUpNoOtherInstance: while an operation waits for a VM's
// sshd, the target's reconciles, of that VM and of others, and its sweeps
// go on: a new instance comes up meanwhile.
func TestASlowVMHoldsUpNoOtherInstance(t *testing.T) {
	w, f := newWorld(t, func(c *hangar.Config) {
		c.ReadyTimeout, c.ReportEvery, c.SweepEvery = time.Minute, 200*time.Millisecond, 100*time.Millisecond
	})
	ref, name := startVM(t, w, "slow")
	if err := run1(t, w.Target, ref, "true"); err != nil {
		t.Fatal(err)
	}
	// Its sshd stops answering, as a guest rebooting: the next operation
	// waits for it, up to ReadyTimeout.
	f.SetBootDelay(time.Hour)
	f.DropVMConnections(name)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiting := make(chan error, 1)
	go func() {
		_, err := w.Target.Exec(ctx, provider.ExecRequest{Instance: ref, Command: []string{"true"}})
		waiting <- err
	}()
	// It is waiting once it has been refused, and asks again.
	before := f.SSHDials(name)
	testutil.Eventually(t, "the operation waiting for the VM's sshd", func() (bool, string) {
		select {
		case err := <-waiting:
			t.Fatalf("the operation did not wait for the VM: %v", err)
		default:
		}
		n := f.SSHDials(name)
		return n >= before+2, fmt.Sprint(n-before, " dials")
	})
	// Reconciles of it and sweeps meanwhile; another instance comes up.
	w.Harness().Start(t, "other", container("ws1"))
	cancel()
	<-waiting
}

// TestManyOperationsInAVMAtOnce: more operations at once than an sshd
// opens sessions on one connection all run, on more connections, none
// refused, none of those running cut short.
func TestManyOperationsInAVMAtOnce(t *testing.T) {
	w, f := newWorld(t, nil)
	ref, name := startVM(t, w, "busy")
	if err := run1(t, w.Target, ref, "true"); err != nil {
		t.Fatal(err)
	}
	if m := f.MaxSessionsOf(name); m <= fake.MaxSessions {
		t.Fatalf("the VM's sshd opens %d sessions on a connection: its user_data set none", m)
	}
	dials := f.SSHDials(name)
	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- run1(t, w.Target, ref, "sleep", "2")
		}()
	}
	testutil.Eventually(t, "the operations all in flight", func() (bool, string) {
		n := f.SessionsOpen(name)
		return n == 40, fmt.Sprint(n, " sessions open")
	})
	if c, p := f.VMConnections(name), w.Target.VMConnections(); c != 1 || p != 1 {
		t.Errorf("%d operations at once: the VM holds %d connections, the provider %d; want one", n, c, p)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("an operation of %d at once: %v", n, err)
		}
	}
	if r := f.SessionsRefused(name); r != 0 {
		t.Errorf("the VM's sshd refused %d sessions: the provider opened more on its connection than it takes", r)
	}
	if d := f.SSHDials(name) - dials; d != 0 {
		t.Errorf("%d operations at once dialed the VM %d times more", n, d)
	}
}

type failingWriter struct{}

var errReaderGone = errors.New("the reader went away")

func (failingWriter) Write([]byte) (int, error) { return 0, errReaderGone }

// TestAFailingWriterEndsItsSessionAlone: an operation whose caller's
// writer fails ends with that error, not as the VM unavailable; another
// operation in flight on the same connection goes on, and the connection
// stays.
func TestAFailingWriterEndsItsSessionAlone(t *testing.T) {
	w, f := newWorld(t, nil)
	ref, name := startVM(t, w, "busy")
	if err := w.Target.WriteFile(t.Context(), ref, "/tmp/small.txt", 0o644, 0, strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "no session open", func() (bool, string) {
		n := f.SessionsOpen(name)
		return n == 0, fmt.Sprint(n, " sessions")
	})
	long := make(chan error, 1)
	go func() { long <- run1(t, w.Target, ref, "sleep", "3") }()
	testutil.Eventually(t, "the sleep in flight", func() (bool, string) {
		n := f.SessionsOpen(name)
		return n == 1, fmt.Sprint(n, " sessions")
	})
	err := w.Target.ReadFile(t.Context(), ref, "/tmp/small.txt", failingWriter{})
	if !errors.Is(err, errReaderGone) || errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("a read into a writer that fails: %v, want its error, not the VM unavailable", err)
	}
	if err := <-long; err != nil {
		t.Errorf("the operation beside it: %v", err)
	}
	if n := w.Target.VMConnections(); n != 1 {
		t.Errorf("the provider holds %d connections, want its one", n)
	}
}

// TestAnEndedProviderHoldsNoConnection: an operation of a provider that
// has ended makes no connection that outlives it.
func TestAnEndedProviderHoldsNoConnection(t *testing.T) {
	w, f := newWorld(t, nil)
	ref, name := startVM(t, w, "vm")
	p := hangar.New(w.Config)
	p.CloseVMConnections()
	if err := run1(t, p, ref, "true"); !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("an operation of an ended provider: %v, want ErrUnavailable", err)
	}
	if n := p.VMConnections(); n != 0 {
		t.Errorf("an ended provider holds %d connections", n)
	}
	testutil.Eventually(t, "its connection closed", func() (bool, string) {
		n := f.VMConnections(name)
		return n == 0, fmt.Sprint(n, " open")
	})
}

// TestAFrozenVMsConnectionIsClosed: a VM whose sshd stops answering, as a
// guest frozen, has its connection closed once a keepalive goes
// unanswered: an operation in flight fails as the VM unavailable, and the
// target's reconciles go on meanwhile.
func TestAFrozenVMsConnectionIsClosed(t *testing.T) {
	testutil.Quarantine(t, "#35")
	w, f := newWorld(t, func(c *hangar.Config) {
		c.KeepAliveEvery, c.KeepAliveTimeout, c.ReportEvery = 100*time.Millisecond, 300*time.Millisecond, 200*time.Millisecond
	})
	ref, name := startVM(t, w, "frozen")
	if err := run1(t, w.Target, ref, "true"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "no session open", func() (bool, string) {
		n := f.SessionsOpen(name)
		return n == 0, fmt.Sprint(n, " sessions")
	})
	inFlight := make(chan error, 1)
	go func() { inFlight <- run1(t, w.Target, ref, "sleep", "30") }()
	testutil.Eventually(t, "the operation in flight", func() (bool, string) {
		n := f.SessionsOpen(name)
		return n == 1, fmt.Sprint(n, " sessions")
	})
	f.Freeze(name)
	defer f.Thaw(name)
	select {
	case err := <-inFlight:
		if !errors.Is(err, provider.ErrUnavailable) {
			t.Errorf("an operation in a frozen VM: %v, want ErrUnavailable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an operation in a frozen VM did not end")
	}
	if n := w.Target.VMConnections(); n != 0 {
		t.Errorf("the provider holds %d connections to a frozen VM", n)
	}
	w.Harness().Start(t, "other", container("ws1"))
}

// TestAVMIsWaitedForAndReconnectedTo: an operation in a VM just started
// waits for its sshd; once its connection drops, the next connects again;
// one in flight as it drops fails at once, and the dropped connection is
// closed.
func TestAVMIsWaitedForAndReconnectedTo(t *testing.T) {
	w, f := newWorld(t, nil)
	f.SetBootDelay(2 * time.Second)
	ref, name := startVM(t, w, "boots")
	if err := run1(t, w.Target, ref, "true"); err != nil {
		t.Fatalf("an operation as the VM boots: %v", err)
	}
	f.SetBootDelay(0)
	f.DropVMConnections(name)
	if err := run1(t, w.Target, ref, "true"); err != nil {
		t.Errorf("an operation after the connection dropped: %v", err)
	}
	// The sessions before it ended, the one open next is its.
	testutil.Eventually(t, "no session open", func() (bool, string) {
		n := f.SessionsOpen(name)
		return n == 0, fmt.Sprint(n, " sessions")
	})
	inFlight := make(chan error, 1)
	go func() { inFlight <- run1(t, w.Target, ref, "sleep", "30") }()
	testutil.Eventually(t, "the operation in flight", func() (bool, string) {
		n := f.SessionsOpen(name)
		return n == 1, fmt.Sprint(n, " sessions")
	})
	f.DropVMConnections(name)
	select {
	case err := <-inFlight:
		if !errors.Is(err, provider.ErrUnavailable) {
			t.Errorf("an operation whose connection dropped: %v, want ErrUnavailable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an operation whose connection dropped did not end")
	}
	testutil.Eventually(t, "the dropped connection closed", func() (bool, string) {
		n := w.Target.VMConnections()
		return n == 0, fmt.Sprint(n, " held")
	})
}

// TestAStreamReachesAVMsPort: a direct stream on a ticket is relayed to a
// VM's port through hangar's port forward of the VM.
func TestAStreamReachesAVMsPort(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer := ticket.NewSigner(key)
	w, _ := newWorld(t, func(c *hangar.Config) { c.TicketKey = signer.PublicKey() })
	ref, _ := startVM(t, w, "web")
	// The fake forwards a VM's port to the host's: an echo server stands
	// for what the VM serves.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}()
		}
	}()
	port := int32(ln.Addr().(*net.TCPAddr).Port)
	where, err := w.Target.Ticket(t.Context(), ref, port)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(where.URL)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := signer.Issue(ticket.Claims{Target: hangartest.Target, Project: ref.Project, Instance: ref.Name, Port: port, Expires: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(w.Target.Streams())
	defer srv.Close()
	conn, err := streams.Dial(t.Context(), srv.URL+u.Path, tk)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, "ping\n"); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || line != "ping\n" {
		t.Errorf("the VM's port echoed %q, %v", line, err)
	}
}

// TestOperationsPastTheSessionsWait: more operations at once than the
// provider opens sessions on a VM's connection wait for room, on that one
// connection, and all run; the VM's sshd refuses none.
func TestOperationsPastTheSessionsWait(t *testing.T) {
	w, f := newWorld(t, nil)
	ref, name := startVM(t, w, "busy")
	const n = 70
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- run1(t, w.Target, ref, "sleep", "1")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("an operation of %d at once: %v", n, err)
		}
	}
	if r := f.SessionsRefused(name); r != 0 {
		t.Errorf("the VM's sshd refused %d sessions", r)
	}
	if c := f.VMConnections(name); c != 1 {
		t.Errorf("%d operations at once: the VM holds %d connections, want one", n, c)
	}
}

// TestARefusedSessionLeavesTheConnection: a VM whose sshd takes fewer
// sessions than the provider opens refuses some: those fail as the VM
// unavailable, while the operations in flight on the connection run on,
// and the connection stays.
func TestARefusedSessionLeavesTheConnection(t *testing.T) {
	w, f := newWorld(t, nil)
	ref, name := startVM(t, w, "small")
	if err := run1(t, w.Target, ref, "true"); err != nil {
		t.Fatal(err)
	}
	f.SetMaxSessions(name, 5)
	const n = 12
	var wg sync.WaitGroup
	var refused, ran atomic.Int32
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch err := run1(t, w.Target, ref, "sleep", "2"); {
			case err == nil:
				ran.Add(1)
			case errors.Is(err, provider.ErrUnavailable):
				refused.Add(1)
			default:
				t.Errorf("an operation past the sshd's sessions: %v", err)
			}
		}()
	}
	wg.Wait()
	if ran.Load() < 5 || refused.Load() == 0 || ran.Load()+refused.Load() != n {
		t.Errorf("%d operations at once on an sshd taking 5: %d ran, %d refused", n, ran.Load(), refused.Load())
	}
	if c, p := f.VMConnections(name), w.Target.VMConnections(); c != 1 || p != 1 {
		t.Errorf("after sessions refused the VM holds %d connections, the provider %d; want its one", c, p)
	}
}

// TestAnOperationInAFrozenVMEndsWithItsContext: an operation whose session
// a frozen VM never opens ends as its context does, whatever the
// keepalives have yet to find.
func TestAnOperationInAFrozenVMEndsWithItsContext(t *testing.T) {
	w, f := newWorld(t, func(c *hangar.Config) { c.KeepAliveEvery = time.Hour })
	ref, name := startVM(t, w, "frozen")
	if err := run1(t, w.Target, ref, "true"); err != nil {
		t.Fatal(err)
	}
	f.Freeze(name)
	defer f.Thaw(name)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := w.Target.Exec(ctx, provider.ExecRequest{Instance: ref, Command: []string{"true"}})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("an operation in a frozen VM as its context ended: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an operation in a frozen VM outlived its context")
	}
}
