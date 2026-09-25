//! Instruction files, read hierarchically (R-COMPAT-1): the person's own
//! first (krowk's `AGENTS.md` in its config directory, then Claude Code's
//! user `CLAUDE.md`), then every directory from the repository's root down
//! to the working directory, and in each one `AGENTS.md` first, then
//! `CLAUDE.md` and `CLAUDE.local.md`, then Cursor's `.cursor/rules` and
//! `.cursorrules`. They reach the model in that order, broadest first, and
//! it is told that where two disagree the later — the deeper — one wins.
//!
//! A Cursor rule is taken as Cursor takes it: one marked `alwaysApply`, or
//! with no front matter, is included whole; one that applies to some files
//! (`globs`) or that the agent is to fetch when relevant (`description`) is
//! listed by its path and what it says it is for, to be read when it
//! applies. Directories below the working directory, and sibling ones, are
//! not read: their files are about code this session is not in.
//!
//! Each file is capped at 64 KB and all of them at 192 KB, so one large
//! file cannot crowd the conversation out.

use crate::permissions::Config;
use std::path::{Path, PathBuf};

/// One instruction file, as it reaches the model.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Instruction {
    pub path: PathBuf,
    pub text: String,
    /// A rule only listed, not included: what it says it is for.
    pub listed: Option<String>,
}

const FILE_CAP: usize = 64 << 10;
const TOTAL_CAP: usize = 192 << 10;

fn read_capped(p: &Path) -> Option<String> {
    let raw = std::fs::read(p).ok()?;
    if raw.contains(&0) {
        return None;
    }
    let text = String::from_utf8_lossy(&raw[..raw.len().min(FILE_CAP)]).into_owned();
    let text = if raw.len() > FILE_CAP { format!("{text}\n… ({} is longer; the rest was left out)", p.display()) } else { text };
    (!text.trim().is_empty()).then_some(text)
}

/// Splits `---` front matter from a body. Returns (the front matter's
/// `key: value` pairs, the body).
pub(crate) fn front_matter(text: &str) -> (Vec<(String, String)>, &str) {
    let t = text.strip_prefix('\u{feff}').unwrap_or(text);
    let Some(rest) = t.strip_prefix("---").and_then(|r| r.strip_prefix('\n').or_else(|| r.strip_prefix("\r\n"))) else { return (Vec::new(), t) };
    let Some(end) = rest.find("\n---") else { return (Vec::new(), t) };
    let head = &rest[..end];
    let body = rest[end + 4..].trim_start_matches(['\r', '\n']);
    (pairs(head), body)
}

/// The `key: value` pairs of YAML front matter, as far as skills and rules
/// use it: plain and quoted scalars, and the `>` (folded) and `|` (literal)
/// block scalars a long `description` is often written in, with their
/// `-`/`+` chomping. Nested maps and lists are skipped.
fn pairs(head: &str) -> Vec<(String, String)> {
    let lines: Vec<&str> = head.lines().collect();
    let mut out = Vec::new();
    let mut i = 0;
    while i < lines.len() {
        let line = lines[i];
        i += 1;
        if line.starts_with([' ', '\t']) {
            continue;
        }
        let Some((k, v)) = line.split_once(':') else { continue };
        let v = v.trim();
        let block = v.chars().next().filter(|c| *c == '>' || *c == '|') ;
        let value = match block {
            Some(style) if v[1..].chars().all(|c| matches!(c, '-' | '+' | '0'..='9')) => {
                let mut body = Vec::new();
                while i < lines.len() && (lines[i].trim().is_empty() || lines[i].starts_with([' ', '\t'])) {
                    body.push(lines[i].trim());
                    i += 1;
                }
                while body.last().is_some_and(|l| l.is_empty()) {
                    body.pop();
                }
                if style == '|' { body.join("\n") } else { body.join(" ").split_whitespace().collect::<Vec<_>>().join(" ") }
            }
            _ => {
                // A plain scalar may run on over indented lines.
                let mut v = v.trim_matches('"').trim_matches('\'').to_string();
                while i < lines.len() && lines[i].starts_with([' ', '\t']) && !lines[i].trim().is_empty() && !lines[i].trim_start().starts_with('-') {
                    v.push(' ');
                    v.push_str(lines[i].trim());
                    i += 1;
                }
                v
            }
        };
        out.push((k.trim().to_string(), value));
    }
    out
}

fn cursor_rule(p: &Path, out: &mut Vec<Instruction>) {
    let Some(text) = read_capped(p) else { return };
    let (fm, body) = front_matter(&text);
    let get = |k: &str| fm.iter().find(|(key, _)| key == k).map(|(_, v)| v.as_str()).unwrap_or_default();
    if fm.is_empty() || get("alwaysApply") == "true" {
        if !body.trim().is_empty() {
            out.push(Instruction { path: p.to_path_buf(), text: body.to_string(), listed: None });
        }
        return;
    }
    let what = match (get("description"), get("globs")) {
        ("", "") => return,
        (d, "") => d.to_string(),
        ("", g) => format!("applies to files matching {g}"),
        (d, g) => format!("{d} (files matching {g})"),
    };
    out.push(Instruction { path: p.to_path_buf(), text: String::new(), listed: Some(what) });
}

/// `.cursor/rules`, nested directories included, in name order.
fn cursor_rules(dir: &Path, out: &mut Vec<Instruction>, depth: usize) {
    let Ok(rd) = std::fs::read_dir(dir) else { return };
    let mut entries: Vec<PathBuf> = rd.flatten().map(|e| e.path()).collect();
    entries.sort();
    for p in entries {
        if p.is_dir() && depth < 4 {
            cursor_rules(&p, out, depth + 1);
        } else if p.extension().is_some_and(|e| e == "mdc" || e == "md") {
            cursor_rule(&p, out);
        }
    }
}

/// Every instruction that applies in `cwd`, broadest first.
pub fn discover(cfg: &Config, cwd: &Path) -> Vec<Instruction> {
    let mut out = Vec::new();
    let add = |p: PathBuf, out: &mut Vec<Instruction>| {
        if let Some(text) = read_capped(&p) {
            out.push(Instruction { path: p, text, listed: None });
        }
    };
    if let Some(d) = &cfg.krowk_dir {
        add(d.join("AGENTS.md"), &mut out);
    }
    if let Some(d) = cfg.claude_home() {
        add(d.join("CLAUDE.md"), &mut out);
    }
    let root = crate::trust::root(cwd);
    let personal = out.len();
    for dir in crate::permissions::settings::chain(&root, cwd) {
        for name in ["AGENTS.md", "CLAUDE.md", "CLAUDE.local.md"] {
            add(dir.join(name), &mut out);
        }
        cursor_rules(&dir.join(".cursor/rules"), &mut out, 0);
        add(dir.join(".cursorrules"), &mut out);
    }
    // A repository's file that is a link out of the repository is not
    // read: `CLAUDE.md -> ~/.ssh/id_rsa` would put a secret in front of the
    // model without anyone asking.
    let mut i = 0;
    out.retain(|ins| {
        i += 1;
        i <= personal || ins.path.canonicalize().is_ok_and(|c| c.starts_with(&root))
    });
    // One file reached twice (a home that is also the repository root) is
    // read once, where it first applied.
    let mut seen = Vec::new();
    out.retain(|i| {
        let c = i.path.canonicalize().unwrap_or_else(|_| i.path.clone());
        let fresh = !seen.contains(&c);
        seen.push(c);
        fresh
    });
    let mut total = 0;
    out.retain(|i| {
        total += i.text.len();
        total <= TOTAL_CAP
    });
    out
}

/// The instructions as the system prompt carries them.
pub fn render(list: &[Instruction]) -> String {
    if list.is_empty() {
        return String::new();
    }
    let mut s = String::from("\n\nInstructions from the person's settings and the repository follow, broadest first. Where two disagree, the later one — the deeper directory — wins.\n");
    for i in list.iter().filter(|i| i.listed.is_none()) {
        s.push_str(&format!("\n<instructions path=\"{}\">\n{}\n</instructions>\n", i.path.display(), i.text.trim_end()));
    }
    let listed: Vec<&Instruction> = list.iter().filter(|i| i.listed.is_some()).collect();
    if !listed.is_empty() {
        s.push_str("\nMore rules, to read when they apply:\n");
        for i in listed {
            s.push_str(&format!("- {}: {}\n", i.path.display(), i.listed.as_deref().unwrap_or_default()));
        }
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn r_compat_1_instructions_are_read_from_the_root_down_agents_first_and_deeper_wins() {
        let base = std::env::temp_dir().join(format!("krowk-instructions-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&base);
        std::fs::create_dir_all(&base).unwrap();
        // Canonical, as the repository root is found: macOS's temporary
        // directory is a symlink into /private.
        let base = base.canonicalize().unwrap();
        let repo = base.join("repo");
        for d in [".git", "app/web/deep", "other", ".cursor/rules/nested", "app/.cursor/rules"] {
            std::fs::create_dir_all(repo.join(d)).unwrap();
        }
        let w = |p: &str, s: &str| std::fs::write(repo.join(p), s).unwrap();
        w("AGENTS.md", "root agents: use tabs");
        w("CLAUDE.md", "root claude");
        w("app/CLAUDE.md", "app claude: use spaces");
        w("app/AGENTS.md", "app agents");
        w("app/web/CLAUDE.local.md", "web local");
        w("app/web/deep/AGENTS.md", "below the working directory");
        w("other/AGENTS.md", "a sibling");
        w(".cursor/rules/always.mdc", "---\ndescription: style\nalwaysApply: true\n---\nroot cursor always");
        w(".cursor/rules/nested/ts.mdc", "---\ndescription: TypeScript rules\nglobs: \"*.ts\"\nalwaysApply: false\n---\nnot inlined");
        w("app/.cursor/rules/plain.md", "app cursor plain");
        w("app/.cursorrules", "app legacy cursorrules");
        std::fs::write(base.join("secret"), "a secret outside the repository").unwrap();
        #[cfg(unix)]
        std::os::unix::fs::symlink(base.join("secret"), repo.join("app/web/AGENTS.md")).unwrap();
        let home = base.join("home/.claude");
        std::fs::create_dir_all(&home).unwrap();
        std::fs::write(home.join("CLAUDE.md"), "user claude").unwrap();
        let cfg = Config { claude_dir: Some(home.clone()), ..Config::default() };
        let found = discover(&cfg, &repo.join("app/web"));
        let order: Vec<String> = found.iter().map(|i| i.path.strip_prefix(&base).unwrap().display().to_string()).collect();
        assert_eq!(
            order,
            [
                "home/.claude/CLAUDE.md",
                "repo/AGENTS.md",
                "repo/CLAUDE.md",
                "repo/.cursor/rules/always.mdc",
                "repo/.cursor/rules/nested/ts.mdc",
                "repo/app/AGENTS.md",
                "repo/app/CLAUDE.md",
                "repo/app/.cursor/rules/plain.md",
                "repo/app/.cursorrules",
                "repo/app/web/CLAUDE.local.md",
            ],
            "the person's first, then root down to the working directory, AGENTS.md before CLAUDE.md before Cursor's; nothing below or beside"
        );
        let prompt = render(&found);
        let at = |s: &str| prompt.find(s).unwrap_or_else(|| panic!("{s} missing: {prompt}"));
        assert!(at("root agents: use tabs") < at("app claude: use spaces"), "the deeper file comes later, and the prompt says later wins");
        assert!(prompt.contains("the later one — the deeper directory — wins"));
        assert!(prompt.contains("root cursor always") && !prompt.contains("not inlined") && prompt.contains("TypeScript rules (files matching *.ts)"));
        assert!(!prompt.contains("a secret outside"), "a repository's instruction file that links out of it is not read");
        // A description in a YAML block scalar, as long ones are written.
        let (fm, body) = front_matter("---\nname: notes\ndescription: >-\n  Write the release\n  notes for a tag\nother: |\n  a\n  b\n---\nbody");
        assert_eq!(fm, [("name".into(), "notes".into()), ("description".into(), "Write the release notes for a tag".into()), ("other".into(), "a\nb".into())]);
        assert_eq!(body, "body");
        let _ = std::fs::remove_dir_all(&base);
    }
}
