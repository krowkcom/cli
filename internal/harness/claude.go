package harness

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// The names Claude Code itself uses. User scope lives in ~/.claude.json, a
// single file holding everything; project scope lives in a .mcp.json beside
// the code, which is the form the README documents.
const (
	claudeUserConfigFile    = ".claude.json"
	claudeProjectConfigFile = ".mcp.json"
	claudeDirName           = ".claude"

	// KrowkMCPServerName is the key krowk's MCP server is registered under —
	// what `claude mcp add krowk -- krowk-mcp` writes.
	KrowkMCPServerName = "krowk"
)

// Check names, so consumers can match on them without repeating strings.
const (
	CheckNameClaudeInstall = "Claude Code"
	CheckNameClaudeMCP     = "Claude Code MCP server"
	CheckNameClaudeSkill   = "Claude Code skill"
)

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
// the skill. cwd is the directory a project-scoped .mcp.json would sit in;
// empty means only user scope is considered.
func ClaudeChecks(env Env, cwd string) []StatusCheck {
	return []StatusCheck{
		CheckClaudeInstalled(env),
		CheckClaudeMCPServer(env, cwd),
		CheckClaudeSkill(env),
	}
}

// DetectClaude reports whether Claude Code is installed: a ~/.claude directory
// first, because that is what a Claude Code that has ever run leaves behind,
// and a binary second, because a fresh install has one before it has the other.
func DetectClaude(env Env) bool {
	if home := HomeDir(env); home != "" && isDir(filepath.Join(home, claudeDirName)) {
		return true
	}
	return FindClaudeBinary(env) != ""
}

// FindClaudeBinary returns the path to the claude binary, or "". PATH first,
// then ~/.local/bin, which is where the official installer puts it and which a
// non-login shell's PATH often misses.
func FindClaudeBinary(env Env) string {
	if p := LookPath(env, "claude"); p != "" {
		return p
	}
	home := HomeDir(env)
	if home == "" {
		return ""
	}
	candidate := filepath.Join(home, ".local", "bin", "claude")
	if isRegularFile(candidate) {
		return candidate
	}
	return ""
}

// CheckClaudeInstalled says whether Claude Code is here, and by which of the
// two signals — the person reading a doctor report should not have to guess
// which one answered.
func CheckClaudeInstalled(env Env) StatusCheck {
	if home := HomeDir(env); home != "" {
		dir := filepath.Join(home, claudeDirName)
		if isDir(dir) {
			return Pass(CheckNameClaudeInstall, "Installed ("+dir+")")
		}
	}
	if bin := FindClaudeBinary(env); bin != "" {
		return Pass(CheckNameClaudeInstall, "Installed ("+bin+")")
	}
	return Fail(CheckNameClaudeInstall, "Not found", "Install Claude Code: https://claude.com/claude-code")
}

// CheckClaudeMCPServer says whether Claude Code can reach krowk over MCP.
// Either scope counts: a user-scope entry in ~/.claude.json works everywhere,
// a project-scope .mcp.json works here, and either one is enough for the agent
// running in this checkout.
func CheckClaudeMCPServer(env Env, cwd string) StatusCheck {
	var candidates []string
	if home := HomeDir(env); home != "" {
		candidates = append(candidates, filepath.Join(home, claudeUserConfigFile))
	}
	if cwd != "" {
		candidates = append(candidates, filepath.Join(cwd, claudeProjectConfigFile))
	}

	var unreadable string
	for _, path := range candidates {
		found, err := mcpServerRegistered(path)
		switch {
		case found:
			return Pass(CheckNameClaudeMCP, "Registered in "+path)
		case err != nil && unreadable == "":
			// A file that exists but cannot be read proves nothing either
			// way, so remember it and keep looking for one that does.
			unreadable = path
		}
	}

	if unreadable != "" {
		return Warn(CheckNameClaudeMCP, "Cannot read "+unreadable,
			"Check that it is valid JSON, then: claude mcp add krowk -- krowk-mcp")
	}
	return Fail(CheckNameClaudeMCP, "Not registered", "Run: claude mcp add krowk -- krowk-mcp")
}

// claudeMCPConfig is the only part of either file this cares about. Both
// scopes share the shape, so both parse into it.
type claudeMCPConfig struct {
	MCPServers map[string]struct {
		Command string `json:"command"`
	} `json:"mcpServers"`
}

// mcpServerRegistered reports whether path registers krowk's MCP server. A
// missing file is a plain no; an unreadable or unparseable one is an error,
// because "krowk is not in there" is not something an unread file can say.
// The key is the usual name, but the command is what actually matters: a
// server registered under another name still launches krowk-mcp.
func mcpServerRegistered(path string) (bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: a path the caller named
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	var cfg claudeMCPConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false, err
	}
	for name, server := range cfg.MCPServers {
		if name == KrowkMCPServerName || commandIsKrowkMCP(server.Command) {
			return true, nil
		}
	}
	return false, nil
}

// CheckClaudeSkill says whether the krowk skill is installed for Claude Code.
// It must be a regular file: a symlinked SKILL.md points at something this
// never read, and a check that cannot see what it is vouching for should not
// vouch for it.
func CheckClaudeSkill(env Env) StatusCheck {
	home := HomeDir(env)
	if home == "" {
		return Warn(CheckNameClaudeSkill, "Cannot determine the home directory",
			"Set HOME to the account Claude Code runs as")
	}
	dir := filepath.Join(home, claudeDirName, "skills", "krowk")
	path := filepath.Join(dir, "SKILL.md")
	if isRegularFile(path) {
		return Pass(CheckNameClaudeSkill, "Linked ("+path+")")
	}
	if _, err := os.Lstat(path); err == nil {
		return Fail(CheckNameClaudeSkill, path+" is not a regular file",
			"Move it aside, then reinstall the krowk skill")
	}
	return Fail(CheckNameClaudeSkill, "Not installed",
		"Install the krowk plugin, or copy skills/krowk/SKILL.md into "+dir+"/")
}
