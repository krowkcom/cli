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
	"maps"
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
  krowk uploads create <file...> [flags]   Upload artifacts, get one canonical URL
  krowk push <file...> [flags]             Alias for ` + "`uploads create`" + `
  krowk auth login --token <token>         Store an API token
  krowk auth token                         Print the stored token
  krowk auth verify                        Check the key and its scopes
  krowk doctor                             Check the local setup

Pasting the result
  GitHub, Linear, Notion   --format markdown   embeds the image
  Slack, Basecamp          --format url        they unfurl the link themselves
  Human output shows both, labelled; --json carries both under "paste".

Upload flags
  --pull-request <url>   Pull request the work belongs to
  --reference <url>      Related link — repeat for more than one
  --session <id>         Agent session ID
  --title <text>         Label for the unfurl card
  --repo <owner/name>    Override the detected repository
  --commit <sha>         Override the detected commit
  --agent <name>         Override the detected agent

Global flags
  --format <fmt>         human | json | markdown | url (default: human on a TTY, json when piped)
                         markdown and url describe an upload; other commands fall back to json
  --json                 Shorthand for --format json
  --quiet                Raw JSON, no envelope
  -h, --help             Show this
  -v, --version          Print the version

Environment
  KROWK_TOKEN            API token — wins over the credentials file
  KROWK_API_URL          API base URL (default %s)
  KROWK_AGENT            Agent name to report

Credentials live in %s (0600).
`

type flags struct {
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
	case len(positionals) > 1 && positionals[0] == "auth" && positionals[1] == "login":
		err = authLogin(stdout, f.token)
	case len(positionals) > 1 && positionals[0] == "auth" && positionals[1] == "token":
		err = authToken(stdout, env)
	case len(positionals) > 1 && positionals[0] == "auth" && positionals[1] == "verify":
		err = authVerify(stdout, format, env, f.quiet, colour)
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

func upload(w io.Writer, files []string, f flags, format output.Format, env runctx.Env, colour bool) error {
	if len(files) == 0 {
		return api.Fail("no_file", "pass at least one path: `krowk uploads create screenshot.png`")
	}

	metadata := runctx.Detect(env)
	overrideString(&metadata.Repo, f.repo)
	overrideString(&metadata.Commit, f.commit)
	overrideString(&metadata.Agent, f.agent)
	overrideString(&metadata.PullRequest, f.pullRequest)
	metadata.Reference = f.references
	metadata.Session = f.session
	metadata.Title = f.title
	metadata.Client = "krowk-cli/" + Version

	client := api.New(env("KROWK_API_URL"), api.ReadToken(env))
	artifact, err := client.CreateArtifact(context.Background(), files, metadata)
	if err != nil {
		return authHint(err, client.Token != "")
	}

	fmt.Fprintln(w, output.Artifact(artifact, format, f.title, f.quiet, colour, time.Now()))
	return nil
}

// authHint points a rejected upload at the self-check. The registry cannot know
// the CLI has a verify command, so the CLI adds that half of the fix itself.
// It keys on the error code, not the HTTP status: a 403 can also come off a
// presigned storage URL mid-upload, where the key is fine and pointing the
// agent at auth would be a dead end.
func authHint(err error, authenticated bool) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code() {
	case "no_key", "invalid_key", "insufficient_scope":
	default:
		return err
	}

	hint := "run `krowk auth verify` to see what this key is allowed to do"
	if !authenticated {
		hint = "this push was anonymous — run `krowk auth login --token krk_...` first"
	}
	body := maps.Clone(apiErr.Body)
	if fix, ok := body["fix"].(string); ok && fix != "" {
		body["fix"] = fix + " — " + hint
	} else {
		body["fix"] = hint
	}
	return &api.Error{Status: apiErr.Status, Body: body}
}

func authLogin(w io.Writer, token string) error {
	if token == "" {
		return api.Fail("missing_token", "pass the key: `krowk auth login --token krk_...`")
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
			"run `krowk auth login --token krk_...`, or upload anonymously")
	}
	fmt.Fprintln(w, token)
	return nil
}

// authVerify reports what the stored key can actually do, rather than trusting
// that a token-shaped string is a working key.
func authVerify(w io.Writer, format output.Format, env runctx.Env, quiet, colour bool) error {
	client := api.New(env("KROWK_API_URL"), api.ReadToken(env))
	if client.Token == "" {
		return api.Fail("not_authenticated",
			"no key to verify — run `krowk auth login --token krk_...`, or upload anonymously")
	}

	key, err := client.VerifyKey(context.Background())
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.Key(key, format, quiet, colour))
	return nil
}

func doctor(w io.Writer, format output.Format, env runctx.Env) error {
	client := api.New(env("KROWK_API_URL"), api.ReadToken(env))

	// One call answers both questions: whether the registry is there, and what
	// the key is good for. An HTTP response of any status proves reachability.
	key, keyErr := client.VerifyKey(context.Background())

	report := map[string]any{
		"version":       Version,
		"runtime":       runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH,
		"api":           client.BaseURL,
		"api_status":    reachability(keyErr),
		"authenticated": client.Token != "",
		"key":           keySummary(client.Token, key, keyErr),
		"credentials":   api.CredentialsPath(),
		"context":       runctx.Detect(env),
	}

	if format != output.Human {
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Fprintln(w, string(b))
		return nil
	}

	for _, k := range []string{"version", "runtime", "api", "api_status", "authenticated", "key", "credentials"} {
		fmt.Fprintf(w, "%-14s %v\n", k, report[k])
	}
	b, _ := json.Marshal(report["context"])
	fmt.Fprintf(w, "%-14s %s\n", "context", b)
	return nil
}

// reachability separates "the registry answered" from "nothing is listening".
func reachability(err error) string {
	if err == nil {
		return "reachable (HTTP 200)"
	}
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		if apiErr.Status != 0 {
			return fmt.Sprintf("reachable (HTTP %d)", apiErr.Status)
		}
		if detail, ok := apiErr.Body["detail"].(string); ok {
			return "unreachable — " + detail
		}
		// A verdict formed before any HTTP exchange, e.g. an unparseable URL.
		return "unreachable — " + apiErr.Code()
	}
	return "unreachable — " + err.Error()
}

// keySummary says what the key is good for in one line.
func keySummary(token string, key *api.Key, err error) string {
	if token == "" {
		return "none — uploads will be anonymous"
	}
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) {
			return "rejected — " + apiErr.Code()
		}
		return "unknown — " + err.Error()
	}
	scopes := strings.Join(key.Scopes, " ")
	if scopes == "" {
		scopes = "no scopes"
	}
	return fmt.Sprintf("%s (%s) %s", key.KeyID, key.Workspace, scopes)
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
