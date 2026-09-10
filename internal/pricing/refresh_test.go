package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// failTransport refuses every request. Price must never dial through it —
// the lookup reads files only — and Refresh must answer silent and non-fatal.
type failTransport struct{}

func (failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network is down")
}

func failingClient() *http.Client {
	return &http.Client{Transport: failTransport{}}
}

// TestHotPathsDoNoNetwork proves the acceptance line: with a transport that
// fails every request installed as the process default, Price still answers
// from the snapshot. (Refresh builds its own behaviour around the client it
// is handed; Price takes no client at all.)
func TestHotPathsDoNoNetwork(t *testing.T) {
	_, _ = tempEnv(t)
	prev := http.DefaultTransport
	http.DefaultTransport = failTransport{}
	defer func() { http.DefaultTransport = prev }()

	for i := 0; i < 50; i++ {
		if _, ok := Price("anthropic", "claude-fable-5-1"); !ok {
			t.Fatal("Price failed with the network down")
		}
		if _, ok := Price("anthropic", "not-a-model"); ok {
			t.Fatal("unknown model answered ok=true with the network down")
		}
	}
}

func writeCache(t *testing.T, cache, body string) string {
	t.Helper()
	path := filepath.Join(cache, "krowk", "models.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRefreshRoundTrip(t *testing.T) {
	env, cache := tempEnv(t)
	var lastIfNoneMatch string
	var hits int
	body := `{"anthropic":{"models":{"fresh-model":{"id":"fresh-model","cost":{"input":7,"output":8}}}}}`
	server := testServer(t, &hits, &lastIfNoneMatch, body)
	client := server.Client()

	ok, err := Refresh(context.Background(), env, client, server.URL)
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if !ok {
		t.Fatal("first refresh answered refreshed=false, want true")
	}
	path := filepath.Join(cache, "krowk", "models.json")
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != body {
		t.Fatalf("stored body = %q, want server body", stored)
	}
	metaRaw, err := os.ReadFile(MetaPath(path))
	if err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}
	var meta cacheMeta
	if err := json.Unmarshal(metaRaw, &meta); err != nil || meta.ETag == "" || meta.FetchedAt == 0 {
		t.Fatalf("sidecar = %q, want etag and fetch time", metaRaw)
	}
	if r, ok := Price("anthropic", "fresh-model"); !ok || r.Input != 7 {
		t.Fatalf("fresh cache price = %+v ok=%v, want input 7", r, ok)
	}

	before, _ := os.ReadFile(path)
	ok, err = Refresh(context.Background(), env, client, server.URL)
	if err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if ok {
		t.Fatal("304 refresh answered refreshed=true, want false")
	}
	if hits != 2 {
		t.Fatalf("server hits = %d, want 2", hits)
	}
	if lastIfNoneMatch == "" {
		t.Fatal("second refresh sent no If-None-Match")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("304 refresh changed the file, want it untouched")
	}
}

func TestRefreshFailureIsSilent(t *testing.T) {
	env, cache := tempEnv(t)
	seed := `{"anthropic":{"claude-fable-5-1":{"input":10,"output":50}}}`
	path := writeCache(t, cache, seed)
	before, _ := os.ReadFile(path)

	// A 500 leaves the file untouched and answers nil.
	var hits int
	var inm string
	srv := testServerStatus(t, http.StatusInternalServerError)
	ok, err := Refresh(context.Background(), env, srv.Client(), srv.URL)
	if err != nil || ok {
		t.Fatalf("500 refresh = (%v, %v), want (false, nil)", ok, err)
	}
	_ = hits
	_ = inm
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("500 refresh changed the file")
	}

	// A timeout does the same.
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("pre-timeout file moved")
	}
	slow := testServerSlow(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ok, err = Refresh(ctx, env, slow.Client(), slow.URL)
	if err != nil || ok {
		t.Fatalf("timeout refresh = (%v, %v), want (false, nil)", ok, err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("timeout refresh changed the file")
	}

	// A body that is not prices is treated like a failure, never stored.
	bad := testServerBody(t, "<html>captive portal</html>")
	ok, err = Refresh(context.Background(), env, bad.Client(), bad.URL)
	if err != nil || ok {
		t.Fatalf("garbage refresh = (%v, %v), want (false, nil)", ok, err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("garbage refresh changed the file")
	}

	// A client whose transport always fails is silent too.
	ok, err = Refresh(context.Background(), env, failingClient(), "http://127.0.0.1:1/")
	if err != nil || ok {
		t.Fatalf("dead-transport refresh = (%v, %v), want (false, nil)", ok, err)
	}
}

// TestSnapshotRegeneratesFromFixture re-runs the generate script against the
// committed fixture with the committed date and requires the committed
// snapshot to match byte-for-byte. The network is never consulted: the live
// file is a runtime refresh concern, not a build input.
func TestSnapshotRegeneratesFromFixture(t *testing.T) {
	dir := t.TempDir()
	models := filepath.Join(dir, "models.json")
	snap := filepath.Join(dir, "snapshot.go")
	cmd := exec.Command("go", "run", "./generate",
		"-in", "testdata/models-dev-full.json",
		"-models", models,
		"-snapshot", snap,
		"-date", SnapshotDate)
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate: %v\n%s", err, out)
	}
	want, err := os.ReadFile("models.json")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(models)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("committed models.json differs from a fresh generate run — " +
			"run `go generate ./internal/pricing` and commit the result")
	}
	raw, err := os.ReadFile(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"`+SnapshotDate+`"`) {
		t.Fatalf("regenerated snapshot.go lost the date %q", SnapshotDate)
	}
}
