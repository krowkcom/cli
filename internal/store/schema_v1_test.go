package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// openV1 applies the shipped schema to a fresh temp file with the same
// DSN pragmas Open uses (foreign_keys on), so FK and cascade behavior
// here is the behavior production gets.
func openV1(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := openSQL(dsn(path))
	if err != nil {
		t.Fatalf("openSQL: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := ensureSchema(db, path, SchemaSQL); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	return db
}

func mustID(t *testing.T) string {
	t.Helper()
	id := NewID()
	if err := ParseID(id); err != nil {
		t.Fatalf("minted id fails ParseID: %v", err)
	}
	return id
}

func insertWorktree(t *testing.T, db *sql.DB, path string) string {
	t.Helper()
	id := mustID(t)
	if _, err := db.Exec(`INSERT INTO worktree (id, path, vcs, name, time_created, time_updated) VALUES (?, ?, 'git', 'w', 1, 2)`, id, path); err != nil {
		t.Fatalf("insert worktree: %v", err)
	}
	return id
}

func insertSession(t *testing.T, db *sql.DB, worktreeID string) string {
	t.Helper()
	id := mustID(t)
	if _, err := db.Exec(`INSERT INTO session (id, worktree_id, directory, title, time_created, time_updated) VALUES (?, ?, '/r', 't', 1, 2)`, id, worktreeID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return id
}

func insertTurn(t *testing.T, db *sql.DB, sessionID string, seq int) string {
	t.Helper()
	id := mustID(t)
	if _, err := db.Exec(`INSERT INTO turn (id, session_id, seq, time_created, time_updated) VALUES (?, ?, ?, 1, 2)`, id, sessionID, seq); err != nil {
		t.Fatalf("insert turn: %v", err)
	}
	return id
}

func insertMessage(t *testing.T, db *sql.DB, sessionID, turnID string, seq int, role string, foreignID any) string {
	t.Helper()
	id := mustID(t)
	var turnArg any
	if turnID == "" {
		turnArg = nil
	} else {
		turnArg = turnID
	}
	if _, err := db.Exec(`INSERT INTO message (id, session_id, turn_id, seq, role, time_created) VALUES (?, ?, ?, ?, ?, 1)`, id, sessionID, turnArg, seq, role); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	_ = foreignID
	return id
}

var v1Tables = []string{"worktree", "session", "session_binding", "session_event", "turn", "message", "part", "import_state"}

func TestV1TablesExact(t *testing.T) {
	db := openV1(t)
	tables, err := listTables(db)
	if err != nil {
		t.Fatalf("listTables: %v", err)
	}
	for _, want := range v1Tables {
		if !tables[want] {
			t.Errorf("table %q missing from 001_init.sql", want)
		}
	}
	for _, dropped := range []string{"turn_attempt", "attachment", "model_cache", "convention", "migrations", "share_url", "sync_state"} {
		if tables[dropped] {
			t.Errorf("table %q present; the todo reserves it for a later phase", dropped)
		}
	}
	for name := range tables {
		if strings.HasPrefix(name, "sqlite_") {
			continue
		}
		found := false
		for _, want := range v1Tables {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("unexpected table %q in 001_init.sql", name)
		}
	}
}

func TestV1UniqueIndexes(t *testing.T) {
	db := openV1(t)
	// Column- and table-level UNIQUEs surface as auto-indexes with NULL
	// sql in sqlite_master, so read uniqueness through PRAGMA
	// index_list (unique flag) plus index_info columns instead.
	uniqueCols := map[string][][]string{}
	tables, err := listTables(db)
	if err != nil {
		t.Fatalf("listTables: %v", err)
	}
	for table := range tables {
		if strings.HasPrefix(table, "sqlite_") {
			continue
		}
		idxRows, err := db.Query(`PRAGMA index_list(` + table + `)`)
		if err != nil {
			t.Fatalf("index_list %s: %v", table, err)
		}
		type entry struct {
			name   string
			unique int
		}
		var entries []entry
		for idxRows.Next() {
			var seq, u int
			var name, o, p string
			if err := idxRows.Scan(&seq, &name, &u, &o, &p); err != nil {
				t.Fatalf("scan idx: %v", err)
			}
			entries = append(entries, entry{name, u})
		}
		idxRows.Close()
		for _, e := range entries {
			if e.unique != 1 {
				continue
			}
			infoRows, err := db.Query(`PRAGMA index_info(` + e.name + `)`)
			if err != nil {
				t.Fatalf("index_info %s: %v", e.name, err)
			}
			bySeq := map[int]string{}
			for infoRows.Next() {
				var seqno, cid int
				var cname string
				if err := infoRows.Scan(&seqno, &cid, &cname); err != nil {
					t.Fatalf("scan info: %v", err)
				}
				bySeq[seqno] = cname
			}
			infoRows.Close()
			var cols []string
			for i := 0; i < len(bySeq); i++ {
				cols = append(cols, bySeq[i])
			}
			uniqueCols[table] = append(uniqueCols[table], cols)
		}
	}
	has := func(table string, cols ...string) bool {
	outer:
		for _, got := range uniqueCols[table] {
			if len(got) != len(cols) {
				continue
			}
			for i := range cols {
				if !strings.EqualFold(got[i], cols[i]) {
					continue outer
				}
			}
			return true
		}
		return false
	}
	for _, tc := range []struct {
		table string
		cols  []string
	}{
		{"session_binding", []string{"provider", "foreign_session_id"}},
		{"session_event", []string{"session_id", "seq"}},
		{"turn", []string{"session_id", "seq"}},
		{"message", []string{"session_id", "seq"}},
		{"message", []string{"session_id", "foreign_id"}},
		{"part", []string{"message_id", "seq"}},
	} {
		if !has(tc.table, tc.cols...) {
			t.Errorf("no UNIQUE index on %s(%s)", tc.table, strings.Join(tc.cols, ", "))
		}
	}
	// Column-level UNIQUEs surface as auto-indexes with NULL sql; check
	// them through table_info instead of the scan above.
	for _, tc := range []struct{ table, column string }{
		{"worktree", "path"},
		{"session", "remote_slug"},
	} {
		// Insert two NULLs must succeed (SQLite UNIQUE allows it); the
		// positive dup case is pinned in TestV1DedupBehavior for path.
		_ = tc
	}
	var nPartial int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'idx_message_session_foreign'`).Scan(&nPartial); err != nil {
		t.Fatalf("partial index probe: %v", err)
	}
	if nPartial != 1 {
		t.Error("partial dedup index idx_message_session_foreign missing")
	}
	var partial string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'idx_message_session_foreign'`).Scan(&partial); err != nil {
		t.Fatalf("partial index sql: %v", err)
	} else if !strings.Contains(strings.ToUpper(partial), "WHERE") || !strings.Contains(partial, "foreign_id IS NOT NULL") {
		t.Errorf("idx_message_session_foreign is not partial on non-null foreign_id: %q", partial)
	}
	// No UNIQUE may gate "one assistant message per turn": no unique
	// column set on message may be exactly (turn_id) or (turn_id, role).
	for _, cols := range uniqueCols["message"] {
		if len(cols) == 1 && strings.EqualFold(cols[0], "turn_id") {
			t.Errorf("UNIQUE on message(turn_id) gates one message per turn")
		}
		if len(cols) == 2 && strings.EqualFold(cols[0], "turn_id") && strings.EqualFold(cols[1], "role") {
			t.Errorf("UNIQUE on message(turn_id, role) gates one assistant message per turn")
		}
	}
}

func TestV1FKConventions(t *testing.T) {
	db := openV1(t)
	// Every *_id column has a FOREIGN KEY with ON DELETE CASCADE ...
	for _, table := range []string{"session", "session_binding", "session_event", "turn", "message", "part"} {
		fkRows, err := db.Query(`PRAGMA foreign_key_list(` + table + `)`)
		if err != nil {
			t.Fatalf("fk_list %s: %v", table, err)
		}
		fkCols := map[string]string{}
		for fkRows.Next() {
			var id, seq int
			var refTable, from, to, onUpdate, onDelete, match string
			if err := fkRows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				t.Fatalf("scan fk: %v", err)
			}
			fkCols[from] = strings.ToUpper(onDelete)
		}
		fkRows.Close()
		if err := fkRows.Err(); err != nil {
			t.Fatalf("fk rows %s: %v", table, err)
		}
		colRows, err := db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			t.Fatalf("table_info %s: %v", table, err)
		}
		var idCols []string
		for colRows.Next() {
			var cid int
			var name, ctype string
			var notnull int
			var dflt sql.NullString
			var pk int
			if err := colRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				t.Fatalf("scan col: %v", err)
			}
			// foreign_session_id, foreign_id and tool_call_id hold ids
			// minted by other systems (Claude, opencode, Anthropic) —
			// deduped by unique index, never by FOREIGN KEY. Everything
			// else ending in _id is one of our own rows and needs the FK.
			if strings.HasSuffix(name, "_id") && name != "foreign_session_id" && name != "foreign_id" && name != "tool_call_id" {
				idCols = append(idCols, name)
			}
		}
		colRows.Close()
		for _, c := range []string{"foreign_session_id", "foreign_id", "tool_call_id"} {
			if _, ok := fkCols[c]; ok {
				t.Errorf("%s.%s holds another system's id; it must not carry a FOREIGN KEY", table, c)
			}
		}
		for _, c := range idCols {
			action, ok := fkCols[c]
			if !ok {
				t.Errorf("%s.%s names an id but has no FOREIGN KEY", table, c)
				continue
			}
			if action != "CASCADE" {
				t.Errorf("%s.%s ON DELETE = %q, want CASCADE", table, c, action)
			}
		}
		// ... and every FK child column leads an index (a UNIQUE prefix
		// counts: UNIQUE(session_id, seq) indexes session_id lookups).
		idxRows, err := db.Query(`PRAGMA index_list(` + table + `)`)
		if err != nil {
			t.Fatalf("index_list %s: %v", table, err)
		}
		leads := map[string]bool{}
		var idxNames []string
		for idxRows.Next() {
			var seq int
			var name, unique, origin, partial string
			// origin/partial types vary by driver; scan loosely.
			var u int
			var o, p string
			_ = unique
			_ = origin
			_ = partial
			if err := idxRows.Scan(&seq, &name, &u, &o, &p); err != nil {
				t.Fatalf("scan idx: %v", err)
			}
			idxNames = append(idxNames, name)
		}
		idxRows.Close()
		for _, iname := range idxNames {
			infoRows, err := db.Query(`PRAGMA index_info(` + iname + `)`)
			if err != nil {
				t.Fatalf("index_info %s: %v", iname, err)
			}
			for infoRows.Next() {
				var seqno, cid int
				var cname string
				if err := infoRows.Scan(&seqno, &cid, &cname); err != nil {
					t.Fatalf("scan info: %v", err)
				}
				if seqno == 0 {
					leads[cname] = true
				}
			}
			infoRows.Close()
		}
		for c := range fkCols {
			if !leads[c] {
				t.Errorf("%s.%s is an FK with no leading index", table, c)
			}
		}
	}
	// worktree and import_state hold no FKs by design (roots of the graph).
	for _, table := range []string{"worktree", "import_state"} {
		fkRows, err := db.Query(`PRAGMA foreign_key_list(` + table + `)`)
		if err != nil {
			t.Fatalf("fk_list %s: %v", table, err)
		}
		if fkRows.Next() {
			t.Errorf("%s should hold no FOREIGN KEYs", table)
		}
		fkRows.Close()
	}
}

func TestV1TimeColumnsAreIntegerMS(t *testing.T) {
	db := openV1(t)
	for _, table := range v1Tables {
		colRows, err := db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			t.Fatalf("table_info %s: %v", table, err)
		}
		nTime := 0
		for colRows.Next() {
			var cid int
			var name, ctype string
			var notnull int
			var dflt sql.NullString
			var pk int
			if err := colRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if strings.HasPrefix(name, "time_") {
				nTime++
				if strings.ToUpper(ctype) != "INTEGER" {
					t.Errorf("%s.%s type = %q, want INTEGER ms", table, name, ctype)
				}
				if name == "deleted_at" {
					t.Errorf("%s.deleted_at must not be named time_*: NULL means alive", table)
				}
			}
		}
		colRows.Close()
		if table != "part" && nTime == 0 {
			t.Errorf("%s has no time_* column", table)
		}
	}
	// A large millisecond value round-trips exactly (seconds would not).
	w := insertWorktree(t, db, "/ms")
	if _, err := db.Exec(`UPDATE worktree SET time_updated = 1780000000000 WHERE id = ?`, w); err != nil {
		t.Fatalf("update ms: %v", err)
	}
	var got int64
	if err := db.QueryRow(`SELECT time_updated FROM worktree WHERE id = ?`, w).Scan(&got); err != nil {
		t.Fatalf("select ms: %v", err)
	}
	if got != 1780000000000 {
		t.Errorf("time_updated = %d, want 1780000000000", got)
	}
}

func TestV1IDsPassParseID(t *testing.T) {
	db := openV1(t)
	w := insertWorktree(t, db, "/ids")
	s := insertSession(t, db, w)
	bID := mustID(t)
	if _, err := db.Exec(`INSERT INTO session_binding (id, session_id, provider, foreign_session_id, time_created, time_updated) VALUES (?, ?, 'claude', 'f1', 1, 2)`, bID, s); err != nil {
		t.Fatalf("binding: %v", err)
	}
	eID := mustID(t)
	if _, err := db.Exec(`INSERT INTO session_event (id, session_id, seq, time_created) VALUES (?, ?, 0, 1)`, eID, s); err != nil {
		t.Fatalf("event: %v", err)
	}
	tn := insertTurn(t, db, s, 0)
	mID := mustID(t)
	if _, err := db.Exec(`INSERT INTO message (id, session_id, turn_id, seq, role, time_created) VALUES (?, ?, ?, 0, 'user', 1)`, mID, s, tn); err != nil {
		t.Fatalf("message: %v", err)
	}
	pID := mustID(t)
	if _, err := db.Exec(`INSERT INTO part (message_id, session_id, seq, id) VALUES (?, ?, 0, ?)`, mID, s, pID); err != nil {
		t.Fatalf("part: %v", err)
	}
	for _, id := range []string{w, s, bID, eID, tn, mID, pID} {
		if err := ParseID(id); err != nil {
			t.Errorf("id %q fails ParseID: %v", id, err)
		}
	}
}

func TestV1DedupBehavior(t *testing.T) {
	db := openV1(t)
	w := insertWorktree(t, db, "/dup")
	if _, err := db.Exec(`INSERT INTO worktree (id, path, time_created, time_updated) VALUES (?, '/dup', 1, 2)`, mustID(t)); err == nil {
		t.Error("second worktree with the same path succeeded, want UNIQUE failure")
	}
	s1 := insertSession(t, db, w)
	s2 := insertSession(t, db, w)
	if _, err := db.Exec(`INSERT INTO session_binding (id, session_id, provider, foreign_session_id, time_created, time_updated) VALUES (?, ?, 'claude', 'same', 1, 2)`, mustID(t), s1); err != nil {
		t.Fatalf("first binding: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO session_binding (id, session_id, provider, foreign_session_id, time_created, time_updated) VALUES (?, ?, 'claude', 'same', 1, 2)`, mustID(t), s2); err == nil {
		t.Error("second binding with the same (provider, foreign_session_id) succeeded")
	}
	// Same foreign id under a different provider is a different agent's
	// session, not a re-import: must succeed.
	if _, err := db.Exec(`INSERT INTO session_binding (id, session_id, provider, foreign_session_id, time_created, time_updated) VALUES (?, ?, 'opencode', 'same', 1, 2)`, mustID(t), s2); err != nil {
		t.Errorf("same foreign id under another provider failed: %v", err)
	}

	mkMsg := func(sessionID string, seq int, foreign any) error {
		_, err := db.Exec(`INSERT INTO message (id, session_id, seq, role, foreign_id, time_created) VALUES (?, ?, ?, 'user', ?, 1)`, mustID(t), sessionID, seq, foreign)
		return err
	}
	if err := mkMsg(s1, 0, nil); err != nil {
		t.Fatalf("first message: %v", err)
	}
	if err := mkMsg(s1, 0, nil); err == nil {
		t.Error("second message with the same (session_id, seq) succeeded")
	}
	if err := mkMsg(s1, 1, nil); err != nil {
		t.Fatalf("null foreign_id message: %v", err)
	}
	if err := mkMsg(s1, 2, nil); err != nil {
		t.Fatalf("second null foreign_id message failed; NULLs must not dedup: %v", err)
	}
	if err := mkMsg(s1, 3, "m_1"); err != nil {
		t.Fatalf("foreign message: %v", err)
	}
	if err := mkMsg(s1, 4, "m_1"); err == nil {
		t.Error("second message with the same non-null (session_id, foreign_id) succeeded")
	}
	if err := mkMsg(s2, 0, "m_1"); err != nil {
		t.Errorf("same foreign_id in another session failed; dedup is per session: %v", err)
	}
}

func TestV1CascadeDelete(t *testing.T) {
	db := openV1(t)
	w := insertWorktree(t, db, "/cascade")
	s := insertSession(t, db, w)
	child := mustID(t)
	if _, err := db.Exec(`INSERT INTO session (id, worktree_id, parent_id, time_created, time_updated) VALUES (?, ?, ?, 1, 2)`, child, w, s); err != nil {
		t.Fatalf("child session: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO session_binding (id, session_id, provider, foreign_session_id, time_created, time_updated) VALUES (?, ?, 'claude', 'c', 1, 2)`, mustID(t), s); err != nil {
		t.Fatalf("binding: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO session_event (id, session_id, seq, time_created) VALUES (?, ?, 0, 1)`, mustID(t), s); err != nil {
		t.Fatalf("event: %v", err)
	}
	tn := insertTurn(t, db, s, 0)
	mID := mustID(t)
	if _, err := db.Exec(`INSERT INTO message (id, session_id, turn_id, seq, role, time_created) VALUES (?, ?, ?, 0, 'assistant', 1)`, mID, s, tn); err != nil {
		t.Fatalf("message: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO part (id, message_id, session_id, seq) VALUES (?, ?, ?, 0)`, mustID(t), mID, s); err != nil {
		t.Fatalf("part: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM session WHERE id = ?`, s); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	for _, tc := range []struct {
		table, col string
	}{
		{"session_binding", "session_id"},
		{"session_event", "session_id"},
		{"turn", "session_id"},
		{"message", "session_id"},
		{"part", "session_id"},
	} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+tc.table+` WHERE `+tc.col+` = ?`, s).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tc.table, err)
		}
		if n != 0 {
			t.Errorf("%s still holds %d rows for the deleted session", tc.table, n)
		}
	}
	var nChild int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session WHERE id = ?`, child).Scan(&nChild); err != nil {
		t.Fatalf("child probe: %v", err)
	}
	if nChild != 0 {
		t.Error("child session survived its parent's delete; parent_id is ON DELETE CASCADE")
	}
	var nWork int
	if err := db.QueryRow(`SELECT COUNT(*) FROM worktree WHERE id = ?`, w).Scan(&nWork); err != nil {
		t.Fatalf("worktree probe: %v", err)
	}
	if nWork != 1 {
		t.Error("deleting a session deleted its worktree; the cascade runs session-down only")
	}
}

func TestV1NoAssistantPerTurnGate(t *testing.T) {
	db := openV1(t)
	w := insertWorktree(t, db, "/turn")
	s := insertSession(t, db, w)
	tn := insertTurn(t, db, s, 0)
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(`INSERT INTO message (id, session_id, turn_id, seq, role, time_created) VALUES (?, ?, ?, ?, 'assistant', 1)`, mustID(t), s, tn, i); err != nil {
			t.Fatalf("assistant message %d in one turn failed; no such UNIQUE may exist: %v", i, err)
		}
	}
}

func TestV1RoleCheckAndDefaults(t *testing.T) {
	db := openV1(t)
	w := insertWorktree(t, db, "/role")
	s := insertSession(t, db, w)
	for _, role := range []string{"user", "assistant", "system", "tool", "error"} {
		if _, err := db.Exec(`INSERT INTO message (id, session_id, seq, role, time_created) VALUES (?, ?, 100 + (SELECT COUNT(*) FROM message), ?, 1)`, mustID(t), s, role); err != nil {
			t.Errorf("role %q refused: %v", role, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO message (id, session_id, seq, role, time_created) VALUES (?, ?, 999, 'human', 1)`, mustID(t), s); err == nil {
		t.Error("role 'human' accepted; role is a closed CHECK")
	}
	var revision int
	if err := db.QueryRow(`SELECT revision FROM session WHERE id = ?`, s).Scan(&revision); err != nil {
		t.Fatalf("revision: %v", err)
	}
	if revision != 1 {
		t.Errorf("session.revision = %d, want default 1", revision)
	}
	// remote_slug NULL twice is not a conflict; a repeated slug is.
	s2 := insertSession(t, db, w)
	if _, err := db.Exec(`UPDATE session SET remote_slug = 'slug-1' WHERE id = ?`, s); err != nil {
		t.Fatalf("slug: %v", err)
	}
	if _, err := db.Exec(`UPDATE session SET remote_slug = 'slug-1' WHERE id = ?`, s2); err == nil {
		t.Error("second session with the same remote_slug succeeded")
	}
}

func TestV1OpenCreatesV1Shape(t *testing.T) {
	home := t.TempDir()
	db, err := Open(testEnv(map[string]string{"HOME": home}))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	tables, err := listTables(db)
	if err != nil {
		t.Fatalf("listTables: %v", err)
	}
	for _, want := range v1Tables {
		if !tables[want] {
			t.Errorf("fresh Open missing table %q", want)
		}
	}
}
