package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"time"

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

// filterDeadline bounds one filter run. See where it is used.
const filterDeadline = 30 * time.Second

// Filter is one compiled --jq expression, ready to run over a rendered result.
type Filter struct{ code *gojq.Code }

// CompileFilter reads what --jq was given. It parses and compiles before the
// command it belongs to does anything at all, because a mistyped expression is
// the caller's to fix, and learning about it after an upload has already landed
// is learning about it too late.
//
// given says whether the flag appeared on the command line, which is a different
// question from whether it carries anything. api.ParseSlug can treat "" as
// absent because --run is genuinely optional and a command without one is a
// command that means something; --jq has no such reading. A flag that was typed
// and is empty is a shell that expanded a variable into nothing, and answering
// it with the whole envelope would hand `krowk auth token --jq "$FIELD"` the
// unfiltered secret it asked one field of.
func CompileFilter(expr string, given bool) (*Filter, error) {
	if !given {
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
	case "bad_jq", "jq_failed", "jq_unsupported":
		return true
	}
	return false
}

// jqFailure is what an expression that compiled and then did not fit the result
// is reported as — indexing an object with a number, say. Its own code, because
// the fix is a different one: bad_jq is a typo, jq_failed is an expression
// pointed at the wrong shape.
//
// gojq's message is kept whole, because it is the diagnostic: which operation,
// which type, which position. What comes out of it is credentials, and only
// credentials.
//
// The message can carry the record. A type error quotes a preview of the value,
// a wrap error quotes it mid-sentence, and `error`/`halt_error` hand back
// whatever the expression gave them, untruncated. Cutting the message at some
// punctuation cannot be made to work — a preview is caller data and picks its
// own brackets and colons, and half these shapes carry no brackets at all. So
// nothing is cut on shape. The record coming back to the caller who just fetched
// it is not the problem; the two secrets inside it are. An anonymous push
// carries a claim token and a login carries an API key, and neither may reach a
// log because someone's filter had a typo in it.
func jqFailure(err error) error {
	var halt *gojq.HaltError
	if errors.As(err, &halt) {
		// gojq renders a bare `halt` as "halt error: null", which says nothing
		// about why krowk refused it. The refusal is deliberate — see where this
		// is caught — so it gets a sentence of its own.
		return api.Fail("jq_failed", "--jq: halt and halt_error are not honoured — "+
			"in jq they choose the process's exit code, and krowk's exit codes are "+
			"what happened to the artifact")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return api.Fail("jq_failed", "--jq did not finish within "+filterDeadline.String()+
			" — an expression that never ends, such as one recursing without a base case")
	}
	return api.Fail("jq_failed", "--jq: "+withoutCredentials(err.Error()))
}

// credentialInMessage matches the two secrets krowk mints, by the only shape
// they have: the prefix the registry stamps them with and the characters after
// it. They are the two values in a record that are not the caller's to see a
// second time.
var credentialInMessage = regexp.MustCompile(`(krowk_(?:sk|claim)_)[A-Za-z0-9_-]+`)

// withoutCredentials redacts those secrets out of a message, keeping the prefix
// so a reader can still tell what kind of value was there.
//
// The message is capped too. `--jq 'error(.)'` hands back the whole document,
// and a run's metadata runs to krowk's 16KB cap — an error envelope carrying
// that is not a diagnostic, it is the result again with `ok: false` on it.
func withoutCredentials(message string) string {
	// The prefix is the captured group and not everything up to the last
	// underscore: a token may carry underscores of its own — base64url does, and
	// so does anything minted in segments — and finding the boundary by scanning
	// would leave all but the last segment of the secret standing.
	message = credentialInMessage.ReplaceAllString(message, "${1}[redacted]")
	const longest = 400
	if len(message) <= longest {
		return message
	}
	// Back off over the trailing partial rune only. Testing the whole prefix
	// would walk the cut all the way to the first invalid byte anywhere in it,
	// and invalid bytes are reachable — `@base64d` decodes to arbitrary ones —
	// so a message could erase itself.
	clipped := message[:longest]
	for len(clipped) > 0 && !utf8.RuneStart(clipped[len(clipped)-1]) {
		clipped = clipped[:len(clipped)-1]
	}
	// The continuation bytes are gone; a lead byte whose tail was cut off is
	// still standing, and would print as a replacement character.
	if r, _ := utf8.DecodeLastRuneInString(clipped); r == utf8.RuneError && len(clipped) > 0 {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped + " …"
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
// The count it answers with is the values that were not null. A caller deciding
// whether the filter had anything to say needs that rather than a byte count:
// an expression written for a result, run over a failure, answers `null` — five
// bytes that mean "the thing you asked for is not in here".
func (f *Filter) Write(w io.Writer, rendered string, tty bool) (said int, err error) {
	dec := json.NewDecoder(strings.NewReader(rendered))
	dec.UseNumber()
	var input any
	if err := dec.Decode(&input); err != nil {
		// The renderer produced something that is not one JSON document, which is
		// krowk's bug rather than the caller's. Saying so beats a jq error about a
		// value the caller never wrote.
		return 0, api.Fail("jq_failed", "--jq had no JSON result to filter: "+err.Error())
	}

	// With a deadline, because an expression travels — into a skill file, a CI
	// job, a command one agent hands another — and `repeat(.)` is a plausible
	// thing to get wrong. jq would spin forever and so would this; in an
	// unattended container that is a wedged pipeline with no diagnostic. The
	// window is enormous next to the work: filtering a page of a hundred rows is
	// microseconds, so nothing legitimate comes near it.
	ctx, cancel := context.WithTimeout(context.Background(), filterDeadline)
	defer cancel()

	iter := f.code.RunWithContext(ctx, input)
	for {
		v, ok := iter.Next()
		if !ok {
			return said, nil
		}
		if err, ok := v.(error); ok {
			// gojq's halt and halt_error arrive here too, and are deliberately
			// reported as failures rather than honoured. jq lets them choose the
			// process's exit code, and krowk's exit codes are a contract a script
			// branches on — `halt_error(2)` would answer "not found" about an
			// artifact that was found. An expression does not get to say that.
			return said, jqFailure(err)
		}
		if v != nil {
			said++
		}
		if s, ok := v.(string); ok {
			if tty {
				s = terminalSafeString(s)
			}
			fmt.Fprintln(w, s)
			continue
		}
		// gojq's encoder, not the stdlib's: it spells a result the way jq spells it,
		// down to which characters it declines to escape, so a filter run here reads
		// the same as the same filter run under jq.
		//
		// Its error is always nil, and it panics instead on a type it does not know
		// — so the thing to hold is the invariant, not the return value. Everything
		// it is handed here came out of gojq's own iterator, and terminalSafe
		// rebuilds slices and objects rather than inventing types, so nothing
		// outside its set can arrive.
		b, _ := gojq.Marshal(v)
		encoded := string(b)
		if tty {
			encoded = terminalSafeJSON(encoded)
		}
		fmt.Fprintln(w, encoded)
	}
}

// terminalSafeString makes one string safe to print on a terminal without
// changing what it says.
//
// The escaped form is the JSON spelling of the same bytes: an escape sequence
// arrives as `\x1b[31m` and stays on the line as those six characters, and a
// newline arrives as `\n` rather than as a second row. A string carrying nothing
// dangerous is printed exactly as it is, which is the case that matters — a
// filename with two spaces in it is a filename with two spaces in it, and a
// caller copying one out of the terminal has to get the name that exists.
//
// This is what folding it would have cost: trimming and collapsing every
// whitespace run reads well in a table, where a value is being shown, and is
// wrong here, where a value is being handed over.
func terminalSafeString(s string) string {
	if !unprintable(s) {
		return s
	}
	return quoteInner(s)
}

// terminalSafeJSON is the same job for a compound result, done on the encoded
// form rather than on the values inside it.
//
// gojq's encoder escapes the ASCII control bytes and nothing else, so what is
// left to deal with is the multi-byte ones — a C1 control, a bidi override —
// which reach the terminal exactly as they arrived. Rewriting the strings before
// encoding would be wrong twice over: the encoder would escape the backslash of
// the escape, so the JSON would go out saying the filename is the eight
// characters `a\u202eb`, and an object's keys would have to be rewritten too and
// could collide as they were.
//
// Done here it is exact. JSON's own structure is printable ASCII, so anything
// this replaces is inside a string, and `\uXXXX` is what JSON already spells
// that character as — the document parses back to precisely what it held.
func terminalSafeJSON(encoded string) string {
	if !unprintable(encoded) {
		return encoded
	}
	var b strings.Builder
	b.Grow(len(encoded))
	for _, r := range encoded {
		if unicode.IsControl(r) || reordering(r) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// unprintable reports whether a string carries anything that would move the
// cursor, recolour the row, or reorder what is drawn after it — the control
// characters and the format characters a human row drops, the newlines and tabs
// among them, since one filter result is one line.
func unprintable(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || reordering(r) {
			return true
		}
	}
	return false
}

// quoteInner is strconv.Quote without the quotes it wraps the result in: the
// escapes are wanted, the extra pair of delimiters is not, since this is a key
// the encoder is about to quote itself.
func quoteInner(s string) string {
	quoted := strconv.Quote(s)
	return quoted[1 : len(quoted)-1]
}
