package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/itchyny/gojq"

	"github.com/krowkcom/cli/internal/api"
)

// --jq filters a result inside krowk, with the jq engine compiled in.
//
// This is the one dependency krowk takes that buys a language rather than a
// mechanism, so the reasoning is written down here instead of being left to be
// re-argued. The alternative was to stay stdlib-only and grow micro-formats — an
// --ids-only, a --count, a --format for each field an agent turned out to want.
// That trades one dependency for an open-ended set of flags, every one of them a
// promise krowk then keeps forever, and none of them composing with the next. jq
// is already that language, agents already write it, and gojq implements it in
// pure Go — so the single static binary an agent container gets stays a single
// static binary, which was the constraint that made the question worth asking.
//
// What it does not buy is a second contract. Every command renders exactly what
// it rendered before and the filter runs over that JSON, so the envelope stays
// the thing expressions are written against, `krowk help --json` still describes
// the whole surface, and an expression that worked yesterday works today for the
// same reason the JSON did.
//
// The process environment is deliberately out of reach. jq's `env` and `$ENV`
// read it, and gojq only exposes them to a query when it is handed a loader,
// which this does not do — so they answer with nothing. KROWK_TOKEN lives in
// that environment by krowk's own design, and a filter expression is a string
// that travels: into a skill file, a CI job, a command one agent hands another.
// Reading back the result krowk just produced is what --jq is for; reading the
// environment it ran in is not.

// Filter is one compiled --jq expression, ready to run over a rendered result.
type Filter struct{ code *gojq.Code }

// CompileFilter reads what --jq was given. It parses and compiles before the
// command it belongs to does anything at all, because a mistyped expression is
// the caller's to fix, and learning about it after an upload has already landed
// is learning about it too late.
//
// An unset flag is no filter and not a mistake. A blank one is a mistake, by the
// rule api.ParseSlug applies to --run: it was typed, or a shell expanded an
// empty variable into it, and reading it as "no filter" would answer with the
// whole envelope to a caller who asked for one field of it.
func CompileFilter(expr string) (*Filter, error) {
	if expr == "" {
		return nil, nil
	}
	if strings.TrimSpace(expr) == "" {
		return nil, api.Fail("bad_jq", "a blank --jq is not an expression — pass one, "+
			"e.g. --jq '.data.artifacts[].slug'")
	}
	query, err := gojq.Parse(expr)
	if err != nil {
		return nil, badJQ(err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return nil, badJQ(err)
	}
	return &Filter{code: code}, nil
}

// badJQ is a refusal about the expression itself rather than about anything
// krowk reached, so it carries no status and classifies as a usage failure.
// gojq's own message names where it gave up, which is the useful part.
func badJQ(err error) error {
	return api.Fail("bad_jq", "--jq: "+err.Error())
}

// IsFilterFailure reports whether a failure is one --jq caused, which is the one
// kind of failure a filter may not be run over.
//
// Running it would bury the news: an expression that does not fit the shape it
// was pointed at will not fit the complaint about it either, so `--jq '.data |
// .[0]'` would answer `null` and an exit code, with nothing anywhere saying the
// expression was the problem. Reported unfiltered, it says so.
func IsFilterFailure(err error) bool {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code() {
	case "bad_jq", "jq_failed":
		return true
	}
	return false
}

// jqFailure is what an expression that compiled and then did not fit the result
// is reported as — indexing an object with a number, say. Its own code, because
// the fix is a different one: bad_jq is a typo, jq_failed is an expression
// pointed at the wrong shape.
//
// The result itself is taken back out of the message. gojq spells a type
// mismatch as the type and then a preview of the value in brackets — `expected
// an array but got: object ({"artifacts":[{"byte_size ...})` — and that preview
// is a slice of the record the command just produced. An anonymous push carries
// a claim token in that record, and a one-shot secret must not reach a log
// because someone's filter had a typo in it. The type says what to fix and
// stays; the contents say nothing to anyone and go.
func jqFailure(err error) error {
	return api.Fail("jq_failed", "--jq: "+withoutPreview(err.Error()))
}

// withoutPreview drops the bracketed value gojq appends to a type mismatch.
//
// gojq keeps the value on the error unexported, so this reads the message rather
// than the error. The shape it is cutting is `<what went wrong>: <type>
// (<preview>)`, and the cut is at the *first* bracket rather than the last: the
// preview is a JSON rendering of caller data and can hold anything, brackets and
// colons included, so anything that looks like structure inside it has to be
// treated as content. A message not ending in a bracket is not one of these and
// is left exactly as gojq wrote it, since a mangled diagnostic is worse than a
// long one.
func withoutPreview(message string) string {
	if !strings.HasSuffix(message, ")") {
		return message
	}
	bracket := strings.Index(message, " (")
	if bracket < 0 {
		return message
	}
	return message[:bracket]
}

// Write runs the filter over one rendered result and writes what comes out, one
// value per line, the way jq does.
//
// The input is the JSON the command rendered — the envelope, or the bare record
// under --quiet — because that is what a caller reading `krowk help --json`
// would write an expression against, and it keeps --jq from being a second shape
// to learn. It is decoded rather than filtered as text, with numbers kept as
// they were written: gojq carries a json.Number as a number, so a build number
// past 2^53 leaves a filter spelled the way --format json spells it.
//
// A string result prints as itself rather than as a quoted JSON string, which is
// what makes `--jq '.data.artifacts[].slug'` usable in a shell — and is also why
// a terminal has to be protected from it. Filenames and titles are caller-
// controlled and travel through the registry; printed raw, an escape sequence in
// one repaints the terminal, and the JSON encoder that would otherwise have
// escaped it is not on this path. So on a terminal every string a result carries
// is folded the way a human row's is. Piped output is the machine contract and
// goes through byte for byte, as --format json does.
func (f *Filter) Write(w io.Writer, rendered string, tty bool) error {
	dec := json.NewDecoder(strings.NewReader(rendered))
	dec.UseNumber()
	var input any
	if err := dec.Decode(&input); err != nil {
		// The renderer produced something that is not one JSON document, which is
		// krowk's bug rather than the caller's. Saying so beats a jq error about a
		// value the caller never wrote.
		return api.Fail("jq_failed", "--jq had no JSON result to filter: "+err.Error())
	}

	iter := f.code.Run(input)
	for {
		v, ok := iter.Next()
		if !ok {
			return nil
		}
		if err, ok := v.(error); ok {
			return jqFailure(err)
		}
		if s, ok := v.(string); ok {
			if tty {
				s = oneLine(s)
			}
			fmt.Fprintln(w, s)
			continue
		}
		if tty {
			v = terminalSafe(v)
		}
		// gojq's encoder, not the stdlib's: it spells a result the way jq spells it,
		// down to which characters it declines to escape, so a filter run here reads
		// the same as the same filter run under jq.
		b, err := gojq.Marshal(v)
		if err != nil {
			return api.Fail("jq_failed", "--jq produced a value that is not JSON: "+err.Error())
		}
		fmt.Fprintln(w, string(b))
	}
}

// terminalSafe folds every string inside a filter result so that nothing printed
// as part of an object or an array can move the cursor or recolour the line.
// Numbers, booleans and nulls have nothing to fold and pass through.
//
// gojq's encoder escapes the ASCII control bytes and nothing else, which is what
// leaves the work here: a C1 control or a bidi override is a multi-byte rune and
// reaches the terminal exactly as it arrived.
func terminalSafe(v any) any {
	switch val := v.(type) {
	case string:
		return oneLine(val)
	case []any:
		for i, elem := range val {
			val[i] = terminalSafe(elem)
		}
		return val
	case map[string]any:
		return terminalSafeKeys(val)
	}
	return v
}

// terminalSafeKeys rebuilds an object with printable keys, without ever dropping
// an entry. A key that would repaint the row is escaped rather than folded: two
// keys that folded to the same text would silently become one, and a field
// vanishing from a result is worse than a field printed with its escapes spelled
// out. Escaping is the JSON spelling of the same bytes, so nothing is lost, and
// a name already taken is escaped again until it is not.
func terminalSafeKeys(val map[string]any) map[string]any {
	safe := make(map[string]any, len(val))
	var escaped []string
	for k, elem := range val {
		if oneLine(k) == k {
			safe[k] = terminalSafe(elem)
			continue
		}
		escaped = append(escaped, k)
	}
	// Sorted, so an object with two unprintable keys resolves its collisions the
	// same way every time rather than by map order.
	slices.Sort(escaped)
	for _, k := range escaped {
		name := quoteInner(k)
		for {
			if _, taken := safe[name]; !taken {
				break
			}
			name = quoteInner(name)
		}
		safe[name] = terminalSafe(val[k])
	}
	return safe
}

// quoteInner is strconv.Quote without the quotes it wraps the result in: the
// escapes are wanted, the extra pair of delimiters is not, since this is a key
// the encoder is about to quote itself.
func quoteInner(s string) string {
	quoted := strconv.Quote(s)
	return quoted[1 : len(quoted)-1]
}
