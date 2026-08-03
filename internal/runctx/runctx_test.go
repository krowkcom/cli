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
