package claude

import (
	"encoding/json"

	"github.com/krowkcom/cli/internal/store"
)

// line is one JSONL record, decoded down to the fields this package acts on
// and no further. Everything else stays in the raw bytes, which are kept
// verbatim on the message row, so a field nobody thought to name here is
// still in the store rather than gone.
//
// Type is a json.RawMessage rather than a string because a line whose `type`
// is missing, or is a number, or is an object, has to be told apart from a
// line whose type is simply one this build has not met: the first is a skip
// with a reason, the second is a classification. Unmarshalling into a string
// would collapse the two into one decode error.
type line struct {
	Type json.RawMessage `json:"type"`

	UUID      string `json:"uuid"`
	SessionID string `json:"sessionId"`
	AgentID   string `json:"agentId"`
	CWD       string `json:"cwd"`

	IsMeta            bool `json:"isMeta"`
	IsAPIErrorMessage bool `json:"isApiErrorMessage"`

	// Origin says where a user line came from when Claude recorded it:
	// `human` for something a person sent, `task-notification` for an
	// agent reporting back, `coordinator` and `peer` for one agent
	// addressing another. Absent on the great majority of lines, which
	// predate the field.
	Origin *origin `json:"origin"`
	// PromptSource is the other half of that question, and a coarser one.
	// See injected in read.go for which values mean "nobody typed this"
	// and, more importantly, which look like they should and do not.
	PromptSource string `json:"promptSource"`

	// Message is the Anthropic API message on a `user` or `assistant`
	// line.
	Message *apiMessage `json:"message"`
	// Content is what a `system` line carries instead of a message. It is
	// raw because it is a string on every line observed and there is no
	// promise that it stays one: decoding it as a string would turn the
	// day it becomes an object into a whole line skipped, which is a
	// large loss to take over a small change.
	Content json.RawMessage `json:"content"`
	// Subtype names what a `system` line is about — turn_duration,
	// api_error and their kin. It rides along as event-ish detail on the
	// message rather than becoming a part of its own.
	Subtype string `json:"subtype"`

	// Attachment is the payload of an `attachment` line.
	Attachment json.RawMessage `json:"attachment"`
	// HookEvent is the hook that produced an attachment, when one did. It
	// is nested inside `attachment` on every transcript observed, but the
	// top-level spelling is read too and wins when both are present: it is
	// the shape the format is documented in, and a line carrying it is not
	// worth losing over where it sits.
	HookEvent string `json:"hookEvent"`

	// AITitle is the session title on an `ai-title` line.
	AITitle string `json:"aiTitle"`
	// Summary is the session title on a `summary` line, the older
	// spelling. No transcript on the machine this was written against
	// still carries one; it is read because a transcript predating the
	// rename should not import untitled.
	Summary string `json:"summary"`
}

// origin is the provenance stamp on a user line.
type origin struct {
	Kind string `json:"kind"`
}

// apiMessage is the Anthropic message inside a `user` or `assistant` line.
// Content is raw because it is a string on some user lines and an array of
// blocks on the rest, and Usage is raw because it is copied into the store
// whole — the token classes this package sums are read from it separately,
// and the fields it does not sum (cache_creation breakdowns, service_tier,
// server_tool_use) are worth keeping for whoever asks later.
type apiMessage struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   json.RawMessage `json:"usage"`
}

// contentBlock is one entry of a message's content array. The fields cover
// every block type observed — text, thinking, tool_use, tool_result, image —
// and a block naming none of them keeps its raw bytes and becomes an
// `unknown` part, so a new block type shows up as a number in
// Result.UnknownTypes instead of as a silently shorter transcript.
type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Signature string          `json:"signature"`
	Text      string          `json:"text"`
}

// attachmentHook is the hook event as it actually appears: nested one level
// down, inside the attachment payload.
type attachmentHook struct {
	HookEvent string `json:"hookEvent"`
}

// tokenUsage is the token classes a turn's cost is summed from. The names
// are Anthropic's; the mapping onto store.Turn's columns is in addTo, in one
// place, because a cache read counted as a cache write is the kind of error
// that only shows up as a bill.
type tokenUsage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

// addTo folds one message's usage into the turn it belongs to. Total is the
// sum of the four rather than a field of its own: Anthropic reports no
// total, and a reader adding the columns up must get the same number the
// column already holds.
func (u tokenUsage) addTo(t *store.Turn) {
	t.CostInput += u.Input
	t.CostOutput += u.Output
	t.CostCacheRead += u.CacheRead
	t.CostCacheWrite += u.CacheWrite
	t.CostTotal += u.Input + u.Output + u.CacheRead + u.CacheWrite
}
