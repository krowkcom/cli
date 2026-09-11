//go:build !windows

package cli

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// lockImportFile is flock(2), LOCK_EX|LOCK_NB: exclusive, and refused rather
// than waited on. flock is the right primitive here because the lock is
// released by the kernel when the process dies however it dies — a killed
// import leaves a stale file and no stale lock, which a pidfile would not.
//
// The file is created 0600, matching krowk.db beside it: it names nothing
// secret, but a world-writable lock file is a lock anyone can hold.
func lockImportFile(path string) (io.Closer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		// Every failure of a non-blocking flock that matters here is
		// EWOULDBLOCK — somebody has it. Reporting the errno instead
		// would put "resource temporarily unavailable" in front of a
		// person whose actual situation is "an import is running".
		return nil, errImportLockHeld
	}
	// Closing the file drops the lock, which is why the file handle is the
	// closer rather than something that unlinks the path: unlinking would
	// let a third import create a fresh file and lock that one instead.
	return f, nil
}
