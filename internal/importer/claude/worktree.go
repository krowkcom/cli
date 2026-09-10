package claude

import (
	"os"
	"path/filepath"
)

// The vcs values a worktree row can carry from this importer.
const (
	vcsGit  = "git"
	vcsNone = "none"
)

// worktreeOf resolves the checkout a session ran in from the directory it
// ran in, by walking up looking for a `.git` entry.
//
// It walks rather than shelling out to `git rev-parse --show-toplevel`, for
// three reasons that all point the same way. Running git means running
// whatever `git` is on the path, in a directory the importer did not choose,
// which is a program execution per session on an import that may touch five
// hundred of them. It also means git's answer depends on the user's config —
// safe.directory, worktree settings — so the same transcript could resolve
// differently on two machines. And the walk gives the right answer for the
// case that actually matters: a `.git` directory, or the `.git` *file* a
// linked worktree carries, both mark a toplevel.
//
// What the walk does not do is follow the gitdir pointer inside a linked
// worktree's `.git` file to the main checkout. That is on purpose: a linked
// worktree is a different checkout with a different branch checked out, and
// collapsing it onto its main repository would file two sessions that shared
// no files under one path.
//
// The walk stats directories the home-trust rule says nothing about, and
// that is deliberate rather than an oversight. importer.OpenHome exists to
// bound what krowk *reads*: a transcript is a file whose contents end up in
// the store, so where it may come from is worth policing. This walk reads
// nothing. It asks whether a name exists, along a path the user's own
// transcript named as the directory the user's own session ran in, and the
// worst a hostile answer can produce is a worktree row pointing at the
// wrong directory — which is a row, not an execution and not a disclosure.
// Refusing to look outside home instead would file every session in a
// checkout under /srv or /opt as having no repository, which is the common
// case being broken to guard against no case at all. The walk is bounded by
// the filesystem root, so a symlink loop cannot spin it: filepath.Dir
// reaches a fixed point in as many steps as the path has components.
//
// A directory with no `.git` above it is not an error and not skipped. The
// store needs a worktree row, so the directory becomes its own, with vcs
// `none` — which says plainly "this ran outside version control" rather than
// inventing a repository or refusing the session.
func worktreeOf(dir string) (path, vcs string) {
	if dir == "" {
		return "", vcsNone
	}
	cur := filepath.Clean(dir)
	for {
		if _, err := os.Lstat(filepath.Join(cur, ".git")); err == nil {
			return cur, vcsGit
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(dir), vcsNone
		}
		cur = parent
	}
}

// baseName is the worktree's display name: the last element of its path. An
// empty path has no name rather than the "." filepath.Base would give it.
func baseName(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}

// fallbackWorktree is the worktree of a session that never recorded a
// working directory.
//
// Those exist. A transcript on a real machine can hold nothing but an
// `ai-title` and an `agent-name` line — a session that was named and then
// never used — and none of the furniture line types carry `cwd`. The store
// requires a worktree path, so such a session would otherwise be the one
// transcript on the box that cannot be ingested at all.
//
// What it does not do is decode the directory slug. `-home-elvinas--buzz`
// could be /home/elvinas/.buzz or /home/elvinas/-buzz or half a dozen other
// things, and a wrong guess does not merely mislabel this session: it
// attaches it to whatever real checkout happens to decode the same way, and
// a session filed under somebody else's repository is worse than a session
// filed under none. So the fallback is the directory the transcript itself
// lives in, which is a path that certainly exists, is unique to the session,
// and cannot collide with a checkout — and its vcs is `none`, because that
// is the truth about it.
func fallbackWorktree(transcriptDir string) (path, vcs string) {
	if transcriptDir == "" {
		return "", vcsNone
	}
	return filepath.Clean(transcriptDir), vcsNone
}
