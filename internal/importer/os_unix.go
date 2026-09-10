//go:build !windows

package importer

// checkOS permits the read. Every OS krowk imports on today is a Unix, and
// the path and locking assumptions in this package are theirs.
func checkOS() error { return nil }
