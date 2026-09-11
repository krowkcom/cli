// Session listing and detail reads: the picker (Phase 1) and show.
//
// The listing query is pinned like the Phase 0 count probe: it must never
// touch message/part blobs, so it reads session columns plus a turn cost
// aggregate plus the binding harness — and nothing else. sessionListQuery
// is a constant so the gate pins the exact text.
package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// sessionListQuery lists sessions newest-first from columns only: session,
// its worktree path, one binding row, and a turn cost aggregate. It names
// no blob column (message.raw_json, message.usage, part.data) and no part
// table at all — a 10k-session listing stays instant because it never
// reads a message or part row.
const sessionListQuery = `SELECT s.id, s.title, s.model, s.provider, s.harness, s.directory, s.time_created, s.time_updated, w.path, b.harness, b.provider, b.foreign_session_id, COUNT(t.id), COALESCE(SUM(t.cost_input_tokens), 0), COALESCE(SUM(t.cost_output_tokens), 0), COALESCE(SUM(t.cost_total_tokens), 0), COALESCE(SUM(t.cost_cache_read_tokens), 0), COALESCE(SUM(t.cost_cache_write_tokens), 0), COALESCE(SUM(t.cost_reasoning_tokens), 0) FROM session s LEFT JOIN worktree w ON w.id = s.worktree_id LEFT JOIN session_binding b ON b.session_id = s.id LEFT JOIN turn t ON t.session_id = s.id WHERE (? = '' OR COALESCE(b.harness, s.harness) = ?) AND (? = '' OR w.path = ?) GROUP BY s.id ORDER BY s.time_updated DESC LIMIT ?`

// SessionRow is one listing row: display columns plus the turn aggregate
// the priced cost is derived from at display time.
type SessionRow struct {
	ID               string
	Title            string
	Model            string
	Provider         string
	Harness          string
	Directory        string
	TimeCreated      int64
	TimeUpdated      int64
	WorktreePath     string
	BindingHarness   string
	BindingProvider  string
	ForeignSessionID string
	TurnCount        int
	SumInput         int64
	SumOutput        int64
	SumTotal         int64
	SumCacheRead     int64
	SumCacheWrite    int64
	SumReasoning     int64
}

// ListSessions runs sessionListQuery: harness and worktree are exact-match
// filters (empty means no filter), limit <= 0 means no cap — callers pass
// -1 for --all. Rows come newest-first.
func ListSessions(db *sql.DB, harness, worktree string, limit int) ([]SessionRow, error) {
	q := sessionListQuery
	lim := limit
	if lim <= 0 {
		lim = -1
	}
	rows, err := db.Query(q, harness, harness, worktree, worktree, lim)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		var wpath, bharness, bprovider, foreign sql.NullString
		if err := rows.Scan(&r.ID, &r.Title, &r.Model, &r.Provider, &r.Harness,
			&r.Directory, &r.TimeCreated, &r.TimeUpdated,
			&wpath, &bharness, &bprovider, &foreign,
			&r.TurnCount, &r.SumInput, &r.SumOutput, &r.SumTotal,
			&r.SumCacheRead, &r.SumCacheWrite, &r.SumReasoning); err != nil {
			return nil, fmt.Errorf("store: scan session row: %w", err)
		}
		r.WorktreePath = wpath.String
		r.BindingHarness = bharness.String
		r.BindingProvider = bprovider.String
		r.ForeignSessionID = foreign.String
		// The binding harness is the displayed harness when present; the
		// session column is the fallback for rows predating bindings.
		if r.BindingHarness != "" {
			r.Harness = r.BindingHarness
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sessions rows: %w", err)
	}
	return out, nil
}

// ResolveSessionID maps what a person typed to a store session id: a full
// id, an unambiguous id prefix of at least 8 chars, or a foreign_session_id
// via the binding. An ambiguous prefix fails naming every candidate.
func ResolveSessionID(db *sql.DB, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("store: pass a session id: `krowk sessions show <id>`")
	}
	var id string
	if err := db.QueryRow(`SELECT id FROM session WHERE id = ?`, ref).Scan(&id); err == nil {
		return id, nil
	}
	// A foreign session id resolves via the binding (Claude sessionId).
	var viaBinding string
	if err := db.QueryRow(`SELECT session_id FROM session_binding WHERE foreign_session_id = ?`, ref).Scan(&viaBinding); err == nil {
		return viaBinding, nil
	}
	if len(ref) < 8 {
		return "", fmt.Errorf("store: %q matches no session — pass a full id, an id prefix of at least 8 chars, or a foreign session id", ref)
	}
	rows, err := db.Query(`SELECT id, title FROM session WHERE id LIKE ? || '%' ORDER BY id`, ref)
	if err != nil {
		return "", fmt.Errorf("store: resolve session: %w", err)
	}
	defer rows.Close()
	var ids, titles []string
	for rows.Next() {
		var cid, ctitle string
		if err := rows.Scan(&cid, &ctitle); err != nil {
			return "", fmt.Errorf("store: scan session candidates: %w", err)
		}
		ids = append(ids, cid)
		titles = append(titles, ctitle)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("store: resolve session rows: %w", err)
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("store: %q matches no session", ref)
	case 1:
		return ids[0], nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "store: %q is ambiguous (%d sessions):", ref, len(ids))
		for i := range ids {
			title := titles[i]
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&b, "\n  %s  %s", ids[i], title)
		}
		return "", fmt.Errorf("%s", b.String())
	}
}

// TurnDetail, MessageDetail and PartDetail are what show hydrates: turns,
// messages and parts in seq order. Linked/ToolName re-link a tool_result
// to its tool_call twin by tool_call_id.
type TurnDetail struct {
	Seq         int
	Status      string
	Input       int64
	Output      int64
	Total       int64
	CacheRead   int64
	CacheWrite  int64
	Reasoning   int64
	USDNull     bool
	USDMicros   int64
	TimeCreated int64
	TimeUpdated int64
}

type PartDetail struct {
	Seq        int
	Type       string
	ToolCallID string
	Signature  string
	Data       string
	ForeignID  string
	// ToolName is the twin tool_call's name for tool_result parts;
	// Linked is false when the twin is missing ("unknown tool").
	ToolName string
	Linked   bool
}

type MessageDetail struct {
	Seq         int
	Role        string
	Provider    string
	Model       string
	ForeignID   string
	TimeCreated int64
	Parts       []PartDetail
}

type SessionDetail struct {
	Session  SessionRow
	Turns    []TurnDetail
	Messages []MessageDetail
}

// LoadSessionDetail hydrates one session: header columns via the listing
// shape (no blobs), then turns, messages and parts in seq order. Part rows
// are read here — show is where parts are hydrated.
func LoadSessionDetail(db *sql.DB, sessionID string) (SessionDetail, error) {
	var d SessionDetail
	var r SessionRow
	var wpath, bharness, bprovider, foreign sql.NullString
	err := db.QueryRow(`SELECT s.id, s.title, s.model, s.provider, s.harness, s.directory, s.time_created, s.time_updated, w.path, b.harness, b.provider, b.foreign_session_id FROM session s LEFT JOIN worktree w ON w.id = s.worktree_id LEFT JOIN session_binding b ON b.session_id = s.id WHERE s.id = ?`, sessionID).
		Scan(&r.ID, &r.Title, &r.Model, &r.Provider, &r.Harness, &r.Directory,
			&r.TimeCreated, &r.TimeUpdated, &wpath, &bharness, &bprovider, &foreign)
	if err == sql.ErrNoRows {
		return d, fmt.Errorf("store: no session %q", sessionID)
	}
	if err != nil {
		return d, fmt.Errorf("store: load session: %w", err)
	}
	r.WorktreePath = wpath.String
	r.BindingHarness = bharness.String
	r.BindingProvider = bprovider.String
	r.ForeignSessionID = foreign.String
	if r.BindingHarness != "" {
		r.Harness = r.BindingHarness
	}
	d.Session = r

	trows, err := db.Query(`SELECT seq, status, cost_input_tokens, cost_output_tokens, cost_total_tokens, cost_cache_read_tokens, cost_cache_write_tokens, cost_reasoning_tokens, cost_usd_micros, time_created, time_updated FROM turn WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return d, fmt.Errorf("store: load turns: %w", err)
	}
	for trows.Next() {
		var t TurnDetail
		var usd sql.NullInt64
		if err := trows.Scan(&t.Seq, &t.Status, &t.Input, &t.Output, &t.Total,
			&t.CacheRead, &t.CacheWrite, &t.Reasoning, &usd, &t.TimeCreated, &t.TimeUpdated); err != nil {
			trows.Close()
			return d, fmt.Errorf("store: scan turn: %w", err)
		}
		if usd.Valid {
			t.USDMicros = usd.Int64
		} else {
			t.USDNull = true
		}
		d.Turns = append(d.Turns, t)
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return d, fmt.Errorf("store: load turns rows: %w", err)
	}
	d.Session.TurnCount = len(d.Turns)
	for _, t := range d.Turns {
		d.Session.SumInput += t.Input
		d.Session.SumOutput += t.Output
		d.Session.SumTotal += t.Total
		d.Session.SumCacheRead += t.CacheRead
		d.Session.SumCacheWrite += t.CacheWrite
		d.Session.SumReasoning += t.Reasoning
	}

	mrows, err := db.Query(`SELECT id, seq, role, provider, model, foreign_id, time_created FROM message WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return d, fmt.Errorf("store: load messages: %w", err)
	}
	type msgKey struct {
		id  string
		msg MessageDetail
	}
	var msgs []msgKey
	for mrows.Next() {
		var mid string
		var m MessageDetail
		var foreignID sql.NullString
		if err := mrows.Scan(&mid, &m.Seq, &m.Role, &m.Provider, &m.Model, &foreignID, &m.TimeCreated); err != nil {
			mrows.Close()
			return d, fmt.Errorf("store: scan message: %w", err)
		}
		m.ForeignID = foreignID.String
		msgs = append(msgs, msgKey{id: mid, msg: m})
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return d, fmt.Errorf("store: load messages rows: %w", err)
	}

	// Tool names keyed by call id, so tool_result parts print their twin's name.
	toolNames := map[string]string{}
	for _, mk := range msgs {
		prows, err := db.Query(`SELECT seq, type, tool_call_id, signature, data, foreign_id FROM part WHERE message_id = ? ORDER BY seq`, mk.id)
		if err != nil {
			return d, fmt.Errorf("store: load parts: %w", err)
		}
		var parts []PartDetail
		for prows.Next() {
			var p PartDetail
			var tcid, sig, pforeign sql.NullString
			if err := prows.Scan(&p.Seq, &p.Type, &tcid, &sig, &p.Data, &pforeign); err != nil {
				prows.Close()
				return d, fmt.Errorf("store: scan part: %w", err)
			}
			p.ToolCallID = tcid.String
			p.Signature = sig.String
			p.ForeignID = pforeign.String
			parts = append(parts, p)
		}
		prows.Close()
		if err := prows.Err(); err != nil {
			return d, fmt.Errorf("store: load parts rows: %w", err)
		}
		for _, p := range parts {
			if p.Type == "tool_call" && p.ToolCallID != "" {
				if name := toolCallName(p.Data); name != "" {
					if _, ok := toolNames[p.ToolCallID]; !ok {
						toolNames[p.ToolCallID] = name
					}
				}
			}
		}
		mk.msg.Parts = parts
		// append now; link pass below fills ToolName/Linked.
		d.Messages = append(d.Messages, mk.msg)
	}
	for mi := range d.Messages {
		for pi := range d.Messages[mi].Parts {
			p := &d.Messages[mi].Parts[pi]
			if p.Type != "tool_result" {
				continue
			}
			if name, ok := toolNames[p.ToolCallID]; ok && p.ToolCallID != "" {
				p.ToolName, p.Linked = name, true
			} else {
				p.ToolName, p.Linked = "unknown tool", false
			}
		}
	}
	return d, nil
}

// toolCallName pulls {"name":...} out of a tool_call part's data without a
// full schema: show only needs the name to label the twin result.
//
// ToolNameOf is the exported half, for the CLI's human and JSON rendering.
func ToolNameOf(data string) string { return toolCallName(data) }
func toolCallName(data string) string {
	start := strings.Index(data, `"name"`)
	if start < 0 {
		return ""
	}
	rest := data[start+len(`"name"`):]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return ""
	}
	rest = strings.TrimSpace(rest[colon+1:])
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	rest = rest[1:]
	var b strings.Builder
	escaped := false
	for _, r := range rest {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == '"' {
			return b.String()
		}
		b.WriteRune(r)
	}
	return ""
}
