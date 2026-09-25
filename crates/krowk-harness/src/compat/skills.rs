//! Claude-format skills, with progressive disclosure (R-COMPAT-1): a
//! directory holding a `SKILL.md` whose front matter names it and says what
//! it is for. The name and the description ride in every native turn's
//! system prompt; the body enters the conversation only when the model
//! calls the `skill` tool with the name, and the files beside it (scripts,
//! references) are the model's to read from there.
//!
//! Skills are found in krowk's config directory (`skills/`), Claude Code's
//! user directory (`skills/`), and `.claude/skills` in every directory from
//! the repository's root down to the working directory; a skill of the same
//! name found later — deeper — replaces the one before. A `SKILL.md` with
//! no description is not listed: the description is what the model chooses
//! by. A skill's directory is readable by the file tools as the working
//! directory is, and never writable.

use crate::permissions::Config;
use schemars::JsonSchema;
use serde::Deserialize;
use std::path::{Path, PathBuf};

pub const TOOL: &str = "skill";

/// One skill.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Skill {
    pub name: String,
    pub description: String,
    /// Its directory.
    pub dir: PathBuf,
}

/// Load a skill's instructions.
#[derive(Debug, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct SkillInput {
    /// The skill's name, as the system prompt lists it.
    pub name: String,
}

/// A skill's body is capped here: the rest is for the model to read.
const BODY_CAP: usize = 256 << 10;

fn read_dir_of(root: &Path, out: &mut Vec<Skill>, repo: Option<&Path>) {
    let Ok(rd) = std::fs::read_dir(root) else { return };
    let mut dirs: Vec<PathBuf> = rd.flatten().map(|e| e.path()).filter(|p| p.join("SKILL.md").is_file()).collect();
    dirs.sort();
    for dir in dirs {
        // A repository's skill that links out of the repository is not
        // taken: its directory becomes readable, and its file is read.
        if let Some(repo) = repo
            && !dir.join("SKILL.md").canonicalize().is_ok_and(|c| c.starts_with(repo))
        {
            continue;
        }
        let Ok(text) = std::fs::read_to_string(dir.join("SKILL.md")) else { continue };
        let (fm, _) = super::instructions::front_matter(&text);
        let get = |k: &str| fm.iter().find(|(key, _)| key == k).map(|(_, v)| v.clone()).unwrap_or_default();
        let name = Some(get("name")).filter(|n| !n.is_empty()).unwrap_or_else(|| dir.file_name().unwrap_or_default().to_string_lossy().into_owned());
        let description = get("description");
        if description.is_empty() {
            continue;
        }
        out.retain(|s| s.name != name);
        out.push(Skill { name, description, dir });
    }
}

/// Every skill that applies in `cwd`.
pub fn discover(cfg: &Config, cwd: &Path) -> Vec<Skill> {
    let mut out = Vec::new();
    if let Some(d) = &cfg.krowk_dir {
        read_dir_of(&d.join("skills"), &mut out, None);
    }
    if let Some(d) = cfg.claude_home() {
        read_dir_of(&d.join("skills"), &mut out, None);
    }
    let root = crate::trust::root(cwd);
    for dir in crate::permissions::settings::chain(&root, cwd) {
        read_dir_of(&dir.join(".claude/skills"), &mut out, Some(&root));
    }
    out
}

/// The skills as the system prompt lists them: names and descriptions only.
pub fn render(list: &[Skill]) -> String {
    if list.is_empty() {
        return String::new();
    }
    let mut s = String::from("\n\nSkills: when a task matches one's description, call the skill tool with its name to load its instructions before you start.\n");
    for k in list {
        s.push_str(&format!("- {}: {}\n", k.name, k.description));
    }
    s
}

/// The `skill` tool's definition, offered when there are skills.
pub fn definition() -> crate::protocol::ToolDefinition {
    crate::protocol::ToolDefinition {
        name: TOOL.into(),
        description: "Load a skill's instructions by its name, as the system prompt lists it. Returns the skill's full text and the directory its other files are in.".into(),
        input_schema: crate::tools::input_schema::<SkillInput>(),
        grammar: None,
    }
}

/// Runs the `skill` tool: the body of the skill named.
pub fn load(list: &[Skill], input: &serde_json::Value) -> (String, bool) {
    let name = match SkillInput::deserialize(input) {
        Ok(i) => i.name,
        Err(e) => return (format!("invalid input for skill: {e}"), true),
    };
    let Some(k) = list.iter().find(|k| k.name == name.trim()) else {
        let names: Vec<&str> = list.iter().map(|k| k.name.as_str()).collect();
        return (format!("there is no skill named {name:?} — the skills are {}", names.join(", ")), true);
    };
    let text = match std::fs::read_to_string(k.dir.join("SKILL.md")) {
        Ok(t) => t,
        Err(e) => return (format!("the skill {name:?} could not be read: {e}"), true),
    };
    let (_, body) = super::instructions::front_matter(&text);
    let body = if body.len() > BODY_CAP { &body[..body.floor_char_boundary(BODY_CAP)] } else { body };
    (format!("{}\n\n(The skill's files are in {}; read them from there.)", body.trim_end(), k.dir.display()), false)
}
