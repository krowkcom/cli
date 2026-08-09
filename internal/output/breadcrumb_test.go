package output

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/krowkcom/cli/internal/api"
)

// Every breadcrumb carries all three fields. A caller reading `description` to
// decide and `cmd` to run must not have to handle either being absent.
func TestEveryBreadcrumbIsWholeAndRunnable(t *testing.T) {
	now := time.Now()
	anon := &api.Artifact{Slug: "art_2e1d", Filename: "shot.png", URL: "https://cdn/x",
		ClaimToken: "krowk_claim_2b7f"}
	claimed := &api.Artifact{Slug: "art_2e1d", Filename: "shot.png", URL: "https://cdn/x"}

	rendered := map[string]string{
		"keyless push": Upload(Result{Artifacts: []*api.Artifact{anon}}, JSON, false, false, now),
		"claim":        Claimed(claimed, JSON, false, false, now),
		"runs start":   Run(&api.Run{Slug: "run_7f", Status: "open"}, JSON, false, false),
		"runs finish":  Run(&api.Run{Slug: "run_7f", Status: "finished"}, JSON, false, false),
		"runs show":    RunDetail(&api.Run{Slug: "run_7f", Status: "open"}, JSON, false, false),
		"runs list":    RunList(&api.RunPage{Runs: []*api.Run{{Slug: "run_7f"}}, Next: "run_6a"}, Listing{}, JSON, false, false),
		"uploads list": List(&api.Page{Artifacts: []*api.Artifact{claimed}, Next: "art_1a"}, Listing{}, JSON, false, false, now),
		"auth verify":  Key(&api.Key{KeyID: "key_1", Workspace: "ws_1"}, JSON, false, false),
		"auth login":   StoredKey(&Login{Path: "/tmp/c", Confirmed: true, KeyID: "key_1", Workspace: "ws_1"}, JSON, false, false),
	}

	for name, out := range rendered {
		for _, b := range crumbsOf(t, out) {
			if b.Action == "" || b.Cmd == "" || b.Description == "" {
				t.Errorf("%s: incomplete breadcrumb %+v", name, b)
			}
			// A command carrying an unfilled placeholder that is not spelled as one
			// is a command that fails when it is pasted.
			if strings.Contains(b.Cmd, "…") {
				t.Errorf("%s: breadcrumb cmd %q is not runnable", name, b.Cmd)
			}
			// Every cmd is a krowk command, save the share crumb, which carries the
			// link itself because no OS agrees on a command for handing one over.
			if b.Action != "share" && !strings.HasPrefix(b.Cmd, "krowk ") {
				t.Errorf("%s: breadcrumb cmd %q is not a krowk command", name, b.Cmd)
			}
			// A push suggested by something that never saw a file names <file>, not a
			// plausible screenshot.png an agent would run and get a file-not-found for.
			if strings.HasPrefix(b.Cmd, "krowk push ") && !strings.Contains(b.Cmd, "<file>") {
				t.Errorf("%s: push breadcrumb %q names a file nobody mentioned", name, b.Cmd)
			}
			// Whatever is angle-bracketed is a word to substitute. A placeholder
			// spelled any other way is one a shell reads as redirection.
			if strings.ContainsAny(b.Cmd, "<>") && !placeholder.MatchString(b.Cmd) {
				t.Errorf("%s: breadcrumb cmd %q has a malformed placeholder", name, b.Cmd)
			}
			// The description has to say what to do with it, or the placeholder is
			// left to be guessed at.
			for _, p := range placeholder.FindAllString(b.Cmd, -1) {
				if !strings.Contains(b.Description, p) {
					t.Errorf("%s: cmd %q carries %s and the description never mentions it",
						name, b.Cmd, p)
				}
			}
		}
	}
}

// placeholder is the one shape a value this side does not have may take:
// angle-bracketed, one word, so it cannot be read as a value and cannot be
// pasted into a shell as it stands.
var placeholder = regexp.MustCompile(`<[a-z]+>`)

// The field is omitted rather than sent empty, so a caller can tell "nothing
// left to do" from "a list of nothing".
func TestBreadcrumbsAreOmittedWhenThereAreNone(t *testing.T) {
	out := Removed("art_2e1d", JSON, false, false)
	if strings.Contains(out, "breadcrumbs") {
		t.Errorf("a takedown carries breadcrumbs it has nothing to suggest:\n%s", out)
	}

	// And present when there are some, in the same envelope shape.
	out = Upload(Result{Artifacts: []*api.Artifact{{Slug: "art_2e1d", URL: "https://cdn/x"}}},
		JSON, false, false, time.Now())
	if !strings.Contains(out, `"breadcrumbs"`) {
		t.Errorf("an upload carries no breadcrumbs:\n%s", out)
	}
}

// --quiet is the raw record, so nothing suggestive belongs in it — in either
// format. The human path is the one that got this wrong: Upload dispatched on
// format before it read quiet, so `krowk push --quiet` on a terminal went on
// printing the claim line the README promises it does not.
func TestQuietCarriesNoBreadcrumbs(t *testing.T) {
	anon := &api.Artifact{Slug: "art_2e1d", Filename: "shot.png", URL: "https://cdn/x",
		ClaimToken: "krowk_claim_2b7f"}
	r := Result{Artifacts: []*api.Artifact{anon}}

	if out := Upload(r, JSON, true, false, time.Now()); strings.Contains(out, "breadcrumbs") {
		t.Errorf("--quiet --json leaked breadcrumbs:\n%s", out)
	}
	out := Upload(r, Human, true, false, time.Now())
	if strings.Contains(out, "keep it") || strings.Contains(out, "krowk claim") {
		t.Errorf("--quiet on a terminal leaked the claim line:\n%s", out)
	}
	// The token itself still comes back, because it is the record and not a
	// suggestion — a quiet push that hid it would lose the upload.
	if quiet := Claimed(&api.Artifact{Slug: "art_2e1d", URL: "https://cdn/x"},
		Human, true, false, time.Now()); strings.Contains(quiet, "group it") {
		t.Errorf("--quiet on a claim leaked the attach line:\n%s", quiet)
	}
}

// One token, one upload: three anonymous files come back with three tokens, and
// a caller that spends only the first loses the other two within the day.
func TestEveryAnonymousUploadGetsItsOwnClaimCommand(t *testing.T) {
	r := Result{Artifacts: []*api.Artifact{
		{Slug: "art_a", URL: "https://cdn/a", ClaimToken: "krowk_claim_a"},
		{Slug: "art_b", URL: "https://cdn/b", ClaimToken: "krowk_claim_b"},
	}}

	var claims []string
	for _, b := range crumbsOf(t, Upload(r, JSON, false, false, time.Now())) {
		if strings.HasPrefix(b.Cmd, "krowk claim ") {
			claims = append(claims, b.Cmd)
		}
	}
	want := []string{"krowk claim art_a krowk_claim_a", "krowk claim art_b krowk_claim_b"}
	if len(claims) != 2 || claims[0] != want[0] || claims[1] != want[1] {
		t.Errorf("claim breadcrumbs = %q, want %q", claims, want)
	}
}

// A keyed push has nothing to claim, so it must not suggest claiming anything.
func TestAKeyedUploadSuggestsNoClaim(t *testing.T) {
	r := Result{Artifacts: []*api.Artifact{{Slug: "art_a", URL: "https://cdn/a"}}}
	for _, b := range crumbsOf(t, Upload(r, JSON, false, false, time.Now())) {
		if strings.Contains(b.Cmd, "claim") {
			t.Errorf("keyed upload suggests %q", b.Cmd)
		}
	}
}

// The token is shown once, by this response, so a person watching a keyless push
// gets the command that spends it without having to re-render as JSON.
func TestTheHumanKeylessPushPrintsTheClaimCommand(t *testing.T) {
	a := &api.Artifact{Slug: "art_2e1d", Filename: "shot.png", ByteSize: 27,
		URL: "https://cdn/x", ClaimToken: "krowk_claim_2b7f"}

	out := Upload(Result{Artifacts: []*api.Artifact{a}}, Human, false, false, time.Now())
	if !strings.Contains(out, "krowk claim art_2e1d krowk_claim_2b7f") {
		t.Errorf("human keyless push does not print the claim command:\n%s", out)
	}
	// The same command the envelope hands an agent, not a second spelling of it.
	if !strings.Contains(out, ClaimCrumb(a).Cmd) {
		t.Errorf("human output and the breadcrumb disagree:\n%s", out)
	}

	// A keyed upload has no token, and gains no line.
	keyed := Upload(Result{Artifacts: []*api.Artifact{{Filename: "shot.png", URL: "https://cdn/x"}}},
		Human, false, false, time.Now())
	if strings.Contains(keyed, "keep it") {
		t.Errorf("keyed push printed a claim line:\n%s", keyed)
	}
}

// The paste forms are what goes into a pull request, so the token may not reach
// them however it is surfaced elsewhere.
func TestTheClaimLineNeverReachesAPasteForm(t *testing.T) {
	r := Result{Artifacts: []*api.Artifact{
		{Slug: "art_2e1d", Filename: "shot.png", URL: "https://cdn/x", ClaimToken: "krowk_claim_2b7f"}}}

	for _, f := range []Format{Markdown, URL} {
		if got := Upload(r, f, false, false, time.Now()); strings.Contains(got, "claim") {
			t.Errorf("--format %s leaked the claim token: %s", f, got)
		}
	}
}

// A claim leaves the upload owned but under no run, and `uploads attach` is the
// only way it ever gets one — so that is the command a claim hands back.
func TestClaimingWithoutARunSaysHowToGroupIt(t *testing.T) {
	a := &api.Artifact{Slug: "art_2e1d", Filename: "shot.png", URL: "https://cdn/x"}

	crumb, ok := find(crumbsOf(t, Claimed(a, JSON, false, false, time.Now())), "uploads attach")
	if !ok {
		t.Fatalf("a claimed upload with no run suggests no attach")
	}
	if crumb.Cmd != "krowk uploads attach art_2e1d --run <run>" {
		t.Errorf("attach breadcrumb = %q", crumb.Cmd)
	}
}

// `claim --run` already did it, so saying it again would be advice to repeat a
// call that has already succeeded.
func TestClaimingIntoARunSuggestsNoAttach(t *testing.T) {
	a := &api.Artifact{Slug: "art_2e1d", Filename: "shot.png", URL: "https://cdn/x", Run: &api.ArtifactRun{Slug: "run_7f"}}

	if _, ok := find(crumbsOf(t, Claimed(a, JSON, false, false, time.Now())), "uploads attach"); ok {
		t.Error("an upload already in a run was told to attach it")
	}
}

// `runs start` and `runs finish` render through the same function, so the
// breadcrumbs have to follow the run's state rather than the command's name.
func TestARunSuggestsWhatIsLeftToDoWithIt(t *testing.T) {
	started := crumbsOf(t, Run(&api.Run{Slug: "run_7f", Status: "open"}, JSON, false, false))
	push, ok := find(started, "krowk push")
	if !ok || push.Cmd != "krowk push <file> --run run_7f" {
		t.Errorf("runs start push breadcrumb = %+v", started)
	}
	if finish, ok := find(started, "runs finish"); !ok || finish.Cmd != "krowk runs finish run_7f" {
		t.Errorf("runs start finish breadcrumb = %+v", started)
	}

	finished := crumbsOf(t, Run(&api.Run{Slug: "run_7f", Status: "finished"}, JSON, false, false))
	if _, ok := find(finished, "runs finish"); ok {
		t.Errorf("a closed run was told to close itself: %+v", finished)
	}
	if _, ok := find(finished, "uploads list --run run_7f"); !ok {
		t.Errorf("a closed run suggests nothing to read back: %+v", finished)
	}
}

// A person claiming an upload is in exactly the position the JSON breadcrumb
// exists for: the upload is kept, it is under no run, and the human output says
// nothing about a run it does not have. Leaving the attach to the envelope would
// have meant the interactive default never learning `uploads attach` exists.
func TestAPersonClaimingWithoutARunIsToldHowToGroupIt(t *testing.T) {
	a := &api.Artifact{Slug: "art_2e1d", Filename: "shot.png", URL: "https://cdn/x"}

	out := Claimed(a, Human, false, false, time.Now())
	if !strings.Contains(out, AttachCrumb(a).Cmd) {
		t.Errorf("a human claim does not print the attach command:\n%s", out)
	}
	// The same shape as the claim line a push prints, and the same command the
	// envelope hands an agent — not a second spelling of either.
	if !strings.Contains(out, "group it:  krowk uploads attach art_2e1d --run <run>") {
		t.Errorf("the attach line does not follow the keep-it line's shape:\n%s", out)
	}

	// `claim --run` already grouped it, and says nothing.
	grouped := Claimed(&api.Artifact{Slug: "art_2e1d", URL: "https://cdn/x", Run: &api.ArtifactRun{Slug: "run_7f"}},
		Human, false, false, time.Now())
	if strings.Contains(grouped, "uploads attach") {
		t.Errorf("an upload already in a run was told to attach it:\n%s", grouped)
	}
}

// A push of three files has three links, and the crumb that shares them may not
// name only the first. It carries the link itself rather than `open <url>`,
// which is a macOS command and on Linux is not a URL opener at all.
func TestEveryUploadGetsItsOwnShareLinkAndNoOpener(t *testing.T) {
	r := Result{Artifacts: []*api.Artifact{
		{Slug: "art_a", URL: "https://cdn/a"},
		{Slug: "art_b", URL: "https://cdn/b"},
	}}

	var shared []string
	for _, b := range crumbsOf(t, Upload(r, JSON, false, false, time.Now())) {
		if b.Action == "share" {
			shared = append(shared, b.Cmd)
		}
	}
	want := []string{"https://cdn/a", "https://cdn/b"}
	if !slices.Equal(shared, want) {
		t.Errorf("share breadcrumbs = %q, want %q", shared, want)
	}
}

// The stride an agent chose is its own. A crumb that dropped --limit would have
// it walking the listing in 50s having asked for 10, with nothing saying so.
func TestTheNextPageKeepsTheStrideItWasAskedFor(t *testing.T) {
	page := &api.Page{Artifacts: []*api.Artifact{{Slug: "art_a", URL: "https://cdn/a"}}, Next: "art_a"}

	crumb, ok := find(crumbsOf(t, List(page, Listing{Run: "run_7f", Limit: 10}, JSON, false, false, time.Now())), "uploads list")
	if !ok {
		t.Fatal("a full page suggested no next page")
	}
	if crumb.Cmd != "krowk uploads list --run run_7f --limit 10 --before art_a" {
		t.Errorf("next-page breadcrumb = %q", crumb.Cmd)
	}

	runs := &api.RunPage{Runs: []*api.Run{{Slug: "run_7f", Status: "open"}}, Next: "run_7f"}
	crumb, ok = find(crumbsOf(t, RunList(runs, Listing{Limit: 10}, JSON, false, false)), "runs list")
	if !ok {
		t.Fatal("a full page of runs suggested no next page")
	}
	if crumb.Cmd != "krowk runs list --limit 10 --before run_7f" {
		t.Errorf("next-page breadcrumb = %q", crumb.Cmd)
	}

	// An unset --limit is not invented: the registry's default is the caller's
	// default too, and naming a number here would pin a stride nobody chose.
	crumb, _ = find(crumbsOf(t, List(page, Listing{}, JSON, false, false, time.Now())), "uploads list")
	if crumb.Cmd != "krowk uploads list --before art_a" {
		t.Errorf("next-page breadcrumb = %q, want no invented --limit", crumb.Cmd)
	}
}

func crumbsOf(t *testing.T, rendered string) []Breadcrumb {
	t.Helper()
	var e struct {
		Breadcrumbs []Breadcrumb `json:"breadcrumbs"`
	}
	if err := json.Unmarshal([]byte(rendered), &e); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, rendered)
	}
	return e.Breadcrumbs
}

func find(crumbs []Breadcrumb, substring string) (Breadcrumb, bool) {
	for _, b := range crumbs {
		if strings.Contains(b.Cmd, substring) {
			return b, true
		}
	}
	return Breadcrumb{}, false
}
