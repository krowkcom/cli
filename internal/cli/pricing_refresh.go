package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/pricing"
	"github.com/krowkcom/cli/internal/runctx"
)

// pricingRefresh refreshes the models.dev price cache: a conditional GET with
// the stored ETag, silent and non-fatal on any network failure. It is the
// only command that dials models.dev — sessions list/show, import and doctor
// price from the snapshot and the cache file and never fetch.
//
// `krowk sessions sync` will call the same pricing.Refresh when sessions
// land; this subcommand exists so a price refresh never requires a sync.
func pricingRefresh(w io.Writer, format output.Format, f flags, env runctx.Env) error {
	penv := pricing.Env(env)
	refreshed, err := pricing.Refresh(context.Background(), penv,
		&http.Client{Timeout: 5 * time.Second}, "")
	if err != nil {
		return err
	}
	path := pricing.CachePath(penv)
	report := map[string]any{
		"refreshed":     refreshed,
		"path":          path,
		"meta_path":     pricing.MetaPath(path),
		"snapshot_date": pricing.SnapshotDate,
		"source":        pricing.ModelsURL,
	}
	if format != output.Human {
		b, _ := json.MarshalIndent(report, "", "  ")
		return emit(w, string(b), f)
	}
	if refreshed {
		fmt.Fprintf(w, "prices refreshed from %s\n", pricing.ModelsURL)
	} else {
		fmt.Fprintf(w, "prices unchanged (snapshot %s)\n", pricing.SnapshotDate)
	}
	fmt.Fprintf(w, "cache: %s\n", path)
	return nil
}
