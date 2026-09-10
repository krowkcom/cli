package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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
-- Names are load-bearing: worktree, never project or workspace. Every id
-- is TEXT PRIMARY KEY holding a uuidv7 minted by this package (see
-- ValidateID); the one table without an id is import_state, keyed by
-- source instead. seq is the only ordering key: transcripts do not keep
-- time straight, so order of record is a sequence, not a clock. Dedup is
-- a unique index on foreign ids, never an id derived from them — a
-- second import of the same Claude session converges on one row because
-- the (provider, foreign_session_id) index says so.
--
-- Conventions every table follows (pinned by schema_conventions_test):
-- every *_id column has a FOREIGN KEY with ON DELETE CASCADE, every FK
-- child column leads an index, every time_* column is INTEGER
-- milliseconds since the Unix epoch (UTC), role is a closed CHECK while
-- provider/harness/type stay open strings.

CREATE TABLE worktree (
  id TEXT PRIMARY KEY,
  path TEXT NOT NULL UNIQUE,
  vcs TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL
);

CREATE TABLE session (
  id TEXT PRIMARY KEY,
  worktree_id TEXT NOT NULL,
  parent_id TEXT NULL,
  directory TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  harness TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 1,
  remote_slug TEXT NULL UNIQUE,
  deleted_at INTEGER NULL,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  FOREIGN KEY (worktree_id) REFERENCES worktree(id) ON DELETE CASCADE,
  FOREIGN KEY (parent_id) REFERENCES session(id) ON DELETE CASCADE
);

CREATE TABLE session_binding (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  provider TEXT NOT NULL DEFAULT '',
  harness TEXT NOT NULL DEFAULT '',
  foreign_session_id TEXT NOT NULL,
  resume_cmd TEXT NOT NULL DEFAULT '',
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE,
  UNIQUE (provider, foreign_session_id)
);

CREATE TABLE session_event (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  type TEXT NOT NULL DEFAULT '',
  data TEXT NOT NULL DEFAULT '{}',
  time_created INTEGER NOT NULL,
  FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE,
  UNIQUE (session_id, seq)
);

CREATE TABLE turn (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  status TEXT NOT NULL DEFAULT '',
  cost_input_tokens INTEGER NOT NULL DEFAULT 0,
  cost_output_tokens INTEGER NOT NULL DEFAULT 0,
  cost_total_tokens INTEGER NOT NULL DEFAULT 0,
  time_created INTEGER NOT NULL,
  time_updated INTEGER NOT NULL,
  FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE,
  UNIQUE (session_id, seq)
);

CREATE TABLE message (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  turn_id TEXT NULL,
  seq INTEGER NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system', 'tool', 'error')),
  provider TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  foreign_id TEXT NULL,
  usage TEXT NOT NULL DEFAULT '{}',
  raw_json TEXT NULL,
  time_created INTEGER NOT NULL,
  FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE,
  FOREIGN KEY (turn_id) REFERENCES turn(id) ON DELETE CASCADE,
  UNIQUE (session_id, seq)
);

CREATE UNIQUE INDEX idx_message_session_foreign ON message(session_id, foreign_id) WHERE foreign_id IS NOT NULL;

-- part.session_id repeats its message's session: a denormalized FK so
-- session-scoped part scans never join through message. The importer
-- keeps the two in agreement; nothing in DDL can check across rows.
CREATE TABLE part (
  id TEXT PRIMARY KEY,
  message_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  type TEXT NOT NULL DEFAULT '',
  tool_call_id TEXT NULL,
  signature TEXT NULL,
  data TEXT NOT NULL DEFAULT '{}',
  foreign_id TEXT NULL,
  FOREIGN KEY (message_id) REFERENCES message(id) ON DELETE CASCADE,
  FOREIGN KEY (session_id) REFERENCES session(id) ON DELETE CASCADE,
  UNIQUE (message_id, seq)
);

-- import_state has no id column: source is the key. It holds cursor
-- state no transcript on disk re-derives, which is why the migrations
-- table still stays out — cursors are rewritten, never migrated.
CREATE TABLE import_state (
  source TEXT PRIMARY KEY,
  cursor TEXT NOT NULL DEFAULT '',
  time_updated INTEGER NOT NULL
);

CREATE INDEX idx_session_worktree_updated ON session(worktree_id, time_updated);
CREATE INDEX idx_session_parent ON session(parent_id);
CREATE INDEX idx_binding_session ON session_binding(session_id);
CREATE INDEX idx_message_turn ON message(turn_id);
CREATE INDEX idx_part_message ON part(message_id);
CREATE INDEX idx_part_session ON part(session_id);
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
// purpose rather than parsing arbitrary DDL — but it skips TEMP and an
// optional schema qualifier, so neither leaks into the expected name.
var createTableRe = regexp.MustCompile(`(?i)CREATE\s+(?:TEMP(?:ORARY)?\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:["'` + "`" + `\[]?[A-Za-z_][A-Za-z0-9_]*["'` + "`" + `\]]?\.)?["'` + "`" + `\[]?([A-Za-z_][A-Za-z0-9_]*)`)

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

// gatePragmas ride the read-write handle only until the schema gate passes:
// busy_timeout and foreign_keys are per-connection, so opening with them
// rewrites no header bytes. journal_mode and synchronous are persistent —
// replaying them at connect would flip a refused file to WAL before the
// re-check even runs — so Open applies those explicitly after the accept.
const gatePragmas = "_pragma=busy_timeout(10000)" +
	"&_pragma=foreign_keys(1)"

// gateDSN opens path read-write without the persistent pragmas, for the
// gate and the init transaction. A file refused after this open is still
// byte-identical: nothing yet had a reason to rewrite its header.
func gateDSN(path string) string {
	return fileURI(path, gatePragmas)
}

// persistPragmas flips the accepted file to the steady-state durability the
// DSN replays on every later connection: WAL so readers never block the
// writer, NORMAL synchronous which is safe under WAL. Runs once, after the
// gate accepts — never before a refusal.
func persistPragmas(db *sql.DB, path string) error {
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		return fmt.Errorf("store: persist journal_mode %s: %w", path, err)
	}
	if _, err := db.Exec(`PRAGMA synchronous = NORMAL`); err != nil {
		return fmt.Errorf("store: persist synchronous %s: %w", path, err)
	}
	return nil
}

// inspectDSN opens path read-only and immutable: no lock, no WAL recovery,
// no journal-mode flip, so a file Open is about to refuse stays
// byte-identical. No pragmas ride it either — even replaying journal_mode
// would rewrite the header of a file that is none of our business.
// Immutable still reads committed WAL frames (verified, not assumed), so a
// version stamped but never checkpointed is seen, not missed; a writer
// mid-commit is the residual TOCTOU the read-write re-check backstops.
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

// busyRetry runs fn until it succeeds or fails with anything but
// SQLITE_BUSY ("database is locked"), bounding the wait. It is the missing
// half of busy_timeout for opens and gate reads: the driver replays the
// connect-time _pragma list before busy_timeout itself is armed, so a
// commit landing in that window fails the open instead of waiting. The lock
// holder always finishes in milliseconds (short txns by contract), so a
// short retry turns a spurious failure into a success; a genuinely stuck
// writer still surfaces after ~0.5s.
func busyRetry(fn func() error) error {
	var err error
	for i := 0; i < 10; i++ {
		if err = fn(); err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "database is locked") {
			return err
		}
		time.Sleep(time.Duration(i+1) * 10 * time.Millisecond)
	}
	return err
}

// readVersionAndTables reads the version and table set as one atomic unit:
// a read-only transaction pins the snapshot, so the pair can never straddle
// a concurrent commit (version 0 from before, tables from after). The store
// has no context plumbing; Background is the whole contract.
func readVersionAndTables(db *sql.DB) (int, map[string]bool, error) {
	var version int
	var tables map[string]bool
	err := busyRetry(func() error {
		tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			return err
		}
		t, err := listTables(tx)
		if err != nil {
			return err
		}
		tables = t
		return tx.Commit()
	})
	if err != nil {
		return 0, nil, err
	}
	return version, tables, nil
}

// inspectSchema reads the version and table set without modifying anything.
// The caller decides what versions and shapes are acceptable.
func inspectSchema(path string) (version int, tables map[string]bool, err error) {
	db, err := openSQL(inspectDSN(path))
	if err != nil {
		return 0, nil, err
	}
	defer db.Close()
	// A single connection keeps the two reads on the same handle; the
	// read transaction keeps them on the same snapshot.
	db.SetMaxOpenConns(1)
	return readVersionAndTables(db)
}

// rebuildHint tells the human the one supported recovery: the file holds
// nothing worth migrating yet, so delete it and re-import from transcripts.
func rebuildHint(path, why string) error {
	return fmt.Errorf("%w: %s; run `krowk sessions rebuild` (delete %s and re-import)", ErrSchemaMismatch, why, path)
}

// hasUserTables reports whether the file holds tables Open did not create.
// sqlite_% internals (sqlite_sequence from AUTOINCREMENT, sqlite_stat1 from
// ANALYZE) never stand alone — a file with only those is still fresh.
func hasUserTables(tables map[string]bool) bool {
	for name := range tables {
		if !strings.HasPrefix(name, "sqlite_") {
			return true
		}
	}
	return false
}

// checkSchemaContent applies the gate to an inspected file: version must be
// 0 on an empty file (fresh) or exactly SchemaVersion with every expected
// table present, and a migrations table must never be there in v1 — its
// presence means a newer writer touched the file. Version 0 with tables is
// refused, never adopted: stamping a foreign file would bless content this
// package did not create.
func checkSchemaContent(path string, version int, tables map[string]bool, schema string) error {
	if version == 0 {
		if hasUserTables(tables) {
			return rebuildHint(path, "krowk.db holds tables at schema version 0, which Open never writes")
		}
		return nil
	}
	if version != SchemaVersion {
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
// journal mode. A file that is not a database at all maps to the rebuild
// hint too: the message names the driver error, so the hint is actionable
// instead of raw. Other inspect failures (permissions, I/O) stay plain
// errors — hinting a rebuild there would blame the file for the filesystem.
// Open calls this before the read-write open; the write path re-checks on
// its own handle, so a file swapped between the two still fails closed
// (and, on the gate DSN, without a header rewrite first).
func checkSchemaFile(path, schema string) error {
	version, tables, err := inspectSchema(path)
	if err != nil {
		if msg := err.Error(); strings.Contains(msg, "file is not a database") ||
			strings.Contains(msg, "database disk image is malformed") {
			// Both wordings are SQLite's own stable SQLITE_NOTADB /
			// SQLITE_CORRUPT texts, not ours to rephrase away.
			return rebuildHint(path, fmt.Sprintf("krowk.db is unreadable (%s)", msg))
		}
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

// verifySchema is the check-only gate: read, then accept or refuse, never
// initialise. The steady handle in Open uses this, not ensureSchema — a
// file that regressed to version 0 between the two opens was swapped or
// truncated, and silently re-initialising it would mask the loss.
func verifySchema(db *sql.DB, path, schema string) error {
	version, tables, err := readVersionAndTables(db)
	if err != nil {
		return fmt.Errorf("store: read schema version %s: %w", path, err)
	}
	if version == 0 {
		return rebuildHint(path, "krowk.db regressed to schema version 0 after the gate accepted it")
	}
	return checkSchemaContent(path, version, tables, schema)
}

// retryDecision picks the error when a schema apply fails: the re-read
// decides. A file that now matches was concurrently initialised — adopt it.
// A file that now disagrees is refused as a mismatch, even though an apply
// also failed; only a still-empty file reports the apply failure itself.
func retryDecision(path string, version int, tables map[string]bool, schema string, applyErr error) error {
	if err := checkSchemaContent(path, version, tables, schema); err != nil {
		return err
	}
	if version != 0 {
		return nil
	}
	return fmt.Errorf("store: init schema %s: %w", path, applyErr)
}

// ensureSchema brings the read-write handle to the gate: re-read the version
// on this handle (the file may have moved since checkSchemaFile), initialise
// a fresh file, accept an exact match, or fail with the rebuild hint before
// any write of its own. A lost init race is accepted, not errored: two
// first-launch Opens may both read version 0, and the loser must adopt the
// winner's schema rather than report "table already exists". Callers close
// db on error.
func ensureSchema(db *sql.DB, path, schema string) error {
	version, tables, err := readVersionAndTables(db)
	if err != nil {
		return fmt.Errorf("store: read schema version %s: %w", path, err)
	}
	if version == 0 {
		if err := checkSchemaContent(path, version, tables, schema); err != nil {
			return err
		}
		if err := applySchema(db, schema); err != nil {
			applyErr := err
			// Re-read: a concurrent Open may have initialised while this
			// apply waited on the write lock. The re-read decides what
			// the failure means (see retryDecision).
			if v2, t2, rerr := readVersionAndTables(db); rerr == nil {
				return retryDecision(path, v2, t2, schema, applyErr)
			}
			return fmt.Errorf("store: init schema %s: %w", path, applyErr)
		}
		return nil
	}
	return checkSchemaContent(path, version, tables, schema)
}
