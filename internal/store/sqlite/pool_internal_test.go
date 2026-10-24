package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestReadersStayOpen: read connections in use at once, up to idleReaders,
// are kept open once done rather than closed and opened again.
func TestReadersStayOpen(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	conns := make([]*sql.Conn, idleReaders)
	for i := range conns {
		if conns[i], err = s.rdb.Conn(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range conns {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if st := s.rdb.Stats(); st.MaxIdleClosed > 0 || st.Idle != idleReaders {
		t.Errorf("%d read connections closed for want of room, %d kept; want none closed, %d kept", st.MaxIdleClosed, st.Idle, idleReaders)
	}
}
