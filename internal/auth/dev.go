package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// DevUserHeader names the header dev mode reads the caller's email from.
const DevUserHeader = "X-Hangar-Dev-User"

// cloudflareEdgeHeaders mark a request that came through Cloudflare: its edge
// sets the first three, and cloudflared adds Cf-Warp-Tag-Id to every request
// it proxies to the origin, whatever the edge sent.
var cloudflareEdgeHeaders = []string{"Cf-Connecting-Ip", "Cdn-Loop", "Cf-Ray", "Cf-Warp-Tag-Id"}

// Dev trusts DevUserHeader as the caller's email. It exists for local
// development and tests; the apiserver only enables it on a loopback address,
// and it refuses anything that came through Cloudflare, so a tunnel pointed at
// a dev-mode apiserver cannot reach it.
type Dev struct{}

func (Dev) Authenticate(_ context.Context, header http.Header) (Principal, bool, error) {
	for _, h := range cloudflareEdgeHeaders {
		if _, ok := header[http.CanonicalHeaderKey(h)]; ok {
			return Principal{}, false, errors.New("dev auth refuses requests that came through Cloudflare (" + h + " is set); switch the apiserver to oidc auth")
		}
	}
	email := strings.ToLower(strings.TrimSpace(header.Get(DevUserHeader)))
	if email == "" {
		return Principal{}, false, nil
	}
	return Principal{Type: User, ID: email}, true, nil
}
