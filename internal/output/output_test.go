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

func TestPasteCarriesBothForms(t *testing.T) {
	a := &api.Artifact{
		ID:         "9f3c2e1",
		URL:        "https://krowk.com/a/9f3c2e1",
		PreviewURL: "https://krowk.com/a/9f3c2e1/preview.png",
		Files:      []api.File{{Filename: "foobar.jpg"}},
	}

	// GitHub renders no card for a third-party link, so the embed is the only
	// form that shows the artifact. Labels are user-controlled, so delimiter
	// characters must arrive escaped or the embed breaks.
	for _, tc := range []struct {
		title    string
		filename string
		want     string
	}{
		{"Checkout", "foobar.jpg", "[![Checkout](https://krowk.com/a/9f3c2e1/preview.png)](https://krowk.com/a/9f3c2e1)"},
		{"Checkout [v2]", "foobar.jpg", `[![Checkout \[v2\]](https://krowk.com/a/9f3c2e1/preview.png)](https://krowk.com/a/9f3c2e1)`},
		{"", "frame[0].png", `[![frame\[0\].png](https://krowk.com/a/9f3c2e1/preview.png)](https://krowk.com/a/9f3c2e1)`},
		{`back\slash`, "foobar.jpg", `[![back\\slash](https://krowk.com/a/9f3c2e1/preview.png)](https://krowk.com/a/9f3c2e1)`},
	} {
		a.Files[0].Filename = tc.filename
		p := PasteFor(a, tc.title)
		if p.Markdown != tc.want {
			t.Errorf("PasteFor(%q/%q).Markdown = %q, want %q", tc.title, tc.filename, p.Markdown, tc.want)
		}
		// Slack renders no markdown image embeds, so it needs the plain link.
		if p.URL != a.URL {
			t.Errorf("url = %q, want the bare link", p.URL)
		}
	}

	a.Files[0].Filename = "foobar.jpg"
	p := PasteFor(a, "Checkout")

	if got := Artifact(a, Markdown, "Checkout", false, false, time.Now()); got != p.Markdown {
		t.Errorf("--format markdown = %q, want just the embed", got)
	}
	if got := Artifact(a, URL, "Checkout", false, false, time.Now()); got != a.URL {
		t.Errorf("--format url = %q, want just the link", got)
	}
}

func TestHumanOutputShowsBothFormsLabelled(t *testing.T) {
	a := &api.Artifact{
		ID:         "9f3c2e1",
		URL:        "https://krowk.com/a/9f3c2e1",
		PreviewURL: "https://krowk.com/a/9f3c2e1/preview.png",
		Bytes:      421888,
		Files:      []api.File{{Filename: "foobar.jpg", Bytes: 421888}},
	}

	got := Artifact(a, Human, "", false, false, time.Now())
	for _, want := range []string{
		"✓ uploaded  foobar.jpg  412 KB",
		embedSurfaces,
		"[![foobar.jpg](https://krowk.com/a/9f3c2e1/preview.png)](https://krowk.com/a/9f3c2e1)",
		linkSurfaces,
		"https://krowk.com/a/9f3c2e1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("human output is missing %q:\n%s", want, got)
		}
	}
}

func TestJSONCarriesBothPasteForms(t *testing.T) {
	a := &api.Artifact{
		ID:         "9f3c2e1",
		URL:        "https://krowk.com/a/9f3c2e1",
		PreviewURL: "https://krowk.com/a/9f3c2e1/preview.png",
		Files:      []api.File{{Filename: "foobar.jpg"}},
	}

	var e struct {
		Paste Paste `json:"paste"`
	}
	if err := json.Unmarshal([]byte(Artifact(a, JSON, "", false, false, time.Now())), &e); err != nil {
		t.Fatal(err)
	}
	if e.Paste.Markdown == "" || e.Paste.URL != a.URL {
		t.Errorf("paste = %+v, want both forms so the agent can pick", e.Paste)
	}

	// --quiet is the registry's own body, untouched; no paste block belongs there.
	if strings.Contains(Artifact(a, JSON, "", true, false, time.Now()), "paste") {
		t.Error("--quiet should stay the raw artifact")
	}
}

func TestMarkdownFallsBackToALinkWithoutAPreview(t *testing.T) {
	a := &api.Artifact{ID: "9f3c2e1", URL: "https://krowk.com/a/9f3c2e1"}

	if got := PasteFor(a, "Checkout").Markdown; got != "[Checkout](https://krowk.com/a/9f3c2e1)" {
		t.Errorf("markdown = %q, want a plain link when there is nothing to embed", got)
	}

	// The human label must not promise an image the markdown does not carry.
	got := Artifact(a, Human, "Checkout", false, false, time.Now())
	if strings.Contains(got, embedSurfaces) {
		t.Errorf("human output claims %q without a preview:\n%s", embedSurfaces, got)
	}
	if !strings.Contains(got, plainSurfaces) {
		t.Errorf("human output is missing %q:\n%s", plainSurfaces, got)
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
