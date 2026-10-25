package chvm

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// serve answers one ssh connection on ln with hostKey, accepting any key.
func serve(t *testing.T, ln net.Listener, hostKey ssh.Signer) {
	t.Helper()
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(hostKey)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, chans, reqs, err := ssh.NewServerConn(conn, cfg); err == nil {
			go ssh.DiscardRequests(reqs)
			for c := range chans {
				_ = c.Reject(ssh.Prohibited, "no")
			}
		}
	}()
}

// TestAVMIsSpokenToWithTheHostKeyItWasGiven: hostd speaks to a VM that
// answers with the host key hostd made for it, and to no other: one at
// the VM's address with another key is refused. The key reaches the VM
// through its cloud-config.
func TestAVMIsSpokenToWithTheHostKeyItWasGiven(t *testing.T) {
	keys, err := loadKeys(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{keys: keys, cfg: Config{StateDir: t.TempDir()}}
	priv, pub, err := d.vmHostKey("vm1")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey([]byte(priv))
	if err != nil {
		t.Fatal(err)
	}
	if again, pub2, _ := d.vmHostKey("vm1"); again != priv || string(pub2.Marshal()) != string(pub.Marshal()) {
		t.Error("a VM's host key was made again")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	impostor, err := ssh.NewSignerFromKey(other)
	if err != nil {
		t.Fatal(err)
	}
	dial := func(hostKey ssh.Signer) error {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		serve(t, ln, hostKey)
		c, err := ssh.Dial("tcp", ln.Addr().String(), d.sshConfig(pub))
		if err == nil {
			_ = c.Close()
		}
		return err
	}
	if err := dial(signer); err != nil {
		t.Errorf("the VM with its key: %v", err)
	}
	if err := dial(impostor); err == nil {
		t.Error("a VM answering with another host key was spoken to")
	}
	ud := d.userData(priv, pub)
	if !strings.Contains(ud, "ssh_keys:\n  ed25519_private: |\n    -----BEGIN OPENSSH PRIVATE KEY-----\n") ||
		!strings.Contains(ud, "  ed25519_public: "+strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))+"\n") ||
		!strings.Contains(ud, "ssh_genkeytypes: []\n") {
		t.Errorf("the cloud-config does not give the VM its host key:\n%s", ud)
	}
}
