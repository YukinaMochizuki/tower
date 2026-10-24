package auth_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/YukinaMochizuki/hangar/internal/auth"
)

// credential is a credential the test revokes, or makes uncheckable, at
// will; it authenticates as alice while it stands.
type credential struct {
	mu  sync.Mutex
	err error
	p   auth.Principal
}

var alice = auth.Principal{Type: auth.User, ID: "alice@example.com"}

func (c *credential) set(p auth.Principal, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.p, c.err = p, err
}

func (c *credential) Authenticate(context.Context, http.Header) (auth.Principal, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return auth.Principal{}, false, c.err
	}
	return c.p, true, nil
}

// decision is an authorization decision the test takes back at will.
type decision struct {
	mu  sync.Mutex
	err error
}

func (d *decision) set(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = err
}

func (d *decision) check(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// held holds a stream as alice inside a synctest bubble, with a decision
// registered, and releases it when the test ends.
func held(t *testing.T) (context.Context, *credential, *decision) {
	t.Helper()
	cred := &credential{p: alice}
	ctx, release, err := auth.Chain{cred}.Hold(t.Context(), http.Header{}, clockwork.NewRealClock())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	d := &decision{}
	auth.Recheck(ctx, d.check)
	return ctx, cred, d
}

// endedWithin checks that ctx's stream, refused just now, at a recheck,
// is still going a second before RecheckInterval has passed, and has ended
// with code once it has; it advances the bubble's virtual time by that.
func endedWithin(t *testing.T, ctx context.Context, code connect.Code) {
	t.Helper()
	d := auth.RecheckInterval
	time.Sleep(d - time.Second) //nolint:forbidigo // virtual time
	synctest.Wait()
	if ctx.Err() != nil {
		t.Fatalf("the stream ended before %v: %v", d, auth.Ended(ctx))
	}
	time.Sleep(time.Second) //nolint:forbidigo // virtual time
	synctest.Wait()
	if ctx.Err() == nil {
		t.Fatalf("the stream still goes after %v", d)
	}
	if got := connect.CodeOf(auth.Ended(ctx)); got != code {
		t.Errorf("the stream ended with %v (%v), want %v", got, auth.Ended(ctx), code)
	}
	if !errors.Is(context.Cause(ctx), auth.Ended(ctx)) {
		t.Errorf("the stream's context was cancelled with %v, not the refusal", context.Cause(ctx))
	}
}

func TestAHeldStreamEndsWithinARecheckOfItsCredentialsRevoke(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cred, _ := held(t)
		cred.set(auth.Principal{}, auth.ErrInvalidCredential)
		endedWithin(t, ctx, connect.CodeUnauthenticated)
	})
}

func TestAHeldStreamEndsWithItsDecision(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, _, d := held(t)
		// Some rounds pass with the decision standing.
		time.Sleep(3 * auth.RecheckInterval) //nolint:forbidigo // virtual time
		synctest.Wait()
		d.set(connect.NewError(connect.CodePermissionDenied, errors.New("no longer")))
		endedWithin(t, ctx, connect.CodePermissionDenied)
	})
}

// round advances virtual time to the next recheck, and lets it run.
func round() {
	time.Sleep(auth.RecheckInterval) //nolint:forbidigo // virtual time
	synctest.Wait()
}

// TestOneUndecidedRoundIsForgiven: a round that cannot decide, as when the
// store is unreachable, refuses nothing; a decided one after it starts the
// count again.
func TestOneUndecidedRoundIsForgiven(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cred, d := held(t)
		cred.set(auth.Principal{}, errors.New("database is locked"))
		round()
		cred.set(alice, nil)
		round()
		d.set(connect.NewError(connect.CodeUnavailable, errors.New("the authorization view is behind")))
		round()
		d.set(nil)
		round()
		if ctx.Err() != nil {
			t.Fatalf("undecided rounds, each followed by a decided one, ended the stream: %v", auth.Ended(ctx))
		}
	})
}

// TestTwoUndecidedRoundsEndTheStream: rounds that cannot decide, two in a
// row, end the stream as a refusal would, whether the credential or a
// decision could not be checked.
func TestTwoUndecidedRoundsEndTheStream(t *testing.T) {
	for name, undecided := range map[string]func(*credential, *decision){
		"credential": func(c *credential, _ *decision) { c.set(auth.Principal{}, errors.New("database is locked")) },
		"decision": func(_ *credential, d *decision) {
			d.set(connect.NewError(connect.CodeUnavailable, errors.New("the authorization view is behind")))
		},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cred, d := held(t)
				undecided(cred, d)
				round()
				if ctx.Err() != nil {
					t.Fatalf("one undecided round ended the stream: %v", auth.Ended(ctx))
				}
				round()
				if ctx.Err() == nil {
					t.Fatal("two undecided rounds in a row left the stream open")
				}
				if got := connect.CodeOf(auth.Ended(ctx)); got != connect.CodeUnauthenticated {
					t.Errorf("ended with %v (%v), want unauthenticated", got, auth.Ended(ctx))
				}
			})
		})
	}
}

// TestAStuckRoundIsUndecided: a round runs out at the next recheck, and
// counts as undecided, so a check that never answers ends the stream
// within three RecheckIntervals, whether it gives up when its context ends
// or not even then. A round that never answers keeps any other from
// starting beside it.
func TestAStuckRoundIsUndecided(t *testing.T) {
	for name, honoursContext := range map[string]bool{"gives up": true, "never answers": false} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, _, _ := held(t)
				unstuck := make(chan struct{})
				defer close(unstuck)
				var running, most atomic.Int32
				auth.Recheck(ctx, func(ctx context.Context) error {
					n := running.Add(1)
					defer running.Add(-1)
					if n > most.Load() {
						most.Store(n)
					}
					if honoursContext {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-unstuck:
						}
					}
					<-unstuck
					return nil
				})
				// The first round starts at one interval and runs out at two.
				time.Sleep(2*auth.RecheckInterval - time.Second) //nolint:forbidigo // virtual time
				synctest.Wait()
				if ctx.Err() != nil {
					t.Fatalf("the stream ended before its first round ran out: %v", auth.Ended(ctx))
				}
				time.Sleep(auth.RecheckInterval + time.Second) //nolint:forbidigo // virtual time
				synctest.Wait()
				if ctx.Err() == nil {
					t.Fatal("a stuck recheck left the stream open past three intervals")
				}
				if got := connect.CodeOf(auth.Ended(ctx)); got != connect.CodeUnauthenticated {
					t.Errorf("ended with %v (%v), want unauthenticated", got, auth.Ended(ctx))
				}
				if n := most.Load(); n != 1 {
					t.Errorf("%d rounds ran the check at once, want one", n)
				}
			})
		})
	}
}

// TestAHeldStreamIsItsPrincipalsAlone: a credential that comes to name
// someone else ends the stream.
func TestAHeldStreamIsItsPrincipalsAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cred, _ := held(t)
		cred.set(auth.Principal{Type: auth.User, ID: "bob@example.com"}, nil)
		endedWithin(t, ctx, connect.CodeUnauthenticated)
	})
}

// TestAReleasedHoldStopsChecking: releasing a hold ends its rechecks, which
// synctest.Test would otherwise find still running, and is no refusal.
func TestAReleasedHoldStopsChecking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cred := &credential{p: alice}
		ctx, release, err := auth.Chain{cred}.Hold(t.Context(), http.Header{}, clockwork.NewRealClock())
		if err != nil {
			t.Fatal(err)
		}
		checks := 0
		auth.Recheck(ctx, func(context.Context) error { checks++; return nil })
		time.Sleep(auth.RecheckInterval) //nolint:forbidigo // virtual time
		synctest.Wait()
		release()
		if auth.Ended(ctx) != nil || ctx.Err() == nil {
			t.Errorf("after release: ended %v, context %v", auth.Ended(ctx), ctx.Err())
		}
		time.Sleep(3 * auth.RecheckInterval) //nolint:forbidigo // virtual time
		if checks != 1 {
			t.Errorf("checked %d times, want once before the release", checks)
		}
	})
}

func TestHoldRefusesWhatAuthenticateDoes(t *testing.T) {
	cred := &credential{err: auth.ErrInvalidCredential}
	if _, _, err := (auth.Chain{cred}).Hold(t.Context(), http.Header{}, clockwork.NewFakeClock()); !errors.Is(err, auth.ErrInvalidCredential) {
		t.Errorf("holding a refused credential: %v", err)
	}
}

// TestRecheckOutsideAHoldIsNothing: a unary call registers its decision as a
// stream does, and nothing comes of it.
func TestRecheckOutsideAHoldIsNothing(t *testing.T) {
	ctx := auth.WithPrincipal(t.Context(), alice)
	auth.Recheck(ctx, func(context.Context) error { t.Error("checked outside a hold"); return nil })
	if auth.Ended(ctx) != nil {
		t.Error("a context with no hold ended")
	}
}

// streamCredential is a StreamAuthenticator that counts how often it is
// authenticated, as a use, and is rechecked by stands.
type streamCredential struct {
	credential
	uses   atomic.Int32
	stands decision
}

func (s *streamCredential) Authenticate(ctx context.Context, h http.Header) (auth.Principal, bool, error) {
	s.uses.Add(1)
	return s.credential.Authenticate(ctx, h)
}

func (s *streamCredential) AuthenticateStream(ctx context.Context, h http.Header) (auth.Principal, func(context.Context) error, bool, error) {
	p, ok, err := s.Authenticate(ctx, h)
	return p, s.stands.check, ok, err
}

// TestAStreamCredentialIsRecheckedWithoutUse: a credential that says how to
// recheck it is rechecked that way, never authenticated again as a use.
func TestAStreamCredentialIsRecheckedWithoutUse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cred := &streamCredential{credential: credential{p: alice}}
		ctx, release, err := auth.Chain{cred}.Hold(t.Context(), http.Header{}, clockwork.NewRealClock())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		round()
		round()
		if n := cred.uses.Load(); n != 1 {
			t.Errorf("authenticated %d times, want once, when the stream opened", n)
		}
		cred.stands.set(auth.ErrInvalidCredential)
		endedWithin(t, ctx, connect.CodeUnauthenticated)
	})
}

const streamProcedure = "/hangar.test.v1.Test/Stream"

// TestAStreamThatRegistersNoDecisionIsLogged: a held stream that answers
// without registering what it was let in under, which no recheck but its
// credential's could end, is logged; one that registers is not, nor one
// refused before it answered.
func TestAStreamThatRegistersNoDecisionIsLogged(t *testing.T) {
	var log syncBuffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	serve := func(register, answer bool) string {
		mux := http.NewServeMux()
		mux.Handle(streamProcedure, connect.NewServerStreamHandler(streamProcedure,
			func(ctx context.Context, _ *connect.Request[emptypb.Empty], stream *connect.ServerStream[emptypb.Empty]) error {
				if register {
					auth.Recheck(ctx, func(context.Context) error { return nil })
				}
				if !answer {
					return connect.NewError(connect.CodePermissionDenied, errors.New("no"))
				}
				return stream.Send(&emptypb.Empty{})
			}, connect.WithInterceptors(auth.Chain{auth.Dev{}}.InterceptorOn(clockwork.NewFakeClock(), logger))))
		srv := httptest.NewServer(mux)
		defer srv.Close()
		req := connect.NewRequest(&emptypb.Empty{})
		req.Header().Set(auth.DevUserHeader, "alice@example.com")
		stream, err := connect.NewClient[emptypb.Empty, emptypb.Empty](http.DefaultClient, srv.URL+streamProcedure).CallServerStream(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		for stream.Receive() {
		}
		_ = stream.Close()
		srv.Close() // the handler has returned, and logged
		out := log.String()
		log.Reset()
		return out
	}
	if out := serve(false, true); !strings.Contains(out, "level=WARN") || !strings.Contains(out, streamProcedure) || !strings.Contains(out, "user:alice@example.com") {
		t.Errorf("a stream that answered without registering a decision was not logged:\n%s", out)
	}
	if out := serve(true, true); out != "" {
		t.Errorf("a stream that registered its decision was logged:\n%s", out)
	}
	if out := serve(false, false); out != "" {
		t.Errorf("a stream refused before it answered was logged:\n%s", out)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.b.Reset()
}
