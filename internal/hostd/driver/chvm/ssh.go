package chvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/YukinaMochizuki/tower/internal/guest/vmssh"
	"github.com/YukinaMochizuki/tower/internal/hostd/driver"
)

// sshKeys are hostd's key for its VMs' root, which cloud-init puts there.
type sshKeys = vmssh.Key

// loadKeys reads hostd's key from dir, making it once.
func loadKeys(dir string) (sshKeys, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return sshKeys{}, err
	}
	path := filepath.Join(dir, "id_ed25519")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		var gerr error
		if b, gerr = vmssh.NewKeyPEM("hostd"); gerr != nil {
			return sshKeys{}, gerr
		}
		if err := writeAtomic(path, b, 0o600); err != nil {
			return sshKeys{}, err
		}
	} else if err != nil {
		return sshKeys{}, err
	}
	keys, err := vmssh.ParseKey(b, "tower-hostd")
	if err != nil {
		return sshKeys{}, fmt.Errorf("%s: %w", path, err)
	}
	return keys, nil
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
		var gerr error
		if b, gerr = vmssh.NewKeyPEM(""); gerr != nil {
			return "", nil, gerr
		}
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
	return vmssh.ClientConfig(d.keys, ssh.FixedHostKey(hostKey), []string{hostKey.Type()})
}

// readyVM is a VM ssh reaches, with tower-guest in place, at one start.
type readyVM struct {
	started time.Time
	addr    string
	conn    *vmssh.Conn
}

// forget drops what the driver knows of a VM's readiness, as at its stop.
func (d *Driver) forget(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.ready[name]; r != nil {
		_ = r.conn.Close()
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
	conn, err := d.dialSSH(ctx, name, g.Address)
	if err != nil {
		return nil, err
	}
	r = &readyVM{started: g.StartedAt, addr: g.Address, conn: conn}
	if err := conn.InstallGuest(ctx, d.guestHash, func() (io.ReadCloser, error) { return os.Open(d.cfg.GuestBinary) }); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("VM %s: copying tower-guest in: %w", name, err)
	}
	d.mu.Lock()
	if old := d.ready[name]; old != nil {
		_ = old.conn.Close()
	}
	d.ready[name] = r
	d.mu.Unlock()
	return r, nil
}

// dialSSH connects to a VM's ssh as root, waiting for it to answer up to
// ReadyTimeout, as a VM just started boots. A VM that stops meanwhile is
// ErrNotRunning. It answers with the host key hostd gave it, or is not
// spoken to.
func (d *Driver) dialSSH(ctx context.Context, name, addr string) (*vmssh.Conn, error) {
	_, hostKey, err := d.vmHostKey(name)
	if err != nil {
		return nil, err
	}
	return vmssh.Dialer{
		What: "VM " + name, Config: d.sshConfig(hostKey), Addr: addr, ReadyTimeout: d.cfg.ReadyTimeout,
		Dial: func(ctx context.Context) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "tcp", net.JoinHostPort(addr, "22"))
		},
		Alive: func(ctx context.Context) error {
			if g, err := d.Inspect(ctx, name); err != nil {
				return err
			} else if !g.Running {
				return fmt.Errorf("VM %s stopped as it booted: %s: %w", name, d.whyEnded(name), driver.ErrNotRunning)
			}
			return nil
		},
	}.DialUntilReady(ctx)
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
