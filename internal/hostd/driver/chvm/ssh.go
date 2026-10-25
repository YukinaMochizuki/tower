package chvm

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
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// sshKeys are hostd's key for its VMs' root, which cloud-init puts there.
type sshKeys struct {
	signer     ssh.Signer
	authorized string
}

// loadKeys reads hostd's key from dir, making it once.
func loadKeys(dir string) (sshKeys, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return sshKeys{}, err
	}
	path := filepath.Join(dir, "id_ed25519")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, priv, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			return sshKeys{}, gerr
		}
		block, merr := ssh.MarshalPrivateKey(priv, "hostd")
		if merr != nil {
			return sshKeys{}, merr
		}
		b = pem.EncodeToMemory(block)
		if err := writeAtomic(path, b, 0o600); err != nil {
			return sshKeys{}, err
		}
	} else if err != nil {
		return sshKeys{}, err
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return sshKeys{}, fmt.Errorf("%s: %w", path, err)
	}
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " tower-hostd"
	return sshKeys{signer: signer, authorized: authorized}, nil
}

// hostKeyFile is a VM's ssh host key, in its directory: hostd makes it
// and gives it to the VM through cloud-init (ssh_keys), so it knows the
// key the VM answers with before the VM ever boots.
const hostKeyFile = "ssh_host_ed25519_key"

// vmHostKey is a VM's host key, made once: its private key in OpenSSH's
// PEM and its public key.
func (d *Driver) vmHostKey(name string) (privPEM string, pub ssh.PublicKey, err error) {
	path := filepath.Join(d.dir(name), hostKeyFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, priv, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			return "", nil, gerr
		}
		block, merr := ssh.MarshalPrivateKey(priv, "")
		if merr != nil {
			return "", nil, merr
		}
		b = pem.EncodeToMemory(block)
		if err := os.MkdirAll(d.dir(name), 0o700); err != nil {
			return "", nil, err
		}
		if err := writeAtomic(path, b, 0o600); err != nil {
			return "", nil, err
		}
	} else if err != nil {
		return "", nil, err
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", path, err)
	}
	return string(b), signer.PublicKey(), nil
}

// sshConfig is how hostd connects to a VM: as root with its key, to a VM
// answering with the host key hostd gave it, and no other.
func (d *Driver) sshConfig(hostKey ssh.PublicKey) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(d.keys.signer)},
		HostKeyCallback:   ssh.FixedHostKey(hostKey),
		HostKeyAlgorithms: []string{hostKey.Type()},
		Timeout:           10 * time.Second,
	}
}

// In a VM, tower-guest is kept here, beside the sha256 it was copied as.
const (
	guestDir     = "/var/lib/tower/bin"
	guestBinary  = guestDir + "/tower-guest"
	guestVersion = guestDir + "/tower-guest.sha256"
)

// readyVM is a VM ssh reaches, with tower-guest in place, at one start.
type readyVM struct {
	started time.Time
	addr    string
	client  *ssh.Client
}

// forget drops what the driver knows of a VM's readiness, as at its stop.
func (d *Driver) forget(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.ready[name]; r != nil {
		_ = r.client.Close()
		delete(d.ready, name)
	}
}

// readyVM waits until a running VM's ssh answers, copies tower-guest in if
// its copy is not this hostd's, and returns it; a VM not running is
// ErrNotRunning.
func (d *Driver) readyVM(ctx context.Context, name string) (*readyVM, error) {
	c, s, err := d.inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	g := guestOf(c, s)
	if !g.Running || g.Address == "" {
		return nil, fmt.Errorf("VM %s: %w", name, driver.ErrNotRunning)
	}
	d.mu.Lock()
	r := d.ready[name]
	d.mu.Unlock()
	if r != nil && r.started.Equal(g.StartedAt) && r.addr == g.Address {
		return r, nil
	}
	d.forget(name)
	client, err := d.dialSSH(ctx, name, g.Address)
	if err != nil {
		return nil, err
	}
	r = &readyVM{started: g.StartedAt, addr: g.Address, client: client}
	if err := d.installGuest(ctx, r); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("VM %s: copying tower-guest in: %w", name, err)
	}
	d.mu.Lock()
	if old := d.ready[name]; old != nil {
		_ = old.client.Close()
	}
	d.ready[name] = r
	d.mu.Unlock()
	return r, nil
}

// dialSSH connects to a VM's ssh as root, waiting for it to answer up to
// ReadyTimeout, as a VM just started boots. A VM that stops meanwhile is
// ErrNotRunning. It answers with the host key hostd gave it, or is not
// spoken to.
func (d *Driver) dialSSH(ctx context.Context, name, addr string) (*ssh.Client, error) {
	_, hostKey, err := d.vmHostKey(name)
	if err != nil {
		return nil, err
	}
	cfg := d.sshConfig(hostKey)
	deadline := time.Now().Add(d.cfg.ReadyTimeout)
	var last error
	for {
		var dialer net.Dialer
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		conn, err := dialer.DialContext(dctx, "tcp", net.JoinHostPort(addr, "22"))
		cancel()
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
			c, chans, reqs, herr := ssh.NewClientConn(conn, addr, cfg)
			if herr == nil {
				_ = conn.SetDeadline(time.Time{})
				return ssh.NewClient(c, chans, reqs), nil
			}
			_ = conn.Close()
			err = herr
		}
		last = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("VM %s: its ssh did not answer in %v: %w", name, d.cfg.ReadyTimeout, last)
		}
		if g, err := d.Inspect(ctx, name); err != nil {
			return nil, err
		} else if !g.Running {
			return nil, fmt.Errorf("VM %s stopped as it booted: %s: %w", name, d.whyEnded(name), driver.ErrNotRunning)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// run runs a shell command line in the VM, returning its output.
func (r *readyVM) run(ctx context.Context, line string, stdin io.Reader) ([]byte, error) {
	s, err := r.client.NewSession()
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

// installGuest copies tower-guest into the VM unless the copy there is
// this hostd's.
func (d *Driver) installGuest(ctx context.Context, r *readyVM) error {
	have, _ := r.run(ctx, "cat "+guestVersion+" 2>/dev/null || true", nil)
	if strings.TrimSpace(string(have)) == d.guestHash {
		return nil
	}
	f, err := os.Open(d.cfg.GuestBinary)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = r.run(ctx, "mkdir -p "+guestDir+" && cat > "+guestBinary+".new && chmod 0755 "+guestBinary+".new && mv "+guestBinary+".new "+guestBinary+
		" && echo "+d.guestHash+" > "+guestVersion, f)
	return err
}

// RunShim runs tower-guest shim in the VM over an ssh session; ctx ending
// closes the session, which ends the shim's stdin, and so what it runs.
func (r *readyVM) RunShim(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	s, err := r.client.NewSession()
	if err != nil {
		return 0, fmt.Errorf("an ssh session: %w", err)
	}
	defer func() { _ = s.Close() }()
	s.Stdout, s.Stderr = stdout, stderr
	in, err := s.StdinPipe()
	if err != nil {
		return 0, err
	}
	if err := s.Start(guestBinary + " shim"); err != nil {
		return 0, err
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

// blockSize is what a base image is written in; a block of zeros is
// skipped, so the disk stays sparse.
const blockSize = 1 << 20

// writeBase writes a base image onto a blank disk: one whose first block
// is zeros. The first block goes last, so a write cut short leaves the disk
// blank, and is written again at the next start. A disk that is not blank
// is left alone.
func writeBase(disk, image string) error {
	dev, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = dev.Close() }()
	first := make([]byte, blockSize)
	if _, err := io.ReadFull(dev, first); err != nil {
		return fmt.Errorf("reading %s: %w", disk, err)
	}
	if !allZero(first) {
		return nil
	}
	img, err := os.Open(image)
	if err != nil {
		return fmt.Errorf("the base image: %w", err)
	}
	defer func() { _ = img.Close() }()
	ist, err := img.Stat()
	if err != nil {
		return err
	}
	size, err := dev.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if ist.Size() > size {
		return fmt.Errorf("the base image %s holds %d bytes, over the disk's %d", filepath.Base(image), ist.Size(), size)
	}
	buf := make([]byte, blockSize)
	for off := int64(blockSize); off < ist.Size(); off += blockSize {
		n, err := img.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n > 0 && !allZero(buf[:n]) {
			if _, err := dev.WriteAt(buf[:n], off); err != nil {
				return err
			}
		}
	}
	n, err := img.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if err := dev.Sync(); err != nil {
		return err
	}
	if _, err := dev.WriteAt(buf[:n], 0); err != nil {
		return err
	}
	return dev.Sync()
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
