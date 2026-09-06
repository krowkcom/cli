package harness

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Everything krowk writes into somebody's home directory goes through the gate
// in this file. The directories it writes to — a skills directory, an agent's
// config directory — are shared with the agent's own files and with whatever
// the user put there by hand, and the difference between "krowk wrote this and
// may rewrite it" and "somebody else wrote this and krowk must not touch it"
// cannot be read off the shape of the contents: a hand-authored skill is one
// SKILL.md too. So provenance is written down. A directory krowk created
// carries a marker file, and only a directory carrying that marker is ever
// refreshed, overwritten or removed.
//
// The rules are deliberately few, because a gate with exceptions is a gate
// somebody routes around: create a missing directory, adopt an empty one,
// accept one that is already marked, refuse everything else. Refusal is not a
// failure of krowk — it is krowk declining to destroy what it did not make —
// so callers report it as a thing the person can fix by moving the directory
// aside, which is what UnmanagedError says.
//
// Every check here is an Lstat, never a Stat. A symlink in the name of a
// directory or a file points at something this package never inspected, and a
// write that followed it would land outside everything the gate reasoned
// about. Intermediate directories still resolve — a home directory symlinked
// onto another volume is a normal arrangement, and the gate's promise is about
// the final component it is asked to write.

const (
	// ManagedMarker names the file that says krowk wrote the directory it
	// sits in. Its presence is the whole of the claim: a directory that
	// merely looks like one of krowk's is never rewritten or removed.
	ManagedMarker = ".managed-by-krowk-cli"

	// InstalledVersionFile records which krowk version wrote the directory's
	// contents, so an upgrade can tell a stale copy from a current one
	// without re-reading everything it wrote.
	InstalledVersionFile = ".installed-version"

	// managedMarkerContent is what the marker says to whoever opens it. It is
	// one line, and it is the same line scripts/install.sh writes, so a
	// directory claimed by the shell installer and one claimed by the binary
	// are the same directory.
	managedMarkerContent = "This directory is managed by krowk. Manual edits will be overwritten on upgrade.\n"

	// maxVersionStampBytes bounds the version stamp read. A semantic version
	// is tens of bytes; the bound is there so that a stamp which is not a
	// stamp — a device, something a symlinked home pointed at — cannot be
	// read into memory in full.
	maxVersionStampBytes = 256

	// maxMarkerBytes bounds the marker read, for the same reason: the marker
	// is one sentence, and anything claiming to be one while being much
	// larger is answering a different question.
	maxMarkerBytes = 512
)

// UnmanagedError reports a path krowk did not write and therefore will not
// write to. Callers match it with errors.As, because the remedy is the same
// wherever it comes from and quite different from an I/O failure: nothing is
// broken, and nothing will change until the person decides it should.
type UnmanagedError struct{ Path string }

func (e *UnmanagedError) Error() string {
	return fmt.Sprintf("%s exists but was not written by krowk; move it aside to let krowk write there", e.Path)
}

// mkdirDir is a seam so a test can stage the one race this gate has to handle:
// something else creating the directory between the Lstat and the Mkdir.
var mkdirDir = os.Mkdir

// ClaimDir is the one gate every managed write goes through. It creates a
// missing directory, adopts an empty one, accepts one that already carries the
// marker, and refuses anything else — a populated directory without the marker
// is somebody's own work, and neither a setup command nor an upgrade may
// overwrite it or claim it.
//
// An empty directory is adopted rather than refused because that is what a
// person who ran `mkdir -p ~/.claude/skills/krowk` before running krowk meant,
// and because there is nothing there to lose.
//
// There is deliberately no relaxation here for directories an older krowk
// wrote before the marker existed. scripts/install.sh has one — it adopts a
// directory holding nothing but a regular SKILL.md, which is the only file a
// pre-marker installer ever wrote and the one it overwrote on every run — and
// that handoff runs
// once, in the installer, so this gate never has to carry an exception that
// weakens it for everything else it will be asked to claim.
//
// The marker has to say what krowk writes, not merely exist: this is the
// destructive side of the pair, and a file of some other shape carrying the
// marker's name is not evidence krowk made the directory around it.
//
// The marker is (re)written on every successful claim, not only on creation:
// it costs one small write, and it repairs a directory whose marker was lost
// to a half-finished run or a hand-cleaned checkout.
func ClaimDir(dir string) error {
	if dir == "" {
		return &UnmanagedError{Path: dir}
	}
	// At most two passes. The first may find nothing there and try to create
	// it; if something else won that race, the second inspects what actually
	// landed rather than assuming it was krowk's. There is no third, because
	// a path being created and removed underneath this in a loop is not a
	// state to negotiate with.
	for attempt := 0; attempt < 2; attempt++ {
		info, err := os.Lstat(dir)
		switch {
		case isNotExist(err):
			// The parent chain is created with MkdirAll, but the directory
			// itself with Mkdir: MkdirAll accepts an existing directory
			// silently, which would turn "somebody else created this between
			// the Lstat and now" into "krowk made this" — the one thing this
			// gate exists to tell apart. Mkdir says EEXIST instead, and the
			// next pass asks what is there.
			if mkErr := os.MkdirAll(filepath.Dir(dir), 0o755); mkErr != nil { //nolint:gosec // G301: a skills directory holds public documentation the agent must be able to read
				return fmt.Errorf("creating %s: %w", filepath.Dir(dir), mkErr)
			}
			if mkErr := mkdirDir(dir, 0o755); mkErr != nil { //nolint:gosec // G301: same
				if errors.Is(mkErr, fs.ErrExist) {
					continue
				}
				return fmt.Errorf("creating %s: %w", dir, mkErr)
			}
		case err != nil:
			return fmt.Errorf("inspecting %s: %w", dir, err)
		case info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0:
			// A symlink is the user's arrangement, and its target was never
			// inspected: writing through it would land in a directory this
			// gate reasoned nothing about. An irregular entry is not a
			// directory krowk can reason about at all.
			return &UnmanagedError{Path: dir}
		case !info.IsDir():
			return &UnmanagedError{Path: dir}
		case !ownedByCaller(info):
			// Somebody else's directory. The permission bits may well allow a
			// write; that is not the question. A marker in a directory its
			// owner can empty, re-mode or replace vouches for nothing.
			return &UnmanagedError{Path: dir}
		case !markerIsOurs(dir):
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				return fmt.Errorf("inspecting %s: %w", dir, readErr)
			}
			if len(entries) > 0 {
				return &UnmanagedError{Path: dir}
			}
		}
		return WriteManagedFile(filepath.Join(dir, ManagedMarker), []byte(managedMarkerContent))
	}
	return &UnmanagedError{Path: dir}
}

// WriteManagedFile writes one file krowk owns, refusing to write through a
// symlink or any other non-regular file: a link's target was never inspected
// by the gate, so following it could truncate a file krowk does not own — even
// inside a directory that carries the marker.
//
// It deliberately does not re-check that the parent directory is claimed. The
// two are separate steps because a claim covers a whole install (one ClaimDir,
// then several files), and making every write re-read the marker would say
// nothing new while turning a claimed directory into a per-file syscall.
// Callers claim first; this refuses the one thing a claim cannot cover, which
// is what the final component turned out to be — and it refuses it through the
// kernel, on the descriptor it is about to write, rather than through a check
// something could have invalidated in between.
func WriteManagedFile(path string, data []byte) error {
	f, err := openManagedFileForWrite(path)
	if err != nil {
		if errors.Is(err, errIsSymlink) || errors.Is(err, errNotRegularFile) {
			return &UnmanagedError{Path: path}
		}
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// The open proved the path was not a link. It did not prove what it was:
	// a FIFO or a device that was already sitting there opens perfectly well.
	// The descriptor answers that, and it is the same descriptor about to be
	// written to, so there is nothing left to swap.
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return &UnmanagedError{Path: path}
	}
	// A file reachable by a second name is a file somebody else may be
	// holding: truncating it here would empty theirs through a path that
	// passed every other test. What krowk writes, krowk is the only name for.
	if hardLinked(info) {
		return &UnmanagedError{Path: path}
	}
	// Best-effort: a file that already existed keeps its old permissions
	// through an O_TRUNC open, and the mode krowk documents is the one an
	// agent can read. Windows has no meaningful answer here, so a refusal is
	// not worth failing a write over.
	_ = f.Chmod(0o644)

	// Only now, with the descriptor proved to be a regular file nobody else
	// has a name for, is the old content thrown away.
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncating %s: %w", path, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewinding %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return f.Close()
}

// DirOwned reports whether the marker exists at dir as a regular file, by
// Lstat, so that a symlink or a directory planted in its name confers nothing.
//
// It is the presence question and only the presence question: it does not read
// the marker. That is enough for the health check that uses it, which is
// reporting on what is on disk rather than deciding to overwrite it. Every
// write and every removal asks markerIsOurs instead, which reads the contents
// — a name is cheap to forge and this is the cheap answer.
func DirOwned(dir string) bool {
	return dir != "" && isRegularFile(filepath.Join(dir, ManagedMarker))
}

// StampVersion records which krowk version wrote dir's contents. It is only
// meaningful in a directory ClaimDir accepted, and it goes through the same
// file gate as everything else.
func StampVersion(dir, version string) error {
	return WriteManagedFile(filepath.Join(dir, InstalledVersionFile), []byte(version+"\n"))
}

// InstalledVersion is the version stamp in dir, or "" when there is none, it
// is not a regular file, or it cannot be read. Absence is not an error to
// report: a directory written by an older krowk has no stamp, and the answer
// to "which version is this" is honestly "unknown".
//
// The read is bounded, for the same reason every other read in this package
// is: what is being opened lives in a directory other things can write.
func InstalledVersion(dir string) string {
	if dir == "" {
		return ""
	}
	data, err := readManagedFile(filepath.Join(dir, InstalledVersionFile), maxVersionStampBytes)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readManagedFile reads at most maxBytes of a file krowk owns, refusing
// anything that is not a regular file. It goes through openConfigFile with
// trusted=false, which is what makes it safe to point at a path other things
// can write: O_NOFOLLOW refuses a final-component symlink outright, and
// O_NONBLOCK means a FIFO planted in the name of a stamp or a marker returns
// instead of waiting forever for a writer that is never coming.
func readManagedFile(path string, maxBytes int64) ([]byte, error) {
	f, err := openConfigFile(path, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errNotRegularFile
	}
	return io.ReadAll(io.LimitReader(f, maxBytes))
}

// IsManagedCopy reports whether dir is a plain directory holding nothing but
// files krowk wrote: the marker, the version stamp, and the names in allowed.
// Every entry must be a regular file.
//
// This is the predicate a removal or a refresh must ask before it deletes
// anything. DirOwned answers "may krowk write here", which is enough to add or
// replace a file; it is not enough to remove the directory, because a user may
// have dropped their own file in beside krowk's. All three conditions are load
// bearing: the marker proves provenance — by its contents as well as its name
// (markerIsOurs), since only a file krowk wrote says what krowk writes — and the
// allowlist keeps anything else in the directory safe.
//
// Entry names are compared exactly. On a case-insensitive filesystem that can
// make this and DirOwned disagree about one directory, and the disagreement
// only ever runs one way: DirOwned resolves `.MANAGED-BY-KROWK-CLI` and says
// the directory is writable, while this sees a name that is not on the
// allowlist and says it is not removable. Writable-but-not-removable is the
// safe half of that pair, so it is left as it is.
//
// This answers about a path, not about a descriptor, and the answer is stale
// the moment it is returned. A caller that goes on to delete something must
// re-inspect through a descriptor it holds; that is the residual this
// predicate cannot close and does not pretend to.
func IsManagedCopy(dir string, allowed ...string) bool {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	permitted := map[string]bool{InstalledVersionFile: true}
	for _, name := range allowed {
		permitted[name] = true
	}
	sawMarker := false
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return false
		}
		if entry.Name() == ManagedMarker {
			sawMarker = markerIsOurs(dir)
			continue
		}
		if !permitted[entry.Name()] {
			return false
		}
	}
	return sawMarker
}

// markerIsOurs reports whether dir carries a marker krowk wrote: a regular
// file, read through the same O_NOFOLLOW open as everything else, saying what
// krowk's markers say. It is the question both destructive paths ask — the
// claim that leads to an overwrite, and the copy test that leads to a removal
// — because a marker file that came from somewhere else (copied into a
// dotfile repository, committed to a template, left by a different tool) is
// not evidence krowk created the directory around it.
func markerIsOurs(dir string) bool {
	if dir == "" {
		return false
	}
	data, err := readManagedFile(filepath.Join(dir, ManagedMarker), maxMarkerBytes)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == strings.TrimSpace(managedMarkerContent)
}

// adoptableDir reports whether an unmarked directory holds nothing but files
// a pre-marker krowk wrote — every entry a regular file, every name in
// allowed, and no marker needed or expected.
//
// It is the shape scripts/install.sh adopts on the next run, and the reason
// this exists in Go is so the health check can say so: a person whose skill
// directory predates the marker should be told it will be picked up, not told
// krowk will never touch it. The gate itself does not use it — ClaimDir stays
// strict, and the handoff stays in the installer.
func adoptableDir(dir string, allowed ...string) bool {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || !ownedByCaller(info) {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return false
	}
	permitted := map[string]bool{}
	for _, name := range allowed {
		permitted[name] = true
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !permitted[entry.Name()] {
			return false
		}
	}
	return true
}
