//go:build !hostdfake

package main

import "testing"

// TestGuestDNSIsPublic: --guest-dns takes public addresses alone, which
// the guests' firewall lets them reach; a private, loopback, link-local or
// tailnet one is refused at start rather than leaving guests unresolved.
func TestGuestDNSIsPublic(t *testing.T) {
	got, err := parseDNS("1.1.1.1, 9.9.9.9")
	if err != nil || len(got) != 2 || got[1].String() != "9.9.9.9" {
		t.Fatalf("1.1.1.1, 9.9.9.9: %v, %v", got, err)
	}
	for _, s := range []string{"192.168.0.1", "10.0.0.53", "127.0.0.53", "100.100.100.100", "169.254.1.1", "fd7a:115c:a1e0::53", "", "dns.example"} {
		if _, err := parseDNS(s); err == nil {
			t.Errorf("%q was taken", s)
		}
	}
}
