package hangar

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/YukinaMochizuki/tower/internal/provider"
)

// TestGuestErrorKeepsADirectory: a read of a directory stays told as one
// through the provider (syscall.EISDIR), as well as failed.
func TestGuestErrorKeepsADirectory(t *testing.T) {
	t.Parallel()
	err := guestError(fmt.Errorf("reading /workspace/web: /workspace/web is a directory: %w", syscall.EISDIR))
	if !errors.Is(err, syscall.EISDIR) || !errors.Is(err, provider.ErrFailed) {
		t.Errorf("guestError of a directory read: %v", err)
	}
}
