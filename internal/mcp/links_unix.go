//go:build unix

package mcp

import (
	"io/fs"
	"syscall"
)

// multiplyLinked reports whether a regular file has more than one name.
//
// Resolving symlinks catches a link inside the root pointing out of it, but a
// hard link is not a link to anything — it is a second name for the same inode,
// indistinguishable from the first. So a name inside the root can be the same
// file as a key outside it, and no amount of path resolution will say so. There
// is no way to ask where the other names are, so the answer is to refuse a file
// that has any.
func multiplyLinked(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Nlink > 1
}
