package vmssh_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/YukinaMochizuki/tower/internal/guest/vmssh"
)

// hostdUserData is hostd's chvm userData as it was before it moved here
// (internal/hostd/driver/chvm/chvm.go at fa3447c), frozen: what hostd's
// VMs are given must not change with the move.
func hostdUserData(authorized, hostKeyPEM string, hostPub ssh.PublicKey) string {
	var key strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(hostKeyPEM), "\n") {
		key.WriteString("    " + line + "\n")
	}
	return "#cloud-config\n" +
		"hostname: " + "machine" + "\n" +
		"preserve_hostname: false\n" +
		"disable_root: false\n" +
		"ssh_pwauth: false\n" +
		"ssh_authorized_keys:\n" +
		"  - " + authorized + "\n" +
		"ssh_deletekeys: true\n" +
		"ssh_genkeytypes: []\n" +
		"ssh_keys:\n" +
		"  ed25519_private: |\n" + key.String() +
		"  ed25519_public: " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostPub))) + "\n" +
		"bootcmd:\n" +
		"  - [sh, -c, \"sed -i 's/^root:![^:]*:/root:*:/' /etc/shadow\"]\n" +
		"  - [sh, -c, \"if [ -d /etc/systemd ]; then mkdir -p /etc/systemd/resolved.conf.d && " +
		"printf '[Resolve]\\\\nResolveUnicastSingleLabel=yes\\\\n' > /etc/systemd/resolved.conf.d/tower.conf && " +
		"(systemctl try-restart systemd-resolved || true); fi\"]\n"
}

func newKey(t *testing.T, comment string) (vmssh.Key, []byte) {
	t.Helper()
	pemKey, err := vmssh.NewKeyPEM(comment)
	if err != nil {
		t.Fatal(err)
	}
	k, err := vmssh.ParseKey(pemKey, "tower-"+comment)
	if err != nil {
		t.Fatal(err)
	}
	return k, pemKey
}

// TestHostdsUserDataIsAsItWas: with a host key, as hostd gives one, the
// cloud-config is byte for byte what hostd's own made.
func TestHostdsUserDataIsAsItWas(t *testing.T) {
	root, _ := newKey(t, "hostd")
	host, hostPEM := newKey(t, "")
	got := vmssh.UserData{Authorized: root.Authorized, HostKeyPEM: string(hostPEM), HostKey: host.Signer.PublicKey()}.String()
	if want := hostdUserData(root.Authorized, string(hostPEM), host.Signer.PublicKey()); got != want {
		t.Errorf("hostd's cloud-config changed:\n%s\nwas:\n%s", got, want)
	}
}

// TestUserDataWithoutAHostKeyHoldsNoPrivateKey: a VM whose host key is
// pinned as first seen is given root's public key, the hostname and
// single-label resolution, and no private key of any kind, as the hangar
// provider's VMs are: their user_data is readable by the project's readers.
func TestUserDataWithoutAHostKeyHoldsNoPrivateKey(t *testing.T) {
	root, rootPEM := newKey(t, "hangar")
	ud := vmssh.UserData{Authorized: root.Authorized}.String()
	for _, want := range []string{"hostname: machine\n", "disable_root: false\n", "  - " + root.Authorized + "\n", "ResolveUnicastSingleLabel=yes"} {
		if !strings.Contains(ud, want) {
			t.Errorf("the cloud-config lacks %q:\n%s", want, ud)
		}
	}
	for _, never := range []string{"PRIVATE KEY", "ssh_keys", "ssh_genkeytypes", strings.TrimSpace(string(rootPEM))} {
		if strings.Contains(ud, never) {
			t.Errorf("the cloud-config holds %q:\n%s", never, ud)
		}
	}
}

// sshd answers ssh connections, each over an in-memory pipe, with
// hostKey, accepting the key authorized for root, and runs `echo`
// sessions.
func sshd(hostKey ssh.Signer, authorized ssh.PublicKey) func(context.Context) (net.Conn, error) {
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(m ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if m.User() == "root" && string(k.Marshal()) == string(authorized.Marshal()) {
			return nil, nil
		}
		return nil, errors.New("not authorized")
	}}
	cfg.AddHostKey(hostKey)
	return func(context.Context) (net.Conn, error) {
		client, server := memPipe()
		go serveOne(server, cfg)
		return client, nil
	}
}

// memPipe is a connection in memory, both ends of it. Unlike net.Pipe's,
// a write does not wait for its read: SSH's version exchange writes first
// at both ends, which over net.Pipe neither end would ever read. Its waits
// are a sync.Cond's, which synctest takes as durably blocked.
func memPipe() (net.Conn, net.Conn) {
	a, b := newMemBuf(), newMemBuf()
	return &memConn{r: a, w: b}, &memConn{r: b, w: a}
}

type memBuf struct {
	mu     sync.Mutex
	cond   *sync.Cond
	data   []byte
	closed bool
}

func newMemBuf() *memBuf {
	b := &memBuf{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *memBuf) close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cond.Broadcast()
}

type memConn struct{ r, w *memBuf }

func (c *memConn) Read(p []byte) (int, error) {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	for len(c.r.data) == 0 && !c.r.closed {
		c.r.cond.Wait()
	}
	if len(c.r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.r.data)
	c.r.data = c.r.data[n:]
	return n, nil
}

func (c *memConn) Write(p []byte) (int, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	if c.w.closed {
		return 0, io.ErrClosedPipe
	}
	c.w.data = append(c.w.data, p...)
	c.w.cond.Broadcast()
	return len(p), nil
}

func (c *memConn) Close() error {
	c.r.close()
	c.w.close()
	return nil
}

func (c *memConn) LocalAddr() net.Addr              { return memAddr{} }
func (c *memConn) RemoteAddr() net.Addr             { return memAddr{} }
func (c *memConn) SetDeadline(time.Time) error      { return nil }
func (c *memConn) SetReadDeadline(time.Time) error  { return nil }
func (c *memConn) SetWriteDeadline(time.Time) error { return nil }

type memAddr struct{}

func (memAddr) Network() string { return "mem" }
func (memAddr) String() string  { return "mem" }

func serveOne(server net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(server, cfg)
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
				_, _ = fmt.Fprintln(ch, strings.TrimPrefix(p.Command, "echo "))
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				_ = ch.Close()
			}
		}()
	}
}

// TestADialStopsAtARefusedHostKey: a host key check that refuses a key for
// good (ErrRefused) ends the wait for the VM at once; one that refuses it
// otherwise, as hostd's fixed key does, is waited out, as a VM booting
// answers with a key not yet its own. In a bubble, on its fake clock.
func TestADialStopsAtARefusedHostKey(t *testing.T) {
	root, _ := newKey(t, "t")
	host, _ := newKey(t, "")
	other, _ := newKey(t, "o")
	synctest.Test(t, func(t *testing.T) {
		refuse := func(err error) ssh.HostKeyCallback {
			return func(string, net.Addr, ssh.PublicKey) error { return err }
		}
		d := vmssh.Dialer{What: "VM v", Dial: sshd(host.Signer, root.Signer.PublicKey()), Addr: "v", ReadyTimeout: 3 * time.Second}

		d.Config = vmssh.ClientConfig(root, refuse(fmt.Errorf("not this one: %w", vmssh.ErrRefused)), nil)
		start := time.Now()
		if _, err := d.DialUntilReady(t.Context()); !errors.Is(err, vmssh.ErrRefused) || time.Since(start) != 0 {
			t.Errorf("a refused key: %v after %v, want ErrRefused at once", err, time.Since(start))
		}
		d.Config = vmssh.ClientConfig(root, refuse(errors.New("not yet")), nil)
		start = time.Now()
		if _, err := d.DialUntilReady(t.Context()); err == nil || errors.Is(err, vmssh.ErrRefused) || time.Since(start) < 3*time.Second {
			t.Errorf("a key refused for now: %v after %v, want the wait out", err, time.Since(start))
		}

		d.Config = vmssh.ClientConfig(root, ssh.FixedHostKey(host.Signer.PublicKey()), nil)
		c, err := d.DialUntilReady(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if out, err := c.Run(t.Context(), "echo hello", nil); err != nil || string(out) != "hello\n" {
			t.Errorf("a command: %q, %v", out, err)
		}
		_ = c.Close()
		// Another key than root's is not let in.
		d.Config, d.ReadyTimeout = vmssh.ClientConfig(other, ssh.FixedHostKey(host.Signer.PublicKey()), nil), 0
		if _, err := d.DialUntilReady(t.Context()); err == nil {
			t.Error("a key root was not given logged in")
		}
	})
}
