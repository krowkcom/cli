// Package output renders one result three ways: for a person, for an agent,
// and for pasting into a pull request.
package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/krowkcom/cli/internal/api"
)

// Format is the shape of a rendered result.
type Format string

const (
	Human    Format = "human"
	JSON     Format = "json"
	Markdown Format = "markdown"
	URL      Format = "url"
)

// Paste is one artifact in the two forms its destinations need. There is no
// single paste-ready string: GitHub does not build preview cards for
// third-party links, so only the image embed shows the artifact there, while
// Slack renders no markdown image embeds at all and unfurls a bare URL into a
// card of its own. So the CLI prints both and says which is which.
type Paste struct {
	// Markdown embeds the image and links through to the artifact page. This is
	// the form for GitHub, Linear and Notion.
	Markdown string `json:"markdown"`
	// URL is the bare link, for Slack and Basecamp, which unfurl it themselves.
	URL string `json:"url"`
}

// Surfaces name where each form belongs, so the choice never has to be guessed.
const (
	embedSurfaces = "GitHub, Linear, Notion — renders the image"
	linkSurfaces  = "Slack, Basecamp — they unfurl the link themselves"
)

// PasteFor builds both forms for one artifact.
func PasteFor(a *api.Artifact, title string) Paste {
	return Paste{Markdown: MarkdownLink(a, title), URL: a.URL}
}

// Breadcrumb suggests the next command, the way the Basecamp CLI does.
type Breadcrumb struct {
	Action string `json:"action"`
	Cmd    string `json:"cmd"`
}

// Envelope wraps every JSON result.
type Envelope struct {
	OK          bool           `json:"ok"`
	Data        any            `json:"data,omitempty"`
	Paste       *Paste         `json:"paste,omitempty"`
	Summary     string         `json:"summary,omitempty"`
	Breadcrumbs []Breadcrumb   `json:"breadcrumbs,omitempty"`
	Error       map[string]any `json:"error,omitempty"`
}

// ResolveFormat defaults to human on a terminal and JSON when piped, so an
// agent capturing stdout gets structured data without asking for it.
func ResolveFormat(flag string, jsonFlag, isTTY bool) (Format, error) {
	if jsonFlag {
		return JSON, nil
	}
	switch Format(flag) {
	case Human, JSON, Markdown, URL:
		return Format(flag), nil
	case "":
		if isTTY {
			return Human, nil
		}
		return JSON, nil
	}
	return "", api.Fail("bad_format", "unknown --format "+flag+" (expected human, json, markdown or url)")
}

// HumanBytes renders a byte count the way the terminal output does.
func HumanBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i > 0 && f < 10 {
		return fmt.Sprintf("%.1f %s", f, units[i])
	}
	return fmt.Sprintf("%.0f %s", f, units[i])
}

// RelativeExpiry turns an RFC 3339 timestamp into "expires in 47h".
func RelativeExpiry(iso string, now time.Time) string {
	if iso == "" {
		return ""
	}
	at, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return ""
	}
	d := at.Sub(now)
	if d <= 0 {
		return "expired"
	}
	if hours := int(d.Round(time.Hour).Hours()); hours < 48 {
		return fmt.Sprintf("expires in %dh", hours)
	}
	return fmt.Sprintf("expires in %dd", int(d.Round(24*time.Hour).Hours()/24))
}

// MarkdownLink is the paste-ready preview link.
func MarkdownLink(a *api.Artifact, title string) string {
	label := title
	if label == "" && len(a.Files) > 0 {
		label = a.Files[0].Filename
	}
	if label == "" {
		label = a.ID
	}
	if a.PreviewURL == "" {
		return fmt.Sprintf("[%s](%s)", label, a.URL)
	}
	return fmt.Sprintf("[![%s](%s)](%s)", label, a.PreviewURL, a.URL)
}

// Artifact renders a successful upload.
func Artifact(a *api.Artifact, f Format, title string, quiet, colour bool, now time.Time) string {
	paste := PasteFor(a, title)

	switch f {
	case Markdown:
		return paste.Markdown
	case URL:
		return paste.URL
	case Human:
		return humanArtifact(a, paste, title, colour, now)
	}

	if quiet {
		return encode(a)
	}
	return encode(Envelope{
		OK:          true,
		Data:        a,
		Paste:       &paste,
		Summary:     fmt.Sprintf("%d artifact(s), %s", max(len(a.Files), 1), HumanBytes(a.Bytes)),
		Breadcrumbs: []Breadcrumb{{Action: "share", Cmd: "open " + a.URL}},
	})
}

// Key renders a verified API key: who it belongs to and what it may do.
func Key(k *api.Key, f Format, colour bool) string {
	if f != Human {
		return encode(Envelope{
			OK:      true,
			Data:    k,
			Summary: fmt.Sprintf("%s in %s, scopes: %s", k.KeyID, k.Workspace, strings.Join(k.Scopes, " ")),
			Breadcrumbs: []Breadcrumb{
				{Action: "push", Cmd: "krowk push screenshot.png"},
			},
		})
	}

	lines := []string{
		fmt.Sprintf("%s key valid  %s", paint(colour, green, "✓"), k.KeyID),
		fmt.Sprintf("  %-11s %s", "workspace", k.Workspace),
		fmt.Sprintf("  %-11s %s", "scopes", strings.Join(k.Scopes, " ")),
	}
	if !k.HasScope(api.ScopeWrite) {
		lines = append(lines, "  "+paint(colour, red, "cannot upload")+
			" — this key is missing "+api.ScopeWrite)
	}
	if k.ExpiresAt != "" {
		lines = append(lines, fmt.Sprintf("  %-11s %s", "expires", k.ExpiresAt))
	}
	if k.RateLimitRemaining != "" {
		lines = append(lines, paint(colour, dim, fmt.Sprintf("  %-11s %s", "remaining", k.RateLimitRemaining)))
	}
	return strings.Join(lines, "\n")
}

func humanArtifact(a *api.Artifact, paste Paste, title string, colour bool, now time.Time) string {
	what := fmt.Sprintf("%d artifacts", len(a.Files))
	if len(a.Files) == 1 {
		what = a.Files[0].Filename
	} else if len(a.Files) == 0 {
		what = a.ID
	}

	lines := []string{
		fmt.Sprintf("%s uploaded  %s  %s", paint(colour, green, "✓"), what, HumanBytes(a.Bytes)),
	}
	if expiry := RelativeExpiry(a.ExpiresAt, now); expiry != "" {
		lines = append(lines, paint(colour, dim, "  "+expiry))
	}
	if title != "" {
		lines = append(lines, paint(colour, dim, "  "+title))
	}
	// Both paste forms, labelled, because the right one depends on where it is
	// going and neither surface renders the other's.
	lines = append(lines,
		"",
		paint(colour, dim, "  "+embedSurfaces),
		"  "+paste.Markdown,
		"",
		paint(colour, dim, "  "+linkSurfaces),
		"  "+paste.URL,
	)
	return strings.Join(lines, "\n")
}

// Error renders a failure. Human output leads with the code and ends with the
// fix; JSON hands back the registry's body untouched.
func Error(err error, f Format, quiet, colour bool) string {
	body := map[string]any{"error": "cli_error", "detail": err.Error()}

	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		body = maps.Clone(apiErr.Body)
		if apiErr.Status != 0 {
			body["status"] = apiErr.Status
		}
	}

	if f == JSON {
		if quiet {
			return encode(body)
		}
		return encode(Envelope{OK: false, Error: body})
	}

	head := fmt.Sprintf("%s %s", paint(colour, red, "✗"), body["error"])
	if status, ok := body["status"].(int); ok {
		head += paint(colour, dim, fmt.Sprintf("  (HTTP %d)", status))
	}
	lines := []string{head}

	// Sorted, so the same failure always prints the same way.
	for _, k := range slices.Sorted(maps.Keys(body)) {
		switch k {
		case "error", "fix", "retryable", "status":
			continue
		}
		lines = append(lines, paint(colour, dim, fmt.Sprintf("  %s: %v", k, body[k])))
	}
	if fix, ok := body["fix"].(string); ok && fix != "" {
		lines = append(lines, "  fix: "+fix)
	}
	if retryable, ok := body["retryable"].(bool); ok && retryable {
		lines = append(lines, paint(colour, dim, "  retryable: yes"))
	}
	return strings.Join(lines, "\n")
}

func encode(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"ok":false,"error":{"error":"encode_failed","detail":%q}}`, err.Error())
	}
	return string(b)
}

const (
	dim   = "2"
	green = "32"
	red   = "31"
)

func paint(colour bool, code, s string) string {
	if !colour {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
