package apiserver_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	iamv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/iam/v1alpha1"
	tenancyv1alpha1 "github.com/YukinaMochizuki/tower/gen/go/tower/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/tower/internal/apiserver"
	"github.com/YukinaMochizuki/tower/internal/auth/token"
	"github.com/YukinaMochizuki/tower/internal/store"
	"github.com/YukinaMochizuki/tower/internal/testutil"
)

// wantAuthenticates fails the test unless the secret authenticates as want
// says.
func (e *env) wantAuthenticates(secret string, want bool, what string) {
	e.t.Helper()
	ok, err := e.authenticates(secret)
	if err != nil || ok != want {
		e.t.Errorf("%s: authenticates %t (%v), want %t", what, ok, err, want)
	}
}

// TestServiceTokenIsIssuedOnce: creating a token returns its secret once,
// which authenticates as the token and holds what its role grants, nothing
// else; nothing shows the secret again.
func TestServiceTokenIsIssuedOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.setup()
	res := must[iamv1alpha1.CreateServiceTokenResponse](t)(e.local().issuer.CreateServiceToken(ctx, createToken("deployer", iamv1alpha1.Role_ROLE_WORKSTREAM, "")))
	secret := res.GetSecret()
	if !strings.HasPrefix(secret, token.Service) || len(secret) != len(token.Service)+43 {
		t.Fatalf("secret %q is not twr_ and 32 random bytes", token.Secret(secret))
	}
	tok := res.GetServiceToken()
	st := tok.GetStatus()
	if st.GetSecretPrefix() != secret[:12] {
		t.Errorf("status.secret_prefix %q, want the secret's first 12 characters", st.GetSecretPrefix())
	}
	if got, want := st.GetExpireTime().AsTime().Sub(st.GetIssueTime().AsTime()), apiserver.DefaultTokenDuration; got != want {
		t.Errorf("the secret lasts %s, want %s", got, want)
	}
	if tok.GetSpec().GetRole() != iamv1alpha1.Role_ROLE_WORKSTREAM {
		t.Errorf("the token's spec: %v", tok.GetSpec())
	}

	// What its role grants, nothing else.
	w := e.withToken(secret)
	must[tenancyv1alpha1.GetProjectResponse](t)(w.projects.GetProject(ctx, getProject("web")))
	_, err := w.tokens.GetServiceToken(ctx, getToken("deployer"))
	wantCode(t, err, connect.CodePermissionDenied, "a workstream token reading tokens")
	_, err = w.issuer.CreateServiceToken(ctx, createToken("another", iamv1alpha1.Role_ROLE_WORKSTREAM, ""))
	wantCode(t, err, connect.CodePermissionDenied, "a workstream token making a token")

	// Nothing answers with the secret again.
	a := e.as(admin)
	got := must[iamv1alpha1.GetServiceTokenResponse](t)(a.tokens.GetServiceToken(ctx, getToken("deployer")))
	list := must[iamv1alpha1.ListServiceTokensResponse](t)(a.tokens.ListServiceTokens(ctx, connect.NewRequest(&iamv1alpha1.ListServiceTokensRequest{})))
	for _, m := range []proto.Message{got, list} {
		if b, _ := protojson.Marshal(m); bytes.Contains(b, []byte(secret[len(token.Service):])) {
			t.Errorf("an answer holds the secret: %T", m)
		}
	}

	// A name is one token's.
	_, err = a.issuer.CreateServiceToken(ctx, createToken("deployer", iamv1alpha1.Role_ROLE_WORKSTREAM, ""))
	wantCode(t, err, connect.CodeAlreadyExists, "creating a token of a name taken")
}

// TestServiceTokenSpecIsChecked: a token needs a role, and a target exactly
// when it is a hostd token's; the issuer refuses as apply does, and leaves
// no token behind.
func TestServiceTokenSpecIsChecked(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.as(admin)
	for _, tc := range []struct {
		role   iamv1alpha1.Role
		target string
		ok     bool
	}{
		{iamv1alpha1.Role_ROLE_UNSPECIFIED, "", false},
		{iamv1alpha1.Role(42), "", false},
		{iamv1alpha1.Role_ROLE_HOSTD, "", false},
		{iamv1alpha1.Role_ROLE_HOSTD, "Not_A_Name", false},
		{iamv1alpha1.Role_ROLE_ADMIN, "h1", false},
		{iamv1alpha1.Role_ROLE_WORKSTREAM, "h1", false},
		{iamv1alpha1.Role_ROLE_HOSTD, "h1", true},
		{iamv1alpha1.Role_ROLE_ADMIN, "", true},
		{iamv1alpha1.Role_ROLE_WORKSTREAM, "", true},
	} {
		what := fmt.Sprintf("role %v, target %q", tc.role, tc.target)
		_, err := a.tokens.ApplyServiceToken(ctx, connect.NewRequest(&iamv1alpha1.ApplyServiceTokenRequest{ServiceToken: serviceToken("applied", tc.role, tc.target)}))
		_, ierr := a.issuer.CreateServiceToken(ctx, createToken("issued", tc.role, tc.target))
		if tc.ok {
			if err != nil || ierr != nil {
				t.Errorf("%s: apply %v, issue %v", what, err, ierr)
			}
			for _, name := range []string{"applied", "issued"} {
				must[iamv1alpha1.DeleteServiceTokenResponse](t)(a.tokens.DeleteServiceToken(ctx, deleteToken(name)))
			}
			e.waitSettled()
			continue
		}
		wantCode(t, err, connect.CodeInvalidArgument, what+" by apply")
		wantCode(t, ierr, connect.CodeInvalidArgument, what+" by the issuer")
		_, err = a.tokens.GetServiceToken(ctx, getToken("issued"))
		wantCode(t, err, connect.CodeNotFound, what+": the refused token")
	}
	long := serviceToken("long", iamv1alpha1.Role_ROLE_ADMIN, "")
	long.Spec.Description = strings.Repeat("x", 257)
	_, err := a.tokens.ApplyServiceToken(ctx, connect.NewRequest(&iamv1alpha1.ApplyServiceTokenRequest{ServiceToken: long}))
	wantCode(t, err, connect.CodeInvalidArgument, "a description past 256 bytes")
}

// TestServiceTokenStoresOnlyTheHash greps every database file for the
// secret: it is not there, its SHA-256 is.
func TestServiceTokenStoresOnlyTheHash(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(e.local().issuer.CreateServiceToken(ctx, createToken("deployer", iamv1alpha1.Role_ROLE_WORKSTREAM, ""))).GetSecret()
	e.wantAuthenticates(secret, true, "the new secret")
	random := []byte(secret[len(token.Service):])
	hash := []byte(token.Hash(secret))
	var all []byte
	files, _ := filepath.Glob(filepath.Join(e.dbDir, "*"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, random) {
			t.Errorf("%s holds the secret", filepath.Base(f))
		}
		all = append(all, b...)
	}
	if !bytes.Contains(all, hash) {
		t.Errorf("no database file holds the secret's SHA-256, so the grep proves nothing; files %v", files)
	}
}

// TestServiceTokenRotate: a rotated token's old secret stops working at
// once, the new one works, and the status names the new one.
func TestServiceTokenRotate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.as(admin)
	first := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("deployer", iamv1alpha1.Role_ROLE_WORKSTREAM, ""))).GetSecret()
	rot := must[iamv1alpha1.RotateServiceTokenResponse](t)(a.issuer.RotateServiceToken(ctx, connect.NewRequest(&iamv1alpha1.RotateServiceTokenRequest{
		Name: "deployer", Duration: durationpb.New(2 * time.Hour),
	})))
	second := rot.GetSecret()
	if second == first {
		t.Fatal("rotating returned the same secret")
	}
	e.wantAuthenticates(first, false, "the secret rotated away")
	e.wantAuthenticates(second, true, "the new secret")
	st := rot.GetServiceToken().GetStatus()
	if st.GetSecretPrefix() != second[:12] || st.GetExpireTime().AsTime().Sub(st.GetIssueTime().AsTime()) != 2*time.Hour {
		t.Errorf("status after rotating: %v", st)
	}
	_, err := a.issuer.RotateServiceToken(ctx, rotateToken("nope"))
	wantCode(t, err, connect.CodeNotFound, "rotating a token that does not exist")
}

// TestServiceTokenMadeByApplyHasNoSecret: apply makes a token with nothing
// to authenticate with until it is rotated.
func TestServiceTokenMadeByApplyHasNoSecret(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.as(admin)
	tok := serviceToken("ci", iamv1alpha1.Role_ROLE_WORKSTREAM, "")
	tok.Spec.Description = "CI"
	res := must[iamv1alpha1.ApplyServiceTokenResponse](t)(a.tokens.ApplyServiceToken(ctx, connect.NewRequest(&iamv1alpha1.ApplyServiceTokenRequest{ServiceToken: tok})))
	if res.GetServiceToken().GetStatus().GetSecretPrefix() != "" {
		t.Errorf("a token made by apply has a secret: %v", res.GetServiceToken().GetStatus())
	}
	secret := must[iamv1alpha1.RotateServiceTokenResponse](t)(a.issuer.RotateServiceToken(ctx, rotateToken("ci"))).GetSecret()
	e.wantAuthenticates(secret, true, "the secret of a token made by apply")
}

// TestServiceTokenDelete: deleting a token refuses its secret at once, and
// frees its name for a token whose secret is its own.
func TestServiceTokenDelete(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.as(admin)
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("deployer", iamv1alpha1.Role_ROLE_ADMIN, ""))).GetSecret()
	must[iamv1alpha1.DeleteServiceTokenResponse](t)(a.tokens.DeleteServiceToken(ctx, deleteToken("deployer")))
	e.wantAuthenticates(secret, false, "a deleted token's secret, right after the delete")
	// Revoked by the delete itself, not only refused while the token goes.
	if _, err := e.store.GetCredential(ctx, token.Hash(secret)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a deleted token's credential, right after the delete: %v, want gone", err)
	}
	testutil.Eventually(t, "the token is gone", func() bool {
		_, err := a.tokens.GetServiceToken(ctx, getToken("deployer"))
		return connect.CodeOf(err) == connect.CodeNotFound
	})

	// A new token of the name, of a lesser role, takes up nothing of the
	// old one's: its old secret stays refused, and its own holds its role.
	again := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("deployer", iamv1alpha1.Role_ROLE_WORKSTREAM, ""))).GetSecret()
	e.wantAuthenticates(secret, false, "the old secret, once the name is a new token's")
	_, err := e.withToken(again).tokens.GetServiceToken(ctx, getToken(admin))
	wantCode(t, err, connect.CodePermissionDenied, "a new token of a deleted admin token's name")
}

// syncBuffer is a bytes.Buffer many goroutines may write.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestServiceTokenSecretsAreNeverLogged: the apiserver's log, at debug,
// across creating, rotating, using, refusing and deleting a token, holds
// none of its secrets.
func TestServiceTokenSecretsAreNeverLogged(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	e := newEnv(t, func(c *apiserver.Config) {
		c.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	ctx := t.Context()
	e.setup()
	a := e.as(admin)
	first := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("deployer", iamv1alpha1.Role_ROLE_WORKSTREAM, ""))).GetSecret()
	e.wantAuthenticates(first, true, "the first secret")
	second := must[iamv1alpha1.RotateServiceTokenResponse](t)(a.issuer.RotateServiceToken(ctx, rotateToken("deployer"))).GetSecret()
	must[tenancyv1alpha1.ApplyProjectResponse](t)(e.withToken(second).projects.ApplyProject(ctx, applyProject(project("site"))))
	e.wantAuthenticates(first, false, "the rotated-away secret")
	must[iamv1alpha1.DeleteServiceTokenResponse](t)(a.tokens.DeleteServiceToken(ctx, deleteToken("deployer")))
	e.wantAuthenticates(second, false, "a deleted token's secret")
	testutil.Eventually(t, "the token is gone", func() bool {
		_, err := a.tokens.GetServiceToken(ctx, getToken("deployer"))
		return connect.CodeOf(err) == connect.CodeNotFound
	})

	out := logs.String()
	if !strings.Contains(out, "created a service token") || !strings.Contains(out, "rotated a service token") {
		t.Fatalf("the log was not captured:\n%s", out)
	}
	for _, s := range []string{first, second} {
		if strings.Contains(out, s[len(token.Service):]) {
			t.Errorf("the log holds a secret:\n%s", out)
		}
	}
}

// failingRevoke is a credentials store whose RevokeCredentialsOf fails as
// many times as fails says.
type failingRevoke struct {
	store.Credentials
	fails atomic.Int32
}

func (f *failingRevoke) RevokeCredentialsOf(ctx context.Context, principal string, until time.Time) (int, error) {
	if f.fails.Add(-1) >= 0 {
		return 0, errors.New("injected: the revoke failed")
	}
	return f.Credentials.RevokeCredentialsOf(ctx, principal, until)
}

// TestAFailedRevokeLeavesNoSecret: when revoking a deleted token's secret
// fails, the secret still stops working, and a token made again under the
// name does not take it up.
func TestAFailedRevokeLeavesNoSecret(t *testing.T) {
	t.Parallel()
	creds := &failingRevoke{}
	e := newEnv(t, func(c *apiserver.Config) {
		creds.Credentials = c.Credentials
		c.Credentials = creds
	})
	ctx := t.Context()
	a := e.as(admin)
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("deployer", iamv1alpha1.Role_ROLE_ADMIN, ""))).GetSecret()
	e.wantAuthenticates(secret, true, "the new secret")

	creds.fails.Store(1)
	// The delete is stored; the revoke after it fails, and so may the call.
	_, _ = a.tokens.DeleteServiceToken(ctx, deleteToken("deployer"))
	e.wantAuthenticates(secret, false, "the deleted token's secret")

	// The same name again, applied with the same role.
	testutil.Eventually(t, "the name is free again", func() bool {
		_, err := a.tokens.ApplyServiceToken(ctx, connect.NewRequest(&iamv1alpha1.ApplyServiceTokenRequest{ServiceToken: serviceToken("deployer", iamv1alpha1.Role_ROLE_ADMIN, "")}))
		return err == nil
	})
	e.wantAuthenticates(secret, false, "the old secret, once the name is a new token's")
}

// lostRevoke is a credentials store whose RevokeCredentialsOf says it
// succeeded and keeps every credential, as a revoke lost by the store
// would.
type lostRevoke struct{ store.Credentials }

func (lostRevoke) RevokeCredentialsOf(context.Context, string, time.Time) (int, error) { return 0, nil }

// TestASecretNeverPassesToALaterTokenOfItsName: a secret whose revoke was
// lost, its credential still stored, authenticates nothing once its token
// is gone, and nothing for a token made again under the same name.
func TestASecretNeverPassesToALaterTokenOfItsName(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *apiserver.Config) { c.Credentials = lostRevoke{c.Credentials} })
	ctx := t.Context()
	a := e.as(admin)
	old := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("deployer", iamv1alpha1.Role_ROLE_ADMIN, ""))).GetSecret()
	must[iamv1alpha1.DeleteServiceTokenResponse](t)(a.tokens.DeleteServiceToken(ctx, deleteToken("deployer")))
	testutil.Eventually(t, "the token is gone", func() bool {
		_, err := a.tokens.GetServiceToken(ctx, getToken("deployer"))
		return connect.CodeOf(err) == connect.CodeNotFound
	})
	if _, err := e.store.GetCredential(ctx, token.Hash(old)); err != nil {
		t.Fatalf("the old credential is not kept, so the test proves nothing: %v", err)
	}
	e.wantAuthenticates(old, false, "the secret of a token gone")

	// The name again, by apply, which issues no secret of its own.
	must[iamv1alpha1.ApplyServiceTokenResponse](t)(a.tokens.ApplyServiceToken(ctx, connect.NewRequest(&iamv1alpha1.ApplyServiceTokenRequest{
		ServiceToken: serviceToken("deployer", iamv1alpha1.Role_ROLE_ADMIN, ""),
	})))
	e.wantAuthenticates(old, false, "the old secret, once the name is a new token's")
}

// TestFailingRevokesKeepTheToken: while every revoke of a deleted token's
// secret fails, the token stays, being deleted, and its secret is refused;
// once revokes work again, the next periodic sweep lets it go.
func TestFailingRevokesKeepTheToken(t *testing.T) {
	t.Parallel()
	creds := &failingRevoke{}
	e := newEnv(t, func(c *apiserver.Config) {
		creds.Credentials = c.Credentials
		c.Credentials = creds
	})
	ctx := t.Context()
	a := e.as(admin)
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("deployer", iamv1alpha1.Role_ROLE_WORKSTREAM, ""))).GetSecret()

	creds.fails.Store(1 << 30)
	_, _ = a.tokens.DeleteServiceToken(ctx, deleteToken("deployer"))
	// The collector has tried, and could not let the token go.
	e.waitSettled()
	got, err := a.tokens.GetServiceToken(ctx, getToken("deployer"))
	if err != nil {
		t.Fatalf("the token went while its secret could not be revoked: %v", err)
	}
	if got.Msg.GetServiceToken().GetMetadata().GetDeleteTime() == nil {
		t.Error("the token is not being deleted")
	}
	e.wantAuthenticates(secret, false, "the secret of a token being deleted, never revoked")

	creds.fails.Store(0)
	e.clock.Advance(5 * time.Minute) // the sweep interval
	testutil.Eventually(t, "the token goes once its secret is revoked", func() bool {
		_, err := a.tokens.GetServiceToken(ctx, getToken("deployer"))
		return connect.CodeOf(err) == connect.CodeNotFound
	})
	e.wantAuthenticates(secret, false, "the secret once the token is gone")
}

// TestRotateRacingDeleteLeavesNoSecret: rotations racing the token's delete
// leave no secret that works, whichever lands first.
func TestRotateRacingDeleteLeavesNoSecret(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.as(admin)
	for round := range 3 {
		name := fmt.Sprintf("racer-%d", round)
		first := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken(name, iamv1alpha1.Role_ROLE_WORKSTREAM, ""))).GetSecret()
		var mu sync.Mutex
		secrets := []string{first}
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				res, err := a.issuer.RotateServiceToken(ctx, rotateToken(name))
				if err == nil {
					mu.Lock()
					secrets = append(secrets, res.Msg.GetSecret())
					mu.Unlock()
				}
			})
		}
		if _, err := a.tokens.DeleteServiceToken(ctx, deleteToken(name)); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		for _, s := range secrets {
			e.wantAuthenticates(s, false, "a secret of a deleted token, issued around its delete")
		}
	}
}

// TestServiceTokenExpires: a secret works until its duration is up, and the
// duration is bounded.
func TestServiceTokenExpires(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.as(admin)
	for _, d := range []time.Duration{59 * time.Minute, apiserver.MaxTokenDuration + time.Hour} {
		_, err := a.issuer.CreateServiceToken(ctx, connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{Name: "short", Role: iamv1alpha1.Role_ROLE_WORKSTREAM, Duration: durationpb.New(d)}))
		wantCode(t, err, connect.CodeInvalidArgument, "a duration of "+d.String())
	}
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, connect.NewRequest(&iamv1alpha1.CreateServiceTokenRequest{
		Name: "short", Role: iamv1alpha1.Role_ROLE_WORKSTREAM, Duration: durationpb.New(time.Hour),
	}))).GetSecret()
	e.wantAuthenticates(secret, true, "a secret within its hour")
	e.clock.Advance(time.Hour)
	e.wantAuthenticates(secret, false, "an expired secret")
}

// TestUnknownTokensAreRefusedAlike: an unknown, a malformed and another
// prefix's secret are refused the same way, and no refusal quotes it.
func TestUnknownTokensAreRefusedAlike(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, s := range []string{token.New(token.Service).Reveal(), "twr_", "twr_short", "hgr_" + token.Random()} {
		_, err := e.withToken(s).projects.ListProjects(t.Context(), connect.NewRequest(&tenancyv1alpha1.ListProjectsRequest{}))
		wantCode(t, err, connect.CodeUnauthenticated, "a made-up token")
		if err != nil && len(s) > 8 && strings.Contains(err.Error(), s) {
			t.Errorf("the refusal holds the token: %v", err)
		}
	}
}

// TestServiceTokenLastUse: a use shows in the token's status, at most one
// write an hour.
func TestServiceTokenLastUse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	a := e.as(admin)
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(a.issuer.CreateServiceToken(ctx, createToken("deployer", iamv1alpha1.Role_ROLE_WORKSTREAM, ""))).GetSecret()
	lastUse := func(want time.Time) func() (bool, string) {
		return func() (bool, string) {
			tok := must[iamv1alpha1.GetServiceTokenResponse](t)(a.tokens.GetServiceToken(ctx, getToken("deployer"))).GetServiceToken()
			got := tok.GetStatus().GetLastUseTime().AsTime()
			return got.Equal(want), fmt.Sprintf("last use %v, want %v", got, want)
		}
	}
	e.wantAuthenticates(secret, true, "the first use")
	first := e.clock.Now()
	testutil.Eventually(t, "status.last_use_time is set", lastUse(first))
	rev, err := e.store.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// A use within the hour writes nothing; one past it does.
	e.clock.Advance(30 * time.Minute)
	e.wantAuthenticates(secret, true, "a use within the hour")
	e.clock.Advance(31 * time.Minute)
	e.wantAuthenticates(secret, true, "a use past the hour")
	third := e.clock.Now()
	testutil.Eventually(t, "status.last_use_time moves past the hour", lastUse(third))
	// Uses are recorded in order, so a write of the one within the hour
	// would come before this one.
	var seen []time.Time
	stop := errors.New("seen")
	err = e.store.Watch(ctx, store.WatchOptions{Kind: "ServiceToken", SinceRevision: rev}, func(ev store.Event) error {
		var tok iamv1alpha1.ServiceToken
		if err := proto.Unmarshal(ev.Object.Data, &tok); err != nil {
			return err
		}
		if tok.GetMetadata().GetName() != "deployer" {
			return nil
		}
		at := tok.GetStatus().GetLastUseTime().AsTime()
		seen = append(seen, at)
		if at.Equal(third) {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Errorf("last use written at %v, want only at %v", seen, third)
	}
}

// TestRootOnTheHostMakesTheFirstAdmin: with no token at all, root on the
// host makes an admin token whose secret then holds the admin role.
func TestRootOnTheHostMakesTheFirstAdmin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	secret := must[iamv1alpha1.CreateServiceTokenResponse](t)(e.local().issuer.CreateServiceToken(ctx, createToken("first", iamv1alpha1.Role_ROLE_ADMIN, ""))).GetSecret()
	c := e.withToken(secret)
	must[iamv1alpha1.CreateServiceTokenResponse](t)(c.issuer.CreateServiceToken(ctx, createToken("second", iamv1alpha1.Role_ROLE_WORKSTREAM, "")))
	tok := must[iamv1alpha1.GetServiceTokenResponse](t)(c.tokens.GetServiceToken(ctx, getToken("second"))).GetServiceToken()
	if tok.GetSpec().GetRole() != iamv1alpha1.Role_ROLE_WORKSTREAM || tok.GetMetadata().GetName() != "second" {
		t.Errorf("the token the first admin made: %v", tok)
	}
}
