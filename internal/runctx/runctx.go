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

// Detect fills in everything that can be read off the machine.
func Detect(env Env) Metadata {
	return Metadata{
		Repo:        firstNonEmpty(env("GITHUB_REPOSITORY"), Slug(git("remote", "get-url", "origin"))),
		Commit:      firstNonEmpty(env("GITHUB_SHA"), git("rev-parse", "HEAD")),
		Branch:      git("rev-parse", "--abbrev-ref", "HEAD"),
		Agent:       DetectAgent(env),
		PullRequest: CIPullRequest(env),
	}
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
)

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
