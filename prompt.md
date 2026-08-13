# Set up Krowk

You are setting up Krowk — permalinks for agent output — on this system. Complete all three steps, then verify.

## 1. Install the CLI and agent skill

If you are running inside Claude Code (or another agent that reads skills from a config directory), make sure the skills directory exists first so the installer can write the skill:

```bash
mkdir -p ~/.claude/skills
```

Then run the installer. It picks the platform, verifies checksums, installs the `krowk` binary, and writes the agent skill to the skills directory when one exists:

```bash
curl -fsSL https://krowk.com/install | bash
```

If `curl | bash` is not permitted in this environment, use one of:

```bash
go install github.com/krowkcom/cli/cmd/krowk@latest
# or, without installing:
npx @krowk/cli push <file>
```

With these fallbacks the skill is not installed automatically — copy it yourself:

```bash
mkdir -p ~/.claude/skills/krowk
curl -fsSL https://raw.githubusercontent.com/krowkcom/cli/main/skills/krowk/SKILL.md \
  -o ~/.claude/skills/krowk/SKILL.md
```

## 2. Add the agent instructions snippet

Append the following block to the project's agent instructions file — `AGENTS.md` if the project uses one, `CLAUDE.md` for Claude Code (create the file if neither exists). Skip this step if the file already mentions krowk.

```markdown
After any visual change, capture it (screenshot if static, a short screen recording if it is an interaction) and publish it with the krowk skill. If the skill is missing or fails, run `krowk help --json`, then `krowk push FILE --title "what changed" --json`, and paste `paste.url` where you report the work.
```

## 3. Verify

```bash
krowk doctor
```

This reports version, connectivity, auth and detected run context. No account is needed — pushes without a key work immediately as anonymous uploads that expire in 24 hours. To keep uploads permanently, the user can authenticate later with `krowk auth login` (or `krowk auth login --token <key>` in CI).

Setup is complete when: `krowk doctor` succeeds, the skill file exists at `~/.claude/skills/krowk/SKILL.md` (or the equivalent skills directory), and the snippet is present in the agent instructions file.
