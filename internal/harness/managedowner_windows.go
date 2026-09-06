//go:build windows

package harness

import "io/fs"

// Windows cannot answer the question the Unix file answers. Ownership there is
// a security descriptor, and reading one means the Win32 API rather than
// anything an os.FileInfo carries — so a refusal based on it would be a
// refusal based on nothing. It says "no objection" instead, which is honest,
// and leaves the checks that do work on Windows — the reparse-point refusal
// and the regular-file test — as the whole of the guarantee here.

// ownedByCaller cannot be answered on Windows, so it does not object.
func ownedByCaller(fs.FileInfo) bool { return true }
