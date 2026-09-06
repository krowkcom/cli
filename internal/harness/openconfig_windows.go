//go:build windows

package harness

import "os"

// openConfigFile opens a configuration file. Windows has neither O_NOFOLLOW
// nor FIFOs on the filesystem paths these configs live at, so there is nothing
// to guard against at open time; the regular-file test on the descriptor,
// which every platform runs, is the whole of the protection here.
func openConfigFile(path string, _ bool) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: a path the caller named
}
