package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testEnv(pairs map[string]string) Env {
	return func(k string) string { return pairs[k] }
}

func TestDBPath(t *testing.T) {
	tests := []struct {
		name string
		env  Env
		want string
	}{
		{
			name: "HOME resolves under .local/share",
			env:  testEnv(map[string]string{"HOME": "/home/u"}),
			want: filepath.Join("/home/u", ".local", "share", "krowk", "krowk.db"),
		},
		{
			name: "absolute XDG_DATA_HOME wins over HOME",
			env:  testEnv(map[string]string{"HOME": "/home/u", "XDG_DATA_HOME": "/data"}),
			want: filepath.Join("/data", "krowk", "krowk.db"),
		},
		{
			name: "relative XDG_DATA_HOME is ignored per spec",
			env:  testEnv(map[string]string{"HOME": "/home/u", "XDG_DATA_HOME": "data"}),
			want: filepath.Join("/home/u", ".local", "share", "krowk", "krowk.db"),
		},
		{
			name: "relative HOME fails closed, never lands under cwd",
			env:  testEnv(map[string]string{"HOME": "data"}),
			want: "",
		},
		{
			name: "no home is empty, never a guess",
			env:  testEnv(map[string]string{}),
			want: "",
		},
		{
			name: "nil env is empty",
			env:  nil,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DBPath(tt.env); got != tt.want {
				t.Errorf("DBPath = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOpenCreatesParentAndKeepsFilePrivate(t *testing.T) {
	home := t.TempDir()
	db, err := Open(testEnv(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	path := filepath.Join(home, ".local", "share", "krowk", "krowk.db")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("krowk.db mode = %04o, want no group/other bits", perm)
		}
	}
}

func TestOpenTightensPreExistingFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".local", "share", "krowk", "krowk.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// An older version may have left the file world-readable; Open must
	// take the group/other bits back off. Chmod pins the precondition
	// explicitly: WriteFile's mode is still masked by the process umask,
	// so under umask 077 the seed could otherwise arrive already 0600
	// and the tightening path would go unexercised.
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod seed: %v", err)
	}
	db, err := Open(testEnv(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not portable to Windows")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("krowk.db mode = %04o, want no group/other bits", perm)
	}
}

func TestOpenSetsPragmasOnEveryConnection(t *testing.T) {
	db, err := Open(testEnv(map[string]string{"HOME": t.TempDir()}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Two connections held at once, so the second cannot be the first one
	// reused: a pragma that only reached one connection fails here.
	ctx := context.Background()
	c1, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn 1: %v", err)
	}
	defer c1.Close()
	c2, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn 2: %v", err)
	}
	defer c2.Close()

	for i, c := range []*sql.Conn{c1, c2} {
		var mode string
		if err := c.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
			t.Fatalf("conn %d journal_mode: %v", i+1, err)
		}
		if mode != "wal" {
			t.Errorf("conn %d journal_mode = %q, want wal", i+1, mode)
		}
		var fk int
		if err := c.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
			t.Fatalf("conn %d foreign_keys: %v", i+1, err)
		}
		if fk != 1 {
			t.Errorf("conn %d foreign_keys = %d, want 1", i+1, fk)
		}
		// PRAGMA synchronous reads back as a number; NORMAL is 1.
		var sync int
		if err := c.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&sync); err != nil {
			t.Fatalf("conn %d synchronous: %v", i+1, err)
		}
		if sync != 1 {
			t.Errorf("conn %d synchronous = %d, want 1 (NORMAL)", i+1, sync)
		}
		var busy int
		if err := c.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
			t.Fatalf("conn %d busy_timeout: %v", i+1, err)
		}
		if busy != 10000 {
			t.Errorf("conn %d busy_timeout = %d, want 10000", i+1, busy)
		}
	}
}

func TestOpenEmptyHomeFailsClosed(t *testing.T) {
	// Isolate the working directory: fail-closed means no file appears
	// anywhere a default could leak to, and the assertion must read the
	// test's own directory, not the package's.
	t.Chdir(t.TempDir())

	if _, err := Open(testEnv(map[string]string{})); err == nil {
		t.Fatal("Open with no home succeeded, want error")
	} else if !errors.Is(err, ErrNoHome) {
		t.Errorf("error %q is not ErrNoHome", err)
	} else if !strings.Contains(err.Error(), "HOME") {
		t.Errorf("error %q carries no hint mentioning HOME", err)
	}
	// Fail closed means no file appears anywhere a default could leak to:
	// the current directory in particular, and /krowk.db.
	if _, statErr := os.Stat("krowk.db"); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("krowk.db appeared in the working directory: stat err = %v", statErr)
	}
	if _, statErr := os.Stat(string(filepath.Separator) + "krowk.db"); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("krowk.db appeared at filesystem root: stat err = %v", statErr)
	}

	if _, err := Open(nil); err == nil {
		t.Fatal("Open(nil) succeeded, want error")
	}
}

func TestOpenRelativeHomeFailsClosed(t *testing.T) {
	t.Chdir(t.TempDir())

	if got := DBPath(testEnv(map[string]string{"HOME": "data"})); got != "" {
		t.Fatalf("DBPath with relative HOME = %q, want empty", got)
	}
	if _, err := Open(testEnv(map[string]string{"HOME": "data"})); err == nil {
		t.Fatal("Open with relative HOME succeeded, want error")
	} else if !errors.Is(err, ErrNoHome) {
		t.Errorf("error %q is not ErrNoHome", err)
	}
	// A regression joining relative HOME would write ./data/..., not
	// ./krowk.db, so assert both are absent.
	if _, statErr := os.Stat("krowk.db"); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("krowk.db appeared in the working directory: stat err = %v", statErr)
	}
	if _, statErr := os.Stat("data"); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("data/ appeared in the working directory: stat err = %v", statErr)
	}
}

func TestDsnEscapesQueryChars(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/tmp/a b/krowk.db", "%20"},
		{"/tmp/a?b/krowk.db", "%3F"},
		{"/tmp/a#b/krowk.db", "%23"},
	} {
		got := dsn(tc.path)
		if !strings.Contains(got, tc.want) {
			t.Errorf("dsn(%q) = %q, want it to contain %q", tc.path, got, tc.want)
		}
		// The escape must actually hold: parsing the DSN back has to
		// recover the input path and keep every pragma key intact.
		u, err := url.Parse(got)
		if err != nil {
			t.Errorf("dsn(%q) does not parse: %v", tc.path, err)
			continue
		}
		if u.Path != tc.path {
			t.Errorf("dsn(%q) parses back to path %q", tc.path, u.Path)
		}
		for _, key := range []string{"_pragma"} {
			if vals := u.Query()[key]; len(vals) != 4 {
				t.Errorf("dsn(%q) carries %d _pragma values, want 4", tc.path, len(vals))
			}
		}
	}
}

func TestOpenTightensPreExistingSidecars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not portable to Windows")
	}
	home := t.TempDir()
	db, err := Open(testEnv(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// A write proves the database is real; the clean close below may
	// checkpoint the WAL away, so crash leftovers are seeded explicitly
	// afterwards — that is the case Open must tighten.
	if _, err := db.Exec(`CREATE TABLE t (x TEXT); INSERT INTO t VALUES ('s');`); err != nil {
		t.Fatalf("write: %v", err)
	}
	db.Close()

	// Pin the precondition explicitly: creation follows the umask, so
	// under umask 077 the seeds could otherwise arrive already 0600
	// and the tightening path would go unexercised.
	dir := filepath.Join(home, ".local", "share", "krowk")
	var sides []string
	for _, name := range []string{"krowk.db-wal", "krowk.db-shm"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte{}, 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatalf("chmod seed %s: %v", name, err)
		}
		sides = append(sides, p)
	}

	db, err = Open(testEnv(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	for _, p := range sides {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s mode = %04o, want no group/other bits", filepath.Base(p), perm)
		}
	}

	// The leftovers must not have broken the store itself.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("select after reopen: %v", err)
	}
	if n != 1 {
		t.Errorf("COUNT(*) = %d, want 1", n)
	}
}
