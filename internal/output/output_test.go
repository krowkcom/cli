package output

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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

func TestKeyRendersScopesAndWarnsWhenItCannotUpload(t *testing.T) {
	k := &api.Key{Valid: true, KeyID: "key_9f3c2e1d", Workspace: "acme",
		Scopes: []string{"artifacts:read"}}

	human := Key(k, Human, false, false)
	for _, want := range []string{"key_9f3c2e1d", "acme", "artifacts:read", "cannot upload"} {
		if !strings.Contains(human, want) {
			t.Errorf("human key output is missing %q:\n%s", want, human)
		}
	}

	k.Scopes = append(k.Scopes, api.ScopeWrite)
	if got := Key(k, Human, false, false); strings.Contains(got, "cannot upload") {
		t.Errorf("a write-scoped key must not warn:\n%s", got)
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
