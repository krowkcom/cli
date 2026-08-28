package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// runHelp invokes krowk the way a shell would, with the terminal decided by the
// caller — which is the whole of the difference between the two help formats.
func runHelp(t *testing.T, isTTY bool, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, &out, &errOut, func(string) string { return "" }, isTTY, isTTY)
	return code, out.String(), errOut.String()
}

func decodeCatalog(t *testing.T, s string) Catalog {
	t.Helper()
	var c Catalog
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		t.Fatalf("help --json is not JSON: %v\n%s", err, s)
	}
	return c
}

// The schema is what anything reading the surface parses, so its top level is
// checked field by field rather than by eye.
func TestHelpJSONCarriesTheWholeSurface(t *testing.T) {
	code, stdout, stderr := runHelp(t, true, "help", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}

	c := decodeCatalog(t, stdout)
	if c.Name != "krowk" {
		t.Errorf("name = %q", c.Name)
	}
	if c.Version != Version {
		t.Errorf("version = %q, want the build's %q", c.Version, Version)
	}
	if len(c.Commands) == 0 || len(c.GlobalFlags) == 0 || len(c.Environment) == 0 {
		t.Fatalf("the surface is missing a whole section: %+v", c)
	}

	// Every command carries what it takes to run it without reading anything else.
	for _, cmd := range c.Leaves() {
		if cmd.Usage == "" || cmd.Summary == "" {
			t.Errorf("`krowk %s` has no usage or no summary: %+v", cmd.Name, cmd)
		}
		if !strings.HasPrefix(cmd.Usage, "krowk "+strings.Fields(cmd.Name)[0]) {
			t.Errorf("`krowk %s` has usage %q, which does not spell the command", cmd.Name, cmd.Usage)
		}
		for _, f := range cmd.Flags {
			if f.Name == "" || f.Type == "" || f.Usage == "" {
				t.Errorf("`krowk %s` carries an incomplete flag: %+v", cmd.Name, f)
			}
		}
	}

	// And the flags a caller is most likely to reach for are really described.
	push, ok := c.Find([]string{"push"})
	if !ok {
		t.Fatal("push is not in the catalog")
	}
	run, ok := flagNamed(push, "run")
	if !ok || run.Type != "string" {
		t.Errorf("push --run = %+v, want a string flag", run)
	}
	reference, ok := flagNamed(push, "reference")
	if !ok || !reference.Repeatable {
		t.Errorf("push --reference = %+v, want it marked repeatable", reference)
	}
}

// All four spellings mean the same thing, because an agent that guesses one
// should not have to guess again.
func TestEverySpellingOfHelpJSONAgrees(t *testing.T) {
	_, viaHelpJSON, _ := runHelp(t, true, "help", "--json")
	_, viaHelpFormat, _ := runHelp(t, true, "help", "--format=json")
	_, viaFlagJSON, _ := runHelp(t, true, "--help", "--json")
	// Piped output is JSON everywhere else in krowk, so it is JSON here too.
	_, viaPipe, _ := runHelp(t, false, "help")

	for name, got := range map[string]string{
		"help --format=json": viaHelpFormat,
		"--help --json":      viaFlagJSON,
		"help, piped":        viaPipe,
	} {
		if got != viaHelpJSON {
			t.Errorf("`krowk %s` differs from `krowk help --json`", name)
		}
	}
}

// One command's help is the same data, narrowed — not a second description of
// it that could drift.
func TestHelpForOneCommandIsThatCommandsEntry(t *testing.T) {
	_, whole, _ := runHelp(t, true, "help", "--json")
	fromCatalog, _ := decodeCatalog(t, whole).Find([]string{"uploads", "attach"})

	for _, args := range [][]string{
		{"help", "uploads", "attach", "--json"},
		{"uploads", "attach", "--help", "--json"},
	} {
		code, stdout, stderr := runHelp(t, true, args...)
		if code != 0 {
			t.Fatalf("`krowk %s` exited %d, stderr:\n%s", strings.Join(args, " "), code, stderr)
		}
		var cmd Command
		if err := json.Unmarshal([]byte(stdout), &cmd); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, stdout)
		}
		if cmd.Name != "uploads attach" {
			t.Errorf("name = %q, want the whole path", cmd.Name)
		}
		if cmd.Usage != fromCatalog.Usage || len(cmd.Flags) != len(fromCatalog.Flags) {
			t.Errorf("`krowk %s` describes the command differently from the catalog: %+v",
				strings.Join(args, " "), cmd)
		}
	}
}

// A group is a real answer: `krowk help runs` lists what is under it rather
// than refusing because `runs` alone cannot be run.
func TestHelpForAGroupListsWhatIsUnderIt(t *testing.T) {
	code, stdout, _ := runHelp(t, true, "help", "runs", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var cmd Command
	if err := json.Unmarshal([]byte(stdout), &cmd); err != nil {
		t.Fatal(err)
	}
	if len(cmd.Subcommands) != 4 {
		t.Errorf("runs has %d subcommands, want 4: %+v", len(cmd.Subcommands), cmd)
	}
}

// Asking about a command that does not exist must not print the whole help and
// exit 0 — that reads as though the command were real.
func TestHelpForSomethingThatIsNotACommandRefuses(t *testing.T) {
	code, stdout, stderr := runHelp(t, true, "help", "frobnicate")
	if code == 0 {
		t.Fatalf("exit 0, stdout:\n%s", stdout)
	}
	if !strings.Contains(stderr, "unknown_command") {
		t.Errorf("stderr = %q, want unknown_command", stderr)
	}

	// Including a subcommand of a group that does exist.
	if code, _, stderr = runHelp(t, true, "help", "runs", "frobnicate"); code == 0 ||
		!strings.Contains(stderr, "unknown_command") {
		t.Errorf("`krowk help runs frobnicate` = %d, %q", code, stderr)
	}
}

// --help is often typed with the command already half-written, so the arguments
// standing after it must not turn the question into a refusal.
func TestHelpIgnoresTheArgumentsOfTheCommandItDescribes(t *testing.T) {
	code, stdout, stderr := runHelp(t, true, "push", "screenshot.png", "--help", "--json")
	if code != 0 {
		t.Fatalf("`krowk push screenshot.png --help` exited %d, stderr:\n%s", code, stderr)
	}
	var cmd Command
	if err := json.Unmarshal([]byte(stdout), &cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Name != "push" {
		t.Errorf("described %q, want push", cmd.Name)
	}

	// A group is different: the word after it is a command name, so a wrong one
	// is a mistake worth naming rather than an argument to ignore.
	if code, _, stderr = runHelp(t, true, "runs", "frobnicate", "--help"); code == 0 ||
		!strings.Contains(stderr, "unknown_command") {
		t.Errorf("`krowk runs frobnicate --help` = %d, %q", code, stderr)
	}
}

// The person at a terminal keeps the help they had.
func TestHumanHelpIsStillHumanOnATerminal(t *testing.T) {
	code, stdout, _ := runHelp(t, true, "help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{
		"krowk push <file...> [flags]",
		"Upload flags",
		"Global flags",
		"Environment",
		"Registry precedence",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the human help is missing %q", want)
		}
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Error("a terminal got JSON")
	}
}

// `krowk` alone is a greeting and not the manual. Somebody who typed the name to
// see what happens is asking what this is and what to type next, and an answer
// that fills the scrollback answers neither.
func TestBareKrowkGreetsRatherThanPrintingTheWholeHelp(t *testing.T) {
	code, stdout, _ := runHelp(t, true)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if lines := strings.Count(strings.TrimSpace(stdout), "\n") + 1; lines > 12 {
		t.Errorf("the greeting is %d lines, which is a wall of text:\n%s", lines, stdout)
	}
	// It says where the rest lives, precisely because it is not the rest.
	if !strings.Contains(stdout, "krowk help") {
		t.Errorf("the greeting does not point at the help:\n%s", stdout)
	}
	for _, section := range []string{"Usage", "Upload flags", "Exit codes"} {
		if strings.Contains(stdout, section) {
			t.Errorf("the greeting carries the %q section of the help:\n%s", section, stdout)
		}
	}

	// And asking for the help is still asking for all of it.
	_, full, _ := runHelp(t, true, "help")
	for _, want := range []string{"Usage", "Upload flags", "Exit codes"} {
		if !strings.Contains(full, want) {
			t.Errorf("`krowk help` no longer carries %q", want)
		}
	}
}

// A program is not greeted. It ran `krowk` with no arguments to find out what
// krowk can do, and three lines of prose are no use to it.
func TestBareKrowkAnswersAProgramWithTheSurface(t *testing.T) {
	_, piped, _ := runHelp(t, false)
	_, surface, _ := runHelp(t, true, "help", "--json")
	if piped != surface {
		t.Errorf("piped `krowk` is not the surface:\n%s", piped)
	}
}

// The greeting is the shortest path into krowk, so a command it recommends that
// has since been renamed would send the first-time reader straight into a
// refusal.
func TestTheGreetingRecommendsCommandsThatExist(t *testing.T) {
	c := Surface()
	checked := 0
	for _, line := range strings.Split(greetingTemplate, "\n") {
		// The indented lines, which are the recommendations. The line above them
		// starts with the name too, and is what krowk is rather than what to type.
		if !strings.HasPrefix(line, "  krowk ") {
			continue
		}
		checked++
		fields := strings.Fields(line)
		// Longest first, so `auth login` is checked as the pair it is, then
		// shortened past the arguments and flags standing after the command.
		words := fields[1:]
		for len(words) > 0 {
			if _, ok := c.Find(words); ok {
				break
			}
			words = words[:len(words)-1]
		}
		if len(words) == 0 {
			t.Errorf("the greeting recommends `%s`, which is not a krowk command", line)
		}
	}
	if checked == 0 {
		t.Error("no recommendation in the greeting was checked, so this test proves nothing")
	}
}

// The logo is the first thing a person sees, and it is only for a person: the
// JSON surface is a data structure, and one command's help answers a narrower
// question than "what is this tool", so neither carries decoration.
func TestHumanHelpOpensWithTheLogo(t *testing.T) {
	code, stdout, _ := runHelp(t, true, "help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(stdout, "\n"+mark+"\n\n") {
		t.Errorf("the human help does not open with the logo:\n%s", stdout)
	}
	// Under it, with a blank line between, so the mark is not jammed against
	// the words. Capitalised the way the brand is written, and two lines, so
	// the version and what krowk is can be read separately.
	if !strings.Contains(stdout, "\n\nKrowk "+Version+"\nPermalinks for agent output\n\n") {
		t.Errorf("the version lines are not under the logo:\n%s", stdout)
	}

	for _, args := range [][]string{
		{"help", "--json"},
		{"push", "--help"},
	} {
		if _, out, _ := runHelp(t, true, args...); strings.Contains(out, mark) {
			t.Errorf("`krowk %s` carries the logo:\n%s", strings.Join(args, " "), out)
		}
	}
}

// markdown and url are the two paste forms of an artifact's link — an embed and
// the card page's URL — and have nothing to say about a command, so they fall
// back to what a person would read rather than to something shaped like a
// paste.
func TestHelpInAPasteFormatFallsBackToTheText(t *testing.T) {
	for _, format := range []string{"markdown", "url"} {
		code, stdout, _ := runHelp(t, false, "help", "--format="+format)
		if code != 0 {
			t.Fatalf("--format=%s exited %d", format, code)
		}
		if !strings.Contains(stdout, "Usage") {
			t.Errorf("--format=%s did not print the help text:\n%s", format, stdout)
		}
	}
}

func flagNamed(cmd Command, name string) (Flag, bool) {
	for _, f := range cmd.Flags {
		if f.Name == name {
			return f, true
		}
	}
	return Flag{}, false
}
