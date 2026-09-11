// Session listing and detail reads: the picker (Phase 1) and show.
//
// The listing query is pinned like the Phase 0 count probe: it must never
// touch message/part blobs, so it reads session columns plus a turn cost
// aggregate plus the binding harness — and nothing else. sessionListQuery
// is a constant so the gate pins the exact text.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// sessionListQuery lists sessions newest-first from columns only: session,
// its worktree path, one binding row, and a turn cost aggregate. It names
// no blob column (message.raw_json, message.usage, part.data) and no part
// table at all — a 10k-session listing stays instant because it never
// reads a message or part row.
//
// The binding and turn joins each yield at most one row per session: the
// binding through the earliest-minted row (binding ids are UUIDv7, so id
// order is creation order) and the turns through a pre-aggregated
// subquery. Joining both tables directly would fan out — a second binding
// multiplies the turn COUNT/SUM and a second turn duplicates the binding
// columns. sessionListQuery is a constant so the gate pins the exact text.
//
// The page CTE selects the listed session ids first (filters, global
// recency order, LIMIT), and the turn aggregate is restricted to those ids:
// without the filter the subquery scans and aggregates the entire turn
// table even for a LIMIT 50 page. The binding MIN(id) dedup still scans
// the binding table; bindings are one row per import, so that scan stays
// small while turns — one row per agent step — are the table that grows.
const sessionListQuery = `WITH page AS (SELECT s.id AS pid FROM session s LEFT JOIN worktree w ON w.id = s.worktree_id LEFT JOIN (SELECT session_id, MIN(id) AS id FROM session_binding GROUP BY session_id) one ON one.session_id = s.id LEFT JOIN session_binding b ON b.id = one.id WHERE (? = '' OR COALESCE(b.harness, s.harness) = ?) AND (? = '' OR w.path = ?) ORDER BY s.time_updated DESC LIMIT ?) SELECT s.id, s.title, s.model, s.provider, s.harness, s.directory, s.time_created, s.time_updated, w.path, b.harness, b.provider, b.foreign_session_id, COALESCE(t.n, 0), COALESCE(t.sum_in, 0), COALESCE(t.sum_out, 0), COALESCE(t.sum_total, 0), COALESCE(t.sum_cread, 0), COALESCE(t.sum_cwrite, 0), COALESCE(t.sum_reason, 0) FROM session s JOIN page ON page.pid = s.id LEFT JOIN worktree w ON w.id = s.worktree_id LEFT JOIN (SELECT session_id, MIN(id) AS id FROM session_binding GROUP BY session_id) one ON one.session_id = s.id LEFT JOIN session_binding b ON b.id = one.id LEFT JOIN (SELECT session_id, COUNT(*) AS n, SUM(cost_input_tokens) AS sum_in, SUM(cost_output_tokens) AS sum_out, SUM(cost_total_tokens) AS sum_total, SUM(cost_cache_read_tokens) AS sum_cread, SUM(cost_cache_write_tokens) AS sum_cwrite, SUM(cost_reasoning_tokens) AS sum_reason FROM turn WHERE session_id IN (SELECT pid FROM page) GROUP BY session_id) t ON t.session_id = s.id ORDER BY s.time_updated DESC`

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
// -1 for --all, and 0 is treated the same (never "zero rows"): it is the
// unset value the shared flag set carries, so a caller that forgot to
// default still lists rather than answering empty. Pass a positive page or
// -1. Rows come newest-first.
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

// AmbiguousSessionError is what ResolveSessionID returns when a prefix
// matches more than one session. Callers match it with errors.As to pick
// the ambiguous_session code — never by substring on the message.
type AmbiguousSessionError struct {
	Ref string
	IDs []string
	msg string
}

func (e *AmbiguousSessionError) Error() string { return e.msg }

// ResolveSessionID maps what a person typed to a store session id: a full
// id, an unambiguous id prefix of at least 8 chars, or a foreign_session_id
// via the binding. An ambiguous prefix fails naming every candidate.
// A ref that matches nothing wraps sql.ErrNoRows, so callers map it with
// errors.Is exactly like LoadSessionDetail's missing row; any other failure
// wraps the store error underneath and is a store problem, not a miss.
func ResolveSessionID(db *sql.DB, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("store: pass a session id: `krowk sessions show <id>`: %w", sql.ErrNoRows)
	}
	var id string
	if err := db.QueryRow(`SELECT id FROM session WHERE id = ?`, ref).Scan(&id); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("store: resolve session: %w", err)
		}
	} else {
		return id, nil
	}
	// A foreign session id resolves via the binding (Claude sessionId).
	var viaBinding string
	if err := db.QueryRow(`SELECT session_id FROM session_binding WHERE foreign_session_id = ?`, ref).Scan(&viaBinding); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("store: resolve session: %w", err)
		}
	} else {
		return viaBinding, nil
	}
	if len(ref) < 8 {
		return "", fmt.Errorf("store: %q matches no session — pass a full id, an id prefix of at least 8 chars, or a foreign session id: %w", ref, sql.ErrNoRows)
	}
	rows, err := db.Query(`SELECT id, title FROM session WHERE id LIKE ? ESCAPE '\' ORDER BY id LIMIT 11`, escapeLikePrefix(ref)+"%")
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
		return "", fmt.Errorf("store: %q matches no session: %w", ref, sql.ErrNoRows)
	case 1:
		return ids[0], nil
	default:
		truncated := false
		if len(ids) > 10 {
			// The candidate query caps at 11: a full head of repeats
			// (UUIDv7 ids share a time prefix) must not pour thousands
			// of rows into one error string.
			truncated = true
			ids, titles = ids[:10], titles[:10]
		}
		var b strings.Builder
		if truncated {
			fmt.Fprintf(&b, "store: %q is ambiguous (more than 10 sessions, showing first 10 — refine the prefix):", ref)
		} else {
			fmt.Fprintf(&b, "store: %q is ambiguous (%d sessions):", ref, len(ids))
		}
		for i := range ids {
			title := sanitizeCell(titles[i])
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(&b, "\n  %s  %s", ids[i], title)
		}
		return "", &AmbiguousSessionError{Ref: ref, IDs: ids, msg: b.String()}
	}
}

// sanitizeCell makes caller-controlled transcript text safe for a terminal
// row: whitespace folds to single spaces and control / bidi / zero-width
// runes are dropped. Local copy of the CLI cleanCell logic — store cannot
// import the CLI package without a cycle.
func sanitizeCell(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case unicode.IsSpace(r):
			space = true
		case unicode.IsControl(r), reorderRune(r):
			// Dropped outright: an escape sequence arrives as ESC plus
			// ordinary letters, spacing it would leave letters as text.
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// reorderRune reports characters that move or hide text while occupying no
// space: bidi overrides/isolates, zero-width spaces, BOM. ZWJ is kept to
// hold multi-part emoji together.
func reorderRune(r rune) bool {
	switch {
	case r == '\u200d':
		return false
	case r == '\ufeff',
		r >= '\u200b' && r <= '\u200f',
		r >= '\u202a' && r <= '\u202e',
		r >= '\u2066' && r <= '\u2069':
		return true
	}
	return false
}

// escapeLikePrefix quotes the LIKE wildcards in a typed id prefix: % and _
// match any run under LIKE, and the ESCAPE '\' clause needs backslashes
// doubled. Without this a prefix of "%" lists every session.
func escapeLikePrefix(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
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
	err := db.QueryRow(`SELECT s.id, s.title, s.model, s.provider, s.harness, s.directory, s.time_created, s.time_updated, w.path, (SELECT b.harness FROM session_binding b WHERE b.session_id = s.id ORDER BY b.id LIMIT 1), (SELECT b.provider FROM session_binding b WHERE b.session_id = s.id ORDER BY b.id LIMIT 1), (SELECT b.foreign_session_id FROM session_binding b WHERE b.session_id = s.id ORDER BY b.id LIMIT 1) FROM session s LEFT JOIN worktree w ON w.id = s.worktree_id WHERE s.id = ?`, sessionID).
		Scan(&r.ID, &r.Title, &r.Model, &r.Provider, &r.Harness, &r.Directory,
			&r.TimeCreated, &r.TimeUpdated, &wpath, &bharness, &bprovider, &foreign)
	if err == sql.ErrNoRows {
		return d, fmt.Errorf("store: no session %q: %w", sessionID, sql.ErrNoRows)
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

	// Parts load in one session-scoped query (part carries session_id for
	// exactly this), then distribute to their messages in seq order. The
	// tool-name pass runs over every part before any tool_result links, so
	// a result links to its call regardless of which message comes first.
	prows, err := db.Query(`SELECT message_id, seq, type, tool_call_id, signature, data, foreign_id FROM part WHERE session_id = ? ORDER BY message_id, seq`, sessionID)
	if err != nil {
		return d, fmt.Errorf("store: load parts: %w", err)
	}
	partsByMsg := map[string][]PartDetail{}
	toolNames := map[string]string{}
	for prows.Next() {
		var msgID string
		var p PartDetail
		var tcid, sig, pforeign sql.NullString
		if err := prows.Scan(&msgID, &p.Seq, &p.Type, &tcid, &sig, &p.Data, &pforeign); err != nil {
			prows.Close()
			return d, fmt.Errorf("store: scan part: %w", err)
		}
		p.ToolCallID = tcid.String
		p.Signature = sig.String
		p.ForeignID = pforeign.String
		partsByMsg[msgID] = append(partsByMsg[msgID], p)
		// Index the twin's name inline: the link pass below runs after
		// every row is scanned, so a result still finds a call that
		// comes later without keeping a second copy of every part.
		if p.Type == "tool_call" && p.ToolCallID != "" {
			if name := toolCallName(p.Data); name != "" {
				if _, ok := toolNames[p.ToolCallID]; !ok {
					toolNames[p.ToolCallID] = name
				}
			}
		}
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return d, fmt.Errorf("store: load parts rows: %w", err)
	}
	// Second pass: link every tool_result to its twin's name.
	for msgID, parts := range partsByMsg {
		for i := range parts {
			p := &parts[i]
			if p.Type != "tool_result" {
				continue
			}
			if name, ok := toolNames[p.ToolCallID]; ok && p.ToolCallID != "" {
				p.ToolName, p.Linked = name, true
			} else {
				p.ToolName, p.Linked = "unknown tool", false
			}
		}
		partsByMsg[msgID] = parts
	}
	for _, mk := range msgs {
		mk.msg.Parts = partsByMsg[mk.id]
		d.Messages = append(d.Messages, mk.msg)
	}
	return d, nil
}

// toolCallName reads the top-level {"name":...} of a tool_call part's
// data with a real JSON decode: a substring scan mistakes a nested
// input.name for the call name and mis-decodes escapes.
//
// ToolNameOf is the exported half, for the CLI's human and JSON rendering.
func ToolNameOf(data string) string { return toolCallName(data) }
func toolCallName(data string) string {
	var v struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return ""
	}
	return v.Name
}
