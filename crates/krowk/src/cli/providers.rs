//! `krowk providers`: the instances a model runs on (R-PROV-4, R-INST-1).
//! `add` writes a named definition into the global config.json — an API-key
//! profile with a base URL, `openai:work` — or signs in to SuperGrok and
//! keeps the tokens in krowk's provider credentials file, or adds a Claude
//! Code account, `claude:personal`: a config directory of its own, signed in
//! by `claude auth login` in Anthropic's own flow (R-INST-2). `list` shows
//! every instance and whether it can run; `remove` takes a definition, and a
//! login, away.
//!
//! A definition names the variable its key is read from and never holds the
//! key, so config.json stays something that can sync between hosts. A
//! Claude Code login is Claude Code's: krowk asks `claude auth status`
//! whether there is one and never reads it (R-BACK-2).

use super::{auth, Ctx};
use crate::config;
use crate::output::Format;
use krowk_api::{fail, Error};
use krowk_harness::claude::auth as claude_auth;
use krowk_harness::instances::{self, Auth, InstanceKind, Registry};
use krowk_harness::oauth::{self, Step, Store};
use serde_json::{json, Value};
use std::path::PathBuf;

/// krowk's provider credentials file, beside config.json.
pub(super) fn credentials_path() -> PathBuf {
    krowk_api::creds::config_dir().join(oauth::CREDENTIALS_FILE)
}

const PROVIDERS: &[&str] = &["anthropic", "openai", "xai", "openrouter", "openai-compatible", "supergrok", "claude"];

/// `NAME` in `OPENAI_NAME_API_KEY`: upper case, anything else an underscore.
fn env_part(s: &str) -> String {
    s.chars().map(|c| if c.is_ascii_alphanumeric() { c.to_ascii_uppercase() } else { '_' }).collect()
}

fn opt(s: &str) -> Option<String> {
    Some(s.trim().to_string()).filter(|s| !s.is_empty())
}

pub(super) fn add(ctx: &mut Ctx, args: &[String]) -> Result<(), Error> {
    let provider = args.first().map(|s| s.trim().to_ascii_lowercase()).unwrap_or_default();
    if !PROVIDERS.contains(&provider.as_str()) {
        let said = if provider.is_empty() { "no provider".to_string() } else { format!("{provider:?}") };
        return Err(fail("bad_argument", format!("{said} is not a provider krowk's engine speaks — one of {}", PROVIDERS.join(", "))));
    }
    let name = opt(&ctx.f.name);
    if name.as_deref().is_some_and(|n| n.contains('/') || n.chars().any(char::is_whitespace)) {
        return Err(fail("bad_flag", "--name cannot hold a `/` or a space: it is the part of --model before the model id"));
    }
    // A name with a `:` is an instance's whole name, as an error that asks
    // for a login spells it.
    let instance = match (&name, provider.as_str()) {
        (Some(n), _) if n.contains(':') => n.clone(),
        (Some(n), "openai-compatible") => n.clone(),
        (None, "openai-compatible") => return Err(fail("bad_flag", "openai-compatible needs --name, e.g. `krowk providers add openai-compatible --name local --base-url http://127.0.0.1:11434/v1`")),
        (Some(n), p) => format!("{p}:{n}"),
        (None, p) => p.to_string(),
    };
    if provider != "claude" && (ctx.f.given.contains("binary") || ctx.f.given.contains("config-dir")) {
        return Err(fail("bad_flag", "`--binary` and `--config-dir` describe a Claude Code instance: `krowk providers add claude --name work --config-dir …`"));
    }
    // A named profile of a provider reads its own variable, so two
    // profiles never share a key by accident.
    let key_env = opt(&ctx.f.api_key_env).or_else(|| match (&name, provider.as_str()) {
        (_, "supergrok" | "claude") => None,
        (Some(n), "openai-compatible") => Some(format!("{}_API_KEY", env_part(n))),
        (Some(n), p) => Some(format!("{}_{}_API_KEY", env_part(p), env_part(n))),
        (None, _) => None,
    });
    let base_url = opt(&ctx.f.base_url);
    let kind = match provider.as_str() {
        "anthropic" => InstanceKind::AnthropicApi { api_key_env: key_env.clone(), base_url, thinking: None, max_tokens: None, effort: None },
        "openai" => InstanceKind::OpenaiApi { api_key_env: key_env.clone(), base_url, wire_api: None, effort: None },
        "xai" => InstanceKind::XaiApi { api_key_env: key_env.clone(), base_url, effort: None },
        "openrouter" => InstanceKind::OpenrouterApi { api_key_env: key_env.clone(), base_url, effort: None },
        "openai-compatible" => InstanceKind::OpenaiCompatible {
            base_url: base_url.ok_or_else(|| fail("bad_flag", "openai-compatible needs --base-url, e.g. http://127.0.0.1:11434/v1"))?,
            api_key_env: opt(&ctx.f.api_key_env),
            provider: None,
            wire_api: None,
            effort: None,
        },
        // A router in front of Claude Code: the base URL it sends to goes in
        // the instance's env, and the key is named by --api-key-env — krowk
        // reads that variable and hands the key to the process
        // (ANTHROPIC_AUTH_TOKEN with a base URL, ANTHROPIC_API_KEY without).
        // Claude Code inherits neither from krowk's own environment.
        "claude" => InstanceKind::ClaudeCode {
            binary: opt(&ctx.f.binary),
            config_dir: opt(&ctx.f.config_dir),
            env: base_url.clone().map(|u| [("ANTHROPIC_BASE_URL".to_string(), u)].into()).unwrap_or_default(),
            args: Vec::new(),
            api_key_env: opt(&ctx.f.api_key_env),
            effort: None,
        },
        _ => InstanceKind::XaiOauth { base_url, issuer: None, client_id: opt(&ctx.f.client_id), scope: None, effort: None },
    };
    let path = config::global_path();
    let existing = super::prompt::load_instances()?;
    // Adding an instance that exists updates it: the flags given replace
    // its fields, and the rest — an issuer, a pinned wire API — are kept, so
    // `krowk providers add supergrok` again is how a login is renewed.
    let kind = match existing.instances.get(&instance) {
        Some(old) if old.tag() != kind.tag() => {
            return Err(fail("instance_exists", format!("{instance} is already defined as {} — remove it first with `krowk providers remove {instance}`", old.tag())));
        }
        Some(old) => {
            let mut merged = serde_json::to_value(old).expect("a definition serializes");
            if let (Some(m), Value::Object(new)) = (merged.as_object_mut(), serde_json::to_value(&kind).expect("a definition serializes")) {
                m.extend(new);
            }
            serde_json::from_value(merged).map_err(|e| fail("bad_config", format!("{instance}: {e}")))?
        }
        None => kind,
    };

    // A Claude Code account signs in first, in its own config directory: a
    // login that fails leaves no definition behind.
    let mut claude_status = None;
    let kind = match kind {
        InstanceKind::ClaudeCode { binary, config_dir, env, args, api_key_env, effort } => {
            // A named instance is its own account, so it gets a config
            // directory of its own unless one is named; the unnamed one is
            // Claude Code as the person already uses it.
            let config_dir = config_dir.or_else(|| {
                name.as_ref().and_then(|_| {
                    krowk_harness::log::sessions_dir(ctx.io.env).and_then(|d| d.parent().map(|p| p.join("claude").join(instance.replace(':', "-")).display().to_string()))
                })
            });
            let kind = InstanceKind::ClaudeCode { binary, config_dir, env, args, api_key_env, effort };
            let (status, _) = sign_in_claude(ctx, &instance, &kind)?;
            claude_status = Some(status);
            kind
        }
        k => k,
    };

    // SuperGrok signs in first: a login that fails leaves nothing behind.
    let mut signed_in = false;
    if let InstanceKind::XaiOauth { .. } = &kind {
        let resolved = Registry::resolve(&instances::InstancesConfig { instances: [(instance.clone(), kind.clone())].into(), ..Default::default() }, ctx.io.env);
        let Auth::OAuth { issuer, client_id, scope } = resolved.get(&instance).map_err(|e| fail("bad_config", e))?.auth.clone() else { unreachable!("an xai-oauth instance signs in with OAuth") };
        let login = oauth::Login { issuer, client_id, scope };
        let open = !ctx.f.no_browser && !auth::headless(ctx);
        let stderr = &mut *ctx.io.stderr;
        let stored = oauth::sign_in(&login, ctx.f.device, &mut |step| match step {
            Step::Open(url) => {
                let _ = writeln!(stderr, "Sign in to xAI with your SuperGrok or X Premium account:\n  {url}");
                if open && auth::open_browser(url) {
                    let _ = writeln!(stderr, "(opened in your browser — waiting for it to finish)");
                }
            }
            Step::Device(p) => {
                let at = p.verification_uri_complete.as_deref().unwrap_or(&p.verification_uri);
                let _ = writeln!(stderr, "Open {at}\nand enter the code {} to sign in to xAI with your SuperGrok or X Premium account. Waiting…", p.user_code);
            }
        })
        .map_err(|e| fail(&e.code, e.message))?;
        Store::new(credentials_path()).save(&instance, &stored).map_err(|e| fail("credentials_unwritable", e.message))?;
        signed_in = true;
    }

    let def = serde_json::to_value(&kind).expect("a definition serializes");
    config::edit(&path, |raw| {
        let instances = raw.entry("instances").or_insert_with(|| json!({}));
        if !instances.is_object() {
            *instances = json!({});
        }
        instances.as_object_mut().expect("an object").insert(instance.clone(), def.clone());
    })
    .map_err(|e| fail("config_unwritable", format!("{}: {e}", path.display())))?;

    // What the definition resolves to, as a prompt will read it.
    let resolved = Registry::resolve(&instances::InstancesConfig { instances: [(instance.clone(), kind.clone())].into(), ..Default::default() }, ctx.io.env);
    let r = resolved.get(&instance).map_err(|e| fail("bad_config", e))?;
    let key_env = (r.auth == Auth::ApiKey || !r.api_key_env.is_empty()).then(|| r.api_key_env.clone());
    let unset = key_env.is_some() && r.api_key.is_empty();
    if ctx.format != Format::Human {
        let mut report = json!({ "instance": instance, "kind": kind.tag(), "config": path.display().to_string(), "definition": def });
        if let Some(k) = &key_env {
            report["api_key_env"] = json!(k);
        }
        if signed_in {
            report["signed_in"] = json!(true);
            report["credentials"] = json!(credentials_path().display().to_string());
        }
        if let Some(st) = &claude_status {
            report["signed_in"] = json!(st.logged_in);
            report["login"] = json!(st.describe());
        }
        let summary = format!("added {instance}");
        return super::sessions::emit_data(ctx, report, summary);
    }
    let out = &mut *ctx.io.stdout;
    let _ = writeln!(out, "added {instance} ({}) to {}", kind.tag(), path.display());
    match &key_env {
        Some(k) if unset => {
            let _ = writeln!(out, "its key is read from ${k}, which is not set here — export it before running a prompt");
        }
        Some(k) => {
            let _ = writeln!(out, "its key is read from ${k}");
        }
        None if signed_in => {
            let _ = writeln!(out, "signed in; the tokens are in {} (0600)", credentials_path().display());
        }
        None => {}
    }
    if let (Some(st), Some(b)) = (&claude_status, &r.backend) {
        let dir = b.config_dir.as_ref().map(|d| format!(" with CLAUDE_CONFIG_DIR={}", d.display())).unwrap_or_default();
        let _ = writeln!(out, "runs Claude Code ({}){dir} — {}", b.path.as_ref().map(|p| p.display().to_string()).unwrap_or_else(|| b.binary.clone()), st.describe());
    }
    let example = match provider.as_str() {
        "anthropic" => "claude-opus-5-5",
        "openai" => "gpt-5.4",
        "xai" | "supergrok" => "grok-4.7",
        "claude" => "sonnet",
        "openrouter" => "openai/gpt-5.4",
        _ => "<model>",
    };
    let _ = writeln!(out, "use it with: krowk -p --model {instance}/{example} \"…\"");
    Ok(())
}

pub(super) fn list(ctx: &mut Ctx) -> Result<(), Error> {
    let cfg = super::prompt::load_instances()?;
    let reg = Registry::resolve(&cfg, ctx.io.env);
    let logins = Store::new(credentials_path()).names().unwrap_or_default();
    let rows: Vec<Value> = reg
        .instances
        .values()
        .map(|r| {
            let (auth, ready, state) = match &r.auth {
                Auth::ApiKey => (format!("api key from ${}", r.api_key_env), !r.api_key.is_empty(), "key not set"),
                Auth::Keyless => ("no key".to_string(), true, ""),
                Auth::OAuth { .. } => ("SuperGrok login".to_string(), logins.contains(&r.name), "not signed in"),
                // Asked of Claude Code, the only thing that may read its login.
                // A keyed instance runs on its key, which is krowk's to check.
                Auth::Vendor if !r.api_key_env.is_empty() => (format!("runs Claude Code with the key from ${}", r.api_key_env), !r.api_key.is_empty() && r.backend.as_ref().is_some_and(|b| b.path.is_some()), "key not set"),
                Auth::Vendor => match r.backend.as_ref().filter(|b| b.path.is_some()).map(claude_auth::status) {
                    None => ("runs Claude Code".to_string(), false, "not installed"),
                    Some(Ok(st)) => (format!("runs Claude Code: {}", st.describe()), st.logged_in, "not signed in"),
                    Some(Err(e)) => (format!("runs Claude Code: {e}"), false, "unknown"),
                },
            };
            let mut row = json!({
                "instance": r.name,
                "kind": r.kind,
                "provider": r.provider,
                "wire_api": r.wire_api.name(),
                "base_url": r.base_url,
                "auth": auth,
                "ready": ready,
                "state": if ready { "ready" } else { state },
                "configured": cfg.instances.contains_key(&r.name),
            });
            // A backend has a binary and a config directory where an API
            // has a base URL.
            if let Some(b) = &r.backend {
                row["binary"] = json!(b.path.as_ref().map(|p| p.display().to_string()).unwrap_or_else(|| b.binary.clone()));
                row["config_dir"] = json!(b.home.as_ref().map(|p| p.display().to_string()));
            }
            row
        })
        .collect();
    if ctx.format != Format::Human {
        let summary = format!("{} instances", rows.len());
        return super::sessions::emit_data(ctx, json!({ "instances": rows }), summary);
    }
    let width = rows.iter().map(|r| r["instance"].as_str().unwrap_or_default().len()).max().unwrap_or(0);
    let out = &mut *ctx.io.stdout;
    for r in &rows {
        let s = |k: &str| r[k].as_str().unwrap_or_default().to_string();
        let state = s("state");
        let place = if r.get("binary").is_some() { s("binary") } else { s("base_url") };
        let _ = writeln!(out, "{:<width$}  {:<17}  {:<13}  {place}  ({})", s("instance"), s("kind"), state, s("auth"));
    }
    Ok(())
}

pub(super) fn remove(ctx: &mut Ctx, args: &[String]) -> Result<(), Error> {
    let Some(instance) = args.first().map(|s| s.trim().to_string()).filter(|s| !s.is_empty()) else {
        return Err(fail("bad_argument", "name the instance: `krowk providers remove openai:work` — `krowk providers list` shows them"));
    };
    let path = config::global_path();
    let cfg = super::prompt::load_instances()?;
    let defined = cfg.instances.contains_key(&instance);
    if defined {
        config::edit(&path, |raw| {
            if let Some(Value::Object(m)) = raw.get_mut("instances") {
                m.remove(&instance);
            }
        })
        .map_err(|e| fail("config_unwritable", format!("{}: {e}", path.display())))?;
    }
    let forgot = Store::new(credentials_path()).remove(&instance).map_err(|e| fail("credentials_unwritable", e.message))?;
    if !defined && !forgot {
        let implicit = instances::implicit().iter().any(|(n, _)| *n == instance);
        let why = if implicit { format!("{instance} is built in and has no definition or login to remove") } else { format!("no instance named {instance} is defined — `krowk providers list` shows them") };
        return Err(fail("no_instance", why));
    }
    // A Claude Code account's directory holds Claude's own login, which is
    // Claude Code's to sign out of: krowk leaves it as it is.
    let kept = match cfg.instances.get(&instance) {
        Some(InstanceKind::ClaudeCode { config_dir: Some(dir), .. }) => Some(dir.clone()),
        _ => None,
    };
    if ctx.format != Format::Human {
        let summary = format!("removed {instance}");
        let mut report = json!({ "instance": instance, "removed_definition": defined, "removed_login": forgot });
        if let Some(dir) = &kept {
            report["config_dir_kept"] = json!(dir);
        }
        return super::sessions::emit_data(ctx, report, summary);
    }
    let what = match (defined, forgot) {
        (true, true) => "its definition and its login",
        (true, false) => "its definition",
        _ => "its login",
    };
    let _ = writeln!(ctx.io.stdout, "removed {instance}: {what}");
    if let Some(dir) = &kept {
        let _ = writeln!(ctx.io.stdout, "its config directory {dir} is kept, with Claude Code's login in it — `CLAUDE_CONFIG_DIR={dir} claude auth logout` signs it out");
    }
    Ok(())
}

/// R-INST-2: a Claude Code account is signed in by Claude Code. Its config
/// directory is made (0700) if it is new, `claude auth status` is asked,
/// and when it says no, `claude auth login` runs on this terminal in
/// Anthropic's own flow; then status is asked again. A keyed instance — a
/// router (a base URL in its env) or a Console key, named by
/// `--api-key-env` — runs on that key, which krowk reads and hands the
/// process, so no login is run for it. A failure removes a directory this
/// made.
fn sign_in_claude(ctx: &mut Ctx, instance: &str, kind: &InstanceKind) -> Result<(claude_auth::Status, bool), Error> {
    let reg = Registry::resolve(&instances::InstancesConfig { instances: [(instance.to_string(), kind.clone())].into(), ..Default::default() }, ctx.io.env);
    let backend = reg.get(instance).map_err(|e| fail("bad_config", e))?.backend.clone().expect("a claude-code instance has a backend");
    if backend.path.is_none() {
        return Err(fail("backend_not_found", format!("{} was not found — install Claude Code (https://claude.com/claude-code), or name the binary with --binary", backend.binary)));
    }
    let made = match &backend.config_dir {
        Some(dir) if !dir.exists() => {
            private_dir(dir).map_err(|e| fail("config_unwritable", format!("create {}: {e}", dir.display())))?;
            Some(dir.clone())
        }
        _ => None,
    };
    let undo = |e: Error| {
        if let Some(dir) = &made {
            let _ = std::fs::remove_dir_all(dir);
        }
        e
    };
    let status = claude_auth::status(&backend).map_err(|e| undo(fail("backend_failed", e)))?;
    // A keyed instance — a router, a Console key — signs in with its key,
    // not a Claude login.
    if status.logged_in || backend.env.contains_key("ANTHROPIC_BASE_URL") || backend.key.is_some() {
        return Ok((status, false));
    }
    let _ = writeln!(ctx.io.stderr, "Signing {instance} in to Claude Code — what follows is Claude's own login (`claude auth login`){}:", backend.config_dir.as_ref().map(|d| format!(", kept in {}", d.display())).unwrap_or_default());
    let _ = ctx.io.stderr.flush();
    let exit = claude_auth::login(&backend).map_err(|e| undo(fail("backend_failed", e)))?;
    let status = claude_auth::status(&backend).map_err(|e| undo(fail("backend_failed", e)))?;
    if !status.logged_in {
        let how = if exit.success() { "finished without signing in".to_string() } else { format!("stopped ({exit})") };
        return Err(undo(fail("not_authenticated", format!("`claude auth login` {how}, so {instance} was not added — run `krowk providers add claude{}` again to retry", instance.split_once(':').map(|(_, n)| format!(" --name {n}")).unwrap_or_default()))));
    }
    Ok((status, true))
}

fn private_dir(dir: &std::path::Path) -> std::io::Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        std::fs::DirBuilder::new().recursive(true).mode(0o700).create(dir)
    }
    #[cfg(not(unix))]
    std::fs::create_dir_all(dir)
}
