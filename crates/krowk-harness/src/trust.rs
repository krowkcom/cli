//! Which repositories a backend may run in (R-BACK-6). `claude -p` skips
//! Claude Code's own workspace-trust dialog, and still runs the project's
//! hooks (`.claude/settings.json`) and MCP servers (`.mcp.json`): a
//! repository someone else wrote can run code the moment a backend starts
//! in it. So before a backend is spawned, krowk asks its own question — the
//! one Claude Code would have asked — and remembers the answer here.
//!
//! The unit of trust is the repository: the nearest ancestor of the working
//! directory holding a `.git`, else the working directory itself. A trusted
//! directory covers everything under it. The list lives in krowk's config
//! directory as `trusted.json`, `0600`, replaced by rename; it is a host's
//! own record and never syncs. The native engine runs nothing of the
//! repository's, so only backends consult it.

use crate::engine::EngineError;
use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};
use std::sync::Arc;

pub const FILE: &str = "trusted.json";

/// Asked before a backend is spawned in a repository, with the
/// repository's root: `Ok` to go ahead, or the refusal to report. The
/// client decides how — a list, a flag, a prompt — so the host stays free
/// of terminals.
pub type Gate = Arc<dyn Fn(&Path) -> Result<(), EngineError> + Send + Sync>;

/// A gate for a host that never runs a backend, or a test that trusts
/// everything it made.
pub fn allow_all() -> Gate {
    Arc::new(|_| Ok(()))
}

/// The repository a working directory belongs to, canonical: what trust is
/// granted to.
pub fn root(cwd: &Path) -> PathBuf {
    let cwd = cwd.canonicalize().unwrap_or_else(|_| cwd.to_path_buf());
    cwd.ancestors().find(|d| d.join(".git").symlink_metadata().is_ok()).map(Path::to_path_buf).unwrap_or(cwd)
}

/// What `claude -p` would run of the repository's own, found in it: the
/// reason the question is asked, shown with it.
pub fn what_runs(root: &Path) -> Vec<String> {
    let mut found = Vec::new();
    for (file, what) in [
        (".claude/settings.json", "its hooks and settings"),
        (".claude/settings.local.json", "its local hooks and settings"),
        (".mcp.json", "its MCP servers"),
        (".claude/commands", "its commands"),
        (".claude/agents", "its agents"),
    ] {
        if root.join(file).symlink_metadata().is_ok() {
            found.push(format!("{file} ({what})"));
        }
    }
    found
}

/// The refusal a gate reports for a repository nobody trusted.
pub fn untrusted(root: &Path, how: &str) -> EngineError {
    let runs = what_runs(root);
    let found = if runs.is_empty() { String::new() } else { format!(" — it has {}", runs.join(", ")) };
    EngineError::new(
        "untrusted_directory",
        format!(
            "{} is not a repository you have trusted, and `claude -p` would run its hooks and MCP servers without asking{found}. {how}",
            root.display()
        ),
    )
}

#[derive(Debug, Default, Serialize, Deserialize)]
struct Listed {
    #[serde(default)]
    directories: Vec<String>,
}

/// The trusted-directories file.
#[derive(Debug, Clone)]
pub struct Store {
    path: PathBuf,
}

impl Store {
    pub fn new(path: PathBuf) -> Store {
        Store { path }
    }

    fn read(&self) -> Result<Listed, String> {
        match std::fs::read(&self.path) {
            Ok(raw) => serde_json::from_slice(&raw).map_err(|e| format!("{} is not valid JSON: {e}", self.path.display())),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Listed::default()),
            Err(e) => Err(format!("reading {}: {e}", self.path.display())),
        }
    }

    /// Whether `root`, or a directory above it, was trusted. A file that
    /// cannot be read trusts nothing.
    pub fn trusts(&self, root: &Path) -> bool {
        let Ok(listed) = self.read() else { return false };
        listed.directories.iter().any(|d| root.starts_with(Path::new(d)))
    }

    /// Remembers `root` as trusted.
    pub fn trust(&self, root: &Path) -> Result<(), String> {
        let mut listed = self.read()?;
        let root = root.display().to_string();
        if listed.directories.contains(&root) {
            return Ok(());
        }
        listed.directories.push(root);
        listed.directories.sort();
        let dir = self.path.parent().ok_or_else(|| format!("{} has no directory", self.path.display()))?;
        crate::log::private_dir(dir).map_err(|e| format!("create {}: {e}", dir.display()))?;
        let tmp = self.path.with_extension("json.tmp");
        let mut o = std::fs::OpenOptions::new();
        o.write(true).create(true).truncate(true);
        #[cfg(unix)]
        std::os::unix::fs::OpenOptionsExt::mode(&mut o, 0o600);
        let body = serde_json::to_vec_pretty(&listed).expect("the list serializes");
        std::io::Write::write_all(&mut o.open(&tmp).map_err(|e| format!("write {}: {e}", tmp.display()))?, &body).map_err(|e| format!("write {}: {e}", tmp.display()))?;
        std::fs::rename(&tmp, &self.path).map_err(|e| format!("replace {}: {e}", self.path.display()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn r_back_6_trust_is_per_repository_covers_what_is_under_it_and_is_remembered() {
        let base = std::env::temp_dir().join(format!("krowk-trust-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&base);
        std::fs::create_dir_all(base.join("repo/.git")).unwrap();
        std::fs::create_dir_all(base.join("repo/src/deep")).unwrap();
        std::fs::create_dir_all(base.join("repo/.claude")).unwrap();
        std::fs::write(base.join("repo/.mcp.json"), "{}").unwrap();
        std::fs::write(base.join("repo/.claude/settings.json"), "{}").unwrap();
        let repo = base.join("repo").canonicalize().unwrap();
        assert_eq!(root(&base.join("repo/src/deep")), repo, "the repository, not the subdirectory");
        let runs = what_runs(&repo);
        assert!(runs.iter().any(|r| r.starts_with(".mcp.json")) && runs.iter().any(|r| r.starts_with(".claude/settings.json")), "{runs:?}");
        let e = untrusted(&repo, "Pass --trust.");
        assert_eq!(e.code, "untrusted_directory");
        assert!(e.message.contains("hooks and MCP servers") && e.message.contains(".mcp.json") && e.message.ends_with("Pass --trust."), "{}", e.message);

        let store = Store::new(base.join("config/trusted.json"));
        assert!(!store.trusts(&repo));
        store.trust(&repo).unwrap();
        store.trust(&repo).unwrap();
        assert!(store.trusts(&repo) && store.trusts(&repo.join("src")), "a trusted directory covers what is under it");
        assert!(!store.trusts(&base), "and nothing above it");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            assert_eq!(std::fs::metadata(base.join("config/trusted.json")).unwrap().permissions().mode() & 0o777, 0o600);
        }
        std::fs::write(base.join("config/trusted.json"), "not json").unwrap();
        assert!(!store.trusts(&repo), "a file that cannot be read trusts nothing");
        let _ = std::fs::remove_dir_all(&base);
    }
}
