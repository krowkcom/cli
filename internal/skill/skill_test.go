// Package skill holds nothing but a test.
//
// skills/krowk/SKILL.md is prose that tells an agent which command to reach for,
// and prose about a command surface rots the moment the surface moves. Nobody
// notices, either: the skill is not compiled, not run by CI in any other way,
// and the agent reading it has no way to tell a renamed flag from a wrong guess
// — it just runs the command and gets exit 1.
//
// So the skill is held to the same catalog `krowk help --json` serialises, in
// the spirit of the surface snapshot next door: every `krowk …` in a code span
// or code block must name a command that exists, and every flag written beside
// one must be a flag that command takes.
package skill

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/cli"
)

// skillPath finds the skill from this file rather than from the working
// directory, so the test says the same thing wherever `go test` is run from.
func skillPath(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file, so cannot locate the skill")
	}
	return filepath.Join(filepath.Dir(self), "..", "..", "skills", "krowk", "SKILL.md")
}

// invocation is one `krowk …` as it appears in the skill, kept with the line it
// came from so a failure says where to look.
type invocation struct {
	line  int
	text  string
	words []string
}

// krowkCall matches the command word and everything after it. The leading class
// keeps `krowk-mcp` and `krowk.com` out: both are followed by something other
// than whitespace, and neither is a call to this CLI. `krowk_claim_…` stays out
// too, since an underscore is a word character and the alternation requires the
// preceding one not to be.
var krowkCall = regexp.MustCompile(`(?:^|[^\w./-])krowk[ \t]+(.*)`)

// TestEveryCommandInTheSkillExists reads the skill's code and holds it to the
// catalog, command and flag alike.
func TestEveryCommandInTheSkillExists(t *testing.T) {
	source, err := os.ReadFile(skillPath(t))
	if err != nil {
		t.Fatal(err)
	}

	surface := cli.Surface()
	global := map[string]bool{}
	for _, f := range surface.GlobalFlags {
		global[f.Name] = true
		for _, alias := range f.Aliases {
			global[alias] = true
		}
	}

	calls := invocations(string(source))
	if len(calls) < 20 {
		// A parser that quietly stops finding anything passes this test forever.
		t.Fatalf("found only %d krowk invocations in the skill, which means the "+
			"parser stopped reading rather than the skill stopped saying anything", len(calls))
	}

	for _, call := range calls {
		path, flags := split(call.words)

		if len(path) == 0 {
			// `krowk --version` and friends: global flags and no command.
			checkFlags(t, call, flags, global, nil, "krowk")
			continue
		}

		cmd, ok := surface.Find(path)
		if !ok {
			t.Errorf("SKILL.md:%d says `krowk %s`, which is not a command krowk has.\n  %s",
				call.line, strings.Join(path, " "), call.text)
			continue
		}
		// Find answers a group with the group itself, which is not invocable:
		// `krowk uploads` alone is advice to run something that fails.
		if len(cmd.Subcommands) > 0 {
			t.Errorf("SKILL.md:%d says `krowk %s`, which is a group rather than a command — "+
				"it needs one of %s after it.\n  %s",
				call.line, cmd.Name, strings.Join(names(cmd.Subcommands), ", "), call.text)
			continue
		}

		checkFlags(t, call, flags, global, cmd.Flags, "krowk "+cmd.Name)
	}
}

func checkFlags(t *testing.T, call invocation, used []string,
	global map[string]bool, own []cli.Flag, label string) {
	t.Helper()

	takes := map[string]bool{}
	for _, f := range own {
		takes[f.Name] = true
	}
	for _, name := range used {
		if global[name] || takes[name] {
			continue
		}
		t.Errorf("SKILL.md:%d writes --%s beside `%s`, which does not take it.\n  %s",
			call.line, name, label, call.text)
	}
}

func names(cmds []cli.Command) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Name)
	}
	return out
}

// split separates the command path from the flags. The path is the leading
// non-flag words, at most two deep because that is as deep as krowk goes; a
// placeholder like `<command>` is not a command name, so it ends the path
// without joining it and the flags are checked against the globals alone.
func split(words []string) (path, flags []string) {
	for _, word := range words {
		switch {
		case strings.HasPrefix(word, "--"):
			name := strings.TrimPrefix(word, "--")
			if i := strings.IndexByte(name, '='); i >= 0 {
				name = name[:i]
			}
			// Prose punctuation rides along on the last word of a sentence.
			name = strings.TrimRight(name, ".,;:)\"'`")
			if name != "" {
				flags = append(flags, name)
			}
		case len(path) < 2 && isCommandWord(word):
			path = append(path, word)
		}
	}
	return path, flags
}

// isCommandWord is true for the lowercase words krowk routes on, and false for
// everything else that can stand in the same position: filenames, slugs,
// placeholders, shell variables and quoted strings.
func isCommandWord(word string) bool {
	if word == "" {
		return false
	}
	for _, r := range word {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// invocations pulls every `krowk …` out of the skill's code — fenced blocks and
// inline spans — and ignores its prose. Prose names commands in passing and in
// the past tense; code is the part a reader copies.
func invocations(source string) []invocation {
	var found []invocation
	fenced := false
	// A shell command wrapped over several lines is still one command, so a
	// trailing backslash holds the line open. Without this the flags on every
	// continuation line go unchecked, which is most of the flags in the file.
	continued, startedAt := "", 0

	for i, raw := range strings.Split(source, "\n") {
		if strings.HasPrefix(strings.TrimSpace(raw), "```") {
			fenced = !fenced
			continued, startedAt = "", 0
			continue
		}

		line, at := raw, i+1
		if continued != "" {
			line, at = continued+" "+strings.TrimSpace(raw), startedAt
			continued = ""
		}
		if fenced && strings.HasSuffix(strings.TrimSpace(line), `\`) {
			continued = strings.TrimSuffix(strings.TrimSpace(line), `\`)
			startedAt = at
			continue
		}

		var code []string
		if fenced {
			code = []string{line}
		} else {
			code = inlineSpans(line)
		}

		for _, chunk := range code {
			// A shell line can hold more than one command, and a comment after
			// one is prose again.
			if i := strings.Index(chunk, " # "); i >= 0 {
				chunk = chunk[:i]
			}
			for _, part := range splitCommands(chunk) {
				m := krowkCall.FindStringSubmatch(part)
				if m == nil {
					continue
				}
				found = append(found, invocation{
					line:  at,
					text:  strings.TrimSpace(part),
					words: strings.Fields(m[1]),
				})
			}
		}
	}
	return found
}

// splitCommands breaks a shell line where one command ends and the next begins,
// so `a && krowk push x` does not read as arguments to `a`.
func splitCommands(line string) []string {
	parts := []string{line}
	for _, sep := range []string{"&&", "||", ";", "|"} {
		var next []string
		for _, part := range parts {
			next = append(next, strings.Split(part, sep)...)
		}
		parts = next
	}
	return parts
}

// inlineSpans returns what is between backticks on one line. A span that opens
// and does not close on the same line is not a span this test can read, so it is
// dropped rather than guessed at — and the skill is written so none exist.
func inlineSpans(line string) []string {
	var spans []string
	parts := strings.Split(line, "`")
	for i := 1; i < len(parts); i += 2 {
		spans = append(spans, parts[i])
	}
	return spans
}
