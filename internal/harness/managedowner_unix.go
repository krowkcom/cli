//go:build !windows

package harness

import (
	"io/fs"
	"os"
	"syscall"
)

// The two questions a *os.FileInfo can answer on Unix and cannot on Windows.
// Both are about the same thing: whether the entry krowk is looking at is
// entirely krowk's to write, or whether somebody else has a claim on it.

// ownedByCaller reports whether info describes something this process's user
// owns. A directory owned by somebody else is not one krowk may claim, even
// when the permission bits happen to allow a write: the other owner can put a
// file there, change the mode, or replace the whole thing at any moment, and
// the marker would then be vouching for a directory krowk does not control.
func ownedByCaller(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// No stat data to judge by. Nothing is proved, so nothing is claimed.
		return false
	}
	return int(st.Uid) == os.Getuid()
}

// hardLinked reports whether info describes a file with more than one name.
// It matters because O_TRUNC does not care which name it arrived by: a second
// link to somebody's file, planted in the name of a managed one, would be
// emptied and rewritten through a path that passed every other test here. The
// link count is the only signal there is — the other names are not knowable
// from this one — so more than one is refused outright.
func hardLinked(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return st.Nlink > 1
}
