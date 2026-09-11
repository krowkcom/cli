package cli

import (
	"errors"
	"io"
	"path/filepath"
)

// importLockName is the file whose lock one import holds for its whole run,
// beside krowk.db rather than in /tmp: the thing being serialised is writes
// to that database, so the lock belongs with it. A second store — a
// different HOME, a different XDG_DATA_HOME — is a different import and is
// not blocked by this one.
const importLockName = "import.lock"

// errImportLockHeld is another import already running. It is a sentinel
// rather than a formatted error so the caller can name the lock file itself,
// and so the two build-tagged implementations cannot word it differently.
var errImportLockHeld = errors.New("another import holds the lock")

// importLockPath is the lock beside the store database. dbPath is what
// store.DBPath answered; an empty one has no directory to lock in, which the
// caller has already refused by then.
func importLockPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), importLockName)
}

// lockImport takes the exclusive lock at path and returns the handle to
// close when the import is done. It never waits: a second import is a
// mistake to report, not a queue to join — the first one may run for
// minutes, and a caller left hanging with no output cannot tell krowk from a
// hung filesystem. It returns errImportLockHeld when somebody else has it,
// and a plain error when the file itself could not be opened.
//
// The two implementations are split by build tag rather than the caller
// being, because sessions refuses to run on Windows before it reaches here
// at all — but the package still has to compile there.
func lockImport(path string) (io.Closer, error) { return lockImportFile(path) }
