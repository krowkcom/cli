//go:build windows

package harness

import "os"

// openManagedFileForWrite opens a file krowk owns for writing, enforcing as
// much of the Unix policy as Windows allows.
//
// What is enforced: a final component that is a symlink, a junction or any
// other reparse point is refused, and the shared test on the descriptor
// refuses anything that turned out not to be a regular file.
//
// What is not: there is no O_NOFOLLOW here, so the refusal is an Lstat before
// the open rather than something the kernel guarantees, and a path swapped
// between the two calls would be followed. That window is accepted for the
// same reason openConfigFile accepts it — closing it needs the reparse-point
// flags of the Win32 API — and it is narrower in practice than it reads:
// every path this opens sits inside a directory ClaimDir has already proved
// krowk's own.
func openManagedFileForWrite(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return nil, errIsSymlink
		}
		if info.IsDir() {
			return nil, errNotRegularFile
		}
	} else if !isNotExist(err) {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // G302/G304: a fixed managed filename, documentation an agent reads
}
