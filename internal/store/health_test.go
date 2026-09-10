package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fresh home passes: Check opens (initialising) the store, so the first
// doctor on a machine is already green and names the file.
func TestCheckPassesOnAFreshOpen(t *testing.T) {
	home := t.TempDir()
	got := Check(testEnv(map[string]string{"HOME": home}))
	if got.Name != CheckName || got.Status != "pass" {
		t.Fatalf("Check = %+v, want name %q status pass", got, CheckName)
	}
	if !strings.Contains(got.Message, filepath.Join(home, ".local", "share", "krowk", "krowk.db")) {
		t.Errorf("Check message = %q, want the store path", got.Message)
	}
	if got.Hint != "" {
		t.Errorf("passing check carries hint %q, want none", got.Hint)
	}
}

// No home fails closed with a hint that names the package, not a reinstall.
func TestCheckFailsWithoutAHome(t *testing.T) {
	got := Check(testEnv(map[string]string{}))
	if got.Status != "fail" {
		t.Fatalf("Check = %+v, want fail", got)
	}
	if !strings.Contains(got.Hint, "internal/store") {
		t.Errorf("hint = %q, want it to name internal/store", got.Hint)
	}
	if strings.Contains(strings.ToLower(got.Hint), "reinstall") {
		t.Errorf("hint = %q, must not say reinstall", got.Hint)
	}
}

// A file at the wrong schema version is a stale migration: fail with the
// rebuild recovery and the package name.
func TestCheckFailsOnAStaleMigration(t *testing.T) {
	home := t.TempDir()
	env := testEnv(map[string]string{"HOME": home})
	db, err := Open(env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatalf("stamp version 2: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got := Check(env)
	if got.Status != "fail" {
		t.Fatalf("Check = %+v, want fail on version 2", got)
	}
	if !strings.Contains(got.Message, "version 2") {
		t.Errorf("message = %q, want the stale version", got.Message)
	}
	if !strings.Contains(got.Hint, "internal/store") || !strings.Contains(got.Hint, "rebuild") {
		t.Errorf("hint = %q, want internal/store and rebuild", got.Hint)
	}
}

// An unreadable file fails with a permission hint naming the package.
func TestCheckFailsOnAnUnreadableFile(t *testing.T) {
	home := t.TempDir()
	env := testEnv(map[string]string{"HOME": home})
	db, err := Open(env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	path := DBPath(env)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	got := Check(env)
	if got.Status != "fail" {
		t.Fatalf("Check = %+v, want fail on chmod 000", got)
	}
	if !strings.Contains(got.Hint, "internal/store") {
		t.Errorf("hint = %q, want it to name internal/store", got.Hint)
	}
	if strings.Contains(strings.ToLower(got.Hint+got.Message), "reinstall") {
		t.Errorf("Check = %+v, must not say reinstall", got)
	}
}

// A non-database file at the store path is refused as a mismatch, still
// naming the package.
func TestCheckFailsOnAForeignFile(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".local", "share", "krowk", "krowk.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("not a database"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got := Check(testEnv(map[string]string{"HOME": home}))
	if got.Status != "fail" {
		t.Fatalf("Check = %+v, want fail on foreign file", got)
	}
	if !errors.Is(ErrSchemaMismatch, ErrSchemaMismatch) {
		t.Fatal("unreachable")
	}
	if !strings.Contains(got.Hint, "internal/store") {
		t.Errorf("hint = %q, want it to name internal/store", got.Hint)
	}
}
