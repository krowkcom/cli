package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// frozenTime is the only clock the writer tests read: every id timestamp
// and every time_* column must equal it, proving the injected clock owns
// both.
var frozenTime = time.UnixMilli(1757414400000).UTC()

func frozenClock() time.Time { return frozenTime }

func openWriterDB(t *testing.T, clock Clock) (*sql.DB, *Writer) {
	t.Helper()
	home := t.TempDir()
	db, err := Open(testEnv(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if clock == nil {
		clock = frozenClock
	}
	return db, NewWriter(db, clock)
}

func sampleThread(provider, foreignID string) Thread {
	return Thread{
		Worktree: Worktree{Path: "/repo/" + foreignID, VCS: "git", Name: foreignID},
		Session:  Session{Directory: "/repo", Title: "t-" + foreignID, Model: "m", Provider: provider, Harness: "claude"},
		Binding:  Binding{Provider: provider, Harness: "claude", ForeignSessionID: foreignID, ResumeCmd: "resume " + foreignID},
		Turns: []Turn{
			{Status: "done", CostInput: 1, CostOutput: 2, CostTotal: 3},
			{Status: "done", CostInput: 4, CostOutput: 5, CostTotal: 9},
		},
		Events: []Event{
			{Type: "start", Data: `{"a":1}`},
			{Type: "stop"},
		},
		Messages: []Message{
			{Role: RoleUser, Provider: provider, Model: "m", ForeignID: foreignID + "-m1", Parts: []Part{{Type: "text", Data: `{"text":"hi"}`}}},
			{Role: RoleAssistant, Provider: provider, Model: "m", ForeignID: foreignID + "-m2", Parts: []Part{
				{Type: "text", Data: `{"text":"ho"}`},
				{Type: "tool_call", ToolCallID: "call_1", Data: `{"name":"sh","input":{}}`},
			}},
			{Role: RoleTool, Provider: provider, Model: "m", ForeignID: foreignID + "-m3", Parts: []Part{
				{Type: "tool_result", ToolCallID: "call_1", Data: `{"output":"ok","is_error":false}`},
			}},
		},
	}
}

func tableCounts(t *testing.T, db *sql.DB) map[string]int {
	t.Helper()
	tables := []string{"worktree", "session", "session_binding", "session_event", "turn", "message", "part"}
	out := map[string]int{}
	for _, tbl := range tables {
		var n int
		// Table names are this test's own list, never caller input.
		if err := db.QueryRow(`SELECT COUNT(*) FROM "` + tbl + `"`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		out[tbl] = n
	}
	return out
}

func equalCounts(a, b map[string]int) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func zeroInserted(r Result) bool {
	for _, c := range []Count{r.Worktrees, r.Sessions, r.Bindings, r.Turns, r.Messages, r.Parts, r.Events} {
		if c.Inserted != 0 {
			return false
		}
	}
	return true
}

func TestWriterIngestIdempotent(t *testing.T) {
	db, w := openWriterDB(t, nil)
	ctx := context.Background()
	th := sampleThread("claude", "ses_a")

	first, err := w.Ingest(ctx, th)
	if err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	if zeroInserted(first) {
		t.Fatalf("first Ingest inserted nothing: %+v", first)
	}
	before := tableCounts(t, db)

	second, err := w.Ingest(ctx, th)
	if err != nil {
		t.Fatalf("second Ingest: %v", err)
	}
	if !zeroInserted(second) {
		t.Errorf("second Ingest inserted rows: %+v", second)
	}
	if after := tableCounts(t, db); !equalCounts(before, after) {
		t.Errorf("row counts changed on re-ingest: before %v after %v", before, after)
	}
}

func TestWriterIngestConvergesBinding(t *testing.T) {
	db, w := openWriterDB(t, nil)
	ctx := context.Background()

	th1 := sampleThread("opencode", "ses_x")
	if _, err := w.Ingest(ctx, th1); err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	// Same key, different everything else: must adopt, not duplicate.
	th2 := sampleThread("opencode", "ses_x")
	th2.Session.Title = "renamed"
	th2.Worktree.Path = "/other/checkout"
	th2.Messages = []Message{
		{Role: RoleUser, ForeignID: "ses_x-m9", Parts: []Part{{Type: "text"}}},
	}
	if _, err := w.Ingest(ctx, th2); err != nil {
		t.Fatalf("second Ingest: %v", err)
	}

	var sessions, bindings int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_binding`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || bindings != 1 {
		t.Errorf("sessions=%d bindings=%d, want 1 and 1", sessions, bindings)
	}
	var title string
	if err := db.QueryRow(`SELECT title FROM session`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "renamed" {
		t.Errorf("title = %q, want %q (second Thread wins the display fields)", title, "renamed")
	}
}

func TestWriterSeqDenseAndContinues(t *testing.T) {
	db, w := openWriterDB(t, nil)
	ctx := context.Background()

	th1 := sampleThread("claude", "ses_seq")
	if _, err := w.Ingest(ctx, th1); err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	// Cumulative resend plus one new entry everywhere: the tail must
	// continue the sequence, not restart it.
	th2 := sampleThread("claude", "ses_seq")
	th2.Turns = append(th2.Turns, Turn{Status: "done"})
	th2.Events = append(th2.Events, Event{Type: "extra"})
	th2.Messages = append(th2.Messages, Message{
		Role: RoleUser, ForeignID: "ses_seq-m4",
		Parts: []Part{{Type: "text"}, {Type: "image"}},
	})
	if _, err := w.Ingest(ctx, th2); err != nil {
		t.Fatalf("second Ingest: %v", err)
	}

	assertDense := func(name, q string, want []int) {
		t.Helper()
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		defer rows.Close()
		var got []int
		for rows.Next() {
			var s int
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			got = append(got, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s = %v, want %v", name, got, want)
			}
		}
	}

	var sessionID string
	if err := db.QueryRow(`SELECT session_id FROM session_binding WHERE foreign_session_id = 'ses_seq'`).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	assertDense("message.seq", `SELECT seq FROM message WHERE session_id = '`+sessionID+`' ORDER BY seq`, []int{0, 1, 2, 3})
	assertDense("turn.seq", `SELECT seq FROM turn WHERE session_id = '`+sessionID+`' ORDER BY seq`, []int{0, 1, 2})
	assertDense("session_event.seq", `SELECT seq FROM session_event WHERE session_id = '`+sessionID+`' ORDER BY seq`, []int{0, 1, 2})

	rows, err := db.Query(`SELECT id FROM message WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var msgIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		msgIDs = append(msgIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	wantParts := [][]int{{0}, {0, 1}, {0}, {0, 1}}
	if len(msgIDs) != len(wantParts) {
		t.Fatalf("messages = %d, want %d", len(msgIDs), len(wantParts))
	}
	for i, id := range msgIDs {
		var got []int
		prows, err := db.Query(`SELECT seq FROM part WHERE message_id = ? ORDER BY seq`, id)
		if err != nil {
			t.Fatal(err)
		}
		for prows.Next() {
			var s int
			if err := prows.Scan(&s); err != nil {
				prows.Close()
				t.Fatal(err)
			}
			got = append(got, s)
		}
		prows.Close()
		if err := prows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(wantParts[i]) {
			t.Fatalf("part.seq for message %d = %v, want %v", i, got, wantParts[i])
		}
		for j := range wantParts[i] {
			if got[j] != wantParts[i][j] {
				t.Fatalf("part.seq for message %d = %v, want %v", i, got, wantParts[i])
			}
		}
	}
}

func TestWriterPartSessionMatchesMessage(t *testing.T) {
	db, w := openWriterDB(t, nil)
	if _, err := w.Ingest(context.Background(), sampleThread("cursor", "ses_p")); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	var bad int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM part p JOIN message m ON m.id = p.message_id
		WHERE p.session_id != m.session_id`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Errorf("%d part rows disagree with their message's session_id", bad)
	}
}

func TestWriterBadRoleRollsBack(t *testing.T) {
	db, w := openWriterDB(t, nil)
	ctx := context.Background()
	if _, err := w.Ingest(ctx, sampleThread("claude", "ses_ok")); err != nil {
		t.Fatalf("seed Ingest: %v", err)
	}
	before := tableCounts(t, db)

	th := sampleThread("claude", "ses_bad")
	th.Messages[1].Role = "admin"
	if _, err := w.Ingest(ctx, th); err == nil {
		t.Fatalf("Ingest with role admin succeeded, want failure")
	} else if !strings.Contains(err.Error(), `"admin"`) {
		t.Fatalf("error %q does not name the role", err)
	}
	if after := tableCounts(t, db); !equalCounts(before, after) {
		t.Errorf("row counts changed on failed ingest: before %v after %v", before, after)
	}
}

func TestWriterIDsAndClock(t *testing.T) {
	db, w := openWriterDB(t, nil)
	usd := int64(42)
	raw := `{"line":1}`
	th := sampleThread("opencode", "ses_id")
	th.Turns[0].CostUSDMicros = &usd
	th.Messages[0].RawJSON = &raw
	if _, err := w.Ingest(context.Background(), th); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	idTables := map[string]string{
		"worktree": "id", "session": "id", "session_binding": "id",
		"session_event": "id", "turn": "id", "message": "id", "part": "id",
	}
	nIDs := 0
	for tbl, col := range idTables {
		rows, err := db.Query(`SELECT "` + col + `" FROM "` + tbl + `"`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if err := ValidateID(id); err != nil {
				rows.Close()
				t.Errorf("%s id %q: %v", tbl, id, err)
			}
			nIDs++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	if nIDs == 0 {
		t.Fatalf("no ids checked")
	}

	want := frozenTime.UnixMilli()
	timeTables := []string{"worktree", "session", "session_binding", "turn", "message", "session_event"}
	for _, tbl := range timeTables {
		var cols []string
		switch tbl {
		case "session_event", "message":
			cols = []string{"time_created"}
		default:
			cols = []string{"time_created", "time_updated"}
		}
		for _, col := range cols {
			rows, err := db.Query(`SELECT "` + col + `" FROM "` + tbl + `"`)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for rows.Next() {
				var ms int64
				if err := rows.Scan(&ms); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				if ms != want {
					rows.Close()
					t.Errorf("%s.%s = %d, want frozen clock %d", tbl, col, ms, want)
				}
				n++
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Errorf("no %s rows checked", tbl)
			}
		}
	}
}

func TestWriterUsesNoEnv(t *testing.T) {
	for _, f := range []string{"writer.go", "thread.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"os.Getenv", "os.LookupEnv", "os.UserHomeDir", "os.UserCacheDir", "os.UserConfigDir", "\"os\""} {
			if strings.Contains(string(src), banned) {
				t.Errorf("%s references %s: the Writer must not read the environment", f, banned)
			}
		}
	}
}

// TestWriterConcurrentProcesses ingests two different sessions from two
// processes at once. Goroutines would share one pool and prove nothing
// about the file; exec proves busy_timeout plus short transactions holds
// across processes. The child branch runs first in the re-execed binary.
func TestWriterConcurrentProcesses(t *testing.T) {
	if child := os.Getenv("KROWK_WRITER_CHILD"); child != "" {
		home := os.Getenv("KROWK_WRITER_HOME")
		if home == "" {
			fmt.Fprintln(os.Stderr, "KROWK_WRITER_HOME unset")
			os.Exit(2)
		}
		db, err := Open(testEnv(map[string]string{"HOME": home}))
		if err != nil {
			fmt.Fprintln(os.Stderr, "open:", err)
			os.Exit(2)
		}
		w := NewWriter(db, nil)
		var th Thread
		switch child {
		case "same1", "same2":
			// Same binding key, overlapping messages: exercises the
			// whole-Ingest retry on UNIQUE conflicts.
			th = sampleThread("claude", "ses-child-same")
			th.Worktree.Path = filepath.Join(home, "wt-same")
			th.Turns = []Turn{{Status: "done"}}
			th.Events = []Event{{Type: "tick"}}
			shared := Message{Role: RoleUser, ForeignID: "shared-m2", Parts: []Part{{Type: "text"}}}
			if child == "same1" {
				th.Messages = []Message{
					{Role: RoleUser, ForeignID: "shared-m1", Parts: []Part{{Type: "text"}}},
					shared,
				}
			} else {
				th.Messages = []Message{
					shared,
					{Role: RoleUser, ForeignID: "shared-m3", Parts: []Part{{Type: "text"}}},
				}
			}
		default:
			th = sampleThread("claude", "ses-child-"+child)
			th.Worktree.Path = filepath.Join(home, "wt-"+child)
		}
		if _, err := w.Ingest(context.Background(), th); err != nil {
			fmt.Fprintln(os.Stderr, "ingest:", err)
			db.Close()
			os.Exit(2)
		}
		db.Close()
		os.Exit(0)
	}

	home := t.TempDir()
	run := func(child string) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run", "TestWriterConcurrentProcesses", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"KROWK_WRITER_CHILD="+child,
			"KROWK_WRITER_HOME="+home,
		)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	type outcome struct {
		out string
		err error
	}
	ch := make(chan outcome, 2)
	go func() { o, e := run("1"); ch <- outcome{o, e} }()
	go func() { o, e := run("2"); ch <- outcome{o, e} }()
	var outs []string
	for i := 0; i < 2; i++ {
		o := <-ch
		outs = append(outs, o.out)
		if o.err != nil {
			t.Fatalf("child failed: %v\n%s", o.err, o.out)
		}
		if strings.Contains(o.out, "database is locked") {
			t.Fatalf("child surfaced a lock error:\n%s", o.out)
		}
	}

	db, err := Open(testEnv(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_binding WHERE foreign_session_id IN ('ses-child-1','ses-child-2')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("bindings for both child sessions = %d, want 2", n)
	}
}

// TestWriterConcurrentSameSession runs two processes against one binding
// key with overlapping messages. One writer wins each race; the loser
// retries the whole Ingest against fresh state and converges instead of
// surfacing a UNIQUE failure.
func TestWriterConcurrentSameSession(t *testing.T) {
	if os.Getenv("KROWK_WRITER_CHILD") != "" {
		t.Skip("child process runs inside TestWriterConcurrentProcesses")
	}
	home := t.TempDir()
	run := func(child string) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run", "TestWriterConcurrentProcesses", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"KROWK_WRITER_CHILD="+child,
			"KROWK_WRITER_HOME="+home,
		)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	type outcome struct {
		out string
		err error
	}
	ch := make(chan outcome, 2)
	go func() { o, e := run("same1"); ch <- outcome{o, e} }()
	go func() { o, e := run("same2"); ch <- outcome{o, e} }()
	for i := 0; i < 2; i++ {
		o := <-ch
		if o.err != nil {
			t.Fatalf("child failed: %v\n%s", o.err, o.out)
		}
		if strings.Contains(o.out, "database is locked") {
			t.Fatalf("child surfaced a lock error:\n%s", o.out)
		}
		if strings.Contains(o.out, "UNIQUE constraint failed") {
			t.Fatalf("child surfaced a uniqueness error:\n%s", o.out)
		}
	}

	db, err := Open(testEnv(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	var sessions, bindings, messages, turns, events int
	for _, q := range []struct {
		query string
		dest  *int
	}{
		{`SELECT COUNT(*) FROM session`, &sessions},
		{`SELECT COUNT(*) FROM session_binding`, &bindings},
		{`SELECT COUNT(*) FROM message`, &messages},
		{`SELECT COUNT(*) FROM turn`, &turns},
		{`SELECT COUNT(*) FROM session_event`, &events},
	} {
		if err := db.QueryRow(q.query).Scan(q.dest); err != nil {
			t.Fatal(err)
		}
	}
	if sessions != 1 || bindings != 1 {
		t.Errorf("sessions=%d bindings=%d, want 1 and 1", sessions, bindings)
	}
	if messages != 3 {
		t.Errorf("messages=%d, want 3 (the union of both children)", messages)
	}
	if turns != 1 || events != 1 {
		t.Errorf("turns=%d events=%d, want 1 and 1", turns, events)
	}
	rows, err := db.Query(`SELECT seq FROM message ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int
	for rows.Next() {
		var s int
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		seqs = append(seqs, s)
	}
	rows.Close()
	if len(seqs) != 3 || seqs[0] != 0 || seqs[1] != 1 || seqs[2] != 2 {
		t.Errorf("message seqs = %v, want [0 1 2]", seqs)
	}
}

// TestWriterLargeBatchCrossesChunks ingests more than ingestBatchSize
// messages: seq must stay dense across chunk transactions, and a re-ingest
// must insert nothing.
func TestWriterLargeBatchCrossesChunks(t *testing.T) {
	db, w := openWriterDB(t, nil)
	ctx := context.Background()
	th := sampleThread("claude", "ses_big")
	th.Turns = nil
	th.Events = nil
	for i := 0; i < 1200; i++ {
		th.Messages = append(th.Messages, Message{
			Role: RoleUser, ForeignID: fmt.Sprintf("ses_big-m%04d", i),
			Parts: []Part{{Type: "text"}},
		})
	}
	first, err := w.Ingest(ctx, th)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if first.Messages.Inserted != 1203 || first.Parts.Inserted != 1204 {
		t.Errorf("inserted messages=%d parts=%d, want 1203 and 1204", first.Messages.Inserted, first.Parts.Inserted)
	}
	var n, mn, mx int
	if err := db.QueryRow(`SELECT COUNT(*), MIN(seq), MAX(seq) FROM message`).Scan(&n, &mn, &mx); err != nil {
		t.Fatal(err)
	}
	if n != 1203 || mn != 0 || mx != 1202 {
		t.Errorf("count=%d min=%d max=%d, want 1203 0 1202", n, mn, mx)
	}
	before := tableCounts(t, db)
	second, err := w.Ingest(ctx, th)
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if !zeroInserted(second) {
		t.Errorf("re-ingest inserted rows: %+v", second)
	}
	if after := tableCounts(t, db); !equalCounts(before, after) {
		t.Errorf("counts changed on re-ingest")
	}
}

// TestWriterIntraBatchDuplicateForeignID names the same foreign id twice in
// one Thread: the second occurrence is skipped, never double-inserted.
func TestWriterIntraBatchDuplicateForeignID(t *testing.T) {
	db, w := openWriterDB(t, nil)
	th := sampleThread("claude", "ses_dup")
	th.Turns = nil
	th.Events = nil
	th.Messages = []Message{
		{Role: RoleUser, ForeignID: "dup-m", Parts: []Part{{Type: "text"}}},
		{Role: RoleUser, ForeignID: "dup-m", Parts: []Part{{Type: "text"}}},
	}
	res, err := w.Ingest(context.Background(), th)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Messages.Inserted != 1 || res.Messages.Skipped != 1 {
		t.Errorf("messages inserted=%d skipped=%d, want 1 and 1", res.Messages.Inserted, res.Messages.Skipped)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM message`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("messages=%d, want 1", n)
	}
}

// TestRoleMatchesCheck pins validRole against the DDL: every role the Go
// boundary admits must appear in the message CHECK, and vice versa.
func TestRoleMatchesCheck(t *testing.T) {
	db, _ := openWriterDB(t, nil)
	var sql string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'message'`).Scan(&sql); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Role{RoleUser, RoleAssistant, RoleSystem, RoleTool, RoleError} {
		if !validRole(r) {
			t.Errorf("validRole rejects %q", r)
		}
		if !strings.Contains(sql, `'`+string(r)+`'`) {
			t.Errorf("message CHECK does not mention role %q", r)
		}
	}
	if validRole("admin") {
		t.Errorf("validRole accepts admin")
	}
	if strings.Contains(sql, `'admin'`) {
		t.Errorf("message CHECK mentions admin")
	}
}

func TestWriterPositionRowsNeedNoResend(t *testing.T) {
	// A message with no ForeignID has no dedup key, so re-sending it
	// appends again with a continuing seq. Importers must hold a cursor
	// for those instead of re-sending.
	db, w := openWriterDB(t, nil)
	ctx := context.Background()
	mk := func() Thread {
		th := sampleThread("claude", "ses_cursor")
		th.Messages = []Message{{Role: RoleUser, Parts: []Part{{Type: "text"}}}}
		th.Turns = nil
		th.Events = nil
		return th
	}
	if _, err := w.Ingest(ctx, mk()); err != nil {
		t.Fatalf("first Ingest: %v", err)
	}
	if _, err := w.Ingest(ctx, mk()); err != nil {
		t.Fatalf("second Ingest: %v", err)
	}
	var seqs []int
	rows, err := db.Query(`SELECT seq FROM message ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s int
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		seqs = append(seqs, s)
	}
	rows.Close()
	if len(seqs) != 2 || seqs[0] != 0 || seqs[1] != 1 {
		t.Errorf("message seqs = %v, want [0 1]: cursor-less resend appends", seqs)
	}
}

// parentOf reads a session's parent_id through its binding, as NULL or as
// the id it points at.
func parentOf(t *testing.T, db *sql.DB, provider, foreignID string) (string, bool) {
	t.Helper()
	var parent sql.NullString
	err := db.QueryRow(
		`SELECT s.parent_id FROM session s JOIN session_binding b ON b.session_id = s.id
		 WHERE b.provider = ? AND b.foreign_session_id = ?`, provider, foreignID).Scan(&parent)
	if err != nil {
		t.Fatalf("read parent of %s: %v", foreignID, err)
	}
	return parent.String, parent.Valid
}

// sessionIDOf is the store id behind a binding, for the tests that have to
// compare a parent_id against the row it should name.
func sessionIDOf(t *testing.T, db *sql.DB, provider, foreignID string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(
		`SELECT session_id FROM session_binding WHERE provider = ? AND foreign_session_id = ?`,
		provider, foreignID).Scan(&id); err != nil {
		t.Fatalf("find session %s: %v", foreignID, err)
	}
	return id
}

// childThread is a Thread naming another session as its parent, which is
// what a Claude subagent transcript produces.
func childThread(provider, foreignID, parentForeignID string) Thread {
	th := sampleThread(provider, foreignID)
	th.Parent = &Binding{Provider: provider, Harness: "claude", ForeignSessionID: parentForeignID}
	return th
}

func TestIngestSetsParentWhenTheParentIsAlreadyThere(t *testing.T) {
	db, w := openWriterDB(t, nil)
	ctx := context.Background()

	if _, err := w.Ingest(ctx, sampleThread("claude", "parent")); err != nil {
		t.Fatalf("Ingest parent: %v", err)
	}
	if _, err := w.Ingest(ctx, childThread("claude", "child", "parent")); err != nil {
		t.Fatalf("Ingest child: %v", err)
	}

	got, ok := parentOf(t, db, "claude", "child")
	if !ok {
		t.Fatal("child has no parent_id")
	}
	if want := sessionIDOf(t, db, "claude", "parent"); got != want {
		t.Fatalf("parent_id = %q, want the parent session %q", got, want)
	}
	// And the parent is nobody's child.
	if _, ok := parentOf(t, db, "claude", "parent"); ok {
		t.Fatal("the parent acquired a parent")
	}
}

func TestIngestLeavesParentNullWhenTheParentIsMissing(t *testing.T) {
	db, w := openWriterDB(t, nil)
	ctx := context.Background()

	// The child arrives first, which is what happens to a caller walking
	// files in whatever order the filesystem gave them.
	if _, err := w.Ingest(ctx, childThread("claude", "child", "parent")); err != nil {
		t.Fatalf("Ingest child: %v", err)
	}
	if _, ok := parentOf(t, db, "claude", "child"); ok {
		t.Fatal("child was given a parent nobody has ingested")
	}
}

func TestReingestFillsInAParentThatArrivedLate(t *testing.T) {
	db, w := openWriterDB(t, nil)
	ctx := context.Background()

	child := childThread("claude", "child", "parent")
	if _, err := w.Ingest(ctx, child); err != nil {
		t.Fatalf("Ingest child: %v", err)
	}
	if _, err := w.Ingest(ctx, sampleThread("claude", "parent")); err != nil {
		t.Fatalf("Ingest parent: %v", err)
	}

	// The second pass over the child is what converges: nothing new is
	// inserted, and the NULL is filled in.
	res, err := w.Ingest(ctx, child)
	if err != nil {
		t.Fatalf("re-Ingest child: %v", err)
	}
	if res.Messages.Inserted != 0 || res.Turns.Inserted != 0 || res.Sessions.Inserted != 0 {
		t.Fatalf("re-ingest inserted rows: %+v", res)
	}
	got, ok := parentOf(t, db, "claude", "child")
	if !ok {
		t.Fatal("re-ingest did not fill in the parent")
	}
	if want := sessionIDOf(t, db, "claude", "parent"); got != want {
		t.Fatalf("parent_id = %q, want %q", got, want)
	}
}

// A session that names itself is not stored as its own parent: the foreign
// key would accept it and every tree walk downstream would loop.
func TestIngestRefusesToMakeASessionItsOwnParent(t *testing.T) {
	db, w := openWriterDB(t, nil)
	ctx := context.Background()

	if _, err := w.Ingest(ctx, childThread("claude", "loop", "loop")); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if _, ok := parentOf(t, db, "claude", "loop"); ok {
		t.Fatal("a session became its own parent")
	}
}

// A parent already set is not rewritten: one importer's reading of one file
// must not silently re-home a session that another pass already placed.
func TestIngestDoesNotRepointAnExistingParent(t *testing.T) {
	db, w := openWriterDB(t, nil)
	ctx := context.Background()

	for _, id := range []string{"first", "second"} {
		if _, err := w.Ingest(ctx, sampleThread("claude", id)); err != nil {
			t.Fatalf("Ingest %s: %v", id, err)
		}
	}
	if _, err := w.Ingest(ctx, childThread("claude", "child", "first")); err != nil {
		t.Fatalf("Ingest child: %v", err)
	}
	if _, err := w.Ingest(ctx, childThread("claude", "child", "second")); err != nil {
		t.Fatalf("re-Ingest child: %v", err)
	}
	got, _ := parentOf(t, db, "claude", "child")
	if want := sessionIDOf(t, db, "claude", "first"); got != want {
		t.Fatalf("parent_id = %q, want it still pointing at the first parent %q", got, want)
	}
}
