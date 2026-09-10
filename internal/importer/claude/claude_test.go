package claude

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/harness"
	"github.com/krowkcom/cli/internal/importer"
	"github.com/krowkcom/cli/internal/store"
)

// update regenerates the golden threads. A flag rather than an environment
// variable so that regenerating is something typed on purpose, the same way
// internal/cli's surface golden works.
var update = flag.Bool("update", false, "rewrite testdata/golden.json from the current import")

// The identifiers the fixture is built around, written out here so a test
// asserting on one is not asserting on a string it also produced.
const (
	fixtureSession  = "11111111-1111-4111-8111-111111111111"
	fixtureAgent    = "a0123456789abcdef"
	fixtureNoGitID  = "22222222-2222-4222-8222-222222222222"
	fixtureUnusedID = "44444444-4444-4444-8444-444444444444"
	fixtureSlug     = "-home-elvinas--buzz"
	fixtureNoGitDir = "-tmp-nogit"
	fixtureHomeDir  = "-home-elvinas"
)

// fixturePrompts is the number of real user prompts in the main fixture,
// counted by hand off the file: "first redacted prompt", "second redacted
// prompt" and "third redacted prompt". Everything else wearing the user
// role in there — the isMeta reminder, the tool_result-only line and the
// interrupt notice — is the protocol talking. A literal rather than a
// computed number on purpose: a test that derives the expected turn count
// from the same code that produces it proves nothing.
const fixturePrompts = 3

// fixtureLines is the line count of the main fixture, also by hand, so the
// accounting test has something to hold the four buckets against that did
// not come out of the reader.
const fixtureLines = 34

// The placeholders the fixture keeps in place of real paths, so a
// transcript checked into the repository names no directory on any
// particular machine and the golden stays machine-independent.
const (
	cwdPlaceholder   = "{{CWD}}"
	noGitPlaceholder = "{{NOGIT}}"
	repoToken        = "{{REPO}}"
	noGitToken       = "{{NOGIT_DIR}}"
	homeToken        = "{{HOME}}"
)

// fixture is one materialised copy of testdata: a home directory with the
// transcripts in it, a git checkout for them to have run in, and a plain
// directory for the session that ran outside one.
type fixture struct {
	env   harness.Env
	home  string
	repo  string
	sub   string
	noGit string
	// realHome is home with its symlinks resolved, which is what the
	// importer hands back: on macOS a temporary directory is reached
	// through /var -> /private/var, so replacing only the unresolved
	// spelling would leave a machine-specific path in the golden.
	realHome string
}

// newFixture writes testdata into a temporary home with the path
// placeholders filled in.
//
// The paths have to be real. The worktree rule is "walk up from cwd looking
// for .git", which is a question about the filesystem, and a fixture that
// mocked the filesystem out would be testing a mock. So the test builds the
// two shapes the rule has to tell apart — a directory inside a checkout and
// a directory that is not — and lets the importer answer for itself.
func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{
		home:  filepath.Join(root, "home"),
		repo:  filepath.Join(root, "repo"),
		noGit: filepath.Join(root, "plain"),
	}
	f.sub = filepath.Join(f.repo, "sub")
	f.env = func(k string) string {
		if k == "HOME" {
			return f.home
		}
		return ""
	}
	for _, dir := range []string{filepath.Join(f.repo, ".git"), f.sub, f.noGit} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll %s: %v", dir, err)
		}
	}

	src := filepath.Join("testdata", "home")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(f.home, undotted(rel))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out := strings.ReplaceAll(string(data), cwdPlaceholder, f.sub)
		out = strings.ReplaceAll(out, noGitPlaceholder, f.noGit)
		return os.WriteFile(dst, []byte(out), 0o600)
	})
	if err != nil {
		t.Fatalf("materialise fixture: %v", err)
	}
	// Resolved after the copy, because EvalSymlinks needs the directory to
	// exist. This is the spelling the importer hands back.
	real, err := filepath.EvalSymlinks(f.home)
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}
	f.realHome = real
	return f
}

// dottedDir is what testdata calls the directory Claude spells `.claude`.
//
// The rename is not cosmetic. The repository's .gitignore has a `.claude/`
// rule, meant for the working copy's own agent configuration, and it
// matches at any depth — so a fixture checked in under its real name is
// silently not checked in at all, and the tests pass for whoever wrote them
// and fail on a fresh clone. Storing it undotted and restoring the dot when
// the fixture is materialised keeps the file tracked and keeps the
// directory the importer sees exactly the one Claude writes. `git add -f`
// would have been the other way and is worse: it fixes one commit rather
// than the rule, and the next file added under testdata is ignored again.
const dottedDir = "dot-claude"

// undotted rewrites a testdata-relative path into the layout the importer
// expects, which is the same path with dottedDir spelled `.claude`.
func undotted(rel string) string {
	parts := strings.Split(rel, string(filepath.Separator))
	for i, p := range parts {
		if p == dottedDir {
			parts[i] = ".claude"
		}
	}
	return filepath.Join(parts...)
}

// unresolve puts the placeholders back, so a golden file holds
// "{{REPO}}/sub" rather than whatever /tmp directory this run happened to
// get. The repository is replaced before the plain directory only because
// doing it in one pass needs an order; the two share no prefix.
func (f fixture) unresolve(s string) string {
	s = strings.ReplaceAll(s, f.repo, repoToken)
	s = strings.ReplaceAll(s, f.noGit, noGitToken)
	if f.realHome != "" {
		s = strings.ReplaceAll(s, f.realHome, homeToken)
	}
	return strings.ReplaceAll(s, f.home, homeToken)
}

func (f fixture) read(t *testing.T, ref importer.Ref) (store.Thread, importer.Cursor, importer.Result) {
	t.Helper()
	th, cur, res, err := Source{}.Read(f.env, ref, nil)
	if err != nil {
		t.Fatalf("Read %s: %v", ref.ID, err)
	}
	return th, cur, res
}

// refByID finds one discovered ref, failing rather than returning a zero
// one: every caller below would otherwise read a ref with an empty path and
// get an unhelpful open error.
func refByID(t *testing.T, refs []importer.Ref, id string) importer.Ref {
	t.Helper()
	for _, r := range refs {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no ref %q in %+v", id, refs)
	return importer.Ref{}
}

func discover(t *testing.T, f fixture) []importer.Ref {
	t.Helper()
	refs, err := Source{}.Discover(f.env)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return refs
}

func TestDiscoverListsSessionsWithTheirSubagentsAfterThem(t *testing.T) {
	f := newFixture(t)
	refs := discover(t, f)

	var got []string
	for _, r := range refs {
		if r.Provider != importer.ProviderClaude {
			t.Fatalf("ref %s has provider %q", r.ID, r.Provider)
		}
		got = append(got, r.ID+" "+r.Path)
	}
	want := []string{
		fixtureUnusedID + " " + filepath.Join(projectsDir, fixtureHomeDir, fixtureUnusedID+".jsonl"),
		fixtureSession + " " + filepath.Join(projectsDir, fixtureSlug, fixtureSession+".jsonl"),
		fixtureAgent + " " + filepath.Join(projectsDir, fixtureSlug, fixtureSession, subagentsDir, "agent-"+fixtureAgent+".jsonl"),
		fixtureNoGitID + " " + filepath.Join(projectsDir, fixtureNoGitDir, fixtureNoGitID+".jsonl"),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Discover =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The import_state key is built off the ref, so it is worth pinning
	// that a subagent does not collide with its parent.
	if refs[1].Key() == refs[2].Key() {
		t.Fatalf("parent and subagent share the key %q", refs[1].Key())
	}
}

func TestDiscoverOnAMachineWithNoTranscripts(t *testing.T) {
	home := t.TempDir()
	env := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	refs, err := Source{}.Discover(env)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("Discover = %+v, want nothing", refs)
	}
}

// TestGolden is the whole import held against a checked-in canonical form.
// It is one assertion covering every field of every row, which is what
// makes the smaller tests below able to be about one thing each.
func TestGolden(t *testing.T) {
	f := newFixture(t)
	refs := discover(t, f)

	var threads []canonicalThread
	for _, ref := range refs {
		th, _, res := f.read(t, ref)
		threads = append(threads, canonicalise(f, ref, th, res))
	}
	encoded, err := json.MarshalIndent(threads, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	encoded = append(encoded, '\n')

	path := filepath.Join("testdata", "golden.json")
	if *update {
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — run `go test ./internal/importer/claude -run TestGolden -update` to create it", err)
	}
	if string(golden) != string(encoded) {
		t.Fatalf("the import no longer matches %s.\n\nIf the change is intended, regenerate it:\n  go test ./internal/importer/claude -run TestGolden -update\n\nfirst difference: %s",
			path, firstDifference(string(golden), string(encoded)))
	}
}

func firstDifference(golden, current string) string {
	was, now := strings.Split(golden, "\n"), strings.Split(current, "\n")
	for i := 0; i < len(was) || i < len(now); i++ {
		a, b := "", ""
		if i < len(was) {
			a = was[i]
		}
		if i < len(now) {
			b = now[i]
		}
		if a != b {
			return fmt.Sprintf("line %d\n  golden:  %s\n  current: %s", i+1, a, b)
		}
	}
	return "none"
}

// TestEveryLineIsAccountedFor is the acceptance the package doc is about: a
// transcript is not a list of messages, and the only way to know nothing was
// dropped is to add the buckets up and compare against the file.
func TestEveryLineIsAccountedFor(t *testing.T) {
	f := newFixture(t)
	ref := refByID(t, discover(t, f), fixtureSession)
	th, _, res := f.read(t, ref)

	classified := 0
	for _, n := range res.Classified {
		classified += n
	}
	total := len(th.Messages) + len(th.Events) + classified + res.SkippedCount
	if res.Lines != fixtureLines {
		t.Fatalf("read %d lines, fixture has %d", res.Lines, fixtureLines)
	}
	if total != fixtureLines {
		t.Fatalf("%d messages + %d events + %d classified + %d skipped = %d, want %d lines",
			len(th.Messages), len(th.Events), classified, res.SkippedCount, total, fixtureLines)
	}
	// The two lines that cannot be used are the one whose type is a number
	// and the one that is not JSON. Both are skips with reasons, not
	// silent drops.
	if res.SkippedCount != 2 {
		t.Fatalf("SkippedCount = %d, want 2", res.SkippedCount)
	}
	for _, s := range res.Skipped {
		if s.Reason == "" {
			t.Fatalf("skipped line %d has no reason", s.Line)
		}
	}
	// A line type this build has never met is classified by its own name,
	// which is what keeps it out of the unaccounted column.
	if res.Classified["future-thing"] != 1 {
		t.Fatalf("Classified = %v, want the unknown line type counted", res.Classified)
	}
	// So is an unrecognised content block, on the parts side.
	if res.Unknown != 1 || res.UnknownTypes["server_tool_use"] != 1 {
		t.Fatalf("Unknown = %d %v, want the one server_tool_use block", res.Unknown, res.UnknownTypes)
	}
}

func TestEveryPartTypeIsCanonical(t *testing.T) {
	f := newFixture(t)
	for _, ref := range discover(t, f) {
		th, _, _ := f.read(t, ref)
		for _, m := range th.Messages {
			for _, p := range m.Parts {
				if !importer.KnownPartType(p.Type) {
					t.Fatalf("%s: message %s emitted part type %q", ref.ID, m.ForeignID, p.Type)
				}
			}
		}
	}
}

// TestEveryToolResultHasItsCall is the pairing acceptance. Nothing in the
// schema joins a result to its call, so if the importer loses the id the
// loss is invisible until somebody tries to render a transcript.
func TestEveryToolResultHasItsCall(t *testing.T) {
	f := newFixture(t)
	for _, ref := range discover(t, f) {
		th, _, _ := f.read(t, ref)
		calls := map[string]bool{}
		results := 0
		for _, m := range th.Messages {
			for _, p := range m.Parts {
				switch p.Type {
				case importer.PartToolCall:
					if p.ToolCallID == "" {
						t.Fatalf("%s: tool_call with no id", ref.ID)
					}
					calls[p.ToolCallID] = true
				case importer.PartToolResult:
					results++
					if !calls[p.ToolCallID] {
						t.Fatalf("%s: tool_result %q has no earlier tool_call", ref.ID, p.ToolCallID)
					}
				}
			}
		}
		if ref.ID == fixtureSession || ref.ID == fixtureAgent {
			if results == 0 {
				t.Fatalf("%s: fixture was meant to contain a tool result", ref.ID)
			}
		}
	}
}

// TestASessionThatNeverNamedADirectory is the shape found on a real machine
// that nothing else covers: a transcript holding only furniture, with no
// cwd anywhere in it. It still has to ingest, and its worktree still must
// not be a guess at what the directory slug meant.
func TestASessionThatNeverNamedADirectory(t *testing.T) {
	f := newFixture(t)
	th, _, res := f.read(t, refByID(t, discover(t, f), fixtureUnusedID))

	if len(th.Messages) != 0 || len(th.Turns) != 0 {
		t.Fatalf("got %d messages and %d turns, want none", len(th.Messages), len(th.Turns))
	}
	if res.Lines != 2 || res.Classified["ai-title"] != 1 || res.Classified["agent-name"] != 1 {
		t.Fatalf("result = %+v, want both furniture lines classified", res)
	}
	if th.Session.Directory != "" {
		t.Fatalf("session directory = %q, want empty: the transcript never said", th.Session.Directory)
	}
	want := filepath.Join(f.realHome, projectsDir, fixtureHomeDir)
	if th.Worktree.Path != want {
		t.Fatalf("worktree path = %q, want the transcript's own directory %q", th.Worktree.Path, want)
	}
	if th.Worktree.VCS != vcsNone {
		t.Fatalf("worktree vcs = %q, want %q", th.Worktree.VCS, vcsNone)
	}
	if th.Session.Title != "Redacted unused session" {
		t.Fatalf("title = %q", th.Session.Title)
	}
}

// TestTurnCountIsTheNumberOfPrompts holds the turn rule against the
// hand-count. The leading span is the extra one: the isMeta reminder and
// the hook attachment came before anybody typed, and folding them into the
// first prompt would bill somebody else's work to it.
func TestTurnCountIsTheNumberOfPrompts(t *testing.T) {
	f := newFixture(t)
	ref := refByID(t, discover(t, f), fixtureSession)
	th, _, _ := f.read(t, ref)

	if got, want := len(th.Turns), fixturePrompts+1; got != want {
		t.Fatalf("got %d turns, want %d (%d prompts plus the leading span)", got, want, fixturePrompts)
	}
	for i, turn := range th.Turns {
		if turn.Status != "done" {
			t.Fatalf("turn %d has status %q", i, turn.Status)
		}
	}
}

// TestTurnCostsSumTheUsage reads the expected totals straight out of the
// fixture file rather than restating them, so the fixture and the assertion
// cannot drift apart — but it reads them with a different mechanism (a jq-ish
// walk of the raw lines) than the importer uses, so it is still a check.
func TestTurnCostsSumTheUsage(t *testing.T) {
	f := newFixture(t)
	ref := refByID(t, discover(t, f), fixtureSession)
	th, _, _ := f.read(t, ref)

	want := usageFromFile(t, filepath.Join(f.home, ref.Path))
	var got tokenUsage
	for _, turn := range th.Turns {
		got.Input += turn.CostInput
		got.Output += turn.CostOutput
		got.CacheRead += turn.CostCacheRead
		got.CacheWrite += turn.CostCacheWrite
		if turn.CostReasoning != 0 {
			t.Fatalf("turn carries %d reasoning tokens; Claude reports none", turn.CostReasoning)
		}
		if turn.CostUSDMicros != nil {
			t.Fatalf("turn carries a dollar cost; the transcript does not price a turn")
		}
		if sum := turn.CostInput + turn.CostOutput + turn.CostCacheRead + turn.CostCacheWrite; turn.CostTotal != sum {
			t.Fatalf("turn total %d != %d, the sum of its classes", turn.CostTotal, sum)
		}
	}
	if got != want {
		t.Fatalf("turn costs = %+v, fixture usage = %+v", got, want)
	}
}

// usageFromFile adds up every `message.usage` in a transcript, independently
// of the importer.
func usageFromFile(t *testing.T, path string) tokenUsage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var total tokenUsage
	for _, raw := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var l struct {
			Message struct {
				Usage tokenUsage `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			continue
		}
		u := l.Message.Usage
		total.Input += u.Input
		total.Output += u.Output
		total.CacheRead += u.CacheRead
		total.CacheWrite += u.CacheWrite
	}
	return total
}

// TestWorktreeComesFromCwdNotTheSlug is the reason this importer does not
// look at the directory name at all: the fixture's slug is
// `-home-elvinas--buzz`, which no transform turns back into a path.
func TestWorktreeComesFromCwdNotTheSlug(t *testing.T) {
	f := newFixture(t)
	refs := discover(t, f)

	th, _, _ := f.read(t, refByID(t, refs, fixtureSession))
	if th.Worktree.Path != f.repo {
		t.Fatalf("worktree path = %q, want the git toplevel %q", th.Worktree.Path, f.repo)
	}
	if th.Worktree.VCS != vcsGit {
		t.Fatalf("worktree vcs = %q, want %q", th.Worktree.VCS, vcsGit)
	}
	if th.Worktree.Name != filepath.Base(f.repo) {
		t.Fatalf("worktree name = %q", th.Worktree.Name)
	}
	if th.Session.Directory != f.sub {
		t.Fatalf("session directory = %q, want the cwd %q", th.Session.Directory, f.sub)
	}
	if strings.Contains(th.Worktree.Path, fixtureSlug) {
		t.Fatalf("worktree path %q was built from the slug", th.Worktree.Path)
	}

	nogit, _, _ := f.read(t, refByID(t, refs, fixtureNoGitID))
	if nogit.Worktree.Path != f.noGit || nogit.Worktree.VCS != vcsNone {
		t.Fatalf("outside a checkout: worktree = %+v, want %q with vcs %q", nogit.Worktree, f.noGit, vcsNone)
	}
}

func TestSessionNamesAnthropicAndBindsOnClaude(t *testing.T) {
	f := newFixture(t)
	th, _, _ := f.read(t, refByID(t, discover(t, f), fixtureSession))

	if th.Session.Provider != Provider || th.Session.Harness != Harness {
		t.Fatalf("session = %+v, want provider %q harness %q", th.Session, Provider, Harness)
	}
	// The binding key is the persisted one and stays importer's, whatever
	// the session says it ran on.
	if th.Binding.Provider != importer.ProviderClaude {
		t.Fatalf("binding provider = %q, want %q", th.Binding.Provider, importer.ProviderClaude)
	}
	if th.Binding.ForeignSessionID != fixtureSession {
		t.Fatalf("binding foreign session = %q", th.Binding.ForeignSessionID)
	}
	if want := "claude --resume " + fixtureSession; th.Binding.ResumeCmd != want {
		t.Fatalf("resume cmd = %q, want %q", th.Binding.ResumeCmd, want)
	}
	if th.Session.Title != "Redacted session title" {
		t.Fatalf("title = %q, want the ai-title line to have won", th.Session.Title)
	}
	for _, m := range th.Messages {
		if m.Provider != Provider {
			t.Fatalf("message %s has provider %q", m.ForeignID, m.Provider)
		}
	}
}

func TestRolesAndPartsOfTheFixture(t *testing.T) {
	f := newFixture(t)
	th, _, _ := f.read(t, refByID(t, discover(t, f), fixtureSession))

	byID := map[string]store.Message{}
	for _, m := range th.Messages {
		byID[m.ForeignID] = m
	}
	// The API error is an error, not something the model said.
	if got := byID["cccc0003-0000-4000-8000-000000000003"].Role; got != store.RoleError {
		t.Fatalf("isApiErrorMessage line has role %q, want %q", got, store.RoleError)
	}
	if got := byID["dddd0001-0000-4000-8000-000000000001"].Role; got != store.RoleSystem {
		t.Fatalf("system line has role %q", got)
	}
	// A thinking block keeps its signature in the column, not buried in
	// the payload.
	thinking := byID["cccc0001-0000-4000-8000-000000000001"].Parts[0]
	if thinking.Type != importer.PartThinking || thinking.Signature != "redactedsignature==" {
		t.Fatalf("thinking part = %+v", thinking)
	}
	// A user line whose content is a bare string still produces the same
	// text shape a block would.
	text := byID["bbbb0002-0000-4000-8000-000000000002"].Parts[0]
	if text.Type != importer.PartText || text.Data != `{"text":"first redacted prompt"}` {
		t.Fatalf("string-content part = %+v", text)
	}
	// The image block survives as an image, payload intact.
	image := byID["bbbb0004-0000-4000-8000-000000000004"].Parts[1]
	if image.Type != importer.PartImage || !strings.Contains(image.Data, "base64") {
		t.Fatalf("image part = %+v", image)
	}
	// Every message keeps the line it came from.
	for _, m := range th.Messages {
		if m.RawJSON == nil || !strings.HasPrefix(*m.RawJSON, "{") {
			t.Fatalf("message %s has no raw line", m.ForeignID)
		}
	}
}

// TestAttachmentsBecomeEventsOnlyWhenAHookFired pins the decision that keeps
// a third of a real transcript from being imported as user messages.
func TestAttachmentsBecomeEventsOnlyWhenAHookFired(t *testing.T) {
	f := newFixture(t)
	th, _, res := f.read(t, refByID(t, discover(t, f), fixtureSession))

	if len(th.Events) != 2 {
		t.Fatalf("got %d events, want the two hook attachments", len(th.Events))
	}
	for _, e := range th.Events {
		if e.Type != eventAttachment {
			t.Fatalf("event type = %q", e.Type)
		}
		if !strings.Contains(e.Data, `"hook_event"`) {
			t.Fatalf("event data = %s, want the hook named", e.Data)
		}
	}
	if !strings.Contains(th.Events[0].Data, "SessionStart") || !strings.Contains(th.Events[1].Data, "UserPromptSubmit") {
		t.Fatalf("events out of order or missing their hooks: %+v", th.Events)
	}
	// The hookless one is counted, not stored and not lost.
	if res.Classified["attachment"] != 1 {
		t.Fatalf("Classified = %v, want the hookless attachment counted", res.Classified)
	}
	for _, m := range th.Messages {
		if strings.Contains(derefString(m.RawJSON), `"type":"attachment"`) {
			t.Fatalf("an attachment was imported as a message: %s", m.ForeignID)
		}
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func TestSubagentBindsOnItsAgentAndNamesItsParent(t *testing.T) {
	f := newFixture(t)
	th, _, _ := f.read(t, refByID(t, discover(t, f), fixtureAgent))

	if th.Binding.ForeignSessionID != fixtureAgent {
		t.Fatalf("subagent binds on %q, want the agent id %q", th.Binding.ForeignSessionID, fixtureAgent)
	}
	if th.Parent == nil {
		t.Fatalf("subagent has no parent binding")
	}
	if th.Parent.Provider != importer.ProviderClaude || th.Parent.ForeignSessionID != fixtureSession {
		t.Fatalf("parent binding = %+v, want the session %q", th.Parent, fixtureSession)
	}
	// A subagent cannot be resumed on its own, so the command opens the
	// conversation that dispatched it.
	if want := "claude --resume " + fixtureSession; th.Binding.ResumeCmd != want {
		t.Fatalf("resume cmd = %q, want %q", th.Binding.ResumeCmd, want)
	}
	// One prompt in the file, one turn out of it: the task dispatch is a
	// prompt, the tool_result line is not.
	if len(th.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(th.Turns))
	}
	// And a parent transcript has no parent.
	parent, _, _ := f.read(t, refByID(t, discover(t, f), fixtureSession))
	if parent.Parent != nil {
		t.Fatalf("the parent session names a parent: %+v", parent.Parent)
	}
}

func TestReadRefusesACursorOfTheWrongKind(t *testing.T) {
	f := newFixture(t)
	ref := refByID(t, discover(t, f), fixtureSession)

	given := importer.SQLiteCursor{TimeUpdated: 7}
	_, back, _, err := Source{}.Read(f.env, ref, given)
	if err == nil {
		t.Fatal("Read accepted a SQLite cursor")
	}
	if !errorsIs(err, importer.ErrCursorType) {
		t.Fatalf("Read err = %v, want ErrCursorType", err)
	}
	// The cursor comes back untouched: nothing was read, so there is no
	// watermark of this source's own to report.
	if back != importer.Cursor(given) {
		t.Fatalf("cursor back = %+v, want the one given", back)
	}
}

// TestReadIgnoresTheCursorOffset is the documented consequence of turns
// being cumulative: a resumed read still produces the whole session.
func TestReadIgnoresTheCursorOffset(t *testing.T) {
	f := newFixture(t)
	ref := refByID(t, discover(t, f), fixtureSession)

	full, cur, _ := f.read(t, ref)
	jc, ok := cur.(importer.JSONLCursor)
	if !ok || jc.Offset == 0 {
		t.Fatalf("cursor = %+v, want a byte offset", cur)
	}
	info, err := os.Stat(filepath.Join(f.home, ref.Path))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if jc.Offset != info.Size() || jc.Size != info.Size() {
		t.Fatalf("cursor = %+v, want offset and size %d", jc, info.Size())
	}

	resumed, _, _, err := Source{}.Read(f.env, ref, jc)
	if err != nil {
		t.Fatalf("resumed Read: %v", err)
	}
	if len(resumed.Messages) != len(full.Messages) || len(resumed.Turns) != len(full.Turns) {
		t.Fatalf("resumed read got %d messages and %d turns, want %d and %d",
			len(resumed.Messages), len(resumed.Turns), len(full.Messages), len(full.Turns))
	}
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// canonicalThread is the golden's shape. It exists rather than marshalling
// store.Thread directly because store's structs have no json tags, so their
// encoding would be Go field names and every rename would rewrite the
// golden — and because the Result belongs in the golden too: the accounting
// is as much of the contract as the rows are.
type canonicalThread struct {
	Ref      canonicalRef      `json:"ref"`
	Worktree store.Worktree    `json:"worktree"`
	Session  canonicalSession  `json:"session"`
	Binding  canonicalBinding  `json:"binding"`
	Parent   *canonicalBinding `json:"parent"`
	Turns    []canonicalTurn   `json:"turns"`
	Events   []store.Event     `json:"events"`
	Messages []canonicalMsg    `json:"messages"`
	Result   canonicalResult   `json:"result"`
}

type canonicalRef struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Path     string `json:"path"`
	Key      string `json:"key"`
}

type canonicalSession struct {
	Directory string `json:"directory"`
	Title     string `json:"title"`
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Harness   string `json:"harness"`
}

type canonicalBinding struct {
	Provider         string `json:"provider"`
	Harness          string `json:"harness"`
	ForeignSessionID string `json:"foreign_session_id"`
	ResumeCmd        string `json:"resume_cmd"`
}

type canonicalTurn struct {
	Status         string `json:"status"`
	CostInput      int64  `json:"cost_input"`
	CostOutput     int64  `json:"cost_output"`
	CostTotal      int64  `json:"cost_total"`
	CostCacheRead  int64  `json:"cost_cache_read"`
	CostCacheWrite int64  `json:"cost_cache_write"`
	CostReasoning  int64  `json:"cost_reasoning"`
	CostUSDMicros  *int64 `json:"cost_usd_micros"`
}

type canonicalMsg struct {
	Role      string          `json:"role"`
	Provider  string          `json:"provider"`
	Model     string          `json:"model"`
	ForeignID string          `json:"foreign_id"`
	Usage     string          `json:"usage"`
	RawJSON   string          `json:"raw_json"`
	Parts     []canonicalPart `json:"parts"`
}

type canonicalPart struct {
	Type       string `json:"type"`
	ToolCallID string `json:"tool_call_id"`
	Signature  string `json:"signature"`
	Data       string `json:"data"`
}

type canonicalResult struct {
	Lines        int            `json:"lines"`
	Unknown      int            `json:"unknown"`
	UnknownTypes map[string]int `json:"unknown_types"`
	Classified   map[string]int `json:"classified"`
	SkippedCount int            `json:"skipped_count"`
	SkippedLines []int          `json:"skipped_lines"`
}

// canonicalise renders one import for the golden, with machine-specific
// paths turned back into placeholders. Skip reasons are reduced to line
// numbers on purpose: a reason is an error string from encoding/json, and
// pinning those would make the golden a test of the standard library's
// wording rather than of this package.
func canonicalise(f fixture, ref importer.Ref, th store.Thread, res importer.Result) canonicalThread {
	out := canonicalThread{
		Ref: canonicalRef{Provider: ref.Provider, ID: ref.ID, Path: ref.Path, Key: ref.Key()},
		Worktree: store.Worktree{
			Path: f.unresolve(th.Worktree.Path),
			VCS:  th.Worktree.VCS,
			Name: th.Worktree.Name,
		},
		Session: canonicalSession{
			Directory: f.unresolve(th.Session.Directory),
			Title:     th.Session.Title,
			Model:     th.Session.Model,
			Provider:  th.Session.Provider,
			Harness:   th.Session.Harness,
		},
		Binding: canonicalBinding{
			Provider:         th.Binding.Provider,
			Harness:          th.Binding.Harness,
			ForeignSessionID: th.Binding.ForeignSessionID,
			ResumeCmd:        th.Binding.ResumeCmd,
		},
		Turns:    []canonicalTurn{},
		Events:   []store.Event{},
		Messages: []canonicalMsg{},
		Result: canonicalResult{
			Lines:        res.Lines,
			Unknown:      res.Unknown,
			UnknownTypes: res.UnknownTypes,
			Classified:   res.Classified,
			SkippedCount: res.SkippedCount,
			SkippedLines: []int{},
		},
	}
	if th.Parent != nil {
		out.Parent = &canonicalBinding{
			Provider:         th.Parent.Provider,
			Harness:          th.Parent.Harness,
			ForeignSessionID: th.Parent.ForeignSessionID,
			ResumeCmd:        th.Parent.ResumeCmd,
		}
	}
	for _, t := range th.Turns {
		out.Turns = append(out.Turns, canonicalTurn{
			Status: t.Status, CostInput: t.CostInput, CostOutput: t.CostOutput,
			CostTotal: t.CostTotal, CostCacheRead: t.CostCacheRead,
			CostCacheWrite: t.CostCacheWrite, CostReasoning: t.CostReasoning,
			CostUSDMicros: t.CostUSDMicros,
		})
	}
	for _, e := range th.Events {
		out.Events = append(out.Events, store.Event{Type: e.Type, Data: f.unresolve(e.Data)})
	}
	for _, m := range th.Messages {
		cm := canonicalMsg{
			Role: string(m.Role), Provider: m.Provider, Model: m.Model,
			ForeignID: m.ForeignID, Usage: m.Usage,
			RawJSON: f.unresolve(derefString(m.RawJSON)),
			Parts:   []canonicalPart{},
		}
		for _, p := range m.Parts {
			cm.Parts = append(cm.Parts, canonicalPart{
				Type: p.Type, ToolCallID: p.ToolCallID,
				Signature: p.Signature, Data: f.unresolve(p.Data),
			})
		}
		out.Messages = append(out.Messages, cm)
	}
	for _, s := range res.Skipped {
		out.Result.SkippedLines = append(out.Result.SkippedLines, s.Line)
	}
	sort.Ints(out.Result.SkippedLines)
	return out
}

// TestALineWithNoUUIDGetsAStableForeignID covers the gap that would grow a
// session by a row per import: a message with no foreign id is appended
// unconditionally by the store, because the dedup is on
// (session_id, foreign_id) and NULL matches nothing.
func TestALineWithNoUUIDGetsAStableForeignID(t *testing.T) {
	f := newFixture(t)
	ref := refByID(t, discover(t, f), fixtureSession)
	th, _, _ := f.read(t, ref)

	var synthesised []string
	for _, m := range th.Messages {
		if m.ForeignID == "" {
			t.Fatalf("message with role %s has no foreign id at all", m.Role)
		}
		if strings.HasPrefix(m.ForeignID, ref.ID+":line:") {
			synthesised = append(synthesised, m.ForeignID)
		}
	}
	if len(synthesised) != 1 {
		t.Fatalf("synthesised ids = %v, want the one uuid-less line", synthesised)
	}
	// Stable across reads, which is the whole point: an id that moved
	// would dedup against nothing next time.
	again, _, _ := f.read(t, ref)
	if again.Messages[len(again.Messages)-1].ForeignID != th.Messages[len(th.Messages)-1].ForeignID {
		t.Fatal("foreign ids are not stable across reads")
	}
}

// TestNullContentIsNoPartsAndNoTurn is the other half of the turn rule
// being about prompts: a user line whose content is JSON null is not a
// person asking anything, and an empty text part standing in for it would
// open a turn and divide every per-turn cost by one too many.
func TestNullContentIsNoPartsAndNoTurn(t *testing.T) {
	f := newFixture(t)
	th, _, _ := f.read(t, refByID(t, discover(t, f), fixtureSession))

	var found bool
	for _, m := range th.Messages {
		if m.ForeignID == "bbbb0007-0000-4000-8000-000000000007" {
			found = true
			if len(m.Parts) != 0 {
				t.Fatalf("null content produced %d parts: %+v", len(m.Parts), m.Parts)
			}
		}
	}
	if !found {
		t.Fatal("the fixture no longer has a null-content user line")
	}
	// Still the hand-counted prompts plus the leading span: the null line
	// did not open one.
	if got, want := len(th.Turns), fixturePrompts+1; got != want {
		t.Fatalf("got %d turns, want %d", got, want)
	}
}

// TestEmptyStringContentIsNoParts is the same rule for the other spelling,
// held as a unit test because no transcript on the machine this was written
// against produces it and a fixture line would be inventing evidence.
func TestEmptyStringContentIsNoParts(t *testing.T) {
	var b builder
	for _, content := range []string{`null`, `""`, ``} {
		if got := b.parts([]byte(content)); len(got) != 0 {
			t.Fatalf("content %q produced %+v, want no parts", content, got)
		}
	}
	if got := b.parts([]byte(`"hello"`)); len(got) != 1 || got[0].Type != importer.PartText {
		t.Fatalf("content \"hello\" produced %+v", got)
	}
}

// TestAnAgentReportingBackIsNotAPrompt is the rule that keeps the turn
// count honest on a machine that dispatches subagents. A task notification
// arrives with the user role and real prose in it, and counting it as a
// prompt inflates the turn count and divides every per-turn cost by the
// same factor.
func TestAnAgentReportingBackIsNotAPrompt(t *testing.T) {
	f := newFixture(t)
	th, _, _ := f.read(t, refByID(t, discover(t, f), fixtureSession))

	var found bool
	for _, m := range th.Messages {
		if m.ForeignID == "bbbb0008-0000-4000-8000-000000000008" {
			found = true
			// It is still a message: the notification is transcript, and
			// dropping it would lose what the agent said.
			if m.Role != store.RoleUser || len(m.Parts) != 1 {
				t.Fatalf("task notification = %+v, want a one-part user message", m)
			}
		}
	}
	if !found {
		t.Fatal("the fixture no longer has a task-notification line")
	}
	if got, want := len(th.Turns), fixturePrompts+1; got != want {
		t.Fatalf("got %d turns, want %d: the task notification opened one", got, want)
	}
}

// TestInjectedLineRule holds the census's verdict directly, including the
// cases that look injected and are not. Marking `sdk` or `<command-name>`
// as machinery would throw away 901 and 65 real prompts respectively on
// the machine this was written against.
func TestInjectedLineRule(t *testing.T) {
	cases := []struct {
		name   string
		line   line
		text   string
		expect bool
	}{
		{"a person typing", line{PromptSource: "typed", Origin: &origin{Kind: "human"}}, "merge", false},
		{"a person through the sdk", line{PromptSource: "sdk"}, "Good to ship?", false},
		{"a person invoking a slash command", line{PromptSource: "sdk"}, "<command-name>/review</command-name>", false},
		{"a person in bash mode", line{}, "<bash-input>ls</bash-input>", false},
		{"a queued human prompt", line{PromptSource: "queued", Origin: &origin{Kind: "human"}}, "push", false},
		{"an old line with neither field", line{}, "what is this", false},
		{"an agent reporting back", line{PromptSource: "sdk", Origin: &origin{Kind: "task-notification"}}, "<task-notification>done</task-notification>", true},
		{"a coordinator", line{Origin: &origin{Kind: "coordinator"}}, "The coordinator sent a message", true},
		{"a peer agent", line{Origin: &origin{Kind: "peer"}}, "please re-run", true},
		{"the system prompting", line{PromptSource: "system"}, "anything", true},
		{"command output echoed back", line{}, "<local-command-stdout>ok</local-command-stdout>", true},
		{"bash output echoed back", line{}, "<bash-stdout>ok</bash-stdout>", true},
	}
	for _, c := range cases {
		if got := injected(c.line, c.text); got != c.expect {
			t.Errorf("%s: injected = %v, want %v", c.name, got, c.expect)
		}
	}
}

// TestRedactedThinkingIsStillThinking: internal/importer says the thinking
// part covers the redacted variant, so a reader asking "did the model
// reason here" gets yes whether or not it may see what about.
func TestRedactedThinkingIsStillThinking(t *testing.T) {
	f := newFixture(t)
	th, res := readSession(t, f)

	var found bool
	for _, m := range th.Messages {
		for _, p := range m.Parts {
			if strings.Contains(p.Data, "redacted_thinking") {
				found = true
				if p.Type != importer.PartThinking {
					t.Fatalf("redacted thinking landed as %q", p.Type)
				}
			}
		}
	}
	if !found {
		t.Fatal("the fixture no longer has a redacted_thinking block")
	}
	if res.UnknownTypes["redacted_thinking"] != 0 {
		t.Fatalf("redacted_thinking was counted as unknown: %v", res.UnknownTypes)
	}
}

// readSession is the main fixture read, for the tests that want both the
// thread and the accounting and nothing else.
func readSession(t *testing.T, f fixture) (store.Thread, importer.Result) {
	t.Helper()
	th, _, res := f.read(t, refByID(t, discover(t, f), fixtureSession))
	return th, res
}

// TestSystemContentThatIsNotAStringIsNotASkip: the field is a string on
// every transcript observed, and the day it is not must cost the content,
// not the line.
func TestSystemContentThatIsNotAStringIsNotASkip(t *testing.T) {
	var b builder
	if err := b.system(line{UUID: "u1", Content: []byte(`{"text":"structured"}`)}, []byte(`{}`), 1); err != nil {
		t.Fatalf("system: %v", err)
	}
	if len(b.messages) != 1 || len(b.messages[0].Parts) != 1 {
		t.Fatalf("messages = %+v", b.messages)
	}
	if got := b.messages[0].Parts[0]; got.Type != importer.PartUnknown || !strings.Contains(got.Data, "structured") {
		t.Fatalf("part = %+v, want an unknown carrying the payload", got)
	}
}

// TestReadKeepsTheCursorWhenItCannotOpenTheFile: a transcript that is
// momentarily unreadable has not invalidated the watermark, and handing
// back a zero one would make the next read re-import the whole session.
func TestReadKeepsTheCursorWhenItCannotOpenTheFile(t *testing.T) {
	f := newFixture(t)
	held := importer.JSONLCursor{Offset: 4096, Size: 8192}
	missing := importer.Ref{Provider: importer.ProviderClaude, ID: "gone", Path: filepath.Join(projectsDir, "-gone", "gone.jsonl")}

	_, back, _, err := Source{}.Read(f.env, missing, held)
	if err == nil {
		t.Fatal("Read of a missing transcript succeeded")
	}
	if back != importer.Cursor(held) {
		t.Fatalf("cursor back = %+v, want the one held %+v", back, held)
	}
}

// TestSubagentsAreFoundWithoutTheirParentFile: a session transcript can be
// deleted or rotated away and leave its subagents behind. Losing a whole
// conversation because the file naming it is gone is not a trade worth
// making.
func TestSubagentsAreFoundWithoutTheirParentFile(t *testing.T) {
	f := newFixture(t)
	parent := filepath.Join(f.home, projectsDir, fixtureSlug, fixtureSession+".jsonl")
	if err := os.Remove(parent); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	refs := discover(t, f)
	ref := refByID(t, refs, fixtureAgent)
	for _, r := range refs {
		if r.ID == fixtureSession {
			t.Fatal("the deleted session was still discovered")
		}
	}
	th, _, _ := f.read(t, ref)
	if th.Parent == nil || th.Parent.ForeignSessionID != fixtureSession {
		t.Fatalf("orphaned subagent parent = %+v, want %q", th.Parent, fixtureSession)
	}
}
