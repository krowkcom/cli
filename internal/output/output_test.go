package output

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/krowkcom/cli/internal/api"
)

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{27, "27 B"},
		{412 * 1024, "412 KB"},
		{5933, "5.8 KB"},
		{214958080, "205 MB"},
	} {
		if got := HumanBytes(tc.in); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRelativeExpiry(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want string
	}{
		{now.Add(47 * time.Hour).Format(time.RFC3339), "expires in 47h"},
		{now.Add(72 * time.Hour).Format(time.RFC3339), "expires in 3d"},
		{now.Add(-time.Second).Format(time.RFC3339), "expired"},
		{"", ""},
		{"not a timestamp", ""},
	} {
		if got := RelativeExpiry(tc.in, now); got != tc.want {
			t.Errorf("RelativeExpiry(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The human path counts days rather than hours: "24h" is exact and nobody
// talks like that, and whether an anonymous upload survives the night is the
// question a person is actually asking.
func TestFriendlyExpiry(t *testing.T) {
	now := time.Date(2026, 8, 3, 22, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want string
	}{
		{now.Add(90 * time.Second).Format(time.RFC3339), "expires in 2 minutes"},
		{now.Add(70 * time.Second).Format(time.RFC3339), "expires in 1 minute"},
		{now.Add(90 * time.Minute).Format(time.RFC3339), "expires in 2 hours"},
		// Two and a half hours from ten at night is tomorrow, and the hours are
		// not what somebody deciding whether to claim it is asking about.
		{now.Add(150 * time.Minute).Format(time.RFC3339), "expires tomorrow"},
		{now.Add(24 * time.Hour).Format(time.RFC3339), "expires tomorrow"},
		{now.Add(72 * time.Hour).Format(time.RFC3339), "expires in 3 days"},
		{now.Add(-time.Second).Format(time.RFC3339), "expired"},
		{"", ""},
		{"not a timestamp", ""},
	} {
		if got := friendlyExpiry(tc.in, now); got != tc.want {
			t.Errorf("friendlyExpiry(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveFormat(t *testing.T) {
	// Piped output defaults to JSON so an agent gets structure for free.
	if got, _ := ResolveFormat("", false, false); got != JSON {
		t.Errorf("piped default = %q, want json", got)
	}
	if got, _ := ResolveFormat("", false, true); got != Human {
		t.Errorf("tty default = %q, want human", got)
	}
	if got, _ := ResolveFormat("markdown", false, true); got != Markdown {
		t.Errorf("explicit markdown = %q", got)
	}
	if got, _ := ResolveFormat("url", false, true); got != URL {
		t.Errorf("explicit url = %q", got)
	}
	if _, err := ResolveFormat("yaml", false, true); err == nil {
		t.Error("--format yaml should be rejected")
	}
}

func TestErrorRendersLimitAndFix(t *testing.T) {
	err := &api.Error{Status: 413, Body: map[string]any{
		"error":       "artifact_too_large",
		"limit_bytes": 104857600,
		"got_bytes":   214958080,
		"fix":         "re-encode below 100 MB or push frames separately",
		"retryable":   false,
	}}

	got := Error(err, Human, false, false)
	want := "✗ Re-encode below 100 MB or push frames separately.  (HTTP 413)\n" +
		"  (artifact_too_large)\n" +
		"  got_bytes: 214958080\n" +
		"  limit_bytes: 104857600"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// A fix is written for an agent: what happened, then what to check, then the
// caveat. A person gets the first of those as a sentence and the command on a
// line to copy — and the agent still gets every clause, in the envelope.
func TestErrorSaysTheFixInAPersonsRegister(t *testing.T) {
	err := &api.Error{Status: 401, Body: map[string]any{
		"error":     "unauthorized",
		"fix":       "this endpoint needs an API key — run `krowk auth login --token krowk_sk_...`, or set KROWK_TOKEN",
		"retryable": false,
	}}

	got := Error(err, Human, false, false)
	want := "✗ This endpoint needs an API key.  (HTTP 401)\n" +
		"  (unauthorized)\n" +
		"  try:  krowk auth login --token krowk_sk_..."
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	// The envelope is the agent's, and it keeps the whole string: nothing that
	// reads JSON should have to go back to the terminal for the clauses the
	// human line left out.
	var e struct {
		Error map[string]any `json:"error"`
	}
	if jsonErr := json.Unmarshal([]byte(Error(err, JSON, false, false)), &e); jsonErr != nil {
		t.Fatal(jsonErr)
	}
	if e.Error["fix"] != err.Body["fix"] {
		t.Errorf("envelope fix = %q, want the agent's whole string", e.Error["fix"])
	}
}

// A command a fix names is worth copying whatever the sentence around it looks
// like, and a clause that ends in one must not print it twice.
func TestErrorFindsTheCommandWhereverTheFixPutsIt(t *testing.T) {
	for name, tc := range map[string]struct{ fix, want string }{
		"after a colon": {
			"pass at least one path: `krowk push screenshot.png`",
			"✗ Pass at least one path.\n  (no_file)\n  try:  krowk push screenshot.png",
		},
		"before the dash": {
			"a key has to go behind the flag: `krowk auth login --token krowk_sk_...` — " +
				"passed as a bare argument it is ignored",
			"✗ A key has to go behind the flag.\n  (no_file)\n  try:  krowk auth login --token krowk_sk_...",
		},
		// A clause opening on something typed verbatim keeps its own spelling:
		// capitalising it would print a flag or a name that does not exist.
		"opening on a quoted name": {
			"`frobnicate` is not a krowk command — run `krowk help` for the list",
			"✗ `frobnicate` is not a krowk command.\n  (no_file)\n  try:  krowk help",
		},
		// No command, so the clause past the dash is the only advice there is
		// and it stays. The one above it names a command, and the command
		// answers everything its own clause was qualifying.
		"opening on a flag": {
			"--jq reads the JSON, so it cannot be combined with --format markdown — drop one of the two",
			"✗ --jq reads the JSON, so it cannot be combined with --format markdown.\n" +
				"  (no_file)\n  Drop one of the two.",
		},
	} {
		got := Error(api.Fail("no_file", tc.fix), Human, false, false)
		if got != tc.want {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", name, got, tc.want)
		}
	}
}

// Two failures at once are joined with a semicolon — a push that failed with a
// run left open is how the CLI builds one — and they are two separate things
// left to do, so neither the sentence nor the command of the second is dropped.
func TestErrorKeepsBothHalvesOfATwoPartFailure(t *testing.T) {
	err := &api.Error{Status: 403, Body: map[string]any{
		"error": "storage_rejected_upload",
		"fix": "object storage refused the bytes — most often the file changed after it was measured; " +
			"the run is still open — close it with `krowk runs finish run_7f`",
	}}

	got := Error(err, Human, false, false)
	want := "✗ Object storage refused the bytes.  (HTTP 403)\n" +
		"  (storage_rejected_upload)\n" +
		"  Most often the file changed after it was measured.\n" +
		"  The run is still open.\n" +
		"  try:  krowk runs finish run_7f"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The forms are the registry's, and this side passes them through. Nothing here
// composes an embed: how a krowk reference looks has to be one deploy away from
// changing everywhere, including in the installs that already exist.
func TestPasteFormsComeFromTheRegistry(t *testing.T) {
	block := "[![Cart before the fix](https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg)]" +
		"(https://krowk.com/a/art_2e1d)\nCart before the fix · " +
		"[View preview ↗](https://krowk.com/a/art_2e1d)"
	a := &api.Artifact{
		Slug:        "art_2e1d",
		Filename:    "foobar.jpg",
		ContentType: "image/jpeg",
		URL:         "https://krowk.com/a/art_2e1d",
		FileURL:     "https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg",
		Paste: &api.Paste{
			Markdown:     block,
			URL:          "https://krowk.com/a/art_2e1d",
			Destinations: map[string]string{"github": "markdown", "slack": "url", "_default": "markdown"},
		},
	}

	p := PasteOf(a)
	if p.Markdown != block {
		t.Errorf("markdown = %q, want the served block verbatim", p.Markdown)
	}
	if p.URL != a.URL {
		t.Errorf("url = %q, want the card page", p.URL)
	}
	if p.Destinations["slack"] != "url" {
		t.Errorf("destinations = %v, want the served table passed through", p.Destinations)
	}

	// A title used to relabel the markdown. It no longer can: what the block
	// says is the caption recorded on the artifact, which is data rather than
	// something re-typed at render time.
	r := Result{Artifacts: []*api.Artifact{a}, Title: "Checkout"}
	if got := Upload(r, Markdown, false, false, time.Now()); got != block {
		t.Errorf("--format markdown = %q, want the served block", got)
	}
	if got := Upload(r, URL, false, false, time.Now()); got != a.URL {
		t.Errorf("--format url = %q, want just the card link", got)
	}
}

// A registry too old to compute a block still serves the single-line markdown
// it always did, and that is served too — so it is what gets pasted.
func TestOlderRegistryMarkdownIsStillWhatIsPasted(t *testing.T) {
	a := &api.Artifact{
		Slug:        "art_2e1d",
		Filename:    "foobar.jpg",
		ContentType: "image/jpeg",
		URL:         "https://krowk.com/a/art_2e1d",
		FileURL:     "https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg",
		Markdown: "[![foobar.jpg](https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg)]" +
			"(https://krowk.com/a/art_2e1d)",
	}
	if got := PasteOf(a).Markdown; got != a.Markdown {
		t.Errorf("markdown = %q, want the registry's own", got)
	}
}

// A registry that serves neither leaves nothing to paste but the link, and the
// link is what is pasted. Composing a form here is the thing being prevented.
func TestWithNothingServedThePasteIsTheLink(t *testing.T) {
	a := &api.Artifact{
		Slug: "art_2e1d", Filename: "shot.png", ContentType: "image/png",
		URL: "https://krowk.com/a/art_2e1d",
	}
	if got := PasteOf(a).Markdown; got != a.URL {
		t.Errorf("markdown = %q, want the bare link", got)
	}
}

// A block is more than one line, so two of them are separated by a blank line:
// run together, CommonMark folds the lot into one paragraph.
func TestBlocksArePastedOneAfterAnotherAsBlocks(t *testing.T) {
	block := func(n string) string {
		return "[![" + n + "](https://cdn.krowk.com/" + n + ")](https://krowk.com/a/" + n + ")\n" +
			n + " · [View preview ↗](https://krowk.com/a/" + n + ")"
	}
	r := Result{Artifacts: []*api.Artifact{
		{Slug: "art_1", URL: "https://krowk.com/a/before", Paste: &api.Paste{Markdown: block("before")}},
		{Slug: "art_2", URL: "https://krowk.com/a/after", Paste: &api.Paste{Markdown: block("after")}},
	}}
	if got := Upload(r, Markdown, false, false, time.Now()); got != block("before")+"\n\n"+block("after") {
		t.Errorf("--format markdown = %q, want the blocks separated by a blank line", got)
	}
}

func TestJSONEnvelopeCarriesBothPasteForms(t *testing.T) {
	a := &api.Artifact{
		Slug:        "art_2e1d",
		Filename:    "foobar.jpg",
		ContentType: "image/jpeg",
		URL:         "https://krowk.com/a/art_2e1d",
		FileURL:     "https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg",
	}
	a.Paste = &api.Paste{
		Markdown:     "[![foobar.jpg](" + a.FileURL + ")](" + a.URL + ")",
		URL:          a.URL,
		Destinations: map[string]string{"github": "markdown", "slack": "url", "_default": "markdown"},
	}
	r := Result{Artifacts: []*api.Artifact{a}}

	var e struct {
		Paste Paste `json:"paste"`
	}
	if err := json.Unmarshal([]byte(Upload(r, JSON, false, false, time.Now())), &e); err != nil {
		t.Fatal(err)
	}
	if e.Paste.Markdown == "" || e.Paste.URL != a.URL {
		t.Errorf("paste = %+v, want both forms so the agent can pick", e.Paste)
	}
	// And the table beside them, so an agent picks by destination rather than
	// by a rule of its own about which tool takes which form.
	if e.Paste.Destinations["slack"] != "url" {
		t.Errorf("paste.destinations = %v, want the registry's table", e.Paste.Destinations)
	}

	// --quiet is the raw result, untouched: no envelope, so nothing krowk added
	// around the record. The artifacts keep their own paste field, because that
	// is part of what the registry sent back, not something wrapped around it.
	var bare map[string]any
	if err := json.Unmarshal([]byte(Upload(r, JSON, true, false, time.Now())), &bare); err != nil {
		t.Fatal(err)
	}
	if _, wrapped := bare["paste"]; wrapped {
		t.Error("--quiet should stay the raw result")
	}
	if _, wrapped := bare["ok"]; wrapped {
		t.Error("--quiet should stay the raw result")
	}
}

// The surfaces label must not promise an image the markdown does not carry.
//
// The card page makes both halves of that harder to get right. An image embed
// is now wrapped in a link to the card, so it no longer begins with the bang
// and a check for one would call every screenshot a plain link. And a
// non-image's markdown is a link to the card rather than to the file — which is
// a better link, and still not a preview: GitHub, Linear and Notion build no
// card for a third-party URL however good its OpenGraph tags are, so what shows
// there is a blue anchor. Slack is where that link becomes a card, and Slack
// takes the url form. So the label stays "no preview to embed".
func TestMarkdownSurfacesLabelIsHonest(t *testing.T) {
	imageArtifact := &api.Artifact{
		Slug: "art_2e1d", Filename: "shot.png", ContentType: "image/png",
		URL: "https://krowk.com/a/art_2e1d", FileURL: "https://cdn.krowk.com/ws_9f3c/art_2e1d/shot.png",
		Paste: &api.Paste{
			Markdown: "[![shot.png](https://cdn.krowk.com/ws_9f3c/art_2e1d/shot.png)]" +
				"(https://krowk.com/a/art_2e1d)\nshot.png · [View preview ↗](https://krowk.com/a/art_2e1d)",
			URL: "https://krowk.com/a/art_2e1d",
		},
	}
	image := PasteOf(imageArtifact)
	if !strings.Contains(image.Markdown, "![") {
		t.Fatalf("markdown = %q, want an image embed", image.Markdown)
	}
	if got := MarkdownSurfacesFor(imageArtifact); got != EmbedSurfaces {
		t.Errorf("image label = %q, want %q", got, EmbedSurfaces)
	}

	logArtifact := &api.Artifact{
		Slug: "art_9f3c", Filename: "build.log", ContentType: "text/plain",
		URL: "https://krowk.com/a/art_9f3c", FileURL: "https://cdn.krowk.com/ws_9f3c/art_9f3c/build.log",
		Paste: &api.Paste{
			Markdown: "**build.log** · [View preview ↗](https://krowk.com/a/art_9f3c)",
			URL:      "https://krowk.com/a/art_9f3c",
		},
	}
	log := PasteOf(logArtifact)
	if strings.Contains(log.Markdown, "![") {
		t.Fatalf("markdown = %q, want a plain link when there is nothing to embed", log.Markdown)
	}
	// And that plain link points at the card, not at the bytes: a reader
	// clicking a log in a pull request should land on the page that says what
	// run produced it rather than on a raw download.
	if !strings.Contains(log.Markdown, "(https://krowk.com/a/art_9f3c)") {
		t.Errorf("markdown = %q, want it to link to the card page", log.Markdown)
	}
	if got := MarkdownSurfacesFor(logArtifact); got != PlainSurfaces {
		t.Errorf("plain-link label = %q, want %q", got, PlainSurfaces)
	}

	// The same two forms for a private artifact promise something else. The
	// image still renders — its byte URL is the capability, and an unfurl bot
	// fetching anonymously is exactly who it was drawn for — but every label
	// that named a destination's preview card has to stop, because the card is
	// served by the app to a signed-in member and answers everyone else as
	// though the slug had never been minted.
	if got := LinkSurfacesFor(imageArtifact); got != LinkSurfaces {
		t.Errorf("public link label = %q, want %q", got, LinkSurfaces)
	}

	imageArtifact.Visibility = api.VisibilityPrivate
	logArtifact.Visibility = api.VisibilityPrivate
	if got := MarkdownSurfacesFor(imageArtifact); got != PrivateEmbedSurfaces {
		t.Errorf("private image label = %q, want %q", got, PrivateEmbedSurfaces)
	}
	if got := MarkdownSurfacesFor(logArtifact); got != PrivatePlainSurfaces {
		t.Errorf("private plain-link label = %q, want %q", got, PrivatePlainSurfaces)
	}
	if got := LinkSurfacesFor(imageArtifact); got != PrivateLinkSurfaces {
		t.Errorf("private link label = %q, want %q", got, PrivateLinkSurfaces)
	}
	for _, label := range []string{PrivateEmbedSurfaces, PrivatePlainSurfaces, PrivateLinkSurfaces} {
		if strings.Contains(label, "unfurl") && !strings.Contains(label, "nothing unfurls") {
			t.Errorf("private label %q promises an unfurl", label)
		}
		for _, tool := range []string{"Slack", "Basecamp"} {
			if strings.Contains(label, tool) {
				t.Errorf("private label %q names %s, which cannot render this card", label, tool)
			}
		}
	}
}

// A claim token adopts the upload, so it stays out of everything destined for a
// pull request comment.
func TestClaimTokenIsSurfacedButNeverPasteable(t *testing.T) {
	a := &api.Artifact{
		Slug:        "art_2e1d",
		Filename:    "foobar.jpg",
		ContentType: "image/jpeg",
		URL:         "https://krowk.com/a/art_2e1d",
		FileURL:     "https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg",
		ClaimToken:  "krowk_claim_2b7f",
	}
	r := Result{Artifacts: []*api.Artifact{a}}

	// Visible to whoever ran the push, as the breadcrumb that spends it...
	crumbs := crumbsOf(t, Upload(r, JSON, false, false, time.Now()))
	if _, ok := find(crumbs, a.ClaimToken); !ok {
		t.Errorf("breadcrumbs = %+v, want the claim command carrying the token", crumbs)
	}

	// ...but never in either paste form.
	p := PasteOf(a)
	if strings.Contains(p.Markdown, "claim") || strings.Contains(p.URL, "claim") {
		t.Errorf("paste = %+v, want no claim token", p)
	}
	if got := Upload(r, Markdown, false, false, time.Now()); strings.Contains(got, "claim") {
		t.Errorf("--format markdown leaked the claim token: %s", got)
	}
	if got := Upload(r, URL, false, false, time.Now()); got != a.URL {
		t.Errorf("--format url = %q, want just the shareable link", got)
	}
}

func TestKeyRendersTheKeyAndItsWorkspace(t *testing.T) {
	k := &api.Key{KeyID: "key_9f3c2e1d", Name: "CI", Workspace: "ws_acme",
		ExpiresAt: "2026-09-01T00:00:00Z"}

	human := Key(k, Human, false, false)
	// The workspace is the fact worth confirming: it is where an upload lands.
	for _, want := range []string{"key_9f3c2e1d", "ws_acme", "CI", "2026-09-01"} {
		if !strings.Contains(human, want) {
			t.Errorf("human key output is missing %q:\n%s", want, human)
		}
	}

	// A key that never expires says nothing about expiry rather than saying none.
	if got := Key(&api.Key{KeyID: "key_9f3c2e1d"}, Human, false, false); strings.Contains(got, "expires") {
		t.Errorf("a key with no expiry must not mention one:\n%s", got)
	}

	// There is no link to a key, so url falls back to the JSON envelope.
	var e struct {
		OK   bool    `json:"ok"`
		Data api.Key `json:"data"`
	}
	if err := json.Unmarshal([]byte(Key(k, URL, false, false)), &e); err != nil {
		t.Fatal(err)
	}
	if !e.OK || e.Data.KeyID != k.KeyID {
		t.Errorf("url format = %+v, want the JSON envelope", e)
	}
}

func TestStoredKeyDistinguishesAConfirmedLoginFromAnUnconfirmedOne(t *testing.T) {
	confirmed := &Login{
		Path: "/home/x/.config/krowk/credentials.json", Confirmed: true,
		KeyID: "key_9f3c2e1d", Workspace: "ws_acme",
	}

	human := StoredKey(confirmed, Human, false, false)
	for _, want := range []string{"key_9f3c2e1d", "ws_acme"} {
		if !strings.Contains(human, want) {
			t.Errorf("human login output is missing %q:\n%s", want, human)
		}
	}
	if strings.Contains(human, "unconfirmed") {
		t.Errorf("a confirmed login must not hedge:\n%s", human)
	}

	// The unconfirmed case is the one an agent has to be able to read: the
	// command succeeded and the key is stored, but nothing has vouched for it.
	pending := &Login{
		Path: confirmed.Path, Confirmed: false, Reason: "network_unreachable",
	}
	if got := StoredKey(pending, Human, false, false); !strings.Contains(got, "unconfirmed") ||
		!strings.Contains(got, "network_unreachable") {
		t.Errorf("unconfirmed login does not say so:\n%s", got)
	}

	var e struct {
		OK      bool   `json:"ok"`
		Data    Login  `json:"data"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(StoredKey(pending, JSON, false, false)), &e); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if !e.OK {
		t.Error("storing the token succeeded, so the envelope says ok")
	}
	if e.Data.Confirmed || e.Data.Reason != "network_unreachable" {
		t.Errorf("data = %+v", e.Data)
	}
	// An unconfirmed login's next step is verifying, not pushing.
	if !strings.Contains(StoredKey(pending, JSON, false, false), "auth verify") {
		t.Error("unconfirmed login should point at `krowk auth verify`")
	}

	// --quiet drops the envelope, the way it does everywhere else.
	if got := StoredKey(confirmed, JSON, true, false); strings.Contains(got, `"ok"`) {
		t.Errorf("--quiet should drop the envelope, got %s", got)
	}
}

// The notice a browser login prints while it waits has one job: put the code and
// the page in front of a person. It is prose in every format, because it is not a
// result — the result is the receipt on stdout once somebody has approved it.
func TestAuthorizingShowsTheCodeAndThePage(t *testing.T) {
	page := "https://app.krowk.com/cli/authorizations/new?code=7K4M-2QXP"
	opened := Authorization{Code: "7K4M-2QXP", Page: page, Opened: true}

	waiting := Authorizing(opened, Human, false)
	for _, want := range []string{"7K4M-2QXP", page, "browser is opening"} {
		if !strings.Contains(waiting, want) {
			t.Errorf("the notice is missing %q:\n%s", want, waiting)
		}
	}

	// Nothing was opened, so nothing may claim it was: this is the SSH and
	// container case, where reading the URL out of the terminal is the whole flow.
	printed := Authorizing(Authorization{Code: opened.Code, Page: page}, Human, false)
	if strings.Contains(printed, "opening") {
		t.Errorf("a login that opened nothing says it did:\n%s", printed)
	}
	if !strings.Contains(printed, "Open this page") {
		t.Errorf("nothing tells the person to open it themselves:\n%s", printed)
	}

	// A program gets a document rather than prose, and one that cannot be mistaken
	// for the command's outcome: no `ok` to read a verdict off.
	machine := Authorizing(opened, JSON, false)
	if !strings.HasSuffix(machine, "\n") {
		t.Errorf("the notice does not end its own line, so the next document runs into it:\n%q", machine)
	}
	var doc struct {
		Authorizing *Authorization `json:"authorizing"`
		OK          *bool          `json:"ok"`
		Error       map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(machine), &doc); err != nil {
		t.Fatalf("the machine notice is not JSON: %v\n%s", err, machine)
	}
	if doc.Authorizing == nil || *doc.Authorizing != opened {
		t.Errorf("authorizing = %+v, want %+v", doc.Authorizing, opened)
	}
	if doc.OK != nil {
		t.Errorf("the notice carries a verdict on a login that has not happened yet:\n%s", machine)
	}
}

// The run is the whole point of `uploads attach` and of `claim --run`, and both
// answer through Artifact. The human line already prints it, so the envelope an
// agent reads must not be the one place it goes missing.
func TestAttachedRunReachesTheSummaryAndTheHumanLine(t *testing.T) {
	attached := &api.Artifact{
		Slug: "art_x", State: "ready", Filename: "shot.png", ByteSize: 15,
		Run: &api.ArtifactRun{Slug: "run_y"}, URL: "https://krowk.com/a/art_x",
	}
	now := time.Now()

	var e struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(Artifact(attached, JSON, false, false, now)), &e); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if !strings.Contains(e.Summary, "run run_y") {
		t.Errorf("summary = %q, want the run named", e.Summary)
	}
	if human := Artifact(attached, Human, false, false, now); !strings.Contains(human, "run run_y") {
		t.Errorf("human = %q, want the run named", human)
	}

	// An artifact with no run says nothing about one rather than trailing a comma.
	loose := *attached
	loose.Run = nil
	if got := Artifact(&loose, JSON, false, false, now); strings.Contains(got, "run ") {
		t.Errorf("envelope = %s, want no run mentioned", got)
	}
}

// The summary speaks for the whole result, so a run it did not open is named only
// when every artifact is in it — a push given --run, however many files it was.
func TestSummaryNamesACallerSuppliedRunForEveryFileCount(t *testing.T) {
	one := &api.Artifact{Slug: "art_a", Filename: "a.png", ByteSize: 10, Run: &api.ArtifactRun{Slug: "run_y"}}
	two := &api.Artifact{Slug: "art_b", Filename: "b.png", ByteSize: 20, Run: &api.ArtifactRun{Slug: "run_y"}}
	elsewhere := &api.Artifact{Slug: "art_c", Filename: "c.png", ByteSize: 30, Run: &api.ArtifactRun{Slug: "run_z"}}

	if got := summary(Result{Artifacts: []*api.Artifact{one}}); !strings.Contains(got, "run run_y") {
		t.Errorf("one file = %q, want the run named", got)
	}
	if got := summary(Result{Artifacts: []*api.Artifact{one, two}}); !strings.Contains(got, "run run_y") {
		t.Errorf("two files in one run = %q, want the run named", got)
	}
	// Artifacts in different runs have no single run to report, so none is claimed.
	if got := summary(Result{Artifacts: []*api.Artifact{one, elsewhere}}); strings.Contains(got, "run ") {
		t.Errorf("mixed runs = %q, want no run claimed", got)
	}
}

// Human and JSON must not disagree about which run an upload went into. A push
// given --run has the run on its artifacts and none of its own, and both formats
// read it from the same place.
func TestHumanAndJSONAgreeOnACallerSuppliedRun(t *testing.T) {
	r := Result{Artifacts: []*api.Artifact{
		{Slug: "art_a", Filename: "a.png", ByteSize: 10, Run: &api.ArtifactRun{Slug: "run_y"}, URL: "https://krowk.com/a/art_a"},
	}}
	now := time.Now()

	if human := Upload(r, Human, false, false, now); !strings.Contains(human, "run run_y") {
		t.Errorf("human = %q, want the run named", human)
	}
	var e struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal([]byte(Upload(r, JSON, false, false, now)), &e); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if !strings.Contains(e.Summary, "run run_y") {
		t.Errorf("summary = %q, want the run named", e.Summary)
	}
}

// A person who just ran a command is owed a sentence about what it did, not
// the record it produced. The record is still on the line under it.
func TestSuccessReadsAsAConfirmation(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	anon := &api.Artifact{
		Slug: "art_2e1d", Filename: "shot.png", ByteSize: 1234, State: "ready",
		URL: "https://krowk.com/a/art_2e1d", ClaimToken: "krowk_claim_2b7f",
		ExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339),
	}

	push := Upload(Result{Artifacts: []*api.Artifact{anon}}, Human, false, false, now)
	wantHead := "✓ Uploaded shot.png → https://krowk.com/a/art_2e1d\n  1.2 KB · expires tomorrow"
	if !strings.HasPrefix(push, wantHead) {
		t.Errorf("push:\n%s\nwant it to open with:\n%s", push, wantHead)
	}

	claimed := &api.Artifact{Slug: "art_2e1d", Filename: "shot.png", ByteSize: 1234,
		URL: "https://krowk.com/a/art_2e1d"}
	claim := Claimed(claimed, Human, false, false, now)
	if !strings.HasPrefix(claim, "✓ Claimed shot.png → https://krowk.com/a/art_2e1d\n  1.2 KB · kept for good") {
		t.Errorf("claim:\n%s", claim)
	}

	if got := Removed("art_2e1d", Human, false, false); !strings.HasPrefix(got, "✓ Took art_2e1d down\n") {
		t.Errorf("takedown:\n%s", got)
	}

	// `runs start` and `runs finish` render through one function, so what it
	// says has to follow the run's state rather than the command's name.
	if got := Run(&api.Run{Slug: "run_7f", Status: "open"}, Human, false, false); got != "✓ Started run run_7f" {
		t.Errorf("runs start = %q", got)
	}
	finished := Run(&api.Run{Slug: "run_7f", Status: StatusFinished,
		FinishedAt: "2026-08-03T12:00:00.123456Z"}, Human, false, false)
	if finished != "✓ Finished run run_7f" {
		t.Errorf("runs finish = %q, want no wire timestamp read at a person", finished)
	}
	// A status this build has no word for still gets a true line rather than a
	// guess at which of the two commands produced it.
	if got := Run(&api.Run{Slug: "run_7f", Status: "abandoned"}, Human, false, false); got != "✓ Run run_7f is abandoned" {
		t.Errorf("unknown status = %q", got)
	}
}

// The registry stores run metadata verbatim from whichever client wrote it, and
// `runs show` exists to read that back — so it meets values krowk itself never
// records. Rendering those with fmt would print Go's own syntax at a person.
func TestRunDetailRendersMetadataThatIsNotAString(t *testing.T) {
	run := &api.Run{
		Slug:   "run_x",
		Status: "finished",
		Metadata: json.RawMessage(`{
			"nothing": null, "flag": true, "count": 3, "ratio": 3.5,
			"nested": {"a": 1}, "refs": ["one", "two"], "holes": ["one", null],
			"empty": [], "url": {"ci": "https://ci/job?a=1&b=2"}
		}`),
	}

	got := RunDetail(run, Human, false, false)
	field := func(name string) string {
		for _, line := range strings.Split(got, "\n") {
			if fields := strings.SplitN(strings.TrimSpace(line), " ", 2); fields[0] == name {
				return strings.TrimSpace(fields[1])
			}
		}
		t.Fatalf("no %q field in:\n%s", name, got)
		return ""
	}

	for name, want := range map[string]string{
		"flag":  "true",
		"count": "3",
		"ratio": "3.5",
		// A list of plain values reads as a line; anything deeper is a structure,
		// and flattening it would render [[1,2],[3]] and [1,2,3] alike.
		"refs":   "one; two",
		"nested": `{"a":1}`,
		// A null among the values must not vanish into an empty entry, and an
		// empty list must not read the same as a null or an empty string.
		"holes": `["one",null]`,
		"empty": "[]",
		// `&` is itself here. encoding/json escapes it for embedding in HTML,
		// which would make an ordinary CI URL unpasteable.
		"url": `{"ci":"https://ci/job?a=1&b=2"}`,
	} {
		if got := field(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// A null renders as nothing rather than as the word null.
	if !strings.Contains(got, "nothing       \n") && !strings.HasSuffix(got, "nothing       ") {
		t.Errorf("a null should render as an empty value:\n%q", got)
	}
}

// A person and an agent have to be told the same number. Metadata decoded into
// float64 silently rounds past 2^53 — build numbers, Unix times in nanoseconds
// and snowflake ids all live up there — so the digits would differ between
// `--format=human` and `--format=json` with nothing to say so.
func TestALargeNumberSurvivesTheHumanRendering(t *testing.T) {
	const exact = "1754582400123456789"
	run := &api.Run{
		Slug:     "run_x",
		Status:   "open",
		Metadata: json.RawMessage(`{"build_id": ` + exact + `, "huge": 1e300}`),
	}

	human := RunDetail(run, Human, false, false)
	if !strings.Contains(human, exact) {
		t.Errorf("human output lost precision, want %s in:\n%s", exact, human)
	}
	// And a number written in exponent form stays in it, rather than expanding
	// to the 301 digits that would swallow the row.
	if !strings.Contains(human, "1e300") {
		t.Errorf("want 1e300 kept as written, got:\n%s", human)
	}

	// The JSON form carries the raw metadata, so the two must agree.
	if encoded := RunDetail(run, JSON, false, false); !strings.Contains(encoded, exact) {
		t.Errorf("json output = %s, want %s", encoded, exact)
	}
}

// One row, one run. A title is free text, and `--title "$(git log -1)"` is an
// ordinary thing for an agent to do — a newline in it would split the row and
// read as runs that do not exist.
func TestARunLabelStaysOnItsOwnRow(t *testing.T) {
	label := func(fields map[string]string) string {
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		row := RunList(&api.RunPage{Runs: []*api.Run{{
			Slug: "run_x", Status: "open", Metadata: encoded,
		}}}, Listing{}, Human, false, false)
		if strings.Count(row, "\n") != 0 {
			t.Fatalf("the label split the row:\n%s", row)
		}
		return strings.TrimPrefix(row, "run_x  open  ")
	}

	if got := label(map[string]string{"title": "Fix checkout\n\nThe button did nothing.\n"}); got != "Fix checkout The button did nothing." {
		t.Errorf("multi-line title = %q", got)
	}
	// Neither an escape sequence nor a bidi override may repaint the row. The
	// override is the subtler one: it reverses everything drawn after it, so a
	// title can make a row read as something it is not.
	if got := label(map[string]string{"title": "\x1b[31mred\x1b[0m"}); strings.ContainsRune(got, '\x1b') {
		t.Errorf("escape sequence survived: %q", got)
	}
	if got := label(map[string]string{"title": "safe ‮gnp.txt"}); strings.ContainsRune(got, '‮') {
		t.Errorf("bidi override survived: %q", got)
	}
	// A joiner is kept, or a multi-part emoji breaks into its pieces.
	if got := label(map[string]string{"title": "ship \U0001f469‍\U0001f4bb"}); !strings.Contains(got, "‍") {
		t.Errorf("zero width joiner dropped, emoji split: %q", got)
	}
}

// The cap is on the label, and the repo@branch rung joins two fields — clipping
// each of them instead of the result would let a row reach twice the cap.
func TestARunLabelIsClippedOnceWhateverItIsBuiltFrom(t *testing.T) {
	label := func(fields map[string]string) string {
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		row := RunList(&api.RunPage{Runs: []*api.Run{{
			Slug: "run_x", Status: "open", Metadata: encoded,
		}}}, Listing{}, Human, false, false)
		return strings.TrimPrefix(row, "run_x  open  ")
	}

	for name, fields := range map[string]map[string]string{
		"title": {"title": strings.Repeat("x", 300)},
		"joined": {
			"repo":   strings.Repeat("r", 100),
			"branch": strings.Repeat("b", 100),
		},
	} {
		got := label(fields)
		if n := len([]rune(got)); n > maxLabelRunes+1 {
			t.Errorf("%s label is %d runes, want at most %d plus the ellipsis", name, n, maxLabelRunes)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("%s label was not clipped: %q", name, got)
		}
	}

	// Clipped on runes, not bytes, so a multi-byte character is never cut in
	// half and the row stays valid UTF-8.
	//
	// The leading "x" is what makes this bite. Without it a byte-slice at the cap
	// would land on a character boundary anyway — the cap divides evenly by every
	// UTF-8 width — and the bug would go unnoticed. One odd byte in front puts the
	// cut in the middle of a character.
	got := label(map[string]string{"title": "x" + strings.Repeat("é", 300)})
	if !utf8.ValidString(got) {
		t.Errorf("clipping produced invalid UTF-8: %q", got)
	}
	if n := len([]rune(got)); n > maxLabelRunes+1 {
		t.Errorf("multi-byte label is %d runes, want at most %d plus the ellipsis", n, maxLabelRunes)
	}
}
