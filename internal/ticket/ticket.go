// Package ticket issues and verifies the tickets that let a client stream
// to one port of one instance on a target directly: tower signs them with
// an ed25519 key it keeps in a file beside its key-encryption key, and the
// target, holding only tower's public key, verifies them. A ticket names
// its target, its instance, its port and when it expires; it says nothing
// else and grants nothing else. A ticket is a bearer credential and
// nothing marks it used: whoever holds it may present it again, any number
// of times, until it expires, which is why tickets live minutes. See
// docs/decisions/traffic.md.
package ticket

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// prefix starts every ticket, naming its format.
const prefix = "twt1."

// Errors Verify returns.
var (
	// ErrInvalid means the ticket is not one tower signed.
	ErrInvalid = errors.New("ticket: not a ticket tower signed")
	// ErrExpired means the ticket was tower's, and is past its expiry.
	ErrExpired = errors.New("ticket: expired")
	// ErrNotFor means the ticket is for another target, instance or port.
	ErrNotFor = errors.New("ticket: for another target, instance or port")
)

// Claims are what a ticket says.
type Claims struct {
	Target   string `json:"target"`
	Project  string `json:"project"`
	Instance string `json:"instance"`
	Port     int32  `json:"port"`
	// Expires is kept to the second.
	Expires time.Time `json:"-"`
}

type wire struct {
	Claims
	Exp int64 `json:"exp"`
}

// Signer issues tickets.
type Signer struct {
	key ed25519.PrivateKey
}

// NewSigner returns a signer with key.
func NewSigner(key ed25519.PrivateKey) *Signer { return &Signer{key: key} }

// PublicKey is what a target verifies tickets with.
func (s *Signer) PublicKey() ed25519.PublicKey { return s.key.Public().(ed25519.PublicKey) }

// Issue signs a ticket of the claims.
func (s *Signer) Issue(c Claims) (string, error) {
	payload, err := json.Marshal(wire{Claims: c, Exp: c.Expires.Unix()})
	if err != nil {
		return "", err
	}
	signed := prefix + base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(s.key, []byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Verify checks that the ticket was signed with the key of pub, is not
// expired at now, and is for want's target, instance and port (want's
// Expires is not read). It returns the ticket's claims.
func Verify(pub ed25519.PublicKey, ticket string, want Claims, now time.Time) (Claims, error) {
	rest, ok := strings.CutPrefix(ticket, prefix)
	if !ok {
		return Claims{}, ErrInvalid
	}
	payload64, sig64, ok := strings.Cut(rest, ".")
	if !ok {
		return Claims{}, ErrInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(sig64)
	if err != nil || !ed25519.Verify(pub, []byte(prefix+payload64), sig) {
		return Claims{}, ErrInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(payload64)
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var w wire
	if err := json.Unmarshal(payload, &w); err != nil {
		return Claims{}, ErrInvalid
	}
	c := w.Claims
	c.Expires = time.Unix(w.Exp, 0)
	if !now.Before(c.Expires) {
		return c, ErrExpired
	}
	if c.Target != want.Target || c.Project != want.Project || c.Instance != want.Instance || c.Port != want.Port {
		return c, ErrNotFor
	}
	return c, nil
}

// keyFileTag starts a key file's line.
const keyFileTag = "ed25519"

// LoadOrCreateKey reads the signing key in the file at path, which only
// its owner may read, making one if there is no file.
func LoadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	key, err := loadKey(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	_, key, err = ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		// Made by another start since: that one is the key.
		return loadKey(path)
	}
	if err != nil {
		return nil, err
	}
	_, err = fmt.Fprintf(f, "%s %s\n", keyFileTag, base64.StdEncoding.EncodeToString(key.Seed()))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return key, nil
}

func loadKey(path string) (ed25519.PrivateKey, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is mode %#o: the ticket signing key is its owner's alone (0600)", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 || fields[0] != keyFileTag {
		return nil, fmt.Errorf("%s: a ticket key file is one line, %q, a space, and a 32-byte seed in base64", path, keyFileTag)
	}
	seed, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s: the seed must be %d bytes in standard base64", path, ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
