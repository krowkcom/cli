package cursor

import (
	"os"
	"path/filepath"
	"strings"
)

// The vcs values a worktree row can carry from this importer.
const (
	vcsGit  = "git"
	vcsNone = "none"
)

// worktreeOf resolves the checkout a session ran in from the project slug.
//
// The slug decodes as "/" + strings.ReplaceAll(slug, "-", "/"), but that
// transform is not invertible — a dash in a real directory name is
// indistinguishable from a separator — so the decoded path is trusted ONLY
// if it is a directory on disk. An existing directory is then walked up
// looking for a `.git` entry, the same walk the Claude reader does; a slug
// that decodes to nothing on this machine is not an error and not skipped.
// The store needs a worktree row, so the slug becomes its own, as
// "cursor:<slug>" with vcs `none` — which says plainly "the transcript named
// no directory this machine still has" rather than inventing a repository.
//
// An empty slug decodes to "/": the root always exists, so without the empty
// check every malformed ref would file under the filesystem root.
func worktreeOf(slug string) (path, vcs string) {
	if slug == "" {
		return "", vcsNone
	}
	decoded := filepath.Clean("/" + strings.ReplaceAll(slug, "-", "/"))
	info, err := os.Stat(decoded)
	if err != nil || !info.IsDir() {
		return "", vcsNone
	}
	cur := decoded
	for {
		if _, err := os.Lstat(filepath.Join(cur, ".git")); err == nil {
			return cur, vcsGit
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return decoded, vcsNone
		}
		cur = parent
	}
}

// baseName is the worktree's display name: the last element of its path. An
// empty path has no name rather than the "." filepath.Base would give it. A
// "cursor:<slug>" fallback keeps the whole string: Base of "cursor:foo" is
// "cursor:foo", which names the slug rather than a directory it never was.
func baseName(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}
