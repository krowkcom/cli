package claude

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/importer"
	"github.com/krowkcom/cli/internal/store"
)

// openStore opens a real sqlite store in its own temporary home, kept apart
// from the fixture's home so that ingesting transcripts cannot be confused
// with reading them.
func openStore(t *testing.T) (*sql.DB, *store.Writer) {
	t.Helper()
	home := t.TempDir()
	db, err := store.Open(func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, store.NewWriter(db, nil)
}

// TestIngestingTheFixtureTwiceInsertsNothingTheSecondTime is the
// idempotency acceptance, held against the real writer rather than against
// a fake: the store dedups messages on (session_id, foreign_id) and treats
// turns and events as positional prefixes, and this importer only converges
// if it produces the same foreign ids and the same cumulative lists on
// every read.
func TestIngestingTheFixtureTwiceInsertsNothingTheSecondTime(t *testing.T) {
	f := newFixture(t)
	_, w := openStore(t)
	ctx := context.Background()
	refs := discover(t, f)

	var first store.Result
	for _, ref := range refs {
		th, _, _ := f.read(t, ref)
		res, err := w.Ingest(ctx, th)
		if err != nil {
			t.Fatalf("Ingest %s: %v", ref.ID, err)
		}
		first = addResults(first, res)
	}
	if first.Messages.Inserted == 0 || first.Turns.Inserted == 0 || first.Events.Inserted == 0 {
		t.Fatalf("first import inserted nothing worth having: %+v", first)
	}

	var second store.Result
	for _, ref := range refs {
		th, _, _ := f.read(t, ref)
		res, err := w.Ingest(ctx, th)
		if err != nil {
			t.Fatalf("re-Ingest %s: %v", ref.ID, err)
		}
		second = addResults(second, res)
	}
	if got := (counts{
		second.Worktrees.Inserted, second.Sessions.Inserted, second.Bindings.Inserted,
		second.Turns.Inserted, second.Events.Inserted, second.Messages.Inserted,
		second.Parts.Inserted,
	}); got != (counts{}) {
		t.Fatalf("re-importing inserted %+v, want nothing", got)
	}
}

// counts is the seven insert totals as one comparable value, so the
// assertion above is one line rather than seven.
type counts struct{ worktrees, sessions, bindings, turns, events, messages, parts int }

func addResults(a, b store.Result) store.Result {
	a.Worktrees.Inserted += b.Worktrees.Inserted
	a.Sessions.Inserted += b.Sessions.Inserted
	a.Bindings.Inserted += b.Bindings.Inserted
	a.Turns.Inserted += b.Turns.Inserted
	a.Events.Inserted += b.Events.Inserted
	a.Messages.Inserted += b.Messages.Inserted
	a.Parts.Inserted += b.Parts.Inserted
	return a
}

// TestRealTranscriptsImport is the evidence run against the machine's own
// ~/.claude/projects. It is skipped unless KROWK_CLAUDE_REAL=1, because a
// test that reads the developer's transcripts must be asked for: it is slow,
// it is different on every machine, and it would make CI depend on somebody
// having used Claude.
//
// It asserts almost nothing on purpose. There is no correct answer to hold
// a stranger's transcripts against; what there is, is the two failures worth
// catching — a panic, and a line the importer could neither use nor name.
// The per-type counts it prints are the thing a person reads.
func TestRealTranscriptsImport(t *testing.T) {
	if os.Getenv("KROWK_CLAUDE_REAL") != "1" {
		t.Skip("set KROWK_CLAUDE_REAL=1 to import this machine's real Claude transcripts")
	}
	env := os.Getenv
	refs, err := Source{}.Discover(env)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	t.Logf("discovered %d transcripts", len(refs))

	_, w := openStore(t)
	ctx := context.Background()
	var (
		total       importer.Result
		messages    int
		events      int
		turns       int
		sessions    int
		readErrors  int
		ingestFails int
	)
	for _, ref := range refs {
		th, _, res, err := Source{}.Read(env, ref, nil)
		total.Merge(res)
		// Counted before the error is handled: a read that failed
		// halfway still produced the messages it got to, and leaving
		// them out would make the accounting below fail on a file that
		// was merely truncated.
		messages += len(th.Messages)
		events += len(th.Events)
		turns += len(th.Turns)
		if err != nil {
			readErrors++
			t.Logf("read %s: %v", ref.Key(), err)
			continue
		}
		if _, err := w.Ingest(ctx, th); err != nil {
			ingestFails++
			t.Logf("ingest %s: %v", ref.Key(), err)
			continue
		}
		sessions++
	}

	classified := 0
	for _, n := range total.Classified {
		classified += n
	}
	t.Logf("lines %d = messages %d + events %d + classified %d + skipped %d",
		total.Lines, messages, events, classified, total.SkippedCount)
	t.Logf("sessions ingested %d, turns %d, read errors %d, ingest failures %d",
		sessions, turns, readErrors, ingestFails)
	for _, kv := range sortedCounts(total.Classified) {
		t.Logf("classified %-28s %d", kv.key, kv.n)
	}
	for _, kv := range sortedCounts(total.UnknownTypes) {
		t.Logf("unknown block %-25s %d", kv.key, kv.n)
	}
	for i, s := range total.Skipped {
		if i >= 10 {
			break
		}
		t.Logf("skipped line %d at offset %d: %s", s.Line, s.Offset, s.Reason)
	}
	if got := messages + events + classified + total.SkippedCount; got != total.Lines {
		t.Fatalf("%d lines accounted for, %d read", got, total.Lines)
	}
}

type countPair struct {
	key string
	n   int
}

func sortedCounts(m map[string]int) []countPair {
	out := make([]countPair, 0, len(m))
	for k, v := range m {
		out = append(out, countPair{k, v})
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].n > out[i].n || (out[j].n == out[i].n && out[j].key < out[i].key) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// turnCosts reads a session's turn costs out of the store in seq order.
func turnCosts(t *testing.T, db *sql.DB, foreignID string) [][2]int64 {
	t.Helper()
	rows, err := db.Query(
		`SELECT t.cost_input_tokens, t.cost_total_tokens FROM turn t
		 JOIN session_binding b ON b.session_id = t.session_id
		 WHERE b.provider = ? AND b.foreign_session_id = ? ORDER BY t.seq`,
		importer.ProviderClaude, foreignID)
	if err != nil {
		t.Fatalf("read turns: %v", err)
	}
	defer rows.Close()
	var out [][2]int64
	for rows.Next() {
		var in, total int64
		if err := rows.Scan(&in, &total); err != nil {
			t.Fatalf("scan turn: %v", err)
		}
		out = append(out, [2]int64{in, total})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read turns: %v", err)
	}
	return out
}

// truncate rewrites a fixture transcript to its first n lines, standing in
// for a session that was still being worked on when the importer ran.
func truncate(t *testing.T, path string, n int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	if n > len(lines) {
		t.Fatalf("fixture has %d lines, cannot truncate to %d", len(lines), n)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines[:n], "")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// TestImportingALiveSessionConvergesOnTheFinalCosts is the end-to-end of
// the store's turn refresh: a transcript caught mid-turn is imported, then
// the finished transcript is imported over it, and the stored costs have to
// be the ones a single import of the finished file would have produced. It
// is the case every real machine hits, because a person imports while using
// the agent, and it was silently wrong before the last stored turn became
// refreshable.
func TestImportingALiveSessionConvergesOnTheFinalCosts(t *testing.T) {
	ctx := context.Background()

	// What the finished transcript is worth, imported in one go.
	whole := newFixture(t)
	wholeDB, wholeW := openStore(t)
	wholeRef := refByID(t, discover(t, whole), fixtureSession)
	th, _, _ := whole.read(t, wholeRef)
	if _, err := wholeW.Ingest(ctx, th); err != nil {
		t.Fatalf("Ingest whole: %v", err)
	}
	want := turnCosts(t, wholeDB, fixtureSession)
	if len(want) != fixturePrompts+1 {
		t.Fatalf("one-shot import produced %d turns, want %d", len(want), fixturePrompts+1)
	}

	// The same session imported twice: once cut off in the middle of the
	// first real turn, once complete.
	live := newFixture(t)
	liveDB, liveW := openStore(t)
	ref := refByID(t, discover(t, live), fixtureSession)
	path := filepath.Join(live.home, ref.Path)
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Seven lines is partway through the first prompt's tool loop: the
	// prompt has landed and one of its two assistant messages has, so the
	// turn is real and its cost is a fraction of the final one.
	truncate(t, path, 7)

	partial, _, _ := live.read(t, ref)
	if _, err := liveW.Ingest(ctx, partial); err != nil {
		t.Fatalf("Ingest partial: %v", err)
	}
	mid := turnCosts(t, liveDB, fixtureSession)
	if len(mid) != 2 || mid[1][1] == want[1][1] {
		t.Fatalf("the truncated import produced %v, which is not a session caught mid-turn", mid)
	}

	if err := os.WriteFile(path, full, 0o600); err != nil {
		t.Fatalf("restore fixture: %v", err)
	}
	grown, _, _ := live.read(t, ref)
	if _, err := liveW.Ingest(ctx, grown); err != nil {
		t.Fatalf("Ingest grown: %v", err)
	}

	got := turnCosts(t, liveDB, fixtureSession)
	if len(got) != len(want) {
		t.Fatalf("got %d turns, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("turn %d = %v after two imports, want %v as one import produces", i, got[i], want[i])
		}
	}
}
