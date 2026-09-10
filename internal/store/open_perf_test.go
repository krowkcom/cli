package store

import (
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"
)

// messageCountQuery is the listing-shape probe for the 10k fixture: a COUNT
// that must never touch blob columns. It is a constant so the gate pins the
// exact text — a future listing that SELECTs raw_json, SELECTs *, or joins
// part cannot drift in behind a variable. No production listing reads these
// rows yet; this pins the shape the first listing must keep, the way the
// schema tests pin DDL before any command writes the tables.
const messageCountQuery = `SELECT COUNT(*) FROM message`

// perfMessageCount is the fixture size the todo names: 10k messages, each
// with one part carrying a ~1KB blob, so the count probe runs against a file
// where touching blobs would actually cost.
const perfMessageCount = 10000

// coldOpenBudget is the acceptance threshold: a cold Open on the 10k fixture
// must finish under 50ms. Under -short the budget loosens to 500ms — loaded
// CI runners flake on tight wall-clock asserts, and a slow open there is
// noise, not a regression signal. The correctness probes (row counts, query
// shape) run at full strength in both modes.
func coldOpenBudget(short bool) time.Duration {
	if short {
		return 500 * time.Millisecond
	}
	return 50 * time.Millisecond
}

// perfPayloads returns the two ~1KB blobs the fixture stores: raw_json on
// message and data on part. One builder serves the test and both benchmarks
// so the measured blob shape cannot diverge between them.
func perfPayloads() (rawJSON, partData string) {
	return `{"text":"` + strings.Repeat("x", 896) + `"}`, `{"text":"` + strings.Repeat("y", 896) + `"}`
}

// seedPerfSession inserts the one worktree and session the fixture hangs off
// and returns the session id. Shared by the test and both benchmarks.
func seedPerfSession(tb testing.TB, db *sql.DB) string {
	tb.Helper()
	wID := NewID()
	if _, err := db.Exec(`INSERT INTO worktree (id, path, time_created, time_updated) VALUES (?, '/perf', 1, 2)`, wID); err != nil {
		tb.Fatalf("insert perf worktree: %v", err)
	}
	sID := NewID()
	if _, err := db.Exec(`INSERT INTO session (id, worktree_id, directory, title, time_created, time_updated) VALUES (?, ?, '/r', 'perf', 1, 2)`, sID, wID); err != nil {
		tb.Fatalf("insert perf session: %v", err)
	}
	return sID
}

// buildPerfFixture fills db with perfMessageCount messages plus one blob part
// each inside a single transaction. Shared by the test and both benchmarks
// so the blob shape and row counts cannot diverge between them. Fixture setup
// is never part of the measured window: callers build, checkpoint, close,
// and only then start the clock on Open.
func buildPerfFixture(tb testing.TB, db *sql.DB, sessionID string) {
	tb.Helper()

	rawPayload, partPayload := perfPayloads()

	tx, err := db.Begin()
	if err != nil {
		tb.Fatalf("begin perf fixture: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	msgStmt, err := tx.Prepare(`INSERT INTO message (id, session_id, turn_id, seq, role, raw_json, time_created) VALUES (?, ?, NULL, ?, 'user', ?, 1)`)
	if err != nil {
		tb.Fatalf("prepare message: %v", err)
	}
	defer msgStmt.Close()
	partStmt, err := tx.Prepare(`INSERT INTO part (id, message_id, session_id, seq, type, data) VALUES (?, ?, ?, 0, 'text', ?)`)
	if err != nil {
		tb.Fatalf("prepare part: %v", err)
	}
	defer partStmt.Close()

	for i := 0; i < perfMessageCount; i++ {
		msgID := NewID()
		if _, err := msgStmt.Exec(msgID, sessionID, i, rawPayload); err != nil {
			tb.Fatalf("insert perf message %d: %v", i, err)
		}
		if _, err := partStmt.Exec(NewID(), msgID, sessionID, partPayload); err != nil {
			tb.Fatalf("insert perf part %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit perf fixture: %v", err)
	}
	committed = true
}

// checkpointPerf collapses the WAL the bulk load wrote so a later cold open
// sees a steady-state file rather than paying a checkpoint a production
// reopen would rarely see. Shared so the test and both benchmarks measure
// the same file state.
func checkpointPerf(tb testing.TB, db *sql.DB) {
	tb.Helper()
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		tb.Fatalf("checkpoint perf fixture: %v", err)
	}
}

// blobTokenRe matches blob identifiers as whole tokens, so a future query
// naming raw_json, a data column, or the part table fails the shape probe
// while words merely containing those letters (particular, partition) do
// not. COUNT(*) is the one allowed star: it counts rows without reading any
// column.
var blobTokenRe = regexp.MustCompile(`(?i)\b(raw_json|data|part)\b`)

// assertCountQueryShape pins the probe text: exact normalized equality plus a
// token scan for blob identifiers. Either fires if a future edit drags blobs
// into the listing path — SELECT *, SELECT raw_json, JOIN part, or an
// aliased part reference.
func assertCountQueryShape(t *testing.T) {
	t.Helper()
	normalized := strings.Join(strings.Fields(strings.ToUpper(messageCountQuery)), " ")
	if normalized != "SELECT COUNT(*) FROM MESSAGE" {
		t.Errorf("count query drifted to %q, want exactly %q (blob-free listing shape)", messageCountQuery, "SELECT COUNT(*) FROM message")
	}
	if blobTokenRe.MatchString(strings.ReplaceAll(normalized, "COUNT(*)", "COUNT_ROWS")) {
		t.Errorf("count query must not name blob columns or the part table: %q", messageCountQuery)
	}
}

// TestColdOpen10kMessages builds the 10k-message fixture, closes the store,
// then times a fresh Open plus the blob-free count probe. Setup is outside
// the measured window; only the Open is budgeted.
//
// The fixture build opens the store first, which warms the WASM driver — by
// design the timed Open measures the file open (permits, pragmas, schema
// gate), not runtime init. A real CLI launch pays WASM init once per process
// on top of this; the todo budgets the per-open file cost, which is what
// every listing pays. The 50ms budget carries ~25x headroom over the ~2ms
// measured on dev hardware, so a single sample is the signal rather than a
// percentile tracker.
func TestColdOpen10kMessages(t *testing.T) {
	home := t.TempDir()
	env := testEnv(map[string]string{"HOME": home})

	db, err := Open(env)
	if err != nil {
		t.Fatalf("Open for fixture: %v", err)
	}
	sID := seedPerfSession(t, db)
	buildPerfFixture(t, db, sID)
	checkpointPerf(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("close perf fixture: %v", err)
	}

	budget := coldOpenBudget(testing.Short())
	start := time.Now()
	db, err = Open(env)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("cold Open: %v", err)
	}
	defer db.Close()
	t.Logf("cold Open on %d-message fixture: %s (budget %s, short=%v)", perfMessageCount, elapsed, budget, testing.Short())
	if elapsed > budget {
		t.Errorf("cold Open took %s, budget %s on %d-message fixture", elapsed, budget, perfMessageCount)
	}

	assertCountQueryShape(t)
	planRows, err := db.Query(`EXPLAIN QUERY PLAN ` + messageCountQuery)
	if err != nil {
		t.Fatalf("explain count query: %v", err)
	}
	defer planRows.Close()
	for planRows.Next() {
		var id, parent, notused int
		var detail string
		if err := planRows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		if blobTokenRe.MatchString(detail) {
			t.Errorf("count query plan touches blobs or part: %q", detail)
		}
	}
	if err := planRows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}

	var n int
	if err := db.QueryRow(messageCountQuery).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if n != perfMessageCount {
		t.Errorf("COUNT(*) = %d, want %d", n, perfMessageCount)
	}
	var nParts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM part`).Scan(&nParts); err != nil {
		t.Fatalf("count parts: %v", err)
	}
	if nParts != perfMessageCount {
		t.Errorf("part COUNT(*) = %d, want %d (one blob part per message)", nParts, perfMessageCount)
	}
}

// BenchmarkColdOpen10k replays the timed half of the test for -bench runs:
// the fixture builds once outside the timer, each iteration pays one cold
// Open plus close.
func BenchmarkColdOpen10k(b *testing.B) {
	home := b.TempDir()
	env := testEnv(map[string]string{"HOME": home})

	db, err := Open(env)
	if err != nil {
		b.Fatalf("Open for fixture: %v", err)
	}
	sID := seedPerfSession(b, db)
	buildPerfFixture(b, db, sID)
	checkpointPerf(b, db)
	if err := db.Close(); err != nil {
		b.Fatalf("close fixture: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db, err := Open(env)
		if err != nil {
			b.Fatalf("cold Open: %v", err)
		}
		if err := db.Close(); err != nil {
			b.Fatalf("close cold open: %v", err)
		}
	}
}

// BenchmarkMessageCount10k times the blob-free count probe on the 10k
// fixture for -bench runs.
func BenchmarkMessageCount10k(b *testing.B) {
	home := b.TempDir()
	env := testEnv(map[string]string{"HOME": home})

	db, err := Open(env)
	if err != nil {
		b.Fatalf("Open for fixture: %v", err)
	}
	defer db.Close()
	sID := seedPerfSession(b, db)
	buildPerfFixture(b, db, sID)
	checkpointPerf(b, db)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var n int
		if err := db.QueryRow(messageCountQuery).Scan(&n); err != nil {
			b.Fatalf("count: %v", err)
		}
		if n != perfMessageCount {
			b.Fatalf("COUNT(*) = %d, want %d", n, perfMessageCount)
		}
	}
}
