package output

import (
	"regexp"
	"strings"
	"unicode"
)

// A fix string is written for an agent: precise, clause-heavy, and complete
// enough to act on without asking anything else. `not_found` runs three
// clauses — what happened, what to check, and the second thing to check —
// which is right for a reader that parses the whole field and wrong for a
// person glancing at a terminal after a command failed.
//
// So the human path reads the same string in a different register. Nothing is
// rewritten and nothing is invented — the envelope carries the agent's text
// unchanged, and this is the same sentence broken where it was already jointed.
// The em dash a fix string uses is that joint: what happened stands to the left
// of it and what to do about it to the right, and a person reads them as two
// short lines far more easily than as one long one.

// fixLine is one failure said the way a person needs it: what happened, what to
// do, and at most one command to run. Then and Cmd are empty where the fix
// names neither, which is common — plenty of failures are a fact rather than a
// chore.
type fixLine struct {
	Say  string
	Then string
	Cmd  string
}

func (l fixLine) empty() bool { return l.Say == "" && l.Then == "" && l.Cmd == "" }

// fixLines is a fix string as the lines a person reads.
//
// A semicolon joins two failures that happened at once — a push that failed
// with a run left open is the case the CLI builds by hand — and each half
// keeps its own line and its own command, because they are two separate things
// left to do. Everything else is one line.
func fixLines(fix string) []fixLine {
	var lines []fixLine
	for _, clause := range strings.Split(fix, "; ") {
		if line := fixLineOf(clause); !line.empty() {
			lines = append(lines, line)
		}
	}
	return lines
}

// fixLineOf breaks one clause at its em dash into what happened and what to do,
// and lifts out any command it names.
//
// A command outranks the prose around it. Where the fix names one, that command
// is the whole of what to do, and everything past the dash is the qualification
// a person can act without reading — "check KROWK_TOKEN, or run `krowk auth
// login …`" is answered by the login line. Where it names none, the text past
// the dash is the only advice there is, and dropping it would leave a person
// told what went wrong and nothing else.
func fixLineOf(clause string) fixLine {
	cmd := commandIn(clause)
	say, then, _ := strings.Cut(strings.TrimSpace(clause), " — ")
	if cmd != "" {
		// A clause that ends in the command itself — "pass at least one path:
		// `krowk push shot.png`" — would otherwise say it twice, once buried in
		// the sentence and once on the line to copy.
		say = danglingRe.ReplaceAllString(strings.TrimSuffix(strings.TrimSpace(say), "`"+cmd+"`"), "")
		then = ""
	}
	return fixLine{Say: sentence(say), Then: sentence(then), Cmd: cmd}
}

// danglingRe is what a sentence trails off into once the command it was
// introducing has been lifted out of it: a connector, or the colon or comma
// that was about to hand over to it.
var danglingRe = regexp.MustCompile(`(?:\s+(?:run|try|use|with))?\s*[,:—]?\s*$`)

// commandRe finds a krowk command a fix names, so it can be printed as
// something to copy rather than left buried mid-sentence. Only a backticked
// span that starts with the program name and a space counts: `krowk_claim_…`
// is a token being described, not a command to run.
var commandRe = regexp.MustCompile("`(krowk [^`]+)`")

func commandIn(s string) string {
	if m := commandRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// sentence makes a clause read as one: a capital at the front and a full stop
// at the end.
//
// Neither is forced where it would damage the text. A clause opening on a flag
// or a quoted name — `--jq reads the JSON`, "`frobnicate` is not a krowk
// command" — is spelled the way it is typed, and capitalising it would print a
// flag that does not exist. A clause already ending in punctuation keeps it.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if unicode.IsLower(runes[0]) {
		runes[0] = unicode.ToUpper(runes[0])
	}
	if strings.ContainsRune(".!?", runes[len(runes)-1]) {
		return string(runes)
	}
	return string(runes) + "."
}
