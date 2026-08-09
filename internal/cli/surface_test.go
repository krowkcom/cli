package cli

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/krowkcom/cli/internal/mcp"
)

// update regenerates the golden surface. It is deliberately a flag rather than
// an environment variable: regenerating is a decision, and it should look like
// one in the shell history that recorded it.
var update = flag.Bool("update", false, "rewrite testdata/surface.json from the current surface")

const surfacePath = "testdata/surface.json"

// snapshot is what the golden file holds: the CLI's surface and the MCP
// server's, together, because they are one product with two front doors.
//
// The version is left out. It is stamped at build time, so pinning it would
// make the snapshot fail on every release for a reason that has nothing to do
// with the surface.
type snapshot struct {
	CLI Catalog           `json:"cli"`
	MCP []mcp.ToolSurface `json:"mcp_tools"`
}

// TestSurface pins the whole machine-facing surface to a file in the repository.
//
// It is not here to catch mistakes — the parity tests below do that. It is here
// so that changing the surface is visible in review as a diff of what krowk
// promises, rather than buried inside a change to how a command works. Once
// `npx krowk` ships, someone else's script is on the other end of this file.
func TestSurface(t *testing.T) {
	current := snapshot{CLI: catalog(), MCP: mcp.Surface()}
	// catalog() rather than Surface(): the version is a build stamp, not surface.
	if current.CLI.Version != "" {
		t.Fatal("the catalog carries a version, which would make the golden file " +
			"fail on every release — Surface fills it in, catalog must not")
	}

	encoded, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')

	if *update {
		if err := os.MkdirAll(filepath.Dir(surfacePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(surfacePath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("wrote " + surfacePath)
		return
	}

	golden, err := os.ReadFile(surfacePath)
	if err != nil {
		t.Fatalf("%v — run `go test ./internal/cli -run TestSurface -update` to create it", err)
	}
	if string(golden) == string(encoded) {
		return
	}

	t.Errorf(`the command surface changed.

%s no longer matches the catalog. That file is the contract krowk publishes:
the JSON that `+"`krowk help --json`"+` answers with, and what anything reading the
surface parses. Changing it is fine — changing it by accident is not.

If the change is intended:
  1. go test ./internal/cli -run TestSurface -update
  2. update the command table in README.md, and the MCP tool table with it
  3. commit the regenerated file in the same commit as the change

first difference: %s`, surfacePath, firstDifference(string(golden), string(encoded)))
}

// firstDifference names the line that moved, since a diff of the whole file
// says less than the one line that changed.
func firstDifference(golden, current string) string {
	was, now := strings.Split(golden, "\n"), strings.Split(current, "\n")
	for i := 0; i < len(was) && i < len(now); i++ {
		if was[i] != now[i] {
			return "line " + strconv.Itoa(i+1) + "\n  was: " + strings.TrimSpace(was[i]) +
				"\n  now: " + strings.TrimSpace(now[i])
		}
	}
	if len(was) != len(now) {
		return "the file grew or shrank: " + strconv.Itoa(len(was)) + " lines to " + strconv.Itoa(len(now))
	}
	return "none — the files differ only in trailing bytes"
}

// routeExpr finds the command names the routing switch matches on. The switch
// is the only thing that decides whether a command exists, so it is what the
// catalog is checked against — a catalog entry with no case is a command krowk
// advertises and does not have.
var routeExpr = regexp.MustCompile(`positionals\[(\d)\] == "([a-z]+)"`)

// TestTheCatalogAndTheRoutingAgree reads the routing switch out of the source
// and holds it to the catalog, in both directions.
func TestTheCatalogAndTheRoutingAgree(t *testing.T) {
	routed := routedCommands(t)

	var advertised []string
	for _, cmd := range catalog().Leaves() {
		advertised = append(advertised, cmd.Name)
	}
	slices.Sort(advertised)
	slices.Sort(routed)

	for _, name := range advertised {
		if !slices.Contains(routed, name) {
			t.Errorf("the catalog advertises `krowk %s`, which the routing switch does not handle", name)
		}
	}
	for _, name := range routed {
		if !slices.Contains(advertised, name) {
			t.Errorf("`krowk %s` is routed but missing from the catalog, so `krowk help --json` "+
				"does not know it exists", name)
		}
	}
}

// routedCommands parses cli.go for the command names the switch matches. Each
// case line names either one word or two, and the two-word ones are the groups.
func routedCommands(t *testing.T) []string {
	t.Helper()
	source, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatal(err)
	}

	var routed []string
	for _, line := range strings.Split(string(source), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "case ") {
			continue
		}
		words := map[string]string{}
		for _, m := range routeExpr.FindAllStringSubmatch(line, -1) {
			words[m[1]] = m[2]
		}
		if words["0"] == "" {
			continue
		}
		name := words["0"]
		if words["1"] != "" {
			name += " " + words["1"]
		}
		if !slices.Contains(routed, name) {
			routed = append(routed, name)
		}
	}
	return routed
}

// TestTheCatalogAndTheFlagSetAgree holds the advertised flags to the ones that
// actually parse. The flag set is the real surface: a flag missing from it is
// advice to type something that fails, and one missing from the catalog is a
// flag no reader of `krowk help --json` will ever find.
func TestTheCatalogAndTheFlagSetAgree(t *testing.T) {
	var f flags
	registered := map[string]*flag.Flag{}
	newFlagSet(&f).VisitAll(func(fl *flag.Flag) { registered[fl.Name] = fl })

	c := catalog()
	advertised := map[string]Flag{}
	for _, cmd := range c.Leaves() {
		for _, fl := range cmd.Flags {
			advertised[fl.Name] = fl
		}
	}
	for _, fl := range c.GlobalFlags {
		advertised[fl.Name] = fl
		for _, alias := range fl.Aliases {
			advertised[alias] = fl
		}
	}

	for name, fl := range advertised {
		real, ok := registered[name]
		if !ok {
			t.Errorf("the catalog advertises --%s, which nothing parses", name)
			continue
		}
		// The default is what the flag set will actually use, so the catalog may
		// not claim a different one. An int flag whose zero means "the registry
		// decides" is the one exception, and it says so by carrying the real
		// default as documentation instead.
		if fl.Type != typeInt && fl.Default != "" && fl.Default != real.DefValue {
			t.Errorf("--%s defaults to %q in the catalog and %q in the flag set",
				name, fl.Default, real.DefValue)
		}
	}
	for name := range registered {
		if _, ok := advertised[name]; !ok {
			t.Errorf("--%s parses but is in no catalog entry, so nothing documents it", name)
		}
	}
}

// TestEveryFlagInTheHelpTextIsReal catches the other half of the drift: the
// flag sections of the help are prose, and prose can name a flag that was
// renamed or removed.
func TestEveryFlagInTheHelpTextIsReal(t *testing.T) {
	var f flags
	registered := map[string]bool{}
	newFlagSet(&f).VisitAll(func(fl *flag.Flag) { registered[fl.Name] = true })

	help := renderedHelp()
	for _, m := range regexp.MustCompile(`--([a-z][a-z-]*)`).FindAllStringSubmatch(help, -1) {
		if !registered[m[1]] {
			t.Errorf("the help text names --%s, which nothing parses", m[1])
		}
	}

	// And every flag that parses is named somewhere a person can find it.
	for name := range registered {
		if len(name) == 1 {
			continue // -h and -v are spelled in the global flags block as shorthands
		}
		if !strings.Contains(help, "--"+name) {
			t.Errorf("--%s parses but the help text never mentions it", name)
		}
	}
}

// TestEveryCommandInTheHelpTextIsInTheCatalog holds the usage block to the
// catalog it is rendered from — cheap insurance that the rendering itself did
// not start dropping rows.
func TestEveryCommandInTheHelpTextIsInTheCatalog(t *testing.T) {
	help := renderedHelp()
	for _, cmd := range catalog().Leaves() {
		if !strings.Contains(help, cmd.Usage) {
			t.Errorf("the help text does not show %q", cmd.Usage)
		}
		if !strings.Contains(help, cmd.Summary) {
			t.Errorf("the help text does not show the summary of `krowk %s`", cmd.Name)
		}
	}
}

// TestEveryEnvironmentVariableInTheCatalogIsRead is the same check for the
// environment: a variable the surface promises and nothing reads is a lie a
// caller cannot detect except by it silently doing nothing.
func TestEveryEnvironmentVariableInTheCatalogIsRead(t *testing.T) {
	sources := map[string]string{}
	for _, pkg := range []string{".", "../api", "../runctx"} {
		entries, err := os.ReadDir(pkg)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			// Not catalog.go. It is the thing being checked: every name this test
			// looks for is written down there as an Environment entry, so scanning
			// it makes the promise its own evidence and the test passes whether or
			// not anything reads the variable.
			if entry.Name() == "catalog.go" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(pkg, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			sources[filepath.Join(pkg, entry.Name())] = string(b)
		}
	}

	for _, v := range catalog().Environment {
		found := false
		for _, source := range sources {
			if strings.Contains(source, `"`+v.Name+`"`) {
				found = true
			}
		}
		if !found {
			t.Errorf("the catalog promises %s, which nothing reads", v.Name)
		}
	}
}

func renderedHelp() string {
	var b strings.Builder
	if err := showHelp(&b, nil, "human"); err != nil {
		panic(err)
	}
	return b.String()
}
