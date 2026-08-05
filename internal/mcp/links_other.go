//go:build !unix

package mcp

import "io/fs"

// multiplyLinked cannot be answered from a FileInfo off unix, where Sys() carries
// no link count. Hard links exist on Windows but are rare, and creating one needs
// the same local write access that makes the whole boundary moot — so this
// reports false rather than refusing every upload on the platform.
func multiplyLinked(fs.FileInfo) bool { return false }
