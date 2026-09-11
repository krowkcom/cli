package opencode

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/krowkcom/cli/internal/harness"
	"github.com/krowkcom/cli/internal/importer"
	"github.com/krowkcom/cli/internal/store"
)

// update regenerates the golden threads. A flag rather than an environment
// variable so that regenerating is something typed on purpose, the same way
// the Claude reader's golden works.
var update = flag.Bool("update", false, "rewrite testdata/golden.json from the current import")

// The identifiers the fixture is built around, written out here so a test
// asserting on one is not asserting on a string it also produced.
const (
	fixtureParent = "ses_parent"
	fixtureChild  = "ses_child"
)

// fixtureTimestamps are the message time_updated values opencode.sql writes
// by hand, so the cursor tests hold the watermark against numbers that did
// not come out of the reader.
const (
	fixtureParentCursor = 1757000000405
	fixtureChildCursor  = 1757000001205
)

// fixture is one materialised copy of testdata: a home directory holding a
// real opencode.db built from the checked-in SQL, and a real directory for
// the project worktree to point at.
type fixture struct {
	env      harness.Env
	home     string
	worktree string
	dbFile   string
}

// newFixture builds a temp opencode.db by executing
// testdata/opencode/opencode.sql with the worktree placeholder filled in.
//
// The database is built through SQL rather than checked in as a binary:
// a binary .db is opaque in review and rots with the SQLite version, while
// the SQL states exactly what the tests hold against.
func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{
		home:     filepath.Join(root, "home"),
		worktree: filepath.Join(root, "repo"),
	}
	f.env = func(k string) string {
		if k == "HOME" {
			return f.home
		}
		return ""
	}
	if err := os.MkdirAll(f.worktree, 0o700); err != nil {
		t.Fatalf("MkdirAll worktree: %v", err)
	}
	dir := filepath.Join(f.home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll db dir: %v", err)
	}
	f.dbFile = filepath.Join(dir, "opencode.db")

	raw, err := os.ReadFile(filepath.Join("testdata", "opencode", "opencode.sql"))
	if err != nil {
		t.Fatalf("ReadFile fixture sql: %v", err)
	}
	sqlText := strings.ReplaceAll(string(raw), "{{WORKTREE}}", f.worktree)
	if strings.Contains(sqlText, "{{") {
		t.Fatalf("fixture sql has an unfilled placeholder")
	}

	db, err := sql.Open(store.DriverName, "file:"+f.dbFile)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range splitStatements(sqlText) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", trunc(stmt), err)
		}
	}
	return f
}

// splitStatements cuts the fixture SQL into executable statements, dropping
// the comment lines. Splitting on the terminator rather than executing the
// whole file keeps one bad statement's error naming the statement.
func splitStatements(sqlText string) []string {
	var kept []string
	for _, line := range strings.Split(sqlText, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	var stmts []string
	for _, stmt := range strings.Split(strings.Join(kept, "\n"), ";") {
		if strings.TrimSpace(stmt) != "" {
			stmts = append(stmts, stmt)
		}
	}
	return stmts
}

func trunc(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// unresolve puts the placeholders back, so a golden file holds
// "{{WORKTREE}}" rather than whatever /tmp directory this run got.
func (f fixture) unresolve(s string) string {
	s = strings.ReplaceAll(s, f.worktree, "{{WORKTREE}}")
	s = strings.ReplaceAll(s, f.home, "{{HOME}}")
	return s
}

func (f fixture) read(t *testing.T, ref importer.Ref) (store.Thread, importer.Cursor, importer.Result) {
	t.Helper()
	th, cur, res, err := Source{}.Read(f.env, ref, nil)
	if err != nil {
		t.Fatalf("Read %s: %v", ref.ID, err)
	}
	return th, cur, res
}

func refByID(t *testing.T, refs []importer.Ref, id string) importer.Ref {
	t.Helper()
	for _, r := range refs {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no ref %q in %+v", id, refs)
	return importer.Ref{}
}

func discover(t *testing.T, f fixture) []importer.Ref {
	t.Helper()
	refs, err := Source{}.Discover(f.env)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return refs
}

func TestDiscoverListsOneRefPerSession(t *testing.T) {
	f := newFixture(t)
	refs := discover(t, f)

	var got []string
	for _, r := range refs {
		if r.Provider != importer.ProviderOpencode {
			t.Fatalf("ref %s has provider %q", r.ID, r.Provider)
		}
		if r.Path != dbRel {
			t.Fatalf("ref %s has path %q, want the db path %q", r.ID, r.Path, dbRel)
		}
		got = append(got, r.ID)
	}
	// Sorted by session id, which is what keeps two runs agreeing.
	want := []string{fixtureChild, fixtureParent}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Discover = %v, want %v", got, want)
	}
	// The import_state key is built off the ref, so it is worth pinning
	// that two sessions do not collide.
	if refs[0].Key() == refs[1].Key() {
		t.Fatalf("two sessions share the key %q", refs[0].Key())
	}
	if want := "opencode:" + fixtureParent; refs[1].Key() != want {
		t.Fatalf("key = %q, want %q", refs[1].Key(), want)
	}
}

func TestDiscoverOnAMachineWithNoTranscripts(t *testing.T) {
	home := t.TempDir()
	env := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	refs, err := Source{}.Discover(env)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("Discover = %+v, want nothing", refs)
	}
}

// TestGolden is the whole import held against a checked-in canonical form.
// It is one assertion covering every field of every row, which is what
// makes the smaller tests below able to be about one thing each.
func TestGolden(t *testing.T) {
	f := newFixture(t)
	refs := discover(t, f)

	var threads []canonicalThread
	for _, ref := range refs {
		th, cur, res := f.read(t, ref)
		threads = append(threads, canonicalise(f, ref, th, cur, res))
	}
	encoded, err := json.MarshalIndent(threads, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	encoded = append(encoded, '\n')

	path := filepath.Join("testdata", "golden.json")
	if *update {
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — run `go test ./internal/importer/opencode -run TestGolden -update` to create it", err)
	}
	if string(golden) != string(encoded) {
		t.Fatalf("the import no longer matches %s.\n\nIf the change is intended, regenerate it:\n  go test ./internal/importer/opencode -run TestGolden -update\n\nfirst difference: %s",
			path, firstDifference(string(golden), string(encoded)))
	}
}

func firstDifference(golden, current string) string {
	was, now := strings.Split(golden, "\n"), strings.Split(current, "\n")
	for i := 0; i < len(was) || i < len(now); i++ {
		a, b := "", ""
		if i < len(was) {
			a = was[i]
		}
		if i < len(now) {
			b = now[i]
		}
		if a != b {
			return fmt.Sprintf("line %d\n  golden:  %s\n  current: %s", i+1, a, b)
		}
	}
	return "none"
}

// TestReadOnlyNoWal is the read-only acceptance: importing must leave the
// database file byte-identical and create no sidecars, because opencode
// holds the database open in WAL mode while it runs and a reader that
// wrote would lock against the live agent. The fixture is switched to WAL
// with a writer held open across the reads, so this proves the point
// against the mode opencode actually uses rather than a delete-mode file
// nobody contends.
func TestReadOnlyNoWal(t *testing.T) {
	f := newFixture(t)

	wal, err := sql.Open(store.DriverName, "file:"+f.dbFile)
	if err != nil {
		t.Fatalf("open writer db: %v", err)
	}
	defer func() { _ = wal.Close() }()
	if _, err := wal.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("journal_mode=WAL: %v", err)
	}
	// One real write so the -wal exists before the reads do: a WAL read
	// maps shared memory next to it, and the assertion below is that the
	// import adds nothing of its own, not that WAL files do not exist.
	// The row is removed again so the fixture sessions read unchanged.
	execFixture(t, wal, `INSERT INTO session (id, project_id, parent_id, directory, title, model, time_created, time_updated) VALUES ('ses_scratch', 'prj_1', NULL, 'x', 'scratch', NULL, 1, 1)`)
	execFixture(t, wal, `DELETE FROM session WHERE id = 'ses_scratch'`)

	before := hashFile(t, f.dbFile)
	beforeWal := hashSidecar(t, f.dbFile+"-wal")
	// The WAL switch above leaves -wal/-shm behind by design; what the
	// import must not do is add anything of its own.
	hadSidecar := map[string]bool{}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err := os.Stat(f.dbFile + suffix)
		hadSidecar[suffix] = !os.IsNotExist(err)
	}

	refs := discover(t, f)
	for _, ref := range refs {
		f.read(t, ref)
	}

	if got := hashFile(t, f.dbFile); got != before {
		t.Fatalf("database changed by import: %s -> %s", before, got)
	}
	// The -wal holds the writer's frames; a read-only import neither
	// appends to it nor checkpoints it away. (-shm is shared memory by
	// design and malleable on any WAL read, so only its presence is
	// held, below.)
	if got := hashSidecar(t, f.dbFile+"-wal"); got != beforeWal {
		t.Fatalf("-wal changed by import: %s -> %s", beforeWal, got)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err := os.Stat(f.dbFile + suffix)
		if got, want := !os.IsNotExist(err), hadSidecar[suffix]; got != want {
			t.Fatalf("import changed sidecar %s (present=%v, was=%v)", f.dbFile+suffix, got, want)
		}
	}
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hashSidecar hashes a sidecar that may not exist: absence hashes as
// absence, so a file created mid-test still fails the comparison.
func hashSidecar(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "absent"
	}
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestEveryToolResultHasItsCall is the pairing acceptance. Nothing in the
// schema joins a result to its call, so if the importer loses the id the
// loss is invisible until somebody tries to render a transcript.
func TestEveryToolResultHasItsCall(t *testing.T) {
	f := newFixture(t)
	refs := discover(t, f)
	parent := refByID(t, refs, fixtureParent)

	th, _, _ := f.read(t, parent)
	calls := map[string]bool{}
	results := 0
	for _, m := range th.Messages {
		for _, p := range m.Parts {
			switch p.Type {
			case importer.PartToolCall:
				if p.ToolCallID == "" {
					t.Fatalf("tool_call with no id")
				}
				calls[p.ToolCallID] = true
			case importer.PartToolResult:
				results++
				if !calls[p.ToolCallID] {
					t.Fatalf("tool_result %q has no earlier tool_call", p.ToolCallID)
				}
			}
		}
	}
	// Two finished tools in the fixture (completed + error); the running
	// one is a call alone. A literal rather than a computed number: a test
	// that derives it from the same code proves nothing.
	if results != 2 {
		t.Fatalf("got %d tool results, want 2", results)
	}

	// And the running tool really is unpaired, not silently twinned.
	var runningCalls, runningResults int
	for _, m := range th.Messages {
		for _, p := range m.Parts {
			if p.ToolCallID == "call_3" {
				if p.Type == importer.PartToolCall {
					runningCalls++
				}
				if p.Type == importer.PartToolResult {
					runningResults++
				}
			}
		}
	}
	if runningCalls != 1 || runningResults != 0 {
		t.Fatalf("running tool has %d calls and %d results, want 1 and 0", runningCalls, runningResults)
	}
}

// TestPartCounts holds the twin rule to its arithmetic: the canonical
// parts are the source rows plus exactly one extra part per finished tool,
// and every part type is one the contract knows.
func TestPartCounts(t *testing.T) {
	f := newFixture(t)
	refs := discover(t, f)

	// Hand-counted off the fixture: the parent holds 12 part rows, the
	// child 2.
	for _, tc := range []struct {
		session string
		rows    int
	}{
		{fixtureParent, 12},
		{fixtureChild, 2},
	} {
		th, _, _ := f.read(t, refByID(t, refs, tc.session))
		rows := countSourceParts(t, f, tc.session)
		if rows != tc.rows {
			t.Fatalf("%s: source holds %d part rows, want %d", tc.session, rows, tc.rows)
		}
		var canonical, twins int
		for _, m := range th.Messages {
			for _, p := range m.Parts {
				if !importer.KnownPartType(p.Type) {
					t.Fatalf("%s: part type %q is not canonical", tc.session, p.Type)
				}
				canonical++
				if p.Type == importer.PartToolResult {
					twins++
				}
				if p.ForeignID == "" {
					t.Fatalf("%s: part of message %s has no foreign id", tc.session, m.ForeignID)
				}
			}
		}
		if canonical < rows {
			t.Fatalf("%s: %d canonical parts from %d source rows", tc.session, canonical, rows)
		}
		if canonical-rows != twins {
			t.Fatalf("%s: %d canonical - %d source = %d, want the %d tool twins",
				tc.session, canonical, rows, canonical-rows, twins)
		}
	}
}

func countSourceParts(t *testing.T, f fixture, sessionID string) int {
	t.Helper()
	db, err := sql.Open(store.DriverName, "file:"+f.dbFile+"?mode=ro")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM part WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		t.Fatalf("count parts: %v", err)
	}
	return n
}

// TestTurnCosts holds the costing rule against an independent recompute:
// the token columns summed over each turn's span, and micros as
// round(sum(cost)*1e6). The recompute walks the database with plain SQL
// rather than the importer's structs, so it is still a check.
func TestTurnCosts(t *testing.T) {
	f := newFixture(t)
	th, _, _ := f.read(t, refByID(t, discover(t, f), fixtureParent))

	want := turnCostsFromDB(t, f, fixtureParent)
	if len(th.Turns) != len(want) {
		t.Fatalf("got %d turns, want %d", len(th.Turns), len(want))
	}
	// Two prompts in the fixture, no leading span: the first message is a
	// prompt, so there is nothing before it to span.
	if len(th.Turns) != 2 {
		t.Fatalf("got %d turns, want 2 prompts", len(th.Turns))
	}
	for i, turn := range th.Turns {
		w := want[i]
		if turn.CostInput != w.input || turn.CostOutput != w.output ||
			turn.CostReasoning != w.reasoning || turn.CostCacheRead != w.read ||
			turn.CostCacheWrite != w.write {
			t.Fatalf("turn %d tokens = %+v, want %+v", i, turn, w)
		}
		if sum := turn.CostInput + turn.CostOutput + turn.CostCacheRead + turn.CostCacheWrite; turn.CostTotal != sum {
			t.Fatalf("turn %d total %d != %d, the sum of its priced classes (reasoning excluded, as in the Claude reader)", i, turn.CostTotal, sum)
		}
		if turn.CostUSDMicros == nil {
			t.Fatalf("turn %d carries no dollar cost", i)
		}
		if *turn.CostUSDMicros != w.micros {
			t.Fatalf("turn %d micros = %d, want %d", i, *turn.CostUSDMicros, w.micros)
		}
		if turn.Status != "done" {
			t.Fatalf("turn %d has status %q", i, turn.Status)
		}
	}
}

type wantTurn struct {
	input, output, reasoning, read, write int64
	micros                                int64
}

// turnCostsFromDB rebuilds the expected turns straight from the database:
// spans split at user messages carrying content, costs summed per span.
func turnCostsFromDB(t *testing.T, f fixture, sessionID string) []wantTurn {
	t.Helper()
	db, err := sql.Open(store.DriverName, "file:"+f.dbFile+"?mode=ro")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	type msg struct {
		id            string
		role          string
		input, output int64
		reasoning     int64
		read, write   int64
		cost          float64
		startsTurn    bool
	}
	rows, err := db.Query(`SELECT id, data FROM message WHERE session_id = ? ORDER BY time_created ASC, rowid ASC`, sessionID)
	if err != nil {
		t.Fatalf("query messages: %v", err)
	}
	var ids, datas []string
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			_ = rows.Close()
			t.Fatalf("scan message: %v", err)
		}
		ids = append(ids, id)
		datas = append(datas, data)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("rows: %v", err)
	}
	_ = rows.Close()
	var msgs []msg
	for k := range ids {
		id, data := ids[k], datas[k]
		var d struct {
			Role   string   `json:"role"`
			Cost   *float64 `json:"cost"`
			Tokens struct {
				Input     int64 `json:"input"`
				Output    int64 `json:"output"`
				Reasoning int64 `json:"reasoning"`
				Cache     struct {
					Read  int64 `json:"read"`
					Write int64 `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			_ = rows.Close()
			t.Fatalf("decode message %s: %v", id, err)
		}
		var nparts int
		if err := db.QueryRow(`SELECT COUNT(*) FROM part WHERE message_id = ?`, id).Scan(&nparts); err != nil {
			_ = rows.Close()
			t.Fatalf("count parts: %v", err)
		}
		m := msg{id: id, role: d.Role, input: d.Tokens.Input, output: d.Tokens.Output,
			reasoning: d.Tokens.Reasoning, read: d.Tokens.Cache.Read, write: d.Tokens.Cache.Write}
		if d.Cost != nil {
			m.cost = *d.Cost
		}
		// A turn opens at a user message carrying content. The source
		// never stores a tool_result row of its own — results are derived
		// twins on assistant messages, not rows — so any part counts as a
		// prompt part here. That is deliberately coarser than the
		// importer's hasPromptPart rule (which excludes tool_result-only
		// user lines): the two agree on every row this source can hold,
		// and the finer rule is pinned separately by
		// TestSplitTurnsExcludesToolResultOnly below rather than by
		// bending this oracle to a shape the database never takes.
		m.startsTurn = d.Role == "user" && nparts > 0
		msgs = append(msgs, m)
	}

	var turns []wantTurn
	var dollars []float64
	for i, m := range msgs {
		if i == 0 || m.startsTurn {
			turns = append(turns, wantTurn{})
			dollars = append(dollars, 0)
		}
		w := &turns[len(turns)-1]
		w.input += m.input
		w.output += m.output
		w.reasoning += m.reasoning
		w.read += m.read
		w.write += m.write
		dollars[len(dollars)-1] += m.cost
	}
	// Micros round per turn, not per message: each span's dollar sum is
	// rounded once, matching the importer's span-wide rounding rather than
	// a per-message rounding that would differ on half-micros.
	for i := range turns {
		turns[i].micros = int64(math.Round(dollars[i] * 1e6))
	}
	return turns
}

// TestParentAndWorktree pins the two links that make a session findable:
// the child naming its parent by binding, and the worktree coming from the
// project row rather than being guessed.
func TestParentAndWorktree(t *testing.T) {
	f := newFixture(t)
	refs := discover(t, f)

	parent, _, _ := f.read(t, refByID(t, refs, fixtureParent))
	if parent.Parent != nil {
		t.Fatalf("the parent session names a parent: %+v", parent.Parent)
	}
	if parent.Worktree.Path != f.worktree {
		t.Fatalf("worktree path = %q, want the project worktree %q", parent.Worktree.Path, f.worktree)
	}
	if parent.Worktree.VCS != vcsGit {
		t.Fatalf("worktree vcs = %q, want %q", parent.Worktree.VCS, vcsGit)
	}
	if parent.Worktree.Name != filepath.Base(f.worktree) {
		t.Fatalf("worktree name = %q", parent.Worktree.Name)
	}
	if parent.Session.Directory != f.worktree {
		t.Fatalf("session directory = %q, want %q", parent.Session.Directory, f.worktree)
	}
	if parent.Session.Title != "Parent session" {
		t.Fatalf("title = %q", parent.Session.Title)
	}
	// The model vendor is whoever ran the model; the binding key stays the
	// harness, whatever the session says.
	if parent.Session.Provider != "openai" || parent.Session.Harness != Harness {
		t.Fatalf("session = %+v, want provider openai harness opencode", parent.Session)
	}
	if parent.Binding.Provider != importer.ProviderOpencode {
		t.Fatalf("binding provider = %q, want %q", parent.Binding.Provider, importer.ProviderOpencode)
	}
	if want := "opencode run --session " + fixtureParent; parent.Binding.ResumeCmd != want {
		t.Fatalf("resume cmd = %q, want %q", parent.Binding.ResumeCmd, want)
	}
	if parent.Session.Model != "gpt-5.5" {
		t.Fatalf("model = %q, want the assistant message's model", parent.Session.Model)
	}
	for _, m := range parent.Messages {
		if m.Provider != "openai" && m.Role == store.RoleAssistant {
			t.Fatalf("assistant message %s has provider %q", m.ForeignID, m.Provider)
		}
	}

	child, _, _ := f.read(t, refByID(t, refs, fixtureChild))
	if child.Parent == nil {
		t.Fatal("child has no parent binding")
	}
	if child.Parent.Provider != importer.ProviderOpencode || child.Parent.ForeignSessionID != fixtureParent {
		t.Fatalf("parent binding = %+v, want the session %q", child.Parent, fixtureParent)
	}
	if child.Worktree.Path != f.worktree {
		t.Fatalf("child worktree path = %q, want %q", child.Worktree.Path, f.worktree)
	}
}

// TestReimportIdempotent is the dedup acceptance: a second read of an
// unchanged session returns the same foreign ids the store already holds,
// and touching one message moves only the watermark.
func TestReimportIdempotent(t *testing.T) {
	f := newFixture(t)
	ref := refByID(t, discover(t, f), fixtureParent)

	first, cur, _ := f.read(t, ref)
	if got := mustSQLiteCursor(t, cur); got.TimeUpdated != fixtureParentCursor {
		t.Fatalf("cursor = %d, want %d", got.TimeUpdated, fixtureParentCursor)
	}
	again, cur2, _, err := Source{}.Read(f.env, ref, cur)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got := mustSQLiteCursor(t, cur2); got.TimeUpdated != fixtureParentCursor {
		t.Fatalf("re-read cursor = %d, want %d", got.TimeUpdated, fixtureParentCursor)
	}
	if idsOf(first) != idsOf(again) {
		t.Fatalf("foreign ids moved across reads:\n%s\n%s", idsOf(first), idsOf(again))
	}

	// Touch one message: only the watermark may move.
	db, err := sql.Open(store.DriverName, "file:"+f.dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	const bumped = fixtureParentCursor + 5000
	if _, err := db.Exec(`UPDATE message SET time_updated = ? WHERE id = 'msg_a2'`, bumped); err != nil {
		_ = db.Close()
		t.Fatalf("bump message: %v", err)
	}
	_ = db.Close()

	third, cur3, _, err := Source{}.Read(f.env, ref, cur2)
	if err != nil {
		t.Fatalf("third read: %v", err)
	}
	if got := mustSQLiteCursor(t, cur3); got.TimeUpdated != bumped {
		t.Fatalf("cursor after touch = %d, want %d", got.TimeUpdated, bumped)
	}
	if idsOf(third) != idsOf(first) {
		t.Fatalf("foreign ids moved after touching a timestamp:\n%s\n%s", idsOf(third), idsOf(first))
	}
	if len(third.Messages) != len(first.Messages) {
		t.Fatalf("message count moved %d -> %d on a timestamp touch", len(first.Messages), len(third.Messages))
	}
}

// TestOversizedMessageSkipsRaw is the OOM regression: one live message
// row is 239MB of summary.diffs, which panics the wasm sqlite driver
// when SELECTed whole — and json_extract parses the blob too, so it OOMs
// the same way. The reader selects lengths first, then the full blob for
// small rows or a 2KB prefix for huge ones, string-scanning role/model/
// provider in Go and leaving RawJSON nil.
func TestOversizedMessageSkipsRaw(t *testing.T) {
	f := newFixture(t)

	big := strings.Repeat("x", 300*1024)
	payload, err := json.Marshal(map[string]any{
		"role":       "assistant",
		"modelID":    "gpt-5.5",
		"providerID": "openai",
		"tokens":     map[string]any{"input": 7, "output": 3, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}},
		"cost":       0.00042,
		"summary":    map[string]any{"diffs": big},
	})
	if err != nil {
		t.Fatalf("marshal big message: %v", err)
	}
	if len(payload) <= 200000 {
		t.Fatalf("fixture payload is %d bytes, want over the 200000 cap", len(payload))
	}

	db, err := sql.Open(store.DriverName, "file:"+f.dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_big', 'ses_parent', 1757000000500, 1757000000501, ?)`, string(payload)); err != nil {
		_ = db.Close()
		t.Fatalf("insert big message: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_big_text', 'msg_big', 'ses_parent', 1757000000500, 1757000000501, '{"type":"text","text":"big answer"}')`); err != nil {
		_ = db.Close()
		t.Fatalf("insert big part: %v", err)
	}
	_ = db.Close()

	th, _, res, err := Source{}.Read(f.env, refByID(t, discover(t, f), fixtureParent), nil)
	if err != nil {
		t.Fatalf("Read with oversized message: %v", err)
	}
	var found *store.Message
	for i := range th.Messages {
		if th.Messages[i].ForeignID == "msg_big" {
			found = &th.Messages[i]
			break
		}
	}
	if found == nil {
		t.Fatal("oversized message missing from import")
	}
	if found.RawJSON != nil {
		t.Fatalf("RawJSON is %d bytes, want nil for the oversized row", len(*found.RawJSON))
	}
	if found.Role != store.RoleAssistant || found.Model != "gpt-5.5" || found.Provider != "openai" {
		t.Fatalf("message fields = %+v, want assistant gpt-5.5 openai", found)
	}
	// Huge rows drop tokens/cost detail: the 300KB summary tail pushes
	// the tokens object past the 2KB prefix, and the prefix path keeps
	// identity+role with Usage empty rather than parsing the blob.
	// (Cost rides first in key order so its micros may still land; the
	// contract held here is identity kept, raw dropped, not skipped.)
	if found.Usage != "" {
		t.Fatalf("Usage = %q, want empty for the oversized row", found.Usage)
	}
	for _, s := range res.Skipped {
		if strings.Contains(s.Reason, "msg_big") {
			t.Fatalf("oversized message was skipped: %q", s.Reason)
		}
	}
}

// TestHugeUserMessageKeepsIdentity is the live-row shape: a user message
// carrying ~1MB of summary.diffs with no tokens or cost, like the 239MB
// ses_fd636a46effeETujw3XL8esxpu row that OOMed the wasm driver. The
// import must succeed with role/provider kept, RawJSON nil, Usage empty,
// zero cost, and no skip — substr never parses JSON so the tail is never
// materialised.
func TestHugeUserMessageKeepsIdentity(t *testing.T) {
	f := newFixture(t)

	big := strings.Repeat("d", 1024*1024)
	payload, err := json.Marshal(map[string]any{
		"role":    "user",
		"summary": map[string]any{"diffs": big},
	})
	if err != nil {
		t.Fatalf("marshal huge user message: %v", err)
	}
	if len(payload) <= 200000 {
		t.Fatalf("fixture payload is %d bytes, want over the 200000 cap", len(payload))
	}

	db, err := sql.Open(store.DriverName, "file:"+f.dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_huge_user', 'ses_child', 1757000001300, 1757000001301, ?)`, string(payload)); err != nil {
		_ = db.Close()
		t.Fatalf("insert huge message: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_huge_text', 'msg_huge_user', 'ses_child', 1757000001300, 1757000001301, '{"type":"text","text":"vendor bundle summary"}')`); err != nil {
		_ = db.Close()
		t.Fatalf("insert huge part: %v", err)
	}
	_ = db.Close()

	th, _, res, err := Source{}.Read(f.env, refByID(t, discover(t, f), fixtureChild), nil)
	if err != nil {
		t.Fatalf("Read with huge user message: %v", err)
	}
	var found *store.Message
	for i := range th.Messages {
		if th.Messages[i].ForeignID == "msg_huge_user" {
			found = &th.Messages[i]
			break
		}
	}
	if found == nil {
		t.Fatal("huge user message missing from import")
	}
	if found.Role != store.RoleUser {
		t.Fatalf("role = %q, want user", found.Role)
	}
	if found.RawJSON != nil {
		t.Fatalf("RawJSON is %d bytes, want nil for the huge row", len(*found.RawJSON))
	}
	if found.Usage != "" {
		t.Fatalf("Usage = %q, want empty for a user row with no tokens", found.Usage)
	}
	if found.Provider != Harness {
		t.Fatalf("provider = %q, want the harness fallback %q", found.Provider, Harness)
	}
	if len(found.Parts) != 1 {
		t.Fatalf("got %d parts, want the 1 text part", len(found.Parts))
	}
	for _, s := range res.Skipped {
		if strings.Contains(s.Reason, "msg_huge_user") {
			t.Fatalf("huge message was skipped: %q", s.Reason)
		}
	}
}

// openFixtureWriter opens the fixture database writable, closed at test
// end. Tests that add rows use fresh session ids so the golden sessions
// stay exactly as the checked-in SQL built them.
func openFixtureWriter(t *testing.T, f fixture) *sql.DB {
	t.Helper()
	db, err := sql.Open(store.DriverName, "file:"+f.dbFile)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func execFixture(t *testing.T, db *sql.DB, stmt string, args ...any) {
	t.Helper()
	if _, err := db.Exec(stmt, args...); err != nil {
		t.Fatalf("exec %s: %v", trunc(stmt), err)
	}
}

// TestSplitTurnsExcludesToolResultOnly pins the finer half of the turn
// rule the database oracle cannot see: a user line carrying nothing but
// tool results is the protocol talking, not a prompt, and opens no turn.
// Opencode never stores such a row (its results ride on assistant
// messages), so the fixture cannot hold the shape and this unit test
// stands in for it.
func TestSplitTurnsExcludesToolResultOnly(t *testing.T) {
	cands := []importer.TurnCandidate{
		{Role: store.RoleUser, PartTypes: []string{importer.PartText}},
		{Role: store.RoleAssistant, PartTypes: []string{importer.PartToolCall}},
		{Role: store.RoleUser, PartTypes: []string{importer.PartToolResult}},
	}
	spans := importer.SplitTurns(cands)
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1: %+v", len(spans), spans)
	}
	if spans[0] != (importer.TurnSpan{Start: 0, End: 3}) {
		t.Fatalf("span = %+v, want the whole session in one turn", spans[0])
	}
}

// TestAnchoredPrefixExtraction holds the prefix scanners against nested
// namesakes: the first `"role"` in the raw bytes is not necessarily the
// message's role, and an unanchored scan would file the row under prose.
func TestAnchoredPrefixExtraction(t *testing.T) {
	hay := `{"nested":{"role":"user","input":99},"role":"assistant","modelID":"m"}`
	if got, ok := extractTopJSONString(hay, "role"); !ok || got != "assistant" {
		t.Fatalf("role = %q,%v, want assistant", got, ok)
	}
	hay2 := `{"role":"assistant","tokens":{"input":5},"cost":0.5}`
	tok, ok := extractTopObject(hay2, "tokens")
	if !ok {
		t.Fatal("tokens object not found")
	}
	if got, ok := extractJSONInt(tok, "input"); !ok || got != 5 {
		t.Fatalf("tokens input = %d,%v, want 5", got, ok)
	}
	if _, ok := extractTopJSONNumber(hay2, "input"); ok {
		t.Fatal("top-level input matched from inside the tokens object")
	}
	if got, ok := extractTopJSONFloat(hay2, "cost"); !ok || got != 0.5 {
		t.Fatalf("cost = %v,%v, want 0.5", got, ok)
	}
}

// TestReadPinsRefPath refuses a ref naming anything but the known
// database: the path is a hint, never an arbitrary file to open.
func TestReadPinsRefPath(t *testing.T) {
	f := newFixture(t)
	held := importer.SQLiteCursor{TimeUpdated: 123}
	ref := importer.Ref{Provider: importer.ProviderOpencode, ID: fixtureParent, Path: "/etc/passwd"}
	_, back, _, err := Source{}.Read(f.env, ref, held)
	if err == nil {
		t.Fatal("Read opened an arbitrary ref path")
	}
	if back != importer.Cursor(held) {
		t.Fatalf("cursor back = %+v, want the one held %+v", back, held)
	}
}

// TestOpenReadOnlyEncodesSpecialChars proves the DSN cannot be escaped:
// ?#& in a directory stay in the path component and mode=ro survives as
// the query.
func TestOpenReadOnlyEncodesSpecialChars(t *testing.T) {
	raw := filepath.Join(t.TempDir(), "a?b#c&d", "opencode.db")
	u := url.URL{Scheme: "file", Path: raw, RawQuery: "mode=ro"}
	back, err := url.Parse(u.String())
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	if back.Query().Get("mode") != "ro" {
		t.Fatalf("DSN %q lost mode=ro", u.String())
	}
	if back.Path != raw {
		t.Fatalf("path round-trip = %q, want %q", back.Path, raw)
	}
}

// TestPartCappedKeepsRouting drives the oversized-part path: a part row
// past the raw cap keeps its type and twin behaviour off a synthesized
// payload instead of failing the message.
func TestPartCappedKeepsRouting(t *testing.T) {
	f := newFixture(t)
	db := openFixtureWriter(t, f)
	execFixture(t, db, `INSERT INTO session (id, project_id, parent_id, directory, title, model, time_created, time_updated) VALUES ('ses_cap', 'prj_1', NULL, ?, 'Cap session', NULL, 1757000002000, 1757000002001)`, f.worktree)
	execFixture(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_cap', 'ses_cap', 1757000002000, 1757000002001, '{"role":"user"}')`)
	big := strings.Repeat("y", partRawLimit+100)
	execFixture(t, db, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_cap_big', 'msg_cap', 'ses_cap', 1757000002000, 1757000002001, ?)`,
		`{"type":"text","text":"`+big+`"}`)

	th, _, res, err := Source{}.Read(f.env, importer.Ref{Provider: importer.ProviderOpencode, ID: "ses_cap", Path: dbRel}, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(th.Messages) != 1 || len(th.Messages[0].Parts) != 1 {
		t.Fatalf("got %d messages/%d parts, want 1/1", len(th.Messages), len(th.Messages[0].Parts))
	}
	p := th.Messages[0].Parts[0]
	if p.Type != importer.PartText || p.ForeignID != "prt_cap_big" {
		t.Fatalf("part = %+v, want the capped text part", p)
	}
	for _, s := range res.Skipped {
		if strings.Contains(s.Reason, "msg_cap") {
			t.Fatalf("capped message was skipped: %q", s.Reason)
		}
	}
}

// TestUnknownPartTypeCounted holds the unknown path: a part type this
// build has never met lands as `unknown` and is counted, not dropped.
func TestUnknownPartTypeCounted(t *testing.T) {
	f := newFixture(t)
	db := openFixtureWriter(t, f)
	execFixture(t, db, `INSERT INTO session (id, project_id, parent_id, directory, title, model, time_created, time_updated) VALUES ('ses_unk', 'prj_1', NULL, ?, 'Unk session', NULL, 1757000002100, 1757000002101)`, f.worktree)
	execFixture(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_unk', 'ses_unk', 1757000002100, 1757000002101, '{"role":"user"}')`)
	execFixture(t, db, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_unk', 'msg_unk', 'ses_unk', 1757000002100, 1757000002101, '{"type":"future-widget","frobnicate":true}')`)

	th, _, res, err := Source{}.Read(f.env, importer.Ref{Provider: importer.ProviderOpencode, ID: "ses_unk", Path: dbRel}, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.Unknown != 1 || res.UnknownTypes["future-widget"] != 1 {
		t.Fatalf("Unknown = %d %v, want 1 future-widget", res.Unknown, res.UnknownTypes)
	}
	if got := th.Messages[0].Parts[0].Type; got != importer.PartUnknown {
		t.Fatalf("part type = %q, want unknown", got)
	}
}

// TestSessionModelFallback pins the session-level backstop: when no
// assistant message names a model, the session row's model JSON answers.
func TestSessionModelFallback(t *testing.T) {
	f := newFixture(t)
	db := openFixtureWriter(t, f)
	execFixture(t, db, `INSERT INTO session (id, project_id, parent_id, directory, title, model, time_created, time_updated) VALUES ('ses_fb', 'prj_1', NULL, ?, 'Fallback session', '{"id":"fallback-model","providerID":"fallback-provider"}', 1757000002200, 1757000002201)`, f.worktree)
	execFixture(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_fb_u', 'ses_fb', 1757000002200, 1757000002201, '{"role":"user"}')`)
	execFixture(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_fb_a', 'ses_fb', 1757000002202, 1757000002203, '{"role":"assistant"}')`)
	execFixture(t, db, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_fb_u', 'msg_fb_u', 'ses_fb', 1757000002200, 1757000002201, '{"type":"text","text":"hi"}')`)
	execFixture(t, db, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_fb_a', 'msg_fb_a', 'ses_fb', 1757000002202, 1757000002203, '{"type":"text","text":"hello"}')`)

	th, _, _, err := Source{}.Read(f.env, importer.Ref{Provider: importer.ProviderOpencode, ID: "ses_fb", Path: dbRel}, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if th.Session.Model != "fallback-model" || th.Session.Provider != "fallback-provider" {
		t.Fatalf("session = %+v, want the session row's model backstop", th.Session)
	}
	// And a span with no cost anywhere keeps a nil dollar cost rather
	// than a guessed zero.
	for i, turn := range th.Turns {
		if turn.CostUSDMicros != nil {
			t.Fatalf("turn %d micros = %d, want nil with no cost on any message", i, *turn.CostUSDMicros)
		}
	}
}

// TestMissingProjectFallsBackToDirectory pins the deleted-project path:
// a session whose project row is gone reads off its own directory with
// vcs none rather than failing.
func TestMissingProjectFallsBackToDirectory(t *testing.T) {
	f := newFixture(t)
	db := openFixtureWriter(t, f)
	execFixture(t, db, `INSERT INTO session (id, project_id, parent_id, directory, title, model, time_created, time_updated) VALUES ('ses_noproj', 'prj_gone', NULL, ?, 'Orphan session', NULL, 1757000002300, 1757000002301)`, f.worktree)
	execFixture(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_np', 'ses_noproj', 1757000002300, 1757000002301, '{"role":"user"}')`)
	execFixture(t, db, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_np', 'msg_np', 'ses_noproj', 1757000002300, 1757000002301, '{"type":"text","text":"hi"}')`)

	th, _, _, err := Source{}.Read(f.env, importer.Ref{Provider: importer.ProviderOpencode, ID: "ses_noproj", Path: dbRel}, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if th.Worktree.Path != f.worktree || th.Worktree.VCS != vcsNone {
		t.Fatalf("worktree = %+v, want the session directory with vcs none", th.Worktree)
	}
}

// TestWatermarkFoldsPartsAndHoldsSkips pins both halves of the watermark
// rule: a part edited after its message still moves it, and a skipped row
// never does.
func TestWatermarkFoldsPartsAndHoldsSkips(t *testing.T) {
	f := newFixture(t)
	db := openFixtureWriter(t, f)
	execFixture(t, db, `INSERT INTO session (id, project_id, parent_id, directory, title, model, time_created, time_updated) VALUES ('ses_wm', 'prj_1', NULL, ?, 'Watermark session', NULL, 1757000002400, 1757000002401)`, f.worktree)
	execFixture(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_wm1', 'ses_wm', 1757000002400, 1757000002401, '{"role":"user"}')`)
	// The part is newer than its message: the watermark must follow the
	// part, not the message row.
	execFixture(t, db, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_wm1', 'msg_wm1', 'ses_wm', 1757000002400, 1757000011401, '{"type":"text","text":"hi"}')`)
	// A newer row this build cannot use: skipped, so the watermark must
	// hold below it rather than sailing past.
	execFixture(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_wm2', 'ses_wm', 1757000002500, 1757000020000, '{"role":"bogus"}')`)
	execFixture(t, db, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_wm2', 'msg_wm2', 'ses_wm', 1757000002500, 1757000020000, '{"type":"text","text":"unusable"}')`)

	th, cur, res, err := Source{}.Read(f.env, importer.Ref{Provider: importer.ProviderOpencode, ID: "ses_wm", Path: dbRel}, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := mustSQLiteCursor(t, cur); got.TimeUpdated != 1757000011401 {
		t.Fatalf("cursor = %d, want the part timestamp 1757000011401", got.TimeUpdated)
	}
	if len(th.Messages) != 1 {
		t.Fatalf("got %d messages, want 1 with the bogus row skipped", len(th.Messages))
	}
	if res.SkippedCount != 1 {
		t.Fatalf("SkippedCount = %d, want 1", res.SkippedCount)
	}
}

// TestToolUnknownStatusClassified holds the compromise on tool statuses:
// a status outside completed/error twins nothing, per the contract, but
// is classified under its own name so the row stays visible.
func TestToolUnknownStatusClassified(t *testing.T) {
	f := newFixture(t)
	db := openFixtureWriter(t, f)
	execFixture(t, db, `INSERT INTO session (id, project_id, parent_id, directory, title, model, time_created, time_updated) VALUES ('ses_ts', 'prj_1', NULL, ?, 'Tool status session', NULL, 1757000002600, 1757000002601)`, f.worktree)
	execFixture(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_ts', 'ses_ts', 1757000002600, 1757000002601, '{"role":"assistant"}')`)
	execFixture(t, db, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_ts', 'msg_ts', 'ses_ts', 1757000002600, 1757000002601, '{"type":"tool","tool":"bash","callID":"call_x","state":{"status":"timeout","input":{},"output":"late"}}')`)

	th, _, res, err := Source{}.Read(f.env, importer.Ref{Provider: importer.ProviderOpencode, ID: "ses_ts", Path: dbRel}, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var calls, results int
	for _, m := range th.Messages {
		for _, p := range m.Parts {
			if p.Type == importer.PartToolCall {
				calls++
			}
			if p.Type == importer.PartToolResult {
				results++
			}
		}
	}
	if calls != 1 || results != 0 {
		t.Fatalf("got %d calls and %d results, want the lone call with no twin", calls, results)
	}
	if res.Classified["tool:timeout"] != 1 {
		t.Fatalf("Classified = %v, want the timeout status counted", res.Classified)
	}
}

func TestResumeCmdRejectsUnsafeID(t *testing.T) {
	if got := resumeCmd("ses_parent"); got != "opencode run --session ses_parent" {
		t.Fatalf("resumeCmd = %q, want the session command", got)
	}
	// The id rides unquoted, so anything outside [A-Za-z0-9_-] omits
	// it rather than risking injection.
	for _, bad := range []string{"ses x", `ses"x`, "ses;rm -rf", "ses$(id)", "ses|less", "../ses"} {
		if got := resumeCmd(bad); got != "opencode run" {
			t.Fatalf("resumeCmd(%q) = %q, want the bare command", bad, got)
		}
	}
	if got := resumeCmd(""); got != "" {
		t.Fatalf("resumeCmd(\"\") = %q, want empty", got)
	}
	// Dashes join words rather than start flags here: the id is always
	// the value after --session, never parsed as flags itself.
	if got := resumeCmd("ses-abc_123"); got != "opencode run --session ses-abc_123" {
		t.Fatalf("resumeCmd = %q, want the session command", got)
	}
}

func TestResolveWorktreeRejectsRelative(t *testing.T) {
	if got, vcs := resolveWorktree("relative/path", "git", "/abs/dir"); got != "/abs/dir" || vcs != vcsNone {
		t.Fatalf("relative worktree = %q,%q, want the directory fallback with vcs none", got, vcs)
	}
	if got, vcs := resolveWorktree("relative/path", "git", ""); got != "" || vcs != vcsNone {
		t.Fatalf("relative worktree with no directory = %q,%q, want empty none", got, vcs)
	}
	if got, vcs := resolveWorktree("/abs/wt", "git", "/abs/dir"); got != "/abs/wt" || vcs != vcsGit {
		t.Fatalf("absolute worktree = %q,%q, want it passed through", got, vcs)
	}
}

func TestDiscoverGarbageDatabaseIsEmptyMachine(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// A file that is not a database where the database should be: the
	// machine reads as empty, not as a failed import.
	if err := os.WriteFile(filepath.Join(dir, "opencode.db"), []byte("replaced-by-garbage"), 0o600); err != nil {
		t.Fatalf("WriteFile garbage: %v", err)
	}
	env := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	refs, err := Source{}.Discover(env)
	if err != nil {
		t.Fatalf("Discover of garbage db: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("Discover = %+v, want nothing for a garbage database", refs)
	}
}

// TestPartCappedErrorStatusTwins drives the capped-part status through the
// state object: status nests at depth 2, so a top-level scan never matches
// it, and an error tool that lost its twin would read as still running.
func TestPartCappedErrorStatusTwins(t *testing.T) {
	f := newFixture(t)
	db := openFixtureWriter(t, f)
	execFixture(t, db, `INSERT INTO session (id, project_id, parent_id, directory, title, model, time_created, time_updated) VALUES ('ses_caperr', 'prj_1', NULL, ?, 'Cap error session', NULL, 1757000002700, 1757000002701)`, f.worktree)
	execFixture(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_caperr', 'ses_caperr', 1757000002700, 1757000002701, '{"role":"assistant"}')`)
	pad := strings.Repeat("z", partRawLimit+100)
	// Hand-built so the discriminator and state ride first: a Go map
	// would marshal keys alphabetically and bury "type" past the prefix.
	data := `{"type":"tool","tool":"bash","callID":"call_big_err","state":{"status":"error","input":{"cmd":"x"},"output":"boom"},"pad":"` + pad + `"}`
	execFixture(t, db, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_caperr', 'msg_caperr', 'ses_caperr', 1757000002700, 1757000002701, ?)`, data)

	th, _, _, err := Source{}.Read(f.env, importer.Ref{Provider: importer.ProviderOpencode, ID: "ses_caperr", Path: dbRel}, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(th.Messages) != 1 || len(th.Messages[0].Parts) != 2 {
		t.Fatalf("got %d messages with %d parts, want 1 message with the call+result twin",
			len(th.Messages), len(th.Messages[0].Parts))
	}
	var result *store.Part
	for i := range th.Messages[0].Parts {
		if th.Messages[0].Parts[i].Type == importer.PartToolResult {
			result = &th.Messages[0].Parts[i]
		}
	}
	if result == nil {
		t.Fatal("capped error tool has no tool_result twin")
	}
	var tr struct {
		IsError bool `json:"is_error"`
	}
	if err := json.Unmarshal([]byte(result.Data), &tr); err != nil {
		t.Fatalf("decode tool result data: %v", err)
	}
	if !tr.IsError {
		t.Fatalf("tool_result data = %s, want is_error=true", result.Data)
	}
}

// TestBadPartSkippedMessageKept holds the part-error rule: one unusable
// part row is a skip with a reason, not a failed message — the good parts
// on the same message still land.
func TestBadPartSkippedMessageKept(t *testing.T) {
	f := newFixture(t)
	db := openFixtureWriter(t, f)
	execFixture(t, db, `INSERT INTO session (id, project_id, parent_id, directory, title, model, time_created, time_updated) VALUES ('ses_badpart', 'prj_1', NULL, ?, 'Bad part session', NULL, 1757000002800, 1757000002801)`, f.worktree)
	execFixture(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES ('msg_bp', 'ses_badpart', 1757000002800, 1757000002801, '{"role":"user"}')`)
	execFixture(t, db, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_bp_good', 'msg_bp', 'ses_badpart', 1757000002800, 1757000002801, '{"type":"text","text":"kept"}')`)
	// Oversized with no type in its prefix: partByID cannot route it.
	big := strings.Repeat("q", partRawLimit+100)
	execFixture(t, db, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES ('prt_bp_bad', 'msg_bp', 'ses_badpart', 1757000002800, 1757000002801, ?)`,
		`{"blob":"`+big+`"}`)

	th, _, res, err := Source{}.Read(f.env, importer.Ref{Provider: importer.ProviderOpencode, ID: "ses_badpart", Path: dbRel}, nil)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(th.Messages) != 1 {
		t.Fatalf("got %d messages, want 1 with the bad part skipped", len(th.Messages))
	}
	if len(th.Messages[0].Parts) != 1 || th.Messages[0].Parts[0].ForeignID != "prt_bp_good" {
		t.Fatalf("parts = %+v, want only the good text part", th.Messages[0].Parts)
	}
	if res.SkippedCount != 1 {
		t.Fatalf("SkippedCount = %d, want 1 for the unroutable part", res.SkippedCount)
	}
}

func idsOf(th store.Thread) string {
	var ids []string
	for _, m := range th.Messages {
		ids = append(ids, m.ForeignID)
		for _, p := range m.Parts {
			ids = append(ids, m.ForeignID+"/"+p.ForeignID+":"+p.Type)
		}
	}
	return strings.Join(ids, "\n")
}

func mustSQLiteCursor(t *testing.T, c importer.Cursor) importer.SQLiteCursor {
	t.Helper()
	typed, ok := c.(importer.SQLiteCursor)
	if !ok {
		t.Fatalf("cursor = %T, want SQLiteCursor", c)
	}
	return typed
}

func TestReadRefusesACursorOfTheWrongKind(t *testing.T) {
	f := newFixture(t)
	ref := refByID(t, discover(t, f), fixtureParent)

	given := importer.JSONLCursor{Offset: 7, Size: 70}
	_, back, _, err := Source{}.Read(f.env, ref, given)
	if err == nil {
		t.Fatal("Read accepted a JSONL cursor")
	}
	if !errors.Is(err, importer.ErrCursorType) {
		t.Fatalf("Read err = %v, want ErrCursorType", err)
	}
	// The cursor comes back untouched: nothing was read, so there is no
	// watermark of this source's own to report.
	if back != importer.Cursor(given) {
		t.Fatalf("cursor back = %+v, want the one given", back)
	}
}

// TestReadKeepsTheCursorWhenTheSessionIsGone: a session deleted between
// Discover and Read has not invalidated the watermark, and handing back a
// zero one would make the next read re-import the whole session.
func TestReadKeepsTheCursorWhenTheSessionIsGone(t *testing.T) {
	f := newFixture(t)
	held := importer.SQLiteCursor{TimeUpdated: 1757000000405}
	missing := importer.Ref{Provider: importer.ProviderOpencode, ID: "ses_gone", Path: dbRel}

	_, back, _, err := Source{}.Read(f.env, missing, held)
	if err == nil {
		t.Fatal("Read of a missing session succeeded")
	}
	if back != importer.Cursor(held) {
		t.Fatalf("cursor back = %+v, want the one held %+v", back, held)
	}
}

// canonicalThread is the golden's shape. It exists rather than marshalling
// store.Thread directly because store's structs have no json tags, so their
// encoding would be Go field names and every rename would rewrite the
// golden — and because the Result and the cursor belong in the golden too:
// the watermark is as much of the contract as the rows are.
type canonicalThread struct {
	Ref      canonicalRef      `json:"ref"`
	Worktree store.Worktree    `json:"worktree"`
	Session  canonicalSession  `json:"session"`
	Binding  canonicalBinding  `json:"binding"`
	Parent   *canonicalBinding `json:"parent"`
	Turns    []canonicalTurn   `json:"turns"`
	Messages []canonicalMsg    `json:"messages"`
	Cursor   int64             `json:"cursor"`
	Result   canonicalResult   `json:"result"`
}

type canonicalRef struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Path     string `json:"path"`
	Key      string `json:"key"`
}

type canonicalSession struct {
	Directory string `json:"directory"`
	Title     string `json:"title"`
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Harness   string `json:"harness"`
}

type canonicalBinding struct {
	Provider         string `json:"provider"`
	Harness          string `json:"harness"`
	ForeignSessionID string `json:"foreign_session_id"`
	ResumeCmd        string `json:"resume_cmd"`
}

type canonicalTurn struct {
	Status         string `json:"status"`
	CostInput      int64  `json:"cost_input"`
	CostOutput     int64  `json:"cost_output"`
	CostTotal      int64  `json:"cost_total"`
	CostCacheRead  int64  `json:"cost_cache_read"`
	CostCacheWrite int64  `json:"cost_cache_write"`
	CostReasoning  int64  `json:"cost_reasoning"`
	CostUSDMicros  *int64 `json:"cost_usd_micros"`
}

type canonicalMsg struct {
	Role      string          `json:"role"`
	Provider  string          `json:"provider"`
	Model     string          `json:"model"`
	ForeignID string          `json:"foreign_id"`
	Usage     string          `json:"usage"`
	RawJSON   string          `json:"raw_json"`
	Parts     []canonicalPart `json:"parts"`
}

type canonicalPart struct {
	Type       string `json:"type"`
	ToolCallID string `json:"tool_call_id"`
	Signature  string `json:"signature"`
	ForeignID  string `json:"foreign_id"`
	Data       string `json:"data"`
}

type canonicalResult struct {
	Lines        int            `json:"lines"`
	Unknown      int            `json:"unknown"`
	UnknownTypes map[string]int `json:"unknown_types"`
	Classified   map[string]int `json:"classified"`
	SkippedCount int            `json:"skipped_count"`
	SkippedLines []int          `json:"skipped_lines"`
}

// canonicalise renders one import for the golden, with machine-specific
// paths turned back into placeholders.
func canonicalise(f fixture, ref importer.Ref, th store.Thread, cur importer.Cursor, res importer.Result) canonicalThread {
	out := canonicalThread{
		Ref: canonicalRef{Provider: ref.Provider, ID: ref.ID, Path: ref.Path, Key: ref.Key()},
		Worktree: store.Worktree{
			Path: f.unresolve(th.Worktree.Path),
			VCS:  th.Worktree.VCS,
			Name: th.Worktree.Name,
		},
		Session: canonicalSession{
			Directory: f.unresolve(th.Session.Directory),
			Title:     th.Session.Title,
			Model:     th.Session.Model,
			Provider:  th.Session.Provider,
			Harness:   th.Session.Harness,
		},
		Binding: canonicalBinding{
			Provider:         th.Binding.Provider,
			Harness:          th.Binding.Harness,
			ForeignSessionID: th.Binding.ForeignSessionID,
			ResumeCmd:        th.Binding.ResumeCmd,
		},
		Turns:    []canonicalTurn{},
		Messages: []canonicalMsg{},
		Result: canonicalResult{
			Lines:        res.Lines,
			Unknown:      res.Unknown,
			UnknownTypes: res.UnknownTypes,
			Classified:   res.Classified,
			SkippedCount: res.SkippedCount,
			SkippedLines: []int{},
		},
	}
	if cur != nil {
		if sc, ok := cur.(importer.SQLiteCursor); ok {
			out.Cursor = sc.TimeUpdated
		}
	}
	if th.Parent != nil {
		out.Parent = &canonicalBinding{
			Provider:         th.Parent.Provider,
			Harness:          th.Parent.Harness,
			ForeignSessionID: th.Parent.ForeignSessionID,
			ResumeCmd:        th.Parent.ResumeCmd,
		}
	}
	for _, t := range th.Turns {
		out.Turns = append(out.Turns, canonicalTurn{
			Status: t.Status, CostInput: t.CostInput, CostOutput: t.CostOutput,
			CostTotal: t.CostTotal, CostCacheRead: t.CostCacheRead,
			CostCacheWrite: t.CostCacheWrite, CostReasoning: t.CostReasoning,
			CostUSDMicros: t.CostUSDMicros,
		})
	}
	for _, m := range th.Messages {
		raw := ""
		if m.RawJSON != nil {
			raw = f.unresolve(*m.RawJSON)
		}
		cm := canonicalMsg{
			Role: string(m.Role), Provider: m.Provider, Model: m.Model,
			ForeignID: m.ForeignID, Usage: m.Usage, RawJSON: raw,
			Parts: []canonicalPart{},
		}
		for _, p := range m.Parts {
			cm.Parts = append(cm.Parts, canonicalPart{
				Type: p.Type, ToolCallID: p.ToolCallID,
				Signature: p.Signature, ForeignID: p.ForeignID,
				Data: f.unresolve(p.Data),
			})
		}
		out.Messages = append(out.Messages, cm)
	}
	for _, s := range res.Skipped {
		out.Result.SkippedLines = append(out.Result.SkippedLines, s.Line)
	}
	sort.Ints(out.Result.SkippedLines)
	return out
}
