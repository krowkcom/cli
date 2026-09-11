package opencode

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"

	// The read-only opens in this package go through database/sql, so the
	// driver has to be registered somewhere this package reaches. store
	// registers it too, but relying on that transitively would break the
	// day store stopped using SQLite.
	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/krowkcom/cli/internal/harness"
	"github.com/krowkcom/cli/internal/importer"
	"github.com/krowkcom/cli/internal/store"
)

// Harness is the tool that produced the transcript. It is also the model
// vendor fallback: a message naming no providerID is filed under
// "opencode", because the harness drove the model whichever vendor ran it.
const Harness = "opencode"

// dbRel is where opencode keeps its single SQLite database, relative to
// home. It is joined under the home directory through importer.HomePath,
// so a symlink pointing out of home is refused rather than followed — the
// same rule the Claude reader applies to its projects directory.
const dbRel = ".local/share/opencode/opencode.db"

// Source is the opencode importer. It holds nothing: every call takes the
// harness.Env it should resolve home through, so a test points it at a
// temporary directory rather than at the developer's own database.
type Source struct{}

// Source really is one, checked at compile time rather than at the call
// site that first tries to use it as one.
var _ importer.Source = Source{}

// Name is the provider key, shared with the store's binding half and the
// import_state row key.
func (Source) Name() string { return importer.ProviderOpencode }

// Discover lists every session row in the database, one ref per session.
//
// The per-session split is what makes incremental import possible: each
// ref's key is "opencode:<session_id>" and each carries its own SQLite
// watermark, so a session that has not changed is never re-read while one
// that has is. A single ref for the whole database would force every Read
// to return every session, which is not a shape store.Thread can hold.
//
// Being cheap and slightly wrong is allowed here, as with the Claude
// reader: a machine with no database has no transcripts, which is an
// answer (nil, nil), and a session list that cannot be produced from an
// unreadable directory is skipped the same way rather than failing a whole
// import over it. Only a database that exists and cannot be queried is an
// error.
func (s Source) Discover(env harness.Env) ([]importer.Ref, error) {
	if err := importer.CheckOS(); err != nil {
		return nil, fmt.Errorf("opencode: %w", err)
	}
	db, err := importer.HomePath(env, dbRel)
	if err != nil {
		// errors.Is rather than os.IsNotExist: HomePath wraps with %w,
		// and the older predicate does not unwrap.
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("opencode: resolve database: %w", err)
	}
	if _, err := os.Stat(db); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		// A machine whose database cannot be statted for permission
		// reasons is an empty machine, not a failed import: the query
		// path below swallows a permission error the same way, so a
		// chmod in either direction reports the same answer.
		if os.IsPermission(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("opencode: stat database: %w", err)
	}
	ids, err := listSessions(db)
	if err != nil {
		// The file was statted above and is gone now, or was replaced
		// by something that is not this database: either way there is
		// nothing to list, which is the empty-machine answer, not an
		// error worth failing a whole import over.
		if isOpenMissing(err) || os.IsPermission(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("opencode: list sessions: %w", err)
	}
	refs := make([]importer.Ref, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, importer.Ref{
			Provider: s.Name(),
			ID:       id,
			Path:     dbRel,
		})
	}
	return refs, nil
}

// listSessions returns every session id in the database, sorted, through
// one read-only connection that is closed before returning. Sorted so two
// runs on an unchanged database produce the same list, which is what makes
// a golden test of a discovery possible at all.
func listSessions(dbPath string) ([]string, error) {
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT id FROM session ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// isOpenMissing reports the errors a read-only open produces when the
// database file vanished between the stat and the query, or was replaced
// by a file that is not this database: the engine cannot open it, or the
// session table is not there.
func isOpenMissing(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "unable to open") || strings.Contains(s, "no such table") ||
		strings.Contains(s, "file is not a database") || strings.Contains(s, "not a database")
}

// openReadOnly opens one read-only handle to a SQLite file: mode=ro in the
// DSN so the engine cannot write, and a single connection so there is only
// ever one reader to reason about. No pragma is issued on the handle —
// journal_mode especially stays whatever opencode left it as.
//
// The path rides in the DSN's file component, so it is percent-encoded
// through net/url rather than concatenated: a directory carrying ?#& would
// otherwise spill out of the path and override mode=ro (or worse), and an
// encoded path keeps the query exactly "mode=ro".
func openReadOnly(dbPath string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro"}
	db, err := sql.Open(store.DriverName, u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
