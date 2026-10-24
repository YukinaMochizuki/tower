package apiserver

import (
	"context"

	"github.com/YukinaMochizuki/tower/internal/store"
)

// txKey carries, in a context, the store transaction a call runs in.
type txKey struct{}

// txStore is the apiserver's store: a call whose context carries a
// transaction (txKey) reads and writes through it, so the generic machinery
// works inside one unchanged; any other call goes to the store. No tower call
// opens a transaction yet; the wrapper is kept so the backend stays hangar's
// for the extraction (docs/decisions/engine-from-hangar.md).
type txStore struct{ store.Store }

func txOf(ctx context.Context) store.Tx {
	tx, _ := ctx.Value(txKey{}).(store.Tx)
	return tx
}

func (s txStore) Get(ctx context.Context, key store.Key) (store.Object, error) {
	if tx := txOf(ctx); tx != nil {
		return tx.Get(ctx, key)
	}
	return s.Store.Get(ctx, key)
}

func (s txStore) List(ctx context.Context, opts store.ListOptions) ([]store.Object, int64, error) {
	if tx := txOf(ctx); tx != nil {
		return tx.List(ctx, opts)
	}
	return s.Store.List(ctx, opts)
}

func (s txStore) Create(ctx context.Context, key store.Key, data []byte) (int64, error) {
	if tx := txOf(ctx); tx != nil {
		return tx.Create(ctx, key, data)
	}
	return s.Store.Create(ctx, key, data)
}

func (s txStore) Update(ctx context.Context, key store.Key, data []byte, expected int64) (int64, error) {
	if tx := txOf(ctx); tx != nil {
		return tx.Update(ctx, key, data, expected)
	}
	return s.Store.Update(ctx, key, data, expected)
}

func (s txStore) Delete(ctx context.Context, key store.Key, expected int64) (store.Object, int64, error) {
	if tx := txOf(ctx); tx != nil {
		return tx.Delete(ctx, key, expected)
	}
	return s.Store.Delete(ctx, key, expected)
}

func (s txStore) Revision(ctx context.Context) (int64, error) {
	if tx := txOf(ctx); tx != nil {
		return tx.Revision(ctx)
	}
	return s.Store.Revision(ctx)
}

// Txn inside a transaction is part of it.
func (s txStore) Txn(ctx context.Context, fn func(tx store.Tx) error) error {
	if tx := txOf(ctx); tx != nil {
		return fn(tx)
	}
	return s.Store.Txn(ctx, fn)
}
