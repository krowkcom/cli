package cli

import (
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/output"
)

// The breadcrumbs are only worth anything if the commands they name work when
// they are run, so each one is taken from the output of the command that
// suggested it and then run against the same registry.

// A keyless push hands back the token that keeps the upload — and it has to be
// the token that upload actually came back with, not a shape that looks right.
func TestTheClaimBreadcrumbFromAKeylessPushRuns(t *testing.T) {
	h := newHarness(t, 0).anonymous()

	e := h.ok("push", h.fixture)
	uploaded := only(t, e)
	crumb, ok := findCrumb(e.Breadcrumbs, "krowk claim ")
	if !ok {
		t.Fatalf("a keyless push suggested no claim: %+v", e.Breadcrumbs)
	}
	if crumb.Description == "" {
		t.Error("the claim breadcrumb says nothing about why it matters")
	}

	// Run exactly what it said, with a key, the way whoever read it would.
	h.env["KROWK_TOKEN"] = "krowk_sk_test"
	claimed := only(t, h.ok(strings.Fields(crumb.Cmd)[1:]...))
	if claimed.Slug != uploaded.Slug {
		t.Errorf("the claim breadcrumb claimed %q, want %q", claimed.Slug, uploaded.Slug)
	}
	if claimed.ExpiresAt != "" {
		t.Errorf("expires_at = %q, want the upload kept", claimed.ExpiresAt)
	}
}

// A person watching a keyless push sees the command too. The token is shown
// once, by this response, so leaving it to `--json` would lose it for anyone
// running krowk by hand.
func TestTheHumanKeylessPushPrintsTheClaimCommand(t *testing.T) {
	h := newHarness(t, 0).anonymous()

	_, stdout, _ := h.run("push", h.fixture, "--format=human")
	if !strings.Contains(stdout, "krowk claim art_") ||
		!strings.Contains(stdout, "krowk_claim_") {
		t.Errorf("human keyless push does not print the claim command:\n%s", stdout)
	}

	// A keyed push has no token to spend, and gains no line.
	h.env["KROWK_TOKEN"] = "krowk_sk_test"
	_, stdout, _ = h.run("push", h.fixture, "--format=human")
	if strings.Contains(stdout, "krowk claim") {
		t.Errorf("keyed push printed a claim command:\n%s", stdout)
	}
}

// `runs start` opens something that stays open, and its artifacts have to be
// told about it. Both breadcrumbs are run.
func TestTheBreadcrumbsFromRunsStartRun(t *testing.T) {
	h := newHarness(t, 0)

	e := h.ok("runs", "start")
	slug := e.Data.Slug
	push, ok := findCrumb(e.Breadcrumbs, "krowk push")
	if !ok {
		t.Fatalf("runs start suggested no push: %+v", e.Breadcrumbs)
	}
	if push.Cmd != "krowk push <file> --run "+slug {
		t.Errorf("push breadcrumb = %q", push.Cmd)
	}
	finish, ok := findCrumb(e.Breadcrumbs, "runs finish")
	if !ok {
		t.Fatalf("runs start suggested no finish: %+v", e.Breadcrumbs)
	}

	// <file> is the one placeholder, and it is the caller's to fill in.
	uploaded := only(t, h.ok(strings.Fields(strings.Replace(push.Cmd, "<file>", h.fixture, 1))[1:]...))
	if uploaded.Run != slug {
		t.Errorf("the push breadcrumb put the upload in %q, want %q", uploaded.Run, slug)
	}

	closed := h.ok(strings.Fields(finish.Cmd)[1:]...)
	if closed.Data.Status != "finished" {
		t.Errorf("the finish breadcrumb left the run %q", closed.Data.Status)
	}
	// And a closed run does not suggest closing itself again.
	if _, ok := findCrumb(closed.Breadcrumbs, "runs finish"); ok {
		t.Errorf("a closed run was told to close: %+v", closed.Breadcrumbs)
	}
}

// Claiming keeps an upload but leaves it under no run, which is the one thing
// `uploads attach` exists for. The run is the caller's to choose, so the command
// comes back with a placeholder — and it works once that is filled in.
func TestClaimingWithoutARunHandsBackTheAttach(t *testing.T) {
	h := newHarness(t, 0)
	run := h.ok("runs", "start").Data.Slug

	h.anonymous()
	uploaded := only(t, h.ok("push", h.fixture))
	h.env["KROWK_TOKEN"] = "krowk_sk_test"

	e := h.ok("claim", uploaded.Slug, uploaded.ClaimToken)
	crumb, ok := findCrumb(e.Breadcrumbs, "uploads attach")
	if !ok {
		t.Fatalf("a claim with no run suggested no attach: %+v", e.Breadcrumbs)
	}
	if crumb.Cmd != "krowk uploads attach "+uploaded.Slug+" --run <run>" {
		t.Errorf("attach breadcrumb = %q", crumb.Cmd)
	}

	attached := only(t, h.ok(strings.Fields(strings.Replace(crumb.Cmd, "<run>", run, 1))[1:]...))
	if attached.Run != run {
		t.Errorf("the attach breadcrumb put it in %q, want %q", attached.Run, run)
	}
}

// `claim --run` already grouped it, so suggesting the attach would be advice to
// repeat a call that has already succeeded.
func TestClaimingIntoARunSuggestsNoAttach(t *testing.T) {
	h := newHarness(t, 0)
	run := h.ok("runs", "start").Data.Slug

	h.anonymous()
	uploaded := only(t, h.ok("push", h.fixture))
	h.env["KROWK_TOKEN"] = "krowk_sk_test"

	e := h.ok("claim", uploaded.Slug, uploaded.ClaimToken, "--run="+run)
	if _, ok := findCrumb(e.Breadcrumbs, "uploads attach"); ok {
		t.Errorf("an upload already in a run was told to attach it: %+v", e.Breadcrumbs)
	}
}

// The interactive default is the one a person sees, and it prints nothing about
// a run an upload does not have — so without this line, claiming by hand never
// mentions that `uploads attach` exists.
func TestTheHumanClaimPrintsTheAttachCommand(t *testing.T) {
	h := newHarness(t, 0)
	runSlug := h.ok("runs", "start").Data.Slug

	h.anonymous()
	uploaded := only(t, h.ok("push", h.fixture))
	h.env["KROWK_TOKEN"] = "krowk_sk_test"

	_, stdout, _ := h.run("claim", uploaded.Slug, uploaded.ClaimToken, "--format=human")
	want := "krowk uploads attach " + uploaded.Slug + " --run <run>"
	if !strings.Contains(stdout, want) {
		t.Errorf("human claim does not print %q:\n%s", want, stdout)
	}

	// And what it printed works, once the run is substituted for the placeholder.
	attached := only(t, h.ok(strings.Fields(strings.Replace(want, "<run>", runSlug, 1))[1:]...))
	if attached.Run != runSlug {
		t.Errorf("the printed attach put it in %q, want %q", attached.Run, runSlug)
	}
}

// --quiet asks for the record and nothing suggested, whichever format renders
// it. The human path is where this leaked: the format was dispatched on before
// quiet was read at all.
func TestQuietPrintsNoBreadcrumbLineForAPerson(t *testing.T) {
	h := newHarness(t, 0).anonymous()

	_, stdout, _ := h.run("push", h.fixture, "--format=human", "--quiet")
	if strings.Contains(stdout, "krowk claim") || strings.Contains(stdout, "keep it") {
		t.Errorf("--quiet on a terminal printed the claim line:\n%s", stdout)
	}
}

func findCrumb(crumbs []output.Breadcrumb, substring string) (output.Breadcrumb, bool) {
	for _, c := range crumbs {
		if strings.Contains(c.Cmd, substring) {
			return c, true
		}
	}
	return output.Breadcrumb{}, false
}
