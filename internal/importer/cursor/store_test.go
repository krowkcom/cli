package cursor

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
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

// TestCursorRoundTripInsertsOnlyWhatIsNew is the delta contract held against
// the real writer. Messages carry no foreign id, so the store appends them
// unconditionally — the only thing that keeps a re-import from duplicating
// the session is the caller holding the cursor and never re-sending. A full
// import inserts the session, a resumed import with nothing new inserts
// nothing, and an import after one line is appended inserts exactly that
// line.
func TestCursorRoundTripInsertsOnlyWhatIsNew(t *testing.T) {
	f := newFixture(t)
	_, w := openStore(t)
	ctx := context.Background()
	ref := refByID(t, discover(t, f), fixtureSession)

	th, cur, _ := f.read(t, ref, nil)
	first, err := w.Ingest(ctx, th)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if first.Messages.Inserted != fixtureMessages {
		t.Fatalf("first import inserted %d messages, want %d", first.Messages.Inserted, fixtureMessages)
	}
	if first.Events.Inserted != 1 {
		t.Fatalf("first import inserted %d events, want the repo sidecar", first.Events.Inserted)
	}

	// Nothing new: the delta thread is empty, and ingesting it must insert
	// nothing — no messages, and no second copy of the positional repo
	// event either, which is why full reads alone emit it.
	delta, cur2, _, err := Source{}.Read(f.env, ref, cur)
	if err != nil {
		t.Fatalf("resumed Read: %v", err)
	}
	second, err := w.Ingest(ctx, delta)
	if err != nil {
		t.Fatalf("Ingest delta: %v", err)
	}
	if second.Messages.Inserted != 0 || second.Events.Inserted != 0 || second.Turns.Inserted != 0 {
		t.Fatalf("re-import inserted %+v, want nothing", second)
	}

	// One line appended: exactly one message lands.
	appendLine(t, filepath.Join(f.home, ref.Path),
		`{"role":"user","message":{"content":[{"type":"text","text":"third redacted prompt"}]}}`+"\n")
	grown, _, _, err := Source{}.Read(f.env, ref, cur2)
	if err != nil {
		t.Fatalf("grown Read: %v", err)
	}
	third, err := w.Ingest(ctx, grown)
	if err != nil {
		t.Fatalf("Ingest grown: %v", err)
	}
	if third.Messages.Inserted != 1 {
		t.Fatalf("appended import inserted %d messages, want 1", third.Messages.Inserted)
	}
}

// TestRealTranscriptsImport is the evidence run against the machine's own
// ~/.cursor/projects. It is skipped unless KROWK_CURSOR_REAL=1, because a
// test that reads the developer's transcripts must be asked for: it is slow,
// it is different on every machine, and it would make CI depend on somebody
// having used Cursor.
//
// It asserts almost nothing on purpose. There is no correct answer to hold a
// stranger's transcripts against; what there is, is the two failures worth
// catching — a panic, and a line the importer could neither use nor name.
// The per-type counts it prints are the thing a person reads.
func TestRealTranscriptsImport(t *testing.T) {
	if os.Getenv("KROWK_CURSOR_REAL") != "1" {
		t.Skip("set KROWK_CURSOR_REAL=1 to import this machine's real Cursor transcripts")
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
		// Counted before the error is handled: a read that failed halfway
		// still produced the messages it got to, and leaving them out would
		// make the accounting below fail on a file that was merely
		// truncated.
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
	t.Logf("lines %d = messages %d + events-in-full-reads %d + classified %d + skipped %d",
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
	// Events ride outside the line accounting — the repo sidecar is not a
	// line — and so do worktree-fallback and tool_result:unlinked: the first
	// is classified once per thread rather than once per line, the second
	// against a line that ALSO became a message. A delta test never runs
	// here (every read above is full), but the principle is the same: all
	// three are asserted, not added.
	lines := messages + classified - total.Classified["worktree-fallback"] - total.Classified["tool_result:unlinked"] + total.SkippedCount
	if lines != total.Lines {
		t.Fatalf("%d lines accounted for, %d read", lines, total.Lines)
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
