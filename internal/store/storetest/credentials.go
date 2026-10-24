package storetest

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/store"
)

// CredentialStore is a store.Credentials that can be closed.
type CredentialStore interface {
	store.Credentials
	Close() error
}

// RunCredentials runs the store.Credentials contract. open must return a new,
// empty store; RunCredentials closes it.
func RunCredentials(t *testing.T, open func(t *testing.T) CredentialStore) {
	tests := []struct {
		name string
		fn   func(t *testing.T, s store.Credentials)
	}{
		{"CredentialCreateGet", testCredentialCreateGet},
		{"CredentialTouch", testCredentialTouch},
		{"CredentialConsumedOnce", testCredentialConsumedOnce},
		{"CredentialRevoke", testCredentialRevoke},
		{"RevokedFamilyTakesNothing", testRevokedFamilyTakesNothing},
		{"ReplaceCredentials", testReplaceCredentials},
		{"ReplaceCredentialsLeavesOne", testReplaceCredentialsLeavesOne},
		{"CredentialsAllOrNone", testCredentialsAllOrNone},
		{"AuthRequests", testAuthRequests},
		{"AuthRequestTakenOnce", testAuthRequestTakenOnce},
		{"DeleteExpired", testDeleteExpired},
		{"FamilyHolds", testFamilyHolds},
		{"CountAuthRequests", testCountAuthRequests},
		{"AuthRequestsWithinLimits", testAuthRequestsWithinLimits},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := open(t)
			t.Cleanup(func() { _ = s.Close() })
			tt.fn(t, s)
		})
	}
}

// at is a fixed time at millisecond precision, which every store keeps.
var at = time.UnixMilli(1_800_000_000_000)

func credential(hash, family, principal string) store.Credential {
	return store.Credential{
		Hash: hash, Type: "refresh", Family: family, Principal: principal,
		CreatedAt: at, ExpiresAt: at.Add(time.Hour), IdleUntil: at.Add(time.Minute),
	}
}

func testCredentialCreateGet(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	c := credential("h1", "f1", "user:a@example.com")
	if err := s.CreateCredentials(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCredentials(ctx, c); !errors.Is(err, store.ErrExists) {
		t.Fatalf("second create: %v, want ErrExists", err)
	}
	got, err := s.GetCredential(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if !sameCredential(got, c) {
		t.Errorf("got %+v, want %+v", got, c)
	}
	if _, err := s.GetCredential(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing credential: %v, want ErrNotFound", err)
	}
	bound := credential("h2", "f1", "user:a@example.com")
	bound.Audience = "web-demo.example.dev"
	if err := s.CreateCredentials(ctx, bound); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetCredential(ctx, "h2"); err != nil || !sameCredential(got, bound) {
		t.Errorf("got %+v, %v; want %+v", got, err, bound)
	}
}

func testCredentialTouch(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	if err := s.CreateCredentials(ctx, credential("h1", "f1", "p")); err != nil {
		t.Fatal(err)
	}
	used, idle := at.Add(time.Second), at.Add(2*time.Minute)
	if err := s.TouchCredential(ctx, "h1", used, idle); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetCredential(ctx, "h1")
	if !got.LastUsedAt.Equal(used) || !got.IdleUntil.Equal(idle) {
		t.Errorf("after touch: last used %v idle %v", got.LastUsedAt, got.IdleUntil)
	}
	if err := s.TouchCredential(ctx, "nope", used, idle); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("touch a missing credential: %v, want ErrNotFound", err)
	}
}

// testCredentialConsumedOnce: of many concurrent consumers exactly one wins,
// and the rest see ErrConflict with the credential.
func testCredentialConsumedOnce(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	if err := s.CreateCredentials(ctx, credential("h1", "f1", "p")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for range 8 {
		wg.Go(func() {
			c, err := s.ConsumeCredential(ctx, "h1", at.Add(time.Second))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && c.UsedAt.IsZero():
				wins++
			case errors.Is(err, store.ErrConflict) && c.Family == "f1":
				conflicts++
			default:
				t.Errorf("consume: %+v, %v", c, err)
			}
		})
	}
	wg.Wait()
	if wins != 1 || conflicts != 7 {
		t.Errorf("%d consumed it and %d conflicted, want 1 and 7", wins, conflicts)
	}
	got, _ := s.GetCredential(ctx, "h1")
	if !got.UsedAt.Equal(at.Add(time.Second)) {
		t.Errorf("used at %v", got.UsedAt)
	}
	if _, err := s.ConsumeCredential(ctx, "nope", at); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("consume a missing credential: %v, want ErrNotFound", err)
	}
}

func testCredentialRevoke(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	for _, c := range []store.Credential{
		credential("a1", "fa", "user:a"), credential("a2", "fa", "user:a"),
		credential("a3", "fb", "user:a"), credential("b1", "fc", "user:b"),
	} {
		if err := s.CreateCredentials(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.RevokeCredentialFamily(ctx, "fa", at); err != nil || n != 2 {
		t.Fatalf("delete family: %d, %v", n, err)
	}
	if _, err := s.GetCredential(ctx, "a1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a revoked family's credential: %v", err)
	}
	if _, err := s.GetCredential(ctx, "a3"); err != nil {
		t.Errorf("another family went too: %v", err)
	}
	if n, err := s.RevokeCredentialsOf(ctx, "user:a", at); err != nil || n != 1 {
		t.Fatalf("delete principal's: %d, %v", n, err)
	}
	if _, err := s.GetCredential(ctx, "b1"); err != nil {
		t.Errorf("another principal's went too: %v", err)
	}
}

func testReplaceCredentials(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	for _, c := range []store.Credential{
		credential("a1", "fa", "serviceToken:p/a"), credential("a2", "fb", "serviceToken:p/a"),
		credential("b1", "fc", "serviceToken:p/b"),
	} {
		if err := s.CreateCredentials(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ReplaceCredentials(ctx, credential("a3", "fd", "serviceToken:p/a"), at); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"a1", "a2"} {
		if _, err := s.GetCredential(ctx, h); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("replaced credential %s: %v, want ErrNotFound", h, err)
		}
	}
	for _, h := range []string{"a3", "b1"} {
		if _, err := s.GetCredential(ctx, h); err != nil {
			t.Errorf("credential %s: %v", h, err)
		}
	}
	// The replaced families stay revoked: nothing is issued into them again.
	if err := s.CreateCredentials(ctx, credential("a4", "fa", "serviceToken:p/a")); !errors.Is(err, store.ErrRevoked) {
		t.Errorf("issue into a replaced family: %v, want ErrRevoked", err)
	}
	// Nor does a replacement go into one.
	if err := s.ReplaceCredentials(ctx, credential("a5", "fb", "serviceToken:p/a"), at); !errors.Is(err, store.ErrRevoked) {
		t.Errorf("replace into a revoked family: %v, want ErrRevoked", err)
	}
	if _, err := s.GetCredential(ctx, "a3"); err != nil {
		t.Errorf("a refused replacement took the current credential: %v", err)
	}
}

func testReplaceCredentialsLeavesOne(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := credential(string(rune('a'+i)), string(rune('A'+i)), "serviceToken:p/a")
			if err := s.ReplaceCredentials(ctx, c, at); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	left := 0
	for i := range n {
		if _, err := s.GetCredential(ctx, string(rune('a'+i))); err == nil {
			left++
		}
	}
	if left != 1 {
		t.Errorf("%d credentials left after concurrent replacements, want 1", left)
	}
}

func testAuthRequests(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	r := store.AuthRequest{Key: "k", Data: []byte("v1"), ExpiresAt: at}
	if err := s.CreateAuthRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAuthRequest(ctx, r); !errors.Is(err, store.ErrExists) {
		t.Fatalf("second create: %v, want ErrExists", err)
	}
	if err := s.SwapAuthRequest(ctx, "k", []byte("other"), []byte("v2")); !errors.Is(err, store.ErrConflict) {
		t.Errorf("swap from stale data: %v, want ErrConflict", err)
	}
	if err := s.SwapAuthRequest(ctx, "k", []byte("v1"), []byte("v2")); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAuthRequest(ctx, "k")
	if err != nil || string(got.Data) != "v2" || !got.ExpiresAt.Equal(at) {
		t.Errorf("after swap: %+v, %v", got, err)
	}
	if err := s.SwapAuthRequest(ctx, "nope", nil, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("swap a missing request: %v, want ErrNotFound", err)
	}
	if _, err := s.GetAuthRequest(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get a missing request: %v, want ErrNotFound", err)
	}
}

func testAuthRequestTakenOnce(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	if err := s.CreateAuthRequest(ctx, store.AuthRequest{Key: "k", Data: []byte("v"), ExpiresAt: at}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	took := 0
	for range 8 {
		wg.Go(func() {
			r, err := s.TakeAuthRequest(ctx, "k")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && string(r.Data) == "v":
				took++
			case errors.Is(err, store.ErrNotFound):
			default:
				t.Errorf("take: %+v, %v", r, err)
			}
		})
	}
	wg.Wait()
	if took != 1 {
		t.Errorf("%d took the request, want 1", took)
	}
}

func testDeleteExpired(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	live := credential("live", "f", "p")
	expired := credential("expired", "f", "p")
	expired.ExpiresAt = at
	idle := credential("idle", "f", "p")
	idle.IdleUntil = at
	noIdle := credential("noidle", "f", "p")
	noIdle.IdleUntil = time.Time{}
	for _, c := range []store.Credential{live, expired, idle, noIdle} {
		if err := s.CreateCredentials(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []store.AuthRequest{{Key: "old", ExpiresAt: at}, {Key: "new", ExpiresAt: at.Add(time.Second)}} {
		if err := s.CreateAuthRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteExpired(ctx, at); err != nil {
		t.Fatal(err)
	}
	for hash, want := range map[string]bool{"live": true, "expired": false, "idle": false, "noidle": true} {
		_, err := s.GetCredential(ctx, hash)
		if got := err == nil; got != want {
			t.Errorf("credential %s kept: %t, want %t (%v)", hash, got, want, err)
		}
	}
	if _, err := s.GetAuthRequest(ctx, "old"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired request kept: %v", err)
	}
	if _, err := s.GetAuthRequest(ctx, "new"); err != nil {
		t.Errorf("live request deleted: %v", err)
	}
}

func sameCredential(a, b store.Credential) bool {
	return a.Hash == b.Hash && a.Type == b.Type && a.Family == b.Family && a.Principal == b.Principal &&
		a.CreatedAt.Equal(b.CreatedAt) && a.ExpiresAt.Equal(b.ExpiresAt) && a.IdleUntil.Equal(b.IdleUntil) &&
		a.LastUsedAt.Equal(b.LastUsedAt) && a.UsedAt.Equal(b.UsedAt) && a.Audience == b.Audience
}

// testRevokedFamilyTakesNothing: once a family is revoked, by itself or with
// its principal's, nothing more is issued into it until the mark expires;
// other families are untouched.
func testRevokedFamilyTakesNothing(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	for _, c := range []store.Credential{credential("a1", "fa", "user:a"), credential("b1", "fb", "user:b")} {
		if err := s.CreateCredentials(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RevokeCredentialFamily(ctx, "fa", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCredentials(ctx, credential("a2", "fa", "user:a")); !errors.Is(err, store.ErrRevoked) {
		t.Errorf("issuing into a revoked family: %v, want ErrRevoked", err)
	}
	if _, err := s.RevokeCredentialsOf(ctx, "user:b", at); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCredentials(ctx, credential("b2", "fb", "user:b")); !errors.Is(err, store.ErrRevoked) {
		t.Errorf("issuing into a family revoked with its principal: %v, want ErrRevoked", err)
	}
	if err := s.CreateCredentials(ctx, credential("c1", "fc", "user:a")); err != nil {
		t.Errorf("a new family of a revoked principal: %v", err)
	}
	// The mark lasts as long as the family's last credential would have.
	if err := s.DeleteExpired(ctx, at.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCredentials(ctx, credential("a3", "fa", "user:a")); !errors.Is(err, store.ErrRevoked) {
		t.Errorf("the mark went before the family's credentials would have expired: %v", err)
	}
	if err := s.DeleteExpired(ctx, at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCredentials(ctx, credential("a4", "fa", "user:a")); err != nil {
		t.Errorf("an expired mark still refuses: %v", err)
	}
}

func testCredentialsAllOrNone(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	if err := s.CreateCredentials(ctx, credential("x", "f", "p")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCredentials(ctx, credential("y", "f", "p"), credential("x", "f", "p")); !errors.Is(err, store.ErrExists) {
		t.Fatalf("a batch with a taken hash: %v", err)
	}
	if _, err := s.GetCredential(ctx, "y"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("half a batch was stored: %v", err)
	}
}

func testCountAuthRequests(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	for i, exp := range []time.Time{at, at.Add(time.Minute), at.Add(time.Hour)} {
		if err := s.CreateAuthRequest(ctx, store.AuthRequest{Key: "a:" + string(rune('a'+i)), ExpiresAt: exp}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateAuthRequest(ctx, store.AuthRequest{Key: "b:x", ExpiresAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountAuthRequests(ctx, "a:", at); err != nil || n != 2 {
		t.Errorf("count a: %d, %v; want 2", n, err)
	}
	if n, err := s.CountAuthRequests(ctx, "b:", at); err != nil || n != 1 {
		t.Errorf("count b: %d, %v; want 1", n, err)
	}
}

// testAuthRequestsWithinLimits: requests are stored all or none, only while
// every limit has room as of now, expired ones not counting; of many
// concurrent calls under one limit, exactly as many as it allows get in.
func testAuthRequestsWithinLimits(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	req := func(key string, ttl time.Duration) store.AuthRequest {
		return store.AuthRequest{Key: key, Data: []byte("{}"), ExpiresAt: at.Add(ttl)}
	}
	limits := []store.AuthLimit{{Prefix: "a:", Max: 2}, {Prefix: "a:p1:", Max: 1}}
	if err := s.CreateAuthRequestsWithin(ctx, at, limits, req("a:p1:x", time.Minute), req("b:x", time.Minute)); err != nil {
		t.Fatal(err)
	}
	// p1 is at its limit: nothing of the call is stored.
	err := s.CreateAuthRequestsWithin(ctx, at, limits, req("a:p1:y", time.Minute), req("b:y", time.Minute))
	var le *store.LimitError
	if !errors.Is(err, store.ErrLimit) || !errors.As(err, &le) || le.Prefix != "a:p1:" {
		t.Fatalf("past a limit: %v, want ErrLimit for a:p1:", err)
	}
	if _, err := s.GetAuthRequest(ctx, "b:y"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a refused call stored part of itself: %v", err)
	}
	// A key taken refuses the whole call.
	if err := s.CreateAuthRequestsWithin(ctx, at, nil, req("c:x", time.Minute), req("b:x", time.Minute)); !errors.Is(err, store.ErrExists) {
		t.Fatalf("a key taken: %v, want ErrExists", err)
	}
	if _, err := s.GetAuthRequest(ctx, "c:x"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a refused call stored part of itself: %v", err)
	}
	// Once p1's expires, it counts no more.
	if err := s.CreateAuthRequestsWithin(ctx, at.Add(2*time.Minute), limits, req("a:p1:z", 3*time.Minute)); err != nil {
		t.Errorf("after the waiting one expired: %v", err)
	}

	// Concurrent calls under one limit: exactly Max get in.
	const max, callers = 5, 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	in, limited := 0, 0
	for i := range callers {
		wg.Go(func() {
			err := s.CreateAuthRequestsWithin(ctx, at, []store.AuthLimit{{Prefix: "race:", Max: max}},
				req(fmt.Sprintf("race:%d", i), time.Minute), req(fmt.Sprintf("race-data:%d", i), time.Minute))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				in++
			case errors.Is(err, store.ErrLimit):
				limited++
			default:
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if in != max || limited != callers-max {
		t.Errorf("%d concurrent calls under a limit of %d: %d in, %d refused", callers, max, in, limited)
	}
}

func testFamilyHolds(t *testing.T, s store.Credentials) {
	ctx := t.Context()
	holds := func(family, typ string, now time.Time) bool {
		t.Helper()
		ok, err := s.FamilyHolds(ctx, family, typ, now)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	spent := credential("spent", "f", "p")
	if err := s.CreateCredentials(ctx, spent, credential("other", "g", "p")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeCredential(ctx, "spent", at); err != nil {
		t.Fatal(err)
	}
	if !holds("f", "refresh", at) {
		t.Error("a family holding a consumed credential holds none")
	}
	if holds("f", "access", at) || holds("nope", "refresh", at) {
		t.Error("a family holds a type it has none of, or an unknown family holds one")
	}
	if holds("f", "refresh", at.Add(time.Minute)) {
		t.Error("a family whose only credential went idle still holds it")
	}
	if holds("g", "refresh", at.Add(time.Hour)) {
		t.Error("a family whose only credential expired still holds it")
	}
	if _, err := s.RevokeCredentialFamily(ctx, "g", at); err != nil {
		t.Fatal(err)
	}
	if holds("g", "refresh", at) {
		t.Error("a revoked family still holds a credential")
	}
}
