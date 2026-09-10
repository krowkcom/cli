package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/krowkcom/cli/internal/harness"
	"github.com/krowkcom/cli/internal/store"
)

// fakeSource is a Source built out of nothing but this package's own parts,
// which is the point: if the contract is usable, a source is a Discover that
// finds files under home and a Read that is ReadJSONL plus NormalizePart
// plus the turn rule, and the thread it produces goes into the store
// unchanged.
type fakeSource struct{}

func (fakeSource) Name() string { return ProviderClaude }

// fakeDir is where this source keeps its transcripts, relative to home.
const fakeDir = ".fake/projects"

func (s fakeSource) Discover(env harness.Env) ([]Ref, error) {
	if err := CheckOS(); err != nil {
		return nil, err
	}
	dir, err := HomePath(env, fakeDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		refs = append(refs, Ref{
			Provider: s.Name(),
			ID:       e.Name()[:len(e.Name())-len(".jsonl")],
			Path:     filepath.Join(fakeDir, e.Name()),
		})
	}
	return refs, nil
}

// fakeLine is the shape this source's transcripts are written in: enough of
// Claude's JSONL to exercise the contract — a line type, an idempotency key,
// a role, the meta and attachment flags the turn rule reads, and typed
// content blocks.
type fakeLine struct {
	Type       string `json:"type"`
	UUID       string `json:"uuid"`
	Role       string `json:"role"`
	IsMeta     bool   `json:"isMeta"`
	Attachment bool   `json:"attachment"`
	Content    []struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	} `json:"content"`
}

func (s fakeSource) Read(env harness.Env, ref Ref, cursor Cursor) (store.Thread, Cursor, Result, error) {
	th := store.Thread{
		Worktree: store.Worktree{Path: "/repo", VCS: "git", Name: "repo"},
		Session:  store.Session{Directory: "/repo", Title: ref.ID, Provider: s.Name(), Harness: s.Name()},
		Binding:  store.Binding{Provider: s.Name(), Harness: s.Name(), ForeignSessionID: ref.ID},
	}
	// A cursor of the wrong kind is refused rather than treated as a fresh
	// read: a silent full rescan would turn a caller's bug into a
	// performance mystery. A nil cursor is not a mismatch — it is how a
	// caller says "from the start".
	jc := JSONLCursor{}
	if cursor != nil {
		typed, ok := cursor.(JSONLCursor)
		if !ok {
			// The cursor comes back untouched: nothing was read, so this
			// source has no watermark of its own to report, and handing
			// back a zero one would look like "start again".
			return th, cursor, Result{}, fmt.Errorf("%s: %w, got %T", s.Name(), ErrCursorType, cursor)
		}
		jc = typed
	}

	f, err := OpenHome(env, ref.Path, 0)
	if err != nil {
		return th, jc, Result{}, err
	}
	defer f.Close()

	// The accounting the closure does (classified lines, unknown parts) and
	// the accounting ReadJSONL does (lines, skips) are merged at the end.
	var acc Result
	var candidates []TurnCandidate
	next, res, err := ReadJSONL(f, jc, func(_ int, line []byte) error {
		var l fakeLine
		if err := json.Unmarshal(line, &l); err != nil {
			// Returned plainly: a line in a shape this source cannot read
			// is a skip, and ReadJSONL counts it. Only a failure that is
			// not the line's fault would wrap ErrAbortFile.
			return err
		}
		switch l.Type {
		case "user", "assistant":
		default:
			// Transcript furniture: understood, counted, not imported.
			// Classified rather than skipped — Result.Skipped is for lines
			// that could not be used, and these were used, by being
			// recognised and declined.
			acc.Classify(l.Type)
			return nil
		}
		msg := store.Message{
			Role:      store.Role(l.Role),
			Provider:  s.Name(),
			ForeignID: l.UUID,
		}
		cand := TurnCandidate{Role: msg.Role, Meta: l.IsMeta, Attachment: l.Attachment}
		for _, c := range l.Content {
			part := acc.NormalizePart(c.Type, c.Data)
			msg.Parts = append(msg.Parts, part)
			cand.PartTypes = append(cand.PartTypes, part.Type)
		}
		th.Messages = append(th.Messages, msg)
		candidates = append(candidates, cand)
		return nil
	})
	acc.Merge(res)
	if err != nil {
		return th, next, acc, err
	}
	for range SplitTurns(candidates) {
		th.Turns = append(th.Turns, store.Turn{Status: "done"})
	}
	return th, next, acc, nil
}

// The fake really is a Source, checked at compile time.
var _ Source = fakeSource{}

func writeFakeTranscript(t *testing.T, home, name, content string) {
	t.Helper()
	dir := filepath.Join(home, fakeDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// The transcript below is the shape the turn rule exists for: a hook line, a
// real prompt, a tool call, a tool_result-only user line, a reply, a
// furniture line, an unknown content block and a corrupt line.
const fakeTranscript = `{"type":"mode","mode":"default"}
{"type":"user","uuid":"u0","role":"user","isMeta":true,"content":[{"type":"text","data":{"text":"hook ran"}}]}
{"type":"user","uuid":"u1","role":"user","content":[{"type":"text","data":{"text":"what is this"}}]}
{"type":"assistant","uuid":"a1","role":"assistant","content":[{"type":"tool_call","data":{"name":"Read","input":{}}}]}
{"type":"user","uuid":"u2","role":"user","content":[{"type":"tool_result","data":{"output":"ok","is_error":false}}]}
{"type":"assistant","uuid":"a2","role":"assistant","content":[{"type":"text","data":{"text":"it is a repo"}},{"type":"server_tool_use","data":{"x":1}}]}
{"type":"summary","summary":"a chat"}
this line is not json
{"type":"user","uuid":"u3","role":"user","content":[{"type":"text","data":{"text":"thanks"}}]}
`

// The contract end to end: Discover finds the file, Read turns it into a
// store.Thread through this package's own helpers, and the store ingests it
// — twice, with the second ingest inserting nothing.
func TestFakeSourceRoundTripsThroughTheStore(t *testing.T) {
	home := t.TempDir()
	writeFakeTranscript(t, home, "sess-1.jsonl", fakeTranscript)
	env := homeEnv(home)

	var src fakeSource
	refs, err := src.Discover(env)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(refs) != 1 || refs[0].Key() != "claude:sess-1" {
		t.Fatalf("Discover = %+v", refs)
	}

	th, cur, res, err := src.Read(env, refs[0], JSONLCursor{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// Five importable messages: the hook line, the prompt, the tool call,
	// the tool result and the reply, plus the closing prompt.
	if len(th.Messages) != 6 {
		t.Fatalf("got %d messages, want 6", len(th.Messages))
	}
	// Three turns: the hook preamble, the real prompt with its whole tool
	// loop, and the closing prompt.
	if len(th.Turns) != 3 {
		t.Fatalf("got %d turns, want 3", len(th.Turns))
	}
	if res.Classified["mode"] != 1 || res.Classified["summary"] != 1 {
		t.Fatalf("Classified = %v, want the mode and summary lines counted", res.Classified)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Reason == "" {
		// Only the corrupt line: the mode and summary lines were
		// recognised and declined, which is Classified, not Skipped.
		t.Fatalf("Skipped = %+v, want just the corrupt line", res.Skipped)
	}
	if res.Unknown != 1 || res.UnknownTypes["server_tool_use"] != 1 {
		t.Fatalf("Unknown = %d %v, want the one server_tool_use block", res.Unknown, res.UnknownTypes)
	}
	// Every part emitted is in the fixed set — the acceptance bullet, held
	// against a real source rather than against NormalizePart alone.
	for _, m := range th.Messages {
		for _, p := range m.Parts {
			if !KnownPartType(p.Type) {
				t.Fatalf("message %s emitted part type %q", m.ForeignID, p.Type)
			}
		}
	}
	// The cursor is a byte offset at the size the file had.
	info, err := os.Stat(filepath.Join(home, fakeDir, "sess-1.jsonl"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	jc, ok := cur.(JSONLCursor)
	if !ok || jc.Offset != info.Size() || jc.Size != info.Size() {
		t.Fatalf("cursor = %+v, want offset and size %d", cur, info.Size())
	}
	// And it serialises into import_state.cursor as JSON.
	if _, err := jc.Encode(); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// The store gets its own home: this test is about the contract feeding
	// the writer, not about where either of them lives.
	storeHome := t.TempDir()
	db, err := store.Open(func(k string) string {
		if k == "HOME" {
			return storeHome
		}
		return ""
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	w := store.NewWriter(db, nil)

	first, err := w.Ingest(t.Context(), th)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if first.Messages.Inserted != 6 || first.Turns.Inserted != 3 {
		t.Fatalf("first ingest = %+v", first)
	}
	// Re-reading the same file from scratch and ingesting again inserts
	// nothing: the foreign ids carry the idempotency, which is what lets a
	// stale-cursor rescan be safe.
	again, _, _, err := src.Read(env, refs[0], JSONLCursor{})
	if err != nil {
		t.Fatalf("Read again: %v", err)
	}
	second, err := w.Ingest(t.Context(), again)
	if err != nil {
		t.Fatalf("re-Ingest: %v", err)
	}
	if second.Messages.Inserted != 0 || second.Parts.Inserted != 0 || second.Turns.Inserted != 0 {
		t.Fatalf("re-ingest inserted rows: %+v", second)
	}
}

// A read resumed after an append sees only the appended lines — the
// watermark doing its job against a real source.
func TestFakeSourceResumesAfterAppend(t *testing.T) {
	home := t.TempDir()
	writeFakeTranscript(t, home, "sess-2.jsonl", fakeTranscript)
	env := homeEnv(home)
	var src fakeSource
	ref := Ref{Provider: ProviderClaude, ID: "sess-2", Path: filepath.Join(fakeDir, "sess-2.jsonl")}

	_, cur, _, err := src.Read(env, ref, JSONLCursor{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	path := filepath.Join(home, fakeDir, "sess-2.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString(`{"type":"assistant","uuid":"a3","role":"assistant","content":[{"type":"text","data":{"text":"welcome"}}]}` + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()

	th, _, res, err := src.Read(env, ref, cur)
	if err != nil {
		t.Fatalf("resumed Read: %v", err)
	}
	if len(th.Messages) != 1 || th.Messages[0].ForeignID != "a3" {
		t.Fatalf("resumed read got %+v, want only a3", th.Messages)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("resumed read skipped %+v, want nothing", res.Skipped)
	}
}

// Acceptance: Discover returns ErrUnsupportedOS on Windows. The assertion
// has to be written both ways round, because this test runs on the host and
// the Windows arm is what GOOS=windows go vet compiles.
func TestCheckOSMatchesPlatform(t *testing.T) {
	err := CheckOS()
	if runtime.GOOS == "windows" {
		if !errors.Is(err, ErrUnsupportedOS) {
			t.Fatalf("CheckOS on windows = %v, want ErrUnsupportedOS", err)
		}
		if _, derr := (fakeSource{}).Discover(homeEnv(t.TempDir())); !errors.Is(derr, ErrUnsupportedOS) {
			t.Fatalf("Discover on windows = %v, want ErrUnsupportedOS", derr)
		}
		return
	}
	if err != nil {
		t.Fatalf("CheckOS on %s = %v, want nil", runtime.GOOS, err)
	}
}

// Acceptance for the cursor-type clause of the Source contract: a cursor of
// the wrong concrete kind is a typed refusal, and nothing is read.
func TestFakeSourceRefusesWrongCursorType(t *testing.T) {
	home := t.TempDir()
	writeFakeTranscript(t, home, "sess-3.jsonl", fakeTranscript)
	ref := Ref{Provider: ProviderClaude, ID: "sess-3", Path: filepath.Join(fakeDir, "sess-3.jsonl")}

	th, cur, res, err := (fakeSource{}).Read(homeEnv(home), ref, SQLiteCursor{TimeUpdated: 1})
	if !errors.Is(err, ErrCursorType) {
		t.Fatalf("err = %v, want ErrCursorType", err)
	}
	if len(th.Messages) != 0 || len(th.Turns) != 0 {
		t.Fatalf("refused read still produced %d messages / %d turns", len(th.Messages), len(th.Turns))
	}
	// A rejected cursor comes back as it went in: the source read nothing
	// and has no watermark of its own to offer.
	if cur != (SQLiteCursor{TimeUpdated: 1}) {
		t.Fatalf("cursor = %+v, want the input cursor untouched", cur)
	}
	if res.Lines != 0 || len(res.Skipped) != 0 {
		t.Fatalf("refused read accounted for %+v, want nothing", res)
	}

	// A nil cursor is not a mismatch: it is how a caller asks for a full
	// read.
	if _, _, _, err := (fakeSource{}).Read(homeEnv(home), ref, nil); err != nil {
		t.Fatalf("Read with a nil cursor = %v, want a full read", err)
	}
}

func TestResultMerge(t *testing.T) {
	a := Result{Lines: 2, Unknown: 1, UnknownTypes: map[string]int{"x": 1}}
	a.Classify("mode")
	a.Skip(1, 0, "invalid json")
	b := Result{Lines: 3, Unknown: 2, UnknownTypes: map[string]int{"x": 1, "y": 2}}
	b.Classify("mode")
	b.Classify("summary")
	b.Skip(7, 100, "invalid json")

	a.Merge(b)
	if a.Lines != 5 || a.Unknown != 3 {
		t.Fatalf("merged = %+v", a)
	}
	if a.UnknownTypes["x"] != 2 || a.UnknownTypes["y"] != 2 {
		t.Fatalf("UnknownTypes = %v", a.UnknownTypes)
	}
	if a.Classified["mode"] != 2 || a.Classified["summary"] != 1 {
		t.Fatalf("Classified = %v", a.Classified)
	}
	if len(a.Skipped) != 2 {
		t.Fatalf("Skipped = %+v", a.Skipped)
	}
}

// Merge has to respect the same bounds as Skip, or folding two Results
// would be a way around them.
func TestResultMergeRespectsSkipBounds(t *testing.T) {
	var a, b Result
	for i := 0; i < 80; i++ {
		a.Skip(i+1, int64(i), "invalid json")
		b.Skip(i+1, int64(i), "invalid json")
	}
	a.Merge(b)
	if a.SkippedCount != 160 {
		t.Fatalf("SkippedCount = %d, want 160", a.SkippedCount)
	}
	if len(a.Skipped) != maxSkippedRetained {
		t.Fatalf("len(Skipped) = %d, want %d", len(a.Skipped), maxSkippedRetained)
	}
}

func TestSkipTruncatesReason(t *testing.T) {
	var res Result
	// An invalid UTF-8 tail, so the cut cannot be left as it fell: these
	// strings end up in a database column and on a terminal.
	res.Skip(1, 0, strings.Repeat("é", 200)+"\xff")
	if got := res.Skipped[0].Reason; len(got) > maxSkipReasonBytes {
		t.Fatalf("Reason is %d bytes, want it bounded", len(got))
	} else if !utf8.ValidString(got) {
		t.Fatalf("Reason is not valid UTF-8: %q", got)
	}
	// A short reason is left exactly as given.
	res.Skip(2, 0, "invalid json")
	if got := res.Skipped[1].Reason; got != "invalid json" {
		t.Fatalf("Reason = %q, want it untouched", got)
	}
}
