//go:build !windows

package harness

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCheckClaudeMCPServerDoesNotHangOnAFIFO(t *testing.T) {
	cwd := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(cwd, ".mcp.json"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	// Nothing will ever write to it. The check must answer anyway.
	done := make(chan StatusCheck, 1)
	go func() { done <- CheckClaudeMCPServer(envFrom(nil), cwd) }()
	select {
	case check := <-done:
		if check.Status != StatusWarn {
			t.Fatalf("check = %+v, want warn", check)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the check blocked on a FIFO nobody is writing to")
	}
}
