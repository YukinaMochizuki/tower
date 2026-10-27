package hangar_test

import (
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"

	"github.com/YukinaMochizuki/tower/internal/provider/hangar"
)

// TestOnlyHangarsAliasHeldWaits: an apply hangar refuses as its alias is
// held waits; another failed precondition, even one naming an alias,
// fails. The refusal is built from hangar's own format (hangar:
// internal/apiserver/aliases.go, checkAliasesFree), and reaches the
// provider through Connect as apiError says it.
func TestOnlyHangarsAliasHeldWaits(t *testing.T) {
	refusal := func(format string, args ...any) error {
		ce := connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(format, args...))
		return fmt.Errorf("applying a container: %w", ce)
	}
	held := refusal("alias %q is held on network %q by %s", "box", "w-web-ws1-d77c84c9b14a", "container t-01a17aa4-26d8-772d-986c-a21743a5041e")
	if reason, waits := hangar.ApplyRefused(held); reason != "AliasHeld" || !waits {
		t.Errorf("hangar's alias held: %s, waits %v; want AliasHeld, waiting", reason, waits)
	}
	for _, err := range []error{
		refusal("disk %q is not there", "alias"),
		refusal("spec.interfaces[0].aliases: %q twice", "box"),
		connect.NewError(connect.CodeInvalidArgument, errors.New(`alias "box" is held on network "n" by container c`)),
	} {
		if reason, waits := hangar.ApplyRefused(err); reason == "AliasHeld" || waits {
			t.Errorf("%v: %s, waits %v; want it failed", err, reason, waits)
		}
	}
}
