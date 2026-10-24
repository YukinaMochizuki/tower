package auth_test

import (
	"net/http"
	"os"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// TestPeerEdge: the edge socket makes hangar-edge's uid an Edge principal,
// and nothing else: not another uid, not a peer another listener recorded,
// and no header.
func TestPeerEdge(t *testing.T) {
	me := uint32(os.Getuid())
	t.Run("the edge's uid", func(t *testing.T) {
		r := overSocket(t, auth.EdgeConnContext, auth.PeerEdge{UIDs: []uint32{me}, Host: "n1"}, nil)
		if r.err != nil || !r.ok || r.p != (auth.Principal{Type: auth.Edge, ID: "n1"}) {
			t.Fatalf("got %v, %t, %v", r.p, r.ok, r.err)
		}
	})
	t.Run("another uid", func(t *testing.T) {
		r := overSocket(t, auth.EdgeConnContext, auth.PeerEdge{UIDs: []uint32{me + 1}, Host: "n1"}, nil)
		if r.ok || r.err == nil {
			t.Fatalf("another uid was the edge: %v, %t, %v", r.p, r.ok, r.err)
		}
	})
	t.Run("a peer the node listener recorded", func(t *testing.T) {
		r := overSocket(t, auth.ConnContext, auth.PeerEdge{UIDs: []uint32{me}, Host: "n1"}, nil)
		if r.ok || r.err != nil {
			t.Fatalf("a peer of the node socket was the edge: %v, %t, %v", r.p, r.ok, r.err)
		}
	})
	t.Run("the edge on the node listener", func(t *testing.T) {
		r := overSocket(t, auth.EdgeConnContext, auth.PeerCred{NodeUIDs: []uint32{me}, NodeName: "n1"}, nil)
		if r.ok || r.err != nil {
			t.Fatalf("a peer of the edge socket was a node: %v, %t, %v", r.p, r.ok, r.err)
		}
	})
	t.Run("no header makes it", func(t *testing.T) {
		h := http.Header{auth.DevUserHeader: {"edge:n1"}}
		_, ok, err := auth.PeerEdge{UIDs: []uint32{me}, Host: "n1"}.Authenticate(t.Context(), h)
		if ok || err != nil {
			t.Fatalf("headers without a connection were recognized: %t, %v", ok, err)
		}
	})
}
