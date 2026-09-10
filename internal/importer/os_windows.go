//go:build windows

package importer

// checkOS refuses. See ErrUnsupportedOS: reporting "unsupported" is the
// difference between a user who knows to run this elsewhere and a user who
// believes their machine has no sessions on it.
func checkOS() error { return unsupportedOS("importer") }
