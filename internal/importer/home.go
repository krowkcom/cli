package importer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/krowkcom/cli/internal/harness"
)

// DefaultMaxBytes bounds one transcript read. Sixty-four mebibytes is far
// more than a session transcript reaches in practice and small enough that
// a file which is not a transcript at all — a growing log, a device, a
// checkout's stray blob — is refused instead of allocated.
const DefaultMaxBytes int64 = 64 << 20

// The refusals a caller reports differently from a plain I/O error. They are
// sentinels rather than one error because they mean different things to the
// person reading: a path outside home is a bug in the caller, an escaping
// symlink is a machine worth looking at, and an oversized file is neither.
var (
	// ErrNoHome is env describing no home directory. Guessing one would
	// mean importing from a directory nobody is using.
	ErrNoHome = errors.New("no home directory in environment")
	// ErrOutsideHome is a resolved path that does not live under the home
	// directory. Home is the only place this package trusts.
	ErrOutsideHome = errors.New("path is outside the home directory")
	// ErrEscapingSymlink is a symlink on the path that does not resolve to
	// somewhere inside home — because its target is elsewhere, or because
	// it does not resolve at all. A checkout can contain a link to
	// anywhere; following it would let whoever wrote the checkout choose
	// what krowk reads.
	ErrEscapingSymlink = errors.New("symlink does not resolve inside the home directory")
	// ErrNotRegularFile is a directory, a FIFO or a device where a
	// transcript was expected.
	ErrNotRegularFile = errors.New("not a regular file")
	// ErrTooLarge is a file past the byte cap.
	ErrTooLarge = errors.New("file is larger than the read limit")
)

// HomePath resolves rel under the home directory env describes and vouches
// that the result is still in there, without reading anything. What comes
// back is the fully resolved path, which is the one a caller should open.
//
// Two checks, and both are needed. The lexical one catches the caller that
// joined a path containing "..", or handed over an absolute path from
// somewhere else entirely. The symlink one catches the path that is under
// home on paper and somewhere else in fact — a `~/.claude/projects` that a
// dotfile repository symlinked into a checkout, say, at which point the
// bytes krowk reads are the checkout's and the trust that let it follow the
// link was the home directory's.
//
// "A checkout is not trusted" is enforced as "outside home is not trusted",
// and the difference is worth being plain about. A symlink from
// ~/.claude/projects to a repository that itself lives under home — say
// ~/Repositories/something — is followed. Home is trusted in full, which is
// the same rule internal/harness applies to configs: a path is untrusted
// because of where it is, not because of what happens to be checked out
// there. Only leaving home is a refusal. Nothing under home can be written
// by anyone who could not equally have written the transcript directory
// itself, so a finer distinction would cost more than it bought.
//
// The home directory is itself resolved before the comparison, because on
// macOS a temporary directory is reached through /var -> /private/var and a
// prefix test against the unresolved path would refuse every read in a test.
//
// A path that does not exist yet resolves as far as it can and is checked on
// what exists: EvalSymlinks fails on a missing leaf, so the leaf is checked
// lexically and its parent chain is checked for real. That is what lets
// HomePath answer for a file a Discover is about to look for. A component
// that exists but does not resolve — a dangling symlink — is refused rather
// than treated as missing; see resolveExisting.
//
// What path resolution cannot see, it does not pretend to. A hard link under
// home to a file outside it is indistinguishable from the file itself: there
// is no path to inspect, because a hard link is not a reference to a path.
// Such a file is read. That is the same bargain as trusting home in the
// first place — creating one requires write access to the home directory,
// and anything with that could simply put the bytes there.
func HomePath(env harness.Env, rel string) (string, error) {
	home := harness.HomeDir(env)
	if home == "" {
		return "", ErrNoHome
	}
	if !filepath.IsAbs(home) {
		// A relative HOME would put the read under whatever directory the
		// process happens to be in, which is the one thing this package
		// promises not to do.
		return "", fmt.Errorf("%w: home %q is not absolute", ErrNoHome, home)
	}
	if filepath.Dir(home) == home {
		// A home directory of "/" (or a Windows drive root) would make
		// "under home" true of every file on the machine, which is the
		// opposite of what this function is for. It is not a home
		// directory anybody has; it is a misconfigured environment.
		return "", fmt.Errorf("%w: home %q is a filesystem root", ErrNoHome, home)
	}

	path := rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(home, path)
	}
	path = filepath.Clean(path)

	if !underDir(home, path) {
		return "", fmt.Errorf("%w: %s", ErrOutsideHome, path)
	}

	// Resolve home once and compare resolved against resolved, so a
	// symlinked home is not mistaken for an escape.
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	realPath, err := resolveExisting(path)
	if err != nil {
		return "", err
	}
	if !underDir(realHome, realPath) {
		return "", fmt.Errorf("%w: %s resolves to %s", ErrEscapingSymlink, path, realPath)
	}
	// The resolved path is what is returned, because it is the path that
	// was vouched for: opening the lexical one would re-follow the links
	// this function just inspected, and a leaf swapped in between would be
	// followed with it. Home is trusted, so a link that stays inside home
	// is fine — a dotfile repository symlinking ~/.claude is ordinary — and
	// what is refused is a link that leaves.
	return realPath, nil
}

// resolveExisting is EvalSymlinks for a path whose leaf may not exist yet.
// The deepest existing ancestor is resolved for real; the components below
// it are then rejoined lexically, which is exact rather than a guess only
// because a component that is not there cannot be a symlink.
//
// That last clause is why the Lstat is here. EvalSymlinks reports a dangling
// symlink as not existing, which is true of its target and false of the link
// — so without the check, a link under home pointing at a checkout would be
// filed as a missing component, rejoined lexically, and vouched for. The
// moment its target appeared, a read that had already been approved would be
// reading outside home. A component that Lstat can see is therefore refused,
// whether it dangles inward or outward: which it is depends on a file that
// does not exist yet, and an answer that changes when somebody else creates
// a file is not an answer.
func resolveExisting(path string) (string, error) {
	missing := []string{}
	cur := path
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(append([]string{resolved}, reverse(missing)...)...), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve %s: %w", cur, err)
		}
		if _, lerr := os.Lstat(cur); lerr == nil {
			// The component is there; only its target is not.
			return "", fmt.Errorf("%w: %s does not resolve", ErrEscapingSymlink, cur)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Walked to the root without finding anything that exists,
			// which on an absolute path means the filesystem is not
			// answering. Nothing to vouch for.
			return "", fmt.Errorf("resolve %s: %w", path, os.ErrNotExist)
		}
		missing = append(missing, filepath.Base(cur))
		cur = parent
	}
}

// reverse returns s back to front, since resolveExisting collects the
// missing components leaf-first and has to rejoin them root-first.
func reverse(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

// underDir reports whether path is dir or lives beneath it. The separator is
// appended before the prefix test so that /home/user-backup is not read as
// being under /home/user.
func underDir(dir, path string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, strings.TrimSuffix(dir, string(os.PathSeparator))+string(os.PathSeparator))
}

// OpenHome opens a transcript under the home directory, refusing rather than
// guessing at every step, and reading nothing until every refusal has had
// its chance.
//
// The order is the point. The path is resolved and vouched for as being
// under home; the file is then opened without following a final-component
// symlink, so the window between the check and the open cannot be used to
// swap the leaf for a link; and only then is the descriptor asked what it
// is and how big. Nothing is read before all of that, so a refusal costs
// one stat rather than a file.
//
// O_NOFOLLOW closes that window for the leaf and only the leaf. An
// intermediate directory replaced by a symlink between the resolve and the
// open would be followed, because the path is walked by the kernel a second
// time and only the last component is guarded. Closing it properly needs an
// openat walk holding a descriptor per component, which is not worth it
// here: every component is under a home directory, and anything able to
// swap one could have written the transcript itself.
//
// maxBytes of zero or less means DefaultMaxBytes. Callers get the open file
// and own closing it — the size check is on the descriptor, so the file they
// hold is the file that was measured.
func OpenHome(env harness.Env, rel string, maxBytes int64) (*os.File, error) {
	f, _, err := openHome(env, rel, maxBytes)
	return f, err
}

// openHome is OpenHome plus the resolved path, so ReadHome can name the same
// path in its errors that OpenHome named in its own — a caller comparing two
// messages about one file should not have to work out that they are about one
// file.
func openHome(env harness.Env, rel string, maxBytes int64) (*os.File, string, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	path, err := HomePath(env, rel)
	if err != nil {
		return nil, "", err
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, "", err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, "", fmt.Errorf("%s: %w", path, ErrNotRegularFile)
	}
	if info.Size() > maxBytes {
		_ = f.Close()
		return nil, "", fmt.Errorf("%s: %w (%d > %d bytes)", path, ErrTooLarge, info.Size(), maxBytes)
	}
	return f, path, nil
}

// ReadHome is OpenHome for a caller that wants the whole file — a SQLite
// source copying a database aside, a small sidecar JSON. A line-oriented
// transcript should go through ReadJSONL against the descriptor instead, so
// a resumed read does not pay for the prefix it is skipping.
func ReadHome(env harness.Env, rel string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	f, path, err := openHome(env, rel, maxBytes)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// One byte past the cap on purpose: it is how a file that grew between
	// the stat and the read is told from one that fit exactly.
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s: %w (over %d bytes)", path, ErrTooLarge, maxBytes)
	}
	return data, nil
}
