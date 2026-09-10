package store

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// seedPlainDB creates path with plain SQLite defaults (journal_mode=delete,
// no pragmas) and the exact version given. Plain defaults are the point —
// a refused file in delete mode proves the gate never flipped it to WAL.
func seedPlainDB(t *testing.T, path string, version int, stmts ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := openSQL("file:" + path)
	if err != nil {
		t.Fatalf("openSQL: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = ` + strconv.Itoa(version)); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}
}

func fileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return sha256.Sum256(b)
}

func TestSchemaSQLMatchesFile(t *testing.T) {
	// Package tests run with the package directory as cwd, so the schema
	// file resolves without anchoring to the repo root.
	raw, err := os.ReadFile(filepath.Join("schema", "001_init.sql"))
	if err != nil {
		t.Fatalf("read 001_init.sql: %v", err)
	}
	if string(raw) != SchemaSQL {
		t.Error("SchemaSQL drifted from schema/001_init.sql: edit the .sql file, then mirror it into the const")
	}
}

func TestExpectedTables(t *testing.T) {
	got := expectedTables("CREATE TABLE worktree (x TEXT);\nCREATE TABLE IF NOT EXISTS `session` (y INT);\nCREATE TABLE \"part\" (z INT);\nCREATE TABLE worktree (dup INT);\n")
	want := []string{"worktree", "session", "part"}
	if len(got) != len(want) {
		t.Fatalf("expectedTables = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expectedTables = %q, want %q", got, want)
		}
	}
	if len(expectedTables("-- only a comment\n")) != 0 {
		t.Fatal("comment-only schema lists tables")
	}
}

func TestSchemaGateFreshInitStampsVersion1(t *testing.T) {
	home := t.TempDir()
	db, err := Open(testEnv(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != SchemaVersion {
		t.Errorf("user_version = %d, want %d", v, SchemaVersion)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='migrations'`).Scan(&n); err != nil {
		t.Fatalf("migrations probe: %v", err)
	}
	if n != 0 {
		t.Error("fresh Open created a migrations table; Phase 2 owns it")
	}
}

func TestSchemaSQLHasNoMigrations(t *testing.T) {
	for _, name := range expectedTables(SchemaSQL) {
		if strings.EqualFold(name, "migrations") {
			t.Errorf("001_init.sql defines %q; the Phase 2 todo introduces it", name)
		}
	}
}

func TestSchemaGateSecondOpenRunsNoDDL(t *testing.T) {
	home := t.TempDir()
	env := testEnv(map[string]string{"HOME": home})
	db, err := Open(env)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	path := DBPath(env)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	dump := func(db *sql.DB) string {
		rows, err := db.Query(`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY type, name`)
		if err != nil {
			t.Fatalf("dump: %v", err)
		}
		defer rows.Close()
		var b strings.Builder
		for rows.Next() {
			var typ, name, tbl string
			var ddl sql.NullString
			if err := rows.Scan(&typ, &name, &tbl, &ddl); err != nil {
				t.Fatalf("scan: %v", err)
			}
			b.WriteString(typ + "/" + name + "/" + tbl + "/" + ddl.String + "\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		var v int
		if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
			t.Fatalf("version: %v", err)
		}
		b.WriteString("user_version=" + strconv.Itoa(v) + "\n")
		return b.String()
	}
	beforeDB, err := openSQL("file:" + path + "?mode=ro&immutable=1")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	before := dump(beforeDB)
	beforeDB.Close()
	beforeHash := fileHash(t, path)

	db, err = Open(env)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	after := dump(db)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if after != before {
		t.Errorf("schema changed across reopen:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if afterHash := fileHash(t, path); afterHash != beforeHash {
		t.Error("krowk.db bytes changed across reopen; the second Open must run no DDL")
	}
}

func TestSchemaGateFutureVersionFailsUnmodified(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".local", "share", "krowk", "krowk.db")
	// Plain defaults: journal_mode=delete. If the gate flipped it to WAL
	// while refusing, the bytes below would differ.
	seedPlainDB(t, path, 2, `CREATE TABLE future (x TEXT)`)
	before := fileHash(t, path)

	_, err := Open(testEnv(map[string]string{"HOME": home}))
	if err == nil {
		t.Fatal("Open on version 2 succeeded, want rebuild error")
	}
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Errorf("error %q is not ErrSchemaMismatch", err)
	}
	if !strings.Contains(err.Error(), "krowk sessions rebuild") {
		t.Errorf("error %q names no `krowk sessions rebuild` hint", err)
	}
	if after := fileHash(t, path); after != before {
		t.Error("refused file was modified; the gate must fail before the read-write open")
	}
	for _, side := range []string{path + "-wal", path + "-shm", path + "-journal"} {
		if _, statErr := os.Stat(side); !os.IsNotExist(statErr) {
			t.Errorf("sidecar %s appeared for a refused file", filepath.Base(side))
		}
	}
	// journal_mode must still be delete: no connect pragma may have run.
	db, err := openSQL("file:" + path + "?mode=ro&immutable=1")
	if err != nil {
		t.Fatalf("re-inspect: %v", err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "delete" {
		t.Errorf("journal_mode = %q after refused Open, want delete (untouched)", mode)
	}
}

func TestSchemaGateMissingTableFailsUnmodified(t *testing.T) {
	const synthetic = `CREATE TABLE turn (id TEXT PRIMARY KEY);`
	home := t.TempDir()
	path := filepath.Join(home, ".local", "share", "krowk", "krowk.db")
	// Version 1, but the expected table was never created.
	seedPlainDB(t, path, 1)
	before := fileHash(t, path)

	if err := checkSchemaFile(path, synthetic); err == nil {
		t.Fatal("checkSchemaFile passed a version-1 file missing its table")
	} else {
		if !errors.Is(err, ErrSchemaMismatch) {
			t.Errorf("error %q is not ErrSchemaMismatch", err)
		}
		if !strings.Contains(err.Error(), `"turn"`) || !strings.Contains(err.Error(), "krowk sessions rebuild") {
			t.Errorf("error %q names no table and no rebuild hint", err)
		}
	}
	if after := fileHash(t, path); after != before {
		t.Error("refused file was modified")
	}

	// The re-check on the read-write handle fails the same way, so a file
	// swapped after the pre-open check still fails closed.
	db, err := openSQL("file:" + path)
	if err != nil {
		t.Fatalf("openSQL: %v", err)
	}
	defer db.Close()
	if err := ensureSchema(db, path, synthetic); err == nil {
		t.Error("ensureSchema passed a version-1 handle missing its table")
	} else if !errors.Is(err, ErrSchemaMismatch) {
		t.Errorf("error %q is not ErrSchemaMismatch", err)
	}

	// And the matching shape passes on the same handle kind.
	seeded := filepath.Join(home, "ok.db")
	seedPlainDB(t, seeded, 1, `CREATE TABLE turn (id TEXT PRIMARY KEY)`)
	ok, err := openSQL("file:" + seeded)
	if err != nil {
		t.Fatalf("openSQL: %v", err)
	}
	defer ok.Close()
	if err := ensureSchema(ok, seeded, synthetic); err != nil {
		t.Errorf("ensureSchema refused an exact match: %v", err)
	}
}

func TestSchemaGateMigrationsTableRefused(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".local", "share", "krowk", "krowk.db")
	// Right version, but a newer writer left a migrations table behind.
	seedPlainDB(t, path, 1, `CREATE TABLE migrations (v INTEGER)`)
	before := fileHash(t, path)

	_, err := Open(testEnv(map[string]string{"HOME": home}))
	if err == nil {
		t.Fatal("Open passed a v1 file with a migrations table")
	}
	if !errors.Is(err, ErrSchemaMismatch) || !strings.Contains(err.Error(), "krowk sessions rebuild") {
		t.Errorf("error %q is not a rebuild-hint ErrSchemaMismatch", err)
	}
	if after := fileHash(t, path); after != before {
		t.Error("refused file was modified")
	}
}

func TestApplySchemaRunsDDLAndStampInOneTransaction(t *testing.T) {
	// The shipped SchemaSQL is comment-only until the v1 schema change, so
	// the multi-statement apply path is pinned here with a synthetic
	// schema: tables land and the stamp reads back on the same handle, and
	// a second ensure is an exact-match accept, not a re-apply.
	const synthetic = `
-- a comment the apply must skip, not execute
CREATE TABLE a (id TEXT PRIMARY KEY);
CREATE TABLE b (id TEXT PRIMARY KEY, a_id TEXT REFERENCES a(id));`
	dir := t.TempDir()
	path := filepath.Join(dir, "apply.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("seed empty: %v", err)
	}
	db, err := openSQL(dsn(path))
	if err != nil {
		t.Fatalf("openSQL: %v", err)
	}
	defer db.Close()
	if err := ensureSchema(db, path, synthetic); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != SchemaVersion {
		t.Errorf("user_version = %d, want %d", v, SchemaVersion)
	}
	for _, want := range []string{"a", "b"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, want).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", want, err)
		}
		if n != 1 {
			t.Errorf("table %q missing after apply", want)
		}
	}
	if err := ensureSchema(db, path, synthetic); err != nil {
		t.Errorf("second ensureSchema refused its own apply: %v", err)
	}
}
