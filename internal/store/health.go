package store

import (
	"errors"
	"fmt"
	"strings"
)

// StatusCheck is one health question and its answer, in the same shape as
// internal/harness.StatusCheck: Message says what is, Hint says what to do
// about it and is present only on failure. Duplicated rather than imported
// so the store keeps no dependency on harness; the field names and JSON tags
// are the contract, and the doctor test pins them.
type StatusCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// CheckName is the store check's name in a doctor report, sibling to the
// harness check names.
const CheckName = "store"

// Check opens the store at DBPath(env) and reports whether the file is
// there, at the current schema version, with the steady-state pragmas
// applied. A fresh home passes: Open initialises the file, stamps
// user_version and persists WAL/NORMAL before the pragma re-check runs, so
// the first doctor on a machine is already green.
//
// Failures name internal/store in the hint, never a reinstall: a missing
// home, an unreadable file and a stale migration are all store problems
// with store recoveries.
func Check(env Env) StatusCheck {
	path := DBPath(env)
	if path == "" {
		return StatusCheck{
			Name:    CheckName,
			Status:  "fail",
			Message: "no home directory in environment, so krowk.db has nowhere to live",
			Hint:    "set HOME (or XDG_DATA_HOME to an absolute path) so krowk.db has a place to live (internal/store)",
		}
	}
	db, err := Open(env)
	if err != nil {
		return fail(path, err)
	}
	defer db.Close()

	var version, syncMode, foreignKeys int
	var journalMode string
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fail(path, fmt.Errorf("read schema version: %w", err))
	}
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return fail(path, fmt.Errorf("read journal_mode: %w", err))
	}
	if err := db.QueryRow(`PRAGMA synchronous`).Scan(&syncMode); err != nil {
		return fail(path, fmt.Errorf("read synchronous: %w", err))
	}
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fail(path, fmt.Errorf("read foreign_keys: %w", err))
	}
	if version != SchemaVersion {
		return StatusCheck{
			Name:    CheckName,
			Status:  "fail",
			Message: fmt.Sprintf("%s schema version %d (want %d)", path, version, SchemaVersion),
			Hint:    fmt.Sprintf("run `krowk sessions rebuild` (delete %s and re-import) (internal/store)", path),
		}
	}
	if !strings.EqualFold(journalMode, "wal") {
		return StatusCheck{
			Name:    CheckName,
			Status:  "fail",
			Message: fmt.Sprintf("%s journal_mode=%s (want wal)", path, journalMode),
			Hint:    "reopen the store to reapply pragmas; if it persists, run `krowk sessions rebuild` (internal/store)",
		}
	}
	// PRAGMA synchronous reads back as a number; NORMAL is 1.
	if syncMode != 1 {
		return StatusCheck{
			Name:    CheckName,
			Status:  "fail",
			Message: fmt.Sprintf("%s synchronous=%d (want 1/NORMAL)", path, syncMode),
			Hint:    "reopen the store to reapply pragmas (internal/store)",
		}
	}
	if foreignKeys != 1 {
		return StatusCheck{
			Name:    CheckName,
			Status:  "fail",
			Message: fmt.Sprintf("%s foreign_keys=%d (want 1)", path, foreignKeys),
			Hint:    "reopen the store to reapply pragmas (internal/store)",
		}
	}
	return StatusCheck{
		Name:    CheckName,
		Status:  "pass",
		Message: fmt.Sprintf("healthy (%s, schema v%d, wal)", path, SchemaVersion),
	}
}

// fail maps an Open (or pragma-read) error onto a StatusCheck whose hint
// names internal/store. A schema mismatch already carries the rebuild
// recovery in the message; the hint still names the package so a reader
// never hears "reinstall krowk" for a store file.
func fail(path string, err error) StatusCheck {
	msg := err.Error()
	if errors.Is(err, ErrNoHome) {
		return StatusCheck{
			Name:    CheckName,
			Status:  "fail",
			Message: msg,
			Hint:    "set HOME (or XDG_DATA_HOME to an absolute path) so krowk.db has a place to live (internal/store)",
		}
	}
	if errors.Is(err, ErrSchemaMismatch) {
		return StatusCheck{
			Name:    CheckName,
			Status:  "fail",
			Message: fmt.Sprintf("%s: %s", path, msg),
			Hint:    fmt.Sprintf("run `krowk sessions rebuild` (delete %s and re-import) (internal/store)", path),
		}
	}
	return StatusCheck{
		Name:    CheckName,
		Status:  "fail",
		Message: fmt.Sprintf("%s: %s", path, msg),
		Hint:    fmt.Sprintf("check permissions on %s (internal/store)", path),
	}
}
