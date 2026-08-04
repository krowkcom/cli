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
	"net"
	"net/http"
	"runtime"
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
const defaultRegistryAddr = ":8787"

const helpTemplate = `krowk %s — permalinks for agent output

Usage
  krowk uploads create <file...> [flags]   Upload artifacts, get one canonical URL
  krowk push <file...> [flags]             Alias for ` + "`uploads create`" + `
  krowk auth login --token <token>         Store an API token
  krowk auth token                         Print the stored token
  krowk auth verify                        Check the key and its scopes
  krowk doctor                             Check the local setup
  krowk registry serve                     Run a local registry to develop against

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

Local registry flags
  --addr <host:port>     Listen address for ` + "`registry serve`" + ` (default %s)
  --site <url>           Origin for the links it returns (default: the request host)
  --limit-bytes <n>      Reject uploads above this size

Global flags
  --dev                  Talk to a local registry at %s
  --format <fmt>         human | json | markdown | url (default: human on a TTY, json when piped)
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

	// registry.Handler treats <= 0 as "use the default", so a negative limit
	// would silently mean 100 MiB. Reject it instead of guessing.
	if f.limitBytes < 0 {
		err := api.Fail("bad_flag", "--limit-bytes must not be negative — omit it or pass 0 for the default")
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
	case len(positionals) > 1 && positionals[0] == "auth" && positionals[1] == "login":
		err = authLogin(stdout, f.token)
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

	metadata := runctx.Resolve(env, runctx.Overrides{
		Repo:        f.repo,
		Commit:      f.commit,
		Agent:       f.agent,
		PullRequest: f.pullRequest,
		Reference:   f.references,
		Session:     f.session,
		Title:       f.title,
		Client:      "krowk-cli/" + Version,
	})

	client := newClient(f, env)
	artifact, err := client.CreateArtifact(context.Background(), files, metadata)
	if err != nil {
		return authHint(err, client.Token != "")
	}

	fmt.Fprintln(w, output.Artifact(artifact, format, f.title, f.quiet, colour, time.Now()))
	return nil
}

// newClient is the one place a registry client gets built, so every command
// honours the same precedence.
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

// registryServe runs the local stand-in for api.krowk.com, so developing against
// a registry needs neither the network nor a checkout of this repository.
func registryServe(w io.Writer, f flags) error {
	addr := f.addr
	if addr == "" {
		addr = defaultRegistryAddr
	}

	// Bind before announcing anything, so a script keying off the banner never
	// proceeds against a port that failed to open.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return api.Fail("registry_unavailable", "could not listen on "+addr+": "+err.Error())
	}
	fmt.Fprint(w, serveBanner(localBase(ln.Addr().String(), addr)))

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

// serveBanner says where the registry is and how to point a push at it. --dev
// only knows the default address, so any other base gets the explicit form.
func serveBanner(base string) string {
	banner := "krowk registry listening on " + base + "\n"
	if base+"/v1" == api.DevBaseURL {
		return banner + "  krowk push screenshot.png --dev\n"
	}
	return banner + "  KROWK_API_URL=" + base + "/v1 krowk push screenshot.png\n"
}

// localBase turns a listen address into a URL a client can call. bound is what
// the listener reports, which resolves ":0" to the real port; asked keeps the
// hostname the user typed, since the listener flattens it to an IP.
func localBase(bound, asked string) string {
	if strings.HasPrefix(asked, ":") {
		_, port, err := net.SplitHostPort(bound)
		if err != nil {
			return "http://localhost" + asked
		}
		return "http://localhost:" + port
	}
	return "http://" + asked
}

// authHint points a rejected upload at the self-check. The registry cannot know
// the CLI has a verify command, so the CLI adds that half of the fix itself.
func authHint(err error, authenticated bool) error {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	if apiErr.Status != http.StatusUnauthorized && apiErr.Status != http.StatusForbidden {
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
func authVerify(w io.Writer, format output.Format, f flags, env runctx.Env, colour bool) error {
	client := newClient(f, env)
	if client.Token == "" {
		return api.Fail("not_authenticated",
			"no key to verify — run `krowk auth login --token krk_...`, or upload anonymously")
	}

	key, err := client.VerifyKey(context.Background())
	if err != nil {
		return err
	}
	fmt.Fprintln(w, output.Key(key, format, colour))
	return nil
}

func doctor(w io.Writer, format output.Format, f flags, env runctx.Env) error {
	client := newClient(f, env)

	// One call answers both questions: whether the registry is there, and what
	// the key is good for. An HTTP response of any status proves reachability.
	key, keyErr := client.VerifyKey(context.Background())

	report := map[string]any{
		"version":       Version,
		"runtime":       runtime.Version() + " " + runtime.GOOS + "/" + runtime.GOARCH,
		"api":           client.BaseURL,
		"registry":      registryMode(client, env),
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

	for _, k := range []string{
		"version", "runtime", "api", "registry", "api_status", "authenticated", "key", "credentials",
	} {
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
		// A client-side verdict, e.g. a 200 that said valid:false.
		return "reachable (" + apiErr.Code() + ")"
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

func clip[T any](s []T, n int) []T {
	if len(s) < n {
		return s
	}
	return s[:n]
}
