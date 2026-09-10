package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// refreshTimeout bounds the one network call a refresh makes. The hot paths
// never call Refresh, so this timeout only ever delays an explicit refresh.
const refreshTimeout = 5 * time.Second

// metaFileName is the sidecar beside the cached price file: the ETag the last
// fetch stored and when it fetched, so the next refresh can ask
// conditionally with If-None-Match.
const metaFileName = "models.meta.json"

type cacheMeta struct {
	ETag      string `json:"etag"`
	FetchedAt int64  `json:"fetched_at_ms"`
}

// MetaPath is the sidecar path for a cache file: the same directory, the
// meta name. Exported so the CLI can report both files.
func MetaPath(cachePath string) string {
	return filepath.Join(filepath.Dir(cachePath), metaFileName)
}

// Refresh fetches the full models.dev price file into the cache, for env's
// cache path. It is the only function in this package that dials out, and it
// runs only from explicit refreshes — `krowk pricing refresh` today,
// `krowk sessions sync` when sessions land — never from Price or any hot
// path.
//
// Conditional GET: the stored ETag goes out as If-None-Match, and a 304
// leaves models.json untouched (only the fetch time advances). Any network
// failure, timeout, or non-200/304 status is silent and non-fatal: the
// previous file stays as it was and Refresh answers refreshed=false with a
// nil error. Only a filesystem failure writing the new file is a real error.
// A 200 whose body is not a price file is treated like a failure —
// untouched, (false, nil) — so a captive portal can never poison the cache.
//
// client may be nil (a default client with the refresh timeout is built);
// tests hand in a client with a failing transport to prove the hot paths
// never needed it. url overrides ModelsURL when non-empty — tests point it
// at a local server; callers leave it empty for the real file. env may be
// nil, meaning the platform cache dir.
func Refresh(ctx context.Context, env Env, client *http.Client, url string) (refreshed bool, err error) {
	path := CachePath(env)
	if path == "" {
		return false, fmt.Errorf("pricing: no cache directory in environment")
	}
	if client == nil {
		client = &http.Client{Timeout: refreshTimeout}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, refreshTimeout)
		defer cancel()
	}

	var etag string
	if raw, err := os.ReadFile(MetaPath(path)); err == nil {
		var meta cacheMeta
		if json.Unmarshal(raw, &meta) == nil {
			etag = meta.ETag
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, firstNonEmpty(url, ModelsURL), nil)
	if err != nil {
		return false, nil
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotModified:
		stampMeta(path, etag)
		return false, nil
	case http.StatusOK:
		// Into memory first, validated before anything on disk moves: a
		// truncated or portal-served body must not replace good prices.
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if err != nil {
			return false, nil
		}
		if _, err := parseRates(body); err != nil {
			return false, nil
		}
		if err := writeFileAtomic(path, body); err != nil {
			return false, err
		}
		newETag := resp.Header.Get("ETag")
		if newETag == "" {
			newETag = etag
		}
		stampMeta(path, newETag)
		return true, nil
	default:
		return false, nil
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// stampMeta records the ETag and the fetch time. A meta write failure is
// swallowed: the prices are the payload, the sidecar only optimises the
// next refresh, and losing it degrades to one unconditional GET.
func stampMeta(cachePath, etag string) {
	raw, err := json.Marshal(cacheMeta{ETag: etag, FetchedAt: time.Now().UnixMilli()})
	if err != nil {
		return
	}
	_ = writeFileAtomic(MetaPath(cachePath), append(raw, '\n'))
}

// writeFileAtomic lands a file without a half-written window: write aside,
// then rename over. A crash mid-write leaves the previous file, never a
// prefix of the new one — which is what keeps a corrupt cache a case Price
// never sees.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
