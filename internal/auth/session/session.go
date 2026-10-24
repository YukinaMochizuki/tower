// Package session issues and checks the credentials tower gives out: the
// secrets of service tokens. Every credential is opaque
// (internal/auth/token) and stored only as its hash (store.Credentials), so
// deleting the row revokes it at once. It keeps hangar's machinery for
// them, without sign-in: tower issues no sessions to people. See
// docs/decisions/authentication.md.
package session

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/auth/token"
	"github.com/YukinaMochizuki/tower/internal/store"
)

// TypeService is a service token's credential type, as stored. Its
// principal holds one at a time, each in a family of its own.
const TypeService = "service"

// familySep ends the token's uid in a service credential's family.
const familySep = "."

const (
	// touchEvery is how often a use is written back, at most.
	touchEvery = time.Minute
	// gcEvery is how often expired credentials are deleted.
	gcEvery = 10 * time.Minute
)

// ErrInvalid means the credential is unknown, revoked, expired or not of
// the kind asked for. Callers say no more than that.
var ErrInvalid = auth.ErrInvalidCredential

// Manager issues, checks and revokes credentials.
type Manager struct {
	store store.Credentials
	now   func() time.Time
	log   *slog.Logger
	// used hears of a service token's use, at most once per touchEvery.
	used func(auth.Principal, time.Time)
	// live says whether the service token of a uid still stands.
	live func(ctx context.Context, p auth.Principal, uid string) (bool, error)
}

// Options configure a Manager; zero values are the defaults.
type Options struct {
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	Log *slog.Logger
	// ServiceUsed is told when a service token is used, at most once a
	// minute per token, to record it where people can see it. It must not
	// block. Nil is no one.
	ServiceUsed func(p auth.Principal, at time.Time)
	// ServiceLive is asked on every use of a service token whether the
	// token it was issued to, by uid, still exists and is not being
	// deleted; a no refuses it, as an unknown one is. Nil asks no one.
	ServiceLive func(ctx context.Context, p auth.Principal, uid string) (bool, error)
}

// New returns a Manager over the store.
func New(st store.Credentials, opts Options) *Manager {
	m := &Manager{store: st, now: opts.Now, log: opts.Log, used: opts.ServiceUsed, live: opts.ServiceLive}
	if m.now == nil {
		m.now = time.Now
	}
	if m.log == nil {
		m.log = slog.New(slog.DiscardHandler)
	}
	return m
}

// Now is the manager's clock.
func (m *Manager) Now() time.Time { return m.now() }

// ServiceToken is a service token just issued: its secret, which exists
// nowhere else, and when it ends.
type ServiceToken struct {
	Secret    token.Secret
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// IssueService issues a service token for the principal, the token of the
// uid, valid for ttl, in place of any it held: from the moment it is stored
// the earlier one authenticates nothing, and of two issued at once only one
// is left. The uid goes in the credential's family, so the secret works only
// while that very token stands (Options.ServiceLive), never for a later one
// of the same name.
func (m *Manager) IssueService(ctx context.Context, p auth.Principal, uid string, ttl time.Duration) (ServiceToken, error) {
	if uid == "" || strings.Contains(uid, familySep) {
		return ServiceToken{}, fmt.Errorf("issue a service token: bad uid %q", uid)
	}
	now := m.now()
	secret := token.New(token.Service)
	c := store.Credential{
		Hash: secret.Hash(), Type: TypeService, Family: uid + familySep + token.Random(), Principal: p.String(),
		CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	if err := m.store.ReplaceCredentials(ctx, c, now); err != nil {
		return ServiceToken{}, fmt.Errorf("issue a service token: %w", err)
	}
	return ServiceToken{Secret: secret, IssuedAt: now, ExpiresAt: c.ExpiresAt}, nil
}

// Lookup returns who holds a service token's secret, and records its use.
// Unknown, revoked, expired and of another type are refused alike.
func (m *Manager) Lookup(ctx context.Context, s token.Secret) (auth.Principal, error) {
	return m.check(ctx, s, true)
}

// Stands is Lookup without counting as a use: it records nothing. A held
// stream rechecks its credential with it.
func (m *Manager) Stands(ctx context.Context, s token.Secret) error {
	_, err := m.check(ctx, s, false)
	return err
}

func (m *Manager) check(ctx context.Context, s token.Secret, use bool) (auth.Principal, error) {
	hash := s.Hash()
	c, err := m.store.GetCredential(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return auth.Principal{}, ErrInvalid
	}
	if err != nil {
		return auth.Principal{}, err
	}
	now := m.now()
	// The store found the row by its hash; comparing again in constant time
	// costs nothing and depends on no index's comparison.
	if subtle.ConstantTimeCompare([]byte(c.Hash), []byte(hash)) != 1 || c.Audience != "" {
		return auth.Principal{}, ErrInvalid
	}
	if c.Type != TypeService || !c.UsedAt.IsZero() || expired(c, now) {
		return auth.Principal{}, ErrInvalid
	}
	p, err := auth.ParsePrincipal(c.Principal)
	if err != nil {
		return auth.Principal{}, ErrInvalid
	}
	if m.live != nil {
		uid, _, ok := strings.Cut(c.Family, familySep)
		if !ok {
			return auth.Principal{}, ErrInvalid
		}
		if live, err := m.live(ctx, p, uid); err != nil {
			return auth.Principal{}, err
		} else if !live {
			return auth.Principal{}, ErrInvalid
		}
	}
	if use && now.Sub(c.LastUsedAt) >= touchEvery {
		if err := m.store.TouchCredential(ctx, c.Hash, now, time.Time{}); errors.Is(err, store.ErrNotFound) {
			return auth.Principal{}, ErrInvalid
		} else if err != nil {
			return auth.Principal{}, err
		}
		if m.used != nil {
			m.used(p, now)
		}
	}
	return p, nil
}

// RevokePrincipal ends every credential of a principal and returns how many
// that deleted.
func (m *Manager) RevokePrincipal(ctx context.Context, p auth.Principal) (int, error) {
	return m.store.RevokeCredentialsOf(ctx, p.String(), m.now())
}

// Run deletes expired credentials until ctx is done.
func (m *Manager) Run(ctx context.Context) error {
	t := time.NewTicker(gcEvery)
	defer t.Stop()
	for {
		if err := m.store.DeleteExpired(ctx, m.now()); err != nil && ctx.Err() == nil {
			m.log.Warn("could not delete expired credentials", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func expired(c store.Credential, now time.Time) bool {
	return !now.Before(c.ExpiresAt) || (!c.IdleUntil.IsZero() && !now.Before(c.IdleUntil))
}
