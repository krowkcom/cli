// Package cli is the krowk command line. Run is the whole entry point, taking
// its streams and environment as arguments so tests never touch the process.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
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
  krowk doctor                             Check the local setup

Upload flags
  --pull-request <url>   Pull request the work belongs to
  --reference <url>      Related link — repeat for more than one
  --session <id>         Agent session ID
  --title <text>         Label for the unfurl card
  --repo <owner/name>    Override the detected repository
  --commit <sha>         Override the detected commit
  --agent <name>         Override the detected agent

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
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return api.Fail("file_unreadable",
				"cannot read `"+path+"` — paths resolve from the current directory")
		}
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
		return err
	}

	fmt.Fprintln(w, output.Artifact(artifact, format, f.title, f.quiet, colour, time.Now()))
	return nil
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

func doctor(w io.Writer, format output.Format, env runctx.Env) error {
	client := api.New(env("KROWK_API_URL"), api.ReadToken(env))

	status := probe(client)

	report := map[string]any{
		"version":       Version,
		"runtime":       runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH,
		"api":           client.BaseURL,
		"api_status":    status,
		"authenticated": client.Token != "",
		"credentials":   api.CredentialsPath(),
		"context":       runctx.Detect(env),
	}

	if format != output.Human {
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Fprintln(w, string(b))
		return nil
	}

	for _, k := range []string{"version", "runtime", "api", "api_status", "authenticated", "credentials"} {
		fmt.Fprintf(w, "%-14s %v\n", k, report[k])
	}
	b, _ := json.Marshal(report["context"])
	fmt.Fprintf(w, "%-14s %s\n", "context", b)
	return nil
}

// probe asks the registry whether it is there at all, without uploading.
func probe(client *api.Client) string {
	req, err := http.NewRequest(http.MethodOptions, client.BaseURL+"/artifacts", nil)
	if err != nil {
		return "unreachable — " + err.Error()
	}
	res, err := client.HTTP.Do(req)
	if err != nil {
		return "unreachable — " + err.Error()
	}
	res.Body.Close()
	return fmt.Sprintf("reachable (HTTP %d)", res.StatusCode)
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
