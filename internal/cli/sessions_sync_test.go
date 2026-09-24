package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/krowkcom/cli/internal/importer"
	"github.com/krowkcom/cli/internal/store"
)

// claudeFixtureID is the Claude fixture's main session, the one transcript
// these tests append to and cut down.
const claudeFixtureID = "11111111-1111-4111-8111-111111111111"

func claudeFixturePath(home string) string {
	return filepath.Join(home, ".claude", "projects", "-home-elvinas--buzz", claudeFixtureID+".jsonl")
}

// syncEnvelope is import's envelope as sync fills it: the same providers,
// plus files_unchanged and pricing.
type syncEnvelope struct {
	OK   bool `json:"ok"`
	Data struct {
		Providers []struct {
			Provider         string `json:"provider"`
			Files            int    `json:"files"`
			FilesUnchanged   int    `json:"files_unchanged"`
			FilesFailed      int    `json:"files_failed"`
			SessionsInserted int    `json:"sessions_inserted"`
			MessagesInserted int    `json:"messages_inserted"`
		} `json:"providers"`
		Pricing *syncPricing `json:"pricing"`
	} `json:"data"`
}

func mustSync(t *testing.T, h *harness, args ...string) syncEnvelope {
	t.Helper()
	args = append([]string{"sessions", "sync"}, args...)
	code, stdout, stderr := h.run(append(args, "--json")...)
	if code != 0 {
		t.Fatalf("`krowk %s` exited %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, stdout, stderr)
	}
	var e syncEnvelope
	if err := json.Unmarshal([]byte(stdout), &e); err != nil || !e.OK {
		t.Fatalf("not an ok envelope: %v\n%s", err, stdout)
	}
	return e
}

// syncHarness is an import harness with all three fixtures seeded and
// imported once, which is where every sync starts from.
func syncHarness(t *testing.T) (*harness, string, func(string) string) {
	t.Helper()
	h, home := importHarness(t)
	seedClaude(t, home)
	seedCursor(t, home)
	seedOpencode(t, home)
	mustRun(t, h, "sessions", "import", "--from", "all", "--json")
	return h, home, func(k string) string { return h.env[k] }
}

// noNetwork fails the test on any request, so a sync that was meant to stay
// offline and did not is a failure, not a slow test.
type noNetwork struct{ t *testing.T }

func (n noNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	n.t.Errorf("sync made a request to %s", r.URL)
	return nil, errors.New("no network in this test")
}

func withSyncTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	saved := syncTransport
	syncTransport = rt
	t.Cleanup(func() { syncTransport = saved })
}

func jsonlCursorOf(t *testing.T, env func(string) string, key string) importer.JSONLCursor {
	t.Helper()
	c, err := importer.DecodeJSONLCursor(importStateRows(t, env)[key])
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func appendedPrompt(n int) string {
	return fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"userType":"external","sessionId":%q,`+
		`"type":"user","message":{"role":"user","content":"appended prompt %d"},`+
		`"uuid":"eeee%04d-0000-4000-8000-000000000000","timestamp":"2026-09-01T10:00:0%d.000Z"}`,
		claudeFixtureID, n, n, n)
}

func TestSyncAfterAppendingTwoLinesInsertsExactlyTwoMessages(t *testing.T) {
	withSyncTransport(t, noNetwork{t})
	h, home, env := syncHarness(t)
	before := rowCounts(t, env)["message"]

	path := claudeFixturePath(home)
	appendLines(t, path, appendedPrompt(1), appendedPrompt(2))
	e := mustSync(t, h, "--no-network")

	if got := rowCounts(t, env)["message"]; got != before+2 {
		t.Errorf("message rows = %d, want %d + 2", got, before)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if c := jsonlCursorOf(t, env, "claude:"+claudeFixtureID); c.Offset != info.Size() {
		t.Errorf("claude cursor offset = %d, want the new file size %d", c.Offset, info.Size())
	}
	for _, p := range e.Data.Providers {
		// Only the appended file moved; everything else is left unread.
		if want := p.Files - map[string]int{"claude": 1}[p.Provider]; p.FilesUnchanged != want {
			t.Errorf("%s: files_unchanged = %d of %d, want %d", p.Provider, p.FilesUnchanged, p.Files, want)
		}
	}
}

func TestSyncAfterTruncatingToHalfRereadsFromZeroWithoutDuplicates(t *testing.T) {
	withSyncTransport(t, noNetwork{t})
	h, home, env := syncHarness(t)
	before := rowCounts(t, env)["message"]

	path := claudeFixturePath(home)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()/2); err != nil {
		t.Fatal(err)
	}
	mustSync(t, h, "--no-network")

	c := jsonlCursorOf(t, env, "claude:"+claudeFixtureID)
	if c.Size != info.Size()/2 || c.Offset > c.Size {
		t.Errorf("cursor = %+v, want one taken over the %d-byte file from 0", c, info.Size()/2)
	}
	db, err := store.Open(store.Env(env))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var dupes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM (SELECT 1 FROM message WHERE foreign_id IS NOT NULL
	  GROUP BY session_id, foreign_id HAVING COUNT(*) > 1)`).Scan(&dupes); err != nil {
		t.Fatal(err)
	}
	if dupes != 0 {
		t.Errorf("%d foreign ids stored twice", dupes)
	}
	if got := rowCounts(t, env)["message"]; got != before {
		t.Errorf("message rows = %d after re-reading a prefix, want %d", got, before)
	}
}

func TestSyncPicksUpANewTranscriptAndWritesItsImportState(t *testing.T) {
	withSyncTransport(t, noNetwork{t})
	h, home, env := syncHarness(t)
	before := rowCounts(t, env)

	seedClaudeCopies(t, home, 1)
	e := mustSync(t, h, "--no-network")

	after := rowCounts(t, env)
	if after["session"] != before["session"]+1 {
		t.Errorf("sessions = %d, want %d + 1", after["session"], before["session"])
	}
	if after["import_state"] != before["import_state"]+1 {
		t.Errorf("import_state rows = %d, want %d + 1", after["import_state"], before["import_state"])
	}
	if _, ok := importStateRows(t, env)["claude:00000000-1111-4111-8111-111111111111"]; !ok {
		t.Error("the new transcript has no import_state row")
	}
	if p := e.Data.Providers[0]; p.Provider != "claude" || p.SessionsInserted != 1 {
		t.Errorf("claude row = %+v, want one session inserted", p)
	}
}

func TestSyncRereadsOnlyTheOpencodeSessionWhoseMessageWasUpdated(t *testing.T) {
	withSyncTransport(t, noNetwork{t})
	h, home, env := syncHarness(t)
	const bumped = 1757000009999
	child := importStateRows(t, env)["opencode:ses_child"]

	db, err := sql.Open(store.DriverName, "file:"+filepath.Join(home, ".local", "share", "opencode", "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE message SET time_updated = ? WHERE id = 'msg_a2'`, bumped); err != nil {
		t.Fatal(err)
	}
	db.Close()

	e := mustSync(t, h, "--no-network")
	for _, p := range e.Data.Providers {
		if p.Provider == "opencode" && (p.Files != 2 || p.FilesUnchanged != 1) {
			t.Errorf("opencode: %d files, %d unchanged — want 2 and 1", p.Files, p.FilesUnchanged)
		}
	}
	rows := importStateRows(t, env)
	c, err := importer.DecodeSQLiteCursor(rows["opencode:ses_parent"])
	if err != nil {
		t.Fatal(err)
	}
	if c.TimeUpdated != bumped {
		t.Errorf("ses_parent cursor = %d, want it advanced to %d", c.TimeUpdated, bumped)
	}
	if rows["opencode:ses_child"] != child {
		t.Errorf("ses_child cursor moved from %s to %s without a change", child, rows["opencode:ses_child"])
	}
}

func TestSyncNoNetworkMakesNoHTTPCall(t *testing.T) {
	withSyncTransport(t, noNetwork{t})
	h, _, _ := syncHarness(t)
	if e := mustSync(t, h, "--no-network"); e.Data.Pricing == nil || e.Data.Pricing.Status != "no_network" {
		t.Errorf("pricing = %+v, want no_network", e.Data.Pricing)
	}
}

// countingFailure stands in for an unreachable models.dev, and counts, so the
// test above cannot pass by the transport simply never being wired in.
type countingFailure struct{ calls *atomic.Int32 }

func (c countingFailure) RoundTrip(*http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return nil, errors.New("unreachable")
}

// Without --no-network a stale cache is refreshed through the injected
// transport; a failure is a warning on an exit-0 sync, and a fresh cache is
// not asked again.
func TestSyncPriceRefreshFailureIsAWarningAndAFreshCacheIsNotAsked(t *testing.T) {
	var calls atomic.Int32
	withSyncTransport(t, countingFailure{&calls})
	h, home, _ := syncHarness(t)

	e := mustSync(t, h)
	if calls.Load() != 1 {
		t.Errorf("%d requests, want 1 for a missing cache", calls.Load())
	}
	if p := e.Data.Pricing; p == nil || p.Status != "failed" || p.Warning == "" {
		t.Errorf("pricing = %+v, want failed with a warning", p)
	}

	meta := filepath.Join(home, ".cache", "krowk", "models.meta.json")
	if err := os.MkdirAll(filepath.Dir(meta), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(meta, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if e := mustSync(t, h); e.Data.Pricing.Status != "fresh" || calls.Load() != 1 {
		t.Errorf("pricing = %+v after %d requests, want fresh and no new request", e.Data.Pricing, calls.Load())
	}
}

// blockingTransport parks the price refresh — which sync makes inside the
// lock — until the test lets it go, so the lock is known to be held.
type blockingTransport struct{ entered, release chan struct{} }

func (b blockingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	close(b.entered)
	<-b.release
	return nil, errors.New("released")
}

func TestSyncHoldingTheLockMakesAConcurrentImportFailFast(t *testing.T) {
	bt := blockingTransport{entered: make(chan struct{}), release: make(chan struct{})}
	withSyncTransport(t, bt)
	h, _, _ := syncHarness(t)

	done := make(chan int, 1)
	go func() {
		code, _, _ := h.run("sessions", "sync", "--json")
		done <- code
	}()
	<-bt.entered

	code, stdout, stderr := h.run("sessions", "import", "--from", "all", "--json")
	close(bt.release)
	if code != 6 || !strings.Contains(stderr, "import_locked") || !strings.Contains(stderr, "import.lock") {
		t.Errorf("import during sync: exit %d, want 6 import_locked naming import.lock\n%s%s", code, stdout, stderr)
	}
	if code := <-done; code != 0 {
		t.Errorf("the sync exited %d", code)
	}
}

func TestHelpJSONListsSessionsSync(t *testing.T) {
	h := newHarness(t, 0)
	code, stdout, _ := h.run("help", "--json")
	if code != 0 || !strings.Contains(stdout, `"krowk sessions sync [--no-network]"`) {
		t.Errorf("help --json does not carry sessions sync (exit %d)", code)
	}
}

func TestSyncFlagsAreRefusedElsewhere(t *testing.T) {
	h, _ := importHarness(t)
	for _, args := range [][]string{
		{"sessions", "import", "--from", "all", "--no-network"},
		{"sessions", "sync", "--from", "claude"},
		{"sessions", "sync", "--since", "1h"},
		{"sessions", "sync", "--limit", "1"},
	} {
		code, _, stderr := h.run(append(args, "--json")...)
		if code != 1 {
			t.Errorf("`krowk %s`: exit %d, want 1\n%s", strings.Join(args, " "), code, stderr)
		}
	}
}
