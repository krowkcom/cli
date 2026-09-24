package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/krowkcom/cli/internal/api"
	harnessenv "github.com/krowkcom/cli/internal/harness"
	"github.com/krowkcom/cli/internal/importer"
	"github.com/krowkcom/cli/internal/importer/opencode"
	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/pricing"
	"github.com/krowkcom/cli/internal/runctx"
	"github.com/krowkcom/cli/internal/store"
)

// pricingMaxAge is how old the last models.dev answer may be before sync asks
// again. Sync is meant to run hourly; prices do not move hourly.
const pricingMaxAge = 24 * time.Hour

// syncPricingTimeout bounds sync's one network call, tighter than `pricing
// refresh`'s own: a scheduled sync is not a place to wait on a slow network.
const syncPricingTimeout = 3 * time.Second

// syncTransport is the RoundTripper sync's price refresh dials through. nil
// is the default transport; a test swaps in one that fails if it is used, so
// "--no-network makes no request" is a thing that is checked, not hoped.
var syncTransport http.RoundTripper

// syncPricing is what sync did about the price cache. Status is one of
// no_network (--no-network), fresh (asked within pricingMaxAge), refreshed
// (a new file landed), unchanged (models.dev answered, nothing new) or failed.
// A failure is a warning and never fails the sync: the cache or the snapshot
// still prices everything.
type syncPricing struct {
	Status  string `json:"status"`
	Warning string `json:"warning,omitempty"`
}

// sessionsSync is import over every source, reading only the refs that moved
// past their stored cursor, plus the price refresh. Import already writes and
// resumes from cursors; what sync adds is the skip — a JSONL file whose size
// is the size its cursor recorded, or an opencode session with no row updated
// after its watermark, is not read at all — and the network. A ref with no
// import_state row yet is new and is always read, so a new transcript lands
// on the next sync. There is no --since: the cursor is the only watermark.
func sessionsSync(w io.Writer, format output.Format, f flags, env runctx.Env) error {
	if err := checkImportOS(); err != nil {
		return api.Fail("unsupported_os", unsupportedOSMessage)
	}
	storePath := store.DBPath(store.Env(env))
	if storePath == "" {
		_, err := store.Open(store.Env(env))
		if err == nil {
			err = store.ErrNoHome
		}
		return api.Fail("store_unavailable", sanitizeStoreErr(err, storePath))
	}
	// The same lock and the same order as import: taken before store.Open,
	// because opening is itself a write.
	if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
		return api.Fail("store_unavailable", err.Error())
	}
	lockPath := importLockPath(storePath)
	release, err := lockImport(lockPath)
	if err != nil {
		return importLockFailure(lockPath, err)
	}
	defer release.Close()

	db, err := store.Open(store.Env(env))
	if err != nil {
		return api.Fail("store_unavailable", sanitizeStoreErr(err, storePath))
	}
	defer db.Close()

	// The refresh runs inside the lock, so "is a sync running" has one
	// answer. It is bounded at three seconds, which is the most it can hold
	// an import off for; move it outside if that ever matters.
	p := syncPrices(pricing.Env(env), f.noNetwork)
	return importInto(w, format, f, env, db, storePath, syncSources(), importReport{Pricing: &p})
}

// syncSources is importSources with the unchanged check filled in, one per
// cursor kind.
func syncSources() []importSource {
	sources := importSources()
	for i := range sources {
		if sources[i].src.Name() == importer.ProviderOpencode {
			sources[i].unchanged = sqliteUnchanged
		} else {
			sources[i].unchanged = jsonlUnchanged
		}
	}
	return sources
}

// jsonlUnchanged is a file the same size as when its cursor was taken. Growth
// is an append to read; shrinking is a rewrite ReadJSONL rescans from zero.
// A same-size rewrite is invisible, the bargain JSONLCursor documents. Any
// doubt — a stat that fails, a cursor of the wrong kind — reads the ref, and
// Read says what was wrong.
func jsonlUnchanged(env harnessenv.Env, ref importer.Ref, cur importer.Cursor) bool {
	c, ok := cur.(importer.JSONLCursor)
	if !ok || c.Zero() {
		return false
	}
	path, err := importer.HomePath(env, ref.Path)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Size() == c.Size
}

// sqliteUnchanged is an opencode session with no message or part updated
// after the watermark: `WHERE time_updated > ?`.
func sqliteUnchanged(env harnessenv.Env, ref importer.Ref, cur importer.Cursor) bool {
	c, ok := cur.(importer.SQLiteCursor)
	if !ok || c.Zero() {
		return false
	}
	changed, err := opencode.ChangedSince(env, ref, c.TimeUpdated)
	return err == nil && !changed
}

// syncPrices refreshes the models.dev cache when it is due. Due is judged by
// the meta sidecar's mtime rather than models.json's: a 304 rewrites only the
// sidecar, and "when did krowk last ask" is the question. pricing.Refresh is
// silent about a network failure, so a sidecar that did not move is how a
// failure is told apart from a 304.
func syncPrices(env pricing.Env, noNetwork bool) syncPricing {
	if noNetwork {
		return syncPricing{Status: "no_network"}
	}
	cache := pricing.CachePath(env)
	if cache == "" {
		return syncPricing{Status: "failed", Warning: "prices were not refreshed: no cache directory in the environment"}
	}
	meta := pricing.MetaPath(cache)
	var before time.Time
	if info, err := os.Stat(meta); err == nil {
		before = info.ModTime()
		if time.Since(before) < pricingMaxAge {
			return syncPricing{Status: "fresh"}
		}
	}
	refreshed, err := pricing.Refresh(context.Background(), env,
		&http.Client{Transport: syncTransport, Timeout: syncPricingTimeout}, "")
	switch {
	case err != nil:
		return syncPricing{Status: "failed", Warning: "prices were not refreshed: " + err.Error()}
	case refreshed:
		return syncPricing{Status: "refreshed"}
	}
	if info, err := os.Stat(meta); err == nil && info.ModTime().After(before) {
		return syncPricing{Status: "unchanged"}
	}
	return syncPricing{Status: "failed", Warning: "models.dev did not answer with prices within " +
		syncPricingTimeout.String() + " — the cache or the snapshot still prices everything"}
}
