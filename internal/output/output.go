// Package output renders one result three ways: for a person, for an agent,
// and for pasting into a pull request.
package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

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

// Paste is one upload in the two forms its destinations need. There is no
// single paste-ready string: GitHub does not build preview cards for
// third-party links, so only the image embed shows the artifact there, while
// Slack renders no markdown image embeds at all and unfurls a bare URL into a
// card of its own. So the CLI carries both and says which is which.
type Paste struct {
	// Markdown embeds the image and links through to the artifact. This is
	// the form for GitHub, Linear and Notion.
	Markdown string `json:"markdown"`
	// URL is the bare link, for Slack and Basecamp, which unfurl it themselves.
	URL string `json:"url"`
}

// EmbedSurfaces and LinkSurfaces name where each form belongs, so the choice
// never has to be guessed. Exported because the MCP server says the same thing.
const (
	EmbedSurfaces = "GitHub, Linear, Notion — renders the image"
	// PlainSurfaces replaces EmbedSurfaces when there is no image to embed and
	// the markdown form is only a plain link, so the label stays honest.
	PlainSurfaces = "GitHub, Linear, Notion — plain link, no preview to embed"
	LinkSurfaces  = "Slack, Basecamp — they unfurl the link themselves"
)

// PasteFor builds both forms for one artifact.
func PasteFor(a *api.Artifact, title string) Paste {
	return Paste{Markdown: MarkdownLink(a, title), URL: a.URL}
}

// MarkdownSurfacesFor is the honest label for a paste's markdown form: it only
// promises an image where the markdown actually embeds one.
//
// An image embed links through to the card page, so it is spelled
// `[![…](file)](card)` and no longer starts with the bang. The embed is looked
// for anywhere in the string rather than at the front, so this side does not
// start calling every image a plain link the day the registry changes what it
// wraps the embed in. A label cannot counterfeit one: the escaper turns a `[`
// in a filename into `\[`.
func MarkdownSurfacesFor(p Paste) string {
	if strings.Contains(p.Markdown, "![") {
		return EmbedSurfaces
	}
	return PlainSurfaces
}

// Breadcrumb is one call left to make, spelled out well enough to run without
// consulting anything: what it would do, the command that does it with this
// result's own slugs and tokens already in it, and why it is worth doing.
//
// The three fields are a contract, so all three are always present. An agent
// reading this decides on the description and pastes the cmd — a breadcrumb
// that leaves either to be worked out is one it has to go and read the help for,
// which is the cost this exists to remove.
//
// Cmd carries real arguments, never placeholders, wherever the result knows
// them. Where it genuinely does not — the run to attach a freshly claimed upload
// to is the caller's to choose, the file a new run is to be fed is not a thing
// this side has ever seen — the value is angle-bracketed so it cannot be
// mistaken for one. An angle-bracketed word is to be substituted before the
// command is run, and never pasted into a shell as it stands: `<` and `>` are
// redirection there, so a verbatim paste runs something other than what was
// suggested, and usually writes a file named after the placeholder.
//
// The share crumb is the one whose Cmd is not a krowk command, for the reason
// given where it is built: there is no portable command for handing a link to a
// person, so it carries the link itself.
type Breadcrumb struct {
	// Action is the short verb phrase, for a menu or a log line.
	Action string `json:"action"`
	// Cmd is the whole command, ready to run once any <placeholder> in it has
	// been substituted.
	Cmd string `json:"cmd"`
	// Description says what running it achieves, and what happens if it is not
	// run when that is the point.
	Description string `json:"description"`
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
	// Read before jsonFlag gets to override it. A --format nobody has heard of is
	// a mistake whether or not something else settled the format afterwards, and
	// answering it in JSON because --json or --jq was also passed tells a caller
	// who meant `--format markdown` and typed it wrong that they got what they
	// asked for.
	switch Format(flag) {
	case Human, JSON, Markdown, URL:
		if jsonFlag {
			return JSON, nil
		}
		return Format(flag), nil
	case "":
		switch {
		case jsonFlag, !isTTY:
			return JSON, nil
		}
		return Human, nil
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
// this itself — an image embed that clicks through to the card page, anything
// else a plain link to the card — so its version is used unless a title was
// asked for, which only the caller knows.
func MarkdownLink(a *api.Artifact, title string) string {
	if title == "" && a.Markdown != "" {
		return a.Markdown
	}
	label := title
	if label == "" {
		label = a.Filename
	}
	if label == "" {
		label = a.Slug
	}
	return link(strings.HasPrefix(a.ContentType, "image/"), label, a.FileURL, a.URL)
}

// labelEscaper escapes the characters that end or nest a link label, and folds
// newlines to spaces because CommonMark link text cannot span lines. Parens
// are legal in link text, so they stay.
var labelEscaper = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`, "\n", " ", "\r", " ")

// link renders one artifact two ways, and the two URLs are not
// interchangeable. An image embed has to name the bytes — a paste destination
// renders an image only where the link resolves to image bytes, and the card
// page resolves to HTML — but the image is then wrapped in a link to the card,
// so clicking through lands on the page with the run metadata rather than on a
// bare file. Anything that cannot be embedded is a plain link to the card.
//
// fileURL empty falls back to the card: an artifact assembled on this side
// rather than read from the registry has no byte URL, and an embed pointing at
// nothing is worse than a link that works.
func link(embed bool, label, fileURL, cardURL string) string {
	// Labels are user-controlled — a title, a filename — so delimiter
	// characters must leave here escaped or the link breaks where it is pasted.
	label = labelEscaper.Replace(label)
	if embed {
		if fileURL == "" {
			fileURL = cardURL
		}
		return fmt.Sprintf("[![%s](%s)](%s)", label, fileURL, cardURL)
	}
	return fmt.Sprintf("[%s](%s)", label, cardURL)
}

// Upload renders a successful upload.
func Upload(r Result, f Format, quiet, colour bool, now time.Time) string {
	switch f {
	case Markdown:
		return markdownResult(r)
	case URL:
		return urlResult(r)
	case Human:
		// quiet reaches the human path too. It means "the record, nothing
		// suggested", and dispatching on format before reading it is how the claim
		// line went on printing under `krowk push --quiet` on a terminal.
		return humanResult(r, quiet, colour, now)
	}

	if quiet {
		return encode(r)
	}
	return encode(uploadEnvelope(r))
}

// uploadEnvelope is the JSON shape an upload and every command that renders one
// artifact share, so an agent parses one thing whichever it ran. extra carries
// the breadcrumbs only the calling command knows about — what to do after a
// claim is not something the artifact itself can say.
func uploadEnvelope(r Result, extra ...Breadcrumb) Envelope {
	return Envelope{
		OK:          true,
		Data:        r,
		Paste:       pasteForResult(r),
		Summary:     summary(r),
		Breadcrumbs: append(breadcrumbs(r), extra...),
	}
}

// pasteForResult carries both paste forms in the envelope, so an agent picks by
// destination instead of templating: markdown for GitHub, Linear and Notion,
// the bare URL for Slack and Basecamp.
func pasteForResult(r Result) *Paste {
	if len(r.Artifacts) == 0 {
		return nil
	}
	return &Paste{Markdown: markdownResult(r), URL: urlResult(r)}
}

// urlResult is the bare link per artifact, for the surfaces that unfurl a URL
// themselves and render no markdown embeds at all.
func urlResult(r Result) string {
	lines := make([]string, 0, len(r.Artifacts))
	for _, a := range r.Artifacts {
		lines = append(lines, a.URL)
	}
	return strings.Join(lines, "\n")
}

func summary(r Result) string {
	noun := "artifacts"
	if len(r.Artifacts) == 1 {
		noun = "artifact"
	}
	s := fmt.Sprintf("%d %s, %s", len(r.Artifacts), noun, HumanBytes(r.Bytes()))
	if run := summaryRun(r); run != "" {
		s += ", run " + run
	}
	return s
}

// summaryRun names the run the summary line should carry. A run this command
// opened is on the result; every other way an artifact ends up in one — a push
// given --run, an artifact read back, one just attached — records it only on the
// artifacts, and that is the fact `uploads attach` and `claim --run` exist to
// report, so the summary must not omit it while the human line prints it.
//
// Falling back needs every artifact to agree, because the line speaks for the
// whole result: naming the first one's run would be wrong for the rest.
func summaryRun(r Result) string {
	if r.Run != nil {
		return r.Run.Slug
	}
	if len(r.Artifacts) == 0 {
		return ""
	}
	shared := r.Artifacts[0].RunSlug()
	for _, a := range r.Artifacts[1:] {
		if a.RunSlug() != shared {
			return ""
		}
	}
	return shared
}

// breadcrumbs name the calls left to make. A claim token is the one that matters
// most: without spending it, an anonymous upload is gone within the day.
//
// One per artifact, because a token belongs to exactly one upload and a push of
// three files comes back with three of them. Collapsing them into a single
// "claim them" line would leave two uploads to expire.
func breadcrumbs(r Result) []Breadcrumb {
	var crumbs []Breadcrumb
	for _, a := range r.Artifacts {
		if a.ClaimToken != "" {
			crumbs = append(crumbs, ClaimCrumb(a))
		}
	}
	for _, a := range r.Artifacts {
		crumbs = append(crumbs, shareCrumb(a))
	}
	return crumbs
}

// shareCrumb hands on one artifact's link.
//
// It carries the bare URL rather than a command that opens it. `open <url>` is
// macOS's spelling and nothing else's — on Linux `open` is not a URL opener at
// all, so a breadcrumb built on it is one that fails for most of the people
// reading it — and there is no portable command to put in its place. What
// actually gets done with a link is that it is handed to a person or pasted into
// a review, so the link is what the crumb carries.
//
// One per artifact, for the same reason the claim crumbs are: a push of three
// files has three links, and naming only the first would leave two unshared.
func shareCrumb(a *api.Artifact) Breadcrumb {
	return Breadcrumb{
		Action:      "share",
		Cmd:         a.URL,
		Description: "hand this link on — it is public and needs no key to read",
	}
}

// ClaimCrumb is the command that keeps one anonymous upload, with its own token
// already in it. Exported because it is the breadcrumb the human format prints
// too, and the two must not drift into quoting different commands.
func ClaimCrumb(a *api.Artifact) Breadcrumb {
	return Breadcrumb{
		Action: "keep past expiry",
		Cmd:    fmt.Sprintf("krowk claim %s %s", a.Slug, a.ClaimToken),
		Description: "this upload is anonymous and expires within the day; " +
			"claiming it with a key keeps it and moves it into that key's workspace. " +
			"The token is shown once and spent once",
	}
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

func humanResult(r Result, quiet, colour bool, now time.Time) string {
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
	// The same run the envelope's summary names, so the two formats do not disagree
	// about which run an upload went into — a push given --run has one recorded on
	// its artifacts and none of its own.
	if run := summaryRun(r); run != "" {
		facts = append(facts, "run "+run)
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
	// The claim command, spelled out. It is the one breadcrumb worth printing to
	// a person: the token is shown exactly once, by this response, and an upload
	// whose token scrolled past is gone within the day. Everything else the
	// envelope suggests can be worked out later from the slug.
	//
	// The label is dimmed and the command is not, because the command is what
	// gets selected and pasted.
	//
	// Suppressed under --quiet, which asks for the record and nothing suggested,
	// whichever format is rendering it. The token itself is still on the artifact
	// where the caller asked for it.
	for _, a := range r.Artifacts {
		if quiet || a.ClaimToken == "" {
			continue
		}
		lines = append(lines, crumbLine("keep it", ClaimCrumb(a).Cmd, colour))
	}
	return strings.Join(lines, "\n")
}

// crumbLine is how a breadcrumb reaches a person: a dimmed label and then the
// command, undimmed, because the command is what gets selected and pasted.
// Shared so the claim line and the attach line cannot drift into two shapes.
func crumbLine(label, cmd string, colour bool) string {
	return paint(colour, dim, "  "+label+":") + "  " + cmd
}

// Listing is what scoped the page being rendered, so the command for the next
// one can be the same query with only the cursor moved.
type Listing struct {
	// Run names the run the page was scoped to, and is empty for a whole
	// workspace's listing.
	Run string
	// Limit is the caller's --limit, or zero when it did not pass one. Zero is
	// how the flag arrives unset, and is also what the registry reads as "use the
	// default", so it is the honest way to say "the caller chose nothing".
	Limit int
}

// List renders a page of artifacts.
func List(p *api.Page, l Listing, f Format, quiet, colour bool, now time.Time) string {
	switch f {
	case Markdown:
		lines := make([]string, 0, len(p.Artifacts))
		for _, a := range p.Artifacts {
			lines = append(lines, MarkdownLink(a, ""))
		}
		return strings.Join(lines, "\n")
	case URL:
		return urlResult(Result{Artifacts: p.Artifacts})
	case Human:
		return humanList(p, l, colour, now)
	}

	if quiet {
		return encode(p)
	}
	env := Envelope{OK: true, Data: p, Summary: summaryOf(len(p.Artifacts))}
	// The cursor is only worth mentioning when there is another page behind it.
	if p.Next != "" {
		env.Breadcrumbs = []Breadcrumb{{
			Action:      "next page",
			Cmd:         nextPageCmd("krowk uploads list", l, p.Next),
			Description: nextPageDescription,
		}}
	}
	return encode(env)
}

// nextPageDescription is deliberately careful about what a cursor promises. The
// registry sets `next` on a page having come back full, not on having looked
// behind it, so a listing of exactly one page's worth carries a cursor with
// nothing after it. Saying "the rows after the last one are behind that cursor"
// would have a caller expecting rows that may not exist.
const nextPageDescription = "this page came back full, so any older rows are behind that cursor"

// nextPageCmd carries whatever scoped this page into the command for the one
// after it. A run's listing paged on without --run would silently widen to the
// whole workspace, which reads as the run having produced far more than it did;
// dropping a --limit the caller chose would change the stride mid-walk, so an
// agent following these crumbs would page in 50s having asked for 10.
func nextPageCmd(cmd string, l Listing, next string) string {
	if l.Run != "" {
		cmd += " --run " + l.Run
	}
	if l.Limit != 0 {
		cmd += " --limit " + strconv.Itoa(l.Limit)
	}
	return cmd + " --before " + next
}

func humanList(p *api.Page, l Listing, colour bool, now time.Time) string {
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
		lines = append(lines, paint(colour, dim, "more: "+nextPageCmd("krowk uploads list", l, p.Next)))
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
	case URL:
		return a.URL
	}
	return Upload(result, f, quiet, colour, now)
}

// Claimed renders an artifact that has just been claimed. It is Artifact plus
// the one thing a claim leaves undone: a claimed upload belongs to a workspace
// but to no run, and `uploads attach` is the only way it ever gets one — the
// upload could not name a run when it was created, and claiming does not give it
// one. Saying so here is the difference between an agent knowing that and
// having to read the help to find out.
//
// Silent when the artifact already has a run, which is what `claim --run` does
// in one step, and under --quiet, which asks for the record and nothing
// suggested. Markdown and url are the paste forms and are untouched.
//
// A person gets the same thing, as a line under the artifact. The human output
// prints a run when there is one and simply omits the fact when there is not, so
// leaving this to the envelope would have meant the interactive default — the
// one a person actually sees — never learning that `uploads attach` exists,
// which is the whole gap this closes.
func Claimed(a *api.Artifact, f Format, quiet, colour bool, now time.Time) string {
	switch {
	// Nothing to add: the upload is already under a run, or the caller asked for
	// the bare record, or the format is a paste form carrying only a link.
	case a.RunSlug() != "", quiet, f == Markdown, f == URL:
		return Artifact(a, f, quiet, colour, now)
	case f == Human:
		return Artifact(a, f, quiet, colour, now) + "\n" +
			crumbLine("group it", AttachCrumb(a).Cmd, colour)
	}
	// Everything else goes through the envelope, spelled as the formats that opt
	// out rather than as "only JSON", so a format added later is carried by
	// default — the same way Upload treats anything it does not recognise.
	return encode(uploadEnvelope(Result{Artifacts: []*api.Artifact{a}}, AttachCrumb(a)))
}

// AttachCrumb is the command that puts a claimed upload under a run. The run is
// the one argument this side cannot fill in: the caller holding the claim token
// is the one that knows which run the upload came from, and guessing at one
// would be a command that fails.
func AttachCrumb(a *api.Artifact) Breadcrumb {
	return Breadcrumb{
		Action: "group under a run",
		Cmd:    "krowk uploads attach " + a.Slug + " --run <run>",
		Description: "a claimed upload belongs to a workspace but to no run, and a run is " +
			"where the pull request, commit and session are recorded — `krowk runs start` " +
			"opens one, and its slug goes in place of <run>",
	}
}

// pushCrumb is what a working key is for. The file is angle-bracketed because
// there is no file in hand here — `auth verify` and `auth login` never saw one —
// and a plausible-looking `krowk push screenshot.png` is worse than a
// placeholder: an agent runs it and gets a file-not-found for a file nobody ever
// mentioned. The why differs between the two commands, so it is the caller's.
func pushCrumb(why string) Breadcrumb {
	return Breadcrumb{
		Action:      "push",
		Cmd:         "krowk push <file>",
		Description: why + "; <file> is the path to upload",
	}
}

// Key renders a verified API key: which key it is, and the workspace every call
// with it lands in — the fact worth confirming before an upload. There is no
// link to a key, so markdown and url fall back to the JSON envelope.
func Key(k *api.Key, f Format, quiet, colour bool) string {
	if f != Human {
		if quiet {
			return encode(k)
		}
		return encode(Envelope{
			OK:      true,
			Data:    k,
			Summary: fmt.Sprintf("%s in %s", k.KeyID, k.Workspace),
			Breadcrumbs: []Breadcrumb{pushCrumb(
				"the key works, so an upload with it lands in that workspace and does not expire")},
		})
	}

	workspace := k.Workspace
	if k.WorkspaceName != "" {
		workspace = k.WorkspaceName + " (" + k.Workspace + ")"
	}
	lines := []string{
		fmt.Sprintf("%s key valid  %s", paint(colour, green, "✓"), k.KeyID),
		fmt.Sprintf("  %-11s %s", "workspace", workspace),
	}
	if k.Name != "" {
		lines = append(lines, fmt.Sprintf("  %-11s %s", "name", k.Name))
	}
	if k.ExpiresAt != "" {
		lines = append(lines, fmt.Sprintf("  %-11s %s", "expires", k.ExpiresAt))
	}
	return strings.Join(lines, "\n")
}

// Login is the result of storing a key: where it landed, and whether the
// registry confirmed it on the way in.
//
// Confirmed is the field worth reading. A login that could not reach the
// registry still stores the token, so "it worked" and "it is going to work" are
// different answers, and an agent capturing stdout has no other way to tell them
// apart. There is no link to a login, so markdown and url fall back to the JSON
// envelope.
type Login struct {
	Path      string `json:"path"`
	Confirmed bool   `json:"confirmed"`
	KeyID     string `json:"key_id,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	// Reason says why the registry did not confirm the key, and is empty when it
	// did.
	Reason string `json:"reason,omitempty"`
	// Shadowed says KROWK_TOKEN is set, which outranks the file this login just
	// wrote. Without it the receipt would name a workspace the next upload does not
	// land in — the one fact a login is a receipt for, wrong.
	Shadowed bool `json:"shadowed_by_env,omitempty"`
}

// StoredKey renders what `auth login` just did.
func StoredKey(l *Login, f Format, quiet, colour bool) string {
	if f != Human {
		if quiet {
			return encode(l)
		}
		summary := fmt.Sprintf("%s stored, uploads land in %s", l.KeyID, l.Workspace)
		crumb := pushCrumb("the key is stored and accepted, so nothing else is needed before uploading")
		if !l.Confirmed {
			summary = "token stored, unconfirmed — " + l.Reason
			crumb = Breadcrumb{
				Action: "verify",
				Cmd:    "krowk auth verify",
				Description: "the token was written down but the registry never confirmed it, " +
					"so whether it works is still unknown",
			}
		}
		if l.Shadowed {
			summary += " — but KROWK_TOKEN is set and outranks it"
			crumb = shadowedCrumb
		}
		return encode(Envelope{
			OK:          true,
			Data:        l,
			Summary:     summary,
			Breadcrumbs: []Breadcrumb{crumb},
		})
	}

	var lines []string
	if !l.Confirmed {
		lines = []string{
			fmt.Sprintf("%s token stored in %s", paint(colour, green, "✓"), l.Path),
			"  " + paint(colour, dim, "unconfirmed") + " — " + l.Reason +
				"; run `krowk auth verify` once the registry is reachable",
		}
	} else {
		lines = []string{
			fmt.Sprintf("%s key %s stored in %s", paint(colour, green, "✓"), l.KeyID, l.Path),
			"  uploads land in " + l.Workspace,
		}
	}
	if l.Shadowed {
		lines = append(lines, "  "+paint(colour, dim,
			"! KROWK_TOKEN is set and wins over this file, so uploads use that key instead — "+
				"unset it to use the one just stored"))
	}
	return strings.Join(lines, "\n")
}

// shadowedCrumb replaces the push a login would otherwise suggest. Pushing would
// work, and it would land somewhere other than where this login just said — so
// the next step is settling which key is actually in play.
var shadowedCrumb = Breadcrumb{
	Action: "verify",
	Cmd:    "krowk auth verify",
	Description: "KROWK_TOKEN outranks the key just stored, so this reports the one that " +
		"uploads will really use — unset KROWK_TOKEN to use the stored one instead",
}

// Authorization is what a browser login says about itself while it is still
// waiting: the code to confirm, and the page to confirm it on.
//
// It is not wrapped in an Envelope, and that is deliberate. An envelope carries
// `ok`, which is a verdict on a command that has finished — and this is written
// before the command knows whether it worked at all. An agent that read `ok:
// true` here would take a login that has not happened yet for one that has, so
// the shape is different on purpose and says what it is instead.
type Authorization struct {
	Code string `json:"code"`
	Page string `json:"page"`
	// Opened says whether a browser was asked to open the page, so a caller knows
	// whether the person in front of it has to do that part themselves.
	Opened bool `json:"opened"`
}

// Authorizing renders that notice.
//
// It goes to stderr rather than stdout, which keeps the one document on stdout
// the receipt a program parses — but it means that a login which then *fails*
// leaves stderr carrying this notice ahead of the error envelope. That is
// unavoidable for a command with something to say before it knows its outcome, so
// it is said in the same format the rest of the output is in: prose for a person,
// a JSON document for a program, which makes stderr a stream of documents whose
// last one is the outcome rather than prose with JSON stuck to it.
//
// The code is shown so it can be compared against the one on the page. That
// comparison is the whole reason a code exists — the slug collects the key and
// never appears in a browser, so what the page asks somebody to approve has to
// be identifiable as the request their own terminal made.
// It takes no quiet: --quiet drops an envelope, and there is no envelope here to
// drop. The `authorizing` key is not a wrapper around a result but the thing that
// says this is not one, so removing it would take away the only field a reader has
// to tell an interim notice from the verdict on the same stream.
func Authorizing(a Authorization, f Format, colour bool) string {
	// `f == JSON` rather than `f != Human`, matching Error: markdown and url fall
	// back to the text a person reads, so those two never end up with a document on
	// one line of a stream and coloured prose on the next.
	if f == JSON {
		// Its own line, so whatever is written to this stream next — the error
		// envelope, if the login goes on to fail — starts a document of its own.
		return encode(map[string]any{"authorizing": a}) + "\n"
	}

	head := "Open this page and confirm the code"
	if a.Opened {
		head = "Your browser is opening — confirm the code there"
	}
	return strings.Join([]string{
		head,
		"  " + paint(colour, dim, "code") + "  " + a.Code,
		"  " + paint(colour, dim, "page") + "  " + a.Page,
		paint(colour, dim, "  waiting for approval, Ctrl-C to stop"),
		"",
	}, "\n")
}

func humanArtifact(a *api.Artifact, colour bool, now time.Time) string {
	head := fmt.Sprintf("%s  %s", a.Filename, HumanBytes(a.ByteSize))
	if a.State != "" {
		head += paint(colour, dim, "  "+a.State)
	}
	lines := []string{head, "  " + a.URL}

	var facts []string
	if run := a.RunSlug(); run != "" {
		facts = append(facts, "run "+run)
	}
	if expiry := RelativeExpiry(a.ExpiresAt, now); expiry != "" {
		facts = append(facts, expiry)
	}
	if len(facts) > 0 {
		lines = append(lines, paint(colour, dim, "  "+strings.Join(facts, " · ")))
	}
	return strings.Join(lines, "\n")
}

// TakenDown is what a takedown leaves to report. The registry answers 204 and
// there is no artifact left to render — a url and markdown naming bytes that are
// gone would be a lie — so the slug and the fact are the whole result.
//
// The fact is its own field rather than a `state`, because the API's `state` says
// whether an upload landed (pending, ready) and a tombstone keeps whichever it
// had. Spelling a takedown as a state would make the two disagree.
type TakenDown struct {
	Slug      string `json:"slug"`
	TakenDown bool   `json:"taken_down"`
}

// Removed renders a completed takedown. There is no link left to paste, so
// markdown and url fall back to the JSON envelope.
func Removed(slug string, f Format, quiet, colour bool) string {
	if f != Human {
		data := TakenDown{Slug: slug, TakenDown: true}
		if quiet {
			return encode(data)
		}
		return encode(Envelope{
			OK:      true,
			Data:    data,
			Summary: slug + " taken down",
		})
	}
	return strings.Join([]string{
		fmt.Sprintf("%s taken down  %s", paint(colour, green, "✓"), slug),
		paint(colour, dim, "  the bytes are gone for good; the link now reports it was taken down"),
	}, "\n")
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
		OK:          true,
		Data:        r,
		Summary:     fmt.Sprintf("run %s is %s", r.Slug, r.Status),
		Breadcrumbs: runCrumbs(r, true),
	})
}

// StatusFinished is the registry's word for a closed run. Its statuses are open
// and finished — nothing sets "running" — and the difference decides which
// breadcrumbs a run gets, so it is named here rather than spelled out at each
// place that branches on it, where the two spellings drifted apart once already.
const StatusFinished = "finished"

// runCrumbs are the calls a run leaves to make, which depend on whether it is
// still open. `runs start` and `runs finish` render through the same function,
// so a fixed pair would have told a caller that had just closed a run to close
// it again.
//
// withPush is set by the commands that just changed a run: there the useful next
// move on an open one is to feed it. `runs show` is a read, and a caller reading
// a run back is asking what is in it, so it gets the listing instead.
func runCrumbs(r *api.Run, withPush bool) []Breadcrumb {
	if r.Status == StatusFinished {
		return []Breadcrumb{artifactsCrumb(r)}
	}
	first := artifactsCrumb(r)
	if withPush {
		first = Breadcrumb{
			Action: "attach uploads",
			Cmd:    "krowk push <file> --run " + r.Slug,
			Description: "every push naming this run is grouped under it and inherits the " +
				"metadata recorded on it; <file> is the path to upload",
		}
	}
	return []Breadcrumb{first, finishCrumb(r)}
}

// artifactsCrumb reads a run's uploads back. A run carries the metadata and
// nothing else — the registry keeps no artifacts on it — so this is the only way
// to see what it produced, open or closed. One wording, because the two this
// replaces said the same thing differently depending on which command rendered
// the run.
func artifactsCrumb(r *api.Run) Breadcrumb {
	return Breadcrumb{
		Action:      "what it made",
		Cmd:         "krowk uploads list --run " + r.Slug,
		Description: "a run holds the metadata; its artifacts and their links are listed separately",
	}
}

// finishCrumb closes a run. A run left open is not a failure — its artifacts are
// up and their links work — but nothing else closes it.
func finishCrumb(r *api.Run) Breadcrumb {
	return Breadcrumb{
		Action:      "close",
		Cmd:         "krowk runs finish " + r.Slug,
		Description: "marks the run finished; a run stays open until something says so",
	}
}

// RunList renders a page of a workspace's runs, newest first. There is no link
// to a run, so markdown and url fall back to the JSON envelope.
func RunList(p *api.RunPage, l Listing, f Format, quiet, colour bool) string {
	// A runs listing has no run to be scoped to, so only the stride travels.
	l.Run = ""
	if f != Human {
		if quiet {
			return encode(p)
		}
		noun := "runs"
		if len(p.Runs) == 1 {
			noun = "run"
		}
		env := Envelope{OK: true, Data: p, Summary: fmt.Sprintf("%d %s", len(p.Runs), noun)}
		if p.Next != "" {
			env.Breadcrumbs = []Breadcrumb{{
				Action:      "next page",
				Cmd:         nextPageCmd("krowk runs list", l, p.Next),
				Description: nextPageDescription,
			}}
		}
		return encode(env)
	}

	if len(p.Runs) == 0 {
		return paint(colour, dim, "no runs")
	}
	var slugWidth, statusWidth int
	for _, r := range p.Runs {
		slugWidth = max(slugWidth, len(r.Slug))
		statusWidth = max(statusWidth, len(r.Status))
	}

	lines := make([]string, 0, len(p.Runs)+1)
	for _, r := range p.Runs {
		line := fmt.Sprintf("%-*s  %-*s", slugWidth, r.Slug, statusWidth, r.Status)
		if label := runLabel(r); label != "" {
			line += "  " + label
		}
		lines = append(lines, strings.TrimRight(line, " "))
	}
	if p.Next != "" {
		lines = append(lines, paint(colour, dim, "more: "+nextPageCmd("krowk runs list", l, p.Next)))
	}
	return strings.Join(lines, "\n")
}

// RunDetail renders one run and everything recorded on it. Unlike Run, which
// reports what just happened to one, this is the whole record — the metadata
// included, since a run is where all of it lives and the registry keeps none on
// the artifacts themselves.
func RunDetail(r *api.Run, f Format, quiet, colour bool) string {
	if f != Human {
		if quiet {
			return encode(r)
		}
		return encode(Envelope{
			OK:          true,
			Data:        r,
			Summary:     fmt.Sprintf("run %s is %s", r.Slug, r.Status),
			Breadcrumbs: runCrumbs(r, false),
		})
	}

	lines := []string{fmt.Sprintf("%s  %s", r.Slug, paint(colour, dim, r.Status))}
	if r.StartedAt != "" {
		lines = append(lines, fmt.Sprintf("  %-13s %s", "started", r.StartedAt))
	}
	if r.FinishedAt != "" {
		lines = append(lines, fmt.Sprintf("  %-13s %s", "finished", r.FinishedAt))
	}
	// Metadata is whatever the caller chose to record, so it is printed as it
	// arrived rather than interpreted into fields this command would have to know
	// about. Sorted, so the same run always prints the same way.
	fields := runFields(r)
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		lines = append(lines, fmt.Sprintf("  %-13s %s", k, metadataValue(fields[k])))
	}
	return strings.Join(lines, "\n")
}

// metadataValue renders one recorded value for a person.
//
// The registry stores metadata verbatim from whichever client wrote it, and this
// command exists to read that back — so the value is not necessarily a string or
// a list of them, which is all krowk itself records. Printing an arbitrary value
// with fmt leaks Go's own syntax: a JSON null becomes `<nil>`, an object becomes
// `map[a:1]`. Anything that is not a scalar or a list of them goes back out as
// the JSON it arrived as.
func metadataValue(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	case bool:
		return strconv.FormatBool(value)
	case json.Number:
		// Printed exactly as it was written, digits and all.
		return value.String()
	case []any:
		// A list of plain values reads better as a line than as JSON — `references`
		// is the field this exists for. Anything deeper is not a list of values
		// but a structure, and flattening it would render [[1,2],[3]] and [1,2,3]
		// identically, so it goes out as JSON instead.
		if parts, ok := scalarList(value); ok {
			return strings.Join(parts, "; ")
		}
	}
	return encodeCompact(v)
}

// scalarList reports the elements of a list that holds only plain values, and
// whether it is one at all. An empty list is not: it would render as nothing,
// which is what a null and an empty string already render as.
func scalarList(items []any) ([]string, bool) {
	if len(items) == 0 {
		return nil, false
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		switch item.(type) {
		case string, bool, json.Number:
			parts = append(parts, metadataValue(item))
		default:
			// A null among them included: rendering it as an empty entry would
			// hide that anything was there.
			return nil, false
		}
	}
	return parts, true
}

// encodeCompact is JSON for a person to read, so `&`, `<` and `>` stay
// themselves. encoding/json escapes them by default, for embedding in HTML,
// which turns an ordinary CI URL into something that cannot be pasted.
func encodeCompact(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimRight(buf.String(), "\n")
}

// runFields is a run's metadata as plain keys and values. A run that recorded
// none, or something that is not an object, reads as having no fields rather
// than as an error: metadata is stored verbatim, so this cannot assume a shape.
func runFields(r *api.Run) map[string]any {
	var fields map[string]any
	if len(r.Metadata) > 0 {
		dec := json.NewDecoder(bytes.NewReader(r.Metadata))
		// Numbers are kept as they were written rather than decoded to float64,
		// which silently rounds anything past 2^53 — a build number, a Unix time
		// in nanoseconds, a snowflake id. Rounding here would print a different
		// number than --format=json reports for the same run, with nothing to say
		// the digits had changed.
		dec.UseNumber()
		_ = dec.Decode(&fields)
	}
	return fields
}

// runLabel is the most identifying thing a run knows about itself, for a listing
// that would otherwise be a column of opaque slugs. The metadata is where a
// run's identity lives, since the registry keeps none on the artifact — so this
// reads back the fields the CLI itself records, most specific first.
func runLabel(r *api.Run) string {
	fields := runFields(r)
	text := func(key string) string {
		s, _ := fields[key].(string)
		// One row, one run. A title is free text — `--title "$(git log -1
		// --pretty=%B)"` is an ordinary thing for an agent to do — and a newline
		// in it would split the row, reading as extra runs that do not exist.
		// Invisible characters go for the same reason: neither an escape sequence
		// nor a bidi override in a title may repaint the row.
		return oneLine(s)
	}

	// Standard key first, flat legacy key as the fallback — runs recorded
	// before the canon vocabulary still carry the old spellings, and a reader
	// serves both forever.
	first := func(keys ...string) string {
		for _, k := range keys {
			if v := text(k); v != "" {
				return v
			}
		}
		return ""
	}

	title := first("vcs.change.title", "title")
	change := first("krowk.change.url", "pull_request")
	repo := first("vcs.repository.name", "repo")
	branch := first("vcs.ref.head.name", "branch")

	var label string
	switch {
	case title != "":
		label = title
	case change != "":
		label = change
	case repo != "" && branch != "":
		label = repo + "@" + branch
	case repo != "":
		label = repo
	default:
		label = first("krowk.harness", "agent")
	}
	// Clipped once, on the way out. Clipping each field instead would let the
	// repo@branch rung join two capped values and reach twice the cap.
	return clipLabel(label)
}

// maxLabelRunes keeps a listing's last column from swallowing the terminal. A
// title long enough to hit this is a commit message, not a label.
const maxLabelRunes = 72

// oneLine folds every run of whitespace to a single space and drops the control
// characters that would otherwise move the cursor or recolour the row.
func oneLine(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case unicode.IsSpace(r):
			space = true
		case unicode.IsControl(r), reordering(r):
			// Dropped outright rather than folded to a space: an escape sequence
			// arrives as ESC plus ordinary letters, and spacing it out would leave
			// the letters behind as text.
		default:
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// reordering reports the characters that move or hide text while occupying no
// space of their own: the bidi overrides and isolates, the zero-width spaces,
// and the byte order mark.
//
// A right-to-left override in a title reverses everything drawn after it, which
// is the same "a label must not repaint the row" problem as an escape sequence
// reached a different way — and unicode.IsControl does not cover it, since these
// are format characters rather than control ones.
//
// U+200D ZERO WIDTH JOINER is deliberately kept: it is what holds a multi-part
// emoji together, so dropping it would break one glyph into several.
//
// Spelled as code points rather than literals, since written literally they
// would be invisible here too — in the one function whose job is knowing they
// exist.
func reordering(r rune) bool {
	switch {
	case r == '\u200d': // ZERO WIDTH JOINER
		return false
	case r == '\ufeff', // BYTE ORDER MARK
		r >= '\u200b' && r <= '\u200f', // zero-width spaces, LRM, RLM
		r >= '\u202a' && r <= '\u202e', // bidi embeddings and overrides
		r >= '\u2066' && r <= '\u2069': // bidi isolates
		return true
	}
	return false
}

// clipLabel truncates on runes rather than bytes, so a multi-byte character is
// never cut in half.
func clipLabel(s string) string {
	runes := []rune(s)
	if len(runes) <= maxLabelRunes {
		return s
	}
	return string(runes[:maxLabelRunes]) + "…"
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
