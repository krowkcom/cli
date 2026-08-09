package output

import (
	"encoding/json"
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
		"runs start":   Run(&api.Run{Slug: "run_7f", Status: "running"}, JSON, false, false),
		"runs finish":  Run(&api.Run{Slug: "run_7f", Status: "finished"}, JSON, false, false),
		"runs show":    RunDetail(&api.Run{Slug: "run_7f", Status: "running"}, JSON, false, false),
		"runs list":    RunList(&api.RunPage{Runs: []*api.Run{{Slug: "run_7f"}}, Next: "run_6a"}, JSON, false, false),
		"uploads list": List(&api.Page{Artifacts: []*api.Artifact{claimed}, Next: "art_1a"}, "", JSON, false, false, now),
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
		}
	}
}

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

// --quiet is the raw record, so nothing suggestive belongs in it.
func TestQuietCarriesNoBreadcrumbs(t *testing.T) {
	out := Upload(Result{Artifacts: []*api.Artifact{{Slug: "art_2e1d", ClaimToken: "krowk_claim_2b7f"}}},
		JSON, true, false, time.Now())
	if strings.Contains(out, "breadcrumbs") {
		t.Errorf("--quiet leaked breadcrumbs:\n%s", out)
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
	a := &api.Artifact{Slug: "art_2e1d", Filename: "shot.png", URL: "https://cdn/x", Run: "run_7f"}

	if _, ok := find(crumbsOf(t, Claimed(a, JSON, false, false, time.Now())), "uploads attach"); ok {
		t.Error("an upload already in a run was told to attach it")
	}
}

// `runs start` and `runs finish` render through the same function, so the
// breadcrumbs have to follow the run's state rather than the command's name.
func TestARunSuggestsWhatIsLeftToDoWithIt(t *testing.T) {
	started := crumbsOf(t, Run(&api.Run{Slug: "run_7f", Status: "running"}, JSON, false, false))
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
