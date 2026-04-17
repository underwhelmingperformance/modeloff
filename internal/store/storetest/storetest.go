// Package storetest provides test helpers for the store package.
package storetest

import (
	"database/sql"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver" // SQLite driver.

	"github.com/laney/modeloff/internal/store"
)

// NewMemoryStore creates an in-memory SQLite store for use in tests.
// The connection pool is limited to one so all goroutines share the
// same in-memory database. The store is closed when the test ends.
func NewMemoryStore(t testing.TB) *store.SQLiteStore {
	t.Helper()

	db, err := sql.Open("sqlite3", ":memory:")
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
