package claude

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/krowkcom/cli/internal/harness"
	"github.com/krowkcom/cli/internal/importer"
	"github.com/krowkcom/cli/internal/store"
)

// interruptPrefix is how Claude records a request the person cancelled. It
// arrives as a user-role line, so without recognising it the cancellation
// would open a turn and swallow whatever was asked next.
const interruptPrefix = "[Request interrupted"

// eventAttachment is the session_event type an attachment with a hook event
// lands under.
const eventAttachment = "attachment"

// Read parses the whole transcript ref names and returns it as one Thread.
//
// The cursor is type-checked and then its offset is deliberately not used;
// see the package doc for why a source whose turns are cumulative cannot
// resume from the middle of a file. A cursor of the wrong concrete kind is
// refused with importer.ErrCursorType and handed straight back, because
// nothing was read and a zero cursor would read as "start again" — a
// caller's bug turned into a silent full rescan is the one failure mode the
// contract asks sources to avoid.
func (s Source) Read(env harness.Env, ref importer.Ref, cursor importer.Cursor) (store.Thread, importer.Cursor, importer.Result, error) {
	if cursor != nil {
		if _, ok := cursor.(importer.JSONLCursor); !ok {
			return store.Thread{}, cursor, importer.Result{}, fmt.Errorf("claude: %w, got %T", importer.ErrCursorType, cursor)
		}
	}

	f, err := importer.OpenHome(env, ref.Path, importer.DefaultMaxBytes)
	if err != nil {
		return store.Thread{}, importer.JSONLCursor{}, importer.Result{}, fmt.Errorf("claude: open %s: %w", ref.Path, err)
	}
	defer func() { _ = f.Close() }()

	b := &builder{ref: ref, subagent: isSubagentPath(ref.Path)}
	// Where the transcript itself lives, for the session that never
	// recorded a working directory; see fallbackWorktree. Resolved before
	// the read so the failure to resolve it is not a failure of the read.
	b.fallback, _ = importer.HomePath(env, filepath.Dir(ref.Path))
	// A zero cursor in, always: the read starts at the top of the file
	// whatever watermark the caller held.
	next, res, err := importer.ReadJSONL(f, importer.JSONLCursor{}, b.line)
	b.acc.Merge(res)
	th := b.thread()
	if err != nil {
		return th, next, b.acc, fmt.Errorf("claude: read %s: %w", ref.Path, err)
	}
	return th, next, b.acc, nil
}

// isSubagentPath reports whether a ref points into a session's subagents
// directory. It is decided from the path rather than from the file's
// contents because the answer is needed before the first line is read — a
// subagent binds on its agent id and a session binds on its session id, and
// the two are different keys into the same table.
func isSubagentPath(path string) bool {
	return strings.Contains(path, "/"+subagentsDir+"/")
}

// builder accumulates one transcript as it is walked. It exists so the
// per-line callback stays a switch over line types rather than a closure
// over a dozen locals, and so the ordering rules — first cwd wins, last
// title wins — are stated once, where the field is assigned.
type builder struct {
	ref      importer.Ref
	subagent bool
	// fallback is the directory the transcript file sits in, used as the
	// worktree of a session that never named one; see fallbackWorktree.
	fallback string
	acc      importer.Result

	// sessionID is the `sessionId` the lines carry. On a subagent file
	// that is the parent's session, which is exactly what makes the parent
	// link findable without parsing the path.
	sessionID string
	// agentID is the `agentId` the lines carry, on a subagent file.
	agentID string
	// directory is the first cwd seen. First rather than last: a session
	// that changed directory mid-run was still started somewhere, and the
	// place it started is the one a person recognises.
	directory string
	// title is the last ai-title or summary seen. Last rather than first:
	// Claude rewrites the title as the conversation turns out to be about
	// something else, and the newest guess is the best one.
	title string
	// model is the last model an assistant message named, which is the
	// model the session ended up on after any mid-session switch.
	model string

	messages   []store.Message
	usages     []tokenUsage
	candidates []importer.TurnCandidate
	events     []store.Event
}

// line is the per-line callback. Returning an error skips the line and
// counts it; only a failure that is not the line's fault would wrap
// importer.ErrAbortFile, and nothing here is that — a Claude transcript is
// read best-effort, one bad line at a time.
func (b *builder) line(_ int, raw []byte) error {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return fmt.Errorf("claude: unreadable line: %w", err)
	}
	kind, ok := lineType(l.Type)
	if !ok {
		return fmt.Errorf("claude: line has no string type")
	}

	// Identity and directory come off any line that carries them, not just
	// the ones that become messages: a transcript whose only `cwd` sits on
	// an attachment still ran somewhere.
	if l.SessionID != "" && b.sessionID == "" {
		b.sessionID = l.SessionID
	}
	if l.AgentID != "" && b.agentID == "" {
		b.agentID = l.AgentID
	}
	if l.CWD != "" && b.directory == "" {
		b.directory = l.CWD
	}

	switch kind {
	case "user", "assistant":
		return b.message(kind, l, raw)
	case "system":
		return b.system(l, raw)
	case "attachment":
		return b.attachment(l, kind)
	case "ai-title", "summary":
		// Title-bearing furniture: the title is taken and the line is
		// still classified, because it did not become a row of its own.
		if t := firstNonEmpty(l.AITitle, l.Summary); t != "" {
			b.title = t
		}
		b.acc.Classify(kind)
		return nil
	default:
		// Everything else — mode, last-prompt, queue-operation,
		// atis-latch, file-history-snapshot, file-history-delta, pr-link,
		// permission-mode, cost-state, frame-link, continued-in,
		// agent-name, and whatever Claude adds next — is understood well
		// enough to be declined. Classifying by the type string rather
		// than matching a fixed list is what keeps an unreleased line
		// type counted instead of unaccounted.
		b.acc.Classify(kind)
		return nil
	}
}

// lineType decodes the `type` field, reporting whether it was a string at
// all. A line that is JSON but has no usable type is not furniture this
// package declined; it is a line nothing can be said about, which is a skip.
func lineType(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return "", false
	}
	return s, true
}

// message turns a `user` or `assistant` line into a store message plus the
// turn candidate the rule will read.
//
// The role is the line type, not the message's own `role` field: they agree
// on every transcript observed, and where they would not, the line type is
// the one the file is organised by. The one override is isApiErrorMessage,
// which marks an assistant line that is the API's failure rather than the
// model's answer — storing that as an ordinary assistant reply would put
// error text in the transcript as if the model had said it.
func (b *builder) message(kind string, l line, raw []byte) error {
	role := store.RoleUser
	if kind == "assistant" {
		role = store.RoleAssistant
		if l.IsAPIErrorMessage {
			role = store.RoleError
		}
	}

	msg := store.Message{
		Role:      role,
		Provider:  Provider,
		ForeignID: l.UUID,
		RawJSON:   rawJSON(raw),
	}
	var usage tokenUsage
	if l.Message != nil {
		msg.Model = l.Message.Model
		if msg.Model != "" {
			b.model = msg.Model
		}
		msg.Usage = usableJSON(l.Message.Usage)
		if len(l.Message.Usage) > 0 {
			// A usage block that will not decode is not worth failing the
			// line over: the raw JSON is on the row either way, and a
			// turn costed at zero is a visibly missing number rather
			// than a wrong one.
			_ = json.Unmarshal(l.Message.Usage, &usage)
		}
		msg.Parts = b.parts(l.Message.Content)
	}

	cand := importer.TurnCandidate{
		Role:      role,
		Meta:      l.IsMeta,
		Interrupt: role == store.RoleUser && isInterrupt(l.Message),
	}
	for _, p := range msg.Parts {
		cand.PartTypes = append(cand.PartTypes, p.Type)
	}

	b.messages = append(b.messages, msg)
	b.usages = append(b.usages, usage)
	b.candidates = append(b.candidates, cand)
	return nil
}

// parts turns a message's content into canonical parts. Content is a string
// on the user lines a person typed straight into the terminal and an array
// of blocks everywhere else, so both are handled here rather than at the
// two call sites that would otherwise each have to know.
func (b *builder) parts(content json.RawMessage) []store.Part {
	if len(content) == 0 {
		return nil
	}
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return []store.Part{{Type: importer.PartText, Data: textData(text)}}
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		// Content that is neither a string nor an array is a shape this
		// build has not met. It becomes one unknown part carrying the
		// whole thing, which is counted, rather than a message with no
		// content, which is not.
		return []store.Part{b.acc.NormalizePart("content", content)}
	}
	var parts []store.Part
	for _, raw := range blocks {
		parts = append(parts, b.block(raw))
	}
	return parts
}

// block maps one content block onto a canonical part.
//
// tool_use and tool_result go through the contract's constructors rather
// than keeping their raw payload, because their Data shape is fixed and the
// call id is what pairs them: a tool_result whose part did not carry
// tool_use_id could never be joined back to the call that produced it, since
// nothing in the schema joins them. Everything else keeps its raw block as
// Data, so a field this package does not name is still stored.
func (b *builder) block(raw json.RawMessage) store.Part {
	var blk contentBlock
	if err := json.Unmarshal(raw, &blk); err != nil {
		return b.acc.NormalizePart("block", raw)
	}
	switch blk.Type {
	case "tool_use":
		return importer.NewToolCallPart(blk.ID, blk.Name, blk.Input)
	case "tool_result":
		return importer.NewToolResultPart(blk.ToolUseID, blk.Content, blk.IsError)
	case importer.PartThinking:
		part := b.acc.NormalizePart(blk.Type, raw)
		// The signature is lifted into its own column because it is what
		// lets a thinking block be replayed to the API: buried in Data it
		// would be a string nobody could find without knowing Anthropic's
		// block shape.
		part.Signature = blk.Signature
		return part
	default:
		// text, image, and anything new: NormalizePart keeps the
		// canonical ones as they are and files the rest as `unknown` with
		// their raw payload, counting them so the gap is visible.
		return b.acc.NormalizePart(blk.Type, raw)
	}
}

// system turns a `system` line into a system-role message with one text
// part. These lines carry Claude's own notes — turn durations, API retries,
// hook failures — and are a message rather than an event because they read
// as transcript: dropping them loses the only record that a request was
// retried four times.
func (b *builder) system(l line, raw []byte) error {
	msg := store.Message{
		Role:      store.RoleSystem,
		Provider:  Provider,
		ForeignID: l.UUID,
		RawJSON:   rawJSON(raw),
	}
	if l.Content != "" {
		msg.Parts = []store.Part{{Type: importer.PartText, Data: textData(l.Content)}}
	}
	b.messages = append(b.messages, msg)
	b.usages = append(b.usages, tokenUsage{})
	// Meta, always: a system line is never a prompt, and a turn opened by
	// one would take the credit for the work that followed.
	b.candidates = append(b.candidates, importer.TurnCandidate{Role: store.RoleSystem, Meta: true})
	return nil
}

// attachment handles the largest line type on a real machine, and the one
// where doing the obvious thing is wrong in both directions.
//
// An attachment is not a message. It is context Claude injected — a file it
// read, a token reminder, a listing of available tools — and importing it as
// a user message would put words in a person's mouth and, worse, would
// double the turn count, since attachments arrive alongside prompts. So the
// vast majority are classified and counted, not stored.
//
// The exception is an attachment carrying a hook event. That one records
// something that actually happened at a point in the session — a
// SessionStart, a UserPromptSubmit hook firing — which is what session_event
// is for. It keeps the hook name and the payload, and nothing else, because
// the rest of the line is the same envelope every other line carries.
func (b *builder) attachment(l line, kind string) error {
	hook := l.HookEvent
	if hook == "" && len(l.Attachment) > 0 {
		var nested attachmentHook
		if err := json.Unmarshal(l.Attachment, &nested); err == nil {
			hook = nested.HookEvent
		}
	}
	if hook == "" {
		b.acc.Classify(kind)
		return nil
	}
	b.events = append(b.events, store.Event{
		Type: eventAttachment,
		Data: eventData(hook, l.Attachment),
	})
	return nil
}

// thread assembles what was accumulated into the canonical shape.
func (b *builder) thread() store.Thread {
	dir := b.directory
	path, vcs := worktreeOf(dir)
	if path == "" {
		path, vcs = fallbackWorktree(b.fallback)
	}

	// A subagent binds on its agent id, a session on its session id. The
	// file's own `agentId` wins over the one Discover read off the
	// filename; see subagentRefs.
	foreignID := b.sessionID
	if b.subagent {
		foreignID = firstNonEmpty(b.agentID, b.ref.ID)
	}
	if foreignID == "" {
		// A transcript with no session id at all still has to bind to
		// something, and the ref id is what the import_state key is built
		// from anyway, so binding on it keeps the two agreeing.
		foreignID = b.ref.ID
	}

	th := store.Thread{
		Worktree: store.Worktree{Path: path, VCS: vcs, Name: baseName(path)},
		Session: store.Session{
			Directory: dir,
			Title:     b.title,
			Model:     b.model,
			Provider:  Provider,
			Harness:   Harness,
		},
		Binding: store.Binding{
			Provider:         importer.ProviderClaude,
			Harness:          Harness,
			ForeignSessionID: foreignID,
			// A subagent cannot be resumed on its own, so the command
			// resumes the conversation that dispatched it. That is the
			// thing a person reading the row would actually want to open.
			ResumeCmd: resumeCmd(firstNonEmpty(b.sessionID, b.ref.ID)),
		},
		Events:   b.events,
		Messages: b.messages,
	}
	if b.subagent && b.sessionID != "" && b.sessionID != foreignID {
		th.Parent = &store.Binding{
			Provider:         importer.ProviderClaude,
			Harness:          Harness,
			ForeignSessionID: b.sessionID,
		}
	}
	th.Turns = b.turns()
	return th
}

// turns splits the messages into turns and costs each one.
//
// The costing is deliberately span-wide rather than per-assistant-message:
// a turn is one prompt and everything the agent did about it, which on a
// tool loop is a dozen API calls, and a turn cost that only counted the last
// one would understate the expensive turns by an order of magnitude. Status
// is "done" for every turn, including one that ends in an interrupt — the
// column exists for a future importer that can tell a cancelled turn from a
// finished one, and guessing from a transcript that records the
// cancellation as a user line would be a guess.
func (b *builder) turns() []store.Turn {
	spans := importer.SplitTurns(b.candidates)
	turns := make([]store.Turn, 0, len(spans))
	for _, span := range spans {
		t := store.Turn{Status: "done"}
		for i := span.Start; i < span.End && i < len(b.usages); i++ {
			b.usages[i].addTo(&t)
		}
		turns = append(turns, t)
	}
	return turns
}

// isInterrupt reports whether a user message is a cancellation notice
// rather than something a person asked for. It reads the leading text
// wherever the content keeps it, string or first block, because the notice
// is written both ways depending on what was interrupted.
func isInterrupt(msg *apiMessage) bool {
	if msg == nil {
		return false
	}
	var text string
	if err := json.Unmarshal(msg.Content, &text); err != nil {
		var blocks []contentBlock
		if err := json.Unmarshal(msg.Content, &blocks); err != nil || len(blocks) == 0 {
			return false
		}
		text = blocks[0].Text
	}
	return strings.HasPrefix(text, interruptPrefix)
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

// eventData is the payload of an attachment event: the hook that fired and
// the attachment it carried, and nothing of the envelope.
func eventData(hook string, attachment json.RawMessage) string {
	b, err := json.Marshal(struct {
		HookEvent  string          `json:"hook_event"`
		Attachment json.RawMessage `json:"attachment,omitempty"`
	}{HookEvent: hook, Attachment: json.RawMessage(usableJSON(attachment))})
	if err != nil {
		return ""
	}
	return string(b)
}

// usableJSON returns raw when it can be stored as it stands and "" when it
// cannot. Valid JSON is not enough: the columns are TEXT and everything
// downstream assumes UTF-8, so a payload carrying raw bytes is dropped to
// the store's '{}' default rather than written and read back as mojibake.
// The line's own raw_json still holds it either way.
func usableJSON(raw json.RawMessage) string {
	if len(raw) == 0 || !json.Valid(raw) || !utf8.Valid(raw) {
		return ""
	}
	return string(raw)
}

// rawJSON keeps the source line for the message row, or nothing when the
// line is not text the column could hold.
func rawJSON(raw []byte) *string {
	if !utf8.Valid(raw) {
		return nil
	}
	s := string(raw)
	return &s
}

// resumeCmd is what a person types to get back into a session.
func resumeCmd(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return "claude --resume " + sessionID
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
