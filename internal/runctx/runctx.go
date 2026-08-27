// Package runctx works out the run metadata an agent should never have to type.
//
// The key names are the canon vocabulary: OpenTelemetry's where OTel has a
// word, `krowk.`-namespaced where it does not. Nothing here writes a flat
// legacy key (`repo`, `commit`, …) — readers fall back to those, writers moved.
package runctx

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
)

// Metadata is the JSON blob that rides along with a record. Flags override
// every detected field; omitempty does the pruning.
//
// One struct serves both records: a run carries all of it, an artifact carries
// the production snapshot only — Artifact() clears the run-only facts.
type Metadata struct {
	RepoName string `json:"vcs.repository.name,omitempty"`
	RepoURL  string `json:"vcs.repository.url.full,omitempty"`
	Commit   string `json:"vcs.ref.head.revision,omitempty"`
	Branch   string `json:"vcs.ref.head.name,omitempty"`
	// Dirty is a pointer so "clean tree" is recorded as false while "not a git
	// checkout" records nothing at all — every key is optional, never defaulted.
	Dirty   *bool  `json:"krowk.vcs.dirty,omitempty"`
	Harness string `json:"krowk.harness,omitempty"`
	// Model and System are recorded only when the environment actually says.
	// KROWK_MODEL is the harness-agnostic spelling any wrapper can export;
	// the vendor variables are the signals specific harnesses already honour.
	// No signal, no key: a guessed model in an audit record is worse than
	// none, and --metadata can always spell one out.
	Model  string `json:"gen_ai.request.model,omitempty"`
	System string `json:"gen_ai.system,omitempty"`
	Client string `json:"krowk.client,omitempty"`

	// Facts about the work, not about one file: run-only.
	ChangeID    string   `json:"vcs.change.id,omitempty"`
	ChangeTitle string   `json:"vcs.change.title,omitempty"`
	ChangeURL   string   `json:"krowk.change.url,omitempty"`
	Session     string   `json:"krowk.session,omitempty"`
	References  []string `json:"krowk.references,omitempty"`

	// remoteSlug remembers what the origin remote itself named, so an
	// overridden repository name can be checked against it. Never serialized.
	remoteSlug string
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
	References  []string
	Session     string
	Title       string
	Client      string
}

// Resolve detects what it can and lets the caller's values win. Shared by the
// CLI and the MCP server so both report metadata the same way.
func Resolve(env Env, o Overrides) Metadata {
	m := Detect(env)
	override(&m.RepoName, o.Repo)
	override(&m.Commit, o.Commit)
	override(&m.Harness, o.Agent)
	override(&m.ChangeURL, o.PullRequest)
	override(&m.Session, o.Session)
	m.References = o.References
	m.ChangeTitle = o.Title
	m.Client = o.Client
	// An overridden harness re-decides the provider: the claim has to follow
	// the correction, not the detection it replaced.
	m.System = DetectSystem(m.Harness, m.Model)
	m.ChangeID = ChangeID(m.ChangeURL)
	m.reconcileRepo()
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
		RepoName:   firstNonEmpty(env("GITHUB_REPOSITORY"), Slug(remote)),
		RepoURL:    RepoURL(env, remote),
		Commit:     firstNonEmpty(env("GITHUB_SHA"), git("rev-parse", "HEAD")),
		Branch:     Branch(env),
		Dirty:      dirty(),
		Harness:    DetectAgent(env),
		Model:      DetectModel(env),
		Session:    DetectSession(env),
		ChangeURL:  CIPullRequest(env),
		remoteSlug: Slug(remote),
	}
	m.System = DetectSystem(m.Harness, m.Model)
	m.ChangeID = ChangeID(m.ChangeURL)
	m.reconcileRepo()
	return m
}

// Branch names the branch the work is on. CI checks out a detached HEAD, so
// `git rev-parse --abbrev-ref HEAD` answers the literal string "HEAD" there —
// the branch is in the environment instead: GITHUB_HEAD_REF on a pull request
// (the source branch, where GITHUB_REF_NAME would say `412/merge`), and
// GITHUB_REF_NAME on a push. A local detached HEAD records no branch at all,
// which is the truth of it.
func Branch(env Env) string {
	if b := firstNonEmpty(env("GITHUB_HEAD_REF"), env("GITHUB_REF_NAME")); b != "" {
		return b
	}
	if b := git("rev-parse", "--abbrev-ref", "HEAD"); b != "HEAD" {
		return b
	}
	return ""
}

// DetectModel names the model that did the work. KROWK_MODEL first, because it
// is ours and any harness can export it; ANTHROPIC_MODEL is a signal Claude
// Code already honours, so when it is set it names the model in use.
func DetectModel(env Env) string {
	return firstNonEmpty(
		env("KROWK_MODEL"),
		env("ANTHROPIC_MODEL"),
	)
}

// DetectSystem names the provider, and only when something actually implies
// it: the model's own family, or Claude Code as the harness — that harness
// runs Anthropic models wherever they are hosted. Anything else stays unsaid
// rather than guessed; --metadata gen_ai.system=... spells out the rest.
func DetectSystem(harness, model string) string {
	switch {
	case strings.HasPrefix(model, "claude"):
		return "anthropic"
	case strings.HasPrefix(model, "gpt"):
		return "openai"
	case strings.HasPrefix(model, "gemini"):
		return "gcp.gemini"
	case harness == "claude-code":
		return "anthropic"
	}
	return ""
}

// Artifact is the production stamp for one file: the snapshot of this moment,
// minus the facts that belong to the work rather than the file — the change,
// the session, the references.
func (m Metadata) Artifact() Metadata {
	m.ChangeID, m.ChangeTitle, m.ChangeURL, m.Session = "", "", "", ""
	m.References = nil
	return m
}

// WithExtras lays caller-supplied keys over the stamp. The caller's value wins
// on any collision, the standard keys included: those are detected or
// defaulted, so a caller spelling one out is correcting a detection.
func (m Metadata) WithExtras(extras map[string]string) any {
	if len(extras) == 0 {
		return m
	}
	b, _ := json.Marshal(m)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	for k, v := range extras {
		out[k] = v
	}
	return out
}

// reconcileRepo drops the repository URL when the name and the URL disagree:
// the URL is read off the checkout's remote while the name can arrive by flag
// or from CI, and a link to the wrong repository is worse than no link.
func (m *Metadata) reconcileRepo() {
	if m.remoteSlug != "" && m.RepoName != m.remoteSlug {
		m.RepoURL = ""
	}
}

// RepoURL is the repository's home on its host. An https remote names it
// directly; an ssh remote gets one built only for a host whose web shape is
// known, rather than guessing a path that 404s.
func RepoURL(env Env, remote string) string {
	if strings.HasPrefix(remote, "http://") || strings.HasPrefix(remote, "https://") {
		return strings.TrimSuffix(strings.TrimRight(remote, "/"), ".git")
	}
	slug := Slug(remote)
	if slug == "" {
		return ""
	}
	server := env("GITHUB_SERVER_URL") // set by Actions, and correct on Enterprise
	if server == "" && Host(remote) == "github.com" {
		server = "https://github.com"
	}
	if server == "" {
		return ""
	}
	return strings.TrimRight(server, "/") + "/" + slug
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

// DetectAgent names the harness driving the upload.
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
	slugRE     = regexp.MustCompile(`[:/]([^/:]+/[^/]+?)(?:\.git)?/?$`)
	prRefRE    = regexp.MustCompile(`^refs/pull/(\d+)/`)
	hostRE     = regexp.MustCompile(`^(?:[a-z+]+://)?(?:[^@/]+@)?([^/:]+)[:/]`)
	changeIDRE = regexp.MustCompile(`/(\d+)/?$`)
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

// ChangeID is the change's number, derived from its URL — OTel has a key for
// the number but none for the URL, so the URL is what gets recorded raw.
func ChangeID(changeURL string) string {
	if m := changeIDRE.FindStringSubmatch(changeURL); m != nil {
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

// dirty reports the worktree state, or nil outside a git checkout — the
// distinction a plain bool cannot carry.
func dirty() *bool {
	out, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		return nil
	}
	d := strings.TrimSpace(string(out)) != ""
	return &d
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
