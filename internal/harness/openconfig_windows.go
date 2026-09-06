//go:build windows

package harness

import "os"

// openConfigFile opens a configuration file, enforcing as much of the Unix
// policy as Windows allows.
//
// What is enforced: an untrusted path is refused when its final component is
// a symlink, a junction or any other reparse point, and — on every path,
// trusted or not — the shared test on the descriptor refuses anything that is
// not a regular file.
//
// What is not: there is no O_NOFOLLOW here, so the refusal is a separate
// Lstat before the open rather than something the kernel guarantees. A path
// swapped between the two calls would be followed. Closing that window needs
// FILE_FLAG_OPEN_REPARSE_POINT through the Win32 API, which this package does
// not reach for yet.
//
// That window is not only a health check's problem any more. readManagedFile
// opens through here, markerIsOurs reads the marker through that, and ClaimDir
// and IsManagedCopy decide whether to overwrite or remove a directory on the
// answer — so on Windows an ownership decision rests on a check-then-open, and
// on nothing else, because ownedByCaller has no uid to compare there either.
// The Windows gate is therefore weaker than the Unix one: it still refuses a
// reparse point and anything that is not a regular file, and it still refuses
// a directory whose contents are not krowk's, but it cannot promise that the
// file it read is the file it checked. Nor is there anything to do about
// FIFOs, which do not exist on the filesystem paths these files live at.

func openConfigFile(path string, trusted bool) (*os.File, error) {
	if !trusted {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return nil, errIsSymlink
		}
	}
	return os.Open(path) //nolint:gosec // G304: a path the caller named
}
