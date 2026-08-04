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
)

// Breadcrumb suggests the next command, the way the Basecamp CLI does.
type Breadcrumb struct {
	Action string `json:"action"`
	Cmd    string `json:"cmd"`
}

// Envelope wraps every JSON result.
type Envelope struct {
	OK          bool           `json:"ok"`
	Data        any            `json:"data,omitempty"`
	Summary     string         `json:"summary,omitempty"`
	Breadcrumbs []Breadcrumb   `json:"breadcrumbs,omitempty"`
	Error       map[string]any `json:"error,omitempty"`
}

// Result is what one upload command produced: an artifact per file, and the run
// they were grouped under when there was one.
//
// Notes carry what the caller should know that is not a failure — chiefly that a
// keyless upload had nowhere to put the run metadata it was given.
type Result struct {
	Artifacts []*api.Artifact `json:"artifacts"`
	Run       *api.Run        `json:"run,omitempty"`
	Notes     []string        `json:"notes,omitempty"`
	// Title is the label the caller asked for, kept so markdown output can use
	// it in place of a filename.
	Title string `json:"title,omitempty"`
}

// Bytes is the total across every artifact.
func (r Result) Bytes() int64 {
	var total int64
	for _, a := range r.Artifacts {
		total += a.ByteSize
	}
	return total
}

// ResolveFormat defaults to human on a terminal and JSON when piped, so an
// agent capturing stdout gets structured data without asking for it.
func ResolveFormat(flag string, jsonFlag, isTTY bool) (Format, error) {
	if jsonFlag {
		return JSON, nil
	}
	switch Format(flag) {
	case Human, JSON, Markdown:
		return Format(flag), nil
	case "":
		if isTTY {
			return Human, nil
		}
		return JSON, nil
	}
	return "", api.Fail("bad_format", "unknown --format "+flag+" (expected human, json or markdown)")
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

// RelativeExpiry turns an RFC 3339 timestamp into "expires in 24h".
func RelativeExpiry(iso string, now time.Time) string {
	if iso == "" {
		return ""
	}
	at, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		// The registry sends sub-second precision, which RFC3339Nano parses and
		// RFC3339 does not.
		if at, err = time.Parse(time.RFC3339Nano, iso); err != nil {
			return ""
		}
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

// MarkdownLink is the paste-ready link for one artifact. The registry renders
// this itself — an image embeds, anything else is a plain link — so its version
// is used unless a title was asked for, which only the caller knows.
func MarkdownLink(a *api.Artifact, title string) string {
	if title == "" || a.Markdown == "" {
		if a.Markdown != "" {
			return a.Markdown
		}
		label := a.Filename
		if label == "" {
			label = a.Slug
		}
		return link(strings.HasPrefix(a.ContentType, "image/"), label, a.URL)
	}
	return link(strings.HasPrefix(a.ContentType, "image/"), title, a.URL)
}

func link(embed bool, label, url string) string {
	if embed {
		return fmt.Sprintf("![%s](%s)", label, url)
	}
	return fmt.Sprintf("[%s](%s)", label, url)
}

// Upload renders a successful upload.
func Upload(r Result, f Format, quiet, colour bool, now time.Time) string {
	switch f {
	case Markdown:
		return markdownResult(r)
	case Human:
		return humanResult(r, colour, now)
	}

	if quiet {
		return encode(r)
	}
	return encode(Envelope{
		OK:          true,
		Data:        r,
		Summary:     summary(r),
		Breadcrumbs: breadcrumbs(r),
	})
}

func summary(r Result) string {
	noun := "artifacts"
	if len(r.Artifacts) == 1 {
		noun = "artifact"
	}
	s := fmt.Sprintf("%d %s, %s", len(r.Artifacts), noun, HumanBytes(r.Bytes()))
	if r.Run != nil {
		s += ", run " + r.Run.Slug
	}
	return s
}

// breadcrumbs name the calls left to make. A claim token is the one that matters
// most: without spending it, an anonymous upload is gone within the day.
func breadcrumbs(r Result) []Breadcrumb {
	var crumbs []Breadcrumb
	for _, a := range r.Artifacts {
		if a.ClaimToken != "" {
			crumbs = append(crumbs, Breadcrumb{
				Action: "keep past expiry",
				Cmd:    fmt.Sprintf("krowk claim %s %s", a.Slug, a.ClaimToken),
			})
		}
	}
	if len(r.Artifacts) > 0 {
		crumbs = append(crumbs, Breadcrumb{Action: "share", Cmd: "open " + r.Artifacts[0].URL})
	}
	return crumbs
}

func markdownResult(r Result) string {
	lines := make([]string, 0, len(r.Artifacts))
	title := r.Title
	// A title names one thing, so it labels a lone artifact and is left off a
	// set of them rather than repeated on every line.
	if len(r.Artifacts) > 1 {
		title = ""
	}
	for _, a := range r.Artifacts {
		lines = append(lines, MarkdownLink(a, title))
	}
	return strings.Join(lines, "\n")
}

func humanResult(r Result, colour bool, now time.Time) string {
	tick := paint(colour, green, "✓")
	total := HumanBytes(r.Bytes())

	var lines []string
	if len(r.Artifacts) == 1 {
		a := r.Artifacts[0]
		lines = append(lines,
			fmt.Sprintf("%s uploaded  %s  %s", tick, a.Filename, total),
			"  "+a.URL,
		)
	} else {
		lines = append(lines, fmt.Sprintf("%s uploaded  %d files  %s", tick, len(r.Artifacts), total))
		width := 0
		for _, a := range r.Artifacts {
			width = max(width, len(a.Filename))
		}
		for _, a := range r.Artifacts {
			lines = append(lines, fmt.Sprintf("  %-*s  %s", width, a.Filename, a.URL))
		}
	}

	// One trailing line for the facts that apply to the whole upload.
	var facts []string
	if r.Run != nil {
		facts = append(facts, "run "+r.Run.Slug)
	}
	if r.Title != "" {
		facts = append(facts, r.Title)
	}
	if len(r.Artifacts) > 0 {
		if expiry := RelativeExpiry(r.Artifacts[0].ExpiresAt, now); expiry != "" {
			facts = append(facts, expiry)
		}
	}
	if len(facts) > 0 {
		lines = append(lines, paint(colour, dim, "  "+strings.Join(facts, " · ")))
	}
	for _, note := range r.Notes {
		lines = append(lines, paint(colour, dim, "  ! "+note))
	}
	return strings.Join(lines, "\n")
}

// List renders a page of a workspace's artifacts.
func List(p *api.Page, f Format, quiet, colour bool, now time.Time) string {
	switch f {
	case Markdown:
		lines := make([]string, 0, len(p.Artifacts))
		for _, a := range p.Artifacts {
			lines = append(lines, MarkdownLink(a, ""))
		}
		return strings.Join(lines, "\n")
	case Human:
		return humanList(p, colour, now)
	}

	if quiet {
		return encode(p)
	}
	env := Envelope{OK: true, Data: p, Summary: summaryOf(len(p.Artifacts))}
	// The cursor is only worth mentioning when there is another page behind it.
	if p.Next != "" {
		env.Breadcrumbs = []Breadcrumb{
			{Action: "next page", Cmd: "krowk uploads list --before " + p.Next},
		}
	}
	return encode(env)
}

func humanList(p *api.Page, colour bool, now time.Time) string {
	if len(p.Artifacts) == 0 {
		return paint(colour, dim, "no artifacts")
	}

	// Widths come from the rows actually being printed, so a page of short names
	// does not inherit a long one's padding.
	var nameWidth, sizeWidth int
	for _, a := range p.Artifacts {
		nameWidth = max(nameWidth, len(a.Filename))
		sizeWidth = max(sizeWidth, len(HumanBytes(a.ByteSize)))
	}

	lines := make([]string, 0, len(p.Artifacts)+1)
	for _, a := range p.Artifacts {
		line := fmt.Sprintf("%-*s  %*s  %s", nameWidth, a.Filename,
			sizeWidth, HumanBytes(a.ByteSize), a.URL)
		// A pending artifact is one whose bytes never landed, which is worth
		// seeing in a listing rather than having to infer from a dead link.
		if a.State != "ready" {
			line += paint(colour, dim, "  ("+a.State+")")
		}
		if expiry := RelativeExpiry(a.ExpiresAt, now); expiry != "" {
			line += paint(colour, dim, "  "+expiry)
		}
		lines = append(lines, line)
	}
	if p.Next != "" {
		lines = append(lines, paint(colour, dim,
			"more: krowk uploads list --before "+p.Next))
	}
	return strings.Join(lines, "\n")
}

func summaryOf(n int) string {
	noun := "artifacts"
	if n == 1 {
		noun = "artifact"
	}
	return fmt.Sprintf("%d %s", n, noun)
}

// Artifact renders a single artifact that already exists, for the commands that
// read or claim one rather than upload it.
//
// The JSON envelope is deliberately the same as an upload's, so an agent parses
// one shape whichever command it ran. Only the human line differs: `uploads show`
// did not upload anything, and saying it did would be a lie.
func Artifact(a *api.Artifact, f Format, quiet, colour bool, now time.Time) string {
	result := Result{Artifacts: []*api.Artifact{a}}

	switch f {
	case Human:
		return humanArtifact(a, colour, now)
	case Markdown:
		return markdownResult(result)
	}
	return Upload(result, f, quiet, colour, now)
}

func humanArtifact(a *api.Artifact, colour bool, now time.Time) string {
	head := fmt.Sprintf("%s  %s", a.Filename, HumanBytes(a.ByteSize))
	if a.State != "" {
		head += paint(colour, dim, "  "+a.State)
	}
	lines := []string{head, "  " + a.URL}

	var facts []string
	if a.Run != "" {
		facts = append(facts, "run "+a.Run)
	}
	if expiry := RelativeExpiry(a.ExpiresAt, now); expiry != "" {
		facts = append(facts, expiry)
	}
	if len(facts) > 0 {
		lines = append(lines, paint(colour, dim, "  "+strings.Join(facts, " · ")))
	}
	return strings.Join(lines, "\n")
}

// Run renders a run without its artifacts, for `runs start` and `runs finish`.
func Run(r *api.Run, f Format, quiet, colour bool) string {
	switch f {
	case Human, Markdown:
		status := r.Status
		if r.FinishedAt != "" {
			status += " at " + r.FinishedAt
		}
		return fmt.Sprintf("%s run %s  %s", paint(colour, green, "✓"), r.Slug, status)
	}
	if quiet {
		return encode(r)
	}
	return encode(Envelope{
		OK:      true,
		Data:    r,
		Summary: fmt.Sprintf("run %s is %s", r.Slug, r.Status),
		Breadcrumbs: []Breadcrumb{
			{Action: "attach uploads", Cmd: "krowk push <file> --run " + r.Slug},
			{Action: "close", Cmd: "krowk runs finish " + r.Slug},
		},
	})
}

// Error renders a failure. Human output leads with the code and ends with the
// fix; JSON hands back the flattened body.
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
		// details is the registry's per-field validation channel, so it reads as
		// one line per field rather than as a printed map.
		if fields, ok := body[k].(map[string]any); ok && k == "details" {
			for _, field := range slices.Sorted(maps.Keys(fields)) {
				lines = append(lines, paint(colour, dim,
					fmt.Sprintf("  %s: %s", field, joinValues(fields[field]))))
			}
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

// joinValues renders one field's validation messages, which arrive as a list.
func joinValues(v any) string {
	list, ok := v.([]any)
	if !ok {
		return fmt.Sprint(v)
	}
	parts := make([]string, 0, len(list))
	for _, item := range list {
		parts = append(parts, fmt.Sprint(item))
	}
	return strings.Join(parts, "; ")
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
