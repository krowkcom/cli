package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Env is a lookup function, the same shape internal/harness and internal/runctx
// use, so a test moves the store by handing Open a different environment
// instead of touching the process one. Nothing in this package reads
// os.Getenv.
type Env func(string) string

// dbFile is the store's one file. The path is global on purpose: sessions from
// every repo land in one database, so a listing is a listing of everything,
// and a relative or cwd-based path that split the store per repo would defeat
// that.
const (
	dbDirName  = "krowk"
	dbFileName = "krowk.db"
)

// DBPath is where the store lives: $XDG_DATA_HOME/krowk/krowk.db when
// XDG_DATA_HOME is set to an absolute path (the XDG basedir spec says a
// relative value must be ignored), otherwise ~/.local/share/krowk/krowk.db.
// No home in env is an empty string, never a guess — the same rule as
// harness.HomeDir, because a store that invented a home would write a
// database on a machine nobody is using.
func DBPath(env Env) string {
	if env == nil {
		return ""
	}
	if dir := env("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, dbDirName, dbFileName)
	}
	home := homeDir(env)
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", dbDirName, dbFileName)
}

// homeDir mirrors harness.HomeDir: HOME wins wherever it is set, USERPROFILE
// is the fallback and only on Windows. Duplicated rather than imported so the
// store keeps no dependency on harness; the rule is the contract, and the
// tests here pin it.
func homeDir(env Env) string {
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
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", OmitHost: true, Path: p, RawQuery: pragmas}
	return u.String()
}

// Open opens the store at DBPath(env), creating the parent directory when it
// is missing, and fails closed when env names no home: a store with nowhere
// to live must not fall back to /krowk.db or the current directory. The
// database file is created 0600 before SQLite touches it — sessions are
// private, and the side files (-wal, -shm) inherit the database's mode.
// The returned handle is pinged, so a path that cannot hold a database fails
// here rather than on first use.
func Open(env Env) (*sql.DB, error) {
	path := DBPath(env)
	if path == "" {
		return nil, errors.New("store: no home directory in environment: set HOME (or XDG_DATA_HOME to an absolute path) so krowk.db has a place to live")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create %s: %w", path, err)
	}
	f.Close()
	db, err := openSQL(dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return db, nil
}
