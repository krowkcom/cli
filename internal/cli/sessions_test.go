package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/store"
)

// seedSessionsHarness ingests two threads and returns their store ids:
// an Alpha claude thread with tools, and an untitled opencode thread.
func seedSessionsHarness(t *testing.T, h *harness) (alphaID, betaID string) {
	t.Helper()
	env := func(k string) string { return h.env[k] }
	db, err := store.Open(store.Env(env))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	w := store.NewWriter(db, nil)
	ctx := context.Background()
	alpha := store.Thread{
		Worktree: store.Worktree{Path: "/wt-a"},
		Session:  store.Session{Title: "Alpha thread", Model: "claude-fable-5-1", Provider: "anthropic", Harness: "claude"},
		Binding:  store.Binding{Provider: "anthropic", Harness: "claude", ForeignSessionID: "ses-alpha-foreign"},
		Turns:    []store.Turn{{Status: "closed", CostInput: 1000, CostOutput: 500, CostTotal: 1500}},
		Messages: []store.Message{
			{Role: store.RoleUser, Parts: []store.Part{{Type: "text", Data: `{"text":"do the thing"}`}}},
			{Role: store.RoleAssistant, Parts: []store.Part{
				{Type: "tool_call", ToolCallID: "call_1", Data: `{"name":"Bash","input":"ls"}`},
			}},
			{Role: store.RoleUser, Parts: []store.Part{
				{Type: "tool_result", ToolCallID: "call_1", Data: `{"output":"ok","is_error":false}`},
				{Type: "tool_result", ToolCallID: "call_gone", Data: `{"output":"x","is_error":true}`},
			}},
		},
	}
	if _, err := w.Ingest(ctx, alpha); err != nil {
		t.Fatalf("ingest alpha: %v", err)
	}
	beta := store.Thread{
		Worktree: store.Worktree{Path: "/wt-b"},
		Session:  store.Session{Model: "gpt-5", Provider: "openai", Harness: "opencode"},
		Binding:  store.Binding{Provider: "opencode", Harness: "opencode", ForeignSessionID: "ses-beta-foreign"},
		Turns:    []store.Turn{{Status: "closed"}},
		Messages: []store.Message{
			{Role: store.RoleUser, Parts: []store.Part{{Type: "text", Data: `{"text":"beta prompt here"}`}}},
		},
	}
	if _, err := w.Ingest(ctx, beta); err != nil {
		t.Fatalf("ingest beta: %v", err)
	}
	if err := db.QueryRow(`SELECT session_id FROM session_binding WHERE foreign_session_id = 'ses-alpha-foreign'`).Scan(&alphaID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT session_id FROM session_binding WHERE foreign_session_id = 'ses-beta-foreign'`).Scan(&betaID); err != nil {
		t.Fatal(err)
	}
	return alphaID, betaID
}

type sessionsListEnvelope struct {
	OK      bool   `json:"ok"`
	Summary string `json:"summary"`
	Data    struct {
		Sessions []struct {
			ID          string   `json:"id"`
			Title       string   `json:"title"`
			Harness     string   `json:"harness"`
			Model       string   `json:"model"`
			Turns       int      `json:"turns"`
			CostDisplay string   `json:"cost_display"`
			CostUSD     *float64 `json:"cost_usd"`
			TimeRel     string   `json:"time_updated_relative"`
			Worktree    string   `json:"worktree"`
			Foreign     string   `json:"foreign_session_id"`
		} `json:"sessions"`
	} `json:"data"`
}

func decodeSessionsList(t *testing.T, stdout string) sessionsListEnvelope {
	t.Helper()
	var e sessionsListEnvelope
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("not a sessions envelope: %v\n%s", err, stdout)
	}
	if !e.OK {
		t.Fatalf("envelope is not ok:\n%s", stdout)
	}
	return e
}

// Non-TTY `krowk sessions` never invokes the picker: isTTY=false answers
// with the JSON envelope, not a huh prompt.
func TestSessionsListNonTTY(t *testing.T) {
	h, _ := importHarness(t)
	seedSessionsHarness(t, h)

	code, stdout, stderr := h.run("sessions")
	if code != 0 {
		t.Fatalf("sessions exited %d, stderr:\n%s", code, stderr)
	}
	e := decodeSessionsList(t, stdout)
	if len(e.Data.Sessions) != 2 {
		t.Fatalf("listed %d sessions, want 2:\n%s", len(e.Data.Sessions), stdout)
	}
	// Order is by recency; ties fall back to row order, so find by title.
	byTitle := map[string]int{}
	for i, s := range e.Data.Sessions {
		byTitle[s.Title] = i
	}
	ai, ok := byTitle["Alpha thread"]
	if !ok {
		t.Fatalf("no Alpha thread row:\n%s", stdout)
	}
	bi, ok := byTitle["beta prompt here"]
	if !ok {
		t.Fatalf("no untitled-fallback row:\n%s", stdout)
	}
	alpha := e.Data.Sessions[ai]
	_ = bi
	if alpha.Harness != "claude" || alpha.Model != "claude-fable-5-1" {
		t.Errorf("alpha row = %+v, want claude/claude-fable-5-1", alpha)
	}
	if alpha.Turns != 1 {
		t.Errorf("alpha turns = %d, want 1", alpha.Turns)
	}
	if alpha.CostUSD == nil || !strings.HasPrefix(alpha.CostDisplay, "$") {
		t.Errorf("alpha cost = %v %q, want a priced value", alpha.CostUSD, alpha.CostDisplay)
	}
	if alpha.TimeRel == "" {
		t.Errorf("alpha row has no relative time")
	}
}

func TestSessionsListFilters(t *testing.T) {
	h, _ := importHarness(t)
	seedSessionsHarness(t, h)

	mustList := func(args ...string) sessionsListEnvelope {
		t.Helper()
		code, stdout, stderr := h.run(args...)
		if code != 0 {
			t.Fatalf("`krowk %s` exited %d, stderr:\n%s", strings.Join(args, " "), code, stderr)
		}
		return decodeSessionsList(t, stdout)
	}

	if e := mustList("sessions", "--harness", "claude"); len(e.Data.Sessions) != 1 || e.Data.Sessions[0].Title != "Alpha thread" {
		t.Errorf("--harness claude listed %+v, want only Alpha", e.Data.Sessions)
	}
	if e := mustList("sessions", "--worktree", "/wt-b"); len(e.Data.Sessions) != 1 || e.Data.Sessions[0].Harness != "opencode" {
		t.Errorf("--worktree /wt-b listed %+v, want only beta", e.Data.Sessions)
	}
	if e := mustList("sessions", "--limit", "1"); len(e.Data.Sessions) != 1 {
		t.Errorf("--limit 1 listed %d, want 1", len(e.Data.Sessions))
	}
	if e := mustList("sessions", "--limit", "1", "--all"); len(e.Data.Sessions) != 2 {
		t.Errorf("--all listed %d, want 2", len(e.Data.Sessions))
	}
	if err := h.fails("sessions", "--limit", "-1"); err["error"] != "bad_flag" {
		t.Errorf("--limit -1 gave %v, want bad_flag", err["error"])
	}
}

func TestSessionsListHumanTable(t *testing.T) {
	h, _ := importHarness(t)
	seedSessionsHarness(t, h)

	code, stdout, stderr := h.runOnStreams(false, false, "sessions", "--format", "human")
	if code != 0 {
		t.Fatalf("sessions --format human exited %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "Alpha thread") || !strings.Contains(stdout, "beta prompt here") {
		t.Errorf("human table misses a title:\n%s", stdout)
	}
	if !strings.Contains(stdout, "turns") {
		t.Errorf("human table has no turn counts:\n%s", stdout)
	}
}

// --json validates as an envelope and --jq reads both list and show.
func TestSessionsJSONAndJQ(t *testing.T) {
	h, _ := importHarness(t)
	alphaID, _ := seedSessionsHarness(t, h)

	code, stdout, stderr := h.run("sessions", "--json")
	if code != 0 {
		t.Fatalf("sessions --json exited %d, stderr:\n%s", code, stderr)
	}
	decodeSessionsList(t, stdout)

	code, stdout, stderr = h.run("sessions", "--jq", ".data.sessions | length")
	if code != 0 {
		t.Fatalf("sessions --jq exited %d, stderr:\n%s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "2" {
		t.Errorf("--jq length = %q, want 2", stdout)
	}

	code, stdout, stderr = h.run("sessions", "show", alphaID, "--json")
	if code != 0 {
		t.Fatalf("show --json exited %d, stderr:\n%s", code, stderr)
	}
	var e struct {
		OK   bool `json:"ok"`
		Data struct {
			ID       string `json:"id"`
			Messages []struct {
				Parts []struct {
					Type   string `json:"type"`
					Linked *bool  `json:"linked"`
				} `json:"parts"`
			} `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil || !e.OK {
		t.Fatalf("show is not an envelope: %v\n%s", err, stdout)
	}
	if e.Data.ID != alphaID {
		t.Errorf("show id = %q, want %q", e.Data.ID, alphaID)
	}

	code, stdout, stderr = h.run("sessions", "show", alphaID, "--jq", ".data.messages | length")
	if code != 0 {
		t.Fatalf("show --jq exited %d, stderr:\n%s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "3" {
		t.Errorf("show --jq messages length = %q, want 3", stdout)
	}
}

// show hydrates parts and re-links tools: every tool_result prints its
// tool_call name, and the orphan prints "unknown tool".
func TestSessionsShowTools(t *testing.T) {
	h, _ := importHarness(t)
	alphaID, _ := seedSessionsHarness(t, h)

	code, stdout, stderr := h.runOnStreams(false, false, "sessions", "show", alphaID, "--format", "human")
	if code != 0 {
		t.Fatalf("show exited %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "result (Bash)") {
		t.Errorf("show misses the linked tool name:\n%s", stdout)
	}
	if !strings.Contains(stdout, "unknown tool") {
		t.Errorf("show misses the orphan label:\n%s", stdout)
	}

	code, stdout, _ = h.run("sessions", "show", alphaID, "--json")
	if code != 0 {
		t.Fatalf("show --json exited %d", code)
	}
	var e struct {
		Data struct {
			Messages []struct {
				Parts []struct {
					Type     string `json:"type"`
					ToolName string `json:"tool_name"`
					Linked   *bool  `json:"linked"`
				} `json:"parts"`
			} `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatal(err)
	}
	var linked, orphan bool
	for _, m := range e.Data.Messages {
		for _, p := range m.Parts {
			if p.Type != "tool_result" {
				continue
			}
			if p.ToolName == "Bash" && p.Linked != nil && *p.Linked {
				linked = true
			}
			if p.ToolName == "unknown tool" && p.Linked != nil && !*p.Linked {
				orphan = true
			}
		}
	}
	if !linked {
		t.Errorf("JSON has no linked Bash result:\n%s", stdout)
	}
	if !orphan {
		t.Errorf("JSON has no linked:false orphan:\n%s", stdout)
	}
}

func TestSessionsShowThinking(t *testing.T) {
	h, _ := importHarness(t)
	env := func(k string) string { return h.env[k] }
	db, err := store.Open(store.Env(env))
	if err != nil {
		t.Fatal(err)
	}
	w := store.NewWriter(db, nil)
	_, err = w.Ingest(context.Background(), store.Thread{
		Worktree: store.Worktree{Path: "/wt-t"},
		Session:  store.Session{Title: "thinker"},
		Binding:  store.Binding{Provider: "anthropic", Harness: "claude", ForeignSessionID: "ses-think"},
		Messages: []store.Message{
			{Role: store.RoleAssistant, Parts: []store.Part{{Type: "thinking", Data: `{"thinking":"line one line two line three line four line five"}`}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := db.QueryRow(`SELECT session_id FROM session_binding WHERE foreign_session_id = 'ses-think'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	db.Close()

	code, collapsed, stderr := h.runOnStreams(false, false, "sessions", "show", id, "--format", "human")
	if code != 0 {
		t.Fatalf("show exited %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(collapsed, "use --thinking for all") {
		t.Errorf("thinking is not collapsed by default:\n%s", collapsed)
	}
	code, full, stderr := h.runOnStreams(false, false, "sessions", "show", id, "--format", "human", "--thinking")
	if code != 0 {
		t.Fatalf("show --thinking exited %d, stderr:\n%s", code, stderr)
	}
	if strings.Contains(full, "use --thinking for all") || !strings.Contains(full, "line five") {
		t.Errorf("--thinking did not expand:\n%s", full)
	}
}

// TestSessionsShowThinkingMultiline pins the --thinking scrub: line breaks
// survive (one thinking block stays multi-line) while ESC sequences die.
func TestSessionsShowThinkingMultiline(t *testing.T) {
	h, _ := importHarness(t)
	env := func(k string) string { return h.env[k] }
	db, err := store.Open(store.Env(env))
	if err != nil {
		t.Fatal(err)
	}
	w := store.NewWriter(db, nil)
	_, err = w.Ingest(context.Background(), store.Thread{
		Worktree: store.Worktree{Path: "/wt-tm"},
		Session:  store.Session{Title: "multiline thinker"},
		Binding:  store.Binding{Provider: "anthropic", Harness: "claude", ForeignSessionID: "ses-think-multi"},
		Messages: []store.Message{
			{Role: store.RoleAssistant, Parts: []store.Part{{Type: "thinking", Data: "{\"thinking\":\"first line\\u001b[31m\\nsecond line\"}"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	if err := db.QueryRow(`SELECT session_id FROM session_binding WHERE foreign_session_id = 'ses-think-multi'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	db.Close()

	code, full, stderr := h.runOnStreams(false, false, "sessions", "show", id, "--format", "human", "--thinking")
	if code != 0 {
		t.Fatalf("show --thinking exited %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(full, "first line[31m\nsecond line") {
		t.Errorf("--thinking folded newlines:\n%s", full)
	}
	if strings.Contains(full, "\x1b") {
		t.Errorf("--thinking leaked an escape:\n%q", full)
	}
}

// Id resolution through the CLI: unique prefix works, ambiguous errors
// naming both, foreign_session_id resolves via the binding.
func TestSessionsShowResolution(t *testing.T) {
	h, _ := importHarness(t)
	alphaID, _ := seedSessionsHarness(t, h)

	// Unique prefix of at least 8 chars.
	prefix := alphaID
	for n := 8; n < len(alphaID); n++ {
		code, _, _ := h.run("sessions", "show", alphaID[:n], "--json")
		if code == 0 {
			prefix = alphaID[:n]
			break
		}
	}
	code, stdout, stderr := h.run("sessions", "show", prefix, "--json")
	if code != 0 {
		t.Fatalf("show %q exited %d, stderr:\n%s", prefix, code, stderr)
	}
	if !strings.Contains(stdout, alphaID) {
		t.Errorf("prefix show misses the full id:\n%s", stdout)
	}

	// Foreign session id.
	code, stdout, stderr = h.run("sessions", "show", "ses-alpha-foreign", "--json")
	if code != 0 {
		t.Fatalf("show foreign id exited %d, stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, alphaID) {
		t.Errorf("foreign show misses the store id:\n%s", stdout)
	}

	// Ambiguous: two sessions sharing an 8-char prefix.
	env := func(k string) string { return h.env[k] }
	db, err := store.Open(store.Env(env))
	if err != nil {
		t.Fatal(err)
	}
	var wID string
	if err := db.QueryRow(`SELECT worktree_id FROM session LIMIT 1`).Scan(&wID); err != nil {
		t.Fatal(err)
	}
	amb := "0196ambig"
	idA, idB := amb+"aaaaaaaaaaaaaaaa", amb+"bbbbbbbbbbbbbbbb"
	for _, id := range []string{idA, idB} {
		if _, err := db.Exec(`INSERT INTO session (id, worktree_id, directory, title, time_created, time_updated) VALUES (?, ?, '/r', ?, 1, 2)`, id, wID, "t-"+id); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	errBody := h.fails("sessions", "show", amb)
	if errBody["error"] != "ambiguous_session" {
		t.Errorf("ambiguous prefix gave %v, want ambiguous_session", errBody["error"])
	}
	code, _, stderr = h.run("sessions", "show", amb)
	if code == 0 {
		t.Fatalf("ambiguous prefix succeeded, want a failure")
	}
	if !strings.Contains(stderr, idA) || !strings.Contains(stderr, idB) {
		t.Errorf("ambiguous error names neither candidate:\n%s", stderr)
	}

	// Unknown id.
	if errBody := h.fails("sessions", "show", "ses-nope"); errBody["error"] != "no_session" {
		t.Errorf("unknown id gave %v, want no_session", errBody["error"])
	}
	// Missing id.
	if errBody := h.fails("sessions", "show"); errBody["error"] != "no_session" {
		t.Errorf("missing id gave %v, want no_session", errBody["error"])
	}
}

// Sessions flags belong to their subcommand: import flags are refused on
// the list and vice versa.
func TestSessionsFlagOwnership(t *testing.T) {
	h, _ := importHarness(t)
	seedSessionsHarness(t, h)

	if err := h.fails("sessions", "--from", "all"); err["error"] != "bad_flag" {
		t.Errorf("--from on list gave %v, want bad_flag", err["error"])
	}
	if err := h.fails("sessions", "--thinking"); err["error"] != "bad_flag" {
		t.Errorf("--thinking on list gave %v, want bad_flag", err["error"])
	}
	if err := h.fails("sessions", "show", "x", "--harness", "claude"); err["error"] != "bad_flag" {
		t.Errorf("--harness on show gave %v, want bad_flag", err["error"])
	}
	if err := h.fails("push", h.fixture, "--harness", "claude"); err["error"] != "bad_flag" {
		t.Errorf("--harness on push gave %v, want bad_flag", err["error"])
	}
}

// `krowk help --json` knows sessions and sessions show.
func TestHelpJSONKnowsSessions(t *testing.T) {
	h := newHarness(t, 0)
	e := h.ok("help", "--json")
	_ = e
	code, stdout, stderr := h.run("help", "--json")
	if code != 0 {
		t.Fatalf("help --json exited %d, stderr:\n%s", code, stderr)
	}
	var c struct {
		Commands []struct {
			Name        string `json:"name"`
			Subcommands []struct {
				Name string `json:"name"`
			} `json:"subcommands"`
		} `json:"commands"`
	}
	if err := json.Unmarshal([]byte(stdout), &c); err != nil {
		t.Fatal(err)
	}
	var sessions *struct {
		Name        string `json:"name"`
		Subcommands []struct {
			Name string `json:"name"`
		} `json:"subcommands"`
	}
	for i := range c.Commands {
		if c.Commands[i].Name == "sessions" {
			sessions = &c.Commands[i]
		}
	}
	if sessions == nil {
		t.Fatalf("help --json has no sessions:\n%s", stdout)
	}
	for _, want := range []string{"show", "import"} {
		found := false
		for _, s := range sessions.Subcommands {
			if s.Name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("sessions has no %q subcommand", want)
		}
	}
}
