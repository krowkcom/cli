package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// The names Claude Code itself uses.
const (
	// claudeUserConfigFile holds user- and local-scope MCP registrations. It
	// stays at $HOME/.claude.json even when CLAUDE_CONFIG_DIR moves the
	// configuration directory: Claude Code keeps this one file in the home
	// directory regardless, so it is resolved from HomeDir and not from
	// claudeConfigDir.
	claudeUserConfigFile = ".claude.json"
	// claudeProjectConfigFile is the checked-in, project-scope form the README
	// documents. It is written by whoever wrote the checkout, so it is read
	// defensively.
	claudeProjectConfigFile = ".mcp.json"
	claudeDirName           = ".claude"

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

// DetectClaude reports whether Claude Code is installed: the configuration
// directory first, because that is what a Claude Code that has ever run leaves
// behind, and a binary second, because a fresh install has one before it has
// the other.
func DetectClaude(env Env) bool {
	if dir := claudeConfigDir(env); dir != "" && isDir(dir) {
		return true
	}
	return FindClaudeBinary(env) != ""
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
// two signals — the person reading a doctor report should not have to guess
// which one answered.
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
		path string
		cwd  string
	}
	var candidates []candidate
	if home := HomeDir(env); home != "" {
		candidates = append(candidates, candidate{filepath.Join(home, claudeUserConfigFile), cwd})
	}
	if cwd != "" {
		candidates = append(candidates, candidate{filepath.Join(cwd, claudeProjectConfigFile), ""})
	}
	if len(candidates) == 0 {
		// Neither a home nor a working directory: there is nowhere a
		// registration could be, so there is nothing to report either way.
		// Asserting "not registered" here would be asserting a negative
		// nothing was ever looked for.
		return Warn(CheckNameClaudeMCP, "Cannot determine where Claude Code's config lives",
			"Set HOME to the account Claude Code runs as")
	}

	var problem string
	for _, c := range candidates {
		found, why := mcpServerRegistered(c.path, c.cwd)
		switch {
		case found:
			return Pass(CheckNameClaudeMCP, "Registered in "+c.path)
		case why != "" && problem == "":
			// A file that cannot be read proves nothing either way, so
			// remember why and keep looking for one that answers.
			problem = why
		}
	}

	if problem != "" {
		return Warn(CheckNameClaudeMCP, problem, mcpHint)
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
// scope). The second result is a human-readable reason the file could not
// answer, empty when it did.
//
// A missing file is a plain no. Nothing is matched on the name a server was
// registered under: an entry called "krowk" that launches something else is
// not krowk's MCP server, and one called anything at all that launches
// krowk-mcp is.
func mcpServerRegistered(path, cwd string) (bool, string) {
	data, err := readConfigFile(path)
	if err != nil {
		if isNotExist(err) {
			return false, ""
		}
		return false, "Cannot read " + path + ": " + err.Error()
	}
	var cfg claudeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false, "Cannot parse " + path + ": " + err.Error()
	}
	if serversLaunchKrowk(cfg.MCPServers) {
		return true, ""
	}
	if cwd != "" {
		want := filepath.Clean(cwd)
		for key, raw := range cfg.Projects {
			if filepath.Clean(key) != want {
				continue
			}
			var project struct {
				MCPServers map[string]json.RawMessage `json:"mcpServers"`
			}
			if err := json.Unmarshal(raw, &project); err != nil {
				continue
			}
			if serversLaunchKrowk(project.MCPServers) {
				return true, ""
			}
		}
	}
	return false, ""
}

// serversLaunchKrowk reports whether any entry launches krowk's MCP server.
func serversLaunchKrowk(servers map[string]json.RawMessage) bool {
	for _, raw := range servers {
		var server struct {
			Command json.RawMessage   `json:"command"`
			Args    []json.RawMessage `json:"args"`
		}
		// A malformed entry is skipped, not fatal: it is one broken server in
		// somebody's config, and it is certainly not a working krowk.
		if err := json.Unmarshal(raw, &server); err != nil {
			continue
		}
		if commandIsKrowkMCP(jsonString(server.Command)) {
			return true
		}
		for _, raw := range server.Args {
			arg := strings.TrimSpace(jsonString(raw))
			if arg == KrowkMCPPackage || commandIsKrowkMCP(arg) {
				return true
			}
		}
	}
	return false
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

// commandIsKrowkMCP reports whether a command line launches krowk's MCP
// server. The base name is what identifies it: an absolute path out of a
// version manager, a bare name off PATH and a Windows krowk-mcp.cmd are all
// the same server. Comparison is case-insensitive because the filesystems
// that produce those suffixes are.
func commandIsKrowkMCP(command string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(command)))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return base == KrowkMCPCommand
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
	skillDir := filepath.Join(dir, "skills", "krowk")
	path := filepath.Join(skillDir, "SKILL.md")
	if isRegularFile(path) {
		return Pass(CheckNameClaudeSkill, "Installed ("+path+")")
	}
	if _, err := os.Lstat(path); err == nil {
		return Fail(CheckNameClaudeSkill, path+" is not a regular file",
			"Move it aside, then copy skills/krowk/SKILL.md into "+skillDir+"/")
	}
	return Fail(CheckNameClaudeSkill, "Not installed",
		"Copy skills/krowk/SKILL.md into "+skillDir+"/")
}
