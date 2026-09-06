package harness

import (
	"os"
	"path/filepath"
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
	if check.Hint != "Run: claude mcp add krowk -- krowk-mcp" {
		t.Fatalf("hint = %q", check.Hint)
	}
}

func TestCheckClaudeMCPServerFailsWhenNothingIsConfiguredAtAll(t *testing.T) {
	check := CheckClaudeMCPServer(homeEnv(t.TempDir()), t.TempDir())
	if check.Status != StatusFail {
		t.Fatalf("check = %+v, want fail", check)
	}
}

func TestCheckClaudeMCPServerWarnsWhenTheConfigCannotBeRead(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	writeFile(t, path, "{not json")

	check := CheckClaudeMCPServer(homeEnv(home), "")
	if check.Status != StatusWarn {
		t.Fatalf("check = %+v, want warn", check)
	}
	if !strings.Contains(check.Message, path) {
		t.Fatalf("warn message %q does not name the file", check.Message)
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
	if check.Status != StatusPass || !strings.Contains(check.Message, "Linked") {
		t.Fatalf("check = %+v, want a pass saying Linked", check)
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
