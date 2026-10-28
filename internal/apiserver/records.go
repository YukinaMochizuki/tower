package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/YukinaMochizuki/tower/internal/store"
)

// tower's internal records: JSON kept in the store under kinds the API
// never serves or watches, so what they hold reaches no client, hostd
// included, and survives a restart.
const (
	// goldenBuildKind holds, per golden by its project and name, what its
	// build reads besides its inputs (buildSpec).
	goldenBuildKind = "GoldenBuild"
	// goldenBackoffKind holds, per project and input hash, how a golden of
	// those inputs failed and when it is built again (goldenBackoff).
	goldenBackoffKind = "GoldenBackoff"
	// warmSizeKind holds, per repository, the size of the last warm
	// golden tower saw of it (warmSizeRecord).
	warmSizeKind = "WarmSize"
	// warmInputsKind holds, per repository whose branch head's pre-build
	// inputs differ from its warm golden's, since when the head has had them
	// (warmInputsRecord).
	warmInputsKind = "WarmInputs"
	// stepBackoffKind holds, per box or machine and instance step, how the
	// step failed and when it runs again (stepBackoff).
	stepBackoffKind = "StepBackoff"
)

// getRecord reads the record of key into v, reporting whether there is
// one.
func (s *Server) getRecord(ctx context.Context, key store.Key, v any) (bool, error) {
	o, err := s.store.Get(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(o.Data, v)
}

// putRecord writes v as the record of key, whether or not there is one.
func (s *Server) putRecord(ctx context.Context, key store.Key, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	for range maxRetries {
		o, err := s.store.Get(ctx, key)
		switch {
		case errors.Is(err, store.ErrNotFound):
			if _, err := s.store.Create(ctx, key, data); !errors.Is(err, store.ErrExists) {
				return err
			}
		case err != nil:
			return err
		case string(o.Data) == string(data):
			return nil
		default:
			if _, err := s.store.Update(ctx, key, data, o.Revision); !errors.Is(err, store.ErrConflict) {
				return err
			}
		}
	}
	return fmt.Errorf("too many writers of %s %s/%s", key.Kind, key.Project, key.Name)
}

// deleteRecord deletes the record of key, if there is one.
func (s *Server) deleteRecord(ctx context.Context, key store.Key) error {
	for range maxRetries {
		o, err := s.store.Get(ctx, key)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, _, err := s.store.Delete(ctx, key, o.Revision); !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return fmt.Errorf("too many writers of %s %s/%s", key.Kind, key.Project, key.Name)
}

// goldenBackoff is how the goldens of one input hash failed.
type goldenBackoff struct {
	// Failures in a row since one last succeeded.
	Failures int       `json:"failures"`
	RetryAt  time.Time `json:"retryAt"`
	// Reason is the last failure's, and Golden the uid of the golden that
	// failed, counted once.
	Reason string `json:"reason"`
	Golden string `json:"golden"`
	// FailedAt is when the last failure was counted; zero in a record
	// written before tower kept it.
	FailedAt time.Time `json:"failedAt,omitzero"`
}

// message is the failure as a golden, and what waits on it, say it: why,
// when it failed, and when it is built again.
func (b goldenBackoff) message() string {
	return failureMessage(b.Reason, b.FailedAt, "built again at", b.RetryAt)
}

// warmSizeRecord is a repository's last warm golden's size.
type warmSizeRecord struct {
	Bytes uint64 `json:"bytes"`
}

// warmInputsRecord is since when a repository's branch head has had the
// pre-build inputs Inputs names.
type warmInputsRecord struct {
	Inputs string    `json:"inputs"`
	Since  time.Time `json:"since"`
}
