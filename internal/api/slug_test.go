package api

import (
	"errors"
	"strings"
	"testing"
)

// The shapes krowk hands out. Every one of them is something a person or an
// agent already holds after an upload, which is the whole reason for taking
// them: the pitch is the pasted link.
func TestParseSlugTakesTheLinksKrowkHandsOut(t *testing.T) {
	const artifact = "art_9f3c2e1a7b04d6c8e5f1a2b3"
	const run = "run_4b1a8d2c7e6f0359ab12cd34"

	for _, c := range []struct {
		name  string
		kind  string
		input string
		want  string
	}{
		{"the slug itself", KindArtifact, artifact, artifact},
		{"the card page", KindArtifact, "https://krowk.com/a/" + artifact, artifact},
		{"the card page with no scheme", KindArtifact, "krowk.com/a/" + artifact, artifact},
		{"the card page with a fragment", KindArtifact, "https://krowk.com/a/" + artifact + "#preview", artifact},
		{"the CDN URL", KindArtifact,
			"https://cdn.krowkusercontent.com/weur/ws_1a2b3c4d5e6f7g8h9i0j1k2l/" + artifact + "/checkout.png",
			artifact},
		{"a local registry's card page", KindArtifact, "http://localhost:8787/a/" + artifact, artifact},
		{"a local registry's storage path", KindArtifact,
			"http://localhost:8787/_storage/ws_1a2b3c4d5e6f7g8h9i0j1k2l/" + artifact + "/checkout.png",
			artifact},
		{"the DNS spelling, where an underscore is not legal", KindArtifact,
			"https://art-9f3c2e1a7b04d6c8e5f1a2b3.krowkusercontent.com/checkout.png", artifact},
		{"an uppercase paste", KindArtifact, "HTTPS://KROWK.COM/A/" + strings.ToUpper(artifact), artifact},
		{"surrounding whitespace", KindArtifact, "  https://krowk.com/a/" + artifact + "\n", artifact},
		{"a run link", KindRun, "https://app.krowk.com/runs/" + run, run},
		{"a run slug", KindRun, run, run},
		{"the run in a CDN URL is not the artifact", KindRun,
			"https://cdn.krowkusercontent.com/weur/ws_1a2b3c4d5e6f7g8h9i0j1k2l/" + artifact + "/x.png?run=" + run,
			run},
		{"a workspace out of a CDN URL", KindWorkspace,
			"https://cdn.krowkusercontent.com/weur/ws_1a2b3c4d5e6f7g8h9i0j1k2l/" + artifact + "/x.png",
			"ws_1a2b3c4d5e6f7g8h9i0j1k2l"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseSlug(c.kind, c.input)
			if err != nil {
				t.Fatalf("ParseSlug(%q) failed: %v", c.input, err)
			}
			if got != c.want {
				t.Errorf("ParseSlug(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

// The leniency is additive: anything that is not link-shaped is handed on
// exactly as it was typed, so the registry stays the authority on what a slug
// is. A client-side validator here would refuse slugs a later registry mints.
func TestParseSlugPassesAnythingThatIsNotALinkStraightThrough(t *testing.T) {
	for _, input := range []string{
		"art_nope",
		"art_9F3C2E1A7B04D6C8E5F1A2B3",
		"run_",
		"",
		"art_with_underscores",
	} {
		got, err := ParseSlug(KindArtifact, input)
		if err != nil {
			t.Fatalf("ParseSlug(%q) failed: %v", input, err)
		}
		if got != input {
			t.Errorf("ParseSlug(%q) = %q, want it passed through", input, got)
		}
	}
}

// A link with no slug of the wanted kind is refused here. Sent on, it would come
// back as "no such record" — which reads as an artifact that expired rather than
// as a URL that never named one.
func TestParseSlugRefusesALinkThatNamesNothing(t *testing.T) {
	_, err := ParseSlug(KindArtifact, "https://krowk.com/pricing")
	if err == nil {
		t.Fatal("a link with no slug in it was accepted")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "bad_artifact" {
		t.Fatalf("err = %v, want a bad_artifact failure", err)
	}
	if fix, _ := apiErr.Body["fix"].(string); !strings.Contains(fix, "krowk.com/a/art_") {
		t.Errorf("fix = %q, want it to show the shape that would work", fix)
	}
}

// A card link handed to a command that wants a run is the commonest way to get
// this wrong, so the refusal names the artifact it found rather than only the
// run it did not.
func TestParseSlugSaysWhichKindTheLinkActuallyNames(t *testing.T) {
	const artifact = "art_9f3c2e1a7b04d6c8e5f1a2b3"

	_, err := ParseSlug(KindRun, "https://krowk.com/a/"+artifact)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "bad_run" {
		t.Fatalf("err = %v, want a bad_run failure", err)
	}
	fix, _ := apiErr.Body["fix"].(string)
	if !strings.Contains(fix, artifact) || !strings.Contains(fix, "artifact") {
		t.Errorf("fix = %q, want it to name the artifact the link carries", fix)
	}
}

// The hyphen spelling only exists as a DNS label, where a slug's 24 random
// characters follow the prefix. A hyphenated word in a path is not a slug, and
// reading one as a slug would send a takedown at whatever it matched.
func TestParseSlugDoesNotReadAHyphenatedWordAsASlug(t *testing.T) {
	if _, err := ParseSlug(KindRun, "https://example.com/run-fast/report.html"); err == nil {
		t.Error("`run-fast` was read as a run slug")
	}
}

// A URL is full of tokens shaped like a slug and not one: `run_id=4821` in a CI
// link, a file called art_report.png, a job path with run_7 in it. Reading any
// of them as a slug would send krowk at a record nobody named.
func TestParseSlugDoesNotReadSlugShapedWordsOutOfALink(t *testing.T) {
	for _, c := range []struct{ kind, input string }{
		{KindRun, "https://ci.example.com/build?run_id=4821"},
		{KindRun, "https://ci.example.com/jobs/run_7/log.txt"},
		{KindArtifact, "https://example.com/art_1.png"},
		{KindArtifact, "./art_report.png"},
		{KindRun, "https://example.com/run-fast/report.html"},
	} {
		if slug, err := ParseSlug(c.kind, c.input); err == nil {
			t.Errorf("ParseSlug(%q) read %q as a slug", c.input, slug)
		}
	}
}

// Nothing and whitespace are different answers. Absent is how an optional run
// is not given; blank was typed, or a shell expanded an empty variable into it,
// and reading it as absent would silently widen the command it was passed to.
func TestParseSlugSeparatesAbsentFromBlank(t *testing.T) {
	if slug, err := ParseSlug(KindRun, ""); err != nil || slug != "" {
		t.Errorf("ParseSlug(\"\") = %q, %v — want absent", slug, err)
	}
	_, err := ParseSlug(KindRun, "   ")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "bad_run" {
		t.Errorf("a blank run gave %v, want bad_run", err)
	}
}

// ParseSlug is exported and takes a plain string, so a kind it has never heard
// of has to be an error rather than a panic.
func TestParseSlugSurvivesAnUnknownKind(t *testing.T) {
	_, err := ParseSlug("job", "https://krowk.com/a/art_9f3c2e1a7b04d6c8e5f1a2b3")
	if err == nil {
		t.Fatal("an unknown kind was accepted")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code() != "bad_job" {
		t.Errorf("err = %v, want bad_job", err)
	}
}

// Only the artifact has a public page, so only its refusal may point at one:
// there is no krowk.com/a/run_… to send anybody to.
func TestParseSlugDoesNotInventAPageForARun(t *testing.T) {
	_, err := ParseSlug(KindRun, "https://krowk.com/pricing")
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an api failure", err)
	}
	if fix, _ := apiErr.Body["fix"].(string); strings.Contains(fix, "/a/run_") {
		t.Errorf("fix = %q, want no artifact card path in a run's message", fix)
	}
}
