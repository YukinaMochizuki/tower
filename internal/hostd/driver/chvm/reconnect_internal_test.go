package chvm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"golang.org/x/crypto/ssh"

	"github.com/YukinaMochizuki/tower/internal/guest/vmssh"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver/docker"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// fakeVM is a running VM as the driver sees one: Docker says its container
// runs, at one start and address, and its sshd answers exec sessions with
// the tower-guest the driver has, and echo.
type fakeVM struct {
	t  *testing.T
	ln net.Listener

	mu sync.Mutex
	// conns are the sshd's side of each connection the driver made.
	conns []*freezable
}

// freezable is the sshd's side of a connection, which freeze makes read
// nothing more, as a hung guest's: the driver hears no answer, and no end.
type freezable struct {
	net.Conn
	once    sync.Once
	frozen  chan struct{}
	release chan struct{}
}

func (c *freezable) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	select {
	case <-c.frozen:
		<-c.release
		return 0, io.EOF
	default:
		return n, err
	}
}

func (c *freezable) freeze() { c.once.Do(func() { close(c.frozen) }) }

// newFakeVM is a driver and the one VM vm1 it runs.
func newFakeVM(t *testing.T) (*Driver, *fakeVM) {
	t.Helper()
	keys, err := loadKeys(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{keys: keys, guestHash: "the-guest", ready: map[string]*readyVM{}, cfg: Config{
		StateDir: t.TempDir(), ReadyTimeout: 5 * time.Second, KeepAliveEvery: 50 * time.Millisecond, KeepAliveTimeout: 200 * time.Millisecond,
	}}
	priv, _, err := d.vmHostKey("vm1")
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.ParsePrivateKey([]byte(priv))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	d.sshPort = port
	vm := &fakeVM{t: t, ln: ln}
	go vm.serve(hostKey)

	spec, _ := json.Marshal(spec{Disk: "vm1", BaseImage: "ubuntu"})
	started := time.Now().UTC().Format(time.RFC3339Nano)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasSuffix(req.URL.Path, "/containers/vm1/json") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Name": "/vm1", "Created": started,
			"State":           map[string]any{"Running": true, "StartedAt": started},
			"Config":          map[string]any{"Labels": map[string]string{docker.LabelDriver: "vm", docker.LabelSpec: string(spec)}},
			"NetworkSettings": map[string]any{"Networks": map[string]any{docker.DefaultNetwork: map[string]any{"IPAddress": "127.0.0.1"}}},
		})
	}))
	t.Cleanup(api.Close)
	u, err := url.Parse(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := client.New(client.WithHost("tcp://"+u.Host), client.WithAPIVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	d.cli = cli
	t.Cleanup(func() {
		d.forget("vm1")
		vm.drop()
	})
	return d, vm
}

func (vm *fakeVM) serve(hostKey ssh.Signer) {
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(hostKey)
	for {
		c, err := vm.ln.Accept()
		if err != nil {
			return
		}
		fc := &freezable{Conn: c, frozen: make(chan struct{}), release: make(chan struct{})}
		vm.mu.Lock()
		vm.conns = append(vm.conns, fc)
		vm.mu.Unlock()
		go vm.answer(fc, cfg)
	}
}

func (vm *fakeVM) answer(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for r := range creqs {
				if r.Type != "exec" {
					_ = r.Reply(false, nil)
					continue
				}
				_ = r.Reply(true, nil)
				var p struct{ Command string }
				_ = ssh.Unmarshal(r.Payload, &p)
				switch {
				case strings.HasPrefix(p.Command, "cat "+vmssh.GuestVersion):
					_, _ = fmt.Fprintln(ch, "the-guest")
				case strings.HasPrefix(p.Command, "echo "):
					_, _ = fmt.Fprintln(ch, strings.TrimPrefix(p.Command, "echo "))
				}
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				_ = ch.Close()
			}
		}()
	}
}

// drop ends every connection on the VM's side, as a VM rebooting in its
// Cloud Hypervisor does: its container's start and address stay the same.
func (vm *fakeVM) drop() {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	for _, c := range vm.conns {
		_ = c.Close()
		close(c.release)
	}
	vm.conns = nil
}

// freeze makes every connection's sshd answer nothing more, keeping it
// open, as a guest that hung or rebooted without a reset does.
func (vm *fakeVM) freeze() {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	for _, c := range vm.conns {
		c.freeze()
	}
}

// awaitForgotten waits until the driver holds no connection to vm1.
func awaitForgotten(t *testing.T, d *Driver) {
	t.Helper()
	testutil.Eventually(t, "the driver drops the connection that ended", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.ready["vm1"] == nil
	})
}

func (vm *fakeVM) dialed() int {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return len(vm.conns)
}

// echo runs `echo hi` in vm1 over the driver's connection.
func echo(ctx context.Context, d *Driver) error {
	r, err := d.readyVM(ctx, "vm1")
	if err != nil {
		return err
	}
	out, err := r.conn.Run(ctx, "echo hi", nil)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "hi" {
		return fmt.Errorf("echo said %q", out)
	}
	return nil
}

// TestAConnectionTheVMEndedIsDialedAgain: a VM that reboots inside its
// Cloud Hypervisor keeps its container's start and address, and ends the
// driver's ssh connection. The next operation dials it again rather than
// using the connection that ended (tower#73).
func TestAConnectionTheVMEndedIsDialedAgain(t *testing.T) {
	d, vm := newFakeVM(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := echo(ctx, d); err != nil {
		t.Fatalf("the first operation: %v", err)
	}
	vm.drop()
	// The driver's side learns of the end as its read fails.
	awaitForgotten(t, d)
	if err := echo(ctx, d); err != nil {
		t.Fatalf("an operation after the VM ended the connection: %v", err)
	}
	if n := vm.dialed(); n != 1 {
		t.Errorf("the VM was dialed %d times since it ended the connection, want 1", n)
	}
}

// TestAConnectionTheVMStoppedAnsweringIsDialedAgain: a VM that stops
// answering without ending the connection (a hung guest, a reboot that
// sends no reset) fails its keepalive; the driver closes the connection
// and its next operation dials the VM again, rather than waiting on it
// until TCP gives up (tower#73).
func TestAConnectionTheVMStoppedAnsweringIsDialedAgain(t *testing.T) {
	d, vm := newFakeVM(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := echo(ctx, d); err != nil {
		t.Fatalf("the first operation: %v", err)
	}
	vm.freeze()
	awaitForgotten(t, d)
	if err := echo(ctx, d); err != nil {
		t.Fatalf("an operation after the VM stopped answering: %v", err)
	}
	if n := vm.dialed(); n != 2 {
		t.Errorf("the VM was dialed %d times, want 2", n)
	}
}
