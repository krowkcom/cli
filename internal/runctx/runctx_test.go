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

func TestRepoURL(t *testing.T) {
	for in, want := range map[string]string{
		// An https remote names the URL directly, shorn of .git.
		"https://github.com/acme/storefront.git": "https://github.com/acme/storefront",
		"https://git.acme.dev/acme/storefront/":  "https://git.acme.dev/acme/storefront",
		// An ssh remote gets a URL built only for a host whose web shape is known.
		"git@github.com:acme/storefront.git": "https://github.com/acme/storefront",
		// A non-GitHub ssh remote gets no URL rather than a guessed one that 404s.
		"git@gitlab.com:acme/storefront.git": "",
		"":                                   "",
	} {
		if got := RepoURL(env(nil), in); got != want {
			t.Errorf("RepoURL(%q) = %q, want %q", in, got, want)
		}
	}
	// Actions supplies the server, so Enterprise hosts link correctly.
	ghes := map[string]string{"GITHUB_SERVER_URL": "https://github.acme.dev/"}
	if got, want := RepoURL(env(ghes), "git@github.acme.dev:acme/storefront.git"),
		"https://github.acme.dev/acme/storefront"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestChangeID(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/storefront/pull/412":            "412",
		"https://gitlab.com/acme/storefront/-/merge_requests/7/": "7",
		"https://github.com/acme/storefront":                     "",
		"":                                                       "",
	} {
		if got := ChangeID(in); got != want {
			t.Errorf("ChangeID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectModelIsHarnessAgnostic(t *testing.T) {
	// KROWK_MODEL is ours, so it wins over any vendor signal.
	both := map[string]string{"KROWK_MODEL": "gpt-5.2", "ANTHROPIC_MODEL": "claude-fable-5"}
	if got := DetectModel(env(both)); got != "gpt-5.2" {
		t.Errorf("got %q, want the agnostic override", got)
	}
	if got := DetectModel(env(map[string]string{"ANTHROPIC_MODEL": "claude-fable-5"})); got != "claude-fable-5" {
		t.Errorf("got %q", got)
	}
	if got := DetectModel(env(nil)); got != "" {
		t.Errorf("got %q, want empty — no signal, no key", got)
	}
}

func TestDetectSystemFollowsTheModelFamily(t *testing.T) {
	for _, tc := range []struct{ harness, model, want string }{
		{"", "claude-fable-5", "anthropic"},
		{"", "gpt-5.2", "openai"},
		{"", "gemini-3-pro", "gcp.gemini"},
		{"claude-code", "", "anthropic"},
		{"cursor", "", ""},
		{"", "", ""},
	} {
		if got := DetectSystem(tc.harness, tc.model); got != tc.want {
			t.Errorf("DetectSystem(%q, %q) = %q, want %q", tc.harness, tc.model, got, tc.want)
		}
	}
}

func TestArtifactStampDropsTheRunOnlyFacts(t *testing.T) {
	dirty := true
	m := Metadata{
		RepoName: "acme/storefront", RepoURL: "https://github.com/acme/storefront",
		Commit: "9e6943f4", Branch: "fix/cart", Dirty: &dirty,
		Harness: "claude-code", Client: "krowk-cli/test",
		ChangeID: "412", ChangeTitle: "Cart total", Session: "01J8",
		ChangeURL:  "https://github.com/acme/storefront/pull/412",
		References: []string{"https://linear.app/acme/issue/STO-1"},
	}
	a := m.Artifact()
	if a.ChangeID != "" || a.ChangeTitle != "" || a.ChangeURL != "" || a.Session != "" || a.References != nil {
		t.Errorf("artifact stamp still carries run-only facts: %+v", a)
	}
	if a.RepoName != m.RepoName || a.Commit != m.Commit || a.Dirty != m.Dirty || a.Harness != m.Harness {
		t.Errorf("artifact stamp lost production facts: %+v", a)
	}
}

func TestWithExtrasLetsTheCallersKeyWin(t *testing.T) {
	m := Metadata{RepoName: "acme/storefront", Client: "krowk-cli/test"}
	out, ok := m.WithExtras(map[string]string{
		"vcs.repository.name": "acme/other",
		"krowk.caption":       "before the fix",
	}).(map[string]any)
	if !ok {
		t.Fatalf("want a merged map, got %T", m.WithExtras(map[string]string{"k": "v"}))
	}
	if out["vcs.repository.name"] != "acme/other" {
		t.Errorf("detected key survived an explicit override: %v", out["vcs.repository.name"])
	}
	if out["krowk.caption"] != "before the fix" {
		t.Errorf("caption = %v", out["krowk.caption"])
	}
	if out["krowk.client"] != "krowk-cli/test" {
		t.Errorf("client = %v, want it carried through the merge", out["krowk.client"])
	}
	// No extras: the stamp passes through untouched, still pruned by omitempty.
	if _, isMap := m.WithExtras(nil).(map[string]any); isMap {
		t.Error("want the struct back when there is nothing to merge")
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
