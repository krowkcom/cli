//go:build !windows

package harness

import (
	"errors"
	"os"
	"syscall"
)

// openManagedFileForWrite opens a file krowk owns for writing, letting the
// kernel — not a check this process made a moment earlier — decide that the
// final component is not a symlink.
//
// O_NOFOLLOW is the whole point: the directories krowk writes into are shared
// with the agent's own files and with whatever the user put there, so a path
// that was a regular file when it was Lstat-ed can be a link by the time it is
// opened. With the flag there is no gap to aim at, and a refused link comes
// back as ELOOP, which is translated here into the refusal callers report.
//
// O_NONBLOCK is there for the other shape that punishes an open: a FIFO in
// the name of a managed file blocks a writer until a reader turns up, and
// nobody is coming. With the flag the kernel answers ENXIO instead, which is
// the refusal below. On a regular file it changes nothing.
//
// The truncation is part of the same open, so the file is never briefly a
// zero-length file under a name something else could claim.
func openManagedFileForWrite(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644) // #nosec G302 -- managed files are documentation an agent must be able to read
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, errIsSymlink
		}
		// A directory in a file's name is not a file krowk wrote either, and
		// the kernel is the one that noticed.
		if errors.Is(err, syscall.EISDIR) {
			return nil, errNotRegularFile
		}
		// A device or FIFO that refuses to be opened this way is not a file
		// krowk wrote either.
		if errors.Is(err, syscall.ENXIO) {
			return nil, errNotRegularFile
		}
		return nil, err
	}
	return f, nil
}
