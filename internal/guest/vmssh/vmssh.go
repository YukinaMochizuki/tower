// Package vmssh is how tower reaches root in its VMs, on every provider
// that runs them: over SSH as root, with tower's own key, which cloud-init
// gives root (UserData), to an sshd whose host key is known before
// anything is sent. tower-guest is copied in once (Conn.InstallGuest) and
// each operation is a `tower-guest shim` session on one connection
// (Conn.RunShim, internal/guest/guestops), as a container's are an exec.
//
// hostd's VM driver (internal/hostd/driver/chvm) dials a VM's address and
// gives it its host key in its cloud-config; the hangar provider dials
// through hangar's port forward and pins the host key the VM first answers
// with (docs/decisions/traffic.md).
package vmssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Hostname is every VM's: no VM's own, the same in a golden's builder and
// in each clone of it, as k3s writes the hostname into its node's name and
// its etcd member (docs/decisions/machine-blueprints.md).
const Hostname = "machine"

// In a VM, tower-guest is kept here, beside the sha256 it was copied as.
const (
	GuestDir     = "/var/lib/tower/bin"
	GuestBinary  = GuestDir + "/tower-guest"
	GuestVersion = GuestDir + "/tower-guest.sha256"
)

// ProcRoot is where a VM keeps its processes: a directory of the VM's own
// name, so a clone of a golden never lists its builder's.
func ProcRoot(name string) string { return "/var/lib/tower/processes/" + name }

// Key is tower's key for its VMs' root.
type Key struct {
	Signer ssh.Signer
	// Authorized is its public key as an authorized_keys line.
	Authorized string
}

// NewKeyPEM makes a key, in OpenSSH's PEM, its comment comment.
func NewKeyPEM(comment string) ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(block), nil
}

// ParseKey reads a key in PEM; its authorized_keys line ends with comment.
// Its error is x/crypto/ssh's, which never quotes the key.
func ParseKey(b []byte, comment string) (Key, error) {
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return Key{}, err
	}
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + comment
	return Key{Signer: signer, Authorized: authorized}, nil
}

// UserData is a VM's cloud-config: Authorized for root, the one hostname,
// and, if HostKeyPEM is set, the host key the VM answers with (its only
// one). A root locked by its password field ("!", as Alpine's is) takes no
// key either; it is given one that only locks its password. The
// workstream's aliases are single labels, which systemd-resolved (as
// Ubuntu's) sends to no DNS server unless told to: it is told to, at each
// boot.
//
// Without HostKeyPEM, cloud-init makes the VM's host keys itself at its
// first boot as an instance, which a VM cloned from a golden is again.
type UserData struct {
	Authorized string
	HostKeyPEM string
	HostKey    ssh.PublicKey
	// MaxSessions, if set, is the sessions the VM's sshd opens on one
	// connection (OpenSSH's MaxSessions, 10 unless set): a drop-in of
	// /etc/ssh/sshd_config.d, written at each boot, before sshd reads it.
	MaxSessions int
}

// SSHDDropIn is where UserData.MaxSessions is written in the VM.
const SSHDDropIn = "/etc/ssh/sshd_config.d/60-tower.conf"

func (u UserData) String() string {
	var b strings.Builder
	b.WriteString("#cloud-config\n" +
		"hostname: " + Hostname + "\n" +
		"preserve_hostname: false\n" +
		"disable_root: false\n" +
		"ssh_pwauth: false\n" +
		"ssh_authorized_keys:\n" +
		"  - " + u.Authorized + "\n")
	if u.HostKeyPEM != "" {
		b.WriteString("ssh_deletekeys: true\n" +
			"ssh_genkeytypes: []\n" +
			"ssh_keys:\n" +
			"  ed25519_private: |\n")
		for _, line := range strings.Split(strings.TrimSpace(u.HostKeyPEM), "\n") {
			b.WriteString("    " + line + "\n")
		}
		b.WriteString("  ed25519_public: " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(u.HostKey))) + "\n")
	}
	b.WriteString("bootcmd:\n" +
		"  - [sh, -c, \"sed -i 's/^root:![^:]*:/root:*:/' /etc/shadow\"]\n" +
		"  - [sh, -c, \"if [ -d /etc/systemd ]; then mkdir -p /etc/systemd/resolved.conf.d && " +
		"printf '[Resolve]\\\\nResolveUnicastSingleLabel=yes\\\\n' > /etc/systemd/resolved.conf.d/tower.conf && " +
		"(systemctl try-restart systemd-resolved || true); fi\"]\n")
	if u.MaxSessions > 0 {
		b.WriteString("  - [sh, -c, \"mkdir -p " + path.Dir(SSHDDropIn) + " && " +
			"printf 'MaxSessions " + strconv.Itoa(u.MaxSessions) + "\\\\n' > " + SSHDDropIn + " && " +
			"(systemctl try-reload-or-restart ssh 2>/dev/null; systemctl try-reload-or-restart sshd 2>/dev/null; true)\"]\n")
	}
	return b.String()
}

// ClientConfig is how tower speaks to a VM: as root with key, to an sshd
// hostKey accepts.
func ClientConfig(key Key, hostKey ssh.HostKeyCallback, algorithms []string) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(key.Signer)},
		HostKeyCallback: hostKey, HostKeyAlgorithms: algorithms,
		Timeout: 10 * time.Second,
	}
}

// ErrRefused wraps what a host key check says of a key it refuses for
// good: a dial gives up on it at once rather than waiting for the VM.
var ErrRefused = errors.New("refused")

// ErrNoSession is a RunShim whose session never opened, so nothing of it
// ran: one sshd refused (an ssh.OpenChannelError, as past its
// MaxSessions), or on a connection that had ended.
var ErrNoSession = errors.New("no ssh session opened")

// noSession is ErrNoSession, saying why as it always has.
type noSession struct{ err error }

func (e noSession) Error() string        { return "an ssh session: " + e.err.Error() }
func (e noSession) Unwrap() error        { return e.err }
func (e noSession) Is(target error) bool { return target == ErrNoSession }

// Dialer dials a VM until its sshd answers.
type Dialer struct {
	// What names the VM in errors, as "VM t-1234".
	What string
	// Dial connects to the VM's port 22.
	Dial func(ctx context.Context) (net.Conn, error)
	// Config is the connection's; Addr the host name it is given.
	Config *ssh.ClientConfig
	Addr   string
	// ReadyTimeout bounds the wait, as a VM just started boots.
	ReadyTimeout time.Duration
	// Alive, if set, is asked between attempts whether to go on waiting:
	// its error, such as the VM having stopped, ends the wait.
	Alive func(ctx context.Context) error
}

// DialUntilReady connects, trying again every second until ReadyTimeout,
// as a VM just started boots, unless Alive says otherwise or the host key
// check refuses the key for good (ErrRefused).
func (d Dialer) DialUntilReady(ctx context.Context) (*Conn, error) {
	var refused error
	cfg := *d.Config
	check := cfg.HostKeyCallback
	cfg.HostKeyCallback = func(host string, remote net.Addr, key ssh.PublicKey) error {
		err := check(host, remote, key)
		if errors.Is(err, ErrRefused) {
			refused = err
		}
		return err
	}
	deadline := time.Now().Add(d.ReadyTimeout)
	var last error
	for {
		refused = nil
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err := d.Dial(dctx)
		cancel()
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
			c, chans, reqs, herr := ssh.NewClientConn(conn, d.Addr, &cfg)
			if herr == nil {
				_ = conn.SetDeadline(time.Time{})
				return &Conn{client: ssh.NewClient(c, chans, reqs)}, nil
			}
			_ = conn.Close()
			err = herr
		}
		if refused != nil {
			return nil, refused
		}
		last = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s: its ssh did not answer in %v: %w", d.What, d.ReadyTimeout, last)
		}
		if d.Alive != nil {
			if err := d.Alive(ctx); err != nil {
				return nil, err
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Conn is a VM's root over SSH.
type Conn struct {
	client *ssh.Client
}

// NewConn is a Conn over a client already connected.
func NewConn(c *ssh.Client) *Conn { return &Conn{client: c} }

// Close closes the connection, ending every session on it.
func (c *Conn) Close() error { return c.client.Close() }

// Wait waits until the connection ends, by Close or by the VM.
func (c *Conn) Wait() error { return c.client.Wait() }

// newSession opens a session, waiting for it no longer than ctx: one that
// opens after is closed.
func (c *Conn) newSession(ctx context.Context) (*ssh.Session, error) {
	type opened struct {
		s   *ssh.Session
		err error
	}
	done := make(chan opened, 1)
	go func() {
		s, err := c.client.NewSession()
		done <- opened{s, err}
	}()
	select {
	case o := <-done:
		return o.s, o.err
	case <-ctx.Done():
		go func() {
			if o := <-done; o.s != nil {
				_ = o.s.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// Alive asks the VM's sshd for an answer (keepalive@openssh.com, as
// OpenSSH's ServerAliveInterval does), waiting up to timeout: one that does
// not come says the connection is dead, as when the guest froze. A refusal
// is an answer.
func (c *Conn) Alive(timeout time.Duration) bool {
	done := make(chan error, 1)
	go func() {
		_, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	}()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err := <-done:
		return err == nil
	case <-t.C:
		return false
	}
}

// KeepAlive asks the VM's sshd every interval whether it answers, until
// the connection ends, and closes it once it does not within timeout.
func (c *Conn) KeepAlive(every, timeout time.Duration) {
	ended := make(chan struct{})
	go func() {
		_ = c.client.Wait()
		close(ended)
	}()
	go func() {
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-ended:
				return
			case <-tick.C:
			}
			if !c.Alive(timeout) {
				_ = c.client.Close()
				return
			}
		}
	}()
}

// Run runs a shell command line in the VM, returning its output.
func (c *Conn) Run(ctx context.Context, line string, stdin io.Reader) ([]byte, error) {
	s, err := c.newSession(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	var out, errOut bytes.Buffer
	s.Stdout, s.Stderr = &out, &errOut
	if stdin != nil {
		s.Stdin = stdin
	}
	if err := s.Run(line); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", line, err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

// GuestHash is the sha256 the tower-guest in the VM was copied as, "" for
// none.
func (c *Conn) GuestHash(ctx context.Context) (string, error) {
	have, err := c.Run(ctx, "cat "+GuestVersion+" 2>/dev/null || true", nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(have)), nil
}

// InstallGuest copies tower-guest into the VM unless the copy there is
// hash, reading it from open.
func (c *Conn) InstallGuest(ctx context.Context, hash string, open func() (io.ReadCloser, error)) error {
	have, _ := c.GuestHash(ctx)
	if have == hash {
		return nil
	}
	f, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = c.Run(ctx, "mkdir -p "+GuestDir+" && cat > "+GuestBinary+".new && chmod 0755 "+GuestBinary+".new && mv "+GuestBinary+".new "+GuestBinary+
		" && echo "+hash+" > "+GuestVersion, f)
	return err
}

// RunShim runs tower-guest shim in the VM over a session; ctx ending
// closes the session, which ends the shim's stdin, and so what it runs.
func (c *Conn) RunShim(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	s, err := c.newSession(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, noSession{err}
	}
	defer func() { _ = s.Close() }()
	s.Stdout, s.Stderr = stdout, stderr
	in, err := s.StdinPipe()
	if err != nil {
		return 0, err
	}
	started := make(chan error, 1)
	go func() { started <- s.Start(GuestBinary + " shim") }()
	select {
	case err := <-started:
		if err != nil {
			return 0, err
		}
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	go func() {
		_, _ = io.Copy(in, stdin)
		_ = in.Close()
	}()
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	err = s.Wait()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}
