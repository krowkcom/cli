//go:build !windows

package harness

import (
	"errors"
	"os"
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

func TestWriteManagedFileRefusesAFileWithASecondName(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "somebody-elses.md")
	if err := os.WriteFile(victim, []byte("# theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "SKILL.md")
	// A hard link is a regular file, is not a symlink, and O_TRUNC through it
	// empties the file at the other name. The link count is the only thing
	// that gives it away.
	if err := os.Link(victim, path); err != nil {
		t.Skipf("link: %v", err)
	}

	var target *UnmanagedError
	if err := WriteManagedFile(path, []byte("# ours\n")); !errors.As(err, &target) {
		t.Fatalf("err = %v, want an *UnmanagedError", err)
	} else if target.Path != path {
		t.Fatalf("refusal names %q, want %q", target.Path, path)
	}
	data, err := os.ReadFile(victim)
	if err != nil || string(data) != "# theirs\n" {
		t.Fatalf("the other name reads %q, %v — it was truncated through the link", data, err)
	}
}
