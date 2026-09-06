package harness

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestDetectClaudeFindsTheClaudeDirectory(t *testing.T) {
	home := t.TempDir()
	mkdirAll(t, filepath.Join(home, ".claude"))

	if !DetectClaude(homeEnv(home)) {
		t.Fatal("a ~/.claude directory was not detected")
	}
	check := CheckClaudeInstalled(homeEnv(home))
	if check.Status != StatusPass {
		t.Fatalf("install check = %+v, want pass", check)
	}
	if want := filepath.Join(home, ".claude"); !strings.Contains(check.Message, want) {
		t.Fatalf("install check message %q does not name %q", check.Message, want)
	}
}

func TestDetectClaudeFallsBackToTheBinaryOnPath(t *testing.T) {
	home := t.TempDir()
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "claude"))

	env := envFrom(map[string]string{"HOME": home, "USERPROFILE": home, "PATH": binDir})
	if !DetectClaude(env) {
		t.Fatal("a claude binary on PATH was not detected")
	}
	check := CheckClaudeInstalled(env)
	if check.Status != StatusPass || !strings.Contains(check.Message, binDir) {
		t.Fatalf("install check = %+v, want a pass naming %s", check, binDir)
	}
}

func TestDetectClaudeFallsBackToLocalBin(t *testing.T) {
	home := t.TempDir()
	mkdirAll(t, filepath.Join(home, ".local", "bin"))
	writeExecutable(t, filepath.Join(home, ".local", "bin", "claude"))

	if !DetectClaude(homeEnv(home)) {
		t.Fatal("~/.local/bin/claude was not detected")
	}
}

func TestDetectClaudeFindsNothingInAnEmptyHome(t *testing.T) {
	env := envFrom(map[string]string{"HOME": t.TempDir(), "USERPROFILE": t.TempDir()})
	if DetectClaude(env) {
		t.Fatal("an empty home detected Claude Code anyway")
	}
	check := CheckClaudeInstalled(env)
	if check.Status != StatusFail || check.Hint == "" {
		t.Fatalf("install check = %+v, want a fail with a hint", check)
	}
}

func TestCheckClaudeMCPServerPassesForEitherScope(t *testing.T) {
	registered := `{"mcpServers":{"krowk":{"command":"krowk-mcp"}}}`

	t.Run("user scope", func(t *testing.T) {
		home := t.TempDir()
		writeFile(t, filepath.Join(home, ".claude.json"), registered)
		check := CheckClaudeMCPServer(homeEnv(home), t.TempDir())
		if check.Status != StatusPass {
			t.Fatalf("check = %+v, want pass", check)
		}
	})

	t.Run("project scope", func(t *testing.T) {
		home, cwd := t.TempDir(), t.TempDir()
		writeFile(t, filepath.Join(cwd, ".mcp.json"), registered)
		check := CheckClaudeMCPServer(homeEnv(home), cwd)
		if check.Status != StatusPass || !strings.Contains(check.Message, ".mcp.json") {
			t.Fatalf("check = %+v, want a pass naming .mcp.json", check)
		}
	})

	t.Run("registered under another name", func(t *testing.T) {
		home := t.TempDir()
		writeFile(t, filepath.Join(home, ".claude.json"),
			`{"mcpServers":{"screenshots":{"command":"/opt/bin/krowk-mcp"}}}`)
		check := CheckClaudeMCPServer(homeEnv(home), "")
		if check.Status != StatusPass {
			t.Fatalf("check = %+v, want pass — the command is krowk-mcp", check)
		}
	})
}

func TestCheckClaudeMCPServerFailsWhenNeitherScopeHasIt(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"other":{"command":"other-mcp"}}}`)

	check := CheckClaudeMCPServer(homeEnv(home), cwd)
	if check.Status != StatusFail {
		t.Fatalf("check = %+v, want fail", check)
	}
	if check.Hint != mcpHint {
		t.Fatalf("hint = %q", check.Hint)
	}
}

func TestCheckClaudeMCPServerFailsWhenNothingIsConfiguredAtAll(t *testing.T) {
	check := CheckClaudeMCPServer(homeEnv(t.TempDir()), t.TempDir())
	if check.Status != StatusFail {
		t.Fatalf("check = %+v, want fail", check)
	}
}

func TestCheckClaudeMCPServerWarnsWhenTheConfigIsNotValidJSON(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	writeFile(t, path, "{not json")

	check := CheckClaudeMCPServer(homeEnv(home), "")
	if check.Status != StatusWarn {
		t.Fatalf("check = %+v, want warn", check)
	}
	if !strings.Contains(check.Message, path) || !strings.Contains(check.Message, "Cannot parse") {
		t.Fatalf("warn message %q does not say it could not parse the file", check.Message)
	}
}

func TestCheckClaudeMCPServerPrefersAWorkingScopeOverAnUnreadableOne(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(home, ".claude.json"), "{not json")
	writeFile(t, filepath.Join(cwd, ".mcp.json"), `{"mcpServers":{"krowk":{"command":"krowk-mcp"}}}`)

	if check := CheckClaudeMCPServer(homeEnv(home), cwd); check.Status != StatusPass {
		t.Fatalf("check = %+v, want pass — the project scope answers", check)
	}
}

func TestCheckClaudeSkillPassesForARegularSkillFile(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "skills", "krowk")
	mkdirAll(t, dir)
	writeFile(t, filepath.Join(dir, "SKILL.md"), "# krowk\n")

	check := CheckClaudeSkill(homeEnv(home))
	if check.Status != StatusPass || !strings.Contains(check.Message, "Installed") {
		t.Fatalf("check = %+v, want a pass saying Installed", check)
	}
}

func TestCheckClaudeSkillFailsWhenItIsMissing(t *testing.T) {
	check := CheckClaudeSkill(homeEnv(t.TempDir()))
	if check.Status != StatusFail {
		t.Fatalf("check = %+v, want fail", check)
	}
	if !strings.Contains(check.Hint, "skills/krowk/SKILL.md") {
		t.Fatalf("hint %q does not say where the skill comes from", check.Hint)
	}
	if strings.Contains(check.Hint, "plugin") {
		t.Fatalf("hint %q suggests a plugin this check never looked at", check.Hint)
	}
}

func TestCheckClaudeSkillFailsWhenSomethingElseOccupiesThePath(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "skills", "krowk")
	mkdirAll(t, filepath.Join(dir, "SKILL.md")) // a directory in the file's name

	check := CheckClaudeSkill(homeEnv(home))
	if check.Status != StatusFail || !strings.Contains(check.Message, "not a regular file") {
		t.Fatalf("check = %+v, want a fail about it not being a regular file", check)
	}
}

func TestCheckClaudeSkillWarnsWithoutAHomeDirectory(t *testing.T) {
	check := CheckClaudeSkill(envFrom(nil))
	if check.Status != StatusWarn {
		t.Fatalf("check = %+v, want warn", check)
	}
}

func TestClaudeChecksAnswerEveryQuestionInOrder(t *testing.T) {
	home := t.TempDir()
	mkdirAll(t, filepath.Join(home, ".claude"))

	checks := ClaudeChecks(homeEnv(home), t.TempDir())
	names := []string{CheckNameClaudeInstall, CheckNameClaudeMCP, CheckNameClaudeSkill}
	if len(checks) != len(names) {
		t.Fatalf("got %d checks, want %d", len(checks), len(names))
	}
	for i, want := range names {
		if checks[i].Name != want {
			t.Fatalf("check %d is %q, want %q", i, checks[i].Name, want)
		}
	}
	if got := Worst(checks); got != StatusFail {
		t.Fatalf("Worst = %q, want fail — neither the MCP server nor the skill is installed", got)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckClaudeMCPServerPassesForLocalScope(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	// What a bare `claude mcp add krowk -- krowk-mcp` actually writes.
	writeFile(t, filepath.Join(home, ".claude.json"), `{"projects":{`+
		strconv.Quote(cwd)+`:{"mcpServers":{"krowk":{"command":"krowk-mcp"}}}}}`)

	if check := CheckClaudeMCPServer(homeEnv(home), cwd); check.Status != StatusPass {
		t.Fatalf("check = %+v, want pass — local scope registers it for this project", check)
	}
	// The same file says nothing about a different project.
	if check := CheckClaudeMCPServer(homeEnv(home), t.TempDir()); check.Status != StatusFail {
		t.Fatalf("check = %+v, want fail — that project has no registration", check)
	}
}

func TestCheckClaudeMCPServerIgnoresAnEntryNamedKrowkThatRunsSomethingElse(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"krowk":{"command":"/bin/false"}}}`)
	if check := CheckClaudeMCPServer(homeEnv(home), ""); check.Status != StatusFail {
		t.Fatalf("check = %+v, want fail — the name is not the server", check)
	}
}

func TestCheckClaudeMCPServerSurvivesOddServerEntries(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude.json"),
		`{"mcpServers":{"krowk":null,"broken":"nonsense","numeric":{"command":7},`+
			`"real":{"command":"npx","args":["-y","@krowk/mcp"]}}}`)
	if check := CheckClaudeMCPServer(homeEnv(home), ""); check.Status != StatusPass {
		t.Fatalf("check = %+v, want pass — one odd entry must not hide a working one", check)
	}
}

func TestCheckClaudeMCPServerWarnsWhenTheConfigIsNotARegularFile(t *testing.T) {
	// Standing in for the FIFO an attacker-authored checkout could carry: a
	// directory is the shape a test can create portably, and the check must
	// refuse to read either.
	cwd := t.TempDir()
	mkdirAll(t, filepath.Join(cwd, ".mcp.json"))

	check := CheckClaudeMCPServer(envFrom(nil), cwd)
	if check.Status != StatusWarn || !strings.Contains(check.Message, "not a regular file") {
		t.Fatalf("check = %+v, want a warn about it not being a regular file", check)
	}
}

func TestCheckClaudeMCPServerWarnsWhenTheConfigIsUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits do not stop this user")
	}
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	writeFile(t, path, `{"mcpServers":{}}`)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}

	check := CheckClaudeMCPServer(homeEnv(home), "")
	if check.Status != StatusWarn || !strings.Contains(check.Message, "Cannot read") {
		t.Fatalf("check = %+v, want a warn saying it could not read the file", check)
	}
}

func TestCheckClaudeMCPServerWarnsWithNowhereToLook(t *testing.T) {
	check := CheckClaudeMCPServer(envFrom(nil), "")
	if check.Status != StatusWarn {
		t.Fatalf("check = %+v, want warn — nothing was looked at", check)
	}
}

func TestCommandIsKrowkMCPAcceptsEverySpellingOfTheSameServer(t *testing.T) {
	yes := []string{"krowk-mcp", "/opt/bin/krowk-mcp", "KROWK-MCP.EXE", "krowk-mcp.cmd", "  krowk-mcp  "}
	for _, command := range yes {
		if !commandIsKrowkMCP(command) {
			t.Errorf("commandIsKrowkMCP(%q) = false, want true", command)
		}
	}
	no := []string{"", "krowk", "krowk-mcpx", "/bin/false", "mcp"}
	for _, command := range no {
		if commandIsKrowkMCP(command) {
			t.Errorf("commandIsKrowkMCP(%q) = true, want false", command)
		}
	}
}

func TestCheckClaudeHonoursCLAUDECONFIGDIR(t *testing.T) {
	home, config := t.TempDir(), t.TempDir()
	skillDir := filepath.Join(config, "skills", "krowk")
	mkdirAll(t, skillDir)
	writeFile(t, filepath.Join(skillDir, "SKILL.md"), "# krowk\n")

	env := envFrom(map[string]string{"HOME": home, "USERPROFILE": home, "CLAUDE_CONFIG_DIR": config})
	if !DetectClaude(env) {
		t.Fatal("a relocated config directory was not detected")
	}
	if check := CheckClaudeInstalled(env); check.Status != StatusPass || !strings.Contains(check.Message, config) {
		t.Fatalf("install check = %+v, want a pass naming %s", check, config)
	}
	if check := CheckClaudeSkill(env); check.Status != StatusPass {
		t.Fatalf("skill check = %+v, want pass — the skill is in the relocated directory", check)
	}
	// ~/.claude.json does not move with it.
	writeFile(t, filepath.Join(home, ".claude.json"), `{"mcpServers":{"krowk":{"command":"krowk-mcp"}}}`)
	if check := CheckClaudeMCPServer(env, ""); check.Status != StatusPass {
		t.Fatalf("mcp check = %+v, want pass — the user config stays in HOME", check)
	}
}

func TestCheckClaudeSkillFailsForASymlinkedSkillFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege this test should not assume")
	}
	home := t.TempDir()
	skillDir := filepath.Join(home, ".claude", "skills", "krowk")
	mkdirAll(t, skillDir)
	target := filepath.Join(home, "elsewhere.md")
	writeFile(t, target, "# something never inspected\n")
	if err := os.Symlink(target, filepath.Join(skillDir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	check := CheckClaudeSkill(homeEnv(home))
	if check.Status != StatusFail || !strings.Contains(check.Message, "not a regular file") {
		t.Fatalf("check = %+v, want a fail: a symlink points somewhere this never read", check)
	}
}

func TestFindClaudeBinaryFollowsTheInstallersSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege this test should not assume")
	}
	home := t.TempDir()
	real := filepath.Join(t.TempDir(), "claude")
	writeExecutable(t, real)
	mkdirAll(t, filepath.Join(home, ".local", "bin"))
	link := filepath.Join(home, ".local", "bin", "claude")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if got := FindClaudeBinary(homeEnv(home)); got != link {
		t.Fatalf("FindClaudeBinary = %q, want the symlink at %s", got, link)
	}
}
