package harness

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
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

	// managedTempPattern names the sibling every managed write lands in
	// before it is renamed into place. The leading dot keeps it out of the
	// way of anything listing the directory, and the prefix is what
	// scripts/install.sh uses, so a half-finished write from either half of
	// krowk is recognisable as one.
	managedTempPattern = ".krowk-*"

	// maxMarkerBytes bounds the marker read, for the same reason: the marker
	// is one sentence, and anything claiming to be one while being much
	// larger is answering a different question.
	maxMarkerBytes = 512
)

// The reasons a path is not krowk's to write. Each one reads as a sentence
// after the path, because that is how UnmanagedError renders it and how a
// health check quotes it: "the directory belongs to another user" is something
// a person can act on, where "move it aside" would be advice about the wrong
// problem.
const (
	reasonUnmanaged    = "exists but was not written by krowk; move it aside to let krowk write there"
	reasonSymlink      = "is a symlink, so krowk never inspected what it points at"
	reasonNotDir       = "is not a directory"
	reasonIsDir        = "is a directory, not a file krowk wrote"
	reasonNotRegular   = "is not a regular file"
	reasonForeignOwner = "belongs to another user, so krowk is not the one managing it"
)

// UnmanagedError reports a path krowk did not write and therefore will not
// write to. Callers match it with errors.As: nothing is broken, and nothing
// will change until the person decides it should. Reason says which of the
// several ways a path can fail to be krowk's this one is, since the answer
// decides what the person is being asked to do about it.
type UnmanagedError struct {
	Path   string
	Reason string
}

func (e *UnmanagedError) Error() string {
	reason := e.Reason
	if reason == "" {
		reason = reasonUnmanaged
	}
	return fmt.Sprintf("%s %s", e.Path, reason)
}

// claimableDir asks the questions every caller has to ask about a directory
// before it decides anything else about it: is it there, is it really a
// directory rather than a link to one, and does it belong to the user this
// process is running as. Three callers ask them — ClaimDir before it writes,
// adoptableDir before it promises an adoption, CheckClaudeSkill before it
// reports one — and they ask through this so they cannot drift apart and
// start describing the same directory two different ways.
//
// A missing directory comes back as fs.ErrNotExist, which is a fact rather
// than a refusal: only ClaimDir can do anything about it.
func claimableDir(dir string) (fs.FileInfo, error) {
	if dir == "" {
		return nil, &UnmanagedError{Path: dir, Reason: reasonNotDir}
	}
	info, err := os.Lstat(dir)
	switch {
	case err != nil:
		if isNotExist(err) {
			return nil, err
		}
		return nil, fmt.Errorf("inspecting %s: %w", dir, err)
	case info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0:
		// A symlink is the user's arrangement, and its target was never
		// inspected: writing through it would land in a directory this gate
		// reasoned nothing about.
		return nil, &UnmanagedError{Path: dir, Reason: reasonSymlink}
	case !info.IsDir():
		return nil, &UnmanagedError{Path: dir, Reason: reasonNotDir}
	case !ownedByCaller(info):
		// Somebody else's directory. The permission bits may well allow a
		// write; that is not the question. A marker in a directory its owner
		// can empty, re-mode or replace vouches for nothing.
		return nil, &UnmanagedError{Path: dir, Reason: reasonForeignOwner}
	}
	return info, nil
}

// euid is a seam, so a test can ask what happens when a directory belongs to
// somebody else without needing a second user to borrow.
var euid = os.Geteuid

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
		return &UnmanagedError{Path: dir, Reason: reasonNotDir}
	}
	// At most two passes. The first may find nothing there and try to create
	// it; if something else won that race, the second inspects what actually
	// landed rather than assuming it was krowk's. There is no third, because
	// a path being created and removed underneath this in a loop is not a
	// state to negotiate with.
	for attempt := 0; attempt < 2; attempt++ {
		_, err := claimableDir(dir)
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
			return err
		case !markerIsOurs(dir):
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				return fmt.Errorf("inspecting %s: %w", dir, readErr)
			}
			if len(entries) > 0 {
				return &UnmanagedError{Path: dir, Reason: reasonUnmanaged}
			}
			// An empty directory somebody else made may be world-writable —
			// `mkdir -p` under a permissive umask is all it takes. Adopting
			// it as it stands would let any local user drop a file in beside
			// krowk's marker, with the marker vouching for it. The mode is
			// therefore brought to the one krowk creates directories with.
			// Best-effort: on a filesystem or a platform with nothing to say
			// about modes, the claim is still the right answer.
			_ = os.Chmod(dir, 0o755)
		}
		return WriteManagedFile(filepath.Join(dir, ManagedMarker), []byte(managedMarkerContent))
	}
	return &UnmanagedError{Path: dir, Reason: reasonUnmanaged}
}

// WriteManagedFile writes one file krowk owns, atomically, and never onto
// anything it did not write.
//
// The bytes go to a temporary file in the destination's own directory and are
// then renamed onto the final name. Two things follow from that, and both are
// the point. A reader either sees the old file or the whole new one, never a
// half-written one — which matters most for the marker, since a truncated
// marker is a directory krowk would refuse to claim next time. And a rename
// replaces a *name*: it does not follow a symlink, and it does not truncate
// whatever the old name shared an inode with, so a second hard link to
// somebody's file keeps both its content and its own name.
//
// What is still refused rather than replaced is anything at the destination
// krowk did not write — a symlink, a FIFO, a directory. The rename would
// happily replace the first two and would fail on the third, but "refuse what
// we did not write" is the promise this gate makes everywhere else, and a
// managed write is not the place to start making exceptions.
//
// It deliberately does not re-check that the parent directory is claimed. The
// two are separate steps because a claim covers a whole install (one ClaimDir,
// then several files), and making every write re-read the marker would say
// nothing new while turning a claimed directory into a per-file syscall.
// Callers claim first; this refuses the one thing a claim cannot cover, which
// is what the final component turned out to be.
func WriteManagedFile(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil {
		switch {
		case info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0:
			return &UnmanagedError{Path: path, Reason: reasonSymlink}
		case info.IsDir():
			return &UnmanagedError{Path: path, Reason: reasonIsDir}
		case !info.Mode().IsRegular():
			return &UnmanagedError{Path: path, Reason: reasonNotRegular}
		}
	} else if !isNotExist(err) {
		return fmt.Errorf("inspecting %s: %w", path, err)
	}

	// A sibling, because a rename is only atomic within one filesystem and
	// $TMPDIR is regularly on another one. CreateTemp opens with O_EXCL, so
	// the name it returns is one nothing else was holding.
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, managedTempPattern)
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	tmp := f.Name()
	// Every failure from here takes the temporary file with it: one left
	// behind in a skill directory is a file the installer's adoption check
	// would later refuse the whole directory over.
	written := false
	defer func() {
		_ = f.Close()
		if !written {
			_ = os.Remove(tmp)
		}
	}()

	// CreateTemp opens at 0600. An agent has to be able to read what krowk
	// installs, so the mode is set before the file is in place rather than
	// after, and there is never a moment where the final name is unreadable.
	if err := f.Chmod(0o644); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// A directory that appeared at the destination after the Lstat above
		// is the one rename failure that is a refusal rather than a fault.
		// (Only EISDIR: ENOTEMPTY is what rename says when the *source* is a
		// directory, and the source here is a file this call just made.)
		if errors.Is(err, syscall.EISDIR) {
			return &UnmanagedError{Path: path, Reason: reasonIsDir}
		}
		return fmt.Errorf("writing %s: %w", path, err)
	}
	written = true
	return nil
}

// DirOwned reports whether the marker exists at dir as a regular file, by
// Lstat, so that a symlink or a directory planted in its name confers nothing.
//
// It is the presence question and only the presence question: it does not read
// the marker. Nothing in krowk decides anything on it today — it is here for a
// status line that wants to say whether the marker is there at all, cheaply.
// Every write and every removal asks markerIsOurs instead, which reads the
// contents, because a name is cheap to forge and this is the cheap answer.
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
	// One byte past the limit is read on purpose: it is how "a stamp" is told
	// apart from "something much larger wearing a stamp's name", and the
	// larger thing is refused rather than silently truncated into an answer.
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxBytes)
	}
	return data, nil
}

// IsManagedCopy reports whether dir is a plain directory holding nothing but
// files krowk wrote: the marker, the version stamp, and the names in allowed.
// Every entry must be a regular file.
//
// This is the predicate a removal or a refresh must ask before it deletes
// anything, and every condition in it is load bearing. The directory has to be
// a real directory this user owns (claimableDir), because a link or somebody
// else's directory is not krowk's to empty however it is furnished. The marker
// proves provenance by its contents as well as its name (markerIsOurs), since
// only a file krowk wrote says what krowk writes. And the allowlist keeps
// anything a user added alongside krowk's files safe: a directory krowk may
// write into is not automatically a directory krowk may delete.
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
	if _, err := claimableDir(dir); err != nil {
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
//
// On Unix the read is as strong as the decision needs: O_NOFOLLOW means the
// file opened is the file named. On Windows it is a check and then an open,
// with a window between them, and no ownership test to fall back on — see
// openConfigFile there. A claim made on Windows is that much weaker, and it is
// weaker in the direction of trusting a marker somebody swapped, which is why
// the allowlist in IsManagedCopy still has to hold on its own.
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
	if _, err := claimableDir(dir); err != nil {
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
