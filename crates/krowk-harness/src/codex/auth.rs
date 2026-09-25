//! A Codex login, as Codex reports it (R-INST-2, with `codex login` in
//! place of `claude auth login`). krowk never opens Codex's login file or
//! runs an OAuth flow of its own with Codex's client: it asks `codex login
//! status`, and a login is made by `codex login` itself, on the person's
//! own terminal, in OpenAI's own flow. Both run with the instance's
//! `CODEX_HOME`, so each account signs in, and is asked about, on its own.

use crate::instances::Backend;
use crate::protocol::Billing;
use std::path::Path;
use std::process::{Command, ExitStatus, Stdio};

/// What `codex login status` said. Only whether there is a login and what
/// it is billed to are kept: an API-key login prints part of its key, and
/// that is never read past the words that name the method.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Status {
    pub logged_in: bool,
    pub billing: Option<Billing>,
}

impl Status {
    /// For a listing: how the instance is signed in.
    pub fn describe(&self) -> String {
        match (self.logged_in, self.billing) {
            (false, _) => "not signed in".into(),
            (true, Some(Billing::Subscription)) => "signed in with ChatGPT".into(),
            (true, Some(Billing::ApiKey)) => "signed in with an API key".into(),
            (true, None) => "signed in".into(),
        }
    }

    /// Reads `codex login status`: it exits 0 with `Logged in using …`
    /// when there is a login, and non-zero with `Not logged in` when there
    /// is none, on stderr or stdout depending on the version.
    pub fn parse(success: bool, said: &str) -> Status {
        let line = said.lines().map(str::trim).find(|l| l.starts_with("Logged in") || l.starts_with("Not logged in")).unwrap_or_default();
        let logged_in = success && line.starts_with("Logged in");
        let method = line.split(" - ").next().unwrap_or_default().to_ascii_lowercase();
        let billing = logged_in
            .then(|| {
                if method.contains("chatgpt") {
                    Some(Billing::Subscription)
                } else if method.contains("api key") {
                    Some(Billing::ApiKey)
                } else {
                    None
                }
            })
            .flatten();
        Status { logged_in, billing }
    }
}

/// `codex <args>` for this instance: its binary, its `CODEX_HOME`, and the
/// environment a backend process gets.
pub fn command(b: &Backend, args: &[&str]) -> Command {
    let mut c = Command::new(b.path.as_deref().unwrap_or(Path::new(&b.binary)));
    c.args(args);
    let (remove, set) = super::environment(b);
    for k in remove {
        c.env_remove(k);
    }
    c.envs(set);
    c
}

/// Asks Codex whether this instance is signed in.
pub fn status(b: &Backend) -> Result<Status, String> {
    let out = command(b, &["login", "status"]).stdin(Stdio::null()).output().map_err(|e| format!("{} could not be run: {e}", b.binary))?;
    let said = format!("{}\n{}", String::from_utf8_lossy(&out.stderr), String::from_utf8_lossy(&out.stdout));
    if !said.contains("Logged in") && !said.contains("Not logged in") {
        return Err(format!("`{} login status` answered in a way krowk does not read (exit {})", b.binary, out.status));
    }
    Ok(Status::parse(out.status.success(), &said))
}

/// Runs `codex login` on this terminal: OpenAI's own sign-in, which opens
/// a browser or prints a link, and writes the login into the instance's
/// home. `device` asks Codex for its device-code flow instead. What Codex
/// prints goes to stderr, where the person reads it, so krowk's own stdout
/// stays the one answer a script parses.
pub fn login(b: &Backend, device: bool) -> Result<ExitStatus, String> {
    let args: &[&str] = if device { &["login", "--device-auth"] } else { &["login"] };
    command(b, args).stdin(Stdio::inherit()).stdout(std::io::stderr()).stderr(Stdio::inherit()).status().map_err(|e| format!("{} could not be run: {e}", b.binary))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn r_inst_2_codex_login_status_is_read_for_its_method_and_nothing_else() {
        let chatgpt = Status::parse(true, "Logged in using ChatGPT\n");
        assert_eq!(chatgpt, Status { logged_in: true, billing: Some(Billing::Subscription) });
        assert_eq!(chatgpt.describe(), "signed in with ChatGPT");
        let key = Status::parse(true, "WARNING: something\nLogged in using an API key - sk-proj-***ABCD\n");
        assert_eq!(key.billing, Some(Billing::ApiKey));
        assert!(!format!("{key:?}").contains("ABCD"), "the key's shown part is not kept");
        assert_eq!(Status::parse(false, "Not logged in\n"), Status { logged_in: false, billing: None });
        assert_eq!(Status::parse(false, "Logged in using ChatGPT"), Status { logged_in: false, billing: None }, "the exit code decides");
    }
}
