//go:build windows

package importer

import (
	"fmt"
	"os"
)

// openNoFollow opens path, enforcing as much of the Unix policy as Windows
// allows: a symlink, junction or other reparse point at the final component
// is refused, and the shared regular-file test on the descriptor refuses
// everything else that is not a transcript.
//
// What it cannot promise is that the file it opened is the file it checked.
// There is no O_NOFOLLOW here, so the refusal is an Lstat before the open
// rather than something the kernel guarantees, and a leaf swapped between
// the two calls would be followed. Closing that window needs
// FILE_FLAG_OPEN_REPARSE_POINT through the Win32 API. It is a window this
// package does not currently have to care about, because CheckOS refuses
// Discover on Windows outright — the file exists so the package compiles and
// vets there, and so the policy is written down for whoever lifts that
// refusal.
func openNoFollow(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return nil, fmt.Errorf("%s: %w", path, ErrEscapingSymlink)
	}
	return os.Open(path) //nolint:gosec // G304: a path HomePath vouched for
}
