package apiserver

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/tower/internal/store"
)

// apiError maps store errors to Connect codes and passes Connect errors
// through; anything else is internal.
func apiError(err error) error {
	var ce *connect.Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ce):
		return err
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, store.ErrExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, store.ErrConflict):
		return connect.NewError(connect.CodeAborted, err)
	case errors.Is(err, store.ErrCompacted):
		return connect.NewError(connect.CodeOutOfRange, fmt.Errorf("%w; list again and watch from the list's revision", err))
	}
	return connect.NewError(connect.CodeInternal, err)
}

// messageOf is what an error says, without the code a Connect error
// prefixes its message with.
func messageOf(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}

func invalid(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

func failedPrecondition(format string, args ...any) error {
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(format, args...))
}

func denied(format string, args ...any) error {
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf(format, args...))
}
