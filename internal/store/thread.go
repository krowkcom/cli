// Thread is the canonical shape every importer produces: one session and
// everything under it, in transcript order. Importers differ in where they
// read (JSONL, SQLite) and how they split parts, but they converge here, so
// the store sees one shape no matter the source.
//
// provider, harness and type stay open strings on purpose: a new agent or a
// new part kind must not need a schema change to land. role is the one
// closed enum — the message table carries the CHECK, and an unknown role
// fails Ingest before any row is written.
//
// seq appears nowhere here: the store is the only place that assigns it, so
// a re-imported transcript converges instead of colliding. Position-keyed
// rows (turns, events) are positional by slice order — the store maps
// position i to seq i, which means a Thread carries the full ordered list
// every time and the store skips the known prefix. Messages with a
// ForeignID dedup by (session_id, foreign_id) instead, so a message batch
// may be a delta; a message with no ForeignID is always appended, so its
// caller must hold a cursor and never re-send.
package store

// Role is the closed message-role enum, mirroring the CHECK on the message
// table. Anything else fails Ingest during validation, before any write.
type Role string

// The roles the message CHECK admits. Kept in one list so the Go boundary
// and the DDL can never disagree silently — validRole is the check, and a
// test pins it against the CHECK text.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleTool      Role = "tool"
	RoleError     Role = "error"
)

// Worktree names the checkout a session ran in. Path is the identity:
// Ingest upserts on it.
type Worktree struct {
	Path string
	VCS  string
	Name string
}

// Session carries the mutable display fields of one session. Identity lives
// in Binding, not here: two Threads naming the same
// (provider, foreign_session_id) converge on one session row.
type Session struct {
	Directory string
	Title     string
	Model     string
	Provider  string
	Harness   string
}

// Binding is how a foreign session maps onto a store session. The
// (Provider, ForeignSessionID) pair is UNIQUE in the schema — it is the
// only lookup key Ingest uses to find a session.
type Binding struct {
	Provider         string
	Harness          string
	ForeignSessionID string
	ResumeCmd        string
}

// Turn is one model turn in transcript order. No foreign id exists for
// turns, so the slice position is the identity: position i becomes
// seq i, and a re-sent prefix is skipped, not duplicated.
type Turn struct {
	Status         string
	CostInput      int64
	CostOutput     int64
	CostTotal      int64
	CostCacheRead  int64
	CostCacheWrite int64
	CostReasoning  int64
	// CostUSDMicros is nil when the cost is unknown — the column is NULL,
	// never a guessed zero.
	CostUSDMicros *int64
}

// Event is one session-scoped event in transcript order. Like Turn it is
// positional: position i becomes seq i. Type and Data stay open strings;
// Data is JSON, defaulting to '{}' when empty.
type Event struct {
	Type string
	Data string
}

// Message is one transcript message. ForeignID is the importer's id for it
// (opencode msg_…, Anthropic msg_…); empty means none, stored as NULL so
// the partial unique index ignores it. Role must be one of the Role
// constants. Usage is JSON defaulting to '{}'; RawJSON is the source line,
// nil when the importer holds none.
type Message struct {
	Role      Role
	Provider  string
	Model     string
	ForeignID string
	Usage     string
	RawJSON   *string
	Parts     []Part
}

// Part is one content block of a message, in order. Seq is the index inside
// its message. Type is open (text, thinking, tool_call, tool_result, image,
// file, patch, step, unknown — fixed by internal/importer, not here).
// ToolCallID links a tool_result back to its tool_call; Signature carries a
// provider signature when one exists. Data is JSON, defaulting to '{}'.
// ForeignID holds a source part id when the importer has one, else empty.
type Part struct {
	Type       string
	ToolCallID string
	Signature  string
	Data       string
	ForeignID  string
}

// Thread is one full session transcript: the worktree it ran in, the
// session display fields, the binding that identifies it, and the
// transcript itself in order. Turns and Events are cumulative ordered
// lists; Messages may be cumulative or delta — anything carrying a
// ForeignID the store already holds is skipped with its parts.
type Thread struct {
	Worktree Worktree
	Session  Session
	Binding  Binding
	// Parent is the binding of the session this one was spawned from — a
	// Claude subagent naming the conversation that dispatched it — or nil
	// for a session nobody spawned. It is a Binding rather than a session
	// id because an importer never sees store ids: it knows the foreign
	// session it read, and the store is the only thing that can turn that
	// into a row id.
	//
	// The link is best-effort by design. If the parent has not been
	// ingested yet the child's parent_id stays NULL and a later Ingest of
	// the same child fills it in, so a caller that walks children first
	// converges rather than failing. Callers should still ingest parents
	// before children — internal/importer/claude's Discover returns them
	// in that order for exactly this reason — because converging costs a
	// second pass and a NULL in between.
	Parent   *Binding
	Turns    []Turn
	Events   []Event
	Messages []Message
}
