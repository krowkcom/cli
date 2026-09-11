package opencode

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/krowkcom/cli/internal/harness"
	"github.com/krowkcom/cli/internal/importer"
	"github.com/krowkcom/cli/internal/store"
)

// Read parses the whole session ref and returns it as one Thread.
//
// The cursor is type-checked and then deliberately not used to skip
// anything; see the package doc for why a source whose turns are
// cumulative cannot resume from the middle of a session. A cursor of the
// wrong concrete kind is refused with importer.ErrCursorType and handed
// straight back, because nothing was read and a zero cursor would read as
// "start again" — a caller's bug turned into a silent full rescan is the
// one failure mode the contract asks sources to avoid. A read that cannot
// open the database likewise hands back the watermark it was given: the
// session has not invalidated it, and a zero cursor would re-import the
// whole session next time.
func (s Source) Read(env harness.Env, ref importer.Ref, cursor importer.Cursor) (store.Thread, importer.Cursor, importer.Result, error) {
	held := importer.SQLiteCursor{}
	if cursor != nil {
		typed, ok := cursor.(importer.SQLiteCursor)
		if !ok {
			return store.Thread{}, cursor, importer.Result{}, fmt.Errorf("opencode: %w, got %T", importer.ErrCursorType, cursor)
		}
		held = typed
	}

	rel := ref.Path
	if rel == "" {
		rel = dbRel
	}
	// The ref's path is a hint naming the one database this source
	// reads, never an arbitrary file to open: anything but the known
	// relative path is a caller's bug, refused rather than followed.
	if rel != dbRel {
		return store.Thread{}, held, importer.Result{}, fmt.Errorf("opencode: unexpected ref path %q", rel)
	}
	dbPath, err := importer.HomePath(env, rel)
	if err != nil {
		return store.Thread{}, held, importer.Result{}, fmt.Errorf("opencode: resolve database: %w", err)
	}
	db, err := openReadOnly(dbPath)
	if err != nil {
		return store.Thread{}, held, importer.Result{}, fmt.Errorf("opencode: open %s: %w", rel, err)
	}
	defer func() { _ = db.Close() }()

	b := &builder{ref: ref}
	if err := b.load(db); err != nil {
		// The thread is whatever accumulated before the failure, which
		// may be empty: a caller that stored nothing re-imports the
		// prefix next time, and the foreign_id dedup keeps that free.
		return b.thread(), held, b.acc, fmt.Errorf("opencode: read %s: %w", ref.ID, err)
	}
	th := b.thread()
	return th, importer.SQLiteCursor{TimeUpdated: b.maxUpdated}, b.acc, nil
}

// sessionRow is the session table read down to the columns this package
// acts on. The table carries far more — costs, token totals, share urls,
// summary diffs — and none of it is selected: per-turn costs come from the
// message rows, which is where the transcript prices itself, and the rest
// is store-derivable or display nobody asked for.
type sessionRow struct {
	projectID string
	parentID  sql.NullString
	directory sql.NullString
	title     sql.NullString
	model     sql.NullString
}

// messageData is the message.data JSON blob: who said it, on what model,
// what it cost, and what it burned. Tokens and cost ride on assistant
// messages; a user message carries neither, which is not an absence worth
// reporting.
type messageData struct {
	Role       string          `json:"role"`
	ModelID    string          `json:"modelID"`
	ProviderID string          `json:"providerID"`
	Tokens     json.RawMessage `json:"tokens"`
	Cost       *float64        `json:"cost"`
}

// tokenData is the token classes a turn's cost is summed from. The names
// are opencode's; the mapping onto store.Turn's columns is in addTo, in one
// place, because a cache read counted as a cache write is the kind of error
// that only shows up as a bill.
type tokenData struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

// partData is the part.data JSON blob, decoded down to the discriminator
// and the fields the tool twin needs. Everything else stays in the raw
// bytes, which are kept verbatim on the part row through NormalizePart, so
// a field nobody thought to name here is still in the store rather than
// gone.
type partData struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Tool     string          `json:"tool"`
	CallID   string          `json:"callID"`
	State    *toolState      `json:"state"`
	Snapshot string          `json:"snapshot"`
	Raw      json.RawMessage `json:"-"`
}

// toolState is one tool part's execution state. Input is the call
// arguments, output the result; both stay raw because a tool that returned
// structured JSON must not be flattened into text.
type toolState struct {
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
}

// sessionModel is the session.model JSON blob: the model the session ended
// up on, read only when no assistant message named one.
type sessionModel struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
}

// messageMeta is the phase-1 row: identity plus blob size, never the
// blob itself. Selecting no data column here is what keeps a 239MB row
// a single integer until its own turn comes.
type messageMeta struct {
	id      string
	created int64
	updated int64
	size    int64
}

// partMeta is the same idea for one message's parts.
type partMeta struct {
	id      string
	size    int64
	updated int64
}

// messageRawLimit caps the message.data blob materialised in one row.
// One live row is 239MB (a user message carrying summary.diffs of a
// vendor bundle); the ncruces/go-sqlite3 wasm driver panics with
// sqlite3: out of memory when that blob is SELECTed whole, and even
// json_extract(data,...) parses the full JSON server-side, so it OOMs
// the same way. No query here touches the data column of a huge row:
// phase 1 selects only id, timestamps and length(data), and phase 2
// selects either the whole blob (small rows) or a 2KB substr prefix
// (huge rows) whose identity fields are string-scanned in Go. Huge rows
// keep identity+role but drop raw/summary: RawJSON is nil, tokens/cost
// are whatever the prefix names (usually absent on user rows, hence
// zero), and Usage is set only when the prefix actually carried tokens.
const messageRawLimit = 200000

// messagePrefixLen is how many bytes of a huge message row are read.
// Role/model/provider/tokens/cost all ride at the top of the blob, so
// 2KB is plenty; the 239MB tail is never materialised.
const messagePrefixLen = 2000

// partRawLimit caps the part.data blob the same way. Live parts peak at
// 2.6MB so this should never fire; it is here so one pathological tool
// result cannot OOM the same driver. Parts are two-phase like messages:
// lengths first, then full blob or a short substr prefix.
const partRawLimit = 5000000

// partPrefixLen covers a part row's discriminator, tool twin ids and
// state status, which all ride at the top of the blob.
const partPrefixLen = 4000

type builder struct {
	ref importer.Ref
	acc importer.Result

	worktreePath string
	worktreeVCS  string
	directory    string
	title        string
	model        string
	provider     string
	parentID     string

	messages   []store.Message
	tokens     []tokenData
	costs      []float64
	hasCost    []bool
	candidates []importer.TurnCandidate

	// maxUpdated is the largest message time_updated seen: the watermark.
	maxUpdated int64
}

// load reads the session row, its project row, and every message with its
// parts, in transcript order.
func (b *builder) load(db *sql.DB) error {
	id := b.ref.ID
	if id == "" {
		return fmt.Errorf("opencode: ref has no session id")
	}
	var s sessionRow
	err := db.QueryRow(`SELECT project_id, parent_id, directory, title, model FROM session WHERE id = ?`, id).
		Scan(&s.projectID, &s.parentID, &s.directory, &s.title, &s.model)
	if err == sql.ErrNoRows {
		return fmt.Errorf("session %s not found", id)
	}
	if err != nil {
		return err
	}
	b.directory = s.directory.String
	b.title = s.title.String
	if s.parentID.Valid {
		b.parentID = s.parentID.String
	}
	b.model, b.provider = sessionModelOf(s.model.String)

	var worktree, vcs sql.NullString
	if err := db.QueryRow(`SELECT worktree, vcs FROM project WHERE id = ?`, s.projectID).Scan(&worktree, &vcs); err != nil && err != sql.ErrNoRows {
		return err
	}
	b.worktreePath, b.worktreeVCS = resolveWorktree(worktree.String, vcs.String, b.directory)

	rows, err := db.Query(`SELECT id, time_created, time_updated, length(CAST(data AS BLOB))
	  FROM message WHERE session_id = ? ORDER BY time_created ASC, rowid ASC`, id)
	if err != nil {
		return err
	}
	// Collected and closed before any part row is read: the handle holds
	// a single connection, so a parts query issued while the message rows
	// are still open would wait for a connection that never frees — a
	// deadlock, not a slow query. No data column is selected here at
	// all, so a 239MB row is just one integer until its own turn comes.
	var metas []messageMeta
	for rows.Next() {
		var r messageMeta
		if err := rows.Scan(&r.id, &r.created, &r.updated, &r.size); err != nil {
			_ = rows.Close()
			return err
		}
		metas = append(metas, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	// One message row plus its part rows per iteration, not one JOIN:
	// a session's parts only ever fit in memory one message at a time
	// this way, and the handle holds a single connection, so the message
	// rows are collected and closed above before any part row is read.
	n := 0
	for _, r := range metas {
		n++
		partMax, err := b.messageByID(db, n, r)
		if err != nil {
			// A message this build cannot use is a skip with a reason,
			// not a failed read: stopping would pin the session to the
			// messages before it on every attempt from then on. A huge
			// row whose prefix does not even name a role lands here
			// rather than under a guessed role. Skipped rows do not move
			// the watermark, so the next read retries them instead of
			// forgetting them.
			b.acc.Skip(n, 0, err.Error())
			continue
		}
		// The watermark is the largest timestamp successfully imported,
		// over message and part rows alike: a part edited after its
		// message must still move it.
		if r.updated > b.maxUpdated {
			b.maxUpdated = r.updated
		}
		if partMax > b.maxUpdated {
			b.maxUpdated = partMax
		}
	}
	return nil
}

// messageByID loads one message row in two phases: the full blob when
// it fits under messageRawLimit, a short substr prefix otherwise. The
// prefix path string-scans role/model/provider (plus tokens/cost when
// the prefix names them) and leaves RawJSON nil; a prefix with no role
// is an error to the caller above, which counts it as skipped.
//
// The returned int64 is the largest part time_updated under the message,
// folded into the watermark by the caller on success only.
//
// Every prefix scan is anchored to the top level of the blob (brace depth
// 1): the first `"role"` in the raw bytes is not necessarily the message's
// role when prose or nested objects name one, and a role flipped by a
// nested match would file an assistant row as a prompt. Tokens are scoped
// tighter still, to the `"tokens"` object substring, and cost to a
// top-level `"cost"`; anything not cleanly parsed there stays zero with a
// nil raw, which is honest about what was actually read.
func (b *builder) messageByID(db *sql.DB, n int, r messageMeta) (int64, error) {
	if r.size <= messageRawLimit {
		var data string
		if err := db.QueryRow(`SELECT data FROM message WHERE id = ?`, r.id).Scan(&data); err != nil {
			return 0, err
		}
		var d messageData
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return 0, fmt.Errorf("opencode: message %s: %w", r.id, err)
		}
		return b.addMessage(db, n, r.id, d.Role, d.ModelID, d.ProviderID, d.Tokens, d.Cost, rawJSON(data))
	}
	var prefix sql.NullString
	if err := db.QueryRow(`SELECT substr(data,1,?) FROM message WHERE id = ?`, messagePrefixLen, r.id).Scan(&prefix); err != nil {
		return 0, err
	}
	head := prefix.String
	role, ok := extractTopJSONString(head, "role")
	if !ok || role == "" {
		return 0, fmt.Errorf("opencode: message %s has no role in its prefix", r.id)
	}
	modelID, _ := extractTopJSONString(head, "modelID")
	providerID, _ := extractTopJSONString(head, "providerID")
	// Tokens/cost ride on assistant rows and are absent on the huge
	// user rows that motivated the cap; when the prefix names them
	// (early fields, as in the regression fixture) they are kept so
	// turn costing still sees the turn. Scoped to the tokens object so
	// a nested "input" in the tail cannot inflate a turn.
	var tk tokenData
	foundTokens := false
	if tokObj, ok := extractTopObject(head, "tokens"); ok {
		if v, ok := extractJSONInt(tokObj, "input"); ok {
			tk.Input, foundTokens = v, true
		}
		if v, ok := extractJSONInt(tokObj, "output"); ok {
			tk.Output, foundTokens = v, true
		}
		if v, ok := extractJSONInt(tokObj, "reasoning"); ok {
			tk.Reasoning, foundTokens = v, true
		}
		if cacheObj, ok := extractObjectField(tokObj, "cache"); ok {
			if v, ok := extractJSONInt(cacheObj, "read"); ok {
				tk.Cache.Read, foundTokens = v, true
			}
			if v, ok := extractJSONInt(cacheObj, "write"); ok {
				tk.Cache.Write, foundTokens = v, true
			}
		}
	}
	var tokensRaw json.RawMessage
	var usage string
	if foundTokens {
		if raw, err := json.Marshal(tk); err == nil {
			tokensRaw = raw
			usage = storable(raw)
		}
	}
	var cost *float64
	if v, ok := extractTopJSONFloat(head, "cost"); ok {
		cost = &v
	}
	partMax, err := b.addMessageRaw(db, n, r.id, role, modelID, providerID, tokensRaw, usage, tk, cost, nil)
	return partMax, err
}

// addMessage assembles one message from decoded fields, keeping the raw
// blob the caller materialised. It exists so the small-row path stays a
// thin decode over the shared assembly in addMessageRaw.
func (b *builder) addMessage(db *sql.DB, n int, msgID, role, modelID, providerID string, tokensRaw json.RawMessage, cost *float64, rawPtr *string) (int64, error) {
	var tk tokenData
	if len(tokensRaw) > 0 {
		// A tokens blob that will not decode is not worth failing the
		// message over: the raw JSON is on the row either way, and a
		// turn costed at zero is a visibly missing number rather than a
		// wrong one.
		_ = json.Unmarshal(tokensRaw, &tk)
	}
	usage := ""
	if len(tokensRaw) > 0 {
		usage = storable(tokensRaw)
	}
	return b.addMessageRaw(db, n, msgID, role, modelID, providerID, tokensRaw, usage, tk, cost, rawPtr)
}

// addMessageRaw is the single assembly both row paths share: role check,
// provider/model tracking, parts, turn candidate, line count. tokensRaw
// feeds Usage, tk feeds turn costing, cost feeds dollar costing, rawPtr
// is nil for huge rows whose blob was never materialised. It returns the
// largest part time_updated under the message for the watermark.
func (b *builder) addMessageRaw(db *sql.DB, n int, msgID, role, modelID, providerID string, tokensRaw json.RawMessage, usage string, tk tokenData, cost *float64, rawPtr *string) (int64, error) {
	var role2 store.Role
	switch role {
	case "user":
		role2 = store.RoleUser
	case "assistant":
		role2 = store.RoleAssistant
	case "system":
		role2 = store.RoleSystem
	default:
		// Live databases hold user and assistant only; anything else is
		// skipped with its kind counted, so a new role shows up in the
		// result instead of vanishing silently.
		b.acc.Classify("role:" + role)
		return 0, fmt.Errorf("opencode: message %s has role %q", msgID, role)
	}

	msg := store.Message{
		Role:      role2,
		Provider:  firstNonEmpty(providerID, Harness),
		Model:     modelID,
		ForeignID: msgID,
		Usage:     usage,
		RawJSON:   rawPtr,
	}
	if providerID != "" {
		b.provider = providerID
	}
	if role2 == store.RoleAssistant && modelID != "" {
		b.model = modelID
	}
	dollars := 0.0
	has := cost != nil
	if has {
		dollars = *cost
	}

	parts, partMax, err := b.parts(db, msgID)
	if err != nil {
		return 0, err
	}
	msg.Parts = parts

	cand := importer.TurnCandidate{Role: role2}
	for _, p := range msg.Parts {
		cand.PartTypes = append(cand.PartTypes, p.Type)
	}

	b.messages = append(b.messages, msg)
	b.tokens = append(b.tokens, tk)
	b.costs = append(b.costs, dollars)
	b.hasCost = append(b.hasCost, has)
	b.candidates = append(b.candidates, cand)
	// Lines counts what was consumed, skipped ones included: messages and
	// their part rows, which is the closest this source has to the JSONL
	// reader's line.
	b.acc.Lines += 1 + len(parts)
	return partMax, nil
}

// extractJSONString scans hay for `"key" : "value"` without parsing it
// as JSON, so a 239MB blob's 2KB prefix yields identity fields while
// the tail is never materialised. Whitespace around the colon is
// allowed; escapes in the value are unquoted when possible.
func extractJSONString(hay, key string) (string, bool) {
	needle := `"` + key + `"`
	for off := 0; off < len(hay); {
		i := strings.Index(hay[off:], needle)
		if i < 0 {
			return "", false
		}
		p := off + i + len(needle)
		for p < len(hay) && (hay[p] == ' ' || hay[p] == '\t' || hay[p] == '\n' || hay[p] == '\r') {
			p++
		}
		if p >= len(hay) || hay[p] != ':' {
			off = off + i + len(needle)
			continue
		}
		p++
		for p < len(hay) && (hay[p] == ' ' || hay[p] == '\t' || hay[p] == '\n' || hay[p] == '\r') {
			p++
		}
		if p >= len(hay) || hay[p] != '"' {
			off = off + i + len(needle)
			continue
		}
		p++
		var sb strings.Builder
		escaped := false
		for ; p < len(hay); p++ {
			c := hay[p]
			if escaped {
				sb.WriteByte(c)
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				return sb.String(), true
			}
			sb.WriteByte(c)
		}
		return "", false
	}
	return "", false
}

// braceDepthAt is the object nesting depth at byte pos: the count of
// unclosed '{' before it, ignoring braces inside double-quoted strings.
// Top-level fields of the blob sit at depth 1; a `"role"` at any other
// depth belongs to prose or a nested object, not to the message.
func braceDepthAt(hay string, pos int) int {
	depth := 0
	inStr := false
	escaped := false
	for i := 0; i < pos && i < len(hay); i++ {
		c := hay[i]
		if inStr {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' && depth > 0 {
			depth--
		}
	}
	return depth
}

// topKeyPos finds `"key"` at brace depth 1 followed by a colon, returning
// the offset just past the colon. Occurrences at any other depth — prose
// quoting a field name, a nested object — are skipped.
func topKeyPos(hay, key string) (int, bool) {
	needle := `"` + key + `"`
	for off := 0; off < len(hay); {
		i := strings.Index(hay[off:], needle)
		if i < 0 {
			return 0, false
		}
		start := off + i
		if braceDepthAt(hay, start) != 1 {
			off = start + len(needle)
			continue
		}
		p := start + len(needle)
		for p < len(hay) && (hay[p] == ' ' || hay[p] == '\t' || hay[p] == '\n' || hay[p] == '\r') {
			p++
		}
		if p >= len(hay) || hay[p] != ':' {
			off = start + len(needle)
			continue
		}
		return p + 1, true
	}
	return 0, false
}

// extractTopJSONString is extractJSONString anchored to the top level of
// the blob, so a nested "role" in prose cannot flip the message's role.
func extractTopJSONString(hay, key string) (string, bool) {
	p, ok := topKeyPos(hay, key)
	if !ok {
		return "", false
	}
	for p < len(hay) && (hay[p] == ' ' || hay[p] == '\t' || hay[p] == '\n' || hay[p] == '\r') {
		p++
	}
	if p >= len(hay) || hay[p] != '"' {
		return "", false
	}
	p++
	var sb strings.Builder
	escaped := false
	for ; p < len(hay); p++ {
		c := hay[p]
		if escaped {
			sb.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			return sb.String(), true
		}
		sb.WriteByte(c)
	}
	return "", false
}

// extractTopJSONNumber is extractJSONNumber anchored to the top level,
// so a nested number cannot pose as the message's own field.
func extractTopJSONNumber(hay, key string) (string, bool) {
	p, ok := topKeyPos(hay, key)
	if !ok {
		return "", false
	}
	for p < len(hay) && (hay[p] == ' ' || hay[p] == '\t' || hay[p] == '\n' || hay[p] == '\r') {
		p++
	}
	start := p
	for p < len(hay) && (hay[p] == '-' || hay[p] == '+' || hay[p] == '.' ||
		(hay[p] >= '0' && hay[p] <= '9') || hay[p] == 'e' || hay[p] == 'E') {
		p++
	}
	if p == start {
		return "", false
	}
	return hay[start:p], true
}

// extractTopJSONFloat is extractTopJSONNumber parsed as a float.
func extractTopJSONFloat(hay, key string) (float64, bool) {
	lit, ok := extractTopJSONNumber(hay, key)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// extractObjectField returns the balanced `{...}` object that is the value
// of `"key"` at any depth, so a scoped scan (tokens, cache) stays inside
// the object it names rather than matching a same-named field elsewhere.
func extractObjectField(hay, key string) (string, bool) {
	needle := `"` + key + `"`
	for off := 0; off < len(hay); {
		i := strings.Index(hay[off:], needle)
		if i < 0 {
			return "", false
		}
		start := off + i
		p := start + len(needle)
		for p < len(hay) && (hay[p] == ' ' || hay[p] == '\t' || hay[p] == '\n' || hay[p] == '\r') {
			p++
		}
		if p >= len(hay) || hay[p] != ':' {
			off = start + len(needle)
			continue
		}
		p++
		for p < len(hay) && (hay[p] == ' ' || hay[p] == '\t' || hay[p] == '\n' || hay[p] == '\r') {
			p++
		}
		if p >= len(hay) || hay[p] != '{' {
			off = start + len(needle)
			continue
		}
		depth := 0
		inStr := false
		escaped := false
		for q := p; q < len(hay); q++ {
			c := hay[q]
			if inStr {
				if escaped {
					escaped = false
					continue
				}
				if c == '\\' {
					escaped = true
					continue
				}
				if c == '"' {
					inStr = false
				}
				continue
			}
			if c == '"' {
				inStr = true
				continue
			}
			if c == '{' {
				depth++
			} else if c == '}' {
				depth--
				if depth == 0 {
					return hay[p : q+1], true
				}
			}
		}
		return "", false
	}
	return "", false
}

// extractTopObject is extractObjectField for a top-level key: the `"tokens"`
// object of the message, not a nested namesake.
func extractTopObject(hay, key string) (string, bool) {
	p, ok := topKeyPos(hay, key)
	if !ok {
		return "", false
	}
	for p < len(hay) && (hay[p] == ' ' || hay[p] == '\t' || hay[p] == '\n' || hay[p] == '\r') {
		p++
	}
	if p >= len(hay) || hay[p] != '{' {
		return "", false
	}
	depth := 0
	inStr := false
	escaped := false
	for q := p; q < len(hay); q++ {
		c := hay[q]
		if inStr {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return hay[p : q+1], true
			}
		}
	}
	return "", false
}

// extractJSONNumber scans hay for `"key" : <number>` and returns the
// raw literal, so ints and floats share one scanner.
func extractJSONNumber(hay, key string) (string, bool) {
	needle := `"` + key + `"`
	for off := 0; off < len(hay); {
		i := strings.Index(hay[off:], needle)
		if i < 0 {
			return "", false
		}
		p := off + i + len(needle)
		for p < len(hay) && (hay[p] == ' ' || hay[p] == '\t' || hay[p] == '\n' || hay[p] == '\r') {
			p++
		}
		if p >= len(hay) || hay[p] != ':' {
			off = off + i + len(needle)
			continue
		}
		p++
		for p < len(hay) && (hay[p] == ' ' || hay[p] == '\t' || hay[p] == '\n' || hay[p] == '\r') {
			p++
		}
		start := p
		for p < len(hay) && (hay[p] == '-' || hay[p] == '+' || hay[p] == '.' ||
			(hay[p] >= '0' && hay[p] <= '9') || hay[p] == 'e' || hay[p] == 'E') {
			p++
		}
		if p == start {
			off = off + i + len(needle)
			continue
		}
		return hay[start:p], true
	}
	return "", false
}

// extractJSONInt is extractJSONNumber parsed as an integer.
func extractJSONInt(hay, key string) (int64, bool) {
	lit, ok := extractJSONNumber(hay, key)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(lit, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// extractJSONFloat is extractJSONNumber parsed as a float.
func extractJSONFloat(hay, key string) (float64, bool) {
	lit, ok := extractJSONNumber(hay, key)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(lit, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parts reads one message's part rows in timeline order and maps each onto
// canonical parts, returning the largest part time_updated for the
// watermark. One row is usually one part; a finished tool row is two
// (see toolParts), which is why Lines above counts what came back rather
// than what went out — the part-count test holds the difference against
// the twin count. Two-phase like messages: lengths first (no data
// column), then the full blob per small part or a short substr prefix
// per oversized part, string-scanned in Go. No json_extract anywhere:
// it parses the whole blob server-side and OOMs the wasm driver.
func (b *builder) parts(db *sql.DB, msgID string) ([]store.Part, int64, error) {
	rows, err := db.Query(`SELECT id, length(CAST(data AS BLOB)), time_updated
	  FROM part WHERE message_id = ? ORDER BY time_created ASC, rowid ASC`, msgID)
	if err != nil {
		return nil, 0, err
	}
	var metas []partMeta
	for rows.Next() {
		var m partMeta
		if err := rows.Scan(&m.id, &m.size, &m.updated); err != nil {
			_ = rows.Close()
			return nil, 0, err
		}
		metas = append(metas, m)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, err
	}
	_ = rows.Close()
	var parts []store.Part
	var partMax int64
	for _, m := range metas {
		if m.updated > partMax {
			partMax = m.updated
		}
		ps, err := b.partByID(db, m)
		if err != nil {
			return nil, 0, err
		}
		parts = append(parts, ps...)
	}
	return parts, partMax, nil
}

// partByID loads one part row: the full blob when it fits under
// partRawLimit, a short substr prefix otherwise. The prefix path
// string-scans type/tool/callID (plus text/snapshot/status when the
// prefix names them) and maps through partCapped with a synthesized
// payload; a prefix with no type is an error naming the part, which the
// message caller counts as skipped with reason.
func (b *builder) partByID(db *sql.DB, m partMeta) ([]store.Part, error) {
	if m.size <= partRawLimit {
		var data string
		if err := db.QueryRow(`SELECT data FROM part WHERE id = ?`, m.id).Scan(&data); err != nil {
			return nil, err
		}
		return b.part(m.id, data), nil
	}
	var prefix sql.NullString
	if err := db.QueryRow(`SELECT substr(data,1,?) FROM part WHERE id = ?`, partPrefixLen, m.id).Scan(&prefix); err != nil {
		return nil, err
	}
	typ, ok := extractTopJSONString(prefix.String, "type")
	if !ok || typ == "" {
		return nil, fmt.Errorf("opencode: part %s has no type in its prefix", m.id)
	}
	tool, _ := extractTopJSONString(prefix.String, "tool")
	callID, _ := extractTopJSONString(prefix.String, "callID")
	text, _ := extractTopJSONString(prefix.String, "text")
	snapshot, _ := extractTopJSONString(prefix.String, "snapshot")
	var state sql.NullString
	if status, ok := extractTopJSONString(prefix.String, "status"); ok {
		raw, _ := json.Marshal(toolState{Status: status})
		state = sql.NullString{String: string(raw), Valid: true}
	}
	return b.partCapped(m.id, typ, text, tool, callID, state, snapshot), nil
}

// part maps one part row onto one or two canonical parts. The raw payload
// is kept on every non-tool part through Result.NormalizePart, so a field
// this package does not name is still stored, and a type it has never met
// is counted as unknown instead of dropped.
func (b *builder) part(partID, data string) []store.Part {
	raw := json.RawMessage(data)
	var d partData
	if err := json.Unmarshal(raw, &d); err != nil {
		return []store.Part{withForeignID(b.acc.NormalizePart("part", raw), partID)}
	}
	switch d.Type {
	case "text":
		return []store.Part{withForeignID(b.acc.NormalizePart(importer.PartText, raw), partID)}
	case "reasoning":
		// Reasoning is thinking under the canonical name: a reader
		// asking "did the model reason here" must get yes.
		return []store.Part{withForeignID(b.acc.NormalizePart(importer.PartThinking, raw), partID)}
	case "tool":
		return b.toolParts(partID, d, raw)
	case "file":
		return []store.Part{withForeignID(b.acc.NormalizePart(importer.PartFile, raw), partID)}
	case "patch":
		return []store.Part{withForeignID(b.acc.NormalizePart(importer.PartPatch, raw), partID)}
	case "step-start", "step-finish":
		// Step markers delimit work rather than carry it; the snapshot
		// hash stays in the payload for whoever wants it.
		return []store.Part{withForeignID(b.acc.NormalizePart(importer.PartStep, raw), partID)}
	default:
		return []store.Part{withForeignID(b.acc.NormalizePart(d.Type, raw), partID)}
	}
}

// partCapped maps an oversized part row whose raw blob was never
// materialised. Only the prefix-scanned fields are known, so the canonical
// part carries a minimal synthesized payload rather than the verbatim
// blob; routing and the tool call/result twin keep working.
func (b *builder) partCapped(partID, typ, text, tool, callID string, state sql.NullString, snapshot string) []store.Part {
	var st *toolState
	if state.Valid && state.String != "" {
		var s toolState
		if err := json.Unmarshal([]byte(state.String), &s); err == nil {
			st = &s
		}
	}
	d := partData{Type: typ, Text: text, Tool: tool, CallID: callID, State: st, Snapshot: snapshot}
	min, _ := json.Marshal(map[string]string{"type": typ, "capped": "part data exceeded 5MB, see source database"})
	raw := json.RawMessage(min)
	if typ == "tool" {
		return b.toolParts(partID, d, raw)
	}
	kind := typ
	switch typ {
	case "text":
		kind = importer.PartText
	case "reasoning":
		kind = importer.PartThinking
	case "file":
		kind = importer.PartFile
	case "patch":
		kind = importer.PartPatch
	case "step-start", "step-finish":
		kind = importer.PartStep
	}
	return []store.Part{withForeignID(b.acc.NormalizePart(kind, raw), partID)}
}

// toolParts splits one tool row into its canonical pair. The call always
// lands; the result only on a completed or error status — because a tool
// still running has returned nothing yet, and a result with no output
// would be a row that claims work happened. The two
// share the row's call id (falling back to the part id when the row names
// none), which is the only thing that pairs them, and both carry the part
// id as ForeignID. A status that is neither completed nor error twins
// nothing, per the contract — but when it is some other terminal state
// (not running, pending, or absent) it is classified under its own name
// so the row is visible in the result instead of silently a lone call.
func (b *builder) toolParts(partID string, d partData, raw json.RawMessage) []store.Part {
	callID := firstNonEmpty(d.CallID, partID)
	var input json.RawMessage
	var output json.RawMessage
	var status string
	if d.State != nil {
		input, output, status = d.State.Input, d.State.Output, d.State.Status
	}
	call := importer.NewToolCallPart(callID, d.Tool, input)
	call.ForeignID = partID
	parts := []store.Part{call}
	if status == "completed" || status == "error" {
		result := importer.NewToolResultPart(callID, output, status == "error")
		result.ForeignID = partID
		parts = append(parts, result)
		return parts
	}
	if status != "" && status != "running" && status != "pending" {
		b.acc.Classify("tool:" + status)
	}
	return parts
}

// thread assembles what was accumulated into the canonical shape.
func (b *builder) thread() store.Thread {
	th := store.Thread{
		Worktree: store.Worktree{Path: b.worktreePath, VCS: b.worktreeVCS, Name: baseName(b.worktreePath)},
		Session: store.Session{
			Directory: b.directory,
			Title:     b.title,
			Model:     b.model,
			Provider:  firstNonEmpty(b.provider, Harness),
			Harness:   Harness,
		},
		Binding: store.Binding{
			Provider:         importer.ProviderOpencode,
			Harness:          Harness,
			ForeignSessionID: b.ref.ID,
			ResumeCmd:        resumeCmd(b.ref.ID),
		},
		Messages: b.messages,
	}
	if b.parentID != "" && b.parentID != b.ref.ID {
		th.Parent = &store.Binding{
			Provider:         importer.ProviderOpencode,
			Harness:          Harness,
			ForeignSessionID: b.parentID,
			ResumeCmd:        resumeCmd(b.parentID),
		}
	}
	th.Turns = b.turns()
	return th
}

// turns splits the messages into turns and costs each one.
//
// The costing is span-wide rather than per-assistant-message: a turn is
// one prompt and everything the agent did about it, and a turn cost that
// only counted one message would understate the tool loops by an order of
// magnitude. Token columns sum the message tokens; the dollar cost sums
// message.data cost and lands in micros, rounded, because the transcript
// prices in dollars and the store costs in micros. CostUSDMicros stays nil
// for a span where no message carried a cost — the column is NULL, never a
// guessed zero. Status is "done" for
// every turn: the transcript records no cancellation this reader could
// tell from a finished turn, and guessing would be a guess.
func (b *builder) turns() []store.Turn {
	spans := importer.SplitTurns(b.candidates)
	turns := make([]store.Turn, 0, len(spans))
	for _, span := range spans {
		t := store.Turn{Status: "done"}
		var dollars float64
		hasCostAny := false
		for i := span.Start; i < span.End && i < len(b.tokens); i++ {
			tk := b.tokens[i]
			t.CostInput += tk.Input
			t.CostOutput += tk.Output
			t.CostReasoning += tk.Reasoning
			t.CostCacheRead += tk.Cache.Read
			t.CostCacheWrite += tk.Cache.Write
			t.CostTotal += tk.Input + tk.Output + tk.Reasoning + tk.Cache.Read + tk.Cache.Write
			if i < len(b.hasCost) && b.hasCost[i] {
				hasCostAny = true
			}
			if i < len(b.costs) {
				dollars += b.costs[i]
			}
		}
		if hasCostAny {
			micros := int64(math.Round(dollars * 1e6))
			t.CostUSDMicros = &micros
		}
		turns = append(turns, t)
	}
	return turns
}

// sessionModelOf reads the session-level model fallback: the model id and
// its vendor out of the session.model JSON blob, or nothing when the blob
// is absent or unreadable — the assistant messages name their model
// anyway, and this is only the backstop.
func sessionModelOf(raw string) (model, provider string) {
	if raw == "" {
		return "", ""
	}
	var m sessionModel
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", ""
	}
	return m.ID, m.ProviderID
}

// storable returns raw when it can go into a TEXT column as it stands and
// "" when it cannot, leaving the store to write its '{}' default. The
// judgement is importer.UsableJSON's — valid JSON is not enough, since
// JSON syntax admits bytes that are not UTF-8 and every reader downstream
// assumes they are — and the message's own raw_json still holds the blob
// either way.
func storable(raw json.RawMessage) string {
	if !importer.UsableJSON(raw) {
		return ""
	}
	return string(raw)
}

// rawJSON keeps the message data blob for the message row, or nothing when
// it is not text the column could hold.
func rawJSON(data string) *string {
	if !utf8.ValidString(data) {
		return nil
	}
	return &data
}

// withForeignID stamps a part with the part row it came from. Twins share
// one: the store dedups messages, not parts, so two parts naming one
// source row converge rather than collide.
func withForeignID(p store.Part, foreignID string) store.Part {
	p.ForeignID = foreignID
	return p
}

// resumeCmd is what a person types to get back into a session.
func resumeCmd(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	return "opencode run --session " + sessionID
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
