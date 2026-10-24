package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
)

// RecheckInterval is how often a long-lived stream checks again the
// credential it was opened with and the decisions it was authorized under,
// and so how long it outlives their revoke while every check can be made;
// see docs/decisions/authentication.md.
const RecheckInterval = 30 * time.Second

// undecidedLimit is how many rounds in a row may fail to decide before a
// stream is ended as though refused, so a revoke the store cannot confirm
// ends it within about three RecheckIntervals.
const undecidedLimit = 2

// ErrInvalidCredential means a credential is unknown, revoked, expired or
// not of the kind asked for. An authenticator returns it, wrapped or not,
// for a credential it knows to be refused, as against one it could not
// check, such as when its store is unreachable.
var ErrInvalidCredential = errors.New("the credential is not valid; sign in again")

// StreamAuthenticator is an Authenticator whose credentials can be checked
// again for as long as a stream lasts. recheck says whether the credential
// still stands, without counting as a use of it: nil if it does, an error
// wrapping ErrInvalidCredential if it is known not to, any other error if
// that could not be told.
type StreamAuthenticator interface {
	Authenticator
	AuthenticateStream(ctx context.Context, header http.Header) (p Principal, recheck func(context.Context) error, ok bool, err error)
}

// streamCredentialRefused ends a stream whose credential was refused at a
// recheck.
func streamCredentialRefused() error {
	return connect.NewError(connect.CodeUnauthenticated,
		errors.New("the credential this stream was opened with is no longer valid; sign in again"))
}

// streamUndecided ends a stream whose rechecks could not decide, for
// undecidedLimit rounds in a row. Unauthenticated, not Unavailable, which a
// client would take for a dropped connection.
func streamUndecided() error {
	return connect.NewError(connect.CodeUnauthenticated,
		errors.New("this stream's credential and authorization could not be checked again; open it again"))
}

type holdKey struct{}

// hold is what a long-lived stream is rechecked against.
type hold struct {
	p Principal
	// credential checks again the credential the stream was opened with.
	credential func(context.Context) error
	clock      clockwork.Clock

	mu     sync.Mutex
	checks []func(context.Context) error
	ended  error
	// pending is a round that ran past its deadline and has not answered
	// yet; no other starts until it has.
	pending chan roundResult
}

type roundResult struct {
	refusal error
	decided bool
}

// Hold authenticates a long-lived stream as Authenticate does, and checks it
// again every RecheckInterval on clock until release is called: its
// credential, without counting that as a use of it, and every decision
// Recheck registered for it. Once either is refused, the returned context is
// cancelled, and Ended returns the refusal: an Unauthenticated or
// PermissionDenied error, which the stream ends with. A round that cannot
// decide, because the store is unreachable, a check fails otherwise, or the
// round runs past RecheckInterval, refuses nothing; undecidedLimit of them in
// a row end the stream with Unauthenticated.
func (c Chain) Hold(ctx context.Context, header http.Header, clock clockwork.Clock) (_ context.Context, release func(), _ error) {
	p, credential, err := c.authenticateStream(ctx, header)
	if err != nil {
		return ctx, func() {}, err
	}
	h := &hold{p: p, credential: credential, clock: clock}
	ctx, cancel := context.WithCancelCause(context.WithValue(WithPrincipal(ctx, p), holdKey{}, h))
	// Made before Hold returns, so a stream the caller has seen open is
	// already timed.
	ticker := clock.NewTicker(RecheckInterval)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ticker.Stop()
		undecided := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.Chan():
			}
			r := h.next(ctx)
			refusal := r.refusal
			switch {
			case refusal != nil:
			case !r.decided:
				if undecided++; undecided >= undecidedLimit {
					refusal = streamUndecided()
				}
			default:
				undecided = 0
			}
			if refusal != nil && ctx.Err() == nil {
				h.mu.Lock()
				h.ended = refusal
				h.mu.Unlock()
				cancel(refusal)
				return
			}
		}
	}()
	return ctx, func() { cancel(nil); <-done }, nil
}

// authenticateStream is Authenticate for a stream: the principal, and how to
// check its credential again.
func (c Chain) authenticateStream(ctx context.Context, header http.Header) (Principal, func(context.Context) error, error) {
	header = header.Clone()
	for _, a := range c {
		if s, ok := a.(StreamAuthenticator); ok {
			p, recheck, ok, err := s.AuthenticateStream(ctx, header)
			if err != nil {
				return Principal{}, nil, err
			}
			if ok {
				return p, recheck, nil
			}
			continue
		}
		p, ok, err := a.Authenticate(ctx, header)
		if err != nil {
			return Principal{}, nil, err
		}
		if ok {
			// Authenticators that keep no credentials of their own, such as
			// a socket's peer, are asked again as they were.
			return p, func(ctx context.Context) error {
				q, ok, err := a.Authenticate(ctx, header)
				switch {
				case err != nil:
					return err
				case !ok || q != p:
					return ErrInvalidCredential
				}
				return nil
			}, nil
		}
	}
	return Principal{}, nil, ErrNoCredentials
}

// next runs a round, given until RecheckInterval has passed. A round that
// takes longer is undecided, and so is every later one until it answers;
// none starts beside it. Neither next nor Hold's release waits for such a
// round: a check that never returns would hold the stream's handler for
// good. Its goroutine ends when its check does, and its answer is dropped.
func (h *hold) next(ctx context.Context) roundResult {
	if h.pending != nil {
		select {
		case <-h.pending:
			h.pending = nil
		default:
			return roundResult{}
		}
	}
	rctx, cancel := clockwork.WithTimeout(ctx, h.clock, RecheckInterval)
	defer cancel()
	out := make(chan roundResult, 1)
	go func() { out <- h.round(rctx) }()
	select {
	case r := <-out:
		return r
	case <-rctx.Done():
		// An answer that came with the deadline counts.
		select {
		case r := <-out:
			return r
		default:
		}
		h.pending = out
		return roundResult{}
	}
}

// round checks the credential and every registered decision once.
func (h *hold) round(ctx context.Context) roundResult {
	switch err := h.credential(ctx); {
	case errors.Is(err, ErrInvalidCredential), errors.Is(err, ErrNoCredentials):
		return roundResult{refusal: streamCredentialRefused()}
	case err != nil:
		return roundResult{}
	}
	h.mu.Lock()
	checks := h.checks
	h.mu.Unlock()
	decided := true
	for _, check := range checks {
		switch err := check(ctx); connect.CodeOf(err) {
		case connect.CodePermissionDenied, connect.CodeUnauthenticated:
			return roundResult{refusal: err}
		default:
			if err != nil {
				decided = false
			}
		}
	}
	return roundResult{decided: decided && ctx.Err() == nil}
}

// Recheck registers check to be asked again at every recheck of the stream
// ctx belongs to; outside a stream Hold keeps, it does nothing. check is a
// decision the stream was authorized under, made again: it returns nil while
// the answer is yes, a PermissionDenied or Unauthenticated error once it is
// no, and any other error when it cannot tell.
func Recheck(ctx context.Context, check func(context.Context) error) {
	h, ok := ctx.Value(holdKey{}).(*hold)
	if !ok {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checks = append(h.checks, check)
}

// Ended returns why the stream ctx belongs to was ended by a recheck, and
// nil while it has not been.
func Ended(ctx context.Context) error {
	h, ok := ctx.Value(holdKey{}).(*hold)
	if !ok {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ended
}

// registered reports whether a decision was registered for the stream ctx
// belongs to.
func registered(ctx context.Context) bool {
	h, ok := ctx.Value(holdKey{}).(*hold)
	if !ok {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.checks) > 0
}
