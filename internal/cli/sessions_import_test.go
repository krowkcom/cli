package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	harnessenv "github.com/krowkcom/cli/internal/harness"
	"github.com/krowkcom/cli/internal/importer"
	"github.com/krowkcom/cli/internal/store"
)

// The importer fixtures live with the readers that own them, so these tests
// materialise the same testdata the reader tests do rather than keeping a
// second copy. A second copy would be the thing that rots: a reader whose
// fixture grows a line would keep passing here against the old one.
const (
	claudeTestdata   = "../importer/claude/testdata"
	cursorTestdata   = "../importer/cursor/testdata"
	opencodeTestdata = "../importer/opencode/testdata"
)

// importEnvelope is the envelope `sessions import` answers with, read back
// as a caller would read it rather than as the struct that produced it.
type importEnvelope struct {
	OK      bool   `json:"ok"`
	Summary string `json:"summary"`
	Data    struct {
		DryRun     bool   `json:"dry_run"`
		Store      string `json:"store"`
		DurationMS int64  `json:"duration_ms"`
		Providers  []struct {
			Provider         string         `json:"provider"`
			Files            int            `json:"files"`
			SessionsSeen     int            `json:"sessions_seen"`
			MessagesSeen     int            `json:"messages_seen"`
			PartsSeen        int            `json:"parts_seen"`
			SessionsInserted int            `json:"sessions_inserted"`
			MessagesInserted int            `json:"messages_inserted"`
			PartsInserted    int            `json:"parts_inserted"`
			SkippedByType    map[string]int `json:"skipped_by_type"`
			SkippedLines     int            `json:"skipped_lines"`
			FilesFailed      int            `json:"files_failed"`
			Errors           []string       `json:"errors"`
			ErrorsTruncated  int            `json:"errors_truncated"`
			DurationMS       int64          `json:"duration_ms"`
		} `json:"providers"`
	} `json:"data"`
}

func (e importEnvelope) provider(t *testing.T, name string) int {
	t.Helper()
	for i, p := range e.Data.Providers {
		if p.Provider == name {
			return i
		}
	}
	t.Fatalf("no %s row in the report: %+v", name, e.Data.Providers)
	return -1
}

// importHarness is a CLI harness pointed at a home of its own, which is both
// where the transcripts are seeded and where krowk.db lands.
func importHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t, 0)
	home := t.TempDir()
	h.env["HOME"] = home
	// Emptied rather than left alone: a developer running the suite with
	// XDG_DATA_HOME set would otherwise have the test write into their own
	// store.
	h.env["XDG_DATA_HOME"] = ""
	return h, home
}

func mustRun(t *testing.T, h *harness, args ...string) importEnvelope {
	t.Helper()
	code, stdout, stderr := h.run(args...)
	if code != 0 {
		t.Fatalf("`krowk %s` exited %d\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), code, stdout, stderr)
	}
	var e importEnvelope
	if err := json.Unmarshal([]byte(stdout), &e); err != nil {
		t.Fatalf("not an envelope: %v\n%s", err, stdout)
	}
	if !e.OK {
		t.Fatalf("envelope is not ok:\n%s", stdout)
	}
	return e
}

// seedClaude materialises the Claude reader's fixture into home, restoring
// the dot its testdata directory drops so the repository's .gitignore does
// not swallow it.
func seedClaude(t *testing.T, home string) {
	t.Helper()
	root := t.TempDir()
	cwd := filepath.Join(root, "repo", "sub")
	noGit := filepath.Join(root, "plain")
	for _, dir := range []string{filepath.Join(root, "repo", ".git"), cwd, noGit} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	copyFixture(t, filepath.Join(claudeTestdata, "home"), home,
		func(rel string) string { return strings.ReplaceAll(rel, "dot-claude", ".claude") },
		func(data string) string {
			data = strings.ReplaceAll(data, "{{CWD}}", cwd)
			return strings.ReplaceAll(data, "{{NOGIT}}", noGit)
		})
}

// seedCursor materialises the Cursor reader's fixture, naming the {{SLUG}}
// directory after a work directory that exists, the way the reader's own
// fixture does.
func seedCursor(t *testing.T, home string) {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	slug := strings.ReplaceAll(strings.TrimPrefix(work, "/"), "/", "-")
	copyFixture(t, filepath.Join(cursorTestdata, "home"), home,
		func(rel string) string { return strings.ReplaceAll(rel, "{{SLUG}}", slug) },
		func(data string) string { return data })
}

// seedOpencode builds a real opencode.db under home from the checked-in SQL.
func seedOpencode(t *testing.T, home string) {
	t.Helper()
	worktree := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(opencodeTestdata, "opencode", "opencode.sql"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(raw), "{{WORKTREE}}", worktree)
	db, err := sql.Open(store.DriverName, "file:"+filepath.Join(dir, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			kept = append(kept, line)
		}
	}
	for _, stmt := range strings.Split(strings.Join(kept, "\n"), ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec fixture sql: %v", err)
		}
	}
}

// copyFixture walks src into dst, rewriting each relative path and each
// file's contents through the two functions.
func copyFixture(t *testing.T, src, dst string, path func(string) string, contents func(string) string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, path(rel))
		if d.IsDir() {
			return os.MkdirAll(out, 0o700)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, []byte(contents(string(data))), 0o600)
	})
	if err != nil {
		t.Fatalf("materialise %s: %v", src, err)
	}
}

// goldenCounts is how many refs and how many messages a reader's golden file
// says its fixture holds. Read rather than written down, so a fixture that
// grows a line does not silently make these tests assert the old number.
func goldenCounts(t *testing.T, dir string) (files, messages int) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		messages += len(e.Messages)
	}
	return len(entries), messages
}

// rowCounts is the store's own tally, which is what an envelope's numbers
// are checked against: a report agreeing with itself proves nothing.
func rowCounts(t *testing.T, env func(string) string) map[string]int {
	t.Helper()
	db, err := store.Open(store.Env(env))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	counts := map[string]int{}
	for _, table := range []string{"session", "message", "part", "import_state"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = n
	}
	return counts
}

// importStateRows is every cursor in the store, keyed by source.
func importStateRows(t *testing.T, env func(string) string) map[string]string {
	t.Helper()
	db, err := store.Open(store.Env(env))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT source, cursor FROM import_state`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var source, cursor string
		if err := rows.Scan(&source, &cursor); err != nil {
			t.Fatal(err)
		}
		out[source] = cursor
	}
	return out
}

// A dry run discovers and counts, and writes nothing. The store is opened —
// which is why the row counts are taken after a first dry run rather than
// before one — and not a row lands in it.
func TestImportDryRunWritesNothing(t *testing.T) {
	h, home := importHarness(t)
	seedClaude(t, home)
	env := func(k string) string { return h.env[k] }

	e := mustRun(t, h, "sessions", "import", "--from", "claude", "--dry-run", "--json")
	before := rowCounts(t, env)

	if !e.Data.DryRun {
		t.Error("the report does not say it was a dry run")
	}
	row := e.Data.Providers[e.provider(t, "claude")]
	if row.Files == 0 {
		t.Fatalf("a dry run on a seeded home discovered nothing: %+v", row)
	}
	if row.MessagesSeen != 0 || row.SessionsSeen != 0 {
		t.Errorf("a dry run reported rows it did not write: %+v", row)
	}

	mustRun(t, h, "sessions", "import", "--from", "claude", "--dry-run", "--json")
	if after := rowCounts(t, env); after["message"] != before["message"] ||
		after["session"] != before["session"] || after["import_state"] != before["import_state"] {
		t.Errorf("a dry run changed the store: %v -> %v", before, after)
	}
	if before["message"] != 0 {
		t.Errorf("a dry run wrote %d messages", before["message"])
	}
}

// `--from all` on a home seeded with all three fixtures imports every one of
// them, and the totals are the goldens'.
func TestImportFromAllMatchesTheGoldens(t *testing.T) {
	h, home := importHarness(t)
	seedClaude(t, home)
	seedCursor(t, home)
	seedOpencode(t, home)
	env := func(k string) string { return h.env[k] }

	e := mustRun(t, h, "sessions", "import", "--from", "all", "--json")

	want := map[string]string{
		"claude":   claudeTestdata,
		"cursor":   cursorTestdata,
		"opencode": opencodeTestdata,
	}
	totalMessages := 0
	for provider, dir := range want {
		files, messages := goldenCounts(t, dir)
		row := e.Data.Providers[e.provider(t, provider)]
		if row.FilesFailed != 0 {
			t.Errorf("%s failed %d files: %v", provider, row.FilesFailed, row.Errors)
		}
		if row.Files != files {
			t.Errorf("%s files = %d, want %d (the golden's refs)", provider, row.Files, files)
		}
		if row.SessionsSeen != files {
			t.Errorf("%s sessions = %d, want %d", provider, row.SessionsSeen, files)
		}
		if row.MessagesSeen != messages {
			t.Errorf("%s messages = %d, want %d (the golden's messages)",
				provider, row.MessagesSeen, messages)
		}
		if row.MessagesInserted != messages {
			t.Errorf("%s inserted %d messages on a first import, want %d",
				provider, row.MessagesInserted, messages)
		}
		if row.SkippedByType == nil {
			t.Errorf("%s reports a null skipped_by_type rather than an empty one", provider)
		}
		totalMessages += messages
	}

	// The store is the check on the report, rather than the report on itself.
	counts := rowCounts(t, env)
	if counts["message"] != totalMessages {
		t.Errorf("the store holds %d messages, the goldens say %d", counts["message"], totalMessages)
	}
	if counts["import_state"] == 0 {
		t.Error("an import wrote no cursors")
	}
}

// Run it twice: the second run inserts nothing and leaves every cursor as it
// was. It is the whole point of import_state, and the one thing a person
// re-running the command has to be able to rely on.
func TestImportTwiceInsertsNothingTheSecondTime(t *testing.T) {
	h, home := importHarness(t)
	seedClaude(t, home)
	seedCursor(t, home)
	seedOpencode(t, home)
	env := func(k string) string { return h.env[k] }

	mustRun(t, h, "sessions", "import", "--from", "all", "--json")
	firstCounts := rowCounts(t, env)
	firstCursors := importStateRows(t, env)

	e := mustRun(t, h, "sessions", "import", "--from", "all", "--json")
	for _, row := range e.Data.Providers {
		if row.SessionsInserted != 0 || row.MessagesInserted != 0 || row.PartsInserted != 0 {
			t.Errorf("%s re-inserted on a second run: %+v", row.Provider, row)
		}
		if row.FilesFailed != 0 {
			t.Errorf("%s failed %d files on a second run: %v", row.Provider, row.FilesFailed, row.Errors)
		}
	}
	if second := rowCounts(t, env); second["message"] != firstCounts["message"] ||
		second["session"] != firstCounts["session"] || second["part"] != firstCounts["part"] {
		t.Errorf("a second import changed the store: %v -> %v", firstCounts, second)
	}
	secondCursors := importStateRows(t, env)
	if len(secondCursors) != len(firstCursors) {
		t.Fatalf("cursor rows went from %d to %d", len(firstCursors), len(secondCursors))
	}
	for source, cursor := range firstCursors {
		if secondCursors[source] != cursor {
			t.Errorf("cursor for %s moved from %q to %q", source, cursor, secondCursors[source])
		}
	}
}

// A second import refuses rather than meeting the first one inside SQLite.
// The refusal names the lock file, exits 6, and never says "database is
// locked" — which is the whole reason the lock is taken outside the database.
func TestImportRefusesWhileAnotherHoldsTheLock(t *testing.T) {
	h, home := importHarness(t)
	seedClaude(t, home)
	env := func(k string) string { return h.env[k] }

	// The store directory has to exist for the lock to be taken in it. A
	// dry run no longer creates it — that is the point of a dry run — so
	// the test makes it the same way the import would.
	path := importLockPath(store.DBPath(store.Env(env)))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := lockImport(path)
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	defer held.Close()

	code, stdout, stderr := h.run("sessions", "import", "--from", "claude", "--json")
	out := stdout + stderr
	if code != 6 {
		t.Errorf("exit = %d, want 6\n%s", code, out)
	}
	if !strings.Contains(out, "import.lock") {
		t.Errorf("the refusal does not name the lock file:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "database is locked") {
		t.Errorf("a SQLite lock message reached the user:\n%s", out)
	}
	if strings.Contains(out, "import_locked") == false {
		t.Errorf("the refusal carries no code:\n%s", out)
	}

	// And once the lock goes, the same command works.
	held.Close()
	mustRun(t, h, "sessions", "import", "--from", "claude", "--json")
}

// No home is no store, and the refusal is store.Open's own hint. Nothing is
// written to the working directory, which is the failure the hint exists to
// prevent.
func TestImportWithNoHomeFailsClosed(t *testing.T) {
	h, _ := importHarness(t)
	h.env["HOME"] = ""
	h.env["XDG_DATA_HOME"] = ""

	code, stdout, stderr := h.run("sessions", "import", "--from", "all", "--json")
	out := stdout + stderr
	if code == 0 {
		t.Fatalf("an import with no home succeeded:\n%s", out)
	}
	if !strings.Contains(out, "set HOME") {
		t.Errorf("the refusal is not store.Open's hint:\n%s", out)
	}
	if _, err := os.Stat("krowk.db"); err == nil {
		os.Remove("krowk.db")
		t.Error("a homeless import wrote krowk.db into the working directory")
	}
}

// Windows is refused in so many words, before anything touches the store.
// The check is injected rather than the test being tagged, because a test
// that only runs on Windows is a test nobody here runs.
func TestImportRefusesWindowsBeforeTouchingTheStore(t *testing.T) {
	h, home := importHarness(t)
	seedClaude(t, home)

	previous := checkImportOS
	checkImportOS = func() error { return os.ErrInvalid }
	t.Cleanup(func() { checkImportOS = previous })

	code, stdout, stderr := h.run("sessions", "import", "--from", "all", "--json")
	out := stdout + stderr
	if code == 0 {
		t.Fatalf("an unsupported OS succeeded:\n%s", out)
	}
	if !strings.Contains(out, "sessions is not supported on Windows in v1") {
		t.Errorf("the refusal does not say what it must, verbatim:\n%s", out)
	}
	if _, err := os.Stat(store.DBPath(func(k string) string { return h.env[k] })); err == nil {
		t.Error("the refusal created krowk.db before refusing")
	}
}

// --from is required, and a source krowk cannot read is named rather than
// silently ignored. Both are the command being wrong, which is exit 1.
func TestImportRefusesAMissingOrUnknownSource(t *testing.T) {
	h, home := importHarness(t)
	seedClaude(t, home)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing", []string{"sessions", "import", "--json"}, "--from"},
		{"unknown", []string{"sessions", "import", "--from", "codex", "--json"}, "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := h.run(tc.args...)
			out := stdout + stderr
			if code != 1 {
				t.Errorf("exit = %d, want 1\n%s", code, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the refusal does not mention %q:\n%s", tc.want, out)
			}
		})
	}
}

// --limit caps the refs read per source, so an import can be tried on a big
// machine without reading all of it.
func TestImportLimitIsPerSource(t *testing.T) {
	h, home := importHarness(t)
	seedClaude(t, home)
	env := func(k string) string { return h.env[k] }

	e := mustRun(t, h, "sessions", "import", "--from", "claude", "--limit", "1", "--json")
	row := e.Data.Providers[e.provider(t, "claude")]
	if row.Files != 1 {
		t.Errorf("--limit 1 read %d files", row.Files)
	}
	if counts := rowCounts(t, env); counts["session"] != 1 {
		t.Errorf("--limit 1 stored %d sessions", counts["session"])
	}

	// And 0 is no limit, not no files.
	e = mustRun(t, h, "sessions", "import", "--from", "claude", "--limit", "0", "--json")
	if row := e.Data.Providers[e.provider(t, "claude")]; row.Files < 2 {
		t.Errorf("--limit 0 read %d files, want every one", row.Files)
	}
}

// The human rendering is one line per source, which is what somebody
// watching it wants and what `--json` is for otherwise.
func TestImportHumanOutputIsOneLinePerProvider(t *testing.T) {
	h, home := importHarness(t)
	seedClaude(t, home)

	code, stdout, stderr := h.runOn(true, "sessions", "import", "--from", "all", "--format=human")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("want a line each for claude, cursor and opencode:\n%s", stdout)
	}
	for i, provider := range []string{"claude", "cursor", "opencode"} {
		if !strings.HasPrefix(lines[i], provider) {
			t.Errorf("line %d = %q, want it to start with %s", i, lines[i], provider)
		}
	}
}

// The cursor is written with the rows it describes, so a store that holds a
// ref's messages also holds that ref's watermark.
func TestImportWritesACursorForEveryRefItRead(t *testing.T) {
	h, home := importHarness(t)
	seedClaude(t, home)
	env := func(k string) string { return h.env[k] }

	e := mustRun(t, h, "sessions", "import", "--from", "claude", "--json")
	row := e.Data.Providers[e.provider(t, "claude")]
	cursors := importStateRows(t, env)
	if len(cursors) != row.Files {
		t.Errorf("%d refs read, %d cursors stored", row.Files, len(cursors))
	}
	for source, cursor := range cursors {
		if !strings.HasPrefix(source, "claude:") {
			t.Errorf("cursor stored under %q", source)
		}
		if cursor == "" {
			t.Errorf("cursor for %s is empty, so the next run rescans it", source)
		}
	}
}

// ReadImportState reads back what IngestWithCursor wrote, and answers "" for
// a key nobody has imported rather than an error.
func TestImportStateRoundTrips(t *testing.T) {
	h, home := importHarness(t)
	seedClaude(t, home)
	env := func(k string) string { return h.env[k] }
	mustRun(t, h, "sessions", "import", "--from", "claude", "--json")

	db, err := store.Open(store.Env(env))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	absent, err := store.ReadImportState(context.Background(), db, "claude:nobody-has-this")
	if err != nil {
		t.Errorf("an unimported key is an error: %v", err)
	}
	if absent != "" {
		t.Errorf("an unimported key reads %q, want empty", absent)
	}

	for source, want := range importStateRows(t, env) {
		got, err := store.ReadImportState(context.Background(), db, source)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("ReadImportState(%s) = %q, want %q", source, got, want)
		}
	}
}

// slowSource is a reader that takes a measurable amount of time to say it
// has nothing, so the stopwatch has something to measure.
type slowSource struct{ delay time.Duration }

func (slowSource) Name() string { return "slow" }

func (s slowSource) Discover(harnessenv.Env) ([]importer.Ref, error) {
	time.Sleep(s.delay)
	return nil, nil
}

func (slowSource) Read(harnessenv.Env, importer.Ref, importer.Cursor) (store.Thread, importer.Cursor, importer.Result, error) {
	return store.Thread{}, nil, importer.Result{}, nil
}

// duration_ms is the source's own, and it has to survive the return. A
// deferred write into an unnamed return value lands on a copy nobody sees,
// which reports every source as instant however long it took.
func TestImportReportsHowLongASourceTook(t *testing.T) {
	const delay = 20 * time.Millisecond
	out := runImportSource(context.Background(), nil, "",
		importSource{src: slowSource{delay: delay}, decode: func(string) (importer.Cursor, error) {
			return nil, nil
		}},
		func(string) string { return "" }, flags{dryRun: true})

	if out.DurationMS < delay.Milliseconds() {
		t.Errorf("duration_ms = %d, want at least %d", out.DurationMS, delay.Milliseconds())
	}
}

// chmodAllTranscripts makes every .jsonl under home unreadable and reports
// how many it changed, so a test can turn one file or all of them into a
// read failure without inventing a fake source.
func chmodTranscripts(t *testing.T, home string, mode os.FileMode, max int) int {
	t.Helper()
	changed := 0
	err := filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".jsonl" {
			return err
		}
		if max > 0 && changed >= max {
			return nil
		}
		if err := os.Chmod(p, mode); err != nil {
			return err
		}
		changed++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && filepath.Ext(p) == ".jsonl" {
				_ = os.Chmod(p, 0o600)
			}
			return nil
		})
	})
	return changed
}

// One transcript that will not read is a lossy import, not a failed one: it
// is counted and listed, everything else lands, and the exit stays 0.
func TestImportSurvivesOneUnreadableTranscript(t *testing.T) {
	requireNotRoot(t)
	h, home := importHarness(t)
	seedClaude(t, home)
	if chmodTranscripts(t, home, 0o000, 1) != 1 {
		t.Fatal("no transcript to make unreadable")
	}

	e := mustRun(t, h, "sessions", "import", "--from", "claude", "--json")
	row := e.Data.Providers[e.provider(t, "claude")]
	if row.FilesFailed != 1 {
		t.Errorf("files_failed = %d, want 1", row.FilesFailed)
	}
	if len(row.Errors) != 1 {
		t.Errorf("errors = %v, want the one file's reason", row.Errors)
	}
	if row.SessionsSeen != row.Files-1 {
		t.Errorf("sessions_seen = %d of %d files, want everything but the bad one",
			row.SessionsSeen, row.Files)
	}
}

// Every transcript failing is a different thing entirely, and it exits 1.
// A lossy import reported as success is how a scheduled import runs green
// for a month having stored nothing.
func TestImportFailsWhenASourceLosesEveryTranscript(t *testing.T) {
	requireNotRoot(t)
	h, home := importHarness(t)
	seedClaude(t, home)
	chmodTranscripts(t, home, 0o000, 0)

	code, stdout, stderr := h.run("sessions", "import", "--from", "claude", "--json")
	out := stdout + stderr
	if code != 1 {
		t.Errorf("exit = %d, want 1 when nothing could be read\n%s", code, out)
	}
	if !strings.Contains(out, "lost every one of its") {
		t.Errorf("the failure does not say what happened:\n%s", out)
	}
}

// The unreadable-file tests need a user the filesystem can say no to.
func requireNotRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 is still readable")
	}
}

// A SQLite lock message is not an answer: it names a condition with no
// action attached. Whatever shape it arrives in, what reaches the caller
// says which file is busy and who is at fault.
func TestSanitizeStoreErrHidesTheSQLiteLockMessage(t *testing.T) {
	const path = "/home/somebody/.local/share/krowk/krowk.db"
	for _, raw := range []string{
		"database is locked",
		"sqlite3: database is locked",
		"store: ingest: database table is locked",
		"DATABASE IS LOCKED",
	} {
		got := sanitizeStoreErr(errors.New(raw), path)
		if strings.Contains(strings.ToLower(got), "database is locked") ||
			strings.Contains(strings.ToLower(got), "database table is locked") {
			t.Errorf("sanitizeStoreErr(%q) still says it: %q", raw, got)
		}
		if !strings.Contains(got, path) || !strings.Contains(got, "busy") {
			t.Errorf("sanitizeStoreErr(%q) = %q, want the store named and called busy", raw, got)
		}
	}
	// Everything else is passed through untouched: the sanitiser exists for
	// one message, not to re-word every store failure.
	if got := sanitizeStoreErr(errors.New("no such table: session"), path); got != "no such table: session" {
		t.Errorf("an unrelated failure was re-worded: %q", got)
	}
}

// A dry run writes nothing at all, which includes the store itself: it takes
// no lock and does not open the database, because discovery only reads the
// environment. A command that promises to write nothing and leaves a
// krowk.db behind has broken the promise on the first word.
func TestImportDryRunCreatesNoStore(t *testing.T) {
	h, home := importHarness(t)
	seedClaude(t, home)
	env := func(k string) string { return h.env[k] }

	e := mustRun(t, h, "sessions", "import", "--from", "claude", "--dry-run", "--json")
	if row := e.Data.Providers[e.provider(t, "claude")]; row.Files == 0 {
		t.Fatalf("a dry run on a seeded home discovered nothing: %+v", row)
	}
	if _, err := os.Stat(store.DBPath(store.Env(env))); !os.IsNotExist(err) {
		t.Errorf("a dry run created the store at %s (stat: %v)",
			store.DBPath(store.Env(env)), err)
	}
}

// And a dry run with no home fails closed on the same hint a real import
// does, rather than being the one shape of this command that does not care
// where the store would have been.
func TestImportDryRunWithNoHomeFailsClosed(t *testing.T) {
	h, _ := importHarness(t)
	h.env["HOME"] = ""
	h.env["XDG_DATA_HOME"] = ""

	code, stdout, stderr := h.run("sessions", "import", "--from", "all", "--dry-run", "--json")
	out := stdout + stderr
	if code == 0 {
		t.Fatalf("a homeless dry run succeeded:\n%s", out)
	}
	if !strings.Contains(out, "set HOME") {
		t.Errorf("the refusal is not store.Open's hint:\n%s", out)
	}
}

// Two real imports racing for the same store: one wins, the other is refused
// by the lock. This is the test the in-process one above cannot be — it
// takes the lock itself and then runs a command — and the thing it proves is
// that whichever one loses says so in krowk's words and never SQLite's.
func TestImportTwoConcurrentRunsOneWinsOneIsRefused(t *testing.T) {
	h, home := importHarness(t)
	seedClaude(t, home)

	type result struct {
		code int
		out  string
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, stdout, stderr := h.run("sessions", "import", "--from", "claude", "--json")
			results[i] = result{code: code, out: stdout + stderr}
		}(i)
	}
	wg.Wait()

	won, refused := 0, 0
	for _, r := range results {
		switch r.code {
		case 0:
			won++
		case 6:
			refused++
			if !strings.Contains(r.out, "import.lock") {
				t.Errorf("the refusal does not name the lock file:\n%s", r.out)
			}
		default:
			t.Errorf("exit = %d, want 0 or 6:\n%s", r.code, r.out)
		}
		if strings.Contains(strings.ToLower(r.out), "database is locked") {
			t.Errorf("a SQLite lock message reached the user:\n%s", r.out)
		}
	}
	if won != 1 || refused != 1 {
		t.Errorf("%d runs succeeded and %d were refused, want one of each:\n%s\n%s",
			won, refused, results[0].out, results[1].out)
	}
}

// The errors list is capped, and the count of what the cap dropped is
// reported beside it: ten reasons next to four hundred failures without one
// reads like ten problems.
func TestImportErrorsCarryATruncationCount(t *testing.T) {
	var out sourceOutcome
	for i := 0; i < maxReportedErrors+3; i++ {
		out.fail(importer.Ref{Provider: "claude", ID: "ref"}, errors.New("boom"), "")
	}
	if len(out.Errors) != maxReportedErrors {
		t.Errorf("errors = %d, want the cap of %d", len(out.Errors), maxReportedErrors)
	}
	if out.ErrorsTruncated != 3 {
		t.Errorf("errors_truncated = %d, want 3", out.ErrorsTruncated)
	}
	if out.FilesFailed != maxReportedErrors+3 {
		t.Errorf("files_failed = %d, want every failure counted", out.FilesFailed)
	}
}

// The keys of `skipped_by_type` are raw `type` fields out of a transcript,
// so neither their number nor their length is krowk's to trust. Past the cap
// the count keeps accruing under `other`, because "is there more of it" is
// the question the list is read for and dropping the tail answers it wrong.
func TestImportBoundsSkippedByType(t *testing.T) {
	out := sourceOutcome{providerReport: providerReport{SkippedByType: map[string]int{}}}

	long := strings.Repeat("t", 5000)
	types := map[string]int{long: 2}
	for i := 0; i < maxSkippedTypes*3; i++ {
		types[fmt.Sprintf("type-%03d", i)] = 1
	}
	total := 0
	for _, v := range types {
		total += v
	}
	out.absorb(importer.Result{UnknownTypes: types})

	if len(out.SkippedByType) > maxSkippedTypes+1 {
		t.Errorf("skipped_by_type names %d types, want at most %d plus %q",
			len(out.SkippedByType), maxSkippedTypes, skippedTypeOther)
	}
	counted := 0
	for k, v := range out.SkippedByType {
		if len(k) > maxSkippedTypeLen {
			t.Errorf("key of %d bytes survived: %q", len(k), k)
		}
		counted += v
	}
	if counted != total {
		t.Errorf("counted %d skipped parts, want %d — the cap dropped some", counted, total)
	}
	if out.SkippedByType[skippedTypeOther] == 0 {
		t.Errorf("nothing landed in %q despite %d types: %v",
			skippedTypeOther, len(types), out.SkippedByType)
	}
}

// An error reason carries a path and a source's own words, and a report is
// not the place to pass either through at whatever length it arrived.
func TestImportBoundsErrorReasons(t *testing.T) {
	var out sourceOutcome
	out.fail(importer.Ref{Provider: "claude", ID: "ref"},
		errors.New(strings.Repeat("e", 100000)), "")

	if len(out.Errors) != 1 {
		t.Fatalf("errors = %v", out.Errors)
	}
	if n := len(out.Errors[0]); n > maxErrorReasonLen {
		t.Errorf("the reason is %d bytes, want at most %d", n, maxErrorReasonLen)
	}
	if !strings.HasPrefix(out.Errors[0], "claude:ref: ") {
		t.Errorf("the truncation ate the ref it names: %q", out.Errors[0])
	}
}
