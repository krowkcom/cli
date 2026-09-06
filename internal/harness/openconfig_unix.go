//go:build !windows

package harness

import (
	"errors"
	"os"
	"syscall"
)

// openConfigFile opens a configuration file without letting the file decide
// how long the open takes or where it lands.
//
// O_NONBLOCK is always set: opening a FIFO otherwise waits for a writer that
// may never come, and a health check that hangs is worse than one that fails.
// An untrusted path adds O_NOFOLLOW, so a final-component symlink is refused
// by the kernel rather than resolved — the caller wanted the file in the
// checkout, not wherever the checkout points.
func openConfigFile(path string, trusted bool) (*os.File, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if !trusted {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		// O_NOFOLLOW reports a refused symlink as ELOOP, which otherwise
		// reads as "too many levels of symbolic links" — true, but not what
		// happened.
		if errors.Is(err, syscall.ELOOP) {
			return nil, errIsSymlink
		}
		// A FIFO opened for reading succeeds under O_NONBLOCK and is caught
		// by the regular-file test; a device that refuses the open this way
		// is not a config either.
		if errors.Is(err, syscall.ENXIO) {
			return nil, errNotRegularFile
		}
		return nil, err
	}
	return f, nil
}
