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
	"runtime"
	"strings"
	"time"

	"github.com/krowkcom/cli/internal/api"
	"github.com/krowkcom/cli/internal/output"
	"github.com/krowkcom/cli/internal/runctx"
)

// Version is stamped at build time: -ldflags "-X .../internal/cli.Version=1.2.3".
var Version = "0.1.0"

const helpTemplate = `krowk %s — permalinks for agent output

Usage
  krowk push <file...> [flags]              Upload files, get a link for each
  krowk uploads create <file...> [flags]    The same thing, spelled out
  krowk uploads list [--limit --before]     List the workspace's uploads, newest first
  krowk uploads show <artifact>             Read one artifact back
  krowk runs start [flags]                  Open a run to group uploads under
  krowk runs finish <run>                   Close a run
  krowk claim <artifact> <claim-token>      Keep an anonymous upload past expiry
  krowk auth login --token <token>          Store an API token
  krowk auth token                          Print the stored token
  krowk doctor                              Check the local setup

Upload flags
  --run <slug>           Attach to an existing run instead of opening one
  --pull-request <url>   Pull request the work belongs to
  --reference <url>      Related link — repeat for more than one
  --session <id>         Agent session ID
  --title <text>         Label for the markdown link
  --repo <owner/name>    Override the detected repository
  --commit <sha>         Override the detected commit
  --agent <name>         Override the detected agent

List flags
  --limit <n>            Artifacts per page (1–100, default 50)
  --before <artifact>    Start after this artifact — the ` + "`next`" + ` of the last page

Global flags
  --format <fmt>         human | json | markdown (default: human on a TTY, json when piped)
  --json                 Shorthand for --format json
  --quiet                Raw JSON, no envelope
  -h, --help             Show this
  -v, --version          Print the version

Environment
  KROWK_TOKEN            API token — wins over the credentials file
  KROWK_API_URL          API base URL (default %s)
  KROWK_AGENT            Agent name to report

Run metadata — the pull request, references and session — is recorded on a run,
and a run belongs to a workspace, so it needs an API key. Without one an upload
still works: it lands anonymously, expires within a day, and comes back with a
claim token that ` + "`krowk claim`" + ` spends to keep it.

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
		fmt.Fprintf(stdout, helpTemplate, Version, api.DefaultBaseURL, api.CredentialsPath())
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
	case len(positionals) > 1 && positionals[0] == "runs" && positionals[1] == "start":
		err = runsStart(stdout, f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "runs" && positionals[1] == "finish":
		err = runsFinish(stdout, positionals[2:], f, format, env, colour)
	case positionals[0] == "claim":
		err = claim(stdout, positionals[1:], f, format, env, colour)
	case len(positionals) > 1 && positionals[0] == "auth" && positionals[1] == "login":
		err = authLogin(stdout, f.token)
	case len(positionals) > 1 && positionals[0] == "auth" && positionals[1] == "token":
		err = authToken(stdout, env)
	case positionals[0] == "doctor":
		err = doctor(stdout, format, env)
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

	client := api.New(env("KROWK_API_URL"), api.ReadToken(env))
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
			return withProgress(err, result.Artifacts)
		}
		result.Artifacts = append(result.Artifacts, artifact)
	}

	// A run this command opened is a run this command closes. Failing to close it
	// is not worth failing the upload over — the artifacts are up and their links
	// work — so the run is reported as it was last known.
	if ownRun {
		if finished, err := client.FinishRun(ctx, runSlug); err == nil {
			result.Run = finished
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
// Flags win; the rest is detected so the agent never has to type it.
func metadataFor(f flags, env runctx.Env) runctx.Metadata {
	metadata := runctx.Detect(env)
	overrideString(&metadata.Repo, f.repo)
	overrideString(&metadata.Commit, f.commit)
	overrideString(&metadata.Agent, f.agent)
	overrideString(&metadata.PullRequest, f.pullRequest)
	metadata.Reference = f.references
	metadata.Session = f.session
	metadata.Title = f.title
	metadata.Client = "krowk-cli/" + Version
	return metadata
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

// withProgress keeps the links of whatever did upload before the failure, so a
// partial upload does not lose the artifacts it already created.
func withProgress(err error, done []*api.Artifact) error {
	if len(done) == 0 {
		return err
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	urls := make([]string, 0, len(done))
	for _, a := range done {
		urls = append(urls, a.URL)
	}
	apiErr.Body["uploaded_before_failure"] = urls
	return apiErr
}

// uploadsList pages through the key's workspace. Needs a key: keyless requests
// all share the anonymous workspace, so there is nothing of one's own to list.
func uploadsList(w io.Writer, f flags, format output.Format, env runctx.Env, colour bool) error {
	client := api.New(env("KROWK_API_URL"), api.ReadToken(env))

	page, err := client.ListArtifacts(context.Background(), f.before, f.limit)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.List(page, format, f.quiet, colour, time.Now()))
	return nil
}

func uploadsShow(w io.Writer, args []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	if len(args) == 0 {
		return api.Fail("no_artifact", "pass the artifact: `krowk uploads show art_...`")
	}
	client := api.New(env("KROWK_API_URL"), api.ReadToken(env))

	artifact, err := client.ShowArtifact(context.Background(), args[0])
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.Artifact(artifact, format, f.quiet, colour, time.Now()))
	return nil
}

func runsStart(w io.Writer, f flags, format output.Format, env runctx.Env, colour bool) error {
	client := api.New(env("KROWK_API_URL"), api.ReadToken(env))

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
	client := api.New(env("KROWK_API_URL"), api.ReadToken(env))

	run, err := client.FinishRun(context.Background(), args[0])
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.Run(run, format, f.quiet, colour))
	return nil
}

// claim spends the token an anonymous upload came back with, which is the only
// way to keep that upload past its expiry.
func claim(w io.Writer, args []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	if len(args) < 2 {
		return api.Fail("missing_claim",
			"pass both the artifact and its token: `krowk claim art_... krowk_claim_...`")
	}
	client := api.New(env("KROWK_API_URL"), api.ReadToken(env))

	artifact, err := client.ClaimArtifact(context.Background(), args[0], args[1])
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.Artifact(artifact, format, f.quiet, colour, time.Now()))
	return nil
}

func authLogin(w io.Writer, token string) error {
	if token == "" {
		return api.Fail("missing_token", "pass the key: `krowk auth login --token krowk_sk_...`")
	}
	path, err := api.SaveToken(token)
	if err != nil {
		return api.Fail("credentials_unwritable", "could not write "+api.CredentialsPath()+": "+err.Error())
	}
	fmt.Fprintln(w, "✓ token stored in "+path)
	return nil
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

func doctor(w io.Writer, format output.Format, env runctx.Env) error {
	client := api.New(env("KROWK_API_URL"), api.ReadToken(env))

	report := map[string]any{
		"version":       Version,
		"runtime":       runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH,
		"api":           client.BaseURL,
		"api_status":    probe(client),
		"authenticated": client.Authenticated(),
		// Runs are where the metadata goes, and they need a key — so whether they
		// are available is the difference between metadata being kept and dropped.
		"runs_available": client.Authenticated(),
		"credentials":    api.CredentialsPath(),
		"context":        runctx.Detect(env),
	}

	keys := []string{"version", "runtime", "api", "api_status", "authenticated",
		"runs_available", "credentials"}

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
func probe(client *api.Client) string {
	service, err := client.Root(context.Background())
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) {
			if detail, ok := apiErr.Body["detail"].(string); ok && detail != "" {
				return apiErr.Code() + " — " + detail
			}
			return apiErr.Code()
		}
		return err.Error()
	}
	if service.Service == "" {
		return "reachable, but not a krowk registry"
	}
	return fmt.Sprintf("reachable (%s, %s)", service.Service, strings.Join(service.Versions, " "))
}

func overrideString(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func clip[T any](s []T, n int) []T {
	if len(s) < n {
		return s
	}
	return s[:n]
}
