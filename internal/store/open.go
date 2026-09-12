package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// Env is a lookup function, the same shape internal/harness and internal/runctx
// use, so a test moves the store by handing Open a different environment
// instead of touching the process one. Nothing in this package reads
// os.Getenv.
type Env func(string) string

// The store's one file is global on purpose: sessions from
// every repo land in one database, so a listing is a listing of everything,
// and a relative or cwd-based path that split the store per repo would defeat
// that.
const (
	dbDirName  = "krowk"
	dbFileName = "krowk.db"
)

// ErrNoHome is why Open fails when the environment names no home: a store
// with nowhere to live must not fall back to /krowk.db or the current
// directory. Match it with errors.Is rather than the hint text, which is
// for humans.
var ErrNoHome = errors.New("store: no home directory in environment")

// DBPath is where the store lives: $XDG_DATA_HOME/krowk/krowk.db when
// XDG_DATA_HOME is set to an absolute path (the XDG basedir spec says a
// relative value must be ignored), otherwise ~/.local/share/krowk/krowk.db.
// No home in env is an empty string, never a guess — the same rule as
// harness.HomeDir, because a store that invented a home would write a
// database on a machine nobody is using. A relative HOME is also empty:
// joining it would land the store under the working directory and split it
// per repo, which is exactly what the global file is for.
func DBPath(env Env) string {
	if env == nil {
		return ""
	}
	if dir := env("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, dbDirName, dbFileName)
	}
	home := homeDir(env)
	if home == "" || !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Join(home, ".local", "share", dbDirName, dbFileName)
}

// homeDir mirrors harness.HomeDir: HOME wins wherever it is set, USERPROFILE
// is the fallback and only on Windows. Duplicated rather than imported so the
// store keeps no dependency on harness; the rule is the contract, and the
// tests here pin it.
func homeDir(env Env) string {
	if env == nil {
		return ""
	}
	if home := env("HOME"); home != "" {
		return filepath.Clean(home)
	}
	if runtime.GOOS == "windows" {
		if home := env("USERPROFILE"); home != "" {
			return filepath.Clean(home)
		}
	}
	return ""
}

// pragmas ride the DSN so the driver replays them on every connection the
// pool opens — a pool is many connections, and a pragma set by hand on one of
// them would leave the others on defaults. Order matters to the driver:
// busy_timeout first, so a locked database waits rather than failing while
// the later pragmas run. Setting any pragma also cancels the driver's default
// busy timeout, which is why it is restated here. journal_mode=wal is what
// lets readers never block the writer; synchronous=NORMAL is durable enough
// under WAL (a power cut may lose the last transactions, never corrupt);
// foreign_keys defaults to off in SQLite and every schema here assumes on.
const pragmas = "_pragma=busy_timeout(10000)" +
	"&_pragma=journal_mode(wal)" +
	"&_pragma=synchronous(normal)" +
	"&_pragma=foreign_keys(1)"

// dsn builds the SQLite filename URI for path. Built here and never from
// caller input, because the driver honors full URIs (paths, modes, _pragma)
// and an open DSN would let a caller relocate the file or weaken durability
// behind Open's back. url.URL escapes the characters a path could smuggle
// into the query string ('?', '#').
func dsn(path string) string {
	return fileURI(path, pragmas)
}

// Open opens the store at DBPath(env), creating the parent directory when it
// is missing, and fails closed when env names no home: a store with nowhere
// to live must not fall back to /krowk.db or the current directory. The
// database file is created 0600 before SQLite touches it, and a pre-existing
// file (or sidecar) with wider permissions is tightened back to 0600, so
// sessions stay private to the user. Side files SQLite creates later (-wal,
// -shm) follow the process umask — Open tightens whatever a previous run
// left behind; files created after Open returns belong to the write path.
//
// A missing or empty file is initialised from 001_init.sql in one
// transaction, stamped PRAGMA user_version = 1. A file at any other version,
// at version 0 with tables Open never wrote, or at version 1 with a table
// missing, fails with a `krowk sessions rebuild` hint and is left
// unmodified — refused by a read-only pre-check before the read-write open,
// and the read-write handle itself carries no persistent pragma until the
// re-check accepts, so not even a swapped-in file gets its header flipped
// first. No silent repair, no in-place ALTER path in v1. The gate checks
// tables, never indexes: a v1 file created before a later index landed is
// still accepted and answers correctly, just on a slower plan, until a
// rebuild recreates the file with every index.
// The returned handle is pinged, so a path that cannot hold a database fails
// here rather than on first use.
func Open(env Env) (*sql.DB, error) {
	path := DBPath(env)
	if path == "" {
		return nil, fmt.Errorf("%w: set HOME (or XDG_DATA_HOME to an absolute path) so krowk.db has a place to live", ErrNoHome)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", filepath.Dir(path), err)
	}
	// MkdirAll creates with 0700 but tightens nothing: take the group/other
	// bits back off a directory an older run left world-readable, the same
	// rule as the database file below. Windows has no mode bits to take.
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Dir(path)); err != nil {
			return nil, fmt.Errorf("store: stat %s: %w", filepath.Dir(path), err)
		} else if fi.Mode().Perm()&0o077 != 0 {
			if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
				return nil, fmt.Errorf("store: chmod %s: %w", filepath.Dir(path), err)
			}
		}
	}
	// Refuse a foreign or future file before touching it: the create and
	// tighten below, and the DSN connect pragmas (journal_mode especially)
	// after them, would otherwise rewrite the header or mode bits of a
	// file Open is about to reject. A missing file skips this — there is
	// nothing to refuse yet — and the write path re-checks on its own
	// handle, so a file swapped in between still fails closed. preStat
	// pins the checked file's identity for the SameFile comparison after
	// open, closing the swap-then-chmod window deterministically.
	var preStat os.FileInfo
	if st, err := os.Stat(path); err == nil {
		preStat = st
		if err := checkSchemaFile(path, SchemaSQL); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("store: stat %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create %s: %w", path, err)
	}
	// The handle is closed on every failure below; the final Close past
	// them is checked, so a failed create-flush fails here, not in SQLite.
	failed := true
	defer func() {
		if failed {
			f.Close()
		}
	}()
	// OpenFile's mode applies at creation only: tighten a pre-existing file
	// (an older version may have left it 0644) so sessions stay private.
	// A stat that cannot even run fails closed rather than hoping.
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("store: stat %s: %w", path, err)
	}
	// The path may have been swapped for another file after the pre-check
	// passed: refuse the stranger before chmodding or initialising it.
	// SameFile compares device and inode, so only the checked file
	// proceeds. (A swap after this point is a same-user race against
	// syscalls nanoseconds apart; the content re-checks still refuse it.)
	if preStat != nil && !os.SameFile(preStat, fi) {
		return nil, fmt.Errorf("store: %s changed during open, refusing", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := f.Chmod(0o600); err != nil {
			return nil, fmt.Errorf("store: chmod %s: %w", path, err)
		}
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("store: close %s: %w", path, err)
	}
	failed = false
	// Gate-phase DSN: no persistent pragma rides this open (see gateDSN),
	// so a file the re-check below refuses keeps its exact header bytes.
	db, err := openSQL(gateDSN(path))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// Cleanup only: the deferred Close reports nothing — the returned
	// error below is the one that matters.
	dbFailed := true
	defer func() {
		if dbFailed {
			db.Close()
		}
	}()
	// Busy-retry: the connect-time pragma replay runs before busy_timeout
	// is armed, so a commit landing in that window fails here instead of
	// waiting (see busyRetry).
	if err := busyRetry(db.Ping); err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// Initialise a fresh file or accept an exact match. Re-reads the
	// version on this handle, so a file swapped after the pre-open check
	// still fails closed — and on the gate DSN, without a header rewrite
	// first.
	if err := ensureSchema(db, path, SchemaSQL); err != nil {
		return nil, err
	}
	// Flip the accepted file to the steady-state durability the full DSN
	// replays on every later connection. Runs only after the accept, so a
	// refusal never persists anything.
	if err := persistPragmas(db, path); err != nil {
		return nil, err
	}
	// The gate handle carries the gate DSN, whose later pool connections
	// would miss the WAL replay. Trade it for the steady handle every
	// other Open caller sees, and check-only verify on it: the gate
	// already initialised or accepted, so this pass only refuses.
	// Disarm the deferred cleanup first: the explicit Close below owns
	// the gate handle from here on.
	dbFailed = false
	if err := db.Close(); err != nil {
		return nil, fmt.Errorf("store: close %s: %w", path, err)
	}
	db, err = openSQL(dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	dbFailed = true
	// Busy-retry: the connect-time pragma replay runs before busy_timeout
	// is armed, so a commit landing in that window fails here instead of
	// waiting (see busyRetry).
	if err := busyRetry(db.Ping); err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// Check-only: the gate handle already initialised or accepted. A file
	// that reads version 0 here was swapped or truncated between the two
	// opens — verifySchema refuses it rather than re-initialising over
	// the loss. Never writes.
	if err := verifySchema(db, path, SchemaSQL); err != nil {
		return nil, err
	}
	// A previous run on a permissive umask may have left world-readable
	// sidecars behind (SQLite creates them lazily under the umask, not
	// from the database's mode). Tighten whatever is already there; only
	// a missing file is fine to skip, anything else failing to stat
	// fails closed like the database file itself.
	for _, side := range []string{path + "-wal", path + "-shm", path + "-journal"} {
		fi, err := os.Stat(side)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && fi.Mode().Perm()&0o077 == 0) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("store: stat %s: %w", side, err)
		}
		if err := os.Chmod(side, 0o600); err != nil {
			return nil, fmt.Errorf("store: chmod %s: %w", side, err)
		}
	}
	dbFailed = false
	return db, nil
}
