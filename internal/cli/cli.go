// Package cli is the krowk command line. Run is the whole entry point, taking
// its streams and environment as arguments so tests never touch the process.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/config"
	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/runctx"
)

// Version is stamped at build time: -ldflags "-X .../internal/cli.Version=1.2.3".
// The default is "dev", not a number: an unstamped `go build` is a source build,
// and calling it one keeps `upgrade` and the update notice honest about it —
// neither will touch a build that git owns.
var Version = "dev"

// claimTokenPrefix is what the registry mints every claim token with, so a
// second positional either looks like one or is a mistake worth naming.
const claimTokenPrefix = "krowk_claim_"

// helpTemplate is the human help. Its Usage block is not written here: it is
// rendered from the catalog, so a command cannot exist in the JSON surface and
// not in the text, or the other way round. Everything after it is prose, which
// is the half a struct has no way to say.
const helpTemplate = `krowk %s — permalinks for agent output

Usage
%s

Upload flags
  --run <slug|link>      Attach to an existing run instead of opening one, by
                         slug or by any link carrying one. On
                         ` + "`claim`" + ` and ` + "`uploads attach`" + ` it names the run an upload
                         already made joins — a claimed upload has none otherwise
  --pull-request <url>   Pull request the work belongs to
  --link <url>           Link this work is about — the issue, the spec, the
                         discussion. Repeat for more than one, up to 20, and
                         label or classify each with the two flags below
  --link-title <text>    What to call the --link before it, instead of its URL
  --link-rel <kind>      What the --link before it is: tracks, fixes, spec,
                         discussion, source, supersedes — or your own word
  --reference <id>       Related identifier that is not a URL, e.g. a ticket
                         key — repeat for more than one. A URL is a --link
  --title <text>         Title for the work, recorded on the run. What a pasted
                         link says about a file is that file's --caption
  --caption <text>       What this file shows, recorded on the artifact as
                         ` + "`krowk.caption`" + ` and used wherever it is pasted. Repeat
                         to caption several files in the order they are given
  --destination <tool>   Print what this tool wants pasted into it — the krowk
                         block for ` + "`github`" + `, ` + "`linear`" + ` and the like, the bare link for
                         the ones that unfurl it themselves, like ` + "`slack`" + `. A tool
                         krowk has not been told about gets the block
  --session <id>         Override the detected agent session
  --repo <owner/name>    Override the detected repository
  --commit <sha>         Override the detected commit
  --agent <name>         Override the detected agent
  --metadata <key=val>   Extra metadata — repeat for more than one. On push it
                         lands on each artifact; on ` + "`runs start`" + `, on the run.
                         Your value wins over a detected one. Metadata is public.

List flags
  --limit <n>            Rows per page (1–100, default 50)
  --before <slug>        Start after this row — the ` + "`next`" + ` of the last page
  --run <slug|link>      On ` + "`uploads list`" + `, narrow it to what one run produced

Auth flags
  --token <key>          Store this key rather than asking the browser — how CI
                         logs in, and it opens nothing
  --no-browser           Print the code and the page instead of opening a browser

Config flags
  --global               On ` + "`config set`" + ` and ` + "`config unset`" + `, write the machine-wide
                         file instead of the repository's

Global flags
  --workspace <name>     Use this workspace's stored key for this one command
  --dev                  Talk to a local registry at %s
  --format <fmt>         human | json | markdown | url (default: human on a TTY, json when piped)
                         markdown and url describe an upload; other commands fall back to json
  --json                 Shorthand for --format json
  --quiet                Raw JSON, no envelope
  --jq <expr>            Filter the JSON with a jq expression, built in — implies
                         --format json, and reads the bare record under --quiet
  -h, --help             Show this
  -v, --version          Print the version

Environment
  KROWK_TOKEN            API token — wins over the credentials file
  KROWK_WORKSPACE        Workspace to use, as if by --workspace
  KROWK_API_URL          API base URL (default %s)
  KROWK_DEV              1/true/yes/on — same as --dev
  KROWK_AGENT            Agent name to report
  KROWK_NO_UPDATE_CHECK  1/true/yes/on — never check for or mention new releases

Exit codes
  0  it worked
  1  the command was wrong, or krowk failed on its own — also anything unclassified
  2  not found — no such artifact or run in this workspace, or no such endpoint
  3  refused for want of credentials — no key, a key the registry rejects, a
     browser login somebody denied or that nothing on CI could approve, or no
     claim token where that is the only authority (a claim token the registry
     does not recognise is 2, since it answers that as no such record)
  4  refused by the registry on the request or the state of things — retrying
     unchanged answers the same
  5  rate limited — wait and retry
  6  the bytes did not move — the registry or object storage could not be reached
  7  the registry failed on its side, or answered something unreadable — or a
     login page krowk will not open, which is the same news
  8  gone — the artifact expired or was taken down, or a browser login lapsed
     before anybody approved it; no retry brings any of them back

Registry precedence: --dev, then KROWK_API_URL, then KROWK_DEV, then the default.

Run metadata — the pull request, the links, the references, the session — is
recorded on a run, and a run belongs to a workspace, so it needs an API key.
Without one an upload still works: it lands anonymously, expires within a day,
and comes back with a claim token that ` + "`krowk claim`" + ` spends to keep it.

Wherever an artifact or a run is named — a positional, or --run — a link that
carries it does just as well: the card page, the CDN URL under it, or anything
else krowk printed. A link carrying no slug of the kind the command wants, or
two different ones, is refused before anything is sent.

Taking an upload down removes the bytes at once and leaves the link reporting
that it was taken down. There is no undo and no confirmation — it is what to
reach for when something was published by accident. A key takes down anything in
its workspace; an upload that is still anonymous is taken down with the claim
token it came back with, passed after the slug.

Logging in goes through a browser. ` + "`krowk auth login`" + ` asks the registry to open
an authorization, prints a short code and opens the page that approves it;
approving mints a key and this command collects it, once. Over SSH or with no
display it prints the code and the page instead of opening anything, which is
what --no-browser asks for everywhere else. On CI it is refused outright, since
nothing there can approve it — that is what --token is for, and --token never
opens or waits for anything.

A key belongs to one workspace, and the credentials file holds one key per
workspace: logging in against a second workspace adds a key rather than
replacing the first. Which key a command uses is decided in order by
--workspace, KROWK_WORKSPACE, the repository's .krowk/config.json, the global
config, and finally whichever key logged in last. ` + "`krowk config set workspace <name>`" + `
pins a repository to a workspace, so every command run inside it — by anyone,
agent or person — lands there without saying so.

Credentials live in %s (0600).
Config lives in %s, and per repository in <git-root>/.krowk/config.json.
`

type flags struct {
	run         string
	before      string
	limit       int
	pullRequest string
	links       linkSlice
	references  stringSlice
	metadata    stringSlice
	caption     stringSlice
	destination string
	session     string
	title       string
	repo        string
	commit      string
	agent       string
	token       string
	workspace   string
	global      bool
	format      string
	dev         bool
	noBrowser   bool
	json        bool
	quiet       bool
	jq          string
	help        bool
	version     bool

	// filter is --jq once it has been compiled, and nil when there was none. It
	// rides along here rather than through every command's signature because it
	// applies to whatever a command renders and to nothing a command decides.
	//
	// tty and errTTY are that filter's other half: a filter prints strings raw,
	// so whether they have to be folded first depends on whether they are going
	// to a terminal. There are two because there are two streams and they are
	// answered separately — results go to stdout and failures to stderr, and
	// `krowk … --jq … > out.json` from a terminal is exactly the case where one
	// is a file and the other is not.
	filter *output.Filter
	tty    bool
	errTTY bool
}

type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// linkSlice collects --link, in the order the links were typed. Order is what
// makes --link-title and --link-rel unambiguous: each of them describes the
// --link before it, the way --caption describes the file at its position.
type linkSlice []runctx.Link

func (l *linkSlice) String() string {
	urls := make([]string, 0, len(*l))
	for _, link := range *l {
		urls = append(urls, link.URL)
	}
	return strings.Join(urls, ",")
}

func (l *linkSlice) Set(v string) error {
	*l = append(*l, runctx.Link{URL: v})
	return nil
}

// linkField is --link-title or --link-rel: it sets one field on the --link it
// follows. A second one for the same link, or one before any --link, is a
// mistake worth naming — silently overwriting would drop a label the caller
// typed, and attaching one to nothing would drop it entirely.
type linkField struct {
	links *linkSlice
	name  string
	field func(*runctx.Link) *string
	// given is the indexes of the links this flag has already described. It is
	// a set rather than a look at the field's emptiness because
	// `--link-title "$UNSET" --link-title fallback` is two flags, and reading
	// the first as absent is how the overwrite this refuses gets in anyway.
	given map[int]bool
}

func (f linkField) String() string { return "" }

func (f linkField) Set(v string) error {
	if len(*f.links) == 0 {
		return fmt.Errorf("--%s describes the --link before it, and none was given yet: "+
			"write --link <url> --%s %q", f.name, f.name, v)
	}
	at := len(*f.links) - 1
	if f.given[at] {
		return fmt.Errorf("--%s was given twice for the same --link (%s): "+
			"each link takes one, after the --link it belongs to",
			f.name, (*f.links)[at].URL)
	}
	f.given[at] = true
	*f.field(&(*f.links)[at]) = v
	return nil
}

// newFlagSet registers every flag krowk takes, against the struct that receives
// them. Split out of Run so the surface catalog can be held to it: the flag set
// is what actually parses a command line, so it — not a list written down
// somewhere — is the answer to which flags exist.
func newFlagSet(f *flags) *flag.FlagSet {
	fs := flag.NewFlagSet("krowk", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors go through the krowk envelope, not flag's

	fs.StringVar(&f.run, "run", "", "")
	fs.StringVar(&f.before, "before", "", "")
	fs.IntVar(&f.limit, "limit", 0, "")
	fs.StringVar(&f.pullRequest, "pull-request", "", "")
	fs.Var(&f.links, "link", "")
	fs.Var(linkField{links: &f.links, name: "link-title", given: map[int]bool{},
		field: func(l *runctx.Link) *string { return &l.Title }}, "link-title", "")
	fs.Var(linkField{links: &f.links, name: "link-rel", given: map[int]bool{},
		field: func(l *runctx.Link) *string { return &l.Rel }}, "link-rel", "")
	fs.Var(&f.references, "reference", "")
	fs.Var(&f.metadata, "metadata", "")
	fs.Var(&f.caption, "caption", "")
	fs.StringVar(&f.destination, "destination", "", "")
	fs.StringVar(&f.session, "session", "", "")
	fs.StringVar(&f.title, "title", "", "")
	fs.StringVar(&f.repo, "repo", "", "")
	fs.StringVar(&f.commit, "commit", "", "")
	fs.StringVar(&f.agent, "agent", "", "")
	fs.StringVar(&f.token, "token", "", "")
	fs.StringVar(&f.workspace, "workspace", "", "")
	fs.BoolVar(&f.global, "global", false, "")
	fs.StringVar(&f.format, "format", "", "")
	fs.BoolVar(&f.dev, "dev", false, "")
	fs.BoolVar(&f.noBrowser, "no-browser", false, "")
	fs.BoolVar(&f.json, "json", false, "")
	fs.BoolVar(&f.quiet, "quiet", false, "")
	fs.StringVar(&f.jq, "jq", "", "")
	fs.BoolVar(&f.help, "help", false, "")
	fs.BoolVar(&f.help, "h", false, "")
	fs.BoolVar(&f.version, "version", false, "")
	fs.BoolVar(&f.version, "v", false, "")
	return fs
}

// Run executes one invocation and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer, env func(string) string, isTTY, isErrTTY bool) int {
	var f flags
	fs := newFlagSet(&f)

	// Go's flag package stops at the first positional, so parse in a loop and
	// collect them. Lets flags follow filenames, the way agents write commands.
	positionals, parseErr := parseInterleaved(fs, args)

	// Whether --jq was typed, which is a different question from what it carries:
	// `--jq "$FIELD"` with the variable unset is a flag that was given and is
	// empty, and reading that as "no filter" would answer with everything.
	jqGiven := false
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "jq" {
			jqGiven = true
		}
	})

	// Resolve the format before reporting anything, including the parse error.
	// --jq asks for a value out of the JSON, so it settles the format the same way
	// --json does: there is nothing for a filter to read in a human row.
	format, formatErr := output.ResolveFormat(f.format, f.json || jqGiven, isTTY)
	if formatErr != nil {
		return report(stderr, formatErr, output.JSON, f.quiet, false)
	}
	colour := isTTY

	if parseErr != nil {
		// Reported unfiltered: the command line did not parse, so what --jq was
		// given — or whether it was given at all — is not something krowk knows.
		err := api.Fail("bad_flag", parseErr.Error()+" — run `krowk --help`")
		return report(stderr, err, format, f.quiet, colour)
	}

	// Compiled here rather than where it is used, so a mistyped expression is
	// refused before the command it belongs to uploads, claims or takes anything
	// down. A refusal about --jq is itself reported unfiltered, since running a
	// broken filter over the complaint about it would bury the complaint.
	filter, jqErr := output.CompileFilter(f.jq, jqGiven)
	// --destination picks a paste form, so it cannot ride alongside a flag that
	// picks a different one. Refused rather than silently ranked: whichever way
	// the tie were broken, the caller asked for two things and would be given
	// one without being told which.
	if jqErr == nil && f.destination != "" {
		if given := destinationConflict(f, jqGiven); given != "" {
			jqErr = api.Fail("bad_flag", "--destination prints the form that destination wants, "+
				"so it cannot be combined with "+given+" — drop one of the two")
		}
	}
	if jqErr == nil && filter != nil && f.format != "" && output.Format(f.format) != output.JSON {
		// Refused rather than overridden. --jq reads JSON and a paste form is not
		// JSON, so one of the two was going to be ignored — and this same change
		// stopped ignoring a --format that is merely misspelled. A --format spelled
		// right deserves at least as much.
		jqErr = api.Fail("bad_flag", "--jq reads the JSON, so it cannot be combined with "+
			"--format "+f.format+" — drop one of the two")
	}
	if jqErr != nil {
		return report(stderr, jqErr, format, f.quiet, colour)
	}
	f.filter = filter
	f.tty = isTTY
	f.errTTY = isErrTTY
	if err := filterHasSomethingToRead(filter, f, positionals); err != nil {
		return report(stderr, err, format, f.quiet, colour)
	}

	switch {
	case f.version:
		fmt.Fprintln(stdout, Version)
		return 0
	case f.help, len(positionals) == 0, positionals[0] == "help":
		// What was asked about: `krowk help uploads attach` and
		// `krowk uploads attach --help` are the same question, so the words are
		// taken from wherever they were typed and the leading `help` dropped.
		topic := positionals
		if len(topic) > 0 && topic[0] == "help" {
			topic = topic[1:]
		}
		if err := showHelp(stdout, topic, format, f); err != nil {
			return reportFiltered(stderr, err, format, f.quiet, colour, f.errTTY, f.filter)
		}
		return 0
	}

	var err error
	switch {
	case positionals[0] == "push":
		err = upload(stdout, positionals[1:], f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "uploads" && positionals[1] == "create":
		err = upload(stdout, positionals[2:], f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "uploads" && positionals[1] == "list":
		err = uploadsList(stdout, f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "uploads" && positionals[1] == "show":
		err = uploadsShow(stdout, positionals[2:], f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "uploads" && positionals[1] == "attach":
		err = uploadsAttach(stdout, positionals[2:], f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "uploads" && positionals[1] == "delete":
		err = uploadsDelete(stdout, positionals[2:], f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "runs" && positionals[1] == "start":
		err = runsStart(stdout, f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "runs" && positionals[1] == "list":
		err = runsList(stdout, f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "runs" && positionals[1] == "show":
		err = runsShow(stdout, positionals[2:], f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "runs" && positionals[1] == "finish":
		err = runsFinish(stdout, positionals[2:], f, format, env, colour)
	case positionals[0] == "claim":
		err = claim(stdout, positionals[1:], f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "auth" && positionals[1] == "login":
		err = authLogin(stdout, stderr, positionals[2:], f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "auth" && positionals[1] == "token":
		err = authToken(stdout, f, env)
	case len(positionals) > 1 && positionals[0] == "auth" && positionals[1] == "verify":
		err = authVerify(stdout, format, f, env, colour)
	case positionals[0] == "workspaces" && (len(positionals) == 1 || positionals[1] == "list"):
		err = workspacesList(stdout, f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "workspaces" && positionals[1] == "use":
		err = workspacesUse(stdout, positionals[2:], f, format, env, colour, isTTY)
	case len(positionals) > 1 && positionals[0] == "config" && positionals[1] == "show":
		err = configShow(stdout, f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "config" && positionals[1] == "set":
		err = configSet(stdout, positionals[2:], f, format, env, colour, isTTY)
	case len(positionals) > 1 && positionals[0] == "config" && positionals[1] == "unset":
		err = configUnset(stdout, positionals[2:], f, format, colour)
	case positionals[0] == "doctor":
		err = doctor(stdout, format, f, env)
	case positionals[0] == "upgrade":
		err = upgradeCmd(stdout, f, format, env, colour)
	default:
		err = api.Fail("unknown_command",
			"`"+strings.Join(clip(positionals, 2), " ")+"` is not a krowk command — run `krowk --help`")
	}

	if err != nil {
		return reportFiltered(stderr, err, format, f.quiet, colour, f.errTTY, f.filter)
	}
	// The nudge comes last, after the command's own output, and only when the
	// command worked: a failure has the floor. `upgrade` just answered the same
	// question.
	if positionals[0] != "upgrade" {
		maybeNotifyUpdate(stderr, env)
	}
	return 0
}

// report renders a failure and answers with the exit code that classifies it.
// Every failing path in Run goes through here, so there is one place where a
// failure becomes a number and no way to exit non-zero without printing why.
// A failure is filtered like any other result, so `--jq '.error.error'` reads
// the code out of a refusal the same way it reads a slug out of an upload — a
// caller that filters everything should not have to stop filtering to find out
// what went wrong. Two failures are never filtered: one caused by --jq, which
// output.IsFilterFailure names and which running the filter again would only
// bury, and one whose filtering fails, which falls back to the whole envelope
// rather than to silence.
func report(w io.Writer, err error, format output.Format, quiet, colour bool) int {
	return reportFiltered(w, err, format, quiet, colour, false, nil)
}

// reportFiltered is report for the paths that have a filter to run over the
// failure. The two terminal questions are separate here: colour is stdout's,
// which is what krowk has always painted a failure by, and errTTY is this
// writer's, which is the only honest answer to whether a string printed raw
// here could repaint something.
func reportFiltered(w io.Writer, err error, format output.Format, quiet, colour, errTTY bool,
	filter *output.Filter) int {
	rendered := output.Error(err, format, quiet, colour)
	if filter != nil && !output.IsFilterFailure(err) {
		// Into a buffer first, because a filter can write several values and then
		// fail on one. Writing straight to stderr would leave those first values
		// standing in front of the whole envelope this falls back to, and a caller
		// reading a failure would have to work out which half was the answer.
		var filtered bytes.Buffer
		// An expression that matches nothing is silence on a result, and silence is
		// an answer there. On a failure it is not: exiting non-zero having printed
		// nothing anywhere leaves a caller a number and no reason for it, which is
		// the one thing this function exists to prevent.
		said, filterErr := filter.Write(&filtered, rendered, errTTY)
		// said, not the byte count. An expression written for a result — the very
		// one the docs hand out, `.data.artifacts[0].url` — answers `null` over a
		// failure, and printing that instead of the envelope would leave a caller
		// five bytes and an exit code where the reason should be.
		if filterErr == nil && said > 0 {
			fmt.Fprint(w, filtered.String())
			return exitCodeFor(err)
		}
		if filterErr != nil {
			// Said before the envelope, and not instead of it. Otherwise whether a
			// caller ever learns their expression is broken depends on whether the
			// registry happened to answer — the same expression against a command
			// that worked would have failed loudly.
			fmt.Fprintln(w, output.Error(filterErr, format, quiet, colour))
		}
	}
	fmt.Fprintln(w, rendered)
	return exitCodeFor(err)
}

// filterHasSomethingToRead refuses --jq where the command answers with something
// other than JSON.
//
// `auth token` prints the bare token so `$(krowk auth token)` works, and
// `--version` prints the version. Neither goes through emit, so a filter would
// be silently skipped and the caller handed an unfiltered line — from `auth
// token`, the raw secret where an expression asked for one field of it. A flag
// that quietly does nothing is what the catalog exists to make impossible, so
// which commands those are is read off the catalog rather than written out again
// here: a command added later carries the answer or does not, and the surface
// test notices either way.
//
// Help is not one of them. `krowk auth token --help` prints the catalog entry,
// which is JSON like any other, and refusing it would make the two spellings of
// one question — that and `krowk help auth token` — disagree.
func filterHasSomethingToRead(filter *output.Filter, f flags, positionals []string) error {
	if filter == nil {
		return nil
	}
	name := ""
	// The order is Run's dispatch order, and has to stay it: --version is answered
	// before --help, so treating --help as "this prints a catalog entry" first
	// would wave `krowk --version --help --jq …` straight past the check and print
	// the version unfiltered.
	switch {
	case f.version:
		name = "--version"
	case f.help, len(positionals) > 0 && positionals[0] == "help":
		return nil
	default:
		// Find answers with the leaf, whose Name is the last word of it, so the
		// message says what was typed rather than "`token`".
		cmd, ok := Surface().Find(positionals)
		if !ok || !cmd.NoJSON {
			return nil
		}
		name = strings.Join(clip(positionals, 2), " ")
	}
	return api.Fail("jq_unsupported", "`"+name+"` prints no JSON, so there is nothing for --jq to read"+
		" — drop the flag, or ask a command that answers with a record")
}

// emit is where a rendered result reaches the caller, and the one place a --jq
// filter gets to touch one. Every command goes through it, so a filter can never
// apply to some results and not others — a caller reading one field out of a
// push must be able to read the same field out of a claim.
func emit(w io.Writer, rendered string, f flags) error {
	if f.filter == nil {
		fmt.Fprintln(w, rendered)
		return nil
	}
	// Into a buffer, so a filter that writes several values and then fails on one
	// writes none of them. Half a listing on stdout followed by an exit code is
	// worse than no listing: `SLUGS=$(krowk uploads list --jq …)` would hold a
	// prefix of the answer with nothing marking where it stopped.
	var filtered bytes.Buffer
	if _, err := f.filter.Write(&filtered, rendered, f.tty); err != nil {
		return afterTheCommandRan(err)
	}
	fmt.Fprint(w, filtered.String())
	return nil
}

// afterTheCommandRan marks a filter failure that happened once the command had
// already done whatever it does.
//
// The command succeeded and only the reading of it failed, which is worth a
// sentence: `krowk push shot.png --jq '.data.artifacts.url'` is a plausible typo
// for `.data.artifacts[0].url`, it compiles, so the up-front check cannot catch
// it — and the upload has landed by the time it does. A wrapper that retries on
// a non-zero exit would upload the file twice. Nothing here can stop it
// retrying, but the message can say what a retry would be repeating.
//
// It says the command ran rather than telling anyone not to run it again: most
// commands are reads, and warning someone off `uploads list` is the opposite of
// useful.
func afterTheCommandRan(err error) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	return api.Fail(apiErr.Code(), apiErr.Fix()+
		". The command itself succeeded — this is the reading of its result failing, "+
		"so running it again repeats whatever it did")
}

// newClient is the one place a registry client gets built, so every command
// honours the same precedence twice over: --dev, then KROWK_API_URL, then
// KROWK_DEV for where to send requests; and --workspace, then KROWK_WORKSPACE,
// then the repo config, then the global config, then the stored default for
// which key to send them with.
//
// A workspace that resolved by name but holds no key is a refusal, not a
// fallback. Every layer that can name one is somebody's deliberate ask — a
// flag typed, a variable exported, a config committed — and quietly uploading
// anonymously instead would land the artifact in the one place the ask
// existed to avoid. KROWK_TOKEN is the exception and stays the strongest
// word: CI injects a token into a checkout whose committed config names a
// workspace that machine never logged into, and the token is the key that was
// meant.
func newClient(f flags, env runctx.Env) (*api.Client, error) {
	// KROWK_TOKEN first, before any config file is even opened. The variable is
	// documented as the strongest word, and CI is why: a job exports a token
	// into a checkout whose committed config it does not control, and a config
	// file that machine cannot even parse must not be able to take the token's
	// place in line — the pre-workspace behaviour, kept on purpose.
	if token := env("KROWK_TOKEN"); token != "" {
		return api.New(api.BaseURLFor(f.dev, env), token), nil
	}
	ws, _, err := resolveWorkspace(f, env)
	if err != nil {
		return nil, err
	}
	token, err := api.ResolveToken(env, ws)
	if err != nil {
		return nil, err
	}
	return api.New(api.BaseURLFor(f.dev, env), token), nil
}

// workspaceKeyMissing reports whether a newClient failure is about a workspace
// key the store could not produce — as against a config file that could not be
// read, which is a broken setup no fallback should paper over.
func workspaceKeyMissing(err error) bool {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	code := apiErr.Code()
	return code == "no_key_for_workspace" || code == "dangling_default"
}

// resolveWorkspace answers which workspace this command was pointed at, and by
// whom. Empty means nothing asked, which is not the anonymous case — it is
// "use the stored default", and only the credential store knows whether that
// is a key or nothing.
func resolveWorkspace(f flags, env runctx.Env) (name, source string, err error) {
	cfg, err := config.Load("", env, config.Overrides{Workspace: f.workspace})
	if err != nil {
		// The file exists and somebody wrote it meaning to steer uploads, so it
		// does not get shrugged off: acting as though it said nothing would send
		// uploads somewhere the file visibly tries to prevent.
		return "", "", api.Fail("bad_config", err.Error()+
			" — fix the file or remove it; `krowk config show` names every layer")
	}
	return cfg.Workspace, cfg.Sources["workspace"], nil
}

// registryMode says where the client is pointed, so `doctor` can tell a
// deliberate local run from a stray KROWK_API_URL.
func registryMode(client *api.Client, env runctx.Env) string {
	switch {
	case client.BaseURL == strings.TrimRight(api.DevBaseURL, "/"):
		return "local"
	case env("KROWK_API_URL") != "":
		return "custom (KROWK_API_URL)"
	}
	return "production"
}

// parseInterleaved lets `krowk uploads create a.png --session=x b.png` work.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return positionals, err
		}
		if fs.NArg() == 0 {
			return positionals, nil
		}
		positionals = append(positionals, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// upload is the whole point of the CLI: every file becomes its own artifact, and
// a run is what groups them and carries the metadata about where they came from.
func upload(w io.Writer, files []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	if len(files) == 0 {
		return api.Fail("no_file", "pass at least one path: `krowk push screenshot.png`")
	}
	extras, err := parseMetadata(f.metadata)
	if err != nil {
		return err
	}
	captions, err := captionsFor(f.caption, len(files))
	if err != nil {
		return err
	}
	if err := checkLinks(f.links); err != nil {
		return err
	}
	if f, err = withRunFlag(f); err != nil {
		return err
	}

	// Every file is measured and digested before anything is sent, so a typo in
	// the last path fails before the first upload rather than halfway through.
	specs := make([]api.Spec, 0, len(files))
	for _, path := range files {
		spec, err := api.Inspect(path)
		if err != nil {
			return err
		}
		specs = append(specs, spec)
	}

	client, err := newClient(f, env)
	if err != nil {
		return err
	}
	ctx := context.Background()

	result := output.Result{Title: f.title}

	runSlug, ownRun, err := resolveRun(ctx, client, f, env, &result)
	if err != nil {
		return err
	}

	// Every push stamps the artifact with the state it finds at its own moment:
	// the production record travels with the file, wherever it is later claimed
	// or attached. Keyless uploads record no metadata — the note above says so,
	// and the state is not even detected for them.
	//
	// The stamp is shared and the caption is not: a caption names one file, so
	// it is laid over the stamp per file rather than folded into it.
	var stamp runctx.Metadata
	authenticated := client.Authenticated()
	if authenticated {
		stamp = metadataFor(f, env).Artifact()
	}

	for i, spec := range specs {
		spec.Run = runSlug
		if authenticated {
			spec.Metadata = stamp.WithExtras(withCaption(extras, captions, i))
		}
		artifact, err := client.Push(ctx, spec)
		if err != nil {
			return withProgress(err, result.Artifacts, runSlug, ownRun)
		}
		result.Artifacts = append(result.Artifacts, artifact)
	}

	// A run this command opened is a run this command closes. Failing to close it
	// is not worth failing the upload over — the artifacts are up and their links
	// work — but it is worth saying: the run stays open until someone retries.
	if ownRun {
		if finished, err := client.FinishRun(ctx, runSlug); err == nil {
			result.Run = finished
		} else {
			result.Notes = append(result.Notes, "run "+runSlug+" could not be finished: "+
				errCode(err)+" — retry `krowk runs finish "+runSlug+"`")
		}
	}

	if f.destination != "" {
		return emit(w, output.Destination(result, f.destination), f)
	}
	return emit(w, output.Upload(result, format, f.quiet, colour, time.Now()), f)
}

// resolveRun decides which run the artifacts belong to: the one named on the
// command line, a fresh one carrying the detected metadata, or none at all when
// there is no key to open one with.
func resolveRun(ctx context.Context, client *api.Client, f flags, env runctx.Env, result *output.Result) (slug string, own bool, err error) {
	if f.run != "" {
		// The caller manages this run's lifecycle, so it is not finished here —
		// and its metadata was recorded when it was opened, so flags describing
		// the work have nowhere to land. Which is worth saying out loud.
		if note := namedRunMetadataNote(f); note != "" {
			result.Notes = append(result.Notes, note)
		}
		return f.run, false, nil
	}
	if !client.Authenticated() {
		if note := anonymousMetadataNote(f); note != "" {
			result.Notes = append(result.Notes, note)
		}
		return "", false, nil
	}

	run, err := client.CreateRun(ctx, metadataFor(f, env))
	if err != nil {
		return "", false, err
	}
	result.Run = run
	return run.Slug, true, nil
}

// metadataFor is everything worth remembering about where an upload came from.
// Flags win; the rest is detected so the agent never has to type it. Resolve is
// the shared path — the MCP server goes through it too, so a new metadata field
// lands in both or neither.
func metadataFor(f flags, env runctx.Env) runctx.Metadata {
	return runctx.Resolve(env, runctx.Overrides{
		Repo:        f.repo,
		Commit:      f.commit,
		Agent:       f.agent,
		PullRequest: f.pullRequest,
		Links:       f.links,
		References:  f.references,
		Session:     f.session,
		Title:       f.title,
		Client:      "krowk-cli/" + Version,
	})
}

// checkLinks holds --link to the vocabulary before anything is sent, so a
// malformed URL fails the command rather than landing in a record that is
// stored verbatim and never validated again.
func checkLinks(links linkSlice) error {
	if err := runctx.ValidateLinks(links); err != nil {
		return api.Fail("bad_flag", "--link: "+err.Error())
	}
	return nil
}

// parseMetadata turns repeated --metadata key=value pairs into a map. The value
// may contain '='; the key may not be empty. Values are stored verbatim as
// strings — the registry never learns shapes, and neither does this flag.
func parseMetadata(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, api.Fail("bad_flag",
				"--metadata takes key=value, like --metadata krowk.caption=\"before the fix\"")
		}
		out[key] = value
	}
	return out, nil
}

// captionKey is the artifact metadata key a caption is recorded under. It is
// artifact data rather than paste-time freehand, so whatever renders a paste —
// the registry, a card page, an integration — reads the caption from the record
// instead of being told it again at every destination.
const captionKey = "krowk.caption"

// destinationConflict names the flag --destination was asked to share the output
// with, or "" when there is none. --json and --jq are conflicts as much as
// --format is: both ask for the envelope, and a paste form is not it.
func destinationConflict(f flags, jqGiven bool) string {
	switch {
	case f.format != "":
		return "--format " + f.format
	case jqGiven:
		return "--jq"
	case f.json:
		return "--json"
	}
	return ""
}

// captionsFor lines the captions up with the files they describe. A caption
// names one file, so a push of three files either captions all three or none of
// them; the single-caption case is spread across the set, which is what a
// before/after pair of the same subject wants and the only reading of one
// caption and several files that is not a guess.
func captionsFor(captions []string, files int) ([]string, error) {
	switch {
	case len(captions) == 0:
		return nil, nil
	case len(captions) == 1 && files > 1:
		return slices.Repeat(captions, files), nil
	case len(captions) != files:
		return nil, api.Fail("bad_flag", fmt.Sprintf(
			"--caption was given %d times for %d files: pass one caption per file, "+
				"in the order the files are, or a single one for all of them",
			len(captions), files))
	}
	return captions, nil
}

// withCaption lays one file's caption over the shared extras. The caller's own
// --metadata krowk.caption still wins, because a key spelled out by hand is a
// correction and this flag is the shorthand for it.
func withCaption(extras map[string]string, captions []string, i int) map[string]string {
	if i >= len(captions) {
		return extras
	}
	if _, spelled := extras[captionKey]; spelled {
		return extras
	}
	out := make(map[string]string, len(extras)+1)
	maps.Copy(out, extras)
	out[captionKey] = captions[i]
	return out
}

// anonymousMetadataNote says plainly that metadata asked for by name was not
// recorded. Silently dropping it would leave an agent believing the pull request
// it named is attached to the upload, and it is not.
func anonymousMetadataNote(f flags) string {
	given := append(runMetadataGiven(f), artifactMetadataGiven(f)...)
	if len(given) == 0 {
		return ""
	}
	return strings.Join(given, ", ") + " was not recorded: a keyless upload records no metadata — " +
		"run `krowk auth login --token krowk_sk_...`"
}

// namedRunMetadataNote is the same honesty for the other path that drops
// metadata: a run's metadata was recorded when the run was opened, so flags
// describing the work are dropped when --run names one that already exists.
// Said rather than refused, because the flags are harmless in a script that
// pushes to a run it opened earlier with the same arguments — and because
// --caption and --metadata land on the artifact and are recorded either way.
func namedRunMetadataNote(f flags) string {
	given := runMetadataGiven(f)
	if len(given) == 0 {
		return ""
	}
	return strings.Join(given, ", ") + " was not recorded: run " + f.run +
		" already carries the metadata it was opened with — pass these to " +
		"`krowk runs start`, or drop --run and let this push open its own run"
}

// runMetadataGiven names the flags that describe the work, which is what a run
// carries; artifactMetadataGiven names the ones that describe one file. The two
// are separate because the paths that drop metadata drop different halves of it.
func runMetadataGiven(f flags) []string {
	var given []string
	if f.pullRequest != "" {
		given = append(given, "--pull-request")
	}
	if len(f.links) > 0 {
		given = append(given, "--link")
	}
	if len(f.references) > 0 {
		given = append(given, "--reference")
	}
	if f.session != "" {
		given = append(given, "--session")
	}
	if f.title != "" {
		given = append(given, "--title")
	}
	return given
}

func artifactMetadataGiven(f flags) []string {
	var given []string
	if len(f.metadata) > 0 {
		given = append(given, "--metadata")
	}
	if len(f.caption) > 0 {
		given = append(given, "--caption")
	}
	return given
}

// withProgress keeps what a failed batch would otherwise lose: the links of
// whatever did upload, and the run this command opened. Without the slug the
// run is unrecoverable — the error body is all the caller ever sees of it.
func withProgress(err error, done []*api.Artifact, runSlug string, ownRun bool) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	if len(done) > 0 {
		urls := make([]string, 0, len(done))
		for _, a := range done {
			urls = append(urls, a.URL)
		}
		apiErr.Body["uploaded_before_failure"] = urls
	}
	if ownRun {
		apiErr.Body["run"] = runSlug
		finish := "the run is still open — close it with `krowk runs finish " + runSlug + "`"
		if fix, _ := apiErr.Body["fix"].(string); fix != "" {
			finish = fix + "; " + finish
		}
		apiErr.Body["fix"] = finish
	}
	return apiErr
}

// withClaimed says what a failed attach leaves behind: the claim succeeded and
// the token is spent, so re-running the whole command would fail on the claim and
// look like the artifact was lost. Only the attach is left to retry.
//
// The artifact slug is spelled out because it is the fact that would otherwise be
// lost — the error body is all the caller sees of it. The run is left as a
// placeholder: quoting back the slug that just 404'd would contradict the same
// sentence's advice to check it.
func withClaimed(err error, claimed *api.Artifact) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	apiErr.Body["claimed"] = claimed.Slug
	retry := "the upload is claimed and kept, only the run is not attached — retry `krowk uploads attach " +
		claimed.Slug + " --run <run>` with a run this workspace holds"
	if fix, _ := apiErr.Body["fix"].(string); fix != "" {
		retry = fix + "; " + retry
	}
	apiErr.Body["fix"] = retry
	return apiErr
}

// errCode names an error the way the envelope would, so a note can carry it.
func errCode(err error) string {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code()
	}
	return err.Error()
}

// withRunFlag reads --run the way the positionals are read: the slug, or a link
// carrying it. Called by the four commands that consume the flag rather than
// once in Run, because a flag nothing reads has to stay a flag nothing reads —
// resolving it centrally made `krowk doctor --run <link>` fail on a value doctor
// never looks at, and answered an unknown command with a complaint about its
// flags instead of naming the command.
func withRunFlag(f flags) (flags, error) {
	slug, err := api.ParseSlug(api.KindRun, f.run)
	if err != nil {
		return f, err
	}
	f.run = slug
	return f, nil
}

// slugArg reads the positional every command that names one record takes: the
// slug, or — since the pitch of the product is the pasted link — any link that
// carries it. The missing case stays each command's own words, because "pass the
// artifact" is only useful when it shows that command's spelling of it.
func slugArg(kind string, args []string, missing, fix string) (string, error) {
	// Blank counts as absent: `krowk uploads show "$SLUG"` with the variable
	// unset arrives as one empty word, and the answer to that is the command's
	// own "pass the artifact" rather than a refusal from the registry about a
	// record with no name.
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return "", api.Fail(missing, fix)
	}
	return api.ParseSlug(kind, args[0])
}

// uploadsList pages through the key's workspace. Needs a key: keyless requests
// all share the anonymous workspace, so there is nothing of one's own to list.
//
// --run narrows it to what one run produced, which the registry serves as a
// collection of that run rather than as a filter here. The difference is worth
// keeping: an unknown run is a 404 from the run itself, where a filter would
// answer an empty page — and a caller cannot tell that apart from a run that
// genuinely produced nothing.
func uploadsList(w io.Writer, f flags, format output.Format, env runctx.Env, colour bool) error {
	f, err := withRunFlag(f)
	if err != nil {
		return err
	}
	client, err := newClient(f, env)
	if err != nil {
		return err
	}
	ctx := context.Background()

	var page *api.Page
	if f.run != "" {
		page, err = client.ListRunArtifacts(ctx, f.run, f.before, f.limit)
	} else {
		page, err = client.ListArtifacts(ctx, f.before, f.limit)
	}
	if err != nil {
		return err
	}
	// The listing's own scope travels into the next-page command, --limit
	// included: an agent paging by 10 that was handed a crumb paging by 50 would
	// change stride halfway through the walk without being told.
	listing := output.Listing{Run: f.run, Limit: f.limit}
	return emit(w, output.List(page, listing, format, f.quiet, colour, time.Now()), f)
}

// runsList pages through the key's runs, newest first.
func runsList(w io.Writer, f flags, format output.Format, env runctx.Env, colour bool) error {
	client, err := newClient(f, env)
	if err != nil {
		return err
	}

	page, err := client.ListRuns(context.Background(), f.before, f.limit)
	if err != nil {
		return err
	}
	return emit(w, output.RunList(page, output.Listing{Limit: f.limit}, format, f.quiet, colour), f)
}

// runsShow reads one run back with everything recorded on it. That is where an
// upload's origin lives — the pull request, the commit, the session — since the
// registry keeps none of it on the artifact.
func runsShow(w io.Writer, args []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	slug, err := slugArg(api.KindRun, args, "no_run", "pass the run: `krowk runs show run_...`")
	if err != nil {
		return err
	}
	client, err := newClient(f, env)
	if err != nil {
		return err
	}

	run, err := client.ShowRun(context.Background(), slug)
	if err != nil {
		return err
	}
	return emit(w, output.RunDetail(run, format, f.quiet, colour), f)
}

func uploadsShow(w io.Writer, args []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	slug, err := slugArg(api.KindArtifact, args, "no_artifact",
		"pass the artifact: `krowk uploads show art_...`")
	if err != nil {
		return err
	}
	client, err := newClient(f, env)
	if err != nil {
		return err
	}

	artifact, err := client.ShowArtifact(context.Background(), slug)
	if err != nil {
		return err
	}
	return emit(w, output.Artifact(artifact, format, f.quiet, colour, time.Now()), f)
}

// uploadsAttach puts an upload under a run after the fact. It is the only way an
// upload that started out anonymous ever gets one: it could not name a run when
// it was created, and claiming it does not give it one.
func uploadsAttach(w io.Writer, args []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	slug, err := slugArg(api.KindArtifact, args, "no_artifact",
		"pass the artifact: `krowk uploads attach art_... --run run_...`")
	if err != nil {
		return err
	}
	if strings.TrimSpace(f.run) == "" {
		return api.Fail("no_run", "pass the run to attach it to: `krowk uploads attach "+slug+" --run run_...`")
	}
	if f, err = withRunFlag(f); err != nil {
		return err
	}
	client, err := newClient(f, env)
	if err != nil {
		return err
	}

	artifact, err := client.AttachRun(context.Background(), slug, f.run)
	if err != nil {
		return err
	}
	return emit(w, output.Artifact(artifact, format, f.quiet, colour, time.Now()), f)
}

// uploadsDelete takes an upload down: the bytes leave storage at once and the
// link reports it afterwards rather than answering as though it never existed.
//
// Not confirmed and not undoable, both deliberately. This is the command reached
// for when a secret was published by accident — a secret that can be restored is
// still leaked, and a prompt in the way is one more moment it stays up. The
// caller is usually an agent or a script, which cannot answer a prompt at all.
//
// The claim token is optional because there are two authorities and they belong
// to different callers: a key speaks for everything in its workspace, and a
// claim token speaks for the one anonymous upload it was issued with.
func uploadsDelete(w io.Writer, args []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	slug, err := slugArg(api.KindArtifact, args, "no_artifact",
		"pass the artifact: `krowk uploads delete art_...`")
	if err != nil {
		return err
	}
	var claimToken string
	if len(args) > 1 {
		// Checked rather than taken as given, because a second word that is not a
		// token is not a wrong token — and passing one withholds the key, so a
		// stray argument would quietly turn an authorised takedown into an
		// unauthorised one and report it as a 404. `uploads delete art_a art_b`,
		// meaning two artifacts, is the way that happens.
		//
		// Trimmed because it was pasted, and not quoted back because of what a
		// paste in this position can be: the second link of two, signature and
		// all, which a refusal would then write into stderr and the envelope.
		claimToken = strings.TrimSpace(args[1])
		if !strings.HasPrefix(claimToken, claimTokenPrefix) {
			return api.Fail("bad_claim_token",
				"the second argument is not a claim token — takedown takes one artifact and, "+
					"after it, only the `"+claimTokenPrefix+"...` token that upload came back with")
		}
	}

	client, err := newClient(f, env)
	if err != nil {
		// A claim token is its own authority — it speaks for the one anonymous
		// upload it was issued with, and needs no key at all. So a workspace
		// that resolved and has no key, which rightly stops an upload, must not
		// stop a token-authorised takedown: the contributor with a claim token
		// in a repo pinned to a workspace they never logged into is exactly who
		// runs this. Everything else about the failure still stands, so it is
		// only stepped around when the token is present to take over.
		if claimToken == "" || !workspaceKeyMissing(err) {
			return err
		}
		client = api.New(api.BaseURLFor(f.dev, env), "")
	}
	// An anonymous upload's only authority is its claim token, so saying plainly
	// that there is none beats a 400 from the registry naming a parameter the
	// caller never saw a flag for.
	if claimToken == "" && !client.Authenticated() {
		return api.Fail("missing_claim",
			"taking down an anonymous upload needs the claim token it came back with: "+
				"`krowk uploads delete "+slug+" krowk_claim_...` — with an API key, the key is authority enough")
	}

	if err := client.TakeDownArtifact(context.Background(), slug, claimToken); err != nil {
		return withTakedownAuthority(err, claimToken != "")
	}
	return emit(w, output.Removed(slug, format, f.quiet, colour), f)
}

// withTakedownAuthority says what a 404 from a takedown actually means. The
// registry answers the same "no such record" for a slug that does not exist, one
// held by another workspace, and a claim token that does not match — a wrong
// guess learns nothing from the difference — so the client is what names the
// possibilities, and which of them apply depends on the authority it sent.
//
// The standing advice for a 404 names the key and its workspace, which is the
// wrong thing to check when the caller authorised with a token instead.
func withTakedownAuthority(err error, byToken bool) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "not_found" {
		return err
	}
	if byToken {
		apiErr.Body["fix"] = "no anonymous upload answers to that slug and token — check both were " +
			"copied whole, and note that claiming one spends the token, after which the key that " +
			"claimed it is what takes it down"
	} else {
		apiErr.Body["fix"] = "this workspace holds no such upload — check the slug, and that the key " +
			"matches the workspace it was uploaded to; an upload that is still anonymous is taken " +
			"down with its claim token instead"
	}
	return apiErr
}

func runsStart(w io.Writer, f flags, format output.Format, env runctx.Env, colour bool) error {
	if err := checkLinks(f.links); err != nil {
		return err
	}
	client, err := newClient(f, env)
	if err != nil {
		return err
	}

	extras, err := parseMetadata(f.metadata)
	if err != nil {
		return err
	}
	run, err := client.CreateRun(context.Background(), metadataFor(f, env).WithExtras(extras))
	if err != nil {
		return err
	}
	return emit(w, output.Run(run, format, f.quiet, colour), f)
}

func runsFinish(w io.Writer, args []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	slug, err := slugArg(api.KindRun, args, "no_run", "pass the run: `krowk runs finish run_...`")
	if err != nil {
		return err
	}
	client, err := newClient(f, env)
	if err != nil {
		return err
	}

	run, err := client.FinishRun(context.Background(), slug)
	if err != nil {
		return err
	}
	return emit(w, output.Run(run, format, f.quiet, colour), f)
}

// claim spends the token an anonymous upload came back with, which is the only
// way to keep that upload past its expiry.
//
// --run is the moment to name a run, because the caller holding the claim token
// is the one that knows which run the upload came from. The order is fixed: the
// attach resolves both slugs in the key's workspace, so it only works once the
// claim has moved the artifact there.
func claim(w io.Writer, args []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	if len(args) < 2 {
		return api.Fail("missing_claim",
			"pass both the artifact and its token: `krowk claim art_... krowk_claim_...`")
	}
	// no_artifact rather than missing_claim: a blank first positional is the
	// command being wrong, and missing_claim is classified as a credential the
	// caller has to produce — which would send a script off to check its key over
	// an argument that was never filled in.
	slug, err := slugArg(api.KindArtifact, args, "no_artifact",
		"pass both the artifact and its token: `krowk claim art_... krowk_claim_...`")
	if err != nil {
		return err
	}
	if f, err = withRunFlag(f); err != nil {
		return err
	}
	client, err := newClient(f, env)
	if err != nil {
		return err
	}
	ctx := context.Background()

	artifact, err := client.ClaimArtifact(ctx, slug, strings.TrimSpace(args[1]))
	if err != nil {
		return err
	}
	if f.run != "" {
		// The claim has already been spent, so a failure here must not read as one
		// the caller can undo by running the whole thing again: the artifact is kept,
		// and only the run is missing.
		attached, err := client.AttachRun(ctx, artifact.Slug, f.run)
		if err != nil {
			return withClaimed(err, artifact)
		}
		artifact = attached
	}
	// Claimed rather than Artifact: a claim that named no run leaves the upload
	// in a workspace and under nothing, and `uploads attach` is the only way it
	// ever gets one. That is worth handing back as a command rather than leaving
	// to be discovered in the help.
	return emit(w, output.Claimed(artifact, format, f.quiet, colour, time.Now()), f)
}

// authLogin gets a key onto this machine, one of two ways, and which one is
// asked for is decided by whether a key was handed over.
//
// With no --token the browser does the work: the registry opens an authorization,
// someone approves it, and the key that approval mints comes back over the wire.
// That is the default because it is the one that works from a container, where
// the alternative was a person with a dashboard open pasting a key in.
//
// --token stores a key that already exists, which is how CI logs in and is
// unchanged. --no-browser alongside it is not a contradiction and not an error:
// it asks for no browser, and this path opens none.
func authLogin(stdout, stderr io.Writer, args []string, f flags, format output.Format,
	env runctx.Env, colour bool) error {
	// A key typed without its flag is the mistake worth catching by name. Nothing
	// reads a positional here, so it would otherwise be dropped on the floor and the
	// command would go and wait a quarter of an hour for somebody to approve a
	// browser login that was never wanted.
	//
	// The stray is not quoted back when it looks like a key. It is already in a shell
	// history, which is bad enough without also putting it in whatever captured this
	// command's stderr.
	if len(args) > 0 {
		if strings.HasPrefix(args[0], "krowk_sk_") {
			return api.Fail("token_not_a_positional",
				"a key has to go behind the flag: `krowk auth login --token krowk_sk_...` — "+
					"passed as a bare argument it is ignored")
		}
		return api.Fail("unexpected_argument",
			"`krowk auth login` takes no arguments, and got `"+strings.Join(clip(args, 2), " ")+
				"` — the key goes behind --token")
	}
	if f.token == "" {
		return authLoginInBrowser(stdout, stderr, f, format, env, colour)
	}
	return authLoginWithToken(stdout, f, format, env, colour)
}

// authLoginWithToken stores a key, but only once the registry has been given the
// chance to reject it. Storing first and finding out later meant a mistyped key
// was discovered by the next upload failing, at which point the paste buffer
// that held the real one is long gone.
//
// A rejection is fatal, and deliberately so: keeping a key the registry has
// just disowned achieves nothing, and writing it over a working key would leave
// the machine worse off than not logging in at all. Every other outcome — no
// network, a registry that is down, a URL answering with something that is not
// a key — stores the token anyway. Logging in before a flight is a real thing
// to do, and none of those outcomes is evidence about the key.
//
// What the registry said, when it said anything, is written alongside the
// token, so nothing afterwards has to ask again which workspace this key acts
// in.
func authLoginWithToken(w io.Writer, f flags, format output.Format, env runctx.Env, colour bool) error {
	// The key being stored, not the one already configured: verifying whatever
	// is in the environment would happily bless a typo in the one that is about
	// to be written to disk.
	key, verifyErr := api.New(api.BaseURLFor(f.dev, env), f.token).VerifyKey(context.Background())
	if api.KeyRejected(verifyErr) {
		// The standing advice for a rejected key is to log in again, which is
		// what just happened. Point at the two things that are actually left.
		var apiErr *api.Error
		if errors.As(verifyErr, &apiErr) {
			apiErr.Body["fix"] = "the registry does not accept this key — check it was pasted whole, " +
				"or issue a new one in the dashboard"
		}
		return verifyErr
	}

	var id api.Identity
	if verifyErr == nil {
		id = api.Identity{KeyID: key.KeyID, Workspace: key.Workspace, WorkspaceName: key.WorkspaceName}
	}

	path, err := api.SaveCredentials(f.token, id)
	if err != nil {
		return api.Fail("credentials_unwritable", "could not write "+api.CredentialsPath()+": "+err.Error())
	}

	result := &output.Login{Path: path, Confirmed: verifyErr == nil, Shadowed: env("KROWK_TOKEN") != ""}
	if verifyErr != nil {
		result.Reason = unconfirmedReason(verifyErr)
	} else {
		result.KeyID, result.Workspace = key.KeyID, key.Workspace
	}
	return emit(w, output.StoredKey(result, format, f.quiet, colour), f)
}

// How long a browser login is given, and how often it is asked about. Both are
// the registry's to decide — it says `expires_at` and `interval` — and both are
// bounded here, because a registry answering something absurd must not be able
// to make krowk hammer it, fall asleep against it, or leave a forgotten terminal
// polling for the rest of the afternoon.
const (
	defaultAuthorizationWindow = 15 * time.Minute
	minAuthorizationWindow     = time.Minute
	maxAuthorizationWindow     = 30 * time.Minute

	defaultPollInterval = 5 * time.Second
	minPollInterval     = time.Second
	maxPollInterval     = 30 * time.Second
)

// authLoginInBrowser mints a key by having someone approve the request in a
// browser, so a machine that has never held a key can get one without a person
// pasting it in.
//
// Two capabilities, and keeping them apart is the design rather than a detail.
// The registry answers with a slug and a short code: the slug is what this
// process polls and is never shown in the browser, the code is what a person
// confirms and can only approve or deny. So the half that travels — through a
// terminal, a chat message, a colleague reading it out — cannot be turned into a
// key by whoever sees it.
//
// The key arrives exactly once. After the read that collects it the registry has
// no plaintext left, so nothing can ask for it a second time, including this
// command run again by mistake.
func authLoginInBrowser(stdout, stderr io.Writer, f flags, format output.Format,
	env runctx.Env, colour bool) error {
	// A build has nobody in front of it, so a browser login left to run there prints
	// a code nobody reads, waits out the whole window and then reports that nobody
	// approved it — where the old behaviour was an instant, accurate failure, and a
	// job that meant to pass --token deserves to hear so at once.
	//
	// --no-browser is the exception, and it is the one that makes sense: it says
	// "print the code and the page, somebody will open them", which is exactly the
	// premise this refusal denies. An agent in a container that exports CI=true is
	// the case this whole flow was built for, and it can say so.
	if inCI(env) && !f.noBrowser {
		return api.Fail("no_one_to_approve",
			"a browser login needs somebody to approve it, and this looks like CI — "+
				"pass `krowk auth login --token krowk_sk_...`, or add --no-browser if "+
				"there is somebody to hand the code to")
	}

	// Keyless, whatever the environment holds — see StartCLIAuthorization. Built
	// once and reused, so a poll that runs for a quarter hour is one connection
	// rather than a fresh one every five seconds.
	client := api.New(api.BaseURLFor(f.dev, env), "")

	ctx := context.Background()
	auth, err := client.StartCLIAuthorization(ctx)
	if err != nil {
		return noBrowserLoginHere(err)
	}
	page, err := browsableURL(auth.VerificationURL, client)
	if err != nil {
		return err
	}

	// Opened first and reported after, so the line claiming a browser is opening
	// is written once it actually is. Starting the handoff costs microseconds; the
	// window takes far longer to appear than this print does.
	opened := !f.noBrowser && !headless(env) && openBrowser(page)
	// stderr, not stdout: the code and the page are what a person needs *during*
	// the command, while stdout stays the single document a program parses. On a
	// terminal both arrive in the same place regardless.
	fmt.Fprint(stderr, output.Authorizing(
		output.Authorization{Code: auth.Code, Page: page, Opened: opened}, format, colour))

	// The window bounds the whole wait, so it bounds the context — which is then the
	// only place it is written down. Without it on the context it would bound only
	// the gaps *between* polls: one call can spend three attempts against a
	// five-minute client timeout with a Retry-After between them, which is longer
	// than the window it is supposed to sit inside.
	ctx, stop := context.WithDeadline(ctx, authorizationDeadline(auth.ExpiresAt, time.Now()))
	defer stop()

	granted, err := awaitAuthorization(ctx, client, auth, time.Now, time.Sleep)
	if err != nil {
		return err
	}

	path, err := api.SaveCredentials(granted.Token, api.Identity{
		KeyID: granted.KeyID, Workspace: granted.Workspace, WorkspaceName: granted.WorkspaceName,
	})
	if err != nil {
		// Worse news than the same failure on the --token path, and it has to say
		// so. There the key is still in the paste buffer; here it was minted for
		// this one delivery and the registry kept no copy, so a key that cannot be
		// written down is a key that is gone.
		return api.Fail("credentials_unwritable", "could not write "+api.CredentialsPath()+": "+
			err.Error()+" — the approved key was handed over once and the registry keeps no copy, "+
			"so fix the path and run `krowk auth login` again for a new one")
	}

	// Confirmed with no round trip of its own. The key came from the registry inside
	// this exchange rather than out of a paste buffer, so there is no typo for
	// `/key` to catch and nothing it would report that the approval did not already
	// say — as long as the approval said which key it minted and where it acts. When
	// it did not, the key is still good and still stored; what is withheld is the
	// claim about it, exactly as on the --token path, and `auth verify` settles it.
	result := &output.Login{
		Path: path, KeyID: granted.KeyID, Workspace: granted.Workspace,
		Confirmed: granted.KeyID != "" && granted.Workspace != "",
		Shadowed:  env("KROWK_TOKEN") != "",
	}
	if !result.Confirmed {
		result.Reason = "the registry approved it without naming the key or its workspace"
	}
	return emit(stdout, output.StoredKey(result, format, f.quiet, colour), f)
}

// noBrowserLoginHere reads a 404 on the way in as a registry that does not offer
// browser login, because most often that is what it is: the two halves of this
// flow live in two repositories and will not ship the same minute, and a
// self-hosted registry may never grow the second one. "Check KROWK_API_URL names
// the API host" is the wrong thing to tell someone whose base URL is exactly
// right — so the fix names both possibilities and the way in that always works.
func noBrowserLoginHere(err error) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		return err
	}
	apiErr.Body["fix"] = "this registry does not answer browser login — check KROWK_API_URL, " +
		"or issue a key in the dashboard and store it with `krowk auth login --token krowk_sk_...`"
	return err
}

// awaitAuthorization polls until someone answers or the window closes.
//
// What stops the loop and what does not is the whole of it. Approved and denied
// are answers. A refusal about *this authorization* — gone, no such slug, no such
// endpoint — is an answer too, and asking again cannot change it. Everything else
// is about the moment rather than the login: a rate limit, a 502, a laptop whose
// wifi dropped while its owner was reading the code off the screen. None of those
// is evidence, so the loop keeps its window instead of throwing away an approval
// that may already have happened.
//
// The window is the context's deadline, and the only value read for it comes from
// there. now and sleep are arguments so a test can make that value elapse without
// spending a quarter of an hour doing it.
//
// A context with no deadline ends the loop at once rather than running forever.
// Every caller bounds it; a loop that could not stop is the wrong thing to leave
// resting on that.
func awaitAuthorization(ctx context.Context, client *api.Client, auth *api.CLIAuthorization,
	now func() time.Time, sleep func(time.Duration)) (*api.CLIAuthorization, error) {
	deadline, _ := ctx.Deadline()
	interval := pollInterval(auth.Interval)
	// The last poll that did not come back, kept rather than dropped. If the window
	// closes with one outstanding, krowk could not ask — and "nobody approved it"
	// would blame a person for a question that never got out, with exit 8 saying no
	// retry helps when retrying is the entire fix. Cleared whenever the registry
	// answers, because then it did get out.
	var unanswered error

	wait := interval
	for {
		// Slept before the first read as well as between them. An authorization
		// milliseconds old, whose code the person has not finished reading, cannot
		// have been approved — so asking straight away spends a request on an
		// endpoint the registry meters to learn something already known.
		//
		// Never past the deadline, whatever a Retry-After asked for: sleeping through
		// the window would report it closed a minute after it closed.
		if left := deadline.Sub(now()); wait > left {
			wait = max(left, 0)
		}
		sleep(wait)
		wait = interval

		if ctx.Err() != nil || !now().Before(deadline) {
			return nil, windowClosed(unanswered)
		}

		granted, err := client.ReadCLIAuthorization(ctx, auth.Slug)
		switch {
		case err != nil:
			// krowk's own deadline aborting the request is the window closing, not a
			// registry that could not be reached: the failure describes the abort. Read
			// before the failure is weighed, since the abort wears whatever shape the
			// transport gave it — a cancellation, or a connection that went away.
			if ctx.Err() != nil {
				return nil, windowClosed(unanswered)
			}
			if !worthAnotherPoll(err) {
				return nil, loginFix(err)
			}
			unanswered = err
			// A rate limit names the wait it wants, and this endpoint is metered.
			// Coming back after the interval regardless would be arguing with it.
			if asked, ok := api.RetryAfterFor(err, now()); ok && asked > wait {
				wait = asked
			}
		case granted.State == api.AuthorizationApproved:
			// A token is the one thing this cannot do without: there would be nothing to
			// store, and storing nothing leaves a machine that looks logged in and
			// cannot upload.
			//
			// A missing key_id or workspace is not that. The plaintext has already left
			// the registry — this read is what consumed it — so refusing here would
			// throw away a working key over a receipt that would read blank. The caller
			// stores it and says the identity is unconfirmed, which is what the --token
			// path does when the registry could not vouch for a key either.
			if granted.Token == "" {
				return nil, api.Fail("malformed_response",
					"the registry approved this login without handing over a key — "+
						"run `krowk auth login` again, and report it if it repeats")
			}
			return granted, nil
		case granted.State == api.AuthorizationDenied:
			return nil, api.Fail("authorization_denied",
				"this login was denied in the browser — run `krowk auth login` to ask again")
		default:
			// Anything else is pending, including a state this build has no word for:
			// a newer registry inventing one is not grounds for abandoning a login
			// that is still inside its window. The question got out, so whatever
			// failed before is no longer what is holding this up.
			unanswered = nil
		}
	}
}

// windowClosed is what a closed window means, and it depends on whether the last
// question got an answer. One still outstanding means krowk could not ask, so it
// says that rather than claiming nobody approved — which it has no way to know.
func windowClosed(unanswered error) error {
	if unanswered != nil {
		return unanswered
	}
	return api.Fail("authorization_expired",
		"nobody approved this login before it lapsed — run `krowk auth login` to ask again")
}

// worthAnotherPoll reports whether a failed poll says nothing about the login. A
// rate limit, a 5xx and a registry that did not answer at all are all the moment
// rather than the authorization; everything else — a 404, a 410, a body this
// client cannot read — is an answer, and repeating the question gets it again.
func worthAnotherPoll(err error) bool {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Code() == "network_unreachable" {
		return true
	}
	return apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
}

// loginFix replaces advice written for artifacts when it arrives on a login. The
// registry spells a lapsed record `expired` and a one-shot secret already handed
// over `spent`, whatever the record is — and "upload it again, and claim it with
// a key to keep it" is nonsense to someone who was trying to log in.
func loginFix(err error) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	switch {
	case apiErr.Code() == "expired":
		apiErr.Body["fix"] = "this login lapsed before it was approved — run `krowk auth login` to ask again"
	case apiErr.Code() == "spent":
		apiErr.Body["fix"] = "this login's key was already collected, and the registry keeps no second copy — " +
			"run `krowk auth login` for a new one"
	case apiErr.Status == http.StatusNotFound:
		// The registry no longer holds this authorization — swept, restarted, or a
		// build that shipped the create before the read. `not_found`'s standing advice
		// is about a slug somebody typed and a key that scopes it, and this command
		// typed no slug and sent no key.
		apiErr.Body["fix"] = "the registry does not know this login — it may have lapsed and been " +
			"swept; run `krowk auth login` to ask again"
	}
	return err
}

// authorizationDeadline is when to give up. The registry's own expiry when it
// gave a readable one, clamped: a machine whose clock is minutes off the
// registry's would otherwise abandon a perfectly good login before its first
// poll, and the registry stays the authority on whether the window has really
// closed either way.
func authorizationDeadline(expiresAt string, now time.Time) time.Time {
	at, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return now.Add(defaultAuthorizationWindow)
	}
	return now.Add(min(max(at.Sub(now), minAuthorizationWindow), maxAuthorizationWindow))
}

// pollInterval is the registry's pace, bounded. It sets one because it is the
// side that knows what its rate limit allows; zero or missing means it did not
// say, and five seconds is what every other device flow settled on.
func pollInterval(seconds int) time.Duration {
	if seconds <= 0 {
		return defaultPollInterval
	}
	// Capped before the multiply, the same way a Retry-After is. A large enough
	// number of seconds overflows time.Duration and wraps negative, which sails
	// straight past the ceiling and comes out at the floor — failing in exactly the
	// hammering direction the bound exists to prevent.
	if seconds > int(maxPollInterval/time.Second) {
		return maxPollInterval
	}
	return max(time.Duration(seconds)*time.Second, minPollInterval)
}

// unconfirmedReason says why a login went unchecked in the few words that fit
// on the line under it. The status is included when there was one, because
// "the registry answered badly" and "nothing answered" call for different
// things next.
func unconfirmedReason(err error) string {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		if apiErr.Status != 0 {
			return fmt.Sprintf("%s (HTTP %d)", apiErr.Code(), apiErr.Status)
		}
		return apiErr.Code()
	}
	return err.Error()
}

// authToken prints the token a command run here would send — the resolved
// workspace's, not just whatever logged in last — because the caller pasting
// it into a curl expects it to act where krowk itself would act.
func authToken(w io.Writer, f flags, env runctx.Env) error {
	// The same short-circuit newClient takes, for the same reason: the
	// environment's token is what a command here would send, whatever the
	// config files are doing or failing to do.
	if token := env("KROWK_TOKEN"); token != "" {
		fmt.Fprintln(w, token)
		return nil
	}
	ws, _, err := resolveWorkspace(f, env)
	if err != nil {
		return err
	}
	token, err := api.ResolveToken(env, ws)
	if err != nil {
		return err
	}
	if token == "" {
		return api.Fail("not_authenticated",
			"run `krowk auth login --token krowk_sk_...`, or upload anonymously")
	}
	fmt.Fprintln(w, token)
	return nil
}

// authVerify reports what the stored key can actually do, rather than trusting
// that a token-shaped string is a working key.
func authVerify(w io.Writer, format output.Format, f flags, env runctx.Env, colour bool) error {
	client, err := newClient(f, env)
	if err != nil {
		return err
	}
	if client.Token == "" {
		return api.Fail("not_authenticated",
			"no key to verify — run `krowk auth login --token krowk_sk_...`, or upload anonymously")
	}

	key, err := client.VerifyKey(context.Background())
	if err != nil {
		return err
	}
	// The registry has just vouched for this key, which is more than the store
	// may know about it: a login that ran offline filed it under "default"
	// with no workspace at all, and a repo pinned to the real one refuses it
	// there. Verify is the command the login receipt says to run, so it is
	// where the record gets set straight. Only for a token the store actually
	// holds — one from the environment was never stored and is not the store's
	// business — and a failure to re-file is not a failure to verify: the
	// answer printed below is true either way.
	if env("KROWK_TOKEN") == "" {
		_, _ = api.AdoptIdentity(client.Token, api.Identity{
			KeyID: key.KeyID, Workspace: key.Workspace, WorkspaceName: key.WorkspaceName,
		})
	}
	return emit(w, output.Key(key, format, f.quiet, colour), f)
}

func doctor(w io.Writer, format output.Format, f flags, env runctx.Env) error {
	// Resolved by hand rather than through newClient, because doctor's job is
	// to describe a broken setup, not to be stopped by one: a malformed config
	// or a workspace with no key fails every other command, and this is the
	// command that says so.
	ws, source, wsErr := resolveWorkspace(f, env)
	client := api.New(api.BaseURLFor(f.dev, env), api.ReadToken(env, ws))

	report := map[string]any{
		"version":       Version,
		"runtime":       runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH,
		"api":           client.BaseURL,
		"registry":      registryMode(client, env),
		"api_status":    probe(client),
		"authenticated": client.Authenticated(),
		"token_source":  api.TokenSource(env, ws),
		"key":           keySummary(client),
		"workspace":     workspaceSummary(ws, source, wsErr, env),
		// Runs are where the metadata goes, and they need a key — so whether they
		// are available is the difference between metadata being kept and dropped.
		"runs_available": client.Authenticated(),
		"credentials":    api.CredentialsPath(),
		"config":         configSummary(),
		"context":        runctx.Detect(env),
	}

	keys := []string{"version", "runtime", "api", "registry", "api_status",
		"authenticated", "token_source", "key", "workspace", "runs_available",
		"credentials", "config"}

	if format != output.Human {
		b, _ := json.MarshalIndent(report, "", "  ")
		return emit(w, string(b), f)
	}

	for _, k := range keys {
		fmt.Fprintf(w, "%-15s %v\n", k, report[k])
	}
	b, _ := json.Marshal(report["context"])
	fmt.Fprintf(w, "%-15s %s\n", "context", b)
	return nil
}

// probe reads the service descriptor at the API root. It needs neither a key nor
// a payload, so it says whether the registry is there without uploading — and
// because it names the service, it also catches a URL pointing at the website or
// at the wrong virtual host, which a bare 200 would not.
//
// It separates "the registry answered" from "nothing is listening": an HTTP
// response of any status proves reachability, so the status that actually
// arrived is reported rather than assumed.
func probe(client *api.Client) string {
	service, err := client.Root(context.Background())
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) {
			if apiErr.Status != 0 {
				return fmt.Sprintf("reachable (HTTP %d) — %s", apiErr.Status, apiErr.Code())
			}
			if detail, ok := apiErr.Body["detail"].(string); ok && detail != "" {
				return "unreachable — " + apiErr.Code() + " — " + detail
			}
			return "unreachable — " + apiErr.Code()
		}
		return "unreachable — " + err.Error()
	}
	if service.Service == "" {
		return "reachable, but not a krowk registry"
	}
	return fmt.Sprintf("reachable (%s, %s)", service.Service, strings.Join(service.Versions, " "))
}

// keySummary names the key and where it lands, in one line. It only calls out
// to the registry when there is a key to verify, so a keyless doctor stays a
// single request.
func keySummary(client *api.Client) string {
	if client.Token == "" {
		return "none — uploads will be anonymous"
	}
	key, err := client.VerifyKey(context.Background())
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) {
			return "rejected — " + apiErr.Code()
		}
		return "unknown — " + err.Error()
	}
	if key.Name != "" {
		return fmt.Sprintf("%s (%s) %s", key.KeyID, key.Workspace, key.Name)
	}
	return fmt.Sprintf("%s (%s)", key.KeyID, key.Workspace)
}

// workspaceSummary says where uploads land and who decided it, without calling
// out at all. It is the answer keySummary spends a request on, available on a
// train.
//
// Silence about the environment token is the point rather than a gap: a token
// from KROWK_TOKEN was never logged in, so nothing was recorded about it and
// naming a stored workspace would name somewhere uploads are not going. A
// login the registry could not confirm recorded nothing either. Both say so
// and point at the one thing that can settle it.
func workspaceSummary(ws, source string, wsErr error, env runctx.Env) string {
	if wsErr != nil {
		return "unresolvable — " + wsErr.Error()
	}
	if ws != "" {
		if api.ReadToken(env, ws) == "" {
			return ws + " (" + source + ") — but no key is stored for it, so every command here fails"
		}
		if env("KROWK_TOKEN") != "" {
			return ws + " (" + source + ") — but KROWK_TOKEN is set and wins; " +
				"uploads land wherever that key acts, and `krowk auth verify` names it"
		}
		return ws + " (" + source + ")"
	}
	if id, ok := api.ReadIdentity(env, ""); ok {
		return id.Workspace + " (stored default)"
	}
	if api.TokenSource(env, "") == api.TokenSourceNone {
		return "none — uploads will be anonymous"
	}
	return "unknown — not recorded at login; `krowk auth verify` asks the registry"
}

// configSummary names the config files a command here would read, marking the
// ones that exist — the difference between "nothing configured" and "the file
// is there and not doing what was hoped" is the first thing to check.
func configSummary() string {
	parts := []string{"global " + existing(config.GlobalPath())}
	if repo, inRepo := config.RepoPath(""); inRepo {
		parts = append(parts, "repo "+existing(repo))
	}
	return strings.Join(parts, ", ")
}

// existing marks a path that is actually on disk, so the doctor line reads at
// a glance which layers are in play.
func existing(path string) string {
	if _, err := os.Stat(path); err != nil {
		return path + " (absent)"
	}
	return path
}

func clip[T any](s []T, n int) []T {
	if len(s) < n {
		return s
	}
	return s[:n]
}
