//go:build !windows

package harness

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO is the shape that punishes a gate for checking a path and then
// opening it: os.Open blocks until somebody writes, and nobody ever will. Both
// halves of the gate have to answer anyway.

func TestWriteManagedFileRefusesAFIFO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- WriteManagedFile(path, []byte("# krowk\n")) }()
	select {
	case err := <-done:
		if got := unmanaged(t, err); got != path {
			t.Fatalf("refusal names %q, want %q", got, path)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the write blocked on a FIFO nobody is reading from")
	}
}

func TestInstalledVersionDoesNotHangOnAFIFO(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, InstalledVersionFile), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	done := make(chan string, 1)
	go func() { done <- InstalledVersion(dir) }()
	select {
	case got := <-done:
		if got != "" {
			t.Fatalf("InstalledVersion = %q, want \"\"", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the read blocked on a FIFO nobody is writing to")
	}
}

func TestIsManagedCopyDoesNotHangOnAFIFOMarker(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, ManagedMarker), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	done := make(chan bool, 1)
	go func() { done <- IsManagedCopy(dir, "SKILL.md") }()
	select {
	case got := <-done:
		if got {
			t.Fatal("a FIFO in the marker's name vouched for the directory")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the check blocked on a FIFO nobody is writing to")
	}
}
