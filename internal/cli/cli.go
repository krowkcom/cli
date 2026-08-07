// Package cli is the krowk command line. Run is the whole entry point, taking
// its streams and environment as arguments so tests never touch the process.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/registry"
	"github.com/krowkcom/cli/internal/runctx"
)

// Version is stamped at build time: -ldflags "-X .../internal/cli.Version=1.2.3".
var Version = "0.1.0"

// defaultRegistryAddr is where `registry serve` listens, matching api.DevBaseURL
// so --dev finds it with no configuration.
//
// Loopback, not ":8787". This registry takes uploads without a key and serves
// their bytes to anyone who can reach it. On a café or office network, binding
// every interface hands that to whoever is nearby. A wider bind stays possible,
// but it has to be asked for.
const defaultRegistryAddr = "127.0.0.1:8787"

// claimTokenPrefix is what the registry mints every claim token with, so a
// second positional either looks like one or is a mistake worth naming.
const claimTokenPrefix = "krowk_claim_"

const helpTemplate = `krowk %s — permalinks for agent output

Usage
  krowk push <file...> [flags]              Upload files, get a link for each
  krowk uploads create <file...> [flags]    The same thing, spelled out
  krowk uploads list [--run --limit --before]
                                            List uploads, newest first — a run's, or the workspace's
  krowk uploads show <artifact>             Read one artifact back
  krowk uploads attach <art> --run <run>    Put an upload under a run afterwards
  krowk uploads delete <art> [token]        Take an upload down — immediate, cannot be undone
  krowk runs start [flags]                  Open a run to group uploads under
  krowk runs list [--limit --before]        List the workspace's runs, newest first
  krowk runs show <run>                     Read one run back, with its metadata
  krowk runs finish <run>                   Close a run
  krowk claim <artifact> <token> [--run]    Keep an anonymous upload past expiry
  krowk auth login --token <token>          Check an API token, then store it
  krowk auth token                          Print the stored token
  krowk auth verify                         Check the key and its workspace
  krowk doctor                              Check the local setup
  krowk registry serve                      Run a local registry to develop against

Upload flags
  --run <slug>           Attach to an existing run instead of opening one. On
                         ` + "`claim`" + ` and ` + "`uploads attach`" + ` it names the run an upload
                         already made joins — a claimed upload has none otherwise
  --pull-request <url>   Pull request the work belongs to
  --reference <url>      Related link — repeat for more than one
  --session <id>         Agent session ID
  --title <text>         Label for the markdown link
  --repo <owner/name>    Override the detected repository
  --commit <sha>         Override the detected commit
  --agent <name>         Override the detected agent

List flags
  --limit <n>            Rows per page (1–100, default 50)
  --before <slug>        Start after this row — the ` + "`next`" + ` of the last page
  --run <slug>           On ` + "`uploads list`" + `, narrow it to what one run produced

Local registry flags
  --addr <host:port>     Listen address for ` + "`registry serve`" + ` (default %s, loopback only)
  --site <url>           Origin for the links it returns (default: the request host)
  --limit-bytes <n>      Reject uploads above this size

Global flags
  --dev                  Talk to a local registry at %s
  --format <fmt>         human | json | markdown | url (default: human on a TTY, json when piped)
                         markdown and url describe an upload; other commands fall back to json
  --json                 Shorthand for --format json
  --quiet                Raw JSON, no envelope
  -h, --help             Show this
  -v, --version          Print the version

Environment
  KROWK_TOKEN            API token — wins over the credentials file
  KROWK_API_URL          API base URL (default %s)
  KROWK_DEV              1/true/yes/on — same as --dev
  KROWK_AGENT            Agent name to report

Registry precedence: --dev, then KROWK_API_URL, then KROWK_DEV, then the default.

Run metadata — the pull request, references and session — is recorded on a run,
and a run belongs to a workspace, so it needs an API key. Without one an upload
still works: it lands anonymously, expires within a day, and comes back with a
claim token that ` + "`krowk claim`" + ` spends to keep it.

Taking an upload down removes the bytes at once and leaves the link reporting
that it was taken down. There is no undo and no confirmation — it is what to
reach for when something was published by accident. A key takes down anything in
its workspace; an upload that is still anonymous is taken down with the claim
token it came back with, passed after the slug.

Credentials live in %s (0600).
`

type flags struct {
	run         string
	before      string
	limit       int
	pullRequest string
	references  stringSlice
	session     string
	title       string
	repo        string
	commit      string
	agent       string
	token       string
	format      string
	addr        string
	site        string
	limitBytes  int64
	dev         bool
	json        bool
	quiet       bool
	help        bool
	version     bool
}

type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// Run executes one invocation and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer, env func(string) string, isTTY bool) int {
	var f flags
	fs := flag.NewFlagSet("krowk", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors go through the krowk envelope, not flag's

	fs.StringVar(&f.run, "run", "", "")
	fs.StringVar(&f.before, "before", "", "")
	fs.IntVar(&f.limit, "limit", 0, "")
	fs.StringVar(&f.pullRequest, "pull-request", "", "")
	fs.Var(&f.references, "reference", "")
	fs.StringVar(&f.session, "session", "", "")
	fs.StringVar(&f.title, "title", "", "")
	fs.StringVar(&f.repo, "repo", "", "")
	fs.StringVar(&f.commit, "commit", "", "")
	fs.StringVar(&f.agent, "agent", "", "")
	fs.StringVar(&f.token, "token", "", "")
	fs.StringVar(&f.format, "format", "", "")
	fs.StringVar(&f.addr, "addr", defaultRegistryAddr, "")
	fs.StringVar(&f.site, "site", "", "")
	fs.Int64Var(&f.limitBytes, "limit-bytes", 0, "")
	fs.BoolVar(&f.dev, "dev", false, "")
	fs.BoolVar(&f.json, "json", false, "")
	fs.BoolVar(&f.quiet, "quiet", false, "")
	fs.BoolVar(&f.help, "help", false, "")
	fs.BoolVar(&f.help, "h", false, "")
	fs.BoolVar(&f.version, "version", false, "")
	fs.BoolVar(&f.version, "v", false, "")

	// Go's flag package stops at the first positional, so parse in a loop and
	// collect them. Lets flags follow filenames, the way agents write commands.
	positionals, parseErr := parseInterleaved(fs, args)

	// Resolve the format before reporting anything, including the parse error.
	format, formatErr := output.ResolveFormat(f.format, f.json, isTTY)
	if formatErr != nil {
		return report(stderr, formatErr, output.JSON, f.quiet, false)
	}
	colour := isTTY

	if parseErr != nil {
		err := api.Fail("bad_flag", parseErr.Error()+" — run `krowk --help`")
		return report(stderr, err, format, f.quiet, colour)
	}

	switch {
	case f.version:
		fmt.Fprintln(stdout, Version)
		return 0
	case f.help, len(positionals) == 0, positionals[0] == "help":
		fmt.Fprintf(stdout, helpTemplate, Version,
			defaultRegistryAddr, api.DevBaseURL, api.DefaultBaseURL, api.CredentialsPath())
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
		err = authLogin(stdout, f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "auth" && positionals[1] == "token":
		err = authToken(stdout, env)
	case len(positionals) > 1 && positionals[0] == "auth" && positionals[1] == "verify":
		err = authVerify(stdout, format, f, env, colour)
	case len(positionals) > 1 && positionals[0] == "registry" && positionals[1] == "serve":
		err = registryServe(stdout, f)
	case positionals[0] == "doctor":
		err = doctor(stdout, format, f, env)
	default:
		err = api.Fail("unknown_command",
			"`"+strings.Join(clip(positionals, 2), " ")+"` is not a krowk command — run `krowk --help`")
	}

	if err != nil {
		return report(stderr, err, format, f.quiet, colour)
	}
	return 0
}

func report(w io.Writer, err error, format output.Format, quiet, colour bool) int {
	fmt.Fprintln(w, output.Error(err, format, quiet, colour))
	return 1
}

// newClient is the one place a registry client gets built, so every command
// honours the same precedence: --dev, then KROWK_API_URL, then KROWK_DEV.
func newClient(f flags, env runctx.Env) *api.Client {
	return api.New(api.BaseURLFor(f.dev, env), api.ReadToken(env))
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

	client := newClient(f, env)
	ctx := context.Background()

	result := output.Result{Title: f.title}

	runSlug, ownRun, err := resolveRun(ctx, client, f, env, &result)
	if err != nil {
		return err
	}

	for _, spec := range specs {
		spec.Run = runSlug
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

	fmt.Fprintln(w, output.Upload(result, format, f.quiet, colour, time.Now()))
	return nil
}

// resolveRun decides which run the artifacts belong to: the one named on the
// command line, a fresh one carrying the detected metadata, or none at all when
// there is no key to open one with.
func resolveRun(ctx context.Context, client *api.Client, f flags, env runctx.Env, result *output.Result) (slug string, own bool, err error) {
	if f.run != "" {
		// The caller manages this run's lifecycle, so it is not finished here.
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
		Reference:   f.references,
		Session:     f.session,
		Title:       f.title,
		Client:      "krowk-cli/" + Version,
	})
}

// anonymousMetadataNote says plainly that metadata asked for by name was not
// recorded. Silently dropping it would leave an agent believing the pull request
// it named is attached to the upload, and it is not.
func anonymousMetadataNote(f flags) string {
	var given []string
	if f.pullRequest != "" {
		given = append(given, "--pull-request")
	}
	if len(f.references) > 0 {
		given = append(given, "--reference")
	}
	if f.session != "" {
		given = append(given, "--session")
	}
	if len(given) == 0 {
		return ""
	}
	return strings.Join(given, ", ") + " was not recorded: run metadata lives on a run, " +
		"and opening a run needs an API key — run `krowk auth login --token krowk_sk_...`"
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

// uploadsList pages through the key's workspace. Needs a key: keyless requests
// all share the anonymous workspace, so there is nothing of one's own to list.
//
// --run narrows it to what one run produced, which the registry serves as a
// collection of that run rather than as a filter here. The difference is worth
// keeping: an unknown run is a 404 from the run itself, where a filter would
// answer an empty page — and a caller cannot tell that apart from a run that
// genuinely produced nothing.
func uploadsList(w io.Writer, f flags, format output.Format, env runctx.Env, colour bool) error {
	client := newClient(f, env)
	ctx := context.Background()

	var page *api.Page
	var err error
	if f.run != "" {
		page, err = client.ListRunArtifacts(ctx, f.run, f.before, f.limit)
	} else {
		page, err = client.ListArtifacts(ctx, f.before, f.limit)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.List(page, f.run, format, f.quiet, colour, time.Now()))
	return nil
}

// runsList pages through the key's runs, newest first.
func runsList(w io.Writer, f flags, format output.Format, env runctx.Env, colour bool) error {
	client := newClient(f, env)

	page, err := client.ListRuns(context.Background(), f.before, f.limit)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.RunList(page, format, f.quiet, colour))
	return nil
}

// runsShow reads one run back with everything recorded on it. That is where an
// upload's origin lives — the pull request, the commit, the session — since the
// registry keeps none of it on the artifact.
func runsShow(w io.Writer, args []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	if len(args) == 0 {
		return api.Fail("no_run", "pass the run: `krowk runs show run_...`")
	}
	client := newClient(f, env)

	run, err := client.ShowRun(context.Background(), args[0])
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.RunDetail(run, format, f.quiet, colour))
	return nil
}

func uploadsShow(w io.Writer, args []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	if len(args) == 0 {
		return api.Fail("no_artifact", "pass the artifact: `krowk uploads show art_...`")
	}
	client := newClient(f, env)

	artifact, err := client.ShowArtifact(context.Background(), args[0])
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.Artifact(artifact, format, f.quiet, colour, time.Now()))
	return nil
}

// uploadsAttach puts an upload under a run after the fact. It is the only way an
// upload that started out anonymous ever gets one: it could not name a run when
// it was created, and claiming it does not give it one.
func uploadsAttach(w io.Writer, args []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	if len(args) == 0 {
		return api.Fail("no_artifact", "pass the artifact: `krowk uploads attach art_... --run run_...`")
	}
	if f.run == "" {
		return api.Fail("no_run", "pass the run to attach it to: `krowk uploads attach "+args[0]+" --run run_...`")
	}
	client := newClient(f, env)

	artifact, err := client.AttachRun(context.Background(), args[0], f.run)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.Artifact(artifact, format, f.quiet, colour, time.Now()))
	return nil
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
	if len(args) == 0 {
		return api.Fail("no_artifact", "pass the artifact: `krowk uploads delete art_...`")
	}
	slug := args[0]
	var claimToken string
	if len(args) > 1 {
		// Checked rather than taken as given, because a second word that is not a
		// token is not a wrong token — and passing one withholds the key, so a
		// stray argument would quietly turn an authorised takedown into an
		// unauthorised one and report it as a 404. `uploads delete art_a art_b`,
		// meaning two artifacts, is the way that happens.
		if !strings.HasPrefix(args[1], claimTokenPrefix) {
			return api.Fail("bad_claim_token",
				"`"+args[1]+"` is not a claim token — takedown takes one artifact and, after it, "+
					"only the `"+claimTokenPrefix+"...` token that upload came back with")
		}
		claimToken = args[1]
	}

	client := newClient(f, env)
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
	fmt.Fprintln(w, output.Removed(slug, format, f.quiet, colour))
	return nil
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
	client := newClient(f, env)

	run, err := client.CreateRun(context.Background(), metadataFor(f, env))
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.Run(run, format, f.quiet, colour))
	return nil
}

func runsFinish(w io.Writer, args []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	if len(args) == 0 {
		return api.Fail("no_run", "pass the run: `krowk runs finish run_...`")
	}
	client := newClient(f, env)

	run, err := client.FinishRun(context.Background(), args[0])
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.Run(run, format, f.quiet, colour))
	return nil
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
	client := newClient(f, env)
	ctx := context.Background()

	artifact, err := client.ClaimArtifact(ctx, args[0], args[1])
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
	fmt.Fprintln(w, output.Artifact(artifact, format, f.quiet, colour, time.Now()))
	return nil
}

// authLogin stores a key, but only once the registry has been given the chance
// to reject it. Storing first and finding out later meant a mistyped key was
// discovered by the next upload failing, at which point the paste buffer that
// held the real one is long gone.
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
func authLogin(w io.Writer, f flags, format output.Format, env runctx.Env, colour bool) error {
	if f.token == "" {
		return api.Fail("missing_token", "pass the key: `krowk auth login --token krowk_sk_...`")
	}

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
		id = api.Identity{KeyID: key.KeyID, Workspace: key.Workspace}
	}

	path, err := api.SaveCredentials(f.token, id)
	if err != nil {
		return api.Fail("credentials_unwritable", "could not write "+api.CredentialsPath()+": "+err.Error())
	}

	result := &output.Login{Path: path, Confirmed: verifyErr == nil}
	if verifyErr != nil {
		result.Reason = unconfirmedReason(verifyErr)
	} else {
		result.KeyID, result.Workspace = key.KeyID, key.Workspace
	}
	fmt.Fprintln(w, output.StoredKey(result, format, f.quiet, colour))
	return nil
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

func authToken(w io.Writer, env runctx.Env) error {
	token := api.ReadToken(env)
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
	client := newClient(f, env)
	if client.Token == "" {
		return api.Fail("not_authenticated",
			"no key to verify — run `krowk auth login --token krowk_sk_...`, or upload anonymously")
	}

	key, err := client.VerifyKey(context.Background())
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.Key(key, format, f.quiet, colour))
	return nil
}

func doctor(w io.Writer, format output.Format, f flags, env runctx.Env) error {
	client := newClient(f, env)

	report := map[string]any{
		"version":       Version,
		"runtime":       runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH,
		"api":           client.BaseURL,
		"registry":      registryMode(client, env),
		"api_status":    probe(client),
		"authenticated": client.Authenticated(),
		"token_source":  api.TokenSource(env),
		"key":           keySummary(client),
		"workspace":     recordedWorkspace(env),
		// Runs are where the metadata goes, and they need a key — so whether they
		// are available is the difference between metadata being kept and dropped.
		"runs_available": client.Authenticated(),
		"credentials":    api.CredentialsPath(),
		"context":        runctx.Detect(env),
	}

	keys := []string{"version", "runtime", "api", "registry", "api_status",
		"authenticated", "token_source", "key", "workspace", "runs_available",
		"credentials"}

	if format != output.Human {
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Fprintln(w, string(b))
		return nil
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

// recordedWorkspace says where uploads land according to what login wrote down,
// without calling out at all. It is the answer keySummary spends a request on,
// available on a train.
//
// Silence here is the point rather than a gap. A token from the environment was
// never logged in, so nothing was recorded about it and the file's workspace
// belongs to a different key; saying it would name somewhere uploads are not
// going. A login the registry could not confirm recorded nothing either. Both
// say so and point at the one thing that can settle it.
func recordedWorkspace(env runctx.Env) string {
	if id, ok := api.ReadIdentity(env); ok {
		return id.Workspace
	}
	if api.TokenSource(env) == api.TokenSourceNone {
		return "none — uploads will be anonymous"
	}
	return "unknown — not recorded at login; `krowk auth verify` asks the registry"
}

// registryServe runs the local stand-in for api.krowk.com, so developing against
// a registry needs neither the network nor a checkout of the registry itself.
func registryServe(w io.Writer, f flags) error {
	// registry.Handler treats <= 0 as "use the default", so a negative limit
	// would silently mean 100 MiB. Reject it instead of guessing.
	if f.limitBytes < 0 {
		return api.Fail("bad_flag", "--limit-bytes must not be negative — omit it or pass 0 for the default")
	}

	addr := f.addr
	// The flag carries the default, but a direct caller may pass the zero value.
	if addr == "" {
		addr = defaultRegistryAddr
	}
	if err := usableAddr(addr); err != nil {
		return err
	}

	// Bind before announcing anything, so a script keying off the banner never
	// proceeds against a port that failed to open.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return api.Fail("registry_unavailable", "could not listen on "+addr+": "+err.Error())
	}
	return serveOn(w, ln, addr, f)
}

// serveOn runs the registry on an already-bound listener. Split from
// registryServe so a test can hold the listener and close it to stop serving.
func serveOn(w io.Writer, ln net.Listener, addr string, f flags) error {
	fmt.Fprint(w, registryBanner(ln.Addr().String(), addr))

	server := &http.Server{
		Handler: registry.Handler(f.limitBytes, f.site),
		// Uploads can be slow and large, so only the header read is bounded.
		ReadHeaderTimeout: 20 * time.Second,
	}
	if err := server.Serve(ln); err != nil {
		return api.Fail("registry_unavailable", "registry on "+addr+" stopped: "+err.Error())
	}
	return nil
}

// registryBanner is what `registry serve` prints once the listener is bound:
// where the registry is, how to point a push at it, and — bound wider than this
// machine — that it is open. Kept separate so it can be checked without a bind.
func registryBanner(bound, asked string) string {
	base := localBase(bound, asked)

	lines := []string{"krowk registry listening on " + base}
	if reachableByDev(asked) {
		lines = append(lines, "  krowk push screenshot.png --dev")
	} else {
		// --dev only knows the default address, so say what to use instead.
		lines = append(lines, "  KROWK_API_URL="+base+"/v1 krowk push screenshot.png")
	}
	// Bound wider than this machine, deliberately or not, it is worth saying out
	// loud: this registry takes uploads without a key and serves their bytes to
	// anyone who can reach it.
	if host := listenHost(asked); !isLoopbackHost(host) {
		lines = append(lines,
			"  ! reachable from the network on "+bindDescription(host)+" — it needs no key to accept uploads")
	}
	return strings.Join(lines, "\n") + "\n"
}

func bindDescription(host string) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return "every interface"
	}
	return host
}

// localBase turns a listen address into a URL a client can call. bound is what
// the listener reports, which resolves ":0" to the real port; asked keeps the
// hostname the user typed, since the listener flattens it to an IP.
func localBase(bound, asked string) string {
	host, port, err := net.SplitHostPort(asked)
	if err != nil {
		return "http://" + asked
	}
	// An asked port of 0 means "any port", and only the bound address knows
	// which one it became.
	if port == "0" {
		if _, boundPort, splitErr := net.SplitHostPort(bound); splitErr == nil {
			port = boundPort
		}
	}
	// Wildcard binds listen everywhere but dial nowhere; localhost is the
	// loopback name that reaches them on either stack. Loopback IPs fold to
	// the same name, so the default bind stays the address --dev dials.
	switch host {
	case "", "0.0.0.0", "::", "127.0.0.1", "::1":
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// reachableByDev reports whether --dev, which knows only one address, will find
// a registry listening here.
func reachableByDev(addr string) bool {
	dev, err := url.Parse(api.DevBaseURL)
	if err != nil {
		return false
	}
	if listenPort(addr) != dev.Port() {
		return false
	}
	// --dev dials localhost, which lands on 127.0.0.1 or ::1 — so only those
	// hosts, their name, and every-interface binds qualify. The rest of
	// 127.0.0.0/8 is loopback too, but localhost does not reach it, so the
	// advice for 127.0.0.2 is KROWK_API_URL, not a --dev that cannot connect.
	switch listenHost(addr) {
	case "", "0.0.0.0", "::", "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

func listenHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return host
}

func listenPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}

// usableAddr rejects what net.Listen would reject anyway, so the banner does not
// print "http://localhost:" and a network warning for an address that never
// binds. --addr 8787 is the easy mistake.
// An empty port is accepted by SplitHostPort but binds a kernel-picked port the
// banner has no name for — ":0" is the explicit spelling of that request, and it
// is welcome: the bind happens before the banner, which then reports the port
// the kernel picked. An empty host is fine: that is what ":8787" means.
func usableAddr(addr string) error {
	if _, port, err := net.SplitHostPort(addr); err != nil || !bindablePort(port) {
		return fmt.Errorf("--addr %q needs a host and a numeric port, like 127.0.0.1:8787 or :8787", addr)
	}
	return nil
}

// bindablePort reports whether port is one the listener can take as given: all
// digits (net.Listen also resolves signs and service names like "http", which
// the banner would print verbatim), within 0-65535 — 99999 announces itself and
// then fails to bind — and spelled the way it binds, since ":08787" binds 8787.
// Port 0 asks the kernel to pick, and the banner reports what it picked.
func bindablePort(port string) bool {
	for _, r := range port {
		if r < '0' || r > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n <= 65535 && strconv.Itoa(n) == port
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func clip[T any](s []T, n int) []T {
	if len(s) < n {
		return s
	}
	return s[:n]
}
