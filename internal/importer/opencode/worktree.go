package opencode

import (
	"path/filepath"
)

// The vcs values a worktree row can carry from this importer. Opencode
// records the version control system on the project row itself, so there
// is no checkout walk here the way the Claude reader needs one: the truth
// is read, not derived.
const (
	vcsGit  = "git"
	vcsNone = "none"
)

// resolveWorktree is the worktree of a session: the project row's worktree
// path, with its vcs, defaulting an empty vcs to none. Only git is passed
// through; anything else is none, because the store only knows checkouts
// it can reason about and a novel vcs string would read as a promise. A
// project row that is missing entirely — a session whose project was
// deleted out from under it — falls back to the session's own directory,
// which certainly exists as a path and cannot collide with a checkout,
// the same bargain the Claude reader's fallback makes.
func resolveWorktree(worktree, vcs, directory string) (path, outVCS string) {
	// A non-empty worktree that is not absolute is rejected to the
	// directory fallback: a relative path resolves against whatever the
	// reader's cwd happens to be, which names a different checkout per
	// caller rather than the session's one.
	if worktree == "" || !filepath.IsAbs(worktree) {
		if directory == "" {
			return "", vcsNone
		}
		return filepath.Clean(directory), vcsNone
	}
	worktree = filepath.Clean(worktree)
	if vcs != vcsGit {
		vcs = vcsNone
	}
	return worktree, vcs
}

// baseName is the worktree's display name: the last element of its path. An
// empty path has no name rather than the "." filepath.Base would give it.
func baseName(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}
