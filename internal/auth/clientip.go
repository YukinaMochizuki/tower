package auth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
)

// ClientIPHeader carries the address of the client hangar-edge took a
// request from. The edge sets it on every request it forwards, replacing
// whatever the client sent, and the apiserver believes it only on the
// socket the edge alone may connect to (TrustClientIPFrom). See
// docs/decisions/front-door.md.
const ClientIPHeader = "Hangar-Client-Ip"

type clientIPKey struct{}

// ClientIP returns the address the request came from, as the listener it
// came in on established it (TrustClientIPFrom, ClientIPFromConn).
func ClientIP(ctx context.Context) (netip.Addr, bool) {
	ip, ok := ctx.Value(clientIPKey{}).(netip.Addr)
	return ip, ok
}

// ClientIPFromConn records the address of the connection a request came in
// on as its client's, and drops a ClientIPHeader, which no one on this
// listener may set. Use it on a TCP listener.
func ClientIPFromConn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del(ClientIPHeader)
		ctx := r.Context()
		if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
			ctx = context.WithValue(ctx, clientIPKey{}, ap.Addr().Unmap())
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type proxyPeerKey struct{}

// ProxyConnContext records the peer credentials of unix socket connections
// for TrustClientIPFrom. Use it as the http.Server's ConnContext on the
// socket hangar-edge forwards public API requests to, and on no other.
func ProxyConnContext(ctx context.Context, c net.Conn) context.Context {
	return withPeer(ctx, c, proxyPeerKey{})
}

// TrustClientIPFrom serves, on the socket ProxyConnContext records peers
// on, requests from the uids in uids alone, hangar-edge's, and takes their
// client's address from ClientIPHeader. Anyone else is refused before the
// request is read further: what the header says is believed only because
// of who sent it. The requests are authenticated as on any public listener;
// the edge vouches for the address, never for the caller.
func TrustClientIPFrom(uids []uint32, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, ok := r.Context().Value(proxyPeerKey{}).(*Peer)
		if !ok || !slices.Contains(uids, cred.UID) {
			http.Error(w, "this socket is hangar-edge's only", http.StatusForbidden)
			return
		}
		ip, err := netip.ParseAddr(r.Header.Get(ClientIPHeader))
		if err != nil {
			http.Error(w, fmt.Sprintf("hangar-edge sent no client address in %s", ClientIPHeader), http.StatusBadRequest)
			return
		}
		r.Header.Del(ClientIPHeader)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPKey{}, ip.Unmap())))
	})
}
