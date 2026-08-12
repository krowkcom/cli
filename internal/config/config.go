// Package config is the layered configuration that lets a repository pin which
// workspace uploads from inside it go to, so an agent working in that checkout
// never has to name one. It depends on nothing but the standard library: the
// packages that do the work read configuration, and configuration must not need
// them back.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Where a resolved value came from, in the order they beat each other. These
// are what doctor and `config show` print, so they read as the thing a person
// would go and edit, not as an internal layer name.
const (
	SourceGlobal = "global config"
	SourceRepo   = "repo config"
	SourceEnv    = "KROWK_WORKSPACE"
	SourceFlag   = "--workspace"
)

// Config is the resolved configuration — every key as it came out of the
// layering, with no memory of the layers that lost.
type Config struct {
	Workspace string

	// Sources records where each key's value came from, keyed by config key
	// name ("workspace"), so doctor and `config show` can say why. A key that
	// nothing set is absent from the map rather than present and empty: "no one
	// set this" is a different answer from "something set it to nothing".
	Sources map[string]string
}

// Overrides are the flags of the command being run, the strongest layer.
type Overrides struct {
	Workspace string // the --workspace flag
}

// key is one configuration key, described once. Load's file parsing, the
// environment layer, the flag layer, KnownKeys and Set/Unset validation all read
// this table, so adding a key is adding a row here and nothing else — the
// alternative is the same key spelled into five if-blocks, four of which someone
// eventually forgets.
type key struct {
	name string
	// env is the variable that sets this key, and doubles as the source string:
	// naming the variable is the only useful thing to tell someone surprised by
	// its value.
	env string
	// flag is the command line flag that sets this key, source string likewise.
	flag string
	// assign puts a resolved value onto the Config.
	assign func(*Config, string)
	// fromOverrides reads this key's value out of the flags, so the strongest
	// layer stays as table-driven as the files are.
	fromOverrides func(Overrides) string
}

var schema = []key{{
	name:          "workspace",
	env:           "KROWK_WORKSPACE",
	flag:          "--workspace",
	assign:        func(c *Config, v string) { c.Workspace = v },
	fromOverrides: func(o Overrides) string { return o.Workspace },
}}

// Load resolves the layered configuration as seen from dir ("" means the
// working directory).
//
// Weakest to strongest: the global file, the repo file, the environment, then
// the flags. Each layer only speaks where it has something to say, so pinning a
// workspace in a repo does not stop KROWK_WORKSPACE from redirecting one
// command, and neither of them has to repeat what the global file already says.
func Load(dir string, env func(string) string, o Overrides) (*Config, error) {
	c := &Config{Sources: map[string]string{}}

	global, err := readFile(GlobalPath())
	if err != nil {
		return nil, err
	}
	apply(c, global, SourceGlobal)

	// An absent repo is not an error: krowk is run from outside checkouts all
	// the time, and having no repo config simply means the layer is not there.
	if path, inRepo := RepoPath(dir); inRepo {
		repo, err := readFile(path)
		if err != nil {
			return nil, err
		}
		apply(c, repo, SourceRepo)
	}

	for _, k := range schema {
		if v := env(k.env); !blank(v) {
			c.Sources[k.name] = k.env
			k.assign(c, v)
		}
	}
	for _, k := range schema {
		if v := k.fromOverrides(o); !blank(v) {
			c.Sources[k.name] = k.flag
			k.assign(c, v)
		}
	}
	return c, nil
}

// blank reports a value that should not override anything below it. A key
// present with an empty value is someone clearing it, not someone selecting the
// empty workspace, and whitespace-only is the same thing typed less carefully.
func blank(v string) bool { return strings.TrimSpace(v) == "" }

// apply copies whatever a parsed file had to say into the Config, recording the
// source of every value that landed.
func apply(c *Config, values map[string]string, source string) {
	for _, k := range schema {
		if v, ok := values[k.name]; ok && !blank(v) {
			c.Sources[k.name] = source
			k.assign(c, v)
		}
	}
}

// readFile parses one config file down to the keys this build knows.
//
// A file that is not there is not a problem — most machines have neither of
// these files, and every one of them would otherwise have to be told so. A file
// that is there and broken is a hard error naming the path: somebody wrote it
// meaning to steer uploads somewhere, and skipping it with a shrug would send
// uploads to the workspace they thought they had just changed, while the file on
// disk says otherwise. Better to refuse and point at the file than to be quietly
// wrong about where bytes went.
//
// Keys outside the schema are ignored without a word. Config files are
// committed and shared, and an older krowk that met a key from a newer one has
// no business refusing to run over a setting it does not need.
func readFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", path, err)
	}

	values := map[string]string{}
	for _, k := range schema {
		v, ok := raw[k.name]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok {
			// A number or a list where a name belongs is the same class of
			// mistake as broken JSON, and gets the same treatment: the file was
			// meant to mean something, and this build cannot tell what.
			return nil, fmt.Errorf("%s: %q must be a string", path, k.name)
		}
		values[k.name] = s
	}
	return values, nil
}

// GlobalPath is ~/.config/krowk/config.json, XDG_CONFIG_HOME honoured. Not
// os.UserConfigDir: that is ~/Library/Application Support on macOS, and the CLI
// documents one path on every platform.
func GlobalPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(".krowk", "config.json")
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "krowk", "config.json")
}

// RepoPath is where the repo config for dir lives ("" means the working
// directory), and whether dir is inside a git repository at all. The path is
// returned whether or not the file exists yet — it is where `config set`
// writes.
func RepoPath(dir string) (path string, inRepo bool) {
	root, ok := gitRoot(dir)
	if !ok {
		return "", false
	}
	return filepath.Join(root, ".krowk", "config.json"), true
}

// gitRoot walks up from dir looking for a .git entry.
//
// A .git that is a plain file counts as much as a directory: worktrees and
// submodules record their real git directory in a file, and treating those
// checkouts as "not a repository" would silently drop the repo layer in exactly
// the setups agents are handed most often.
//
// The walk runs to the filesystem root with no $HOME boundary, which some CLIs
// impose to avoid honouring a config file planted above a user's home. That
// boundary would cost more than it buys here: agents run krowk in containers and
// scratch checkouts at arbitrary paths, where /work or /srv is nobody's home,
// and this file can only choose among workspaces the machine already holds a key
// for — it can never point krowk at a different registry or send a token
// anywhere new. The day a key with authority over that lands in this schema — a
// registry URL, anything naming where credentials go — the walk needs a trust
// gate to go with it, because then a file found up the tree could redirect a
// secret rather than pick a destination for bytes.
func gitRoot(dir string) (string, bool) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", false
		}
		dir = wd
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// KnownKeys lists every key this build understands, for help and errors.
func KnownKeys() []string {
	names := make([]string, 0, len(schema))
	for _, k := range schema {
		names = append(names, k.name)
	}
	return names
}

// known reports whether key is in the schema. Writing a key krowk does not
// understand is refused rather than stored: a typo that lands in the file is a
// setting that never takes effect, and the file would sit there looking like it
// had.
func known(name string) error {
	for _, k := range schema {
		if k.name == name {
			return nil
		}
	}
	return fmt.Errorf("unknown config key %q, valid keys: %s", name, strings.Join(KnownKeys(), ", "))
}

// Set writes key=value into the JSON file at path, creating the file and its
// directory if needed, preserving any keys it does not understand.
func Set(path, key, value string) error {
	if err := known(key); err != nil {
		return err
	}
	return rewrite(path, func(raw map[string]any) { raw[key] = value })
}

// Unset removes key from the file at path. Removing what is not there succeeds,
// including from a file that does not exist: the caller asked for a state, and
// that state already holds.
func Unset(path, key string) error {
	if err := known(key); err != nil {
		return err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return rewrite(path, func(raw map[string]any) { delete(raw, key) })
}

// rewrite reads the file as a bare map, hands it to edit, and writes it back.
//
// The map is deliberately untyped. Keys this build never heard of survive the
// edit that way, so a newer krowk's setting is not quietly deleted the first
// time an older one touches the same file — the two builds share a checkout far
// more often than either of them is upgraded.
func rewrite(path string, edit func(map[string]any)) error {
	raw := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &raw); err != nil {
			return fmt.Errorf("%s is not valid JSON: %w", path, err)
		}
	case !os.IsNotExist(err):
		return fmt.Errorf("reading %s: %w", path, err)
	}
	edit(raw)

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(out, '\n'))
}

// writeAtomic replaces the file at path in one step — a temporary file in the
// same directory, then a rename — so a crash or a full disk partway through
// leaves the previous configuration intact rather than a truncated file that
// fails to parse and, by the rule above, stops every later command.
//
// The result is 0644, unlike the credentials file next to it. This is not a
// secret: the repo copy is meant to be committed and read by everyone working in
// the checkout, and os.CreateTemp's 0600 would leave it readable only by
// whoever happened to run `config set` first.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	// A no-op once the rename has consumed the name, so it can run on every path
	// and still not delete the config it just wrote.
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	// Durable before visible: the rename must not publish a name pointing at
	// bytes the kernel has not committed.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
