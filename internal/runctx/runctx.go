// Package runctx works out the run metadata an agent should never have to type.
package runctx

import (
	"os/exec"
	"regexp"
	"strings"
)

// Metadata is the JSON blob that rides along with an upload. Flags override
// every detected field; omitempty does the pruning.
type Metadata struct {
	Repo        string   `json:"repo,omitempty"`
	Commit      string   `json:"commit,omitempty"`
	CommitURL   string   `json:"commit_url,omitempty"`
	Dirty       bool     `json:"dirty,omitempty"`
	Branch      string   `json:"branch,omitempty"`
	Agent       string   `json:"agent,omitempty"`
	PullRequest string   `json:"pull_request,omitempty"`
	Reference   []string `json:"reference,omitempty"`
	Session     string   `json:"session,omitempty"`
	Title       string   `json:"title,omitempty"`
	Client      string   `json:"client,omitempty"`
}

// Env is a lookup function, so tests do not have to touch the process
// environment. Pass os.Getenv.
type Env func(string) string

// Overrides are the metadata fields a caller supplies rather than detects.
// Empty strings leave the detected value alone; the fields that cannot be
// detected at all are simply carried through.
type Overrides struct {
	Repo        string
	Commit      string
	Agent       string
	PullRequest string
	Reference   []string
	Session     string
	Title       string
	Client      string
}

// Resolve detects what it can and lets the caller's values win. Shared by the
// CLI and the MCP server so both report metadata the same way.
func Resolve(env Env, o Overrides) Metadata {
	m := Detect(env)
	override(&m.Repo, o.Repo)
	override(&m.Commit, o.Commit)
	override(&m.Agent, o.Agent)
	override(&m.PullRequest, o.PullRequest)
	override(&m.Session, o.Session)
	m.Reference = o.Reference
	m.Title = o.Title
	m.Client = o.Client
	// An overridden repo or commit makes the detected link point at the wrong
	// thing, so it is rebuilt rather than left stale.
	if o.Repo != "" || o.Commit != "" {
		m.CommitURL = CommitURL(env, git("remote", "get-url", "origin"), m.Repo, m.Commit)
	}
	return m
}

func override(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// Detect fills in everything that can be read off the machine.
func Detect(env Env) Metadata {
	remote := git("remote", "get-url", "origin")

	m := Metadata{
		Repo:        firstNonEmpty(env("GITHUB_REPOSITORY"), Slug(remote)),
		Commit:      firstNonEmpty(env("GITHUB_SHA"), git("rev-parse", "HEAD")),
		Branch:      git("rev-parse", "--abbrev-ref", "HEAD"),
		Dirty:       git("status", "--porcelain") != "",
		Agent:       DetectAgent(env),
		Session:     DetectSession(env),
		PullRequest: CIPullRequest(env),
	}
	m.CommitURL = CommitURL(env, remote, m.Repo, m.Commit)
	return m
}

// CommitURL links the commit on GitHub. It stays empty for any other host
// rather than guessing a path shape and emitting a link that 404s.
func CommitURL(env Env, remote, repo, commit string) string {
	if repo == "" || commit == "" {
		return ""
	}
	server := env("GITHUB_SERVER_URL") // set by Actions, and correct on Enterprise
	if server == "" && Host(remote) == "github.com" {
		server = "https://github.com"
	}
	if server == "" {
		return ""
	}
	return strings.TrimRight(server, "/") + "/" + repo + "/commit/" + commit
}

// DetectSession finds the agent run this upload belongs to, so --session is a
// correction rather than something the agent has to remember.
func DetectSession(env Env) string {
	return firstNonEmpty(
		env("KROWK_SESSION"),
		env("CLAUDE_CODE_SESSION_ID"),
		env("CURSOR_TRACE_ID"),
		env("GITHUB_RUN_ID"),
	)
}

// DetectAgent names the tool driving the upload.
func DetectAgent(env Env) string {
	switch {
	case env("KROWK_AGENT") != "":
		return env("KROWK_AGENT")
	case env("CLAUDECODE") != "" || env("CLAUDE_CODE") != "":
		return "claude-code"
	case env("CURSOR_TRACE_ID") != "":
		return "cursor"
	case env("GITHUB_ACTIONS") != "":
		return "github-actions"
	}
	return ""
}

var (
	slugRE  = regexp.MustCompile(`[:/]([^/:]+/[^/]+?)(?:\.git)?/?$`)
	prRefRE = regexp.MustCompile(`^refs/pull/(\d+)/`)
	hostRE  = regexp.MustCompile(`^(?:[a-z+]+://)?(?:[^@/]+@)?([^/:]+)[:/]`)
)

// Host pulls github.com out of either remote spelling.
func Host(remote string) string {
	if m := hostRE.FindStringSubmatch(remote); m != nil {
		return m[1]
	}
	return ""
}

// Slug turns git@github.com:acme/storefront.git into acme/storefront.
func Slug(remote string) string {
	if m := slugRE.FindStringSubmatch(remote); m != nil {
		return m[1]
	}
	return ""
}

// CIPullRequest turns refs/pull/412/merge into the pull request URL.
func CIPullRequest(env Env) string {
	m := prRefRE.FindStringSubmatch(env("GITHUB_REF"))
	repo := env("GITHUB_REPOSITORY")
	if m == nil || repo == "" {
		return ""
	}
	return "https://github.com/" + repo + "/pull/" + m[1]
}

// git returns trimmed stdout, or "" for any failure — no git, no repo, no remote.
func git(args ...string) string {
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
