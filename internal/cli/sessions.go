package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/pricing"
	"github.com/krowkcom/cli/internal/runctx"
	"github.com/krowkcom/cli/internal/store"
	"github.com/krowkcom/cli/internal/termclean"
)

// defaultSessionLimit is the listing page when --limit is unset and --all
// is not passed. It aliases the store's default page so the picker gate and
// the query agree in one place.
const defaultSessionLimit = store.DefaultSessionPageSize

// sessionsList lists every thread on this machine from columns only: title,
// harness, model, turn count, priced cost and recency. Message/part blobs
// are never read here — select → show is where parts are hydrated.
//
// On a terminal showing human output the rows go to a picker that prints
// `krowk sessions show <id>`; anywhere else (piped, JSON, quiet, CI) the
// table (or envelope) is the answer and no picker ever appears.
func sessionsList(w io.Writer, f flags, format output.Format, env runctx.Env, colour, isTTY bool) error {
	if err := checkImportOS(); err != nil {
		return api.Fail("unsupported_os", unsupportedOSMessage)
	}
	limit, all, err := sessionLimit(f)
	if err != nil {
		return err
	}
	if all {
		limit = -1
	}
	// limit 0 is the flag set's unset value; store.ListSessions applies the
	// default page (store.DefaultSessionPageSize) in that one place.
	db, err := store.Open(store.Env(env))
	if err != nil {
		return api.Fail("store_unavailable", sanitizeStoreErr(err, store.DBPath(store.Env(env))))
	}
	defer db.Close()

	rows, err := store.ListSessions(db, f.harness, f.worktree, limit)
	if err != nil {
		return api.Fail("store_unavailable", sanitizeStoreErr(err, store.DBPath(store.Env(env))))
	}
	if format != output.Human {
		return emitSessionsList(w, format, f, rows)
	}
	// The picker builds one huh option per row: an unbounded --all page (or
	// any page past the default) would hang the TTY building options, so the
	// picker only fires on a paged list and the full page prints as a table.
	if interactive(f, format, env, isTTY) && !all && len(rows) > 0 && len(rows) <= defaultSessionLimit {
		id, err := pickSession(rows)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "krowk sessions show %s\n", id)
		return nil
	}
	fmt.Fprint(w, humanSessionsList(rows, colour, time.Now()))
	if len(rows) > 0 {
		fmt.Fprint(w, "\n")
	}
	return nil
}

// sessionLimit validates --limit/--all: --all wins over --limit, a negative
// limit is a mistake, and zero is "unset".
func sessionLimit(f flags) (int, bool, error) {
	if f.limit < 0 {
		return 0, false, api.Fail("bad_flag", "--limit is a maximum, so it cannot be negative")
	}
	return f.limit, f.all, nil
}

type sessionJSON struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Harness        string   `json:"harness"`
	Model          string   `json:"model"`
	Provider       string   `json:"provider"`
	Turns          int      `json:"turns"`
	CostUSD        *float64 `json:"cost_usd"`
	CostDisplay    string   `json:"cost_display"`
	TimeUpdatedMS  int64    `json:"time_updated_ms"`
	TimeUpdatedRel string   `json:"time_updated_relative"`
	Worktree       string   `json:"worktree"`
	ForeignSession string   `json:"foreign_session_id,omitempty"`
	Directory      string   `json:"directory,omitempty"`
}

type sessionsListJSON struct {
	Sessions []sessionJSON `json:"sessions"`
}

// sessionRowJSON renders one row, pricing the turn token aggregate at
// display time only. An unknown (provider, model) pair renders "—", never 0.
func sessionRowJSON(r store.SessionRow, now time.Time) sessionJSON {
	var cost *float64
	display := "—"
	if priced, ok := priceRow(r); ok {
		cost = &priced
		display = formatCost(priced)
	}
	return sessionJSON{
		ID: r.ID, Title: r.Title, Harness: r.Harness, Model: r.Model,
		Provider: r.Provider, Turns: r.TurnCount, CostUSD: cost,
		CostDisplay: display, TimeUpdatedMS: r.TimeUpdated,
		TimeUpdatedRel: relativeTime(r.TimeUpdated, now),
		Worktree:       r.WorktreePath, ForeignSession: r.ForeignSessionID,
		Directory: r.Directory,
	}
}

func emitSessionsList(w io.Writer, format output.Format, f flags, rows []store.SessionRow) error {
	now := time.Now()
	out := sessionsListJSON{Sessions: make([]sessionJSON, 0, len(rows))}
	for _, r := range rows {
		out.Sessions = append(out.Sessions, sessionRowJSON(r, now))
	}
	if f.quiet {
		return emit(w, encodeJSONValue(out), f)
	}
	return emit(w, encodeJSONValue(output.Envelope{
		OK: true, Data: out,
		Summary: fmt.Sprintf("%d sessions", len(rows)),
	}), f)
}

// humanSessionsList is the non-TTY table (and the TTY fallback when no
// picker applies): short id, title, harness, model, turns, cost, recency.
// Colour is gated on the colour flag so piped output stays byte-stable.
func humanSessionsList(rows []store.SessionRow, colour bool, now time.Time) string {
	if len(rows) == 0 {
		return "no sessions — run `krowk sessions import --from all`"
	}
	const maxTitleWidth = 60
	const maxHarnessWidth = 24
	const maxModelWidth = 32
	var tw, hw, mw int
	for _, r := range rows {
		title := cleanCell(r.Title)
		if title == "" {
			title = "(untitled)"
		}
		tw = max(tw, utf8.RuneCountInString(title))
		hw = max(hw, utf8.RuneCountInString(cleanCell(r.Harness)))
		mw = max(mw, utf8.RuneCountInString(cleanCell(r.Model)))
	}
	tw = min(max(tw, utf8.RuneCountInString("TITLE")), maxTitleWidth)
	hw = min(max(hw, utf8.RuneCountInString("HARNESS")), maxHarnessWidth)
	mw = min(max(mw, utf8.RuneCountInString("MODEL")), maxModelWidth)
	lines := make([]string, 0, len(rows)+1)
	lines = append(lines, strings.Join([]string{
		sessionPadRight(colour, "1;2", "ID", 8),
		sessionPadRight(colour, "1;2", "TITLE", tw),
		sessionPadRight(colour, "1;2", "HARNESS", hw),
		sessionPadRight(colour, "1;2", "MODEL", mw),
		sessionPadLeft(colour, "1;2", "TURNS", 9),
		sessionPadLeft(colour, "1;2", "COST", 10),
		sessionPaint(colour, "1;2", "UPDATED"),
	}, "  "))
	for _, r := range rows {
		title := cleanCell(r.Title)
		if title == "" {
			title = "(untitled)"
		}
		title = truncateRunes(title, maxTitleWidth)
		harness := truncateRunes(cleanCell(r.Harness), maxHarnessWidth)
		model := truncateRunes(cleanCell(r.Model), maxModelWidth)
		short := r.ID
		if rs := []rune(short); len(rs) > 8 {
			short = string(rs[:8])
		}
		turns := fmt.Sprintf("%3d", r.TurnCount) + " " + sessionPaint(colour, "2", "turns")
		cost := "—"
		costCode := "2"
		if priced, ok := priceRow(r); ok {
			cost = formatCost(priced)
			costCode = "32"
		}
		lines = append(lines, strings.Join([]string{
			sessionPadRight(false, "", short, 8),
			sessionPadRight(colour, "1", title, tw),
			sessionPadRight(colour, "2", harness, hw),
			sessionPadRight(colour, "2", model, mw),
			turns,
			sessionPadLeft(colour, costCode, cost, 10),
			sessionPaint(colour, "2", relativeTime(r.TimeUpdated, now)),
		}, "  "))
	}
	return strings.Join(lines, "\n")
}

// sessionPaint mirrors the output package's paint idiom: ANSI-wrap s in
// code only when colour is on, so colour=false stays byte-stable plain
// text for tests and pipes.
func sessionPaint(colour bool, code, s string) string {
	if !colour {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// sessionPadRight styles s then pads to w visible runes. Padding is
// computed from the raw rune count, so the ANSI codes never break column
// alignment.
func sessionPadRight(colour bool, code, s string, w int) string {
	styled := sessionPaint(colour, code, s)
	if pad := w - utf8.RuneCountInString(s); pad > 0 {
		styled += strings.Repeat(" ", pad)
	}
	return styled
}

// sessionPadLeft is the right-aligned twin of sessionPadRight, for the
// numeric COST column and its header.
func sessionPadLeft(colour bool, code, s string, w int) string {
	styled := sessionPaint(colour, code, s)
	if pad := w - utf8.RuneCountInString(s); pad > 0 {
		return strings.Repeat(" ", pad) + styled
	}
	return styled
}

func priceRow(r store.SessionRow) (float64, bool) {
	rates, ok := pricing.Price(r.Provider, pricing.Normalize(r.Provider, r.Model))
	if !ok {
		return 0, false
	}
	return rates.Cost(pricing.Tokens{
		Input: r.SumInput, Output: r.SumOutput,
		CacheRead: r.SumCacheRead, CacheWrite: r.SumCacheWrite,
		Reasoning: r.SumReasoning,
	}), true
}

func formatCost(usd float64) string {
	if usd < 0.01 {
		return fmt.Sprintf("$%.4f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}

// relativeTime turns epoch millis into "5m ago" beside the listing.
func relativeTime(ms int64, now time.Time) string {
	t := time.UnixMilli(ms)
	if t.After(now) {
		return "in the future"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy ago", int(d.Hours()/(24*365)))
	}
}

// sessionsShow renders one thread: turns → messages → parts in seq order.
// Tool_result parts print their tool_call twin's name; a result whose call
// is missing prints "unknown tool" (and linked:false in JSON). Thinking
// collapses to one line unless --thinking.
func sessionsShow(w io.Writer, args []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	if err := checkImportOS(); err != nil {
		return api.Fail("unsupported_os", unsupportedOSMessage)
	}
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return api.Fail("no_session", "pass the session: `krowk sessions show <id>`")
	}
	if len(args) > 1 {
		return api.Fail("bad_flag", "`krowk sessions show` takes one session id, got extra "+strings.Join(args[1:], " "))
	}
	db, err := store.Open(store.Env(env))
	if err != nil {
		return api.Fail("store_unavailable", sanitizeStoreErr(err, store.DBPath(store.Env(env))))
	}
	defer db.Close()

	id, err := store.ResolveSessionID(db, args[0])
	if err != nil {
		var amb *store.AmbiguousSessionError
		if errors.As(err, &amb) {
			return api.Fail("ambiguous_session", err.Error())
		}
		if errors.Is(err, sql.ErrNoRows) {
			return api.Fail("no_session", noSessionDetail(args[0]))
		}
		return api.Fail("store_unavailable", sanitizeStoreErr(err, store.DBPath(store.Env(env))))
	}
	d, err := store.LoadSessionDetail(db, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return api.Fail("no_session", fmt.Sprintf("no session %q", id))
		}
		return api.Fail("store_unavailable", sanitizeStoreErr(err, store.DBPath(store.Env(env))))
	}
	if format != output.Human {
		return emitSessionShow(w, f, d)
	}
	fmt.Fprint(w, humanSessionShow(d, f.thinking, colour, time.Now()))
	return nil
}

// noSessionDetail builds the miss message from the typed ref alone, without
// wrapping (and then stripping) the driver text ResolveSessionID carries
// underneath.
func noSessionDetail(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "pass the session: `krowk sessions show <id>`"
	}
	return fmt.Sprintf("%q matches no session — pass a full id, an id prefix of at least 8 chars, or a foreign session id", ref)
}

type showPartJSON struct {
	Seq        int    `json:"seq"`
	Type       string `json:"type"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	Linked     *bool  `json:"linked,omitempty"`
	Data       any    `json:"data"`
}

type showMessageJSON struct {
	Seq      int            `json:"seq"`
	Role     string         `json:"role"`
	Provider string         `json:"provider,omitempty"`
	Model    string         `json:"model,omitempty"`
	Parts    []showPartJSON `json:"parts"`
}

type showTurnJSON struct {
	Seq     int    `json:"seq"`
	Status  string `json:"status,omitempty"`
	Input   int64  `json:"input_tokens"`
	Output  int64  `json:"output_tokens"`
	Total   int64  `json:"total_tokens"`
	USD     *int64 `json:"cost_usd_micros,omitempty"`
	Unknown bool   `json:"cost_unknown,omitempty"`
}

type sessionShowJSON struct {
	ID             string            `json:"id"`
	Title          string            `json:"title"`
	Harness        string            `json:"harness"`
	Model          string            `json:"model"`
	Provider       string            `json:"provider"`
	Worktree       string            `json:"worktree"`
	Directory      string            `json:"directory,omitempty"`
	ForeignSession string            `json:"foreign_session_id,omitempty"`
	Turns          []showTurnJSON    `json:"turns"`
	Messages       []showMessageJSON `json:"messages"`
	PricedCostUSD  *float64          `json:"priced_cost_usd,omitempty"`
	CostDisplay    string            `json:"cost_display"`
	PricedWith     string            `json:"priced_with,omitempty"`
}

func emitSessionShow(w io.Writer, f flags, d store.SessionDetail) error {
	msgs := make([]showMessageJSON, 0, len(d.Messages))
	for _, m := range d.Messages {
		parts := make([]showPartJSON, 0, len(m.Parts))
		for _, p := range m.Parts {
			var data any
			var raw json.RawMessage
			if err := json.Unmarshal([]byte(p.Data), &raw); err != nil {
				data = p.Data
			} else {
				data = raw
			}
			sp := showPartJSON{Seq: p.Seq, Type: p.Type, Data: data}
			if p.ToolCallID != "" {
				sp.ToolCallID = p.ToolCallID
			}
			if p.Type == "tool_call" {
				if name := store.ToolNameOf(p.Data); name != "" {
					sp.ToolName = name
				}
			}
			if p.Type == "tool_result" {
				sp.ToolName = p.ToolName
				linked := p.Linked
				sp.Linked = &linked
			}
			parts = append(parts, sp)
		}
		msgs = append(msgs, showMessageJSON{
			Seq: m.Seq, Role: m.Role, Provider: m.Provider, Model: m.Model, Parts: parts,
		})
	}
	turns := make([]showTurnJSON, 0, len(d.Turns))
	for _, t := range d.Turns {
		st := showTurnJSON{Seq: t.Seq, Status: t.Status, Input: t.Input,
			Output: t.Output, Total: t.Total}
		if t.USDNull {
			st.Unknown = true
		} else {
			v := t.USDMicros
			st.USD = &v
		}
		turns = append(turns, st)
	}
	out := sessionShowJSON{
		ID: d.Session.ID, Title: d.Session.Title, Harness: d.Session.Harness,
		Model: d.Session.Model, Provider: d.Session.Provider,
		Worktree: d.Session.WorktreePath, Directory: d.Session.Directory, ForeignSession: d.Session.ForeignSessionID,
		Turns: turns, Messages: msgs, CostDisplay: "—",
	}
	if priced, ok := priceRow(d.Session); ok {
		out.PricedCostUSD = &priced
		out.CostDisplay = formatCost(priced)
		out.PricedWith = fmt.Sprintf("priced from embedded models.dev snapshot %s", pricing.SnapshotDate)
	}
	if f.quiet {
		return emit(w, encodeJSONValue(out), f)
	}
	return emit(w, encodeJSONValue(output.Envelope{
		OK: true, Data: out,
		Summary: fmt.Sprintf("%s — %d turns, %d messages", displayTitle(cleanCell(d.Session.Title)), len(turns), len(msgs)),
	}), f)
}

func displayTitle(t string) string {
	if t == "" {
		return "(untitled)"
	}
	return t
}

// humanSessionShow renders turns, then messages with their parts. Every
// stored string printed here is caller-controlled transcript text, so each
// passes through cleanCell before it reaches the terminal.
func humanSessionShow(d store.SessionDetail, showThinking bool, colour bool, now time.Time) string {
	var b strings.Builder
	title := displayTitle(cleanCell(d.Session.Title))
	fmt.Fprintf(&b, "%s\n", title)
	meta := cleanCell(d.Session.Harness)
	if m := cleanCell(d.Session.Model); m != "" {
		meta += "  " + m
	}
	if priced, ok := priceRow(d.Session); ok {
		meta += "  " + formatCost(priced)
	} else {
		meta += "  —"
	}
	meta += "  " + relativeTime(d.Session.TimeUpdated, now)
	if wp := cleanCell(d.Session.WorktreePath); wp != "" {
		meta += "\n" + wp
	}
	if dir := cleanCell(d.Session.Directory); dir != "" {
		meta += "\n" + dir
	}
	b.WriteString(meta + "\n")
	for _, t := range d.Turns {
		fmt.Fprintf(&b, "\nturn %d", t.Seq)
		if st := cleanCell(t.Status); st != "" {
			fmt.Fprintf(&b, "  %s", st)
		}
		fmt.Fprintf(&b, "  %d tokens", t.Total)
		if !t.USDNull {
			fmt.Fprintf(&b, "  %s", formatCost(float64(t.USDMicros)/1e6))
		}
		b.WriteString("\n")
	}
	for _, m := range d.Messages {
		fmt.Fprintf(&b, "\n[%s]\n", cleanCell(m.Role))
		for _, p := range m.Parts {
			b.WriteString(humanPart(p, showThinking) + "\n")
		}
	}
	_ = colour
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func humanPart(p store.PartDetail, showThinking bool) string {
	switch p.Type {
	case "text":
		return cleanCell(partTextString(p.Data))
	case "thinking":
		text := partThinkingString(p.Data)
		if showThinking {
			return "thinking: " + cleanLines(text)
		}
		one := truncateOneLine(text, 100)
		return "thinking: " + one + " (use --thinking for all)"
	case "tool_call":
		name := cleanCell(store.ToolNameOf(p.Data))
		if name == "" {
			name = "unknown tool"
		}
		input := partInputString(p.Data)
		if input != "" {
			return fmt.Sprintf("tool %s: %s", name, truncateOneLine(input, 200))
		}
		return "tool " + name
	case "tool_result":
		name := cleanCell(p.ToolName)
		if name == "" {
			name = "unknown tool"
		}
		out := partOutputString(p.Data)
		if out != "" {
			return fmt.Sprintf("result (%s): %s", name, truncateOneLine(out, 300))
		}
		return fmt.Sprintf("result (%s)", name)
	default:
		s := strings.Join(strings.Fields(p.Data), " ")
		return cleanCell(p.Type) + ": " + truncateOneLine(s, 200)
	}
}

func truncateOneLine(s string, n int) string {
	return truncateRunes(cleanCell(s), n)
}

// truncateRunes caps s at n runes, so a multi-byte character is never cut
// in half. cleanCell has already folded whitespace to single spaces.
func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		if n < 3 {
			return string(r[:n])
		}
		return string(r[:n-3]) + "..."
	}
	return s
}

// cleanCell is the CLI's name for the shared terminal scrubber.
func cleanCell(s string) string {
	return termclean.Cell(s)
}

// cleanLines scrubs caller-controlled text that keeps its line breaks
// (the --thinking path): each line passes through Cell, so ESC/bidi die
// but newlines survive.
func cleanLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = termclean.Cell(l)
	}
	return strings.Join(lines, "\n")
}

func partTextString(data string) string {
	var v struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return strings.Join(strings.Fields(data), " ")
	}
	return v.Text
}

func partThinkingString(data string) string {
	var v struct {
		Thinking string `json:"thinking"`
		Text     string `json:"text"`
	}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return strings.Join(strings.Fields(data), " ")
	}
	if v.Thinking != "" {
		return v.Thinking
	}
	return v.Text
}

func partInputString(data string) string {
	var v struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(data), &v); err != nil || len(v.Input) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(v.Input, &s); err == nil {
		return s
	}
	return string(v.Input)
}

func partOutputString(data string) string {
	var v struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal([]byte(data), &v); err != nil || len(v.Output) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(v.Output, &s); err == nil {
		return s
	}
	return string(v.Output)
}

// encodeJSONValue renders indented JSON without HTML escaping, like every
// other krowk answer.
func encodeJSONValue(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf(`{"ok":false,"error":{"error":"encode_failed","detail":%q}}`, err.Error())
	}
	return strings.TrimRight(buf.String(), "\n")
}
