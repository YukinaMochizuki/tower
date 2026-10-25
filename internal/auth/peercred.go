package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// Peer is what the kernel reports for the process at the other end of a
// unix socket connection.
type Peer struct {
	UID uint32
	PID int32
}

// The admin listener records its peers' credentials under a key of its own,
// so an authenticator only ever sees the credentials of the listener it was
// meant for, whatever chain it is put in.
type adminPeerKey struct{}

// AdminConnContext records the peer credentials of unix socket connections
// for LocalRoot. Use it as the http.Server's ConnContext on the admin
// listener, and on no other.
func AdminConnContext(ctx context.Context, c net.Conn) context.Context {
	return withPeer(ctx, c, adminPeerKey{})
}

// AdminPeer returns the credentials AdminConnContext recorded for the
// request's connection.
func AdminPeer(ctx context.Context) (*Peer, bool) {
	cred, ok := ctx.Value(adminPeerKey{}).(*Peer)
	return cred, ok
}

func withPeer(ctx context.Context, c net.Conn, key any) context.Context {
	cred, err := PeerOf(c)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, key, cred)
}

// LocalRoot authenticates root on the host's admin socket, the way in that
// works whatever the public listener's auth does, and the way the first
// admin token is made. It reads only what AdminConnContext recorded, and no
// header, so no other listener can produce its principal. See
// docs/decisions/authentication.md.
type LocalRoot struct {
	// Host names the host in the principal, such as root@<host>.
	Host string
	// UID is the uid accepted; zero, root, everywhere but in tests.
	UID uint32
}

func (l LocalRoot) Authenticate(ctx context.Context, _ http.Header) (Principal, bool, error) {
	cred, ok := AdminPeer(ctx)
	if !ok {
		return Principal{}, false, nil
	}
	if cred.UID != l.UID {
		return Principal{}, false, fmt.Errorf("peer uid %d is not root; the admin socket is root's only", cred.UID)
	}
	if l.Host == "" {
		return Principal{}, false, errors.New("the admin socket has no host name configured")
	}
	return Principal{Type: Local, ID: "root@" + l.Host}, true, nil
}
