package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/krowkcom/cli/internal/api"
	harnessenv "github.com/krowkcom/cli/internal/harness"
	"github.com/krowkcom/cli/internal/importer"
	"github.com/krowkcom/cli/internal/importer/claude"
	"github.com/krowkcom/cli/internal/importer/cursor"
	"github.com/krowkcom/cli/internal/importer/opencode"
	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/runctx"
	"github.com/krowkcom/cli/internal/store"
)

// fromAll is what `--from` takes to mean every source krowk has a reader
// for. It is a word rather than the flag being repeatable because "all"
// keeps meaning all as readers are added, and a caller who wrote out the
// three names would quietly stop importing the fourth.
const fromAll = "all"

// maxReportedErrors bounds the `errors` list in the report. A machine whose
// transcript directory has gone bad can fail every file on it, and a
// response carrying ten thousand near-identical sentences is not a better
// answer than ten and a count.
const maxReportedErrors = 10

// unsupportedOSMessage is what Windows gets, verbatim. It is a constant
// because it is checked as an exact string: the message is the whole answer
// there, and a word drifting into it is a promise about a later version that
// nobody made.
const unsupportedOSMessage = "sessions is not supported on Windows in v1"

// checkImportOS is importer.CheckOS behind a variable so a test can make
// this machine answer as Windows does. The alternative is a test that only
// runs on Windows, which is a test nobody here runs.
var checkImportOS = importer.CheckOS

// importSource pairs a reader with the cursor kind it takes. The kind is
// written down rather than inferred because a Source that is handed the
// wrong concrete cursor refuses with importer.ErrCursorType — deliberately,
// so the mistake surfaces where it was made — and the place that decides
// which kind to decode is here.
type importSource struct {
	src importer.Source
	// decode turns a stored import_state.cursor into the concrete cursor
	// this source takes. An empty string decodes to the zero cursor, which
	// every reader reads as "from the start".
	decode func(string) (importer.Cursor, error)
}

// importSources is every reader krowk has, in the order they run. Claude
// first because it is the one most machines have; the order is otherwise
// only what the report rows come out in.
func importSources() []importSource {
	jsonl := func(s string) (importer.Cursor, error) { return importer.DecodeJSONLCursor(s) }
	sqlite := func(s string) (importer.Cursor, error) { return importer.DecodeSQLiteCursor(s) }
	return []importSource{
		{src: claude.Source{}, decode: jsonl},
		{src: cursor.Source{}, decode: jsonl},
		{src: opencode.Source{}, decode: sqlite},
	}
}

// providerReport is what one source did, in rows rather than in prose. The
// inserted counts sit beside the totals rather than replacing them because
// the two answer different questions: "how much of my history is in the
// store" is the total, and "did this run do anything" is what was inserted.
type providerReport struct {
	Provider string `json:"provider"`
	// Files is how many refs were read, which for every reader here is one
	// per session file or session row.
	Files    int `json:"files"`
	Sessions int `json:"sessions"`
	Messages int `json:"messages"`
	Parts    int `json:"parts"`

	SessionsInserted int `json:"sessions_inserted"`
	MessagesInserted int `json:"messages_inserted"`
	PartsInserted    int `json:"parts_inserted"`

	// SkippedByType is the merged importer.Result.UnknownTypes across every
	// file: the raw source types that landed as `unknown` parts, and how
	// many of each. It is the list to read when adding the type that got
	// missed. Never nil, so a consumer can index it without a check.
	SkippedByType map[string]int `json:"skipped_by_type"`
	// SkippedLines is how many lines could not be used at all, which is a
	// different thing from a part of an unknown type and so is its own
	// number.
	SkippedLines int `json:"skipped_lines"`

	// FilesFailed is refs whose Read returned an error and which were
	// therefore not ingested at all.
	FilesFailed int `json:"files_failed"`
	// Errors is the first maxReportedErrors of those, plus a Discover
	// failure if there was one.
	Errors []string `json:"errors,omitempty"`

	DurationMS int64 `json:"duration_ms"`
}

// importReport is the whole run.
type importReport struct {
	DryRun     bool             `json:"dry_run"`
	Store      string           `json:"store"`
	Providers  []providerReport `json:"providers"`
	DurationMS int64            `json:"duration_ms"`
}

// sessionsImport reads every transcript the named sources can see and writes
// them into the local store.
//
// The failure policy is deliberate and has two levels, because the two
// failures mean different things.
//
// A single file that will not read is not a failed import. A transcript
// being rewritten under the walk, a file that vanished between Discover and
// Read, one corrupt session out of four hundred — losing the other three
// hundred and ninety-nine over any of those would make the command useless
// on exactly the machines it is for. So the file is counted in
// `files_failed`, its reason is listed, the rest of the import continues,
// and the exit code stays 0. A file that failed writes no cursor either, so
// the next run tries it again from the same watermark.
//
// A Discover that fails is different: the source could not say what is
// there, so the count krowk reports for it is not a small answer, it is no
// answer. That is worth a non-zero exit — but not worth abandoning the other
// sources, which know nothing about it. So every source still runs and the
// command exits 1 at the end if any Discover failed.
//
// Every file of a source failing is the third case, and it is not a tolerant
// one. "One transcript in four hundred is corrupt" and "four hundred out of
// four hundred failed" arrive through the same path and mean entirely
// different things: the second is not a lossy import, it is a broken one —
// a store on a schema the writer does not know, a permissions change, a
// reader that cannot read anything any more. Reporting that as success
// because each individual failure was survivable is how a scheduled import
// runs green for a month having stored nothing. So a source that discovered
// files and lost all of them exits 1 as well.
//
// There are no breadcrumbs. Every breadcrumb krowk prints names a command
// that can be run, and the tests run them; the one worth suggesting here is
// the `krowk sessions` picker, which does not exist yet. A crumb pointing at
// it would be the first one in the codebase that does not work.
func sessionsImport(w io.Writer, format output.Format, f flags, env runctx.Env) error {
	// Before anything touches a path. The store would otherwise be created
	// — krowk.db and its directory — on a machine that is about to be told
	// the command does not run there, which is a file left behind by a
	// command that did nothing.
	if err := checkImportOS(); err != nil {
		return api.Fail("unsupported_os", unsupportedOSMessage)
	}

	sources, err := selectedSources(f.from)
	if err != nil {
		return err
	}
	if f.limit < 0 {
		return api.Fail("bad_flag", "--limit is a maximum, so it cannot be negative — "+
			"use 0 for no limit")
	}

	// Open before locking, so a store that cannot be opened at all reports
	// its own hint — a relative or missing HOME is store.ErrNoHome with the
	// sentence that says what to set — rather than a failure to create a
	// lock file in a directory that was never going to exist.
	db, err := store.Open(store.Env(env))
	if err != nil {
		return api.Fail("store_unavailable", err.Error())
	}
	defer db.Close()

	lockPath := importLockPath(store.DBPath(store.Env(env)))
	release, err := lockImport(lockPath)
	if err != nil {
		return importLockFailure(lockPath, err)
	}
	defer release.Close()

	ctx := context.Background()
	started := time.Now()
	report := importReport{DryRun: f.dryRun, Store: store.DBPath(store.Env(env))}
	var broken []string
	for _, s := range sources {
		row := runImportSource(ctx, db, s, harnessenv.Env(env), f)
		if row.discoverFailed {
			broken = append(broken, row.Provider+" could not be listed")
		} else if row.Files > 0 && row.FilesFailed == row.Files {
			broken = append(broken, fmt.Sprintf("%s lost every one of its %d transcripts",
				row.Provider, row.Files))
		}
		report.Providers = append(report.Providers, row.providerReport)
	}
	report.DurationMS = time.Since(started).Milliseconds()

	if err := emitImportReport(w, format, f, report); err != nil {
		return err
	}
	if len(broken) > 0 {
		return api.Fail("import_failed", strings.Join(broken, "; ")+
			" — the reasons are in `errors` in the report above")
	}
	return nil
}

// importLockFailure turns a refused lock into the failure a person reads.
// The lock file is named because that is the one thing a stuck machine needs:
// somebody whose import died with the file left behind can see what to look
// at. The code maps to exitUnreachable (6) rather than to a usage failure,
// which is the same number as a store that will not answer — see the entry
// in clientCodes for why storage is the right class for it.
//
// A SQLite lock message never reaches here, and that is the point of taking
// this lock at all: two imports meeting inside the database would surface as
// "database is locked", which tells a caller nothing about what to do.
func importLockFailure(path string, err error) error {
	if err == errImportLockHeld {
		return api.Fail("import_locked", "another `krowk sessions import` is running on this store — "+
			"wait for it to finish, or check "+path+" if you think it is not")
	}
	return api.Fail("import_locked", "the import lock at "+path+" could not be taken: "+err.Error())
}

// sourceOutcome is one source's report plus the one thing the report does
// not carry: whether the failure was a Discover, which is what decides the
// exit code.
type sourceOutcome struct {
	providerReport
	discoverFailed bool
}

// runImportSource discovers, reads and ingests everything one source can
// see, and never returns an error: everything that can go wrong for one
// source is reported in its own row, so one bad source does not end the run.
// The return is named so the deferred stopwatch below writes into the value
// that is actually returned. With an unnamed return the defer runs after the
// copy and every duration reports 0, which is a lie that looks like a fast
// machine.
func runImportSource(ctx context.Context, db *sql.DB, s importSource, env harnessenv.Env, f flags) (out sourceOutcome) {
	started := time.Now()
	out = sourceOutcome{providerReport: providerReport{
		Provider:      s.src.Name(),
		SkippedByType: map[string]int{},
	}}
	defer func() { out.DurationMS = time.Since(started).Milliseconds() }()

	refs, err := s.src.Discover(env)
	if err != nil {
		out.discoverFailed = true
		out.Errors = append(out.Errors, "discover: "+err.Error())
		return out
	}
	// The limit is per source and it is a limit on refs, which is what
	// makes it the same number whichever source it is applied to: one ref
	// is one session file for the JSONL readers and one session row for
	// opencode.
	if f.limit > 0 && len(refs) > f.limit {
		refs = refs[:f.limit]
	}
	out.Files = len(refs)
	if f.dryRun {
		// Discover and count, and stop. Reading would cost the whole
		// parse of every transcript on the machine to produce numbers
		// this command promises not to write anywhere.
		return out
	}

	writer := store.NewWriter(db, nil)
	for _, ref := range refs {
		stored, err := store.ReadImportState(ctx, db, ref.Key())
		if err != nil {
			out.fail(ref, err)
			continue
		}
		cur, err := s.decode(stored)
		if err != nil {
			out.fail(ref, err)
			continue
		}
		th, next, res, err := s.src.Read(env, ref, cur)
		if err != nil {
			// Not ingested, and no cursor written. The store's contract
			// is explicit that a Thread from a failed Read must not be
			// ingested — its turn list may be truncated, and the last
			// turn would be refreshed downward from it — so the honest
			// thing is to leave the ref exactly as it was and try again
			// next run.
			out.fail(ref, err)
			continue
		}
		out.absorb(res)

		encoded := ""
		if next != nil {
			encoded, err = next.Encode()
			if err != nil {
				out.fail(ref, err)
				continue
			}
		}
		ing, err := writer.IngestWithCursor(ctx, th, ref.Key(), encoded)
		out.count(ing)
		if err != nil {
			out.fail(ref, err)
		}
	}
	return out
}

// count folds one Ingest's counts into the row.
func (o *sourceOutcome) count(r store.Result) {
	o.Sessions += r.Sessions.Inserted + r.Sessions.Skipped
	o.Messages += r.Messages.Inserted + r.Messages.Skipped
	o.Parts += r.Parts.Inserted + r.Parts.Skipped
	o.SessionsInserted += r.Sessions.Inserted
	o.MessagesInserted += r.Messages.Inserted
	o.PartsInserted += r.Parts.Inserted
}

// absorb folds one Read's lossiness into the row.
func (o *sourceOutcome) absorb(r importer.Result) {
	for k, v := range r.UnknownTypes {
		o.SkippedByType[k] += v
	}
	o.SkippedLines += r.SkippedCount
}

// fail records one ref that did not make it, with why, and keeps the list
// bounded.
func (o *sourceOutcome) fail(ref importer.Ref, err error) {
	o.FilesFailed++
	if len(o.Errors) >= maxReportedErrors {
		return
	}
	o.Errors = append(o.Errors, ref.Key()+": "+err.Error())
}

// selectedSources resolves --from. An empty flag is a mistake rather than a
// default: importing everything on the machine is a big thing to do by
// accident, and `--from all` is four more characters than being asked.
func selectedSources(from string) ([]importSource, error) {
	all := importSources()
	names := make([]string, 0, len(all))
	for _, s := range all {
		names = append(names, s.src.Name())
	}
	choices := strings.Join(names, "|") + "|" + fromAll

	switch from {
	case "":
		return nil, api.Fail("parameter_missing",
			"`krowk sessions import` needs --from <"+choices+"> — it says which agent's transcripts to read")
	case fromAll:
		return all, nil
	}
	for _, s := range all {
		if s.src.Name() == from {
			return []importSource{s}, nil
		}
	}
	return nil, api.Fail("bad_flag", "--from "+from+" is not a source krowk can read — "+
		"one of "+choices)
}

// emitImportReport renders the run, as the envelope for a program and one
// line per source for a person.
func emitImportReport(w io.Writer, format output.Format, f flags, report importReport) error {
	if format != output.Human {
		if f.quiet {
			return emit(w, encodeImport(report), f)
		}
		return emit(w, encodeImport(output.Envelope{
			OK:      true,
			Data:    report,
			Summary: importSummary(report),
		}), f)
	}
	for _, p := range report.Providers {
		fmt.Fprintln(w, humanProviderLine(p, report.DryRun))
		for _, e := range p.Errors {
			fmt.Fprintf(w, "  ! %s\n", e)
		}
	}
	return nil
}

// humanProviderLine is one source's row for a person: the same numbers the
// envelope carries, in the order they are asked about.
func humanProviderLine(p providerReport, dryRun bool) string {
	if dryRun {
		return fmt.Sprintf("%-9s %d files found (dry run, nothing written)  %dms",
			p.Provider, p.Files, p.DurationMS)
	}
	line := fmt.Sprintf("%-9s %d files  %d sessions  %d messages  %d parts  %dms",
		p.Provider, p.Files, p.Sessions, p.Messages, p.Parts, p.DurationMS)
	if p.FilesFailed > 0 {
		line += fmt.Sprintf("  (%d failed)", p.FilesFailed)
	}
	return line
}

// importSummary is the one sentence the envelope carries, which is the
// totals across every source.
func importSummary(report importReport) string {
	var files, sessions, messages int
	for _, p := range report.Providers {
		files += p.Files
		sessions += p.Sessions
		messages += p.Messages
	}
	if report.DryRun {
		return fmt.Sprintf("%d files found, nothing written", files)
	}
	return fmt.Sprintf("%d files, %d sessions, %d messages", files, sessions, messages)
}

// encodeImport renders JSON the way every other krowk answer is rendered:
// indented, and without HTML escaping, so a path or a URL in a reason stays
// readable.
func encodeImport(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"ok":false,"error":{"error":"encode_failed","detail":%q}}`, err.Error())
	}
	return string(b)
}
