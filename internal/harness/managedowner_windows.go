//go:build windows

package harness

import "io/fs"

// Windows answers neither question the Unix file answers.
//
// There is no uid to compare against: ownership is a security descriptor, and
// reading one means the Win32 API rather than anything os.FileInfo carries.
// And while NTFS has hard links, the link count is not in the information a
// Go stat returns, so a refusal based on it would be a refusal based on
// nothing. Both therefore say "no objection" — which is honest, and leaves the
// checks that do work on Windows (the reparse-point refusal and the
// regular-file test on the descriptor) as the whole of the guarantee here.

// ownedByCaller cannot be answered on Windows, so it does not object.
func ownedByCaller(fs.FileInfo) bool { return true }

// hardLinked cannot be answered on Windows, so it does not object.
func hardLinked(fs.FileInfo) bool { return false }
