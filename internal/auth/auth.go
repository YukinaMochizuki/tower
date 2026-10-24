// Package auth turns a request into a Principal: who is calling. Each listener
// runs its own chain of authenticators; see docs/decisions/authentication.md.
// Deciding what a principal may do is internal/authz's job.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"

	"connectrpc.com/connect"
	"github.com/jonboulle/clockwork"
)

// Type is the kind of identity behind a principal.
type Type int

const (
	// User is a person, identified by an email their identity provider
	// verified.
	User Type = iota + 1
	// ServiceToken is an agent, identified by its service token.
	ServiceToken
	// Node is a hangar-node process.
	Node
	// System is the apiserver itself, acting through its controllers.
	System
	// Local is root on the host, through the admin socket. It holds
	// platform.root without a RoleBinding, and no RoleBinding can name it.
	Local
	// Edge is a hangar-edge process, through the edge socket. It reads the
	// public endpoints and what decides who may use them, nothing else.
	Edge
)

func (t Type) String() string {
	switch t {
	case User:
		return "user"
	case ServiceToken:
		return "serviceToken"
	case Node:
		return "node"
	case System:
		return "system"
	case Local:
		return "local"
	case Edge:
		return "edge"
	}
	return fmt.Sprintf("Type(%d)", int(t))
}

// Principal is an authenticated caller.
type Principal struct {
	Type Type
	ID   string
}

func (p Principal) String() string { return p.Type.String() + ":" + p.ID }

// ParsePrincipal reads what Principal.String wrote.
func ParsePrincipal(s string) (Principal, error) {
	name, id, ok := strings.Cut(s, ":")
	if ok && id != "" {
		for t := User; t <= Edge; t++ {
			if t.String() == name {
				return Principal{Type: t, ID: id}, nil
			}
		}
	}
	return Principal{}, fmt.Errorf("%q is not a principal", s)
}

// SystemPrincipal is the identity in-process controllers act as.
var SystemPrincipal = Principal{Type: System, ID: "apiserver"}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the principal the request was authenticated as.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Authenticator recognizes one kind of credential. It returns ok false when
// the request carries none of its credentials, and an error when it carries
// some that are invalid.
type Authenticator interface {
	Authenticate(ctx context.Context, header http.Header) (p Principal, ok bool, err error)
}

// ErrNoCredentials is returned when no authenticator in the chain recognized
// the request.
var ErrNoCredentials = errors.New("no credentials")

// Chain tries authenticators in order; the first that recognizes the request
// decides it.
type Chain []Authenticator

// Authenticate returns a context carrying the request's principal.
func (c Chain) Authenticate(ctx context.Context, header http.Header) (context.Context, error) {
	for _, a := range c {
		p, ok, err := a.Authenticate(ctx, header)
		if err != nil {
			return ctx, err
		}
		if ok {
			return WithPrincipal(ctx, p), nil
		}
	}
	return ctx, ErrNoCredentials
}

// Interceptor authenticates every incoming Connect call with the chain and
// rejects calls it cannot authenticate. It holds every streaming call as
// Hold does, on the real clock, and logs to the default logger.
func (c Chain) Interceptor() connect.Interceptor {
	return c.InterceptorOn(clockwork.NewRealClock(), slog.Default())
}

// InterceptorOn is Interceptor with streaming calls rechecked on clock, and
// log told of a stream that sent something without registering a decision
// to recheck (Recheck): one that only its credential ends.
func (c Chain) InterceptorOn(clock clockwork.Clock, log *slog.Logger) connect.Interceptor {
	return interceptor{chain: c, clock: clock, log: log}
}

type interceptor struct {
	chain Chain
	clock clockwork.Clock
	log   *slog.Logger
}

func (i interceptor) authenticate(ctx context.Context, header http.Header) (context.Context, error) {
	ctx, err := i.chain.Authenticate(ctx, header)
	if err != nil {
		return ctx, connect.NewError(connect.CodeUnauthenticated, err)
	}
	return ctx, nil
}

func (i interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().IsClient {
			return next(ctx, req)
		}
		ctx, err := i.authenticate(ctx, req.Header())
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, release, err := i.chain.Hold(ctx, conn.RequestHeader(), i.clock)
		if err != nil {
			return connect.NewError(connect.CodeUnauthenticated, err)
		}
		defer release()
		sent := &sendTracker{StreamingHandlerConn: conn}
		err = next(ctx, sent)
		if sent.sent.Load() && !registered(ctx) {
			p, _ := FromContext(ctx)
			i.log.Warn("a stream answered without registering the decisions it was let in under, so no recheck could end it but its credential's",
				"procedure", conn.Spec().Procedure, "principal", p.String())
		}
		// Whatever the handler made of its context ending, the caller is told
		// why it did.
		if refused := Ended(ctx); refused != nil {
			return refused
		}
		return err
	}
}

// sendTracker records whether a stream sent anything.
type sendTracker struct {
	connect.StreamingHandlerConn
	sent atomic.Bool
}

func (s *sendTracker) Send(msg any) error {
	s.sent.Store(true)
	return s.StreamingHandlerConn.Send(msg)
}
