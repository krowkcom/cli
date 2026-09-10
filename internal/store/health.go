package store

import (
	"errors"
	"fmt"
	"strings"
)

// StatusCheck is one health question and its answer, in the same shape as
// internal/harness.StatusCheck: Message says what is, Hint says what to do
// about it and is present only on failure. Duplicated rather than imported
// so the store keeps no dependency on harness (see homeDir); the field
// names and JSON tags are the contract, and the doctor test pins them.
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
// applied. Open is the whole gate — it refuses a stale or foreign file, a
// missing home and an unreadable path before returning — so a returned
// handle is the health proof and no version or pragma is re-read here:
// re-reading what Open just enforced would add queries without adding
// signal. A fresh home passes: Open initialises the file, stamps
// user_version and persists WAL/NORMAL, so the first doctor on a machine
// is already green. (Creating the file from a read-only diagnostic is
// the point, not a side effect: doctor is the user-visible proof the file
// lives.)
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
	if err := db.Close(); err != nil {
		return fail(path, fmt.Errorf("close store: %w", err))
	}
	return StatusCheck{
		Name:    CheckName,
		Status:  "pass",
		Message: fmt.Sprintf("healthy (%s, schema v%d, wal)", path, SchemaVersion),
	}
}

// fail maps an Open error onto a StatusCheck whose hint names
// internal/store. A schema mismatch already carries the path and the
// rebuild recovery in the message, so it passes through untouched —
// prefixing the path again would stutter. Anything else names the file
// and the package without guessing the cause: a busy lock and a full
// disk are not permission problems.
func fail(path string, err error) StatusCheck {
	msg := err.Error()
	if errors.Is(err, ErrSchemaMismatch) {
		return StatusCheck{
			Name:    CheckName,
			Status:  "fail",
			Message: msg,
			Hint:    fmt.Sprintf("run `krowk sessions rebuild` (delete %s and re-import) (internal/store)", path),
		}
	}
	if strings.HasPrefix(msg, "close store:") {
		return StatusCheck{
			Name:    CheckName,
			Status:  "fail",
			Message: fmt.Sprintf("%s: %s", path, msg),
			Hint:    fmt.Sprintf("the store opened but did not close cleanly; inspect %s (internal/store)", path),
		}
	}
	return StatusCheck{
		Name:    CheckName,
		Status:  "fail",
		Message: fmt.Sprintf("%s: %s", path, msg),
		Hint:    fmt.Sprintf("inspect %s (internal/store)", path),
	}
}
