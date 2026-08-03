package runctx

import "testing"

func env(pairs map[string]string) Env {
	return func(k string) string { return pairs[k] }
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:acme/storefront.git":     "acme/storefront",
		"https://github.com/acme/storefront":     "acme/storefront",
		"https://github.com/acme/storefront.git": "acme/storefront",
		"ssh://git@host.dev/acme/storefront/":    "acme/storefront",
		"":                                       "",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectAgent(t *testing.T) {
	if got := DetectAgent(env(map[string]string{"CLAUDECODE": "1"})); got != "claude-code" {
		t.Errorf("got %q", got)
	}
	if got := DetectAgent(env(map[string]string{"GITHUB_ACTIONS": "true"})); got != "github-actions" {
		t.Errorf("got %q", got)
	}
	// An explicit override beats detection.
	both := map[string]string{"KROWK_AGENT": "custom", "CLAUDECODE": "1"}
	if got := DetectAgent(env(both)); got != "custom" {
		t.Errorf("got %q, want the override", got)
	}
	if got := DetectAgent(env(nil)); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestHost(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:acme/storefront.git":     "github.com",
		"https://github.com/acme/storefront":     "github.com",
		"ssh://git@git.acme.dev:2222/acme/store": "git.acme.dev",
		"":                                       "",
	} {
		if got := Host(in); got != want {
			t.Errorf("Host(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommitURL(t *testing.T) {
	const sha = "757cc51982cd8291486f65e921d45d96a9f688a6"
	want := "https://github.com/acme/storefront/commit/" + sha

	if got := CommitURL(env(nil), "git@github.com:acme/storefront.git", "acme/storefront", sha); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Actions supplies the server, so Enterprise hosts link correctly.
	ghes := map[string]string{"GITHUB_SERVER_URL": "https://github.acme.dev/"}
	if got, want := CommitURL(env(ghes), "", "acme/storefront", sha),
		"https://github.acme.dev/acme/storefront/commit/"+sha; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// A non-GitHub remote gets no link rather than a broken one.
	if got := CommitURL(env(nil), "git@gitlab.com:acme/storefront.git", "acme/storefront", sha); got != "" {
		t.Errorf("got %q, want empty for a non-GitHub remote", got)
	}
	if got := CommitURL(env(nil), "git@github.com:acme/storefront.git", "acme/storefront", ""); got != "" {
		t.Errorf("got %q, want empty without a commit", got)
	}
}

func TestDetectSession(t *testing.T) {
	claude := map[string]string{"CLAUDE_CODE_SESSION_ID": "3fe6808d-088d-4a6f-a04c-cc9690bcf852"}
	if got := DetectSession(env(claude)); got != claude["CLAUDE_CODE_SESSION_ID"] {
		t.Errorf("got %q, want the Claude Code session", got)
	}
	if got := DetectSession(env(map[string]string{"GITHUB_RUN_ID": "18273645"})); got != "18273645" {
		t.Errorf("got %q, want the CI run", got)
	}
	// An explicit override beats detection.
	both := map[string]string{"KROWK_SESSION": "mine", "CLAUDE_CODE_SESSION_ID": "theirs"}
	if got := DetectSession(env(both)); got != "mine" {
		t.Errorf("got %q, want the override", got)
	}
	if got := DetectSession(env(nil)); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestCIPullRequest(t *testing.T) {
	in := map[string]string{"GITHUB_REF": "refs/pull/412/merge", "GITHUB_REPOSITORY": "acme/storefront"}
	if got, want := CIPullRequest(env(in)), "https://github.com/acme/storefront/pull/412"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// A branch build is not a pull request.
	branch := map[string]string{"GITHUB_REF": "refs/heads/main", "GITHUB_REPOSITORY": "acme/storefront"}
	if got := CIPullRequest(env(branch)); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
