package auth_test

import (
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// TestASCII: an email with any byte past ASCII is not ASCII, the Kelvin sign
// that lower-cases to k included.
func TestASCII(t *testing.T) {
	for s, want := range map[string]bool{
		"":                      true,
		"kate@example.com":      true,
		"Kate+x@Example.COM":    true,
		"\u212aate@example.com": false,
		"gil@ex\u00e4mple.com":  false,
		"bob@example.com\u00a0": false,
	} {
		if got := auth.ASCII(s); got != want {
			t.Errorf("ASCII(%q) = %v, want %v", s, got, want)
		}
	}
}
