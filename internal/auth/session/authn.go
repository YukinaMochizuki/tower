package session

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/auth/token"
)

// Bearer authenticates `Authorization: Bearer <token>` with a service
// token. Any other token, and one unknown, revoked or expired, is refused
// alike.
type Bearer struct {
	M *Manager
}

func (b Bearer) Authenticate(ctx context.Context, header http.Header) (auth.Principal, bool, error) {
	p, _, ok, err := b.authenticate(ctx, header)
	return p, ok, err
}

// AuthenticateStream rechecks the service token as it is, without counting
// that as a use.
func (b Bearer) AuthenticateStream(ctx context.Context, header http.Header) (auth.Principal, func(context.Context) error, bool, error) {
	p, s, ok, err := b.authenticate(ctx, header)
	if !ok || err != nil {
		return auth.Principal{}, nil, ok, err
	}
	return p, func(ctx context.Context) error { return b.M.Stands(ctx, s) }, true, nil
}

func (b Bearer) authenticate(ctx context.Context, header http.Header) (auth.Principal, token.Secret, bool, error) {
	v := header.Get("Authorization")
	if v == "" {
		return auth.Principal{}, "", false, nil
	}
	scheme, raw, _ := strings.Cut(v, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return auth.Principal{}, "", false, errors.New("the Authorization header takes a Bearer token")
	}
	s := token.Secret(strings.TrimSpace(raw))
	if s.Prefix() != token.Service {
		return auth.Principal{}, "", false, ErrInvalid
	}
	p, err := b.M.Lookup(ctx, s)
	if err != nil {
		return auth.Principal{}, "", false, err
	}
	return p, s, true, nil
}
