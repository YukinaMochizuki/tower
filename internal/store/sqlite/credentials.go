package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite/db"
)

var _ store.Credentials = (*Store)(nil)

// Credentials and auth requests are written on the write connection like
// objects, but outside s.mu: they take no revision and no event, so they
// need no ordering with object writes.

func (s *Store) CreateCredentials(ctx context.Context, cs ...store.Credential) error {
	return s.credTx(ctx, func(q *db.Queries) error {
		return createCredentials(ctx, q, cs...)
	})
}

// createCredentials stores credentials in the caller's transaction, refusing
// a revoked family or a hash taken.
func createCredentials(ctx context.Context, q *db.Queries, cs ...store.Credential) error {
	for _, c := range cs {
		if n, err := q.FamilyRevoked(ctx, c.Family); err != nil {
			return err
		} else if n > 0 {
			return store.ErrRevoked
		}
		if _, err := q.GetCredential(ctx, c.Hash); err == nil {
			return store.ErrExists
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := q.InsertCredential(ctx, db.InsertCredentialParams{
			Hash: c.Hash, Type: c.Type, Family: c.Family, Principal: c.Principal,
			CreatedAt: millis(c.CreatedAt), ExpiresAt: millis(c.ExpiresAt), IdleUntil: millis(c.IdleUntil),
			LastUsedAt: millis(c.LastUsedAt), UsedAt: millis(c.UsedAt), Audience: c.Audience,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetCredential(ctx context.Context, hash string) (store.Credential, error) {
	if s.closing.Load() {
		return store.Credential{}, store.ErrClosed
	}
	row, err := s.q.GetCredential(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Credential{}, store.ErrNotFound
	}
	if err != nil {
		return store.Credential{}, s.closedOr(err)
	}
	return credentialFromRow(row), nil
}

func (s *Store) FamilyHolds(ctx context.Context, family, typ string, now time.Time) (bool, error) {
	if s.closing.Load() {
		return false, store.ErrClosed
	}
	n, err := s.q.FamilyHolds(ctx, db.FamilyHoldsParams{Family: family, Type: typ, Now: millis(now)})
	if err != nil {
		return false, s.closedOr(err)
	}
	return n > 0, nil
}

func (s *Store) TouchCredential(ctx context.Context, hash string, lastUsed, idleUntil time.Time) error {
	return s.credTx(ctx, func(q *db.Queries) error {
		n, err := q.TouchCredential(ctx, db.TouchCredentialParams{LastUsedAt: millis(lastUsed), IdleUntil: millis(idleUntil), Hash: hash})
		if err == nil && n == 0 {
			return store.ErrNotFound
		}
		return err
	})
}

func (s *Store) ConsumeCredential(ctx context.Context, hash string, at time.Time) (store.Credential, error) {
	var c store.Credential
	err := s.credTx(ctx, func(q *db.Queries) error {
		row, err := q.GetCredential(ctx, hash)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		c = credentialFromRow(row)
		if row.UsedAt != 0 {
			return store.ErrConflict
		}
		_, err = q.ConsumeCredential(ctx, db.ConsumeCredentialParams{UsedAt: millis(at), Hash: hash})
		return err
	})
	return c, err
}

func (s *Store) RevokeCredentialFamily(ctx context.Context, family string, until time.Time) (int, error) {
	var n int64
	err := s.credTx(ctx, func(q *db.Queries) error {
		exp, err := q.FamilyExpiry(ctx, family)
		if err != nil {
			return err
		}
		n, err = revokeFamily(ctx, q, family, max(exp, millis(until)))
		return err
	})
	return int(n), err
}

func (s *Store) RevokeCredentialsOf(ctx context.Context, principal string, until time.Time) (int, error) {
	var n int64
	err := s.credTx(ctx, func(q *db.Queries) error {
		families, err := q.FamiliesOf(ctx, principal)
		if err != nil {
			return err
		}
		for _, f := range families {
			d, err := revokeFamily(ctx, q, f.Family, max(f.ExpiresAt, millis(until)))
			if err != nil {
				return err
			}
			n += d
		}
		return nil
	})
	return int(n), err
}

func (s *Store) ReplaceCredentials(ctx context.Context, c store.Credential, until time.Time) error {
	return s.credTx(ctx, func(q *db.Queries) error {
		families, err := q.FamiliesOf(ctx, c.Principal)
		if err != nil {
			return err
		}
		for _, f := range families {
			if _, err := revokeFamily(ctx, q, f.Family, max(f.ExpiresAt, millis(until))); err != nil {
				return err
			}
		}
		return createCredentials(ctx, q, c)
	})
}

// revokeFamily marks a family revoked until expires (Unix milliseconds) and
// deletes its credentials, in the caller's transaction.
func revokeFamily(ctx context.Context, q *db.Queries, family string, expires int64) (int64, error) {
	if err := q.RevokeFamily(ctx, db.RevokeFamilyParams{Family: family, ExpiresAt: expires}); err != nil {
		return 0, err
	}
	return q.DeleteCredentialFamily(ctx, family)
}

func (s *Store) CreateAuthRequest(ctx context.Context, r store.AuthRequest) error {
	return s.credTx(ctx, func(q *db.Queries) error {
		if _, err := q.GetAuthRequest(ctx, r.Key); err == nil {
			return store.ErrExists
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return q.InsertAuthRequest(ctx, db.InsertAuthRequestParams{Key: r.Key, Data: nonNil(r.Data), ExpiresAt: millis(r.ExpiresAt)})
	})
}

func (s *Store) CreateAuthRequestsWithin(ctx context.Context, now time.Time, limits []store.AuthLimit, rs ...store.AuthRequest) error {
	// Write transactions run one at a time (the write connection takes its
	// lock as it begins), so nothing lands between the count and the insert.
	return s.credTx(ctx, func(q *db.Queries) error {
		for _, l := range limits {
			n, err := q.CountAuthRequests(ctx, db.CountAuthRequestsParams{Prefix: l.Prefix, Now: millis(now)})
			if err != nil {
				return err
			}
			if int(n) >= l.Max {
				return &store.LimitError{Prefix: l.Prefix}
			}
		}
		for _, r := range rs {
			if _, err := q.GetAuthRequest(ctx, r.Key); err == nil {
				return store.ErrExists
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err := q.InsertAuthRequest(ctx, db.InsertAuthRequestParams{Key: r.Key, Data: nonNil(r.Data), ExpiresAt: millis(r.ExpiresAt)}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetAuthRequest(ctx context.Context, key string) (store.AuthRequest, error) {
	if s.closing.Load() {
		return store.AuthRequest{}, store.ErrClosed
	}
	row, err := s.q.GetAuthRequest(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return store.AuthRequest{}, store.ErrNotFound
	}
	if err != nil {
		return store.AuthRequest{}, s.closedOr(err)
	}
	return authRequestFromRow(row), nil
}

func (s *Store) SwapAuthRequest(ctx context.Context, key string, old, data []byte) error {
	return s.credTx(ctx, func(q *db.Queries) error {
		row, err := q.GetAuthRequest(ctx, key)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		if !bytes.Equal(row.Data, old) {
			return store.ErrConflict
		}
		_, err = q.SwapAuthRequest(ctx, db.SwapAuthRequestParams{Data: nonNil(data), Key: key, Old: row.Data})
		return err
	})
}

func (s *Store) TakeAuthRequest(ctx context.Context, key string) (store.AuthRequest, error) {
	var r store.AuthRequest
	err := s.credTx(ctx, func(q *db.Queries) error {
		row, err := q.GetAuthRequest(ctx, key)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		r = authRequestFromRow(row)
		_, err = q.DeleteAuthRequest(ctx, key)
		return err
	})
	return r, err
}

func (s *Store) CountAuthRequests(ctx context.Context, prefix string, now time.Time) (int, error) {
	if s.closing.Load() {
		return 0, store.ErrClosed
	}
	n, err := s.q.CountAuthRequests(ctx, db.CountAuthRequestsParams{Prefix: prefix, Now: millis(now)})
	return int(n), s.closedOr(err)
}

func (s *Store) DeleteExpired(ctx context.Context, now time.Time) error {
	return s.credTx(ctx, func(q *db.Queries) error {
		if err := q.DeleteExpiredCredentials(ctx, millis(now)); err != nil {
			return err
		}
		if err := q.DeleteExpiredRevokedFamilies(ctx, millis(now)); err != nil {
			return err
		}
		return q.DeleteExpiredAuthRequests(ctx, millis(now))
	})
}

// credTx runs fn in a write transaction and commits it if fn succeeds.
func (s *Store) credTx(ctx context.Context, fn func(q *db.Queries) error) error {
	if s.closing.Load() {
		return store.ErrClosed
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return s.closedOr(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(s.q.WithTx(tx)); err != nil {
		return s.closedOr(err)
	}
	return s.closedOr(tx.Commit())
}

// millis is t in Unix milliseconds, 0 for the zero time.
func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func credentialFromRow(r db.Credential) store.Credential {
	return store.Credential{
		Hash: r.Hash, Type: r.Type, Family: r.Family, Principal: r.Principal,
		CreatedAt: fromMillis(r.CreatedAt), ExpiresAt: fromMillis(r.ExpiresAt), IdleUntil: fromMillis(r.IdleUntil),
		LastUsedAt: fromMillis(r.LastUsedAt), UsedAt: fromMillis(r.UsedAt), Audience: r.Audience,
	}
}

func authRequestFromRow(r db.AuthRequest) store.AuthRequest {
	return store.AuthRequest{Key: r.Key, Data: r.Data, ExpiresAt: fromMillis(r.ExpiresAt)}
}
