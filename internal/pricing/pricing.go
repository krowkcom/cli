// Package pricing is the one price source for the cross-harness cost view:
// per-(provider, model) USD per 1M tokens from models.dev.
//
// Two sources, one lookup. The embedded snapshot (models.json, refreshed by
// go generate from the committed models.dev fixture) always works offline.
// The cache file ($XDG_CACHE_HOME/krowk/models.json, refreshed only by an
// explicit refresh — `krowk pricing refresh`, and later `krowk sessions sync`)
// wins when present and parseable, so newer prices apply without a release.
// Lookup order is cache first, embedded second; a corrupt cache falls back to
// embedded without an error.
//
// Price never touches the network: the hot paths (sessions list/show, import,
// doctor) call it per row and must work offline and fast. The snapshot parses
// once per process and the cache file re-parses only when its path or mtime
// changes, so a listing of ten thousand rows parses nothing per row. Refresh
// is the only function that dials out, and it takes its HTTP client as an
// argument — tests hand it a failing transport to prove the hot paths hold.
//
// The environment is injected, never read: Bind sets the lookup the package
// uses (the CLI binds the process environment once at startup), CachePath and
// Refresh take an Env directly, and nothing here reads the process environment. The
// models.dev URL lives in this package and nowhere else.
//
// Prices are provider-specific — Bedrock's us.anthropic.claude-fable-5-1 is
// not Anthropic's claude-fable-5-1 — so every lookup is keyed (provider,
// model), never model alone. Each importer normalises its own model string to
// the models.dev id for its provider before calling Price (see Normalize);
// an unknown pair answers ok=false and the caller renders "—", never 0.
//
// Rates are current, never historical: sessions show footnotes "priced at
// current models.dev rates (snapshot YYYY-MM-DD)" from SnapshotDate.
package pricing

//go:generate go run ./generate -date 2026-09-10

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ModelsURL is where the full price file lives. Only this package names it:
// Refresh fetches it, the generate script can fetch it behind -live, and no
// other package may dial it for prices.
const ModelsURL = "https://models.dev/api.json"

//go:embed models.json
var embeddedJSON []byte

// Env is a lookup function, the same shape internal/store, internal/harness
// and internal/runctx use, so a test moves the cache by handing over a
// different environment instead of touching the process one.
type Env func(string) string

// Rates are USD per 1M tokens for one (provider, model) pair. Reasoning is
// models.dev's reasoning rate when the model publishes one; HasReasoning says
// whether it did, because a published 0 and no published rate price
// differently — without one, reasoning tokens fall back to the output rate.
type Rates struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read"`
	CacheWrite   float64 `json:"cache_write"`
	Reasoning    float64 `json:"reasoning,omitempty"`
	HasReasoning bool    `json:"has_reasoning,omitempty"`
}

// Tokens are the token counts a cost is derived from.
type Tokens struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
}

// Cost derives the USD price: the sum over kinds of tokens/1e6 × rate.
// Reasoning tokens use the reasoning rate when the model publishes one, else
// the output rate. Nothing about the result is persisted: message.cost_usd
// stays source-reported only, and this is computed at display time.
// Negative counts clamp to zero — they are caller bugs, not negative prices —
// and int64 counts convert losslessly only below 2^53, past which the last
// tokens blur: display-time only, so that blur never compounds.
func (r Rates) Cost(t Tokens) float64 {
	const perMillion = 1e6
	reasoning := r.Reasoning
	if !r.HasReasoning {
		reasoning = r.Output
	}
	clamp := func(n int64) float64 {
		if n < 0 {
			return 0
		}
		return float64(n)
	}
	return clamp(t.Input)/perMillion*r.Input +
		clamp(t.Output)/perMillion*r.Output +
		clamp(t.CacheRead)/perMillion*r.CacheRead +
		clamp(t.CacheWrite)/perMillion*r.CacheWrite +
		clamp(t.Reasoning)/perMillion*reasoning
}

// Normalize maps an importer's model string to the models.dev id for its
// provider before lookup. Today it is the identity: Claude JSONL already
// names claude-fable-5-1 under anthropic and Bedrock ARNs arrive as
// us.anthropic.claude-fable-5-1 under amazon-bedrock, both of which are the
// models.dev ids. It exists so the per-importer mappings the plan calls for
// have one seam to grow in when an importer disagrees, rather than each
// call site string-munging on its own.
func Normalize(provider, model string) string {
	return model
}

type key struct {
	provider string
	model    string
}

var (
	mu        sync.Mutex
	bound     Env
	embedded  map[key]Rates
	embedErr  error
	embedOnce sync.Once

	cachePath string
	cacheMod  time.Time
	cacheSize int64
	cached    map[key]Rates
	cacheSeen bool
)

// Bind sets the environment Price resolves the cache file against. The CLI
// calls it once at startup with the process environment; tests bind a fake.
// Unbound, Price falls back to the platform cache dir. The lookup reads
// files only — never the network — and consults only the bound Env for
// where the cache lives (or the platform default when unbound).
func Bind(env Env) {
	mu.Lock()
	defer mu.Unlock()
	bound = env
	cachePath = ""
	cacheMod = time.Time{}
	cacheSize = 0
	cached = nil
	cacheSeen = false
}

// CachePath is where the refreshed price file lives:
// $XDG_CACHE_HOME/krowk/models.json, falling back to ~/.cache/krowk/.
// XDG_CACHE_HOME must be absolute to count (the XDG basedir rule); a missing
// home is an empty string, never a guess — a cache that invented a home
// would write prices onto a machine nobody is using.
func CachePath(env Env) string {
	if env != nil {
		if dir := env("XDG_CACHE_HOME"); filepath.IsAbs(dir) {
			return filepath.Join(dir, "krowk", "models.json")
		}
		if home := env("HOME"); filepath.IsAbs(home) {
			return filepath.Join(home, ".cache", "krowk", "models.json")
		}
		return ""
	}
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "krowk", "models.json")
	}
	return ""
}

// Price answers the rates for one (provider, model) pair: the cache file
// when present and parseable, else the embedded snapshot. The two sources
// fail independently — a corrupt snapshot never hides a good cache, and a
// corrupt cache never hides the snapshot. Unknown pairs answer ok=false —
// the caller renders "—", never 0.
func Price(provider, model string) (Rates, bool) {
	embedOnce.Do(func() {
		embedded, embedErr = parseRates(embeddedJSON)
	})
	mu.Lock()
	defer mu.Unlock()
	if r, ok := lookupLocked(provider, model); ok {
		return r, true
	}
	if embedErr == nil {
		if r, ok := embedded[key{provider, model}]; ok {
			return r, true
		}
	}
	return Rates{}, false
}

// lookupLocked consults the cache file, reloading it only when its path,
// mtime or size moved since the last load. The stat comes first: an
// unchanged file costs one syscall per call, never a read and a parse per
// row — a 10k-row listing over a 4.5 MB cache must not read 45 GB. Misses
// are remembered under the same key, so a missing or corrupt file costs one
// stat per call too; anything appearing, fixed or rewritten under a new key
// reloads on the next call. (A rewrite inside one mtime tick at the same
// size stays stale until the key moves — nanosecond mtimes make that a
// same-nanosecond rewrite, which only a test harness does on purpose.) A
// missing file is not an error — it means no refresh has run yet. A corrupt
// one falls back to embedded without an error, so a half-written refresh can
// never break pricing. The stat-then-read race is benign by construction: a
// writer swapping the file between the two only schedules one extra parse
// on the next call.
func lookupLocked(provider, model string) (Rates, bool) {
	path := CachePath(bound)
	if path == "" {
		return Rates{}, false
	}
	info, statErr := os.Stat(path)
	var mod time.Time
	var size int64
	if statErr == nil {
		mod, size = info.ModTime(), info.Size()
	}
	if cacheSeen && cachePath == path && mod.Equal(cacheMod) && size == cacheSize {
		r, ok := cached[key{provider, model}]
		return r, ok
	}
	var parsed map[key]Rates
	if statErr == nil {
		if raw, err := os.ReadFile(path); err == nil {
			parsed, _ = parseRates(raw)
		}
	}
	cachePath, cacheMod, cacheSize, cached, cacheSeen = path, mod, size, parsed, true
	r, ok := parsed[key{provider, model}]
	return r, ok
}

// parseRates reads either shape prices come in: the trimmed snapshot
// ({provider: {model: cost}}) and the full models.dev file
// ({provider: {models: {id: {cost...}}, ...}}). Each provider decodes on its
// own: a provider that decodes as a full-shape models map is read that way,
// anything else is read as model→cost, and a provider that decodes as
// neither is skipped without failing the rest — one top-level metadata field
// or one renamed provider must never drop every other provider's prices.
// (A snapshot model literally named "models" lands in the second branch as
// long as its value is a cost object, which is why the discriminator tries
// the models decode instead of sniffing the key.) Only the numeric base
// per-token rates are kept — tiered overrides and modality rates are skipped
// value by value, so a refreshed full-file cache with tomorrow's models
// still parses. Extra providers and models pass through: the generate script
// is what trims the snapshot, not this.
func parseRates(raw []byte) (map[key]Rates, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	out := make(map[key]Rates)
	for provider, fieldsRaw := range top {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(fieldsRaw, &fields); err != nil {
			continue
		}
		if models, ok := decodeModels(fields); ok {
			for model, cost := range models {
				if r, ok := ratesFromRaw(cost); ok {
					out[key{provider, model}] = r
				}
			}
			continue
		}
		for model, costRaw := range fields {
			var cost map[string]json.RawMessage
			if err := json.Unmarshal(costRaw, &cost); err != nil {
				continue
			}
			if r, ok := ratesFromRaw(cost); ok {
				out[key{provider, model}] = r
			}
		}
	}
	return out, nil
}

// decodeModels reads a full-shape provider's models map, reporting whether
// the "models" member actually held models with costs. Trying the decode is
// what keeps a cost object that happens to be named "models" on the snapshot
// path instead of vanishing down the full-shape one.
func decodeModels(fields map[string]json.RawMessage) (map[string]map[string]json.RawMessage, bool) {
	modelsRaw, ok := fields["models"]
	if !ok {
		return nil, false
	}
	var models map[string]struct {
		Cost map[string]json.RawMessage `json:"cost"`
	}
	if err := json.Unmarshal(modelsRaw, &models); err != nil {
		return nil, false
	}
	out := make(map[string]map[string]json.RawMessage, len(models))
	for model, m := range models {
		if len(m.Cost) == 0 {
			continue
		}
		out[model] = m.Cost
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// ratesFromRaw keeps the numeric base rates out of a cost object and reports
// whether it found any. Non-numeric members (tiers, context_over_200k) and
// explicit nulls are skipped — a null is an unpublished rate, not a zero
// one, and pricing it at 0 would undercharge. A cost with no numeric rate at
// all is not a price.
func ratesFromRaw(cost map[string]json.RawMessage) (Rates, bool) {
	var r Rates
	found := false
	take := func(name string, dst *float64, mark *bool) {
		raw, ok := cost[name]
		if !ok || string(raw) == "null" {
			return
		}
		var v float64
		if err := json.Unmarshal(raw, &v); err != nil || v < 0 {
			// Negative is not a price either: a hostile or broken cache
			// must not price rows below zero.
			return
		}
		*dst, found = v, true
		if mark != nil {
			*mark = true
		}
	}
	take("input", &r.Input, nil)
	take("output", &r.Output, nil)
	take("cache_read", &r.CacheRead, nil)
	take("cache_write", &r.CacheWrite, nil)
	take("reasoning", &r.Reasoning, &r.HasReasoning)
	return r, found
}
