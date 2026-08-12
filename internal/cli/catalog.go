package cli

import (
	"fmt"
	"strings"

	"github.com/krowkcom/cli/internal/api"
)

// The catalog is what krowk says it can do, as data rather than as prose.
//
// It exists because the help text was the only description of the surface, and
// a hand-written paragraph cannot be checked against the code that routes the
// commands. Once `npx @krowk/cli` puts this in front of tooling that reads the
// surface instead of a person, a help text that has drifted from the routing is
// not a documentation bug — it is an interface that lies.
//
// So the catalog is the single source of the *command list*: the human help's
// Usage block is rendered from it, `krowk help --json` serialises it, and the
// tests hold it to the routing switch and to the flag set that actually parses.
// The prose in the help text stays prose — a struct cannot say why a keyless
// upload has nowhere to put its metadata — but every fact a caller could branch
// on comes from here.

// Catalog is the whole surface: what krowk is called, what it can do, and what
// it reads from the environment.
type Catalog struct {
	Name string `json:"name"`
	// Version is stamped at build time, so it is filled in when the catalog is
	// asked for rather than written down here.
	Version     string    `json:"version"`
	Summary     string    `json:"summary"`
	Commands    []Command `json:"commands"`
	GlobalFlags []Flag    `json:"global_flags"`
	Environment []EnvVar  `json:"environment"`
}

// Command is one thing krowk does, or a group of them. A group carries
// subcommands and is not invocable itself: `krowk uploads` alone is not a
// command, which is why Usage is empty for one.
type Command struct {
	Name        string    `json:"name"`
	Usage       string    `json:"usage,omitempty"`
	Summary     string    `json:"summary"`
	Args        []Arg     `json:"args,omitempty"`
	Flags       []Flag    `json:"flags,omitempty"`
	Subcommands []Command `json:"subcommands,omitempty"`
}

// Arg is one positional. Required and Repeated are separate questions: `push`
// takes one file or twenty, and needs at least one.
type Arg struct {
	Name     string `json:"name"`
	Summary  string `json:"summary"`
	Required bool   `json:"required"`
	Repeated bool   `json:"repeated,omitempty"`
}

// Flag is one flag as the flag set actually registers it. Type and Default are
// the flag set's, not a description of them — a test holds the two together.
type Flag struct {
	Name string `json:"name"`
	// Aliases are the other spellings that set the same value, e.g. `h` for
	// `help`. They are the same flag, so they are not separate entries.
	Aliases []string `json:"aliases,omitempty"`
	Type    string   `json:"type"`
	Default string   `json:"default"`
	Usage   string   `json:"usage"`
	// Repeatable says the flag may be given more than once and collects every
	// value, rather than the last one winning.
	Repeatable bool `json:"repeatable,omitempty"`
}

// EnvVar is one variable krowk reads.
type EnvVar struct {
	Name    string `json:"name"`
	Usage   string `json:"usage"`
	Default string `json:"default,omitempty"`
}

// Flag types, spelled as the flag set's own kinds rather than as Go types, so a
// consumer that has never seen Go can still read the catalog.
const (
	typeString = "string"
	typeInt    = "int"
	typeBool   = "bool"
)

// uploadFlags are what `push` and `uploads create` take — the same command
// spelled two ways.
func uploadFlags() []Flag {
	return append([]Flag{runFlag("Attach to an existing run instead of opening one")},
		metadataFlags()...)
}

// metadataFlags record where the work came from. They live on the run, which is
// why `runs start` takes them all and no command that names an existing run
// does: the metadata was recorded when the run was opened.
func metadataFlags() []Flag {
	return []Flag{
		{Name: "pull-request", Type: typeString, Usage: "Pull request the work belongs to"},
		{Name: "reference", Type: typeString, Repeatable: true,
			Usage: "Related link — repeat for more than one"},
		{Name: "session", Type: typeString, Usage: "Agent session ID"},
		{Name: "title", Type: typeString, Usage: "Label for the markdown link"},
		{Name: "repo", Type: typeString, Usage: "Override the detected repository"},
		{Name: "commit", Type: typeString, Usage: "Override the detected commit"},
		{Name: "agent", Type: typeString, Usage: "Override the detected agent"},
	}
}

// runFlag is one flag with three jobs, so each place that takes it says what it
// means there. On a push it opens or names the run the artifacts go under; on a
// listing it narrows the page; on a claim or an attach it names the run an
// upload that already exists joins.
func runFlag(usage string) Flag {
	return Flag{Name: "run", Type: typeString, Usage: usage}
}

// globalFlag picks which config file `config set` and `config unset` write.
// One definition for both, so the two commands can never describe it
// differently.
var globalFlag = Flag{Name: "global", Type: typeBool, Default: "false",
	Usage: "Write the machine-wide config instead of the repository's"}

// pageFlags walk a listing. Every listing takes them; only the uploads listing
// also takes --run, since runs are not grouped under anything themselves.
func pageFlags() []Flag {
	return []Flag{
		{Name: "limit", Type: typeInt, Default: "50", Usage: "Rows per page (1–100)"},
		{Name: "before", Type: typeString, Usage: "Start after this row — the `next` of the last page"},
	}
}

// catalog is the surface itself. Version is left blank here and filled in by
// Surface, so the one place a build stamps a version stays cli.Version.
func catalog() Catalog {
	return Catalog{
		Name:    "krowk",
		Summary: "permalinks for agent output",
		Commands: []Command{
			{
				Name:    "push",
				Usage:   "krowk push <file...> [flags]",
				Summary: "Upload files, get a link for each",
				Args:    []Arg{{Name: "file", Summary: "Path to upload", Required: true, Repeated: true}},
				Flags:   uploadFlags(),
			},
			{
				Name:    "uploads",
				Summary: "Work with uploads",
				Subcommands: []Command{
					{
						Name:    "create",
						Usage:   "krowk uploads create <file...> [flags]",
						Summary: "The same thing, spelled out",
						Args:    []Arg{{Name: "file", Summary: "Path to upload", Required: true, Repeated: true}},
						Flags:   uploadFlags(),
					},
					{
						Name:    "list",
						Usage:   "krowk uploads list [flags]",
						Summary: "List uploads, newest first — a run's, or the workspace's",
						Flags: append(pageFlags(),
							runFlag("Narrow it to what one run produced")),
					},
					{
						Name:    "show",
						Usage:   "krowk uploads show <artifact>",
						Summary: "Read one artifact back",
						Args:    []Arg{{Name: "artifact", Summary: "The artifact slug", Required: true}},
					},
					{
						Name:    "attach",
						Usage:   "krowk uploads attach <art> --run <run>",
						Summary: "Put an upload under a run afterwards",
						Args:    []Arg{{Name: "artifact", Summary: "The artifact slug", Required: true}},
						Flags: []Flag{runFlag(
							"The run to put it under — required, and it must be one this workspace holds")},
					},
					{
						Name:    "delete",
						Usage:   "krowk uploads delete <art> [token]",
						Summary: "Take an upload down — immediate, cannot be undone",
						Args: []Arg{
							{Name: "artifact", Summary: "The artifact slug", Required: true},
							{Name: "claim-token", Summary: "The claim token an anonymous upload came back " +
								"with, when there is no key to authorise the takedown"},
						},
					},
				},
			},
			{
				Name:    "runs",
				Summary: "Work with runs",
				Subcommands: []Command{
					{
						Name:    "start",
						Usage:   "krowk runs start [flags]",
						Summary: "Open a run to group uploads under",
						// A run is where the metadata lives, so opening one takes every
						// flag that records any of it — but not --run, which would name a
						// second run for the run being opened.
						Flags: metadataFlags(),
					},
					{
						Name:    "list",
						Usage:   "krowk runs list [--limit --before]",
						Summary: "List the workspace's runs, newest first",
						Flags:   pageFlags(),
					},
					{
						Name:    "show",
						Usage:   "krowk runs show <run>",
						Summary: "Read one run back, with its metadata",
						Args:    []Arg{{Name: "run", Summary: "The run slug", Required: true}},
					},
					{
						Name:    "finish",
						Usage:   "krowk runs finish <run>",
						Summary: "Close a run",
						Args:    []Arg{{Name: "run", Summary: "The run slug", Required: true}},
					},
				},
			},
			{
				Name:    "claim",
				Usage:   "krowk claim <artifact> <token> [--run]",
				Summary: "Keep an anonymous upload past expiry",
				Args: []Arg{
					{Name: "artifact", Summary: "The artifact slug", Required: true},
					{Name: "claim-token", Summary: "The token the anonymous upload came back with",
						Required: true},
				},
				Flags: []Flag{runFlag(
					"The run to group it under while claiming — a claimed upload has none otherwise")},
			},
			{
				Name:    "auth",
				Summary: "Manage the API key",
				Subcommands: []Command{
					{
						Name:    "login",
						Usage:   "krowk auth login [--token <token>] [--no-browser]",
						Summary: "Approve this machine in the browser, or store a key",
						Flags: []Flag{
							{Name: "token", Type: typeString,
								Usage: "Check and store this key instead of asking the browser — " +
									"how CI logs in, e.g. krowk_sk_..."},
							{Name: "no-browser", Type: typeBool, Default: "false",
								Usage: "Print the code and the page instead of opening a browser — " +
									"the default over SSH, or with no display"},
						},
					},
					{Name: "token", Usage: "krowk auth token", Summary: "Print the stored token"},
					{Name: "verify", Usage: "krowk auth verify", Summary: "Check the key and its workspace"},
				},
			},
			{
				Name:    "workspaces",
				Summary: "The stored keys, one per workspace",
				Subcommands: []Command{
					{Name: "list", Usage: "krowk workspaces [list]",
						Summary: "List the stored keys, and which workspace resolves here"},
					{
						Name:    "use",
						Usage:   "krowk workspaces use <workspace>",
						Summary: "Make a stored key the machine-wide default",
						Args: []Arg{{Name: "workspace", Required: true,
							Summary: "A workspace `krowk workspaces` lists"}},
					},
				},
			},
			{
				Name:    "config",
				Summary: "Pin a repository, or the machine, to a workspace",
				Subcommands: []Command{
					{Name: "show", Usage: "krowk config show",
						Summary: "The effective configuration, and which layer set each value"},
					{
						Name:    "set",
						Usage:   "krowk config set <key> <value> [--global]",
						Summary: "Write one value into the repo config, or the global one",
						Args: []Arg{
							{Name: "key", Required: true, Summary: "A configuration key, e.g. `workspace`"},
							{Name: "value", Required: true, Summary: "What to set it to"},
						},
						Flags: []Flag{globalFlag},
					},
					{
						Name:    "unset",
						Usage:   "krowk config unset <key> [--global]",
						Summary: "Remove one value from the repo config, or the global one",
						Args: []Arg{{Name: "key", Required: true,
							Summary: "A configuration key, e.g. `workspace`"}},
						Flags: []Flag{globalFlag},
					},
				},
			},
			{Name: "doctor", Usage: "krowk doctor", Summary: "Check the local setup"},
			{
				Name:    "registry",
				Summary: "The local registry to develop against",
				Subcommands: []Command{
					{
						Name:    "serve",
						Usage:   "krowk registry serve",
						Summary: "Run a local registry to develop against",
						Flags: []Flag{
							{Name: "addr", Type: typeString, Default: defaultRegistryAddr,
								Usage: "Listen address (loopback only by default)"},
							{Name: "site", Type: typeString,
								Usage: "Origin for the links it returns (default: the request host)"},
							{Name: "limit-bytes", Type: typeInt,
								Usage: "Reject uploads above this size"},
						},
					},
				},
			},
			{
				Name:    "help",
				Usage:   "krowk help [command]",
				Summary: "Show this, or one command's own help",
				Args: []Arg{{Name: "command",
					Summary: "The command to describe, e.g. `uploads attach`"}},
			},
		},
		GlobalFlags: []Flag{
			{Name: "workspace", Type: typeString,
				Usage: "Use this workspace's stored key for this one command — " +
					"outranks KROWK_WORKSPACE and every config file"},
			{Name: "dev", Type: typeBool, Default: "false",
				// The address is part of what the flag does, and a machine reading
				// the surface has nowhere else to learn it.
				Usage: "Talk to a local registry at " + api.DevBaseURL},
			{Name: "format", Type: typeString,
				Usage: "human | json | markdown | url (default: human on a TTY, json when piped)"},
			{Name: "json", Type: typeBool, Default: "false", Usage: "Shorthand for --format json"},
			{Name: "quiet", Type: typeBool, Default: "false", Usage: "Raw JSON, no envelope"},
			{Name: "help", Aliases: []string{"h"}, Type: typeBool, Default: "false", Usage: "Show the help"},
			{Name: "version", Aliases: []string{"v"}, Type: typeBool, Default: "false",
				Usage: "Print the version"},
		},
		Environment: []EnvVar{
			{Name: "KROWK_TOKEN", Usage: "API token — wins over the credentials file"},
			{Name: "KROWK_WORKSPACE",
				Usage: "Workspace whose stored key to use, as if by --workspace — " +
					"outranks the config files, loses to the flag"},
			{Name: "KROWK_API_URL", Usage: "API base URL", Default: api.DefaultBaseURL},
			{Name: "KROWK_DEV", Usage: "1/true/yes/on — same as --dev"},
			{Name: "KROWK_AGENT", Usage: "Agent name to report"},
		},
	}
}

// Surface is the catalog as this build of krowk would report it.
func Surface() Catalog {
	c := catalog()
	c.Version = Version
	return c
}

// Leaves are the commands that can actually be run, each with the whole path a
// caller types. A group is not one of them: `krowk uploads` on its own is not a
// command, and the tests that hold the catalog to the routing switch would
// otherwise be looking for a case that should not exist.
func (c Catalog) Leaves() []Command {
	var leaves []Command
	for _, cmd := range c.Commands {
		if len(cmd.Subcommands) == 0 {
			leaves = append(leaves, cmd)
			continue
		}
		for _, sub := range cmd.Subcommands {
			sub.Name = cmd.Name + " " + sub.Name
			leaves = append(leaves, sub)
		}
	}
	return leaves
}

// Find resolves what a caller typed after `help` to one entry: a leaf, or a
// group with everything under it. The path is words, so `help uploads attach`
// and `help uploads` both land somewhere sensible.
func (c Catalog) Find(path []string) (Command, bool) {
	if len(path) == 0 {
		return Command{}, false
	}
	for _, cmd := range c.Commands {
		if cmd.Name != path[0] {
			continue
		}
		// A command with nothing under it answers for whatever follows it, because
		// what follows is its arguments: `krowk push shot.png --help` is someone
		// asking about push while holding the command they were about to run, and
		// refusing it over the filename would be pedantry.
		if len(path) == 1 || len(cmd.Subcommands) == 0 {
			return cmd, true
		}
		for _, sub := range cmd.Subcommands {
			if sub.Name == path[1] {
				sub.Name = cmd.Name + " " + sub.Name
				return sub, true
			}
		}
		return Command{}, false
	}
	return Command{}, false
}

// usageBlock renders the command list at the top of the help text. It is
// generated rather than written down so that a command cannot be added to the
// catalog, and to the JSON, without appearing here too.
func usageBlock(c Catalog) string {
	leaves := c.Leaves()

	width := 0
	for _, cmd := range leaves {
		width = max(width, len(cmd.Usage))
	}

	var b strings.Builder
	for _, cmd := range leaves {
		fmt.Fprintf(&b, "  %-*s%s\n", width+4, cmd.Usage, cmd.Summary)
	}
	return strings.TrimRight(b.String(), "\n")
}

// commandHelp is one command's own help, for `krowk uploads attach --help`. It
// is the catalog read back as text: the same facts the JSON carries, in the
// order someone typing the command needs them.
func commandHelp(cmd Command) string {
	lines := []string{"krowk " + cmd.Name + " — " + strings.ToLower(cmd.Summary[:1]) + cmd.Summary[1:]}

	if cmd.Usage != "" {
		lines = append(lines, "", "Usage", "  "+cmd.Usage)
	}
	if len(cmd.Subcommands) > 0 {
		lines = append(lines, "", "Commands")
		width := 0
		for _, sub := range cmd.Subcommands {
			width = max(width, len(sub.Usage))
		}
		for _, sub := range cmd.Subcommands {
			lines = append(lines, fmt.Sprintf("  %-*s%s", width+4, sub.Usage, sub.Summary))
		}
	}
	// One column width across the whole page, so the arguments and both sets of
	// flags line up as a single table rather than three ragged ones.
	globals := catalog().GlobalFlags
	width := 0
	for _, arg := range cmd.Args {
		width = max(width, len(argLabel(arg)))
	}
	for _, f := range append(append([]Flag{}, cmd.Flags...), globals...) {
		width = max(width, len(flagLabel(f)))
	}
	row := func(label, usage string) string {
		return fmt.Sprintf("  %-*s%s", width+2, label, usage)
	}

	if len(cmd.Args) > 0 {
		lines = append(lines, "", "Arguments")
		for _, arg := range cmd.Args {
			lines = append(lines, row(argLabel(arg), arg.Summary))
		}
	}
	if len(cmd.Flags) > 0 {
		lines = append(lines, "", "Flags")
		for _, f := range cmd.Flags {
			lines = append(lines, row(flagLabel(f), f.Usage))
		}
	}
	lines = append(lines, "", "Global flags")
	for _, f := range globals {
		lines = append(lines, row(flagLabel(f), f.Usage))
	}
	return strings.Join(lines, "\n")
}

// argLabel spells a positional the way the usage line does: angle brackets for
// one that is required, square for one that is not.
func argLabel(a Arg) string {
	label := "<" + a.Name + ">"
	if !a.Required {
		label = "[" + a.Name + "]"
	}
	if a.Repeated {
		label = strings.TrimSuffix(label, ">") + "...>"
	}
	return label
}

// flagLabel spells a flag the way it is typed. A boolean takes no value, and
// its default of false is what "not passed" already means — printing it would
// be noise on every line.
func flagLabel(f Flag) string {
	label := "--" + f.Name
	for _, alias := range f.Aliases {
		label += ", -" + alias
	}
	if f.Type == typeBool {
		return label
	}
	label += " <" + f.Type + ">"
	if f.Default != "" {
		label += " (default " + f.Default + ")"
	}
	return label
}
