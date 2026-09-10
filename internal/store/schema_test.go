package store

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	// TEMP and schema qualifiers must not leak into the expected name.
	for _, tc := range []struct{ ddl, want string }{
		{`CREATE TEMP TABLE tmp (x TEXT)`, "tmp"},
		{`CREATE TEMPORARY TABLE IF NOT EXISTS tmp2 (x TEXT)`, "tmp2"},
		{`CREATE TABLE main.session (x TEXT)`, "session"},
	} {
		got := expectedTables(tc.ddl)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("expectedTables(%q) = %q, want [%q]", tc.ddl, got, tc.want)
		}
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

func TestSchemaGateVersionZeroWithTablesRefused(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".local", "share", "krowk", "krowk.db")
	// A foreign file: tables Open never wrote, and no version stamp to
	// claim them with. Stamping it would bless content this package did
	// not create.
	seedPlainDB(t, path, 0, `CREATE TABLE stranger (x TEXT)`)
	before := fileHash(t, path)

	_, err := Open(testEnv(map[string]string{"HOME": home}))
	if err == nil {
		t.Fatal("Open adopted a version-0 file with foreign tables")
	}
	if !errors.Is(err, ErrSchemaMismatch) || !strings.Contains(err.Error(), "krowk sessions rebuild") {
		t.Errorf("error %q is not a rebuild-hint ErrSchemaMismatch", err)
	}
	if after := fileHash(t, path); after != before {
		t.Error("refused file was modified")
	}
}

func TestSchemaGateCorruptFileGetsRebuildHint(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "share", "krowk")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "krowk.db")
	if err := os.WriteFile(path, []byte("this is not a database file at all"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := fileHash(t, path)

	_, err := Open(testEnv(map[string]string{"HOME": home}))
	if err == nil {
		t.Fatal("Open passed a corrupt file")
	}
	if !errors.Is(err, ErrSchemaMismatch) || !strings.Contains(err.Error(), "krowk sessions rebuild") {
		t.Errorf("error %q is not a rebuild-hint ErrSchemaMismatch", err)
	}
	if after := fileHash(t, path); after != before {
		t.Error("refused file was modified")
	}
}

func TestSchemaGateRefusedFileKeepsModeBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not portable to Windows")
	}
	home := t.TempDir()
	path := filepath.Join(home, ".local", "share", "krowk", "krowk.db")
	seedPlainDB(t, path, 2, `CREATE TABLE future (x TEXT)`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod seed: %v", err)
	}
	before := fileHash(t, path)

	if _, err := Open(testEnv(map[string]string{"HOME": home})); err == nil {
		t.Fatal("Open passed a version-2 file")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("refused file mode = %04o, want 0644 untouched", perm)
	}
	if after := fileHash(t, path); after != before {
		t.Error("refused file content was modified")
	}
}

func TestSchemaGateInitFailureStillFailsClosed(t *testing.T) {
	// Broken DDL is a programmer bug, not a foreign file: the error must
	// say init, not rebuild — and the file must stay at version 0 with
	// no half-applied tables.
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("seed empty: %v", err)
	}
	db, err := openSQL(dsn(path))
	if err != nil {
		t.Fatalf("openSQL: %v", err)
	}
	defer db.Close()
	err = ensureSchema(db, path, `CREATE TABLE a (id TEXT PRIMARY KEY); CREATE TABLE a (dup TEXT);`)
	if err == nil {
		t.Fatal("ensureSchema passed broken DDL")
	}
	if errors.Is(err, ErrSchemaMismatch) {
		t.Errorf("DDL failure %q misreported as schema mismatch", err)
	}
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != 0 {
		t.Errorf("user_version = %d after failed init, want 0", v)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='a'`).Scan(&n); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if n != 0 {
		t.Error("half-applied table survived a failed init; the apply is not atomic")
	}
}

func TestSchemaGateConcurrentFirstOpen(t *testing.T) {
	// Two first-launch Opens may both read version 0; the loser must adopt
	// the winner's schema rather than report "table already exists". One
	// handle per goroutine, like one process each; the DSN busy_timeout
	// serialises the writers so the loser fails only after the winner
	// commits, and its retry then accepts.
	const synthetic = `CREATE TABLE c1 (id TEXT PRIMARY KEY); CREATE TABLE c2 (id TEXT PRIMARY KEY);`
	dir := t.TempDir()
	path := filepath.Join(dir, "race.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("seed empty: %v", err)
	}
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db, err := openSQL(dsn(path))
			if err != nil {
				errs[i] = err
				return
			}
			defer db.Close()
			errs[i] = ensureSchema(db, path, synthetic)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
	db, err := openSQL("file:" + path + "?mode=ro&immutable=1")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	defer db.Close()
	if err := ensureSchema(db, path, synthetic); err != nil {
		t.Errorf("final accept: %v", err)
	}
}

func TestOpenTightensStoreDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not portable to Windows")
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "share", "krowk")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Pin the precondition explicitly: under umask 077 the dir could
	// otherwise arrive already 0700 and the tightening path would go
	// unexercised.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod seed: %v", err)
	}
	db, err := Open(testEnv(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db.Close()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("store dir mode = %04o, want no group/other bits", perm)
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
