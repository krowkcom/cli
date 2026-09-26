//! Whether an instance can run a turn here, asked one way everywhere:
//! `krowk status`, `krowk doctor`, `krowk providers list`, and the host
//! before it moves a session or starts a turn (`check_model`, `settle`,
//! rollover candidates). Before this, `providers list` spawned each vendor
//! CLI in turn, the host checked only keys and binaries, and a SuperGrok
//! login counted as there whenever its name was in the credentials file.
//!
//! The answer is a `Readiness` and, around it, a `Report`: where the
//! credential comes from (`source` — an environment variable's name, a
//! vendor's own login, krowk's OAuth file; never the secret) and one line
//! that fixes it. A key is checked in the environment krowk resolved at
//! start, an OAuth login in krowk's provider credentials file (an expired
//! access token with no refresh token is `Expired`: nothing krowk can do
//! renews it), and a vendor login by asking the vendor — `claude auth status
//! --json`, Codex's `account/read` over `codex app-server` with `codex login
//! status` as the fallback — never by reading its files (R-BACK-2, R-BACK-3).
//!
//! Vendor checks spawn a process, so `check_all` runs them in parallel,
//! each bounded by `VENDOR_TIMEOUT`, and a long-lived host (the TUI) does
//! not re-ask for every switch: a vendor's "signed in" is kept for
//! `CACHE_FOR`. Only "signed in" is kept. A person who is told to sign in
//! does so in another terminal and tries again at once, and a remembered
//! "not signed in" would refuse them for a minute for nothing.

use crate::claude::auth as claude_auth;
use crate::codex::auth as codex_auth;
use crate::engine::EngineError;
use crate::instances::{Auth, Backend, Resolved};
use crate::oauth;
use crate::protocol::WireApi;
use serde_json::{json, Value};
use std::collections::HashMap;
use std::path::Path;
use std::process::{Command, Output, Stdio};
use std::sync::Mutex;
use std::time::{Duration, Instant};

/// How long one vendor check may take before its answer is `Unknown`. Long
/// enough for a cold `claude` (a Node start) on a slow disk; checks run in
/// parallel, so a listing waits for the slowest one, not their sum.
pub const VENDOR_TIMEOUT: Duration = Duration::from_secs(10);
/// How long a vendor's "signed in" is believed without asking again.
pub const CACHE_FOR: Duration = Duration::from_secs(60);

/// Whether an instance can run a turn, and if not, which kind of not.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Readiness {
    /// It can. `source` names where the credential comes from.
    Ready { source: String },
    /// An API key is read from `var`, and `var` is not set.
    KeyNotSet { var: String },
    /// No login: the vendor says so, or krowk's OAuth file has none.
    NotSignedIn,
    /// A login whose access token has expired and cannot be refreshed.
    Expired,
    /// The vendor binary is not there.
    NotInstalled,
    /// The check itself failed: a vendor that did not answer in time, or
    /// answered in a way krowk does not read. Not a refusal: the turn is
    /// left to fail, or not, on its own.
    Unknown { reason: String },
}

impl Readiness {
    pub fn is_ready(&self) -> bool {
        matches!(self, Readiness::Ready { .. })
    }

    /// The `state` field of `krowk status --json`: stable, snake_case.
    pub fn state(&self) -> &'static str {
        match self {
            Readiness::Ready { .. } => "ready",
            Readiness::KeyNotSet { .. } => "key_not_set",
            Readiness::NotSignedIn => "not_signed_in",
            Readiness::Expired => "expired",
            Readiness::NotInstalled => "not_installed",
            Readiness::Unknown { .. } => "unknown",
        }
    }

    /// The same, in words, for a table.
    pub fn label(&self) -> &'static str {
        match self {
            Readiness::Ready { .. } => "ready",
            Readiness::KeyNotSet { .. } => "key not set",
            Readiness::NotSignedIn => "not signed in",
            Readiness::Expired => "expired",
            Readiness::NotInstalled => "not installed",
            Readiness::Unknown { .. } => "unknown",
        }
    }
}

/// One instance's row: what `krowk status` prints, and what a refusal is
/// made of.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Report {
    pub instance: String,
    pub kind: &'static str,
    pub readiness: Readiness,
    /// Where the credential comes from — or, when it is not there, would.
    /// A name or a place, never a secret.
    pub source: String,
    /// What makes it ready; none when it is.
    pub fix: Option<String>,
}

impl Report {
    /// A row of `krowk status --json` (and `providers list`, `doctor`).
    /// Every key is always there, `null` when it has no value, so a script
    /// reads the same shape whatever the state.
    pub fn json(&self) -> Value {
        json!({
            "instance": self.instance,
            "kind": self.kind,
            "state": self.readiness.state(),
            "ready": self.readiness.is_ready(),
            "source": self.source,
            "fix": self.fix,
            "var": match &self.readiness { Readiness::KeyNotSet { var } => Some(var), _ => None },
            "reason": match &self.readiness { Readiness::Unknown { reason } => Some(reason), _ => None },
        })
    }

    /// Why a turn cannot run here, with its fix, before a session is moved
    /// or a process started — or none: ready, or a check that could not
    /// tell, which is left to the turn.
    pub fn refusal(&self, inst: &Resolved) -> Option<EngineError> {
        let name = &self.instance;
        let fix = self.fix.clone().unwrap_or_default();
        Some(match &self.readiness {
            Readiness::Ready { .. } | Readiness::Unknown { .. } => return None,
            Readiness::KeyNotSet { .. } => EngineError::new("not_authenticated", inst.missing_key().unwrap_or(fix)),
            Readiness::NotInstalled => EngineError::new("backend_not_found", format!("{} was not found — {fix}", inst.backend.as_ref().map(|b| b.binary.as_str()).unwrap_or_default())),
            Readiness::NotSignedIn => match inst.auth {
                Auth::OAuth { .. } => EngineError::new("not_authenticated", format!("the {name} instance is not signed in — {fix} (a SuperGrok or X Premium subscription)")),
                _ => EngineError::new("not_authenticated", format!("{} is not signed in for the {name} instance — {fix}", inst.vendor)).with_status(401),
            },
            Readiness::Expired => EngineError::new("not_authenticated", format!("the {name} instance's login has expired and cannot be refreshed — {fix}")).with_status(401),
        })
    }
}

impl Resolved {
    /// Why a call cannot be made, before one is: an API-key instance whose
    /// variable is unset. The readiness check is its one caller outside the
    /// wire clients, which keep it as a defensive check of their own.
    pub fn missing_key(&self) -> Option<String> {
        let keyed = self.auth == Auth::ApiKey || (self.auth == Auth::Vendor && !self.api_key_env.is_empty());
        (keyed && self.api_key.is_empty()).then(|| {
            format!("no API key for the {} instance — set {} (krowk reads the key from the environment, never from a file)", self.name, self.api_key_env)
        })
    }
}

/// What can be known without spawning anything: a key, an OAuth login, a
/// binary. None when only the vendor can say — a backend on its own login.
pub fn local(inst: &Resolved, credentials: &Path) -> Option<Readiness> {
    if let Some(b) = &inst.backend
        && b.path.is_none()
    {
        return Some(Readiness::NotInstalled);
    }
    if inst.missing_key().is_some() {
        return Some(Readiness::KeyNotSet { var: inst.api_key_env.clone() });
    }
    match &inst.auth {
        Auth::ApiKey => Some(Readiness::Ready { source: format!("${}", inst.api_key_env) }),
        Auth::Keyless => Some(Readiness::Ready { source: "no key".into() }),
        Auth::OAuth { .. } => Some(oauth_login(inst, credentials)),
        // A keyed backend runs on its key, which is krowk's to check, not a
        // login the vendor holds.
        Auth::Vendor if !inst.api_key_env.is_empty() => Some(Readiness::Ready { source: format!("${}, handed to {}", inst.api_key_env, inst.vendor) }),
        Auth::Vendor => None,
    }
}

/// An OAuth login, as krowk's own credentials file holds it. Nothing of
/// the tokens leaves this function but whether they can still be used.
fn oauth_login(inst: &Resolved, credentials: &Path) -> Readiness {
    match oauth::Store::new(credentials.to_path_buf()).load(&inst.name) {
        Err(e) => Readiness::Unknown { reason: e.message },
        Ok(None) => Readiness::NotSignedIn,
        Ok(Some(s)) if s.expired_for_good(krowk_store::now_ms()) => Readiness::Expired,
        Ok(Some(_)) => Readiness::Ready { source: format!("OAuth login in {}", credentials.display()) },
    }
}

/// One instance, asked: locally when that answers, else of its vendor
/// (bounded by `VENDOR_TIMEOUT`, a "signed in" cached for `CACHE_FOR`).
/// Blocks for as long as the vendor takes.
pub fn check(inst: &Resolved, credentials: &Path) -> Report {
    let readiness = local(inst, credentials).unwrap_or_else(|| match &inst.backend {
        Some(b) => vendor_cached(inst, b),
        None => Readiness::Unknown { reason: "no way to check this instance".into() },
    });
    report(inst, readiness, credentials)
}

/// Every instance, the vendor checks in parallel: as long as the slowest
/// one, never their sum. In the order given.
pub fn check_all(instances: &[&Resolved], credentials: &Path) -> Vec<Report> {
    std::thread::scope(|s| {
        let running: Vec<_> = instances.iter().map(|inst| s.spawn(move || check(inst, credentials))).collect();
        running
            .into_iter()
            .zip(instances)
            .map(|(h, inst)| h.join().unwrap_or_else(|_| report(inst, Readiness::Unknown { reason: "the check panicked".into() }, credentials)))
            .collect()
    })
}

/// `check`, off an async runtime's thread: a vendor check blocks on a
/// process for up to `VENDOR_TIMEOUT`, and the host's runtime has one
/// thread that the TUI's drawing and every other session also run on.
pub async fn check_async(inst: &Resolved, credentials: &Path) -> Report {
    if let Some(r) = local(inst, credentials) {
        return report(inst, r, credentials);
    }
    let (inst2, creds) = (inst.clone(), credentials.to_path_buf());
    match tokio::task::spawn_blocking(move || check(&inst2, &creds)).await {
        Ok(r) => r,
        Err(_) => report(inst, Readiness::Unknown { reason: "the check did not finish".into() }, credentials),
    }
}

/// A readiness with its source and fix around it: the row every caller
/// prints or refuses with.
pub fn report(inst: &Resolved, readiness: Readiness, credentials: &Path) -> Report {
    let source = match &readiness {
        Readiness::Ready { source } => source.clone(),
        _ => expected_source(inst, credentials),
    };
    let fix = fix(inst, &readiness);
    Report { instance: inst.name.clone(), kind: inst.kind, readiness, source, fix }
}

/// Where the credential would come from, said of an instance that is not
/// ready, so the person knows which variable or login to look at.
fn expected_source(inst: &Resolved, credentials: &Path) -> String {
    match &inst.auth {
        Auth::ApiKey => format!("${}", inst.api_key_env),
        Auth::Keyless => "no key".into(),
        Auth::OAuth { .. } => format!("OAuth login in {}", credentials.display()),
        Auth::Vendor if !inst.api_key_env.is_empty() => format!("${}, handed to {}", inst.api_key_env, inst.vendor),
        Auth::Vendor => vendor_login_source(inst, None),
    }
}

/// "Claude Code's own login in ~/.claude", with what it said when it is one.
fn vendor_login_source(inst: &Resolved, said: Option<&str>) -> String {
    let at = inst.backend.as_ref().and_then(|b| b.home.as_ref()).map(|h| format!(" in {}", h.display())).unwrap_or_default();
    match said {
        Some(s) => format!("{}'s own login{at} ({s})", inst.vendor),
        None => format!("{}'s own login{at}", inst.vendor),
    }
}

/// `krowk providers add <vendor>[ --name N]` for a backend instance: the
/// command that runs the vendor's own login in the instance's directory.
fn add_command(inst: &Resolved) -> String {
    let vendor = match inst.wire_api {
        WireApi::CodexAppServer => "codex",
        _ => "claude",
    };
    match inst.name.split_once(':') {
        Some((_, n)) => format!("krowk providers add {vendor} --name {n}"),
        None if inst.name == vendor => format!("krowk providers add {vendor}"),
        None => format!("krowk providers add {vendor} --name {}", inst.name),
    }
}

fn fix(inst: &Resolved, r: &Readiness) -> Option<String> {
    let codex = inst.wire_api == WireApi::CodexAppServer;
    Some(match r {
        Readiness::Ready { .. } => return None,
        Readiness::KeyNotSet { var } => format!("set {var} (krowk reads the key from the environment, never from a file)"),
        Readiness::NotSignedIn | Readiness::Expired if matches!(inst.auth, Auth::OAuth { .. }) => format!("sign in with `{}`", oauth::login_command(&inst.name)),
        Readiness::NotSignedIn | Readiness::Expired => {
            let own = if codex { "Codex's" } else { "Claude's" };
            format!("sign in with `{}`, which runs {own} own login", add_command(inst))
        }
        Readiness::NotInstalled if codex => "install Codex (https://developers.openai.com/codex), or name the binary with `krowk providers add codex --binary <path>`".into(),
        Readiness::NotInstalled => "install Claude Code (https://claude.com/claude-code), or name the binary with `krowk providers add claude --binary <path>`".into(),
        Readiness::Unknown { .. } => "see the reason, then run `krowk status` again".into(),
    })
}

/// Vendor answers that said "signed in", by instance, and when.
static SIGNED_IN: Mutex<Option<HashMap<String, (Instant, String)>>> = Mutex::new(None);

/// What makes two vendor checks the same question: the instance and
/// everything that changes which login the vendor reads. No key value is
/// in it — a keyed backend never reaches the vendor check.
fn cache_key(inst: &Resolved, b: &Backend) -> String {
    format!("{}\0{:?}\0{:?}\0{:?}\0{:?}", inst.name, b.path, b.config_dir, b.env, b.args)
}

fn vendor_cached(inst: &Resolved, b: &Backend) -> Readiness {
    let key = cache_key(inst, b);
    let known = SIGNED_IN.lock().unwrap_or_else(|e| e.into_inner()).as_ref().and_then(|m| m.get(&key)).filter(|(at, _)| at.elapsed() < CACHE_FOR).map(|(_, s)| s.clone());
    if let Some(source) = known {
        return Readiness::Ready { source };
    }
    let r = vendor(inst, b);
    if let Readiness::Ready { source } = &r {
        SIGNED_IN.lock().unwrap_or_else(|e| e.into_inner()).get_or_insert_with(HashMap::new).insert(key, (Instant::now(), source.clone()));
    }
    r
}

/// Asks the vendor. Only whether there is a login and its kind come back;
/// an email, a plan's owner, the part of a key Codex prints, stay out.
fn vendor(inst: &Resolved, b: &Backend) -> Readiness {
    let said = match inst.wire_api {
        WireApi::CodexAppServer => codex_auth::signed_in(b).map(|st| (st.logged_in, st.describe())),
        _ => claude_auth::status(b).map(|st| (st.logged_in, st.describe())),
    };
    match said {
        Ok((true, how)) => Readiness::Ready { source: vendor_login_source(inst, Some(&how)) },
        Ok((false, _)) => Readiness::NotSignedIn,
        Err(reason) => Readiness::Unknown { reason },
    }
}

/// Runs a command to its end, or kills it at `within`: none then. Its
/// output is read while it runs, so a chatty vendor cannot fill a pipe and
/// hang on it. It runs in the temporary directory, never the repository
/// krowk was started in: a status check is not a reason to read a project's
/// settings, which may name commands (an `apiKeyHelper`), in a repository
/// nobody trusted.
pub(crate) fn output_within(cmd: &mut Command, within: Duration) -> std::io::Result<Option<Output>> {
    use std::io::Read;
    let mut child = cmd.current_dir(std::env::temp_dir()).stdin(Stdio::null()).stdout(Stdio::piped()).stderr(Stdio::piped()).spawn()?;
    let drain = |pipe: Option<Box<dyn Read + Send>>| {
        std::thread::spawn(move || {
            let mut buf = Vec::new();
            if let Some(mut p) = pipe {
                let _ = p.read_to_end(&mut buf);
            }
            buf
        })
    };
    let out = drain(child.stdout.take().map(|p| Box::new(p) as Box<dyn Read + Send>));
    let err = drain(child.stderr.take().map(|p| Box::new(p) as Box<dyn Read + Send>));
    let started = Instant::now();
    let status = loop {
        if let Some(s) = child.try_wait()? {
            break s;
        }
        if started.elapsed() > within {
            let _ = child.kill();
            let _ = child.wait();
            return Ok(None);
        }
        std::thread::sleep(Duration::from_millis(20));
    };
    Ok(Some(Output { status, stdout: out.join().unwrap_or_default(), stderr: err.join().unwrap_or_default() }))
}
