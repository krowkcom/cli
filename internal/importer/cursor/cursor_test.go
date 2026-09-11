package cursor

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	fixtureSession = "11111111-1111-4111-8111-111111111111"
	fixtureMissing = "22222222-2222-4222-8222-222222222222"
	fixtureRepoID  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	missingSlug    = "missing-dir-xyz"
)

// fixtureLines is the line count of the main fixture, by hand, so the
// accounting test has something to hold the buckets against that did not
// come out of the reader: two user prompts, two assistant lines, one linked
// tool_result, one unlinked tool_result, one turn_ended, one garbage line.
const fixtureLines = 7

// fixtureMessages is the message count of the main fixture, also by hand:
// every role-carrying line above except the turn_ended and the garbage.
const fixtureMessages = 5

// The placeholders the golden keeps in place of real paths, so the fixture
// names no directory on any particular machine.
const (
	slugToken = "{{SLUG}}"
	workToken = "{{WORK}}"
	homeToken = "{{HOME}}"
)

// fixture is one materialised copy of testdata: a home directory with the
// transcripts in it, and a plain directory for the slug that decodes to a
// real checkout.
type fixture struct {
	env  harness.Env
	home string
	// work is the directory the main slug decodes to. It is built without
	// dashes — t.TempDir names none — so "/" + ReplaceAll(slug, "-", "/")
	// round-trips exactly.
	work string
	slug string
}

// newFixture writes testdata into a temporary home.
//
// The {{SLUG}} directory is renamed to the slug that decodes to the work
// directory created alongside it. The worktree rule is "decode, then believe
// the filesystem", which is a question about the filesystem, and a fixture
// that mocked it out would be testing a mock. So the test builds the two
// shapes the rule has to tell apart — a slug decoding to a directory that
// exists, and one decoding to nothing — and lets the importer answer.
func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{
		home: filepath.Join(root, "home"),
		work: filepath.Join(root, "work"),
	}
	f.slug = strings.ReplaceAll(strings.TrimPrefix(f.work, "/"), "/", "-")
	f.env = func(k string) string {
		if k == "HOME" {
			return f.home
		}
		return ""
	}
	if err := os.MkdirAll(f.work, 0o700); err != nil {
		t.Fatalf("MkdirAll %s: %v", f.work, err)
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
		dst := filepath.Join(f.home, strings.ReplaceAll(rel, slugToken, f.slug))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o600)
	})
	if err != nil {
		t.Fatalf("materialise fixture: %v", err)
	}
	return f
}

// unresolve puts the placeholders back, so a golden file holds "{{WORK}}"
// rather than whatever /tmp directory this run happened to get.
func (f fixture) unresolve(s string) string {
	s = strings.ReplaceAll(s, f.work, workToken)
	s = strings.ReplaceAll(s, f.slug, slugToken)
	return strings.ReplaceAll(s, f.home, homeToken)
}

func (f fixture) read(t *testing.T, ref importer.Ref, cursor importer.Cursor) (store.Thread, importer.Cursor, importer.Result) {
	t.Helper()
	th, cur, res, err := Source{}.Read(f.env, ref, cursor)
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

func TestDiscoverListsSessionsSorted(t *testing.T) {
	f := newFixture(t)
	refs := discover(t, f)

	var got []string
	for _, r := range refs {
		if r.Provider != importer.ProviderCursor {
			t.Fatalf("ref %s has provider %q", r.ID, r.Provider)
		}
		got = append(got, r.ID+" "+r.Path)
	}
	want := []string{
		fixtureSession + " " + filepath.Join(projectsDir, f.slug, transcriptsDir, fixtureSession, fixtureSession+".jsonl"),
		fixtureMissing + " " + filepath.Join(projectsDir, missingSlug, transcriptsDir, fixtureMissing, fixtureMissing+".jsonl"),
	}
	// Discover sorts by slug, not by session id: the want list is ordered
	// the same way, so the assertion pins the order rather than the set.
	if missingSlug < f.slug {
		want[0], want[1] = want[1], want[0]
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Discover =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The import_state key is built off the ref, so it is worth pinning
	// that the two sessions do not collide.
	if refs[0].Key() == refs[1].Key() {
		t.Fatalf("both sessions share the key %q", refs[0].Key())
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
// It is one assertion covering every field of every row, which is what makes
// the smaller tests below able to be about one thing each.
func TestGolden(t *testing.T) {
	f := newFixture(t)
	refs := discover(t, f)

	var threads []canonicalThread
	for _, ref := range refs {
		th, _, res := f.read(t, ref, nil)
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
		t.Fatalf("%v — run `go test ./internal/importer/cursor -run TestGolden -update` to create it", err)
	}
	if string(golden) != string(encoded) {
		t.Fatalf("the import no longer matches %s.\n\nIf the change is intended, regenerate it:\n  go test ./internal/importer/cursor -run TestGolden -update\n\nfirst difference: %s",
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

// TestEveryLineIsAccountedFor is the acceptance the package doc is about:
// every line lands in a message, in Classified, or in Skipped, and those
// three add up to the line count with nothing left over. The repo.json event
// is held apart on purpose: it is not a line, so folding it into the sum
// would make the accounting prove nothing.
func TestEveryLineIsAccountedFor(t *testing.T) {
	f := newFixture(t)
	ref := refByID(t, discover(t, f), fixtureSession)
	th, _, res := f.read(t, ref, nil)

	classified := 0
	for _, n := range res.Classified {
		classified += n
	}
	if res.Lines != fixtureLines {
		t.Fatalf("read %d lines, fixture has %d", res.Lines, fixtureLines)
	}
	// Only turn_ended is line-level furniture here: tool_result:unlinked is
	// classified against a line that ALSO became a message, so it rides
	// outside the sum — pinned separately in the linkage test.
	if total := len(th.Messages) + res.Classified["turn_ended"] + res.SkippedCount; total != fixtureLines {
		t.Fatalf("%d messages + %d turn_ended + %d skipped = %d, want %d lines (all classified: %v)",
			len(th.Messages), res.Classified["turn_ended"], res.SkippedCount, total, fixtureLines, res.Classified)
	}
	if len(th.Messages) != fixtureMessages {
		t.Fatalf("messages = %d, want %d", len(th.Messages), fixtureMessages)
	}
	// The turn_ended line is furniture, counted under its own type.
	if res.Classified["turn_ended"] != 1 {
		t.Fatalf("Classified = %v, want the turn_ended line counted", res.Classified)
	}
	// The one line that cannot be used is the one that is not JSON. It is a
	// skip with a reason, not a silent drop.
	if res.SkippedCount != 1 {
		t.Fatalf("SkippedCount = %d, want 1", res.SkippedCount)
	}
	for _, s := range res.Skipped {
		if s.Reason == "" {
			t.Fatalf("skipped line %d has no reason", s.Line)
		}
	}
	// The sidecar is one event, and it is not a line: asserting it here pins
	// that the accounting above did not swallow it.
	if len(th.Events) != 1 || th.Events[0].Type != eventRepo {
		t.Fatalf("events = %+v, want the one cursor_repo event", th.Events)
	}
	if !strings.Contains(th.Events[0].Data, fixtureRepoID) {
		t.Fatalf("event data = %s, want the repo id %q", th.Events[0].Data, fixtureRepoID)
	}
}

func TestEveryPartTypeIsCanonical(t *testing.T) {
	f := newFixture(t)
	for _, ref := range discover(t, f) {
		th, _, _ := f.read(t, ref, nil)
		for _, m := range th.Messages {
			for _, p := range m.Parts {
				if !importer.KnownPartType(p.Type) {
					t.Fatalf("%s: message with role %s emitted part type %q", ref.ID, m.Role, p.Type)
				}
			}
		}
	}
}

// TestEveryToolResultLinksOrIsCounted is the pairing acceptance. Nothing in
// the schema joins a result to its call, so if the importer loses the id the
// loss is invisible until somebody tries to render a transcript. The fixture
// holds one of each: a result linked to the tool_use on the line before it,
// and an orphan counted as unlinked and still emitted.
func TestEveryToolResultLinksOrIsCounted(t *testing.T) {
	f := newFixture(t)
	ref := refByID(t, discover(t, f), fixtureSession)
	th, _, res := f.read(t, ref, nil)

	calls := map[string]bool{}
	var results []string
	for _, m := range th.Messages {
		for _, p := range m.Parts {
			switch p.Type {
			case importer.PartToolCall:
				if p.ToolCallID == "" {
					t.Fatal("tool_call with no id")
				}
				calls[p.ToolCallID] = true
			case importer.PartToolResult:
				results = append(results, p.ToolCallID)
			}
		}
	}
	if len(results) != 2 {
		t.Fatalf("got %d tool_results, want the linked one and the orphan", len(results))
	}
	if !calls[results[0]] {
		t.Fatalf("tool_result %q has no earlier tool_call", results[0])
	}
	if calls[results[1]] {
		t.Fatalf("tool_result %q links, the fixture meant it orphaned", results[1])
	}
	if res.Classified["tool_result:unlinked"] != 1 {
		t.Fatalf("Classified = %v, want the orphan counted once", res.Classified)
	}
	// The id-less tool_use is position-keyed: the second line of the read.
	if !calls["cursor:2"] {
		t.Fatalf("calls = %v, want the synthesised cursor:2 id", calls)
	}
}

// TestTurnCountIsTheNumberOfPrompts holds the turn rule against the
// hand-count: two user prompts, two turns. The tool_result-only assistant
// lines open none — a result is the protocol talking, not a person.
func TestTurnCountIsTheNumberOfPrompts(t *testing.T) {
	f := newFixture(t)
	ref := refByID(t, discover(t, f), fixtureSession)
	th, _, _ := f.read(t, ref, nil)

	if len(th.Turns) != 2 {
		t.Fatalf("got %d turns, want 2 for the two prompts", len(th.Turns))
	}
	for i, turn := range th.Turns {
		if turn.Status != "done" {
			t.Fatalf("turn %d has status %q", i, turn.Status)
		}
	}
}

// TestTimestampsStayInTheText pins that the <timestamp> tags opening user
// prose are left where they were found, not parsed into a time.
func TestTimestampsStayInTheText(t *testing.T) {
	f := newFixture(t)
	ref := refByID(t, discover(t, f), fixtureSession)
	th, _, _ := f.read(t, ref, nil)

	var first string
	for _, m := range th.Messages {
		if m.Role == store.RoleUser && len(m.Parts) > 0 {
			for _, p := range m.Parts {
				if p.Type == importer.PartText {
					first = p.Data
				}
			}
			break
		}
	}
	// Data is JSON-escaped, so the tags are matched without their brackets:
	// what matters is they survived verbatim instead of being parsed away.
	if !strings.Contains(first, "timestamp") || !strings.Contains(first, "user_query") {
		t.Fatalf("first user text = %s, want the tags left in", first)
	}
}

func TestWorktreeComesFromTheSlugOnlyIfItExists(t *testing.T) {
	f := newFixture(t)
	refs := discover(t, f)

	th, _, _ := f.read(t, refByID(t, refs, fixtureSession), nil)
	if th.Worktree.Path != f.work {
		t.Fatalf("worktree path = %q, want the decoded directory %q", th.Worktree.Path, f.work)
	}
	if th.Worktree.VCS != vcsNone {
		t.Fatalf("worktree vcs = %q, want %q for a directory with no .git", th.Worktree.VCS, vcsNone)
	}
	if th.Worktree.Name != filepath.Base(f.work) {
		t.Fatalf("worktree name = %q", th.Worktree.Name)
	}
	if th.Session.Directory != "" {
		t.Fatalf("session directory = %q, want empty: the transcript never names one", th.Session.Directory)
	}

	missing, _, res := f.read(t, refByID(t, refs, fixtureMissing), nil)
	if want := "cursor:" + missingSlug; missing.Worktree.Path != want {
		t.Fatalf("worktree path = %q, want the fallback %q", missing.Worktree.Path, want)
	}
	if missing.Worktree.VCS != vcsNone {
		t.Fatalf("worktree vcs = %q, want %q", missing.Worktree.VCS, vcsNone)
	}
	if missing.Worktree.Name != "cursor:"+missingSlug {
		t.Fatalf("worktree name = %q, want the slug fallback spelled whole", missing.Worktree.Name)
	}
	if res.Classified["worktree-fallback"] != 1 {
		t.Fatalf("Classified = %v, want the fallback counted", res.Classified)
	}
	// No repo.json beside that slug's transcripts: no event, not an error.
	if len(missing.Events) != 0 {
		t.Fatalf("events = %+v, want none without a sidecar", missing.Events)
	}
}

// TestWorktreeOfDetectsGit pins the vcs half of the rule directly: a
// directory holding a .git entry resolves git, one holding nothing resolves
// none. The shared fixture stays .git-free on purpose — adding one there
// would flip the existing vcsNone assertion and the golden.
func TestWorktreeOfDetectsGit(t *testing.T) {
	git := t.TempDir()
	if err := os.Mkdir(filepath.Join(git, ".git"), 0o700); err != nil {
		t.Fatalf("Mkdir .git: %v", err)
	}
	slug := strings.ReplaceAll(strings.TrimPrefix(git, "/"), "/", "-")
	if path, vcs := worktreeOf(slug); path != git || vcs != vcsGit {
		t.Fatalf("worktreeOf(%q) = (%q, %q), want (%q, git)", slug, path, vcs, git)
	}

	plain := t.TempDir()
	pslug := strings.ReplaceAll(strings.TrimPrefix(plain, "/"), "/", "-")
	if path, vcs := worktreeOf(pslug); path != plain || vcs != vcsNone {
		t.Fatalf("worktreeOf(%q) = (%q, %q), want (%q, none)", pslug, path, vcs, plain)
	}
}

func TestSessionNamesCursorAndBindsOnIt(t *testing.T) {
	f := newFixture(t)
	th, _, _ := f.read(t, refByID(t, discover(t, f), fixtureSession), nil)

	if th.Session.Provider != Provider || th.Session.Harness != Harness {
		t.Fatalf("session = %+v, want provider %q harness %q", th.Session, Provider, Harness)
	}
	if th.Binding.Provider != importer.ProviderCursor {
		t.Fatalf("binding provider = %q, want %q", th.Binding.Provider, importer.ProviderCursor)
	}
	if th.Binding.ForeignSessionID != fixtureSession {
		t.Fatalf("binding foreign session = %q", th.Binding.ForeignSessionID)
	}
	if th.Binding.ResumeCmd != "" {
		t.Fatalf("resume cmd = %q, want empty: resuming is unknown in v1", th.Binding.ResumeCmd)
	}
	if th.Parent != nil {
		t.Fatalf("session names a parent: %+v", th.Parent)
	}
	// Messages carry no foreign id: the store appends them, and the cursor
	// is the only dedup. Pinning it here keeps a future id from silently
	// changing the ingest contract.
	for _, m := range th.Messages {
		if m.ForeignID != "" {
			t.Fatalf("message with role %s carries foreign id %q", m.Role, m.ForeignID)
		}
		if m.Provider != Provider {
			t.Fatalf("message has provider %q", m.Provider)
		}
		if m.RawJSON == nil || !strings.HasPrefix(*m.RawJSON, "{") {
			t.Fatal("a message lost the line it came from")
		}
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
	if !errors.Is(err, importer.ErrCursorType) {
		t.Fatalf("Read err = %v, want ErrCursorType", err)
	}
	// The cursor comes back untouched: nothing was read, so there is no
	// watermark of this source's own to report.
	if back != importer.Cursor(given) {
		t.Fatalf("cursor back = %+v, want the one given", back)
	}
}

// TestReadHonorsTheCursor is the delta contract the package doc is about:
// messages carry no foreign id and the store appends them, so a resumed read
// must return only new lines rather than the whole file.
func TestReadHonorsTheCursor(t *testing.T) {
	f := newFixture(t)
	ref := refByID(t, discover(t, f), fixtureSession)

	full, cur, _ := f.read(t, ref, nil)
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

	// Nothing appended: a resumed read is empty, not a second copy.
	delta, _, _, err := Source{}.Read(f.env, ref, jc)
	if err != nil {
		t.Fatalf("resumed Read: %v", err)
	}
	if len(delta.Messages) != 0 {
		t.Fatalf("resumed read got %d messages, want none", len(delta.Messages))
	}

	// One line appended: the delta is exactly it.
	appendLine(t, filepath.Join(f.home, ref.Path),
		`{"role":"user","message":{"content":[{"type":"text","text":"third redacted prompt"}]}}`+"\n")
	grown, _, _, err := Source{}.Read(f.env, ref, jc)
	if err != nil {
		t.Fatalf("grown Read: %v", err)
	}
	if len(grown.Messages) != 1 {
		t.Fatalf("grown read got %d messages, want the appended one", len(grown.Messages))
	}
	// The delta's turn is computed over the delta: one prompt, one span,
	// rather than the session's three.
	if len(grown.Turns) != 1 {
		t.Fatalf("grown read got %d turns, want 1 over the delta", len(grown.Turns))
	}
	_ = full
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = fh.Close() }()
	if _, err := fh.WriteString(line); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
}

// adhocTranscript materialises lines as one transcript under a throwaway
// home and returns the env, ref and transcript path to read it through.
//
// It exists so the tests below do not touch the shared fixture: each builds
// the smallest file its case needs. refPath is slash-joined under home —
// the .cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl shape for
// cases that need a sidecar, any other shape for the one that must not have
// one. A non-empty repoID writes {"id": ...} to the repo.json beside
// agent-transcripts, the same seat repoEvent reads.
func adhocTranscript(t *testing.T, refPath, id string, lines []string, repoID string) (harness.Env, importer.Ref, string) {
	t.Helper()
	home := t.TempDir()
	env := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	full := filepath.Join(home, filepath.FromSlash(refPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(full), err)
	}
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l)
		sb.WriteString("\n")
	}
	if err := os.WriteFile(full, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("WriteFile %s: %v", full, err)
	}
	if repoID != "" {
		sidecar := filepath.Join(home, filepath.FromSlash(filepath.Dir(filepath.Dir(filepath.Dir(refPath)))), "repo.json")
		if err := os.MkdirAll(filepath.Dir(sidecar), 0o700); err != nil {
			t.Fatalf("MkdirAll %s: %v", filepath.Dir(sidecar), err)
		}
		data := `{"id": ` + strconv.Quote(repoID) + `}` + "\n"
		if err := os.WriteFile(sidecar, []byte(data), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", sidecar, err)
		}
	}
	return env, importer.Ref{Provider: importer.ProviderCursor, ID: id, Path: refPath}, full
}

func adhocRead(t *testing.T, env harness.Env, ref importer.Ref, cursor importer.Cursor) (store.Thread, importer.Cursor, importer.Result) {
	t.Helper()
	th, cur, res, err := Source{}.Read(env, ref, cursor)
	if err != nil {
		t.Fatalf("Read %s: %v", ref.ID, err)
	}
	return th, cur, res
}

// TestEmptySlugFilesUnderTheSessionID: a ref with no .cursor shape has no
// slug to decode, so the session files under cursor:<id> — the session
// identity, unique per import_state key — rather than the bare "cursor:" a
// missing slug would otherwise build. Counted as the same fallback, and
// with no sidecar beside it there is no repo event.
func TestEmptySlugFilesUnderTheSessionID(t *testing.T) {
	env, ref, _ := adhocTranscript(t, "odd/path.jsonl", "odd-session-9",
		[]string{`{"role":"user","message":{"content":[{"type":"text","text":"hi"}]}}`}, "")
	th, _, res := adhocRead(t, env, ref, nil)

	if want := "cursor:odd-session-9"; th.Worktree.Path != want {
		t.Fatalf("worktree path = %q, want %q", th.Worktree.Path, want)
	}
	if th.Worktree.VCS != vcsNone {
		t.Fatalf("worktree vcs = %q, want %q", th.Worktree.VCS, vcsNone)
	}
	if res.Classified["worktree-fallback"] != 1 {
		t.Fatalf("Classified = %v, want the fallback counted", res.Classified)
	}
	if len(th.Events) != 0 {
		t.Fatalf("events = %+v, want none: no slug means no sidecar", th.Events)
	}
}

// TestSystemRoleBecomesAMessageWithoutATurn: system/tool/error lines are
// messages, not furniture — only empty-role lines classify-by-type or skip.
// Only user opens turns, so the two system lines below share the one
// leading span SplitTurns always opens rather than opening one each.
func TestSystemRoleBecomesAMessageWithoutATurn(t *testing.T) {
	env, ref, _ := adhocTranscript(t,
		projectsDir+"/adhoc-sys/agent-transcripts/adhoc-sys-1/adhoc-sys-1.jsonl", "adhoc-sys-1",
		[]string{
			`{"role":"system","message":{"content":[{"type":"text","text":"first note"}]}}`,
			`{"role":"system","message":{"content":[{"type":"text","text":"second note"}]}}`,
		}, "")
	th, _, _ := adhocRead(t, env, ref, nil)

	if len(th.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(th.Messages))
	}
	for _, m := range th.Messages {
		if m.Role != store.RoleSystem {
			t.Fatalf("message role = %q, want system", m.Role)
		}
		if len(m.Parts) != 1 || m.Parts[0].Type != importer.PartText {
			t.Fatalf("message parts = %+v, want one text part", m.Parts)
		}
	}
	if len(th.Turns) != 1 {
		t.Fatalf("turns = %d, want the one leading span", len(th.Turns))
	}
}

// TestShorterRewriteRescansFromZero: a transcript rewritten shorter is a
// rescan, not a delta — the returned thread holds the rewritten content
// whole, and the repo sidecar is re-emitted because a rescan is a full read.
func TestShorterRewriteRescansFromZero(t *testing.T) {
	const repoID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	env, ref, path := adhocTranscript(t,
		projectsDir+"/adhoc-rescan/agent-transcripts/adhoc-rescan-1/adhoc-rescan-1.jsonl", "adhoc-rescan-1",
		[]string{
			`{"role":"user","message":{"content":[{"type":"text","text":"first version one"}]}}`,
			`{"role":"user","message":{"content":[{"type":"text","text":"first version two"}]}}`,
		}, repoID)
	_, held, _ := adhocRead(t, env, ref, nil)

	rewritten := `{"role":"user","message":{"content":[{"type":"text","text":"v2"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	th, _, res, err := Source{}.Read(env, ref, held)
	if err != nil {
		t.Fatalf("rescan Read: %v", err)
	}
	if len(th.Messages) != 1 {
		t.Fatalf("messages = %d, want the 1 rewritten line whole, not a delta", len(th.Messages))
	}
	if len(th.Messages[0].Parts) != 1 || !strings.Contains(th.Messages[0].Parts[0].Data, "v2") {
		t.Fatalf("message parts = %+v, want the rewritten content", th.Messages[0].Parts)
	}
	if res.Lines != 1 {
		t.Fatalf("lines = %d, want 1", res.Lines)
	}
	if len(th.Events) != 1 || th.Events[0].Type != eventRepo {
		t.Fatalf("events = %+v, want the re-emitted repo sidecar", th.Events)
	}
	if !strings.Contains(th.Events[0].Data, repoID) {
		t.Fatalf("event data = %s, want the repo id %q", th.Events[0].Data, repoID)
	}
}

// TestMidLineOffsetKeepsAbsoluteToolIDs: a cursor stranded mid-line backs
// up and re-reads the whole line, and the re-read tool_use carries the same
// absolute id the full read gave it — base is the newline count before the
// resume offset, not a per-read number.
func TestMidLineOffsetKeepsAbsoluteToolIDs(t *testing.T) {
	env, ref, _ := adhocTranscript(t,
		projectsDir+"/adhoc-mid/agent-transcripts/adhoc-mid-1/adhoc-mid-1.jsonl", "adhoc-mid-1",
		[]string{
			`{"role":"user","message":{"content":[{"type":"text","text":"prompt"}]}}`,
			`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"/x"}}]}}`,
		}, "")
	full, cur, _ := adhocRead(t, env, ref, nil)

	var want string
	for _, m := range full.Messages {
		for _, p := range m.Parts {
			if p.Type == importer.PartToolCall {
				want = p.ToolCallID
			}
		}
	}
	if want == "" {
		t.Fatal("full read emitted no tool_call")
	}

	jc, ok := cur.(importer.JSONLCursor)
	if !ok {
		t.Fatalf("cursor = %T, want JSONLCursor", cur)
	}
	mid := jc.Offset - 5
	if mid <= 0 {
		t.Fatalf("cursor = %+v, too small to sit mid-line", jc)
	}
	reread, _, _, err := Source{}.Read(env, ref, importer.JSONLCursor{Offset: mid, Size: jc.Size})
	if err != nil {
		t.Fatalf("mid-line Read: %v", err)
	}
	var got string
	for _, m := range reread.Messages {
		for _, p := range m.Parts {
			if p.Type == importer.PartToolCall {
				got = p.ToolCallID
			}
		}
	}
	if got != want {
		t.Fatalf("re-read tool_call id = %q, want the full-read id %q", got, want)
	}
}

// TestDeltaResultWithoutCallIsUnlinked: pairing is per-read, so a delta
// carrying only a result — its call went out with an earlier read — keeps
// the message and counts tool_result:unlinked rather than dropping it.
func TestDeltaResultWithoutCallIsUnlinked(t *testing.T) {
	env, ref, path := adhocTranscript(t,
		projectsDir+"/adhoc-unlinked/agent-transcripts/adhoc-unlinked-1/adhoc-unlinked-1.jsonl", "adhoc-unlinked-1",
		[]string{
			`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"path":"/x"}}]}}`,
		}, "")
	_, held, _ := adhocRead(t, env, ref, nil)

	appendLine(t, path,
		`{"role":"assistant","message":{"content":[{"type":"tool_result","tool_use_id":"foreign-xyz","content":"late output"}]}}`+"\n")
	delta, _, res, err := Source{}.Read(env, ref, held)
	if err != nil {
		t.Fatalf("delta Read: %v", err)
	}
	if len(delta.Messages) != 1 {
		t.Fatalf("messages = %d, want the 1 appended result line", len(delta.Messages))
	}
	var sawResult bool
	for _, p := range delta.Messages[0].Parts {
		if p.Type == importer.PartToolResult {
			sawResult = true
			if p.ToolCallID != "foreign-xyz" {
				t.Fatalf("tool_result call id = %q, want foreign-xyz", p.ToolCallID)
			}
		}
	}
	if !sawResult {
		t.Fatalf("message parts = %+v, want a tool_result", delta.Messages[0].Parts)
	}
	if res.Classified["tool_result:unlinked"] != 1 {
		t.Fatalf("Classified = %v, want the cross-read link counted unlinked", res.Classified)
	}
}

// TestReadKeepsTheCursorWhenItCannotOpenTheFile: a transcript that is
// momentarily unreadable has not invalidated the watermark, and handing back
// a zero one would make the next read re-import the whole session —
// duplicating every message, since none carries a foreign id.
func TestReadKeepsTheCursorWhenItCannotOpenTheFile(t *testing.T) {
	f := newFixture(t)
	held := importer.JSONLCursor{Offset: 4096, Size: 8192}
	missing := importer.Ref{Provider: importer.ProviderCursor, ID: "gone", Path: filepath.Join(projectsDir, "gone", transcriptsDir, "gone", "gone.jsonl")}

	_, back, _, err := Source{}.Read(f.env, missing, held)
	if err == nil {
		t.Fatal("Read of a missing transcript succeeded")
	}
	if back != importer.Cursor(held) {
		t.Fatalf("cursor back = %+v, want the one held %+v", back, held)
	}
}

// canonicalThread is the golden's shape. It exists rather than marshalling
// store.Thread directly because store's structs have no json tags, so their
// encoding would be Go field names and every rename would rewrite the
// golden — and because the Result belongs in the golden too: the accounting
// is as much of the contract as the rows are.
type canonicalThread struct {
	Ref      canonicalRef     `json:"ref"`
	Worktree store.Worktree   `json:"worktree"`
	Session  canonicalSession `json:"session"`
	Binding  canonicalBinding `json:"binding"`
	Turns    []canonicalTurn  `json:"turns"`
	Events   []store.Event    `json:"events"`
	Messages []canonicalMsg   `json:"messages"`
	Result   canonicalResult  `json:"result"`
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
	Status string `json:"status"`
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
		Ref: canonicalRef{Provider: ref.Provider, ID: ref.ID, Path: f.unresolve(ref.Path), Key: ref.Key()},
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
	for _, t := range th.Turns {
		out.Turns = append(out.Turns, canonicalTurn{Status: t.Status})
	}
	for _, e := range th.Events {
		out.Events = append(out.Events, store.Event{Type: e.Type, Data: f.unresolve(e.Data)})
	}
	for _, m := range th.Messages {
		raw := ""
		if m.RawJSON != nil {
			raw = f.unresolve(*m.RawJSON)
		}
		cm := canonicalMsg{
			Role: string(m.Role), Provider: m.Provider, Model: m.Model,
			ForeignID: m.ForeignID, Usage: m.Usage,
			RawJSON: raw,
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
