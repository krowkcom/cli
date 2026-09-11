//go:build windows

package cli

import (
	"errors"
	"io"
)

// lockImportFile refuses. It is unreachable: `krowk sessions import` checks
// the operating system before it resolves a store path, so nothing on
// Windows gets as far as taking a lock. It exists so the package compiles
// under GOOS=windows, and it returns an error rather than a no-op closer
// because a lock that silently does not lock is worse than one that is
// never taken.
func lockImportFile(path string) (io.Closer, error) {
	return nil, errors.New("import locking is not implemented on Windows")
}
