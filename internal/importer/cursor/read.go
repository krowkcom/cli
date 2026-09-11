package cursor

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/krowkcom/cli/internal/harness"
	"github.com/krowkcom/cli/internal/importer"
	"github.com/krowkcom/cli/internal/store"
)

// eventRepo is the session_event type the repo.json sidecar lands under. Its
// data is {"repo_id": ...}: the file holds an id, not a path, so there is no
// worktree to derive from it.
const eventRepo = "cursor_repo"

// Read parses the transcript ref names from cursor onward and returns it as
// one Thread.
//
// Unlike claude/opencode this read honors the cursor: it passes it to
// importer.ReadJSONL, so a delta read returns only new lines. That is safe
// here because messages carry no foreign id — the store appends them, so the
// caller must hold the cursor and never re-send, and re-reading the whole
// file would duplicate every message. A cursor of the wrong concrete kind is
// refused with importer.ErrCursorType and handed straight back, because
// nothing was read and a zero cursor would read as "start again". An open
// failure likewise hands back the held cursor: the transcript has not
// invalidated the watermark.
func (s Source) Read(env harness.Env, ref importer.Ref, cursor importer.Cursor) (store.Thread, importer.Cursor, importer.Result, error) {
	// The watermark to report if the read never gets started. A file that
	// could not be opened has not invalidated the one the caller was
	// holding — returning a zero cursor instead would tell a caller whose
	// transcript is momentarily unreadable to re-import the whole session
	// next time, duplicating every message it already holds.
	held := importer.JSONLCursor{}
	if cursor != nil {
		typed, ok := cursor.(importer.JSONLCursor)
		if !ok {
			return store.Thread{}, cursor, importer.Result{}, fmt.Errorf("cursor: %w, got %T", importer.ErrCursorType, cursor)
		}
		held = typed
	}

	f, err := importer.OpenHome(env, ref.Path, importer.DefaultMaxBytes)
	if err != nil {
		return store.Thread{}, held, importer.Result{}, fmt.Errorf("cursor: open %s: %w", ref.Path, err)
	}
	defer func() { _ = f.Close() }()

	// The mtime is read now so a future store writer that stamps
	// per-message times has the value in hand. Thread carries no per-message
	// time today and the store stamps ingest time, so it is retained, not
	// used — deliberately, not an oversight.
	var mtime int64
	if info, serr := f.Stat(); serr == nil {
		mtime = info.ModTime().UnixMilli()
	}

	b := &builder{ref: ref, full: held.Zero(), fileMTime: mtime}
	next, res, err := importer.ReadJSONL(f, held, b.line)
	b.acc.Merge(res)
	// The repo sidecar is read after the transcript so a transcript failure
	// still reports what the transcript produced; its own failure is never
	// the transcript's. Missing or unreadable is no event, not an error.
	if b.full {
		b.repoEvent(env)
	}
	th := b.thread()
	if err != nil {
		return th, next, b.acc, fmt.Errorf("cursor: read %s: %w", ref.Path, err)
	}
	return th, next, b.acc, nil
}

// builder accumulates one transcript as it is walked. It exists so the
// per-line callback stays a switch over line shapes rather than a closure
// over locals.
type builder struct {
	ref importer.Ref
	acc importer.Result
	// full is whether this Read started at the top of the file. Only a full
	// read emits the repo.json event — turns and events are positional
	// cumulative lists in the store, so re-emitting it on every delta read
	// would append a duplicate per import.
	full bool
	// fileMTime is the transcript's mtime at import, retained for a future
	// writer; see Read.
	fileMTime int64

	// calls is every tool_call_id emitted so far in this Read, in order.
	// Pairing only ever happens within one pass — real transcripts carry no
	// tool_results at all — so a set rebuilt per Read is the whole of it.
	calls map[string]bool

	messages   []store.Message
	candidates []importer.TurnCandidate
	events     []store.Event
}

// line is the per-line callback. Returning an error skips the line and counts
// it; only a failure that is not the line's fault would wrap
// importer.ErrAbortFile, and nothing here is that — a Cursor transcript is
// read best-effort, one bad line at a time.
func (b *builder) line(lineNo int, raw []byte) error {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return fmt.Errorf("cursor: unreadable line: %w", err)
	}
	role, ok := l.Role, l.Role == "user" || l.Role == "assistant"
	if !ok {
		// A line with no role is furniture if it names itself — turn_ended
		// — and a skip otherwise. Classifying by the type string rather
		// than a fixed list keeps whatever Cursor adds next counted
		// instead of unaccounted.
		if l.Type != "" {
			b.acc.Classify(l.Type)
			return nil
		}
		return fmt.Errorf("cursor: line has no role")
	}
	return b.message(role, l, raw, lineNo)
}

// message turns one role-carrying line into a store message plus the turn
// candidate the rule reads.
//
// ForeignID is always "": nothing in the transcript names a message, and a
// synthesised id would be worse than none — it would claim a dedup key the
// store cannot honor across rewrites. The store appends NULL-foreign-id
// messages unconditionally, which is why the caller must hold the cursor.
func (b *builder) message(role string, l line, raw []byte, lineNo int) error {
	msg := store.Message{
		Role:      store.Role(role),
		Provider:  Provider,
		ForeignID: "",
		RawJSON:   rawJSON(raw),
	}
	msg.Parts = nil
	// A role line with no message object is a message with no parts,
	// not a failure: the role is transcript, and a turn needs a prompt
	// part to open on, which this line has none of.
	if l.Message != nil {
		msg.Parts = b.parts(l.Message.Content, lineNo)
	}

	// The <timestamp> tags inside user text are left in the text, not
	// parsed: they are prose the transcript happens to format, and the turn
	// rule treats any non-tool-result user line as a prompt.
	cand := importer.TurnCandidate{Role: store.Role(role)}
	for _, p := range msg.Parts {
		cand.PartTypes = append(cand.PartTypes, p.Type)
	}

	b.messages = append(b.messages, msg)
	b.candidates = append(b.candidates, cand)
	return nil
}

// parts turns a message's content into canonical parts. Content is an array
// of blocks on every transcript observed; a bare string is handled too, so
// the one shape a reader has to know for text is the same either way.
func (b *builder) parts(content json.RawMessage, lineNo int) []store.Part {
	if len(content) == 0 || string(content) == "null" {
		return nil
	}
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		if text == "" {
			return nil
		}
		return []store.Part{{Type: importer.PartText, Data: textData(text)}}
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		// Content that is neither a string nor an array is a shape this
		// build has not met. It becomes one unknown part carrying the whole
		// thing, which is counted, rather than a message with no content,
		// which is not.
		return []store.Part{b.acc.NormalizePart("message_content", content)}
	}
	var parts []store.Part
	for _, raw := range blocks {
		parts = append(parts, b.block(raw, lineNo))
	}
	return parts
}

// block maps one content block onto a canonical part.
//
// tool_use goes through the contract's constructor because its Data shape is
// fixed and the call id is what pairs it. The id is "cursor:<lineNo>" when
// the block names none — which is every block observed — position-keyed and
// stable for full reads; on delta reads only new lines are numbered, which
// is exactly the append case. Everything else keeps its raw block as Data
// through NormalizePart, so a field this package does not name is still
// stored and a type it has never met is counted as unknown instead of
// dropped.
func (b *builder) block(raw json.RawMessage, lineNo int) store.Part {
	var blk contentBlock
	if err := json.Unmarshal(raw, &blk); err != nil {
		return b.acc.NormalizePart("block", raw)
	}
	switch blk.Type {
	case "text":
		return store.Part{Type: importer.PartText, Data: textData(blk.Text)}
	case "tool_use":
		id := blk.ID
		if id == "" {
			id = "cursor:" + strconv.Itoa(lineNo)
		}
		if b.calls == nil {
			b.calls = map[string]bool{}
		}
		b.calls[id] = true
		return importer.NewToolCallPart(id, blk.Name, blk.Input)
	case "tool_result":
		// UNOBSERVED SHAPE, held leniently: zero tool_results on the census
		// machine, so every field name here is a guess at what Cursor
		// writes. Each alias is tried in turn; a result that links to
		// nothing is still emitted, counted as unlinked, because dropping
		// it would lose transcript over a pairing this package cannot
		// verify.
		id := firstNonEmpty(blk.ToolUseID, blk.CallID, blk.ID)
		if id != "" && !b.calls[id] {
			b.acc.Classify("tool_result:unlinked")
		} else if id == "" {
			b.acc.Classify("tool_result:unlinked")
		}
		return importer.NewToolResultPart(id, resultOutput(blk), blk.IsError || blk.IsErr)
	default:
		return b.acc.NormalizePart(blk.Type, raw)
	}
}

// resultOutput is the payload of a tool_result block: content, else output,
// else null. A string stays a JSON string rather than being flattened —
// ToolResultData keeps structured results structured, and a string is the
// common case of one.
func resultOutput(blk contentBlock) json.RawMessage {
	for _, raw := range []json.RawMessage{blk.Content, blk.Output} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			text, _ := json.Marshal(s)
			return json.RawMessage(text)
		}
		return raw
	}
	return json.RawMessage("null")
}

// repoEvent appends the repo.json sidecar as one session event. The file
// holds {"id": "<uuid>"} — a repo id, not a path — so the event carries the
// id under "repo_id" and nothing else. Missing, unreadable, or id-less is no
// event, not an error: the transcript is the session, the sidecar is a note.
func (b *builder) repoEvent(env harness.Env) {
	// The sidecar sits beside agent-transcripts, not beside the session
	// file: <slug>/repo.json, three directories up from <id>/<id>.jsonl.
	rel := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(b.ref.Path))), "repo.json")
	data, err := importer.ReadHome(env, rel, importer.DefaultMaxBytes)
	if err != nil {
		return
	}
	var sidecar struct {
		ID     string `json:"id"`
		RepoID string `json:"repo_id"`
		RepoId string `json:"repoId"`
	}
	if err := json.Unmarshal(data, &sidecar); err != nil {
		return
	}
	id := firstNonEmpty(sidecar.RepoID, sidecar.RepoId, sidecar.ID)
	if id == "" {
		return
	}
	payload, err := json.Marshal(struct {
		RepoID string `json:"repo_id"`
	}{RepoID: id})
	if err != nil {
		return
	}
	b.events = append(b.events, store.Event{Type: eventRepo, Data: string(payload)})
}

// thread assembles what was accumulated into the canonical shape.
func (b *builder) thread() store.Thread {
	slug := slugOf(b.ref.Path)
	path, vcs := worktreeOf(slug)
	if path == "" {
		// The slug decoded to nothing on disk: file under the slug
		// itself, counted, rather than inventing a directory or refusing
		// the session. Base of "cursor:<slug>" is the whole thing, which
		// names the slug — documented, not a guess at a path.
		path, vcs = "cursor:"+slug, vcsNone
		b.acc.Classify("worktree-fallback")
	}
	th := store.Thread{
		Worktree: store.Worktree{Path: path, VCS: vcs, Name: baseName(path)},
		Session: store.Session{
			Directory: "",
			Provider:  Provider,
			Harness:   Harness,
		},
		Binding: store.Binding{
			Provider:         importer.ProviderCursor,
			Harness:          Harness,
			ForeignSessionID: b.ref.ID,
			// Resuming is unknown in v1: anything here would be a command
			// nobody verified.
			ResumeCmd: "",
		},
		Events:   b.events,
		Messages: b.messages,
	}
	th.Turns = b.turns()
	// Referenced for the build, and for the day a writer takes it; see Read.
	_ = b.fileMTime
	return th
}

// turns splits the messages into turns. Cursor transcripts price nothing, so
// every turn costs zero — the spans are still computed, because the turn
// count is the prompt count and that is worth knowing. Status is "done" for
// every turn: the transcript records no cancellation this reader could tell
// from a finished turn, and guessing would be a guess.
func (b *builder) turns() []store.Turn {
	spans := importer.SplitTurns(b.candidates)
	turns := make([]store.Turn, 0, len(spans))
	for range spans {
		turns = append(turns, store.Turn{Status: "done"})
	}
	return turns
}

// line is one transcript line: a role with a message, or furniture with a
// type and no role.
type line struct {
	Role    string    `json:"role"`
	Type    string    `json:"type"`
	Message *envelope `json:"message"`
}

// envelope is the message object: content only. Cursor names no model per
// message on any transcript observed, so none is read.
type envelope struct {
	Content json.RawMessage `json:"content"`
}

// contentBlock is one entry of a message's content array.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// tool_use fields. ID is read too, though no transcript observed
	// carries it: the day one does, the block's own id should win over the
	// synthesised one.
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result fields, all unobserved — see block. Each name is one
	// guess at what Cursor writes.
	ToolUseID string          `json:"tool_use_id"`
	CallID    string          `json:"callID"`
	Content   json.RawMessage `json:"content"`
	Output    json.RawMessage `json:"output"`
	IsError   bool            `json:"isError"`
	IsErr     bool            `json:"is_error"`
}

// textData is the Data of a text part built from a bare string, so the one
// shape a reader has to know for text is the same whether the transcript
// wrote a block or a string.
func textData(s string) string {
	b, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: s})
	if err != nil {
		return ""
	}
	return string(b)
}

// rawJSON keeps the source line for the message row, or nothing when the line
// is not text the column could hold.
func rawJSON(raw []byte) *string {
	if !utf8.Valid(raw) {
		return nil
	}
	s := string(raw)
	return &s
}

// firstNonEmpty is the first of its arguments that says something.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// slugOf pulls the project slug out of a ref path:
// .cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl. Anything but
// that shape decodes to "" and falls back the way a missing directory does.
func slugOf(refPath string) string {
	parts := strings.Split(filepath.ToSlash(refPath), "/")
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] == ".cursor" && parts[i+1] == "projects" && parts[i+2] != "" && parts[i+3] == transcriptsDir {
			return parts[i+2]
		}
	}
	return ""
}
