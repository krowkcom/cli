package store

import (
	"testing"
)

// The driver is wired if an in-memory database answers a query. This stays
// small on purpose: paths, WAL and pragmas belong to the store.Open todo.
func TestDriverWired(t *testing.T) {
	db, err := openSQL(":memory:")
	if err != nil {
		t.Fatalf("openSQL: %v", err)
	}
	defer db.Close()

	// :memory: is per-connection, so hold the pool to one connection;
	// otherwise CREATE and SELECT can land on different empty databases.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}

	var version string
	if err := db.QueryRow(`SELECT sqlite_version()`).Scan(&version); err != nil {
		t.Fatalf("sqlite_version: %v", err)
	}
	if version == "" {
		t.Fatal("sqlite_version returned empty string")
	}
	t.Logf("sqlite_version=%s driver=%s", version, DriverName)

	if _, err := db.Exec(`CREATE TABLE driver_probe (id TEXT PRIMARY KEY, v INTEGER)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO driver_probe (id, v) VALUES (?, ?)`, "probe-1", 1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM driver_probe`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
}
