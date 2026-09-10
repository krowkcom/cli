package store

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// messageCountQuery is the listing-shape probe for the 10k fixture: a COUNT
// that must never touch blob columns. It is a constant so the gate pins the
// exact text — a future listing query that SELECTs raw_json or joins part
// cannot drift in behind a variable.
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

// buildPerfFixture creates a temp store through Open (so the file carries the
// production pragmas and version stamp), fills it with perfMessageCount
// messages plus one blob part each inside a single transaction, checkpoints
// the WAL so the later cold open sees a steady-state file, and closes the
// handle. The caller must close the database before timing Open: fixture
// setup is never part of the measured window.
func buildPerfFixture(t *testing.T, db *sql.DB, sessionID string) {
	t.Helper()

	rawPayload := `{"text":"` + strings.Repeat("x", 896) + `"}`
	partPayload := `{"text":"` + strings.Repeat("y", 896) + `"}`

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin perf fixture: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	msgStmt, err := tx.Prepare(`INSERT INTO message (id, session_id, turn_id, seq, role, raw_json, time_created) VALUES (?, ?, NULL, ?, 'user', ?, 1)`)
	if err != nil {
		t.Fatalf("prepare message: %v", err)
	}
	defer msgStmt.Close()
	partStmt, err := tx.Prepare(`INSERT INTO part (id, message_id, session_id, seq, type, data) VALUES (?, ?, ?, 0, 'text', ?)`)
	if err != nil {
		t.Fatalf("prepare part: %v", err)
	}
	defer partStmt.Close()

	for i := 0; i < perfMessageCount; i++ {
		msgID := NewID()
		if _, err := msgStmt.Exec(msgID, sessionID, i, rawPayload); err != nil {
			t.Fatalf("insert perf message %d: %v", i, err)
		}
		if _, err := partStmt.Exec(NewID(), msgID, sessionID, partPayload); err != nil {
			t.Fatalf("insert perf part %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit perf fixture: %v", err)
	}
	committed = true
}

// TestColdOpen10kMessages builds the 10k-message fixture, closes the store,
// then times a fresh Open plus the blob-free count probe. Setup is outside
// the measured window; only the Open is budgeted.
//
// The first Open in the process warms the WASM driver, so the timed Open
// measures the file open (permits, pragmas, schema gate) rather than runtime
// init — that is the "cold" the todo budgets, and it is what a CLI launch
// pays on every invocation.
func TestColdOpen10kMessages(t *testing.T) {
	home := t.TempDir()
	env := testEnv(map[string]string{"HOME": home})

	db, err := Open(env)
	if err != nil {
		t.Fatalf("Open for fixture: %v", err)
	}
	wID := NewID()
	if _, err := db.Exec(`INSERT INTO worktree (id, path, time_created, time_updated) VALUES (?, '/perf', 1, 2)`, wID); err != nil {
		db.Close()
		t.Fatalf("insert perf worktree: %v", err)
	}
	sID := NewID()
	if _, err := db.Exec(`INSERT INTO session (id, worktree_id, directory, title, time_created, time_updated) VALUES (?, ?, '/r', 'perf', 1, 2)`, sID, wID); err != nil {
		db.Close()
		t.Fatalf("insert perf session: %v", err)
	}
	buildPerfFixture(t, db, sID)
	// Steady-state file: collapse the WAL the bulk load wrote so the timed
	// open does not pay a checkpoint a production reopen would rarely see.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		db.Close()
		t.Fatalf("checkpoint perf fixture: %v", err)
	}
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

	// The count probe must not name blob columns: raw_json lives on message,
	// data lives on part, and either in the text means the query drags blobs.
	upper := strings.ToUpper(messageCountQuery)
	if strings.Contains(upper, "RAW_JSON") {
		t.Errorf("count query must not SELECT raw_json: %q", messageCountQuery)
	}
	if strings.Contains(upper, "PART") {
		t.Errorf("count query must not touch the part table: %q", messageCountQuery)
	}
	var plan string
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
		plan += detail + "; "
		if strings.Contains(strings.ToLower(detail), "part") {
			t.Errorf("count query plan touches part: %q", detail)
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
	wID := NewID()
	if _, err := db.Exec(`INSERT INTO worktree (id, path, time_created, time_updated) VALUES (?, '/perf', 1, 2)`, wID); err != nil {
		db.Close()
		b.Fatalf("insert perf worktree: %v", err)
	}
	sID := NewID()
	if _, err := db.Exec(`INSERT INTO session (id, worktree_id, directory, title, time_created, time_updated) VALUES (?, ?, '/r', 'perf', 1, 2)`, sID, wID); err != nil {
		db.Close()
		b.Fatalf("insert perf session: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		b.Fatalf("begin: %v", err)
	}
	payload := strings.Repeat("x", 896)
	for i := 0; i < perfMessageCount; i++ {
		msgID := NewID()
		if _, err := tx.Exec(`INSERT INTO message (id, session_id, turn_id, seq, role, raw_json, time_created) VALUES (?, ?, NULL, ?, 'user', ?, 1)`, msgID, sID, i, payload); err != nil {
			tx.Rollback()
			db.Close()
			b.Fatalf("insert message: %v", err)
		}
		if _, err := tx.Exec(`INSERT INTO part (id, message_id, session_id, seq, type, data) VALUES (?, ?, ?, 0, 'text', ?)`, NewID(), msgID, sID, payload); err != nil {
			tx.Rollback()
			db.Close()
			b.Fatalf("insert part: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		b.Fatalf("commit: %v", err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		db.Close()
		b.Fatalf("checkpoint: %v", err)
	}
	if err := db.Close(); err != nil {
		b.Fatalf("close fixture: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db, err := Open(env)
		if err != nil {
			b.Fatalf("cold Open: %v", err)
		}
		db.Close()
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
	wID := NewID()
	if _, err := db.Exec(`INSERT INTO worktree (id, path, time_created, time_updated) VALUES (?, '/perf', 1, 2)`, wID); err != nil {
		b.Fatalf("insert worktree: %v", err)
	}
	sID := NewID()
	if _, err := db.Exec(`INSERT INTO session (id, worktree_id, directory, title, time_created, time_updated) VALUES (?, ?, '/r', 'perf', 1, 2)`, sID, wID); err != nil {
		b.Fatalf("insert session: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		b.Fatalf("begin: %v", err)
	}
	payload := strings.Repeat("x", 896)
	for i := 0; i < perfMessageCount; i++ {
		msgID := NewID()
		if _, err := tx.Exec(`INSERT INTO message (id, session_id, turn_id, seq, role, raw_json, time_created) VALUES (?, ?, NULL, ?, 'user', ?, 1)`, msgID, sID, i, payload); err != nil {
			tx.Rollback()
			b.Fatalf("insert message: %v", err)
		}
		if _, err := tx.Exec(`INSERT INTO part (id, message_id, session_id, seq, type, data) VALUES (?, ?, ?, 0, 'text', ?)`, NewID(), msgID, sID, payload); err != nil {
			tx.Rollback()
			b.Fatalf("insert part: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatalf("commit: %v", err)
	}

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
