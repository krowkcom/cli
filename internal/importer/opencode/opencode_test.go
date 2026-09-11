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
// wrote would lock against the live agent.
func TestReadOnlyNoWal(t *testing.T) {
	f := newFixture(t)
	before := hashFile(t, f.dbFile)

	refs := discover(t, f)
	for _, ref := range refs {
		f.read(t, ref)
	}

	if got := hashFile(t, f.dbFile); got != before {
		t.Fatalf("database changed by import: %s -> %s", before, got)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(f.dbFile + suffix); !os.IsNotExist(err) {
			t.Fatalf("import created sidecar %s", f.dbFile+suffix)
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
		if sum := turn.CostInput + turn.CostOutput + turn.CostReasoning + turn.CostCacheRead + turn.CostCacheWrite; turn.CostTotal != sum {
			t.Fatalf("turn %d total %d != %d, the sum of its classes", i, turn.CostTotal, sum)
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
		// never stores a tool_result type of its own, so any part counts
		// as a prompt part here.
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
