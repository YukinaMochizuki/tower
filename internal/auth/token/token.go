// Package token is the format of every secret Hangar issues: a prefix that
// says what it is, then 32 random bytes in unpadded base64url. Only its
// SHA-256 is ever stored. A Secret never prints itself, so a token cannot
// end up in a log, an error or a terminal by accident. See
// docs/decisions/authentication.md.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Prefixes name what a token is. Each is unique, so a token found in the
// open says what it unlocks, and secret scanners can match it.
const (
	// Session is a browser's session cookie.
	Session = "hgs_"
	// Access is a CLI's short-lived access token.
	Access = "hga_"
	// Refresh is a CLI's refresh token.
	Refresh = "hgf_"
	// Service is an agent's service token.
	Service = "hgr_"
	// Endpoint is a browser's session cookie on one private endpoint.
	Endpoint = "hge_"
)

// Prefixes is every prefix there is.
var Prefixes = []string{Session, Access, Refresh, Service, Endpoint}

// randomBytes is how much randomness a token carries.
const randomBytes = 32

// Secret is a token's value. Its String and Format never show it; Reveal
// does, for the one place that must send or store it.
type Secret string

// New returns a fresh secret with the prefix.
func New(prefix string) Secret {
	return Secret(prefix + Random())
}

// Random returns 32 random bytes in unpadded base64url, for a secret or an
// opaque handle.
func Random() string {
	b := make([]byte, randomBytes)
	_, _ = rand.Read(b) // never fails; see crypto/rand.Read
	return base64.RawURLEncoding.EncodeToString(b)
}

// Reveal returns the secret's value.
func (s Secret) Reveal() string { return string(s) }

// Prefix returns the secret's prefix, such as "hga_", or "" if it has none
// Hangar knows.
func (s Secret) Prefix() string {
	for _, p := range Prefixes {
		if strings.HasPrefix(string(s), p) {
			return p
		}
	}
	return ""
}

// Hash is what is stored in place of the secret: its SHA-256 in hex.
func (s Secret) Hash() string { return Hash(string(s)) }

// Hash returns the SHA-256 of v in hex.
func Hash(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// String hides the value, keeping the prefix so it is clear what was there.
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return s.Prefix() + "[redacted]"
}

// GoString hides the value from %#v.
func (s Secret) GoString() string { return s.String() }

// Format hides the value from every verb, %s, %v, %q and %x alike.
func (s Secret) Format(f fmt.State, verb rune) {
	_, _ = fmt.Fprint(f, s.String())
}

// MarshalText hides the value from encoders, which would otherwise write
// the string underneath; whoever must write it uses Reveal.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }
