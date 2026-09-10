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

// maxBody caps the price file: the live file is ~4.5 MB, so 32 MB leaves
// headroom while a runaway body fails closed instead of filling memory.
const maxBody = 32 << 20

// maxETagLen caps the ETag the sidecar stores and the next refresh sends.
// ETags are short response metadata; an unbounded one from a hostile server
// would grow the sidecar and ride along on every future request and proxy
// log. Overlong or non-token values are dropped, degrading to one
// unconditional GET.
const maxETagLen = 4096

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
// client may be nil (a default is built); tests hand in a client with a
// failing transport to prove the hot paths never needed it. Redirects are
// never followed — a 3xx answers like any other unexpected status, silent
// with the file untouched — so a portal or MITM cannot bounce the fetch onto
// an attacker host mid-refresh. url overrides ModelsURL when non-empty —
// tests point it at a local server; callers leave it empty for the real
// file. env may be nil, meaning the platform cache dir. The proxy, if any,
// is the process default (ProxyFromEnvironment): the request carries no
// secrets, only an ETag, and a machine that needs a proxy to reach the
// network needs it here too.
func Refresh(ctx context.Context, env Env, client *http.Client, url string) (refreshed bool, err error) {
	path := CachePath(env)
	if path == "" {
		return false, fmt.Errorf("pricing: no cache directory in environment")
	}
	client = noRedirectClient(client)
	// Always bounded: with a sooner deadline the context's own wins, and a
	// timeout-less client under a bare context can no longer hang the
	// explicit refresh this was called from.
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	etag := loadETag(path)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, firstNonEmpty(url, ModelsURL), nil)
	if err != nil {
		return false, err
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
		// truncated or portal-served body must not replace good prices. The
		// +1 byte tells a body over the cap apart from one exactly at it —
		// LimitReader alone fails silently at the boundary.
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		if err != nil {
			return false, nil
		}
		if len(body) > maxBody {
			return false, nil
		}
		parsed, err := parseRates(body)
		if err != nil || len(parsed) == 0 {
			// Not a price file — captive portal, empty upstream, truncated
			// JSON — so the previous cache stays and nobody is told beyond
			// refreshed=false. An empty map parses without error and would
			// otherwise clobber good prices with nothing.
			return false, nil
		}
		if err := writeFileAtomic(path, body); err != nil {
			return false, err
		}
		newETag := sanitizeETag(resp.Header.Get("ETag"))
		if newETag == "" {
			newETag = etag
		}
		stampMeta(path, newETag)
		return true, nil
	default:
		return false, nil
	}
}

// noRedirectClient clones the client with redirects disabled, preserving its
// transport (test servers, proxies) and timeout. A nil client becomes the
// default with the refresh timeout.
func noRedirectClient(client *http.Client) *http.Client {
	out := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if client != nil {
		out.Transport = client.Transport
		out.Timeout = client.Timeout
	}
	if out.Timeout == 0 {
		out.Timeout = refreshTimeout
	}
	return out
}

// loadETag reads the stored ETag, dropping anything the server should never
// have sent: overlong or non-token bytes degrade to an unconditional GET
// rather than riding along forever.
func loadETag(cachePath string) string {
	raw, err := os.ReadFile(MetaPath(cachePath))
	if err != nil {
		return ""
	}
	var meta cacheMeta
	if json.Unmarshal(raw, &meta) != nil {
		return ""
	}
	return sanitizeETag(meta.ETag)
}

// sanitizeETag keeps printable ASCII token bytes up to the cap: what ETags
// are, and all a conditional GET needs.
func sanitizeETag(etag string) string {
	if len(etag) == 0 || len(etag) > maxETagLen {
		return ""
	}
	for i := 0; i < len(etag); i++ {
		if etag[i] < 0x21 || etag[i] > 0x7e {
			return ""
		}
	}
	return etag
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
