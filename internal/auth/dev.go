package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// DevPrincipalHeader names the header dev mode reads the caller from, a
// principal as Principal.String writes it, such as serviceToken:workstream.
const DevPrincipalHeader = "X-Tower-Dev-Principal"

// Dev trusts DevPrincipalHeader as the caller. It exists for local
// development and tests; the apiserver only enables it on a loopback
// address. It names a service token alone, which holds the role its
// ServiceToken object gives it, as one presenting its secret would; the
// system and root on the host never come from a header.
type Dev struct{}

func (Dev) Authenticate(_ context.Context, header http.Header) (Principal, bool, error) {
	v := strings.TrimSpace(header.Get(DevPrincipalHeader))
	if v == "" {
		return Principal{}, false, nil
	}
	p, err := ParsePrincipal(v)
	if err != nil {
		return Principal{}, false, err
	}
	if p.Type != ServiceToken {
		return Principal{}, false, errors.New("dev auth takes a service token alone, as serviceToken:<name>")
	}
	return p, true, nil
}
