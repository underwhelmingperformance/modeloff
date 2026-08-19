// Package storetest provides test helpers for the store package.
package storetest

import (
	"database/sql"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver" // SQLite driver.

	"github.com/laney/modeloff/internal/store"
)

// NewMemoryStore creates an in-memory SQLite store for use in tests.
// It opens through [store.SQLitePragmaDSN], the same DSN construction
// the real store uses, so `foreign_keys` (and the other
// connection-time PRAGMAs) match production — a test that leans on
// foreign-key enforcement needs it turned on here to fail the same
// way it would against the real database. The connection pool is
// limited to one so all goroutines share the same in-memory database.
// The store is closed when the test ends.
func NewMemoryStore(t testing.TB) *store.SQLiteStore {
	t.Helper()

	db, err := sql.Open("sqlite3", store.SQLitePragmaDSN(":memory:"))
	if err != nil {
		t.Fatal("open in-memory sqlite:", err)
	}

	db.SetMaxOpenConns(1)

	s, err := store.NewSQLiteStore(t.Context(), db)
	if err != nil {
		_ = db.Close()
		t.Fatal("create test store:", err)
	}

	t.Cleanup(func() { _ = s.Close() })

	return s
}
