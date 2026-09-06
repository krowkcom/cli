//go:build !windows

package harness

import (
	"io/fs"
	"syscall"
)

// The one question an os.FileInfo can answer on Unix and cannot on Windows:
// whether the entry krowk is looking at belongs to the user krowk is running
// as, or to somebody else who could change it at any moment.

// ownedByCaller reports whether info describes something this process's
// effective user owns — the effective one, because that is the identity the
// filesystem will judge the writes by, and the identity bash's -O test uses,
// so the installer and the binary agree about the same directory. A directory owned by somebody else is not one krowk may claim, even
// when the permission bits happen to allow a write: the other owner can put a
// file there, change the mode, or replace the whole thing at any moment, and
// the marker would then be vouching for a directory krowk does not control.
func ownedByCaller(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// No stat data to judge by. Nothing is proved, so nothing is claimed.
		return false
	}
	return int(st.Uid) == euid()
}
