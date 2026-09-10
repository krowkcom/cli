//go:build !windows

package importer

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO under home is not a transcript, and the danger is not that it might
// be read — it is that opening it blocks until somebody writes, which for a
// FIFO nobody is writing to is forever. O_NONBLOCK is what makes this test
// finish; the regular-file check on the descriptor is what makes it refuse.
func TestOpenHomeRefusesFIFOWithoutHanging(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "fifo.jsonl")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("Mkfifo: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		f, err := OpenHome(homeEnv(home), "fifo.jsonl", 0)
		if f != nil {
			f.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegularFile) {
			t.Fatalf("err = %v, want ErrNotRegularFile", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("OpenHome blocked on a FIFO nobody is writing to")
	}
}
