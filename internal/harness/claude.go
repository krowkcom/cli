package harness

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode"
)

// The names Claude Code itself uses.
const (
	// claudeUserConfigFile holds user- and local-scope MCP registrations.
	// $HOME/.claude.json is its documented location, and the one that is
	// checked first. Whether CLAUDE_CONFIG_DIR moves it too is not documented
	// either way, so the configuration directory is probed as well rather
	// than assumed to be irrelevant — a file that is not there costs a failed
	// open and says nothing.
	claudeUserConfigFile = ".claude.json"
	// claudeProjectConfigFile is the checked-in, project-scope form the README
	// documents. It is written by whoever wrote the checkout, so it is read
	// defensively.
	claudeProjectConfigFile = ".mcp.json"
	claudeDirName           = ".claude"
	// claudeSkillDirName and claudeSkillFile are where the krowk skill lands
	// under Claude Code's configuration directory — the same two names
	// scripts/install.sh writes.
	claudeSkillDirName = "krowk"
	claudeSkillFile    = "SKILL.md"

	// KrowkMCPCommand is the binary krowk's MCP server runs as.
	KrowkMCPCommand = "krowk-mcp"
	// KrowkMCPPackage is the npm package that runs the same server under npx.
	KrowkMCPPackage = "@krowk/mcp"
)

// Check names, so consumers can match on them without repeating strings.
const (
	CheckNameClaudeInstall = "Claude Code"
	CheckNameClaudeMCP     = "Claude Code MCP server"
	CheckNameClaudeSkill   = "Claude Code skill"
)

// skillSourceURL is where the skill can be fetched from by hand — the same
// address scripts/install.sh names when it finds nowhere to write one. A hint
// that said "copy skills/krowk/SKILL.md" would be addressed to somebody
// standing in this repository, which the person reading a doctor report is
// not.
const skillSourceURL = "https://github.com/krowkcom/cli/blob/main/skills/krowk/SKILL.md"

// mcpHint is scope-explicit on purpose. A bare `claude mcp add` writes local
// scope — this project only, in this user's copy of ~/.claude.json — which is
// rarely what someone fixing a doctor failure means.
const mcpHint = "Run: claude mcp add --scope user krowk -- krowk-mcp"

func init() {
	RegisterAgent(AgentInfo{
		Name:   "Claude Code",
		ID:     "claude",
		Detect: DetectClaude,
		Checks: ClaudeChecks,
	})
}

// ClaudeChecks is every Claude Code health check, in the order a person would
// want them: is it here, does it know about krowk's MCP server, does it have
// the skill. cwd is the project directory the answer is being asked about —
// both the local-scope key in ~/.claude.json and the .mcp.json that would sit
// beside the code. Empty means user scope only.
func ClaudeChecks(env Env, cwd string) []StatusCheck {
	return []StatusCheck{
		CheckClaudeInstalled(env),
		CheckClaudeMCPServer(env, cwd),
		CheckClaudeSkill(env),
	}
}

// claudeConfigDir is where Claude Code keeps its configuration directory:
// CLAUDE_CONFIG_DIR when set, since someone who moved it has said where it
// lives, and ~/.claude otherwise. This is the same rule scripts/install.sh
// follows, so the installer and the checks look in one place. Empty when env
// describes neither.
func claudeConfigDir(env Env) string {
	if env != nil {
		if dir := env("CLAUDE_CONFIG_DIR"); dir != "" {
			return filepath.Clean(dir)
		}
	}
	if home := HomeDir(env); home != "" {
		return filepath.Join(home, claudeDirName)
	}
	return ""
}

// DetectClaude reports whether Claude Code is installed. It is the install
// check's own answer, so detection and the check a person reads can never
// disagree about whether an agent is here.
func DetectClaude(env Env) bool {
	return CheckClaudeInstalled(env).Status == StatusPass
}

// FindClaudeBinary returns the path to the claude binary, or "". PATH first,
// then ~/.local/bin, which is where the official installer puts it and which a
// non-login shell's PATH often misses. Both use the same executable test, and
// it follows symlinks: the installer's ~/.local/bin/claude is one.
func FindClaudeBinary(env Env) string {
	if p := LookPath(env, "claude"); p != "" {
		return p
	}
	home := HomeDir(env)
	if home == "" {
		return ""
	}
	for _, name := range executableNames(env, "claude") {
		candidate := filepath.Join(home, ".local", "bin", name)
		if isExecutableFile(candidate) {
			return candidate
		}
	}
	return ""
}

// CheckClaudeInstalled says whether Claude Code is here, and by which of the
// two signals — the configuration directory, which is what a Claude Code that
// has ever run leaves behind, or the binary, which a fresh install has before
// it has the directory. The person reading a doctor report should not have to
// guess which one answered.
func CheckClaudeInstalled(env Env) StatusCheck {
	if dir := claudeConfigDir(env); dir != "" && isDir(dir) {
		return Pass(CheckNameClaudeInstall, "Installed ("+dir+")")
	}
	if bin := FindClaudeBinary(env); bin != "" {
		return Pass(CheckNameClaudeInstall, "Installed ("+bin+")")
	}
	return Fail(CheckNameClaudeInstall, "Not found", "Install Claude Code: https://claude.com/claude-code")
}

// CheckClaudeMCPServer says whether Claude Code can reach krowk over MCP.
// Any of the three scopes counts, because any of them is enough for the agent
// running in cwd:
//
//   - user scope, top-level "mcpServers" in ~/.claude.json — everywhere;
//   - local scope, projects["<cwd>"].mcpServers in the same file — here, and
//     the scope a bare `claude mcp add` actually writes;
//   - project scope, .mcp.json in cwd — here, for everyone who clones it.
func CheckClaudeMCPServer(env Env, cwd string) StatusCheck {
	type candidate struct {
		path     string
		cwd      string
		trusted  bool
		maxBytes int64
	}
	homeless := HomeDir(env) == ""
	var candidates []candidate
	if home := HomeDir(env); home != "" {
		candidates = append(candidates, candidate{
			path: filepath.Join(home, claudeUserConfigFile), cwd: cwd,
			trusted: true, maxBytes: maxUserConfigBytes,
		})
	}
	if dir := claudeConfigDir(env); dir != "" && dir != filepath.Join(HomeDir(env), claudeDirName) {
		// A relocated configuration directory, in case the user config
		// followed it there.
		candidates = append(candidates, candidate{
			path: filepath.Join(dir, claudeUserConfigFile), cwd: cwd,
			trusted: true, maxBytes: maxUserConfigBytes,
		})
	}
	if cwd != "" {
		candidates = append(candidates, candidate{
			path:    filepath.Join(cwd, claudeProjectConfigFile),
			trusted: false, maxBytes: maxProjectConfigBytes,
		})
	}
	var problem string
	for _, c := range candidates {
		matched, why := mcpServerRegistered(c.path, c.cwd, c.trusted, c.maxBytes)
		switch {
		case matched != "":
			return Pass(CheckNameClaudeMCP, "Registered in "+c.path+" ("+matched+")")
		case why != "" && problem == "":
			// A file that cannot be read proves nothing either way, so
			// remember why and keep looking for one that answers.
			problem = why
		}
	}

	if problem != "" {
		return Warn(CheckNameClaudeMCP, problem, mcpHint)
	}
	if homeless {
		// The user config holds two of the three scopes and was never
		// located, so "not registered" would be a claim about a file nothing
		// opened. A .mcp.json that said nothing does not answer for it.
		return Warn(CheckNameClaudeMCP, "Cannot determine where Claude Code's config lives",
			"Set HOME to the account Claude Code runs as")
	}
	return Fail(CheckNameClaudeMCP, "Not registered", mcpHint)
}

// claudeConfig is the only part of either file this cares about. Both scopes
// share the "mcpServers" shape; ~/.claude.json adds the per-project map that
// local scope is written into. Every value stays raw so that one server
// written oddly — a null, a string where an object belongs — costs that
// server rather than the whole file.
type claudeConfig struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
	Projects   map[string]json.RawMessage `json:"projects"`
}

// mcpServerRegistered reports whether path registers krowk's MCP server, for
// the project at cwd when path is the user config (pass "" to skip local
// scope). It returns the command line that matched, for the report to quote,
// and a human-readable reason the file could not answer — empty when it did.
//
// A missing file is a plain no. Nothing is matched on the name a server was
// registered under: an entry called "krowk" that launches something else is
// not krowk's MCP server, and one called anything at all that launches
// krowk-mcp is.
func mcpServerRegistered(path, cwd string, trusted bool, maxBytes int64) (matched, problem string) {
	data, err := readConfigFile(path, trusted, maxBytes)
	if err != nil {
		if isNotExist(err) {
			return "", ""
		}
		return "", "Cannot read " + path + ": " + err.Error()
	}
	var cfg claudeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", "Cannot parse " + path + ": " + err.Error()
	}
	if matched := serversLaunchKrowk(cfg.MCPServers); matched != "" {
		return matched, ""
	}
	if cwd != "" {
		want := projectDirsOf(cwd)
		// Sorted for the same reason the server list is: two keys that spell
		// the same directory must not answer differently between runs.
		keys := make([]string, 0, len(cfg.Projects))
		for key := range cfg.Projects {
			if matchesProjectDir(want, key) {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			raw := cfg.Projects[key]
			var project struct {
				MCPServers map[string]json.RawMessage `json:"mcpServers"`
			}
			if err := json.Unmarshal(raw, &project); err != nil {
				continue
			}
			if matched := serversLaunchKrowk(project.MCPServers); matched != "" {
				return matched, ""
			}
		}
	}
	return "", ""
}

// projectDirForms is the spellings of one directory a config key might use:
// as given, and as it resolves through symlinks. It is computed once per
// check, not once per key.
type projectDirForms struct {
	clean    string
	resolved string
}

// projectDirsOf resolves cwd into the forms a projects key could match.
func projectDirsOf(cwd string) projectDirForms {
	// Claude Code records absolute paths, so a relative cwd has to be made
	// absolute before it can match one.
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	forms := projectDirForms{clean: filepath.Clean(cwd)}
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		forms.resolved = filepath.Clean(resolved)
	}
	return forms
}

// matchesProjectDir reports whether a projects key names the directory the
// check is about.
//
// The key is cleaned and never resolved. Claude Code records the path it had
// already resolved, so resolving again buys nothing — and it would cost a
// great deal: a real ~/.claude.json holds hundreds of keys naming directories
// that have since been deleted, unmounted or moved onto a network share, and
// stat-ing every one of them is exactly the hang this package refuses to risk
// elsewhere. Only the one directory the caller asked about is resolved, and
// only once.
//
// Case is folded where the filesystem folds it: Windows and macOS are
// case-insensitive by default, while folding on Linux would call two different
// directories the same one.
func matchesProjectDir(want projectDirForms, key string) bool {
	key = filepath.Clean(key)
	fold := runtime.GOOS == "windows" || runtime.GOOS == "darwin"
	for _, form := range []string{want.clean, want.resolved} {
		if form == "" {
			continue
		}
		if key == form || (fold && strings.EqualFold(key, form)) {
			return true
		}
	}
	return false
}

// serversLaunchKrowk returns the command line of the first entry that launches
// krowk's MCP server, or "" if none does.
//
// Only a stdio registration can match, because krowk ships no HTTP MCP server:
// an entry with a `"type": "http"` and a URL is a known non-match, whatever it
// is called. What counts is either a command that is krowk-mcp, or a package
// runner launching the @krowk/mcp package — a package name in the arguments of
// something that is not a package runner proves nothing about what runs.
func serversLaunchKrowk(servers map[string]json.RawMessage) string {
	// Sorted, so that a config registering the server twice reports the same
	// one every run: a doctor line that changes between two identical runs
	// makes a person doubt the tool rather than the config.
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		raw := servers[name]
		// command and args are decoded separately so that one written oddly
		// — args as a string, say — does not discard the other.
		var server struct {
			Command json.RawMessage `json:"command"`
			Args    json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(raw, &server); err != nil {
			continue
		}
		command := jsonString(server.Command)
		if commandIsKrowkMCP(command) {
			return describeCommand(command, "")
		}
		if !isPackageRunner(command) {
			continue
		}
		var args []json.RawMessage
		if err := json.Unmarshal(server.Args, &args); err != nil {
			continue
		}
		for _, raw := range args {
			if arg := strings.TrimSpace(jsonString(raw)); isKrowkMCPPackage(arg) {
				return describeCommand(command, arg)
			}
		}
	}
	return ""
}

// packageRunners are the commands that run an npm package by name. Only these
// turn "@krowk/mcp" in an argument list into a claim about what launches.
// `node` is deliberately absent: node runs a file, not a package name, so
// `node @krowk/mcp` is not an invocation anything would write.
var packageRunners = map[string]bool{
	"npx": true, "bunx": true, "pnpm": true, "pnpx": true,
	"npm": true, "yarn": true,
}

// isPackageRunner reports whether command runs a package named in its args.
func isPackageRunner(command string) bool { return packageRunners[commandBase(command)] }

// isKrowkMCPPackage reports whether arg names krowk's npm package, with or
// without the version suffix npx accepts (@krowk/mcp@latest, @krowk/mcp@1.2.3).
func isKrowkMCPPackage(arg string) bool {
	rest, ok := strings.CutPrefix(arg, KrowkMCPPackage)
	return ok && (rest == "" || strings.HasPrefix(rest, "@"))
}

// describeCommand renders a matched registration for the report. Both halves
// came out of a file this package does not trust, so the result is stripped of
// anything unprintable — an escape sequence in a doctor line is a terminal
// somebody else is driving — and cut to a length that stays on one line.
func describeCommand(command, arg string) string {
	if arg != "" {
		command += " " + arg
	}
	var b strings.Builder
	runes := 0
	for _, r := range command {
		if !unicode.IsPrint(r) {
			continue
		}
		if runes == 80 {
			b.WriteRune('\u2026')
			break
		}
		b.WriteRune(r)
		runes++
	}
	if b.Len() == 0 {
		return "unnamed command"
	}
	return b.String()
}

// jsonString is raw as a string when raw is one, and "" for a number, an
// object, null or absence — none of which name a command.
func jsonString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// commandBase is the name a command would be known by: its final path element,
// and on Windows lowercased with an executable suffix removed, because there
// KROWK-MCP.EXE and krowk-mcp are one file. Nothing is stripped elsewhere: on
// a case-sensitive filesystem `krowk-mcp.py` is a different program that
// happens to be named after this one.
func commandBase(command string) string {
	base := filepath.Base(strings.TrimSpace(command))
	if runtime.GOOS != "windows" {
		return base
	}
	base = strings.ToLower(base)
	if ext := filepath.Ext(base); ext != "" && windowsExecutableExts[ext] {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

// windowsExecutableExts is the default PATHEXT, lowercased: the suffixes that
// mean "this is the executable" rather than "this is a file about it".
var windowsExecutableExts = func() map[string]bool {
	out := map[string]bool{}
	for _, ext := range strings.Split(defaultPathExt, ";") {
		if ext = strings.ToLower(strings.TrimSpace(ext)); ext != "" {
			out[ext] = true
		}
	}
	return out
}()

// commandIsKrowkMCP reports whether a command launches krowk's MCP server. The
// base name is what identifies it: an absolute path out of a version manager
// and a bare name off PATH are the same server.
func commandIsKrowkMCP(command string) bool {
	return command != "" && commandBase(command) == KrowkMCPCommand
}

// CheckClaudeSkill says whether the krowk skill is installed for Claude Code.
// It must be a regular file, by Lstat: a symlinked SKILL.md points at a file
// this never read, and what a skill file says is the whole of what it does.
// (The binary check takes the opposite line — see isExecutableFile — because
// there what matters is that something runs, not what it says.)
func CheckClaudeSkill(env Env) StatusCheck {
	dir := claudeConfigDir(env)
	if dir == "" {
		return Warn(CheckNameClaudeSkill, "Cannot determine where Claude Code's config lives",
			"Set HOME to the account Claude Code runs as")
	}
	skillDir := filepath.Join(dir, "skills", claudeSkillDirName)
	path := filepath.Join(skillDir, claudeSkillFile)
	if isRegularFile(path) {
		// A skill krowk did not write still works — the agent reads it the
		// same way — so every branch here passes. What differs is what
		// happens on the next install, and that is the part a person cannot
		// see for themselves.
		//
		// The same three questions the write gate asks come first, through
		// the same function, so this cannot promise a refresh the gate would
		// refuse: a skill directory that is a symlink, or that belongs to
		// another user, is left alone whatever marker it carries.
		if _, err := claimableDir(skillDir); err != nil {
			var unmanaged *UnmanagedError
			if errors.As(err, &unmanaged) {
				return Pass(CheckNameClaudeSkill, "Installed ("+path+") — krowk will not refresh it: the directory "+unmanaged.Reason)
			}
			return Pass(CheckNameClaudeSkill, "Installed ("+path+")")
		}
		switch {
		case markerIsOurs(skillDir):
			return Pass(CheckNameClaudeSkill, "Installed ("+path+")")
		case adoptableDir(skillDir, claudeSkillFile):
			// Nothing here but the one file a pre-marker krowk wrote, which
			// the installer adopts and marks on its next run.
			return Pass(CheckNameClaudeSkill, "Installed ("+path+"), not yet marked as krowk's — the next install will adopt it")
		default:
			return Pass(CheckNameClaudeSkill, "Installed by hand ("+path+") — krowk will not refresh it")
		}
	}

	if _, err := os.Lstat(path); err == nil {
		return Fail(CheckNameClaudeSkill, path+" is not a regular file",
			"Move it aside, then re-run the krowk installer")
	}
	return Fail(CheckNameClaudeSkill, "Not installed",
		"Re-run the krowk installer, or copy "+skillSourceURL+" into "+skillDir+"/")
}
