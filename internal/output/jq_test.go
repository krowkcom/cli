package output

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/api"
)

// filter compiles an expression a test expects to be good.
func filter(t *testing.T, expr string) *Filter {
	t.Helper()
	f, err := CompileFilter(expr, true)
	if err != nil {
		t.Fatalf("CompileFilter(%q) = %v", expr, err)
	}
	if f == nil {
		t.Fatalf("CompileFilter(%q) returned no filter", expr)
	}
	return f
}

// filtered runs an expression over a document and returns what was written.
func filtered(t *testing.T, expr, document string, tty bool) string {
	t.Helper()
	var b strings.Builder
	if _, err := filter(t, expr).Write(&b, document, tty); err != nil {
		t.Fatalf("--jq %q: %v", expr, err)
	}
	return b.String()
}

// codeOf reads the error code out of a failure, which is the part callers and
// the exit-code table both branch on.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a failure, got none")
	}
	apiErr, ok := err.(*api.Error)
	if !ok {
		t.Fatalf("failure is a %T, not an *api.Error", err)
	}
	return apiErr.Code()
}

func TestCompileFilterTellsAnUnsetFlagFromABlankOne(t *testing.T) {
	// No flag is no filter, and no mistake.
	f, err := CompileFilter("", false)
	if err != nil || f != nil {
		t.Errorf("CompileFilter(unset) = %v, %v — want no filter and no failure", f, err)
	}

	// A flag that was typed and carries nothing is a shell that expanded an
	// empty variable into it. Reading that as "no filter" would answer the whole
	// envelope to a caller who asked for one field of it — and, on a command
	// that prints no JSON at all, would skip the refusal that keeps a raw secret
	// off stdout. The empty string counts: it is the spelling a shell produces.
	for _, blank := range []string{"", " ", "\t", "\n"} {
		_, err := CompileFilter(blank, true)
		if got := codeOf(t, err); got != "bad_jq" {
			t.Errorf("CompileFilter(%q, given) = %q, want bad_jq", blank, got)
		}
	}
}

func TestCompileFilterRefusesAnExpressionThatDoesNotParse(t *testing.T) {
	// The point of compiling here rather than at the point of use: this happens
	// before the command it belongs to uploads, claims or takes anything down.
	for _, expr := range []string{".data | [", ".data |", "{"} {
		_, err := CompileFilter(expr, true)
		if got := codeOf(t, err); got != "bad_jq" {
			t.Errorf("CompileFilter(%q) = %q, want bad_jq", expr, got)
		}
	}
}

func TestAStringResultPrintsWithoutItsQuotes(t *testing.T) {
	// This is what makes `--jq '.data.artifacts[].slug'` usable in a shell: a
	// JSON-quoted slug would have to be unquoted again by whatever ran it.
	const doc = `{"data":{"artifacts":[{"slug":"art_2e1d"},{"slug":"art_9fk1"}]}}`
	got := filtered(t, ".data.artifacts[].slug", doc, false)
	if want := "art_2e1d\nart_9fk1\n"; got != want {
		t.Errorf("slugs = %q, want %q", got, want)
	}
}

func TestACompoundResultPrintsAsJSON(t *testing.T) {
	const doc = `{"data":{"artifacts":[{"slug":"art_2e1d","filename":"shot.png"}]}}`
	got := filtered(t, "[.data.artifacts[] | {slug}]", doc, false)
	if want := `[{"slug":"art_2e1d"}]` + "\n"; got != want {
		t.Errorf("reshaped = %q, want %q", got, want)
	}
}

func TestAnExpressionThatMatchesNothingWritesNothing(t *testing.T) {
	// Not an empty line, and not a failure: jq answers a selection that matched
	// nothing with silence, and a caller counting lines has to be able to count
	// zero.
	const doc = `{"data":{"artifacts":[{"slug":"art_2e1d"}]}}`
	if got := filtered(t, `.data.artifacts[] | select(.slug == "art_nope")`, doc, false); got != "" {
		t.Errorf("no match wrote %q, want nothing", got)
	}
}

func TestNumbersSurviveAFilterAsTheyWereWritten(t *testing.T) {
	// The same rule --format json follows: a build number or a nanosecond
	// timestamp past 2^53 must not come back rounded, which is what decoding it
	// into a float64 on the way through would do.
	const doc = `{"data":{"metadata":{"build":90071992547409931}}}`
	got := filtered(t, ".data.metadata.build", doc, false)
	if want := "90071992547409931\n"; got != want {
		t.Errorf("big number = %q, want %q", got, want)
	}
}

func TestAnExpressionPointedAtTheWrongShapeFails(t *testing.T) {
	const doc = `{"data":{"artifacts":[]}}`
	_, err := filter(t, ".data | .[0]").Write(&strings.Builder{}, doc, false)
	if got := codeOf(t, err); got != "jq_failed" {
		t.Errorf("shape mismatch = %q, want jq_failed", got)
	}
	// bad_jq is a typo and jq_failed is an expression that does not fit, so a
	// caller can tell "fix the expression" from "point it somewhere else".
	if !IsFilterFailure(err) {
		t.Error("a jq_failed is not recognised as a --jq failure, so report would filter it")
	}
	if !IsFilterFailure(api.Fail("bad_jq", "…")) {
		t.Error("a bad_jq is not recognised as a --jq failure")
	}
	if IsFilterFailure(api.Fail("not_found", "…")) {
		t.Error("a registry failure reads as a --jq failure, so it would never be filtered")
	}
}

func TestAFailedExpressionNeverCarriesACredential(t *testing.T) {
	// gojq puts the value into a failure in three different shapes, and only one
	// of them is the bracketed preview: a wrap error quotes it mid-sentence with
	// no brackets at all, and error/halt_error hand back whatever they were given,
	// whole. Cutting the message on shape catches one of the three, so nothing is
	// cut on shape — the credentials come out by name instead.
	const doc = `{"data":{"artifacts":[{"claim_token":"krowk_claim_s3crets3cret","key":"krowk_sk_alsos3cret"}]}}`
	for _, expr := range []string{
		".data.artifacts[0].claim_token | tonumber",
		".data.artifacts[0].claim_token | fromjson",
		".data | error",
		".data.artifacts[0] | halt_error",
		".data | .[0]",
	} {
		_, err := filter(t, expr).Write(&strings.Builder{}, doc, false)
		if err == nil {
			t.Errorf("--jq %q did not fail", expr)
			continue
		}
		message := err.(*api.Error).Fix()
		for _, secret := range []string{"s3cret", "alsos3cret"} {
			if strings.Contains(message, secret) {
				t.Errorf("--jq %q put a credential in the failure: %q", expr, message)
			}
		}
	}
}

func TestACredentialWithUnderscoresInItIsStillRedacted(t *testing.T) {
	// The prefix is the captured group, not everything up to the last underscore:
	// base64url uses "_", and so does anything minted in segments, so scanning for
	// the boundary would leave all but the last segment of the secret standing.
	for _, message := range []string{
		"got: krowk_sk_QmFzZTY0dXJsX3Rva2Vu_XY",
		"cannot index krowk_claim_aa_bb_cc_ddeeff with a number",
	} {
		got := withoutCredentials(message)
		for _, fragment := range []string{"QmFz", "aa_bb", "ddeeff"} {
			if strings.Contains(got, fragment) {
				t.Errorf("withoutCredentials(%q) = %q — the secret survived", message, got)
			}
		}
		if !strings.Contains(got, "[redacted]") {
			t.Errorf("withoutCredentials(%q) = %q — nothing was redacted", message, got)
		}
	}
}

func TestALongFailureIsClippedRatherThanCarryingTheWholeRecord(t *testing.T) {
	// `error(.)` hands the whole document back, and a run's metadata runs to
	// krowk's 16KB cap. An error envelope carrying that is the result again with
	// ok: false on it, not a diagnostic.
	doc := `{"data":{"note":"` + strings.Repeat("x", 4000) + `"}}`
	_, err := filter(t, ".data | error").Write(&strings.Builder{}, doc, false)
	if message := err.(*api.Error).Fix(); len(message) > 500 {
		t.Errorf("failure is %d bytes long, want it clipped", len(message))
	}
}

func TestHaltSaysWhyItIsRefusedRatherThanRepeatingGojq(t *testing.T) {
	// Refusing halt is deliberate — it picks the process's exit code in jq, and
	// krowk's exit codes say what happened to the artifact. gojq renders a bare
	// halt as "halt error: null", which tells a caller none of that.
	_, err := filter(t, "halt").Write(&strings.Builder{}, `{}`, false)
	message := err.(*api.Error).Fix()
	if strings.Contains(message, "null") || !strings.Contains(message, "halt") {
		t.Errorf("halt reported as %q, want it named and explained", message)
	}
}

func TestAPipedResultIsNotFoldedForATerminalItIsNotGoingTo(t *testing.T) {
	// Piped output is the machine contract, byte for byte, the same as
	// --format json.
	const doc = `{"data":{"title":"first\nsecond"}}`
	if got := filtered(t, ".data.title", doc, false); got != "first\nsecond\n" {
		t.Errorf("piped = %q, want the newline kept", got)
	}
}

func TestATerminalNeverSeesAStringThatCouldRepaintIt(t *testing.T) {
	// A string result prints raw, so the JSON encoder that would have escaped
	// this is not on the path. A title is caller-controlled and travels through
	// the registry, so an escape sequence in one reaches here.
	const doc = "{\"data\":{\"title\":\"safe\\u001b[31mred\"}}"
	got := filtered(t, ".data.title", doc, true)
	if strings.Contains(got, "\x1b") {
		t.Errorf("a terminal was handed an escape sequence: %q", got)
	}
	// Escaped, not dropped: the value is being handed over, not displayed, so it
	// has to still say what it said.
	if want := `safe\x1b[31mred`; !strings.Contains(got, want) {
		t.Errorf("the escape was rewritten rather than spelled out: %q", got)
	}

	// One value, one line: a newline would otherwise read as a second result to
	// whatever is counting them, so it is spelled rather than printed.
	multiline := filtered(t, ".data.title", `{"data":{"title":"first\nsecond"}}`, true)
	if want := `first\nsecond` + "\n"; multiline != want {
		t.Errorf("terminal escaping = %q, want %q", multiline, want)
	}
}

func TestATerminalStillSeesAValueThatIsMerelyUnusual(t *testing.T) {
	// The reason strings are escaped rather than folded. Two spaces in a filename
	// are two spaces in a filename, and a caller copying one out of the terminal
	// into a command has to get the name that exists.
	const doc = `{"data":{"filename":"  my  file .png"}}`
	if got := filtered(t, ".data.filename", doc, true); got != "  my  file .png\n" {
		t.Errorf("a harmless value was rewritten for the terminal: %q", got)
	}
}

func TestATerminalNeverSeesAnEscapeInsideAPrintedObject(t *testing.T) {
	// gojq's encoder escapes the ASCII control bytes and nothing else, so a C1
	// control or a bidi override inside an object reaches the terminal as it
	// arrived. Keys count: a hostile one repaints the row just as well.
	const doc = "{\"data\":{\"\\u001b[31mkey\":\"\\u202evalue\"}}"
	got := filtered(t, ".data", doc, true)
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\u202e") {
		t.Errorf("a printed object carried an escape to the terminal: %q", got)
	}
	// Nothing is lost doing it: the escapes are what JSON already spells those
	// characters as, so the line parses back to exactly what it held.
	if !strings.Contains(got, "key") || !strings.Contains(got, "value") {
		t.Errorf("making the line safe dropped a field: %q", got)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(got)), &back); err != nil {
		t.Errorf("the safe line is no longer JSON: %v", err)
	}
}

func TestTheEnvironmentIsNotReachableFromAnExpression(t *testing.T) {
	// A filter expression is a string that travels — into a skill file, a CI
	// job, a command one agent hands another — and KROWK_TOKEN lives in the
	// environment by krowk's own design. gojq only exposes `env` and `$ENV` to a
	// query when it is handed a loader, and CompileFilter hands it none.
	t.Setenv("KROWK_TOKEN", "krowk_sk_s3cret")
	for _, expr := range []string{"env | length", "$ENV | length"} {
		if got := filtered(t, expr, `{}`, false); got != "0\n" {
			t.Errorf("--jq %q read %s of the environment, want none", expr, strings.TrimSpace(got))
		}
	}
}

func TestFoldingForATerminalDoesNotChangeWhatTheFilterComputes(t *testing.T) {
	// One expression, several outputs, computed out of the same decoded document.
	// Making the first output safe in place would leave the second measuring the
	// escaped form: a bidi override is one rune where its JSON spelling is six,
	// so the length would read 8 rather than 3.
	const doc = "{\"data\":{\"artifacts\":[{\"filename\":\"a\\u202eb\"}]}}"
	const expr = `.data.artifacts, (.data.artifacts[0].filename | length)`

	got := filtered(t, expr, doc, true)
	if !strings.Contains(got, "\n3\n") {
		t.Errorf("the terminal copy changed what a later output was computed from: %q", got)
	}
	// And the terminal copy carries the override spelled out, which is what JSON
	// already calls that character — so the document still parses back to it.
	if !strings.Contains(got, `a\u202eb`) {
		t.Errorf("the printed object was not made safe: %q", got)
	}
}
