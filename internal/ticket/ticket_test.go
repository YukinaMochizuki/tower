package ticket_test

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/ticket"
)

var now = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

func signer(t *testing.T) *ticket.Signer {
	t.Helper()
	key, err := ticket.LoadOrCreateKey(filepath.Join(t.TempDir(), "ticket-key"))
	if err != nil {
		t.Fatal(err)
	}
	return ticket.NewSigner(key)
}

func claims() ticket.Claims {
	return ticket.Claims{Target: "t1", Project: "web", Instance: "box-a", Port: 8080, Expires: now.Add(5 * time.Minute)}
}

func TestATicketVerifiesForWhatItNames(t *testing.T) {
	s := signer(t)
	tk, err := s.Issue(claims())
	if err != nil {
		t.Fatal(err)
	}
	c, err := ticket.Verify(s.PublicKey(), tk, claims(), now)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Expires.Equal(claims().Expires) || c.Instance != "box-a" {
		t.Errorf("claims %+v, want %+v", c, claims())
	}
}

func TestWhatDoesNotVerify(t *testing.T) {
	s := signer(t)
	tk, err := s.Issue(claims())
	if err != nil {
		t.Fatal(err)
	}
	other := signer(t)
	forged, err := other.Issue(claims())
	if err != nil {
		t.Fatal(err)
	}
	// A payload changed under the same signature: port 8080 becomes 9090.
	parts := strings.Split(tk, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(payload), "8080", "9090", 1)))
	tampered := strings.Join(parts, ".")
	with := func(change func(*ticket.Claims)) ticket.Claims {
		c := claims()
		change(&c)
		return c
	}
	for _, tt := range []struct {
		name   string
		ticket string
		want   ticket.Claims
		at     time.Time
		err    error
	}{
		{"expired", tk, claims(), claims().Expires, ticket.ErrExpired},
		{"tampered", tampered, with(func(c *ticket.Claims) { c.Port = 9090 }), now, ticket.ErrInvalid},
		{"signed by another key", forged, claims(), now, ticket.ErrInvalid},
		{"another port", tk, with(func(c *ticket.Claims) { c.Port = 22 }), now, ticket.ErrNotFor},
		{"another instance", tk, with(func(c *ticket.Claims) { c.Instance = "box-b" }), now, ticket.ErrNotFor},
		{"another project", tk, with(func(c *ticket.Claims) { c.Project = "api" }), now, ticket.ErrNotFor},
		{"another target", tk, with(func(c *ticket.Claims) { c.Target = "t2" }), now, ticket.ErrNotFor},
		{"no ticket", "", claims(), now, ticket.ErrInvalid},
		{"garbage", "twt1.!!.!!", claims(), now, ticket.ErrInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ticket.Verify(s.PublicKey(), tt.ticket, tt.want, tt.at); !errors.Is(err, tt.err) {
				t.Errorf("verified with %v, want %v", err, tt.err)
			}
		})
	}
}

func TestTheKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ticket-key")
	made, err := ticket.LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the key file is mode %#o, want 0600", fi.Mode().Perm())
	}
	again, err := ticket.LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !made.Equal(again) {
		t.Error("a second start made another key instead of reading the file")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ticket.LoadOrCreateKey(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Errorf("a key file others read: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad")
	if err := os.WriteFile(bad, []byte("ed25519 c2hvcnQ=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ticket.LoadOrCreateKey(bad); err == nil {
		t.Error("a key file with a short seed loaded")
	}
}
