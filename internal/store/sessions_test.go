package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// perfSessionCount is the fixture size the todo names: 10k sessions with
// small message counts, so the listing probe runs against a file where
// touching blobs would actually cost.
const perfSessionCount = 10000

// sessionListBudget is the acceptance threshold: Open plus the listing on
// the 10k-session fixture must finish under 100ms. Under -short the budget
// loosens 10x — the same escape as the 50ms Open test — since loaded CI
// runners flake on tight wall-clock asserts.
func sessionListBudget(short bool) time.Duration {
	if short {
		return 1000 * time.Millisecond
	}
	return 100 * time.Millisecond
}

// buildSessionsFixture fills db with perfSessionCount sessions, each with
// one binding, one turn carrying token counts, and two messages with one
// small part each, inside a single transaction. Fixture setup is never part
// of the measured window: callers build, checkpoint, close, and only then
// start the clock on Open plus the listing.
func buildSessionsFixture(tb testing.TB, db *sql.DB) {
	tb.Helper()

	wID := NewID()
	if _, err := db.Exec(`INSERT INTO worktree (id, path, time_created, time_updated) VALUES (?, '/perf-wt', 1, 2)`, wID); err != nil {
		tb.Fatalf("insert perf worktree: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		tb.Fatalf("begin sessions fixture: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	sessStmt, err := tx.Prepare(`INSERT INTO session (id, worktree_id, directory, title, model, provider, harness, time_created, time_updated) VALUES (?, ?, '/r', ?, 'claude-opus-4-6', 'anthropic', 'claude', ?, ?)`)
	if err != nil {
		tb.Fatalf("prepare session: %v", err)
	}
	defer sessStmt.Close()
	bindStmt, err := tx.Prepare(`INSERT INTO session_binding (id, session_id, provider, harness, foreign_session_id, time_created, time_updated) VALUES (?, ?, 'anthropic', 'claude', ?, 1, 2)`)
	if err != nil {
		tb.Fatalf("prepare binding: %v", err)
	}
	defer bindStmt.Close()
	turnStmt, err := tx.Prepare(`INSERT INTO turn (id, session_id, seq, status, cost_input_tokens, cost_output_tokens, cost_total_tokens, cost_cache_read_tokens, cost_cache_write_tokens, cost_reasoning_tokens, time_created, time_updated) VALUES (?, ?, 0, 'closed', 100, 50, 150, 10, 5, 0, 1, 2)`)
	if err != nil {
		tb.Fatalf("prepare turn: %v", err)
	}
	defer turnStmt.Close()
	msgStmt, err := tx.Prepare(`INSERT INTO message (id, session_id, turn_id, seq, role, raw_json, time_created) VALUES (?, ?, NULL, ?, ?, ?, 1)`)
	if err != nil {
		tb.Fatalf("prepare message: %v", err)
	}
	defer msgStmt.Close()
	partStmt, err := tx.Prepare(`INSERT INTO part (id, message_id, session_id, seq, type, data) VALUES (?, ?, ?, 0, 'text', ?)`)
	if err != nil {
		tb.Fatalf("prepare part: %v", err)
	}
	defer partStmt.Close()

	payload := `{"text":"` + strings.Repeat("p", 256) + `"}`
	for i := 0; i < perfSessionCount; i++ {
		sID := NewID()
		ts := int64(1000 + i)
		if _, err := sessStmt.Exec(sID, wID, "session title", ts, ts); err != nil {
			tb.Fatalf("insert perf session %d: %v", i, err)
		}
		if _, err := bindStmt.Exec(NewID(), sID, "foreign-ses-"+strings.Repeat("x", 8)+"-"+padInt(i, 6)); err != nil {
			tb.Fatalf("insert perf binding %d: %v", i, err)
		}
		if _, err := turnStmt.Exec(NewID(), sID); err != nil {
			tb.Fatalf("insert perf turn %d: %v", i, err)
		}
		for m := 0; m < 2; m++ {
			role := "user"
			if m == 1 {
				role = "assistant"
			}
			msgID := NewID()
			if _, err := msgStmt.Exec(msgID, sID, m, role, `{"line":`+itoa(i)+`}`); err != nil {
				tb.Fatalf("insert perf message %d/%d: %v", i, m, err)
			}
			if _, err := partStmt.Exec(NewID(), msgID, sID, payload); err != nil {
				tb.Fatalf("insert perf part %d/%d: %v", i, m, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit sessions fixture: %v", err)
	}
	committed = true
}

func padInt(i, width int) string {
	s := itoa(i)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// assertSessionListQueryShape pins the listing text: exact normalized
// equality plus a token scan for blob identifiers. Either fires if a future
// edit drags message.raw_json, message.usage, part.data or the part table
// into the listing path.
func assertSessionListQueryShape(tb testing.TB) {
	tb.Helper()
	normalized := strings.Join(strings.Fields(strings.ToUpper(sessionListQuery)), " ")
	want := `WITH PAGE AS (SELECT S.ID AS PID FROM SESSION S LEFT JOIN WORKTREE W ON W.ID = S.WORKTREE_ID LEFT JOIN (SELECT SESSION_ID, MIN(ID) AS ID FROM SESSION_BINDING GROUP BY SESSION_ID) ONE ON ONE.SESSION_ID = S.ID LEFT JOIN SESSION_BINDING B ON B.ID = ONE.ID WHERE (? = '' OR COALESCE(B.HARNESS, S.HARNESS) = ?) AND (? = '' OR W.PATH = ?) ORDER BY S.TIME_UPDATED DESC LIMIT ?) SELECT S.ID, S.TITLE, S.MODEL, S.PROVIDER, S.HARNESS, S.DIRECTORY, S.TIME_CREATED, S.TIME_UPDATED, W.PATH, B.HARNESS, B.PROVIDER, B.FOREIGN_SESSION_ID, COALESCE(T.N, 0), COALESCE(T.SUM_IN, 0), COALESCE(T.SUM_OUT, 0), COALESCE(T.SUM_TOTAL, 0), COALESCE(T.SUM_CREAD, 0), COALESCE(T.SUM_CWRITE, 0), COALESCE(T.SUM_REASON, 0) FROM SESSION S JOIN PAGE ON PAGE.PID = S.ID LEFT JOIN WORKTREE W ON W.ID = S.WORKTREE_ID LEFT JOIN (SELECT SESSION_ID, MIN(ID) AS ID FROM SESSION_BINDING GROUP BY SESSION_ID) ONE ON ONE.SESSION_ID = S.ID LEFT JOIN SESSION_BINDING B ON B.ID = ONE.ID LEFT JOIN (SELECT SESSION_ID, COUNT(*) AS N, SUM(COST_INPUT_TOKENS) AS SUM_IN, SUM(COST_OUTPUT_TOKENS) AS SUM_OUT, SUM(COST_TOTAL_TOKENS) AS SUM_TOTAL, SUM(COST_CACHE_READ_TOKENS) AS SUM_CREAD, SUM(COST_CACHE_WRITE_TOKENS) AS SUM_CWRITE, SUM(COST_REASONING_TOKENS) AS SUM_REASON FROM TURN WHERE SESSION_ID IN (SELECT PID FROM PAGE) GROUP BY SESSION_ID) T ON T.SESSION_ID = S.ID ORDER BY S.TIME_UPDATED DESC`
	if normalized != want {
		tb.Errorf("session list query drifted:\n got: %s\nwant: %s", sessionListQuery, want)
	}
	scrubbed := strings.ReplaceAll(normalized, "COUNT(T.ID)", "COUNT_ROWS")
	scrubbed = strings.ReplaceAll(scrubbed, "COUNT(*)", "COUNT_ROWS")
	if blobTokenRe.MatchString(scrubbed) {
		tb.Errorf("session list query must not name blob columns or the part table: %q", sessionListQuery)
	}
}

// TestSessionList10kSessions builds the 10k-session fixture, closes the
// store, then times a fresh Open plus the default listing page. Setup is
// outside the measured window; only Open plus ListSessions is budgeted.
//
// The listing runs at the default page (50): that is what `krowk sessions`
// runs, and the gate proves that page stays instant on a 10k-session
// store. The full 10k-row walk is checked untimed below, so the fixture
// size is still proved row by row rather than trusted.
func TestSessionList10kSessions(t *testing.T) {
	home := t.TempDir()
	env := testEnv(map[string]string{"HOME": home})

	db, err := Open(env)
	if err != nil {
		t.Fatalf("Open for fixture: %v", err)
	}
	buildSessionsFixture(t, db)
	checkpointPerf(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("close sessions fixture: %v", err)
	}

	budget := sessionListBudget(testing.Short())
	start := time.Now()
	db, err = Open(env)
	if err != nil {
		t.Fatalf("cold Open: %v", err)
	}
	rows, err := ListSessions(db, "", "", 50)
	elapsed := time.Since(start)
	if err != nil {
		db.Close()
		t.Fatalf("list sessions: %v", err)
	}
	t.Logf("Open+list(default page) on %d-session fixture: %s (%d rows, budget %s, short=%v)",
		perfSessionCount, elapsed, len(rows), budget, testing.Short())
	if elapsed > budget {
		t.Errorf("Open+list took %s, budget %s on %d-session fixture", elapsed, budget, perfSessionCount)
	}
	if len(rows) != 50 {
		t.Errorf("default page listed %d sessions, want 50", len(rows))
	}

	assertSessionListQueryShape(t)
	planRows, err := db.Query(`EXPLAIN QUERY PLAN `+sessionListQuery, "", "", "", "", -1)
	if err != nil {
		db.Close()
		t.Fatalf("explain list query: %v", err)
	}
	for planRows.Next() {
		var id, parent, notused int
		var detail string
		if err := planRows.Scan(&id, &parent, &notused, &detail); err != nil {
			planRows.Close()
			db.Close()
			t.Fatalf("scan plan: %v", err)
		}
		if blobTokenRe.MatchString(strings.ReplaceAll(strings.ToUpper(detail), "COUNT(*)", "COUNT_ROWS")) {
			t.Errorf("list query plan touches blobs or part: %q", detail)
		}
	}
	planRows.Close()
	if err := planRows.Err(); err != nil {
		db.Close()
		t.Fatalf("plan rows: %v", err)
	}

	// Untimed: the whole 10k rows walk, proving the fixture size row by row.
	all, err := ListSessions(db, "", "", -1)
	if err != nil {
		db.Close()
		t.Fatalf("list all sessions: %v", err)
	}
	if len(all) != perfSessionCount {
		t.Errorf("listed %d sessions, want %d", len(all), perfSessionCount)
	}
	if err := db.Close(); err != nil {
		t.Errorf("close cold open: %v", err)
	}
}

// BenchmarkSessionList10k times the default listing page on the
// 10k-session fixture for -bench runs: the shipped path pages 50, so the
// benchmark measures the page, not a 10k-row --all walk. The fixture
// builds once; each iteration pays Open plus the listing and close.
func BenchmarkSessionList10k(b *testing.B) {
	home := b.TempDir()
	env := testEnv(map[string]string{"HOME": home})

	db, err := Open(env)
	if err != nil {
		b.Fatalf("Open for fixture: %v", err)
	}
	buildSessionsFixture(b, db)
	checkpointPerf(b, db)
	if err := db.Close(); err != nil {
		b.Fatalf("close fixture: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db, err := Open(env)
		if err != nil {
			b.Fatalf("Open: %v", err)
		}
		rows, err := ListSessions(db, "", "", 50)
		if err != nil {
			db.Close()
			b.Fatalf("list: %v", err)
		}
		if len(rows) != 50 {
			db.Close()
			b.Fatalf("listed %d, want %d", len(rows), 50)
		}
		if err := db.Close(); err != nil {
			b.Fatalf("close: %v", err)
		}
	}
}

// TestResolveSessionID pins prefix and binding resolution.
func TestResolveSessionID(t *testing.T) {
	home := t.TempDir()
	env := testEnv(map[string]string{"HOME": home})
	db, err := Open(env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	w := NewWriter(db, nil)
	ctx := context.Background()
	mk := func(foreign, title string) Thread {
		return Thread{
			Worktree: Worktree{Path: "/r-" + foreign},
			Session:  Session{Title: title},
			Binding:  Binding{Provider: "anthropic", Harness: "claude", ForeignSessionID: foreign},
			Turns:    []Turn{{Status: "closed"}},
		}
	}
	if _, err := w.Ingest(ctx, mk("foreign-aaa", "Alpha")); err != nil {
		t.Fatalf("ingest aaa: %v", err)
	}
	if _, err := w.Ingest(ctx, mk("foreign-bbb", "Beta")); err != nil {
		t.Fatalf("ingest bbb: %v", err)
	}
	var idA, idB string
	if err := db.QueryRow(`SELECT session_id FROM session_binding WHERE foreign_session_id = 'foreign-aaa'`).Scan(&idA); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT session_id FROM session_binding WHERE foreign_session_id = 'foreign-bbb'`).Scan(&idB); err != nil {
		t.Fatal(err)
	}

	// Full id works.
	if got, err := ResolveSessionID(db, idA); err != nil || got != idA {
		t.Errorf("full id = %q, %v; want %q", got, err, idA)
	}
	// A unique prefix of at least 8 chars works. uuidv7 ids are
	// time-ordered, so two ids minted together share a long head — take
	// the shortest prefix of A that B does not share.
	prefix := idA
	for n := 8; n < len(idA); n++ {
		if !strings.HasPrefix(idB, idA[:n]) {
			prefix = idA[:n]
			break
		}
	}
	if len(prefix) < 8 {
		t.Fatalf("no unique prefix of %q against %q", idA, idB)
	}
	if got, err := ResolveSessionID(db, prefix); err != nil || got != idA {
		t.Errorf("unique prefix %q = %q, %v; want %q", prefix, got, err, idA)
	}
	// Foreign session id resolves via the binding.
	if got, err := ResolveSessionID(db, "foreign-bbb"); err != nil || got != idB {
		t.Errorf("foreign id = %q, %v; want %q", got, err, idB)
	}
	// Short prefix is refused.
	if _, err := ResolveSessionID(db, idA[:7]); err == nil {
		t.Errorf("7-char prefix resolved, want an error")
	}
	// Unknown resolves to nothing.
	if _, err := ResolveSessionID(db, "foreign-nope"); err == nil {
		t.Errorf("unknown foreign id resolved, want an error")
	}
}

// TestResolveAmbiguousPrefix forces two sessions under one prefix and
// requires the error to name both.
func TestResolveAmbiguousPrefix(t *testing.T) {
	home := t.TempDir()
	env := testEnv(map[string]string{"HOME": home})
	db, err := Open(env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Two ids sharing an 8-char prefix: insert directly, since NewID is
	// random and will not collide on its own.
	wID := NewID()
	if _, err := db.Exec(`INSERT INTO worktree (id, path, time_created, time_updated) VALUES (?, '/amb', 1, 2)`, wID); err != nil {
		t.Fatal(err)
	}
	prefix := "0196ambig"
	idA, idB := prefix+"aaaaaaaaaaaaaaaa", prefix+"bbbbbbbbbbbbbbbb"
	for _, id := range []string{idA, idB} {
		if _, err := db.Exec(`INSERT INTO session (id, worktree_id, directory, title, time_created, time_updated) VALUES (?, ?, '/r', ?, 1, 2)`, id, wID, "t-"+id); err != nil {
			t.Fatal(err)
		}
	}
	_, err = ResolveSessionID(db, prefix)
	if err == nil {
		t.Fatalf("ambiguous prefix resolved, want an error naming both")
	}
	msg := err.Error()
	if !strings.Contains(msg, idA) || !strings.Contains(msg, idB) {
		t.Errorf("ambiguous error names neither candidate: %q", msg)
	}
	var amb *AmbiguousSessionError
	if !errors.As(err, &amb) {
		t.Errorf("ambiguous error is %T, want *AmbiguousSessionError", err)
	}
}

// TestResolvePrefixWildcards pins LIKE escaping: % and _ in a typed prefix
// are literals, so they cannot overmatch every session.
func TestResolvePrefixWildcards(t *testing.T) {
	home := t.TempDir()
	env := testEnv(map[string]string{"HOME": home})
	db, err := Open(env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	wID := NewID()
	if _, err := db.Exec(`INSERT INTO worktree (id, path, time_created, time_updated) VALUES (?, '/wild', 1, 2)`, wID); err != nil {
		t.Fatal(err)
	}
	sID := NewID()
	if _, err := db.Exec(`INSERT INTO session (id, worktree_id, directory, title, time_created, time_updated) VALUES (?, ?, '/r', 'wild', 1, 2)`, sID, wID); err != nil {
		t.Fatal(err)
	}
	// 8-char prefixes carrying wildcards: % must not list the store and
	// _ must not stand in for the real id char.
	for _, ref := range []string{sID[:7] + "%", sID[:7] + "_", "________", "%%%%%%%%"} {
		if got, err := ResolveSessionID(db, ref); err == nil {
			t.Errorf("wildcard prefix %q resolved to %q, want no match", ref, got)
		} else if strings.Contains(err.Error(), "ambiguous") {
			t.Errorf("wildcard prefix %q went ambiguous, want no match: %q", ref, err)
		}
	}
}

// TestSessionListMultiBindingNoFanout pins the listing aggregate: a second
// binding row must neither multiply the turn COUNT/SUM nor duplicate the
// session, and the displayed binding is the earliest-minted one.
func TestSessionListMultiBindingNoFanout(t *testing.T) {
	home := t.TempDir()
	env := testEnv(map[string]string{"HOME": home})
	db, err := Open(env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	wID := NewID()
	if _, err := db.Exec(`INSERT INTO worktree (id, path, time_created, time_updated) VALUES (?, '/fan', 1, 2)`, wID); err != nil {
		t.Fatal(err)
	}
	sID := NewID()
	if _, err := db.Exec(`INSERT INTO session (id, worktree_id, directory, title, model, provider, harness, time_created, time_updated) VALUES (?, ?, '/r', 'fan', 'm', 'anthropic', 'claude', 1, 2)`, sID, wID); err != nil {
		t.Fatal(err)
	}
	// Two bindings: the earliest-minted (first) is the displayed one.
	b1, b2 := NewID(), NewID()
	if _, err := db.Exec(`INSERT INTO session_binding (id, session_id, provider, harness, foreign_session_id, time_created, time_updated) VALUES (?, ?, 'anthropic', 'first', 'foreign-one', 1, 2)`, b1, sID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session_binding (id, session_id, provider, harness, foreign_session_id, time_created, time_updated) VALUES (?, ?, 'anthropic', 'second', 'foreign-two', 1, 2)`, b2, sID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(`INSERT INTO turn (id, session_id, seq, status, cost_input_tokens, cost_output_tokens, cost_total_tokens, cost_cache_read_tokens, cost_cache_write_tokens, cost_reasoning_tokens, time_created, time_updated) VALUES (?, ?, ?, 'closed', 100, 50, 150, 10, 5, 0, 1, 2)`, NewID(), sID, i); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := ListSessions(db, "", "", -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("listed %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.TurnCount != 2 {
		t.Errorf("TurnCount = %d, want 2 (binding join multiplied it)", r.TurnCount)
	}
	if r.SumInput != 200 || r.SumOutput != 100 || r.SumTotal != 300 || r.SumCacheRead != 20 || r.SumCacheWrite != 10 {
		t.Errorf("sums = %d/%d/%d/%d/%d, want 200/100/300/20/10", r.SumInput, r.SumOutput, r.SumTotal, r.SumCacheRead, r.SumCacheWrite)
	}
	if r.BindingHarness != "first" || r.ForeignSessionID != "foreign-one" {
		t.Errorf("binding = %q/%q, want first/foreign-one", r.BindingHarness, r.ForeignSessionID)
	}
}

// TestToolCallNameNested pins the JSON decode: a nested input.name is not
// the call name, and escapes decode.
func TestToolCallNameNested(t *testing.T) {
	if got := ToolNameOf(`{"name":"outer","input":{"name":"inner"}}`); got != "outer" {
		t.Errorf("nested name = %q, want outer", got)
	}
	if got := ToolNameOf(`{"name":"a\"b"}`); got != `a"b` {
		t.Errorf("escaped name = %q, want a\"b", got)
	}
	if got := ToolNameOf(`{"input":{"name":"inner"}}`); got != "" {
		t.Errorf("input-only name = %q, want empty", got)
	}
	if got := ToolNameOf(`not json`); got != "" {
		t.Errorf("garbage name = %q, want empty", got)
	}
}

// TestToolResultForwardRef pins two-pass linking: a result in an earlier
// message still finds the call that comes later.
func TestToolResultForwardRef(t *testing.T) {
	home := t.TempDir()
	env := testEnv(map[string]string{"HOME": home})
	db, err := Open(env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	w := NewWriter(db, nil)
	ctx := context.Background()
	th := Thread{
		Worktree: Worktree{Path: "/r-fwd"},
		Session:  Session{Title: "fwd"},
		Binding:  Binding{Provider: "anthropic", Harness: "claude", ForeignSessionID: "foreign-fwd"},
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{
				{Type: "tool_result", ToolCallID: "call_9", Data: `{"output":"late","is_error":false}`},
			}},
			{Role: RoleAssistant, Parts: []Part{
				{Type: "tool_call", ToolCallID: "call_9", Data: `{"name":"Read","input":"x"}`},
			}},
		},
	}
	if _, err := w.Ingest(ctx, th); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var id string
	if err := db.QueryRow(`SELECT session_id FROM session_binding WHERE foreign_session_id = 'foreign-fwd'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	d, err := LoadSessionDetail(db, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	found := false
	for _, m := range d.Messages {
		for _, p := range m.Parts {
			if p.Type == "tool_result" && p.ToolCallID == "call_9" {
				found = true
				if p.ToolName != "Read" || !p.Linked {
					t.Errorf("forward result = %+v, want Read/linked", p)
				}
			}
		}
	}
	if !found {
		t.Errorf("forward tool_result missing from detail")
	}
}

// TestTitleFallbackAtImport pins the first-80-chars rule: an untitled
// thread lists by its first user text, and a re-import never wipes a title.
func TestTitleFallbackAtImport(t *testing.T) {
	home := t.TempDir()
	env := testEnv(map[string]string{"HOME": home})
	db, err := Open(env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	w := NewWriter(db, nil)
	ctx := context.Background()
	long := strings.Repeat("w", 100)
	th := Thread{
		Worktree: Worktree{Path: "/r-fb"},
		Session:  Session{},
		Binding:  Binding{Provider: "anthropic", Harness: "claude", ForeignSessionID: "foreign-fb"},
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{{Type: "text", Data: `{"text":"` + long + `"}`}}},
		},
	}
	if _, err := w.Ingest(ctx, th); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var title string
	if err := db.QueryRow(`SELECT title FROM session WHERE id = (SELECT session_id FROM session_binding WHERE foreign_session_id = 'foreign-fb')`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != strings.Repeat("w", 80) {
		t.Errorf("title = %q, want first 80 chars", title)
	}
	rows, err := ListSessions(db, "", "", -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Title != strings.Repeat("w", 80) {
		t.Errorf("listed title = %+v, want the fallback", rows)
	}
	// Re-import untitled: the stored title survives.
	if _, err := w.Ingest(ctx, th); err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	var title2 string
	if err := db.QueryRow(`SELECT title FROM session WHERE id = (SELECT session_id FROM session_binding WHERE foreign_session_id = 'foreign-fb')`).Scan(&title2); err != nil {
		t.Fatal(err)
	}
	if title2 != title {
		t.Errorf("re-import title = %q, want stored %q", title2, title)
	}
}

// TestToolResultRelinking pins show's twin rule: every tool_result carries
// its tool_call name, and an orphan prints "unknown tool" with linked:false.
func TestToolResultRelinking(t *testing.T) {
	home := t.TempDir()
	env := testEnv(map[string]string{"HOME": home})
	db, err := Open(env)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	w := NewWriter(db, nil)
	ctx := context.Background()
	th := Thread{
		Worktree: Worktree{Path: "/r-tools"},
		Session:  Session{Title: "tools"},
		Binding:  Binding{Provider: "anthropic", Harness: "claude", ForeignSessionID: "foreign-tools"},
		Messages: []Message{
			{Role: RoleAssistant, Parts: []Part{
				{Type: "tool_call", ToolCallID: "call_1", Data: `{"name":"Bash","input":"ls"}`},
			}},
			{Role: RoleUser, Parts: []Part{
				{Type: "tool_result", ToolCallID: "call_1", Data: `{"output":"ok","is_error":false}`},
				{Type: "tool_result", ToolCallID: "call_missing", Data: `{"output":"x","is_error":false}`},
			}},
		},
	}
	if _, err := w.Ingest(ctx, th); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var id string
	if err := db.QueryRow(`SELECT session_id FROM session_binding WHERE foreign_session_id = 'foreign-tools'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	d, err := LoadSessionDetail(db, id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := map[string]PartDetail{}
	for _, m := range d.Messages {
		for _, p := range m.Parts {
			if p.Type == "tool_result" {
				got[p.ToolCallID] = p
			}
		}
	}
	if p, ok := got["call_1"]; !ok || p.ToolName != "Bash" || !p.Linked {
		t.Errorf("linked result = %+v, want Bash/linked", p)
	}
	if p, ok := got["call_missing"]; !ok || p.ToolName != "unknown tool" || p.Linked {
		t.Errorf("orphan result = %+v, want unknown tool/unlinked", p)
	}
}
