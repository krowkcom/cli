package pricing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tempEnv binds a fresh empty cache dir and returns it. Every test binds its
// own: the loader caches per path+mtime, so a shared environment would leak
// prices between tests.
func tempEnv(t *testing.T) (Env, string) {
	t.Helper()
	cache := t.TempDir()
	home := t.TempDir()
	Bind(func(k string) string {
		switch k {
		case "XDG_CACHE_HOME":
			return cache
		case "HOME":
			return home
		}
		return ""
	})
	return func(k string) string {
		switch k {
		case "XDG_CACHE_HOME":
			return cache
		case "HOME":
			return home
		}
		return ""
	}, cache
}

func TestEmbeddedLookupOffline(t *testing.T) {
	_, _ = tempEnv(t)
	r, ok := Price("anthropic", "claude-fable-5-1")
	if !ok {
		t.Fatal("embedded Price(anthropic, claude-fable-5-1) answered ok=false")
	}
	if r.Input != 10 || r.Output != 50 {
		t.Fatalf("embedded rates = %+v, want input 10 output 50", r)
	}
	if _, ok := Price("anthropic", "not-a-model"); ok {
		t.Fatal("Price(anthropic, not-a-model) answered ok=true, want false")
	}
	if _, ok := Price("no-such-provider", "claude-fable-5-1"); ok {
		t.Fatal("Price for unknown provider answered ok=true, want false")
	}
}

func TestProviderSpecificity(t *testing.T) {
	_, _ = tempEnv(t)
	bedrock, ok := Price("amazon-bedrock", "us.anthropic.claude-fable-5-1")
	if !ok {
		t.Fatal("embedded bedrock price missing")
	}
	direct, ok := Price("anthropic", "claude-fable-5-1")
	if !ok {
		t.Fatal("embedded anthropic price missing")
	}
	if bedrock == direct {
		t.Fatalf("bedrock and anthropic rates identical (%+v): lookup must key (provider, model)", bedrock)
	}
	if bedrock.Input != 11 || bedrock.Output != 55 {
		t.Fatalf("bedrock rates = %+v, want input 11 output 55", bedrock)
	}
}

func TestCachePrecedenceAndCorruptFallback(t *testing.T) {
	env, cache := tempEnv(t)
	path := filepath.Join(cache, "krowk", "models.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// A cache carrying a different rate wins over the snapshot.
	alt := `{"anthropic":{"claude-fable-5-1":{"input":1,"output":2}}}`
	if err := os.WriteFile(path, []byte(alt), 0o644); err != nil {
		t.Fatal(err)
	}
	if r, ok := Price("anthropic", "claude-fable-5-1"); !ok || r.Input != 1 || r.Output != 2 {
		t.Fatalf("cache precedence: got %+v ok=%v, want input 1 output 2", r, ok)
	}
	// A corrupt cache falls back to embedded without an error.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r, ok := Price("anthropic", "claude-fable-5-1"); !ok || r.Input != 10 {
		t.Fatalf("corrupt fallback: got %+v ok=%v, want embedded input 10", r, ok)
	}
	// And a file that is valid JSON but holds no prices does the same.
	if err := os.WriteFile(path, []byte(`{"unrelated":[1,2]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = env
	if r, ok := Price("anthropic", "claude-fable-5-1"); !ok || r.Input != 10 {
		t.Fatalf("non-price fallback: got %+v ok=%v, want embedded input 10", r, ok)
	}
}

func TestCostMath(t *testing.T) {
	r := Rates{Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5}
	got := r.Cost(Tokens{Input: 1_000_000, Output: 1_000_000})
	if got != 60 {
		t.Fatalf("1M in + 1M out at 10/50 = %v, want 60", got)
	}
	got = r.Cost(Tokens{CacheRead: 1_000_000, CacheWrite: 1_000_000})
	if got != 13.5 {
		t.Fatalf("cache tokens priced %v, want 13.5", got)
	}
	// Reasoning without a published rate falls back to output.
	got = r.Cost(Tokens{Reasoning: 1_000_000})
	if got != 50 {
		t.Fatalf("reasoning fallback priced %v, want output rate 50", got)
	}
	rr := Rates{Input: 10, Output: 50, Reasoning: 3, HasReasoning: true}
	if got := rr.Cost(Tokens{Reasoning: 1_000_000}); got != 3 {
		t.Fatalf("reasoning rate priced %v, want 3", got)
	}
	// A published zero reasoning rate is a rate, not a missing one.
	zr := Rates{Output: 50, Reasoning: 0, HasReasoning: true}
	if got := zr.Cost(Tokens{Reasoning: 1_000_000}); got != 0 {
		t.Fatalf("zero reasoning rate priced %v, want 0", got)
	}
}

func TestCachePath(t *testing.T) {
	env := func(k string) string {
		if k == "XDG_CACHE_HOME" {
			return "/tmp/xdg"
		}
		return ""
	}
	if got := CachePath(env); got != "/tmp/xdg/krowk/models.json" {
		t.Fatalf("XDG path = %q", got)
	}
	rel := func(k string) string {
		if k == "XDG_CACHE_HOME" {
			return "relative/path"
		}
		if k == "HOME" {
			return "/home/u"
		}
		return ""
	}
	if got := CachePath(rel); got != "/home/u/.cache/krowk/models.json" {
		t.Fatalf("relative XDG must be ignored, got %q", got)
	}
	if got := CachePath(func(string) string { return "" }); got != "" {
		t.Fatalf("no home must be empty, got %q", got)
	}
}

func TestNoGetenvInPackage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "os.Getenv") {
			t.Errorf("%s reads os.Getenv: the environment is injected, never read", e.Name())
		}
	}
}

func TestSnapshotBudget(t *testing.T) {
	info, err := os.Stat("models.json")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 60*1024 {
		t.Fatalf("embedded snapshot is %d bytes, over the 60 KB budget", info.Size())
	}
	if SnapshotDate == "" {
		t.Fatal("SnapshotDate is empty")
	}
}

func TestNullRatesAreUnknownNotZero(t *testing.T) {
	_, _ = tempEnv(t)
	parsed, err := parseRates([]byte(`{"anthropic":{"null-model":{"input":null,"output":5}}}`))
	if err != nil {
		t.Fatal(err)
	}
	r, ok := parsed[key{"anthropic", "null-model"}]
	if !ok {
		t.Fatal("model with one null and one rate vanished, want kept")
	}
	if r.Input != 0 {
		t.Fatalf("null input priced %v, want skipped (0 without ok)", r.Input)
	}
	if r.Output != 5 {
		t.Fatalf("output = %v, want 5", r.Output)
	}
	allNull, err := parseRates([]byte(`{"anthropic":{"ghost":{"input":null}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := allNull[key{"anthropic", "ghost"}]; ok {
		t.Fatal("all-null cost parsed as a price — null must mean unpublished, not 0")
	}
}

func TestTopLevelMetadataDoesNotKillProviders(t *testing.T) {
	_, _ = tempEnv(t)
	parsed, err := parseRates([]byte(`{"updated":"2026-09-10","anthropic":{"claude-fable-5-1":{"input":10,"output":50}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := parsed[key{"anthropic", "claude-fable-5-1"}]; !ok || r.Input != 10 {
		t.Fatalf("provider beside top-level metadata lost: %+v ok=%v", r, ok)
	}
}

func TestBindSwitchIsolatesCaches(t *testing.T) {
	envA, cacheA := tempEnv(t)
	pathA := filepath.Join(cacheA, "krowk", "models.json")
	if err := os.MkdirAll(filepath.Dir(pathA), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathA, []byte(`{"anthropic":{"claude-fable-5-1":{"input":1,"output":2}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if r, ok := Price("anthropic", "claude-fable-5-1"); !ok || r.Input != 1 {
		t.Fatalf("env A price = %+v ok=%v, want input 1", r, ok)
	}
	// Rebinding to an empty cache hides A's file entirely: the old path's
	// prices must not leak into the new environment.
	_, _ = tempEnv(t)
	_ = envA
	if r, ok := Price("anthropic", "claude-fable-5-1"); !ok || r.Input != 10 {
		t.Fatalf("after rebind price = %+v ok=%v, want embedded input 10", r, ok)
	}
}

func TestNormalizeIsIdentity(t *testing.T) {
	// The seam the importers grow into: today both houses already speak
	// models.dev ids, so nothing rewrites. Pinned so a future mapping lands
	// here and not scattered across call sites.
	for _, tc := range [][2]string{
		{"anthropic", "claude-fable-5"},
		{"anthropic", "claude-fable-5-1"},
		{"amazon-bedrock", "us.anthropic.claude-fable-5-1"},
	} {
		if got := Normalize(tc[0], tc[1]); got != tc[1] {
			t.Fatalf("Normalize(%q, %q) = %q, want identity", tc[0], tc[1], got)
		}
	}
}
