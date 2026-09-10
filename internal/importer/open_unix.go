//go:build !windows

package importer

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openNoFollow opens path with the kernel refusing a final-component
// symlink, which is what closes the gap between HomePath resolving the path
// and this call using it: a leaf swapped for a link in between is refused
// here rather than followed.
//
// O_NONBLOCK is set for the same reason internal/harness sets it — opening a
// FIFO otherwise waits for a writer that may never come, and an import that
// hangs is worse than one that fails. The regular-file test on the
// descriptor then rejects it.
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		// O_NOFOLLOW reports a refused symlink as ELOOP, which otherwise
		// reads as "too many levels of symbolic links" — true, and not
		// what happened.
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s: %w", path, ErrEscapingSymlink)
		}
		if errors.Is(err, syscall.ENXIO) {
			return nil, fmt.Errorf("%s: %w", path, ErrNotRegularFile)
		}
		return nil, err
	}
	return f, nil
}
