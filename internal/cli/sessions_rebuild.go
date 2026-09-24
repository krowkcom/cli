package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/charmbracelet/huh"
	"github.com/mattn/go-isatty"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/runctx"
	"github.com/krowkcom/cli/internal/store"
)

// sessionsRebuild deletes krowk.db and imports every source into a fresh
// one. It is the recovery the schema gate's hint names: through Phase 1 every
// row is re-derivable from the transcripts on disk, so a file at the wrong
// version is not migrated, it is thrown away and read again.
//
// It never calls store.Open before deleting — Open is exactly what refuses a
// mismatched file, and a rebuild that needed Open to work first could never
// run where it is needed. The path comes from store.DBPath and nothing is
// opened until the old file is gone.
//
// Deleting is only done on a clear yes: --yes, or a confirmation naming the
// path when a person is at the terminal. Anywhere else without --yes it
// refuses before touching anything.
func sessionsRebuild(w io.Writer, format output.Format, f flags, env runctx.Env, isTTY bool) error {
	if err := checkImportOS(); err != nil {
		return api.Fail("unsupported_os", unsupportedOSMessage)
	}
	storePath, err := resolveStorePath(env)
	if err != nil {
		return err
	}
	prompt := !f.yes && stdinIsTerminal() && interactive(f, format, env, isTTY)
	if !f.yes && !prompt {
		// The command to run is the only backticked span, because the human
		// renderer offers the first one as "try:" — and the bare command is the
		// one that just refused.
		return api.Fail("confirmation_required", "rebuilding deletes "+storePath+
			" and re-imports every transcript — run `krowk sessions rebuild --yes` to confirm when nobody is at a terminal to ask")
	}

	// Asked before the lock is taken, so a question left on screen does not
	// hold off every import on the machine while it waits.
	if prompt {
		ok := false
		err := huh.NewConfirm().
			Title("Delete " + storePath + " and re-import every transcript?").
			Value(&ok).
			Run()
		if err != nil || !ok {
			return api.Fail("selection_cancelled", "nothing was deleted")
		}
	}

	// The same lock import takes, for the same reason: an import writing
	// into the file while it is deleted would lose its rows, or land them in
	// a file that is about to be unlinked.
	release, err := lockStore(storePath)
	if err != nil {
		return err
	}
	defer release.Close()

	// Exactly the database and its two WAL-mode sidecars, and nothing else
	// in the directory: import.lock is held right now, and anything else
	// there is not krowk's to remove. A missing file is not an error — a
	// rebuild on a machine with no store is just an import.
	removed := []string{}
	for _, path := range []string{storePath, storePath + "-wal", storePath + "-shm"} {
		if err := os.Remove(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return api.Fail("store_unavailable", fmt.Sprintf("remove %s: %v", path, err))
		}
		removed = append(removed, path)
	}

	db, err := store.Open(store.Env(env))
	if err != nil {
		return api.Fail("store_unavailable", sanitizeStoreErr(err, storePath))
	}
	defer db.Close()
	return importInto(w, format, f, env, db, storePath, importSources(), importReport{Removed: &removed})
}

// stdinIsTerminal is whether a confirmation has anyone to read it. stdout
// being a terminal is not enough: `yes | krowk sessions rebuild` has a
// terminal on stdout and nobody answering on stdin. isatty rather than
// ModeCharDevice, because /dev/null is a character device too and
// `</dev/null` is exactly the caller with nobody to answer.
func stdinIsTerminal() bool {
	return isatty.IsTerminal(os.Stdin.Fd())
}
