package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// SchemaSQL is 001_init.sql, the single source of truth for the v1 schema,
// held as a string so the binary never resolves a schema path at runtime:
// the file Open applies is always the file reviewed in this package. The
// .sql file stays the edited artifact; TestSchemaSQLMatchesFile fails the
// gate on any drift between the two.
//
// (No backticks may appear in the schema file — it lives inside a raw
// string. Quote identifiers with double quotes if v1 tables ever need it.)
const SchemaSQL = `-- 001_init.sql is the whole v1 schema story: applied once, in one
-- transaction, on a fresh file, then PRAGMA user_version is set to 1.
-- Open never runs an in-place ALTER in v1 and there is no migrations
-- table until Phase 2 (the first phase that writes state the source
-- files do not hold). A version or shape mismatch fails Open with a
-- rebuild hint instead of a silent repair.
--
-- The v1 tables (worktree, session, ...) land here with the v1 schema
-- change, which also grows the gate tests to name them.
`

// SchemaVersion is the only user_version Open understands. Through Phase 1
// every row is re-derivable from transcripts on disk, so a mismatch is a
// rebuild, not a migration: the Phase 2 change introduces the migrations
// table alongside the first state the sources do not hold.
const SchemaVersion = 1

// ErrSchemaMismatch is why Open refuses a file whose version or shape is
// not exactly this package's schema. Match it with errors.Is; the hint
// text naming `krowk sessions rebuild` is for humans.
var ErrSchemaMismatch = errors.New("store: schema mismatch")

// createTableRe finds the table each CREATE TABLE in the schema defines.
// The schema is this package's own file, so the pattern stays narrow on
// purpose rather than parsing arbitrary DDL.
var createTableRe = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["'` + "`" + `\[]?([A-Za-z_][A-Za-z0-9_]*)`)

// expectedTables lists the tables 001_init.sql defines, in file order.
// Deduplicated: a later v1 edit must not make a re-listed table look like
// two expectations.
func expectedTables(schema string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range createTableRe.FindAllStringSubmatch(schema, -1) {
		name := m[1]
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// fileURI builds the SQLite filename URI for path with the given query
// string. Shared shape with dsn: url.URL escapes the characters a path
// could smuggle into the query string ('?', '#').
func fileURI(path, rawQuery string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", OmitHost: true, Path: p, RawQuery: rawQuery}
	return u.String()
}

// inspectDSN opens path read-only and immutable: no lock, no WAL recovery,
// no journal-mode flip, so a file Open is about to refuse stays
// byte-identical. No pragmas ride it either — even replaying journal_mode
// would rewrite the header of a file that is none of our business.
func inspectDSN(path string) string {
	return fileURI(path, "mode=ro&immutable=1")
}

// listTables returns the tables in the opened database.
func listTables(q interface {
	Query(string, ...any) (*sql.Rows, error)
}) (map[string]bool, error) {
	rows, err := q.Query(`SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return tables, nil
}

// inspectSchema reads the version and table set without modifying anything.
// The caller decides what versions and shapes are acceptable.
func inspectSchema(path string) (version int, tables map[string]bool, err error) {
	db, err := openSQL(inspectDSN(path))
	if err != nil {
		return 0, nil, err
	}
	defer db.Close()
	// A single connection keeps the two reads on the same snapshot.
	db.SetMaxOpenConns(1)
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return 0, nil, err
	}
	tables, err = listTables(db)
	if err != nil {
		return 0, nil, err
	}
	return version, tables, nil
}

// rebuildHint tells the human the one supported recovery: the file holds
// nothing worth migrating yet, so delete it and re-import from transcripts.
func rebuildHint(path, why string) error {
	return fmt.Errorf("%w: %s; run `krowk sessions rebuild` (delete %s and re-import)", ErrSchemaMismatch, why, path)
}

// checkSchemaContent applies the gate to an inspected file: version must be
// 0 (fresh) or exactly SchemaVersion with every expected table present, and
// a migrations table must never be there in v1 — its presence means a newer
// writer touched the file.
func checkSchemaContent(path string, version int, tables map[string]bool, schema string) error {
	if version != 0 && version != SchemaVersion {
		return rebuildHint(path, fmt.Sprintf("krowk.db schema version %d (want %d)", version, SchemaVersion))
	}
	if tables["migrations"] {
		return rebuildHint(path, "krowk.db has a migrations table, which v1 never creates")
	}
	if version == SchemaVersion {
		for _, want := range expectedTables(schema) {
			if !tables[want] {
				return rebuildHint(path, fmt.Sprintf("krowk.db is missing table %q (schema version %d)", want, SchemaVersion))
			}
		}
	}
	return nil
}

// checkSchemaFile runs the gate against the file without modifying it —
// immutable and read-only, so a refusal leaves no WAL sidecar and flips no
// journal mode. Open calls this before the read-write open; the write path
// re-checks on its own handle, so a file swapped between the two still fails
// closed (only DSN connect pragmas could have touched it, which is the
// concurrent writer's doing, not this Open's).
func checkSchemaFile(path, schema string) error {
	version, tables, err := inspectSchema(path)
	if err != nil {
		return fmt.Errorf("store: inspect %s: %w", path, err)
	}
	return checkSchemaContent(path, version, tables, schema)
}

// stripSQLComments drops full-line `--` comments. The schema file is ours,
// so line comments are the only comment form it may use; anything fancier
// belongs in Go, not in DDL.
func stripSQLComments(schema string) string {
	var b strings.Builder
	for _, line := range strings.Split(schema, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// applySchema runs schema plus the version stamp in one transaction: a crash
// between them must never leave tables at version 0, which the next Open
// would otherwise re-apply over. PRAGMA user_version is transactional, so it
// rolls back with the DDL. A comment-only schema still commits the stamp, so
// the file is never left half-initialised.
func applySchema(db *sql.DB, schema string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	// Rollback on every failure below; the Commit past them reports its
	// own error, so a failed stamp fails here, not on next Open.
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()
	if stmts := strings.TrimSpace(stripSQLComments(schema)); stmts != "" {
		if _, err := tx.Exec(stmts); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// ensureSchema brings the read-write handle to the gate: re-read the version
// on this handle (the file may have moved since checkSchemaFile), initialise
// a fresh file, accept an exact match, or fail with the rebuild hint before
// any write of its own. Callers close db on error.
func ensureSchema(db *sql.DB, path, schema string) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version %s: %w", path, err)
	}
	if version == 0 {
		if err := applySchema(db, schema); err != nil {
			return fmt.Errorf("store: init schema %s: %w", path, err)
		}
		return nil
	}
	tables, err := listTables(db)
	if err != nil {
		return fmt.Errorf("store: inspect %s: %w", path, err)
	}
	return checkSchemaContent(path, version, tables, schema)
}
