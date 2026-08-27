// Package runctx works out the run metadata an agent should never have to type.
//
// The key names are the canon vocabulary: OpenTelemetry's where OTel has a
// word, `krowk.`-namespaced where it does not. Nothing here writes a flat
// legacy key (`repo`, `commit`, …) — readers fall back to those, writers moved.
package runctx

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"unicode/utf8"
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
	Links       []Link   `json:"krowk.links,omitempty"`
	References  []string `json:"krowk.references,omitempty"`

	// remoteSlug remembers what the origin remote itself named, so an
	// overridden repository name can be checked against it. Never serialized.
	remoteSlug string
}

// Link is one structured reference: where it points, optionally what to call
// it and what kind of link it is. The canon vocabulary (`krowk.links` in
// engineering/metadata.md) fixes the shape — url required, title and rel
// optional, rel an open string rather than an enum so a writer with a kind the
// suggested list does not name records its own word instead of mislabelling.
type Link struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
	Rel   string `json:"rel,omitempty"`
}

// The limits links are held to. They are the vocabulary's, not the registry's:
// the registry validates size and nothing else, so a link that is malformed
// here is malformed in the record forever.
//
// MaxLinks bounds the count and MaxLinksBytes the whole of it, because the two
// answer different failures: twenty is what a run's links are worth reading,
// and twenty links at the per-link maximum is 40KB — past the registry's 16KB
// metadata cap, which would fail the run with the registry's own opaque
// refusal rather than with a sentence naming the flag. Half the cap, so the
// detected metadata always has room beside them.
//
// Lengths are counted in characters rather than bytes: a title cut at 140
// bytes is 46 characters of Japanese, and the number in the vocabulary is what
// a person writing a title counts.
const (
	MaxLinks      = 20
	MaxLinkURL    = 2048
	MaxLinkTitle  = 140
	MaxLinksBytes = 8192
)

// LinkRels are the suggested values for a link's rel, in the order a writer is
// likely to want them. Suggested, not enforced — see Link.
var LinkRels = []string{"tracks", "fixes", "spec", "discussion", "source", "supersedes"}

// ValidateLinks refuses what it cannot record faithfully rather than mangling
// it: a truncated URL is a link to somewhere else, and a title carrying a
// newline breaks every renderer that puts it on a row. Errors are plain, so
// each caller can dress them in its own failure code — `bad_flag` from the CLI,
// `bad_arguments` from the MCP server.
func ValidateLinks(links []Link) error {
	if len(links) > MaxLinks {
		return fmt.Errorf("%d links is more than the %d a run records: "+
			"link what the work is about, not everything it touched", len(links), MaxLinks)
	}
	for i, l := range links {
		where := fmt.Sprintf("link %d", i+1)
		if err := validateLinkURL(where, l.URL); err != nil {
			return err
		}
		switch {
		case utf8.RuneCountInString(l.Title) > MaxLinkTitle:
			return fmt.Errorf("%s has a %d-character title, past the %d one line holds",
				where, utf8.RuneCountInString(l.Title), MaxLinkTitle)
		case strings.ContainsFunc(l.Title, func(r rune) bool { return r == '\r' || r == '\n' }):
			return fmt.Errorf("%s has a title spanning more than one line: "+
				"a title is what a reader sees instead of the URL, on one row", where)
		}
	}
	// The whole of them, not each: the per-link maxima multiply past the
	// registry's 16KB metadata cap, and a refusal from there names nothing.
	if n := linksBytes(links); n > MaxLinksBytes {
		return fmt.Errorf("the links come to %d bytes of metadata, past the %d they may fill: "+
			"a run's metadata budget is shared with everything krowk detects", n, MaxLinksBytes)
	}
	return nil
}

// validateLinkURL holds a URL to what can be pasted and followed. Parsed
// rather than prefix-matched: a string that reads as a URL and does not parse
// as one is a link nothing can follow, and it is stored verbatim, so this is
// the only place it is ever looked at.
func validateLinkURL(where, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%s has no url: every link needs an absolute http(s) URL", where)
	}
	// Before parsing, because url.Parse accepts a space in a path and every
	// renderer that puts the URL in markdown then breaks on it — and a URL is
	// one word by definition.
	if strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return fmt.Errorf("%s (%q) has a space or a control character in it: "+
			"a URL is one unbroken word", where, raw)
	}
	if utf8.RuneCountInString(raw) > MaxLinkURL {
		return fmt.Errorf("%s is %d characters, past the %d a URL may be",
			where, utf8.RuneCountInString(raw), MaxLinkURL)
	}
	u, err := url.Parse(raw)
	// The scheme is compared lowercased because it is case-insensitive in the
	// URL itself: HTTPS://example.com is the same link, and refusing it would
	// point the caller at --reference for a URL that is perfectly good.
	if err != nil || (strings.ToLower(u.Scheme) != "http" && strings.ToLower(u.Scheme) != "https") || u.Host == "" {
		return fmt.Errorf("%s (%s) is not an absolute http(s) URL — "+
			"a ticket key or an internal ID is a --reference, not a link", where, raw)
	}
	return nil
}

// linksBytes is what these links will occupy in the record: the JSON, since
// that is what the registry counts against its cap.
func linksBytes(links []Link) int {
	if len(links) == 0 {
		return 0
	}
	b, err := json.Marshal(links)
	if err != nil {
		return 0
	}
	return len(b)
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
	Links       []Link
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
	m.Links = o.Links
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
// GITHUB_REF_NAME on a push when the ref is a branch — a tag push carries the
// tag there, and a tag is not a branch. A local detached HEAD records no
// branch at all, which is the truth of it.
func Branch(env Env) string {
	if b := ciBranch(env); b != "" {
		return b
	}
	if b := git("rev-parse", "--abbrev-ref", "HEAD"); b != "HEAD" {
		return b
	}
	return ""
}

// ciBranch is the environment half of Branch, on its own so it can be tested
// without a repository underneath.
func ciBranch(env Env) string {
	if b := env("GITHUB_HEAD_REF"); b != "" {
		return b
	}
	if b := env("GITHUB_REF_NAME"); b != "" && env("GITHUB_REF_TYPE") == "branch" {
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
// the session, the links and the references.
//
// The vocabulary allows krowk.links on an artifact too, for a link that is
// about the one file. Nothing here writes one: --link names what the work is
// about, and copying the work's links onto every artifact would claim the file
// is what each of them points at.
func (m Metadata) Artifact() Metadata {
	m.ChangeID, m.ChangeTitle, m.ChangeURL, m.Session = "", "", "", ""
	m.Links, m.References = nil, nil
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
