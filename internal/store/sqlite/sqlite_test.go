package sqlite_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/YukinaMochizuki/hangar/internal/store"
	"github.com/YukinaMochizuki/hangar/internal/store/sqlite"
	"github.com/YukinaMochizuki/hangar/internal/store/storetest"
)

func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "hangar.db"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestCredentialsContract(t *testing.T) {
	storetest.RunCredentials(t, func(t *testing.T) storetest.CredentialStore {
		s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "hangar.db"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "hangar.db")
	s, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	k := store.Key{Kind: "Tenant", Name: "acme"}
	if _, err := s.Create(ctx, k, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, store.Key{Kind: "Tenant", Name: "other"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if obj, err := s.Get(ctx, k); err != nil || string(obj.Data) != "v1" {
		t.Errorf("after reopening, Get = %+v, %v", obj, err)
	}
	if rev, err := s.Create(ctx, store.Key{Kind: "Tenant", Name: "third"}, nil); err != nil || rev != 3 {
		t.Errorf("after reopening, the next revision is %d (%v), want 3", rev, err)
	}
	err = s.Watch(ctx, store.WatchOptions{SinceRevision: 1}, func(store.Event) error { return nil })
	if !errors.Is(err, store.ErrCompacted) {
		t.Errorf("after reopening, a watch below the compaction point: %v, want ErrCompacted", err)
	}
}
