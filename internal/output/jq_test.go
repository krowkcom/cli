package output

import (
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/api"
)

// filter compiles an expression a test expects to be good.
func filter(t *testing.T, expr string) *Filter {
	t.Helper()
	f, err := CompileFilter(expr)
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
	if err := filter(t, expr).Write(&b, document, tty); err != nil {
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
	f, err := CompileFilter("")
	if err != nil || f != nil {
		t.Errorf("CompileFilter(\"\") = %v, %v — want no filter and no failure", f, err)
	}

	// A blank one was typed, or a shell expanded an empty variable into it.
	// Reading it as "no filter" would answer the whole envelope to a caller who
	// asked for one field of it.
	for _, blank := range []string{" ", "\t", "\n"} {
		_, err := CompileFilter(blank)
		if got := codeOf(t, err); got != "bad_jq" {
			t.Errorf("CompileFilter(%q) = %q, want bad_jq", blank, got)
		}
	}
}

func TestCompileFilterRefusesAnExpressionThatDoesNotParse(t *testing.T) {
	// The point of compiling here rather than at the point of use: this happens
	// before the command it belongs to uploads, claims or takes anything down.
	for _, expr := range []string{".data | [", ".data |", "{"} {
		_, err := CompileFilter(expr)
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
	err := filter(t, ".data | .[0]").Write(&strings.Builder{}, doc, false)
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

func TestAFailedExpressionDoesNotQuoteTheRecordBack(t *testing.T) {
	// gojq spells a type mismatch as the type and then a preview of the value.
	// That preview is a slice of the record the command just produced, and an
	// anonymous push carries a claim token in one — a one-shot secret that must
	// not reach a log because a filter had a typo in it.
	const doc = `{"data":{"artifacts":[{"claim_token":"krowk_claim_s3cret"}]}}`
	err := filter(t, ".data | .[0]").Write(&strings.Builder{}, doc, false)
	message := err.(*api.Error).Fix()
	if strings.Contains(message, "krowk_claim") || strings.Contains(message, "claim_token") {
		t.Errorf("the failure quotes the record back: %q", message)
	}
	// The type is the part that says what to fix, so it stays.
	if !strings.Contains(message, "object") {
		t.Errorf("the failure no longer names the type it met: %q", message)
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
	if want := "saferedred"; strings.Contains(got, want) {
		t.Errorf("folding lost text rather than the escape: %q", got)
	}

	// One value, one line: a newline in a title would otherwise read as a second
	// result to whatever is counting them.
	multiline := filtered(t, ".data.title", `{"data":{"title":"first\nsecond"}}`, true)
	if want := "first second\n"; multiline != want {
		t.Errorf("terminal folding = %q, want %q", multiline, want)
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
	// Escaped rather than folded, because a key that folded into another key
	// would take a field out of the result with it.
	if !strings.Contains(got, "key") || !strings.Contains(got, "value") {
		t.Errorf("folding dropped a field: %q", got)
	}
}

func TestTwoUnprintableKeysBothSurvive(t *testing.T) {
	// The reason keys are escaped and not folded: these two fold to the same
	// text, and a result quietly losing one of them is worse than a result
	// printing both with their escapes spelled out.
	const doc = "{\"\\u001b[31mk\":1,\"\\u001b[32mk\":2}"
	got := filtered(t, ".", doc, true)
	if strings.Count(got, `:`) != 2 {
		t.Errorf("an entry was lost folding the keys: %q", got)
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
