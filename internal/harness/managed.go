package harness

import (
	"fmt"
	"io"
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
)

// UnmanagedError reports a path krowk did not write and therefore will not
// write to. Callers match it with errors.As, because the remedy is the same
// wherever it comes from and quite different from an I/O failure: nothing is
// broken, and nothing will change until the person decides it should.
type UnmanagedError struct{ Path string }

func (e *UnmanagedError) Error() string {
	return fmt.Sprintf("%s exists but was not written by krowk; move it aside to let krowk write there", e.Path)
}

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
// The marker is (re)written on every successful claim, not only on creation:
// it costs one small write, and it repairs a directory whose marker was lost
// to a half-finished run or a hand-cleaned checkout.
func ClaimDir(dir string) error {
	if dir == "" {
		return &UnmanagedError{Path: dir}
	}
	info, err := os.Lstat(dir)
	switch {
	case isNotExist(err):
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil { // #nosec G301 -- a skills directory holds public documentation the agent must be able to read
			return fmt.Errorf("creating %s: %w", dir, mkErr)
		}
	case err != nil:
		return fmt.Errorf("inspecting %s: %w", dir, err)
	case info.Mode()&os.ModeSymlink != 0:
		// A symlink is the user's arrangement, and its target was never
		// inspected: writing through it would land in a directory this gate
		// reasoned nothing about.
		return &UnmanagedError{Path: dir}
	case !info.IsDir():
		return &UnmanagedError{Path: dir}
	case !DirOwned(dir):
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
// is what the final component turned out to be.
func WriteManagedFile(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return &UnmanagedError{Path: path}
		}
	} else if !isNotExist(err) {
		return fmt.Errorf("inspecting %s: %w", path, err)
	}
	return os.WriteFile(path, data, 0o644) // #nosec G306 -- managed files are documentation an agent reads, gated by the Lstat above
}

// DirOwned reports whether krowk wrote the directory at dir. The marker must
// itself be a regular file, by Lstat, so that a symlink or a directory planted
// in the marker's name confers nothing.
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
	path := filepath.Join(dir, InstalledVersionFile)
	if !isRegularFile(path) {
		return ""
	}
	f, err := os.Open(path) // #nosec G304 -- a fixed filename inside a directory the caller named, proved regular above
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxVersionStampBytes))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// IsManagedCopy reports whether dir is a plain directory holding nothing but
// files krowk wrote: the marker, the version stamp, and the names in allowed.
// Every entry must be a regular file.
//
// This is the predicate a removal or a refresh must ask before it deletes
// anything. DirOwned answers "may krowk write here", which is enough to add or
// replace a file; it is not enough to remove the directory, because a user may
// have dropped their own file in beside krowk's. All three conditions are load
// bearing: the marker proves provenance (and a symlink in its name proves
// nothing), and the allowlist keeps anything else in the directory safe.
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
			sawMarker = true
			continue
		}
		if !permitted[entry.Name()] {
			return false
		}
	}
	return sawMarker
}
