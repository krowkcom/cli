//go:build !windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The lock file is a file krowk owns at a path krowk chose. A symlink
// standing there would have the create-and-lock land on a file somebody else
// named, so it is refused rather than followed — the same rule store.Open
// applies to krowk.db beside it.
func TestLockImportRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	path := filepath.Join(dir, importLockName)
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}

	closer, err := lockImport(path)
	if err == nil {
		closer.Close()
		t.Fatal("a symlinked lock file was taken")
	}
	if _, statErr := os.Lstat(target); statErr == nil {
		t.Errorf("the refused open created %s through the link", target)
	}

	// And a plain path in the same directory still works, so the check is
	// the symlink and not the directory.
	closer, err = lockImport(filepath.Join(dir, "plain.lock"))
	if err != nil {
		t.Fatalf("an ordinary lock file was refused: %v", err)
	}
	closer.Close()
}

// A path that is not a regular file is not a lock to take: flock on a fifo
// serialises nothing anybody asked about.
func TestLockImportRefusesANonRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, importLockName)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("no fifo here: %v", err)
	}

	closer, err := lockImport(path)
	if err == nil {
		closer.Close()
		t.Fatal("a fifo was taken as a lock")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}
