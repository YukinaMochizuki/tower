package session_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YukinaMochizuki/tower/internal/auth"
	"github.com/YukinaMochizuki/tower/internal/auth/session"
	"github.com/YukinaMochizuki/tower/internal/auth/token"
	"github.com/YukinaMochizuki/tower/internal/store/sqlite"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newManager(t *testing.T, opts session.Options) (*session.Manager, *clock, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "tower.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := &clock{now: time.UnixMilli(1_800_000_000_000)}
	opts.Now = c.Now
	return session.New(st, opts), c, st
}

var agent = auth.Principal{Type: auth.ServiceToken, ID: "deployer"}

// TestBearer: only a service token is a bearer credential, and an invalid
// one is refused rather than passed over.
func TestBearer(t *testing.T) {
	ctx := t.Context()
	m, _, _ := newManager(t, session.Options{})
	tok, err := m.IssueService(ctx, agent, "uid-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bearer := session.Bearer{M: m}
	authn := func(v string) (auth.Principal, bool, error) {
		return bearer.Authenticate(ctx, http.Header{"Authorization": {v}})
	}
	if p, ok, err := authn("Bearer " + tok.Secret.Reveal()); !ok || err != nil || p != agent {
		t.Errorf("service token: %v, %t, %v", p, ok, err)
	}
	for _, v := range []string{"Bearer hgr_" + tok.Secret.Reveal()[len(token.Service):], "Bearer twr_nope", "Bearer x", "Basic abc"} {
		if _, ok, err := authn(v); ok || err == nil {
			t.Errorf("%.12s…: %t, %v", v, ok, err)
		}
	}
	if _, ok, err := bearer.Authenticate(ctx, http.Header{}); ok || err != nil {
		t.Errorf("no header: %t, %v", ok, err)
	}
}

// TestServiceTokens: a service token works until it expires, as a bearer
// token, and issuing another replaces it; its uses are reported at most
// once a minute.
func TestServiceTokens(t *testing.T) {
	ctx := t.Context()
	var mu sync.Mutex
	var uses []time.Time
	m, c, _ := newManager(t, session.Options{ServiceUsed: func(p auth.Principal, at time.Time) {
		if p != agent {
			t.Errorf("use reported for %v", p)
		}
		mu.Lock()
		uses = append(uses, at)
		mu.Unlock()
	}})
	first, err := m.IssueService(ctx, agent, "uid-1", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if first.Secret.Prefix() != token.Service || !first.ExpiresAt.Equal(first.IssuedAt.Add(2*time.Hour)) {
		t.Fatalf("issued %v until %v", first.Secret, first.ExpiresAt)
	}
	bearer := session.Bearer{M: m}
	authn := func(s token.Secret) (auth.Principal, error) {
		p, ok, err := bearer.Authenticate(ctx, http.Header{"Authorization": {"Bearer " + s.Reveal()}})
		if err == nil && !ok {
			err = errors.New("not recognized")
		}
		return p, err
	}
	if p, err := authn(first.Secret); err != nil || p != agent {
		t.Fatalf("service token: %v, %v", p, err)
	}
	// Uses within a minute are reported once.
	if _, err := authn(first.Secret); err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	if _, err := authn(first.Secret); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(uses) != 2 {
		t.Errorf("%d uses reported, want 2: %v", len(uses), uses)
	}
	mu.Unlock()

	second, err := m.IssueService(ctx, agent, "uid-1", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authn(first.Secret); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("a replaced service token: %v", err)
	}
	if _, err := authn(second.Secret); err != nil {
		t.Errorf("its replacement: %v", err)
	}
	for _, s := range []token.Secret{"twr_nope", token.New(token.Service)} {
		if _, err := authn(s); !errors.Is(err, session.ErrInvalid) {
			t.Errorf("an unknown service token: %v", err)
		}
	}

	c.Advance(2 * time.Hour)
	if _, err := authn(second.Secret); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("an expired service token: %v", err)
	}
	// Revoking the principal ends it, as deleting the token does.
	third, _ := m.IssueService(ctx, agent, "uid-1", time.Hour)
	if _, err := m.RevokePrincipal(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if _, err := authn(third.Secret); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("a revoked service token: %v", err)
	}
}

// TestServiceTokenNeedsItsToken: a service token works only while the token
// of the uid it was issued to stands, and is refused, as an unknown one is,
// once that token is gone or another holds its name.
func TestServiceTokenNeedsItsToken(t *testing.T) {
	ctx := t.Context()
	var mu sync.Mutex
	standing := "uid-1"
	m, _, _ := newManager(t, session.Options{ServiceLive: func(_ context.Context, p auth.Principal, uid string) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return p == agent && uid == standing, nil
	}})
	issued, err := m.IssueService(ctx, agent, "uid-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Lookup(ctx, issued.Secret); err != nil {
		t.Fatalf("while its token stands: %v", err)
	}
	mu.Lock()
	standing = "uid-2" // a token of the same name, made again
	mu.Unlock()
	if _, err := m.Lookup(ctx, issued.Secret); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("once another token holds the name: %v", err)
	}
	if _, err := m.IssueService(ctx, agent, "a.b", time.Hour); err == nil {
		t.Error("a uid holding the family separator was taken")
	}
}

// usage is when a credential was last used, and when it goes idle.
func usage(t *testing.T, st *sqlite.Store, hash string) [2]time.Time {
	t.Helper()
	c, err := st.GetCredential(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	return [2]time.Time{c.LastUsedAt, c.IdleUntil}
}

// TestAServiceTokenStreamRecheckIsNoUse: rechecking a service token's
// stream neither records a use nor reports one.
func TestAServiceTokenStreamRecheckIsNoUse(t *testing.T) {
	ctx := t.Context()
	var reported atomic.Int32
	m, c, st := newManager(t, session.Options{ServiceUsed: func(auth.Principal, time.Time) { reported.Add(1) }})
	tok, err := m.IssueService(ctx, agent, "uid1", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, recheck, ok, err := session.Bearer{M: m}.AuthenticateStream(ctx, http.Header{"Authorization": {"Bearer " + tok.Secret.Reveal()}})
	if !ok || err != nil {
		t.Fatalf("opening a stream: %t, %v", ok, err)
	}
	opened, uses := usage(t, st, tok.Secret.Hash()), reported.Load()
	for range 3 {
		c.Advance(time.Hour)
		if err := recheck(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := usage(t, st, tok.Secret.Hash()); got != opened || reported.Load() != uses {
		t.Errorf("rechecks recorded a use: %v to %v, %d reports to %d", opened, got, uses, reported.Load())
	}
	c.Advance(24 * time.Hour)
	if err := recheck(ctx); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("a recheck after the token expired: %v, want ErrInvalid", err)
	}
}

// TestSecretsDoNotPrint: a secret never shows in fmt or in an error.
func TestSecretsDoNotPrint(t *testing.T) {
	s := token.New(token.Service)
	err := fmt.Errorf("wrapped: %w", fmt.Errorf("token %v %s %q %+v %#v %x", s, s, s, s, s, s))
	for _, out := range []string{fmt.Sprint(s), err.Error(), fmt.Sprintf("%v", []token.Secret{s})} {
		if strings.Contains(out, s.Reveal()[len(token.Service):]) {
			t.Errorf("printed the secret: %s", out)
		}
	}
	if !strings.HasPrefix(fmt.Sprint(s), token.Service) {
		t.Errorf("the redacted form %q lost the prefix", fmt.Sprint(s))
	}
}
