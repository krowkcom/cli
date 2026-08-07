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
	want := "✗ artifact_too_large  (HTTP 413)\n" +
		"  got_bytes: 214958080\n" +
		"  limit_bytes: 104857600\n" +
		"  fix: re-encode below 100 MB or push frames separately"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// GitHub renders no card for a third-party link, so the image embed is the only
// form that shows the artifact there. Labels are user-controlled, so delimiter
// characters must arrive escaped or the embed breaks.
func TestPasteCarriesBothFormsAndEscapesLabels(t *testing.T) {
	a := &api.Artifact{
		Slug:        "art_2e1d",
		Filename:    "foobar.jpg",
		ContentType: "image/jpeg",
		URL:         "https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg",
	}
	url := a.URL

	for _, tc := range []struct {
		title    string
		filename string
		want     string
	}{
		{"Checkout", "foobar.jpg", "![Checkout](" + url + ")"},
		{"Checkout [v2]", "foobar.jpg", `![Checkout \[v2\]](` + url + ")"},
		{"", "frame[0].png", `![frame\[0\].png](` + url + ")"},
		{`back\slash`, "foobar.jpg", `![back\\slash](` + url + ")"},
		{"line1\nline2\r\nline3", "foobar.jpg", "![line1 line2  line3](" + url + ")"},
	} {
		a.Filename = tc.filename
		p := PasteFor(a, tc.title)
		if p.Markdown != tc.want {
			t.Errorf("PasteFor(%q/%q).Markdown = %q, want %q", tc.title, tc.filename, p.Markdown, tc.want)
		}
		// Slack renders no markdown image embeds, so it needs the plain link.
		if p.URL != a.URL {
			t.Errorf("url = %q, want the bare link", p.URL)
		}
	}

	a.Filename = "foobar.jpg"
	r := Result{Artifacts: []*api.Artifact{a}, Title: "Checkout"}
	if got := Upload(r, Markdown, false, false, time.Now()); got != "![Checkout]("+url+")" {
		t.Errorf("--format markdown = %q, want just the embed", got)
	}
	if got := Upload(r, URL, false, false, time.Now()); got != url {
		t.Errorf("--format url = %q, want just the link", got)
	}
}

// The registry renders paste-ready markdown itself; without a caller-supplied
// title, its version wins verbatim.
func TestRegistryMarkdownWinsWithoutATitle(t *testing.T) {
	a := &api.Artifact{
		Slug:        "art_2e1d",
		Filename:    "foobar.jpg",
		ContentType: "image/jpeg",
		URL:         "https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg",
		Markdown:    "![foobar.jpg](https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg)",
	}
	if got := MarkdownLink(a, ""); got != a.Markdown {
		t.Errorf("MarkdownLink = %q, want the registry's own markdown", got)
	}
	if got := MarkdownLink(a, "Checkout"); got != "![Checkout]("+a.URL+")" {
		t.Errorf("MarkdownLink with a title = %q, want the title to win", got)
	}
}

func TestJSONEnvelopeCarriesBothPasteForms(t *testing.T) {
	a := &api.Artifact{
		Slug:        "art_2e1d",
		Filename:    "foobar.jpg",
		ContentType: "image/jpeg",
		URL:         "https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg",
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

	// --quiet is the raw result, untouched; no paste block belongs there.
	if strings.Contains(Upload(r, JSON, true, false, time.Now()), `"paste"`) {
		t.Error("--quiet should stay the raw result")
	}
}

// The surfaces label must not promise an image the markdown does not carry.
func TestMarkdownSurfacesLabelIsHonest(t *testing.T) {
	image := PasteFor(&api.Artifact{
		Slug: "art_2e1d", Filename: "shot.png", ContentType: "image/png",
		URL: "https://cdn.krowk.com/a/shot.png",
	}, "")
	if got := MarkdownSurfacesFor(image); got != EmbedSurfaces {
		t.Errorf("image label = %q, want %q", got, EmbedSurfaces)
	}

	log := PasteFor(&api.Artifact{
		Slug: "art_9f3c", Filename: "build.log", ContentType: "text/plain",
		URL: "https://cdn.krowk.com/a/build.log",
	}, "")
	if !strings.HasPrefix(log.Markdown, "[") {
		t.Fatalf("markdown = %q, want a plain link when there is nothing to embed", log.Markdown)
	}
	if got := MarkdownSurfacesFor(log); got != PlainSurfaces {
		t.Errorf("plain-link label = %q, want %q", got, PlainSurfaces)
	}
}

// A claim token adopts the upload, so it stays out of everything destined for a
// pull request comment.
func TestClaimTokenIsSurfacedButNeverPasteable(t *testing.T) {
	a := &api.Artifact{
		Slug:        "art_2e1d",
		Filename:    "foobar.jpg",
		ContentType: "image/jpeg",
		URL:         "https://cdn.krowk.com/ws_9f3c/art_2e1d/foobar.jpg",
		ClaimToken:  "krowk_claim_2b7f",
	}
	r := Result{Artifacts: []*api.Artifact{a}}

	// Visible to whoever ran the push, as the breadcrumb that spends it...
	var e struct {
		Breadcrumbs []Breadcrumb `json:"breadcrumbs"`
	}
	if err := json.Unmarshal([]byte(Upload(r, JSON, false, false, time.Now())), &e); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range e.Breadcrumbs {
		if strings.Contains(b.Cmd, a.ClaimToken) {
			found = true
		}
	}
	if !found {
		t.Errorf("breadcrumbs = %+v, want the claim command carrying the token", e.Breadcrumbs)
	}

	// ...but never in either paste form.
	p := PasteFor(a, "")
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

// The run is the whole point of `uploads attach` and of `claim --run`, and both
// answer through Artifact. The human line already prints it, so the envelope an
// agent reads must not be the one place it goes missing.
func TestAttachedRunReachesTheSummaryAndTheHumanLine(t *testing.T) {
	attached := &api.Artifact{
		Slug: "art_x", State: "ready", Filename: "shot.png", ByteSize: 15,
		Run: "run_y", URL: "https://cdn.example/art_x/shot.png",
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
	loose.Run = ""
	if got := Artifact(&loose, JSON, false, false, now); strings.Contains(got, "run ") {
		t.Errorf("envelope = %s, want no run mentioned", got)
	}
}

// The summary speaks for the whole result, so a run it did not open is named only
// when every artifact is in it — a push given --run, however many files it was.
func TestSummaryNamesACallerSuppliedRunForEveryFileCount(t *testing.T) {
	one := &api.Artifact{Slug: "art_a", Filename: "a.png", ByteSize: 10, Run: "run_y"}
	two := &api.Artifact{Slug: "art_b", Filename: "b.png", ByteSize: 20, Run: "run_y"}
	elsewhere := &api.Artifact{Slug: "art_c", Filename: "c.png", ByteSize: 30, Run: "run_z"}

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
		{Slug: "art_a", Filename: "a.png", ByteSize: 10, Run: "run_y", URL: "https://cdn.example/a.png"},
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
		}}}, Human, false, false)
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
		}}}, Human, false, false)
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
