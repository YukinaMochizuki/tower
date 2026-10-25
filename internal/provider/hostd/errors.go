package hostd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/tower/internal/provider"
)

// errorKindKey is the metadata a HostService error names its provider
// error's kind in, so the client gets back the very error value the host's
// provider returned, which a code alone does not tell apart.
const errorKindKey = "Tower-Provider-Error"

// errorKinds are the provider errors a HostService error carries, each by
// its name on the wire and its Connect code.
var errorKinds = []struct {
	name string
	err  error
	code connect.Code
}{
	{"unavailable", provider.ErrUnavailable, connect.CodeUnavailable},
	{"not-found", provider.ErrNotFound, connect.CodeNotFound},
	{"no-space", provider.ErrNoSpace, connect.CodeResourceExhausted},
	{"failed", provider.ErrFailed, connect.CodeFailedPrecondition},
	{"lease-lost", provider.ErrLeaseLost, connect.CodeAborted},
	{"no-such-file", fs.ErrNotExist, connect.CodeNotFound},
}

// toConnect is what HostService answers a provider's error with. A call
// whose context ended is UNAVAILABLE: tower ending it does not read the
// answer, so only the host stopping does, and to tower that is a target
// that did not answer.
func toConnect(err error) error {
	var ce *connect.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ce):
		return err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		err = fmt.Errorf("the host ended the call: %w: %w", err, provider.ErrUnavailable)
	}
	for _, k := range errorKinds {
		if errors.Is(err, k.err) {
			ce := connect.NewError(k.code, err)
			ce.Meta().Set(errorKindKey, k.name)
			return ce
		}
	}
	return connect.NewError(connect.CodeUnknown, err)
}

// fromConnect is the provider's error a HostService error stands for: its
// message, wrapping the provider error its kind names.
func fromConnect(err error) error {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return err
	}
	kind := ce.Meta().Get(errorKindKey)
	for _, k := range errorKinds {
		if k.name == kind {
			return &hostError{msg: ce.Message(), err: k.err}
		}
	}
	switch ce.Code() {
	case connect.CodeUnavailable:
		return fmt.Errorf("%s: %w", ce.Message(), provider.ErrUnavailable)
	case connect.CodeCanceled:
		return fmt.Errorf("%s: %w", ce.Message(), context.Canceled)
	case connect.CodeDeadlineExceeded:
		return fmt.Errorf("%s: %w", ce.Message(), context.DeadlineExceeded)
	}
	return errors.New(ce.Message())
}

// hostError is a provider's error as the host said it: its message, which
// already says what err does, and err for errors.Is.
type hostError struct {
	msg string
	err error
}

func (e *hostError) Error() string { return e.msg }
func (e *hostError) Unwrap() error { return e.err }
