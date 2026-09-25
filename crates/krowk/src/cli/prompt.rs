//! `krowk -p "…"`: one prompt, answered headless by krowk's own engine, then
//! projected into krowk.db so `krowk sessions` lists it. The engine, the log
//! and the output formats live in `krowk-harness`; this is the command line
//! around them — flags, the prompt from stdin, the config, the exit code.

use super::{sessions, Ctx};
use crate::pricing;
use krowk_api::{fail, Error};
use krowk_harness::headless::{self, OutputFormat};
use krowk_harness::host::HostConfig;
use krowk_harness::instances::{self, Registry};
use krowk_harness::log;
use krowk_harness::protocol::{Effort, PermissionMode, TurnStatus, Usage};
use krowk_harness::trust;
use std::io::{IsTerminal, Read};
use std::sync::Arc;

pub(super) fn run(ctx: &mut Ctx, positionals: &[String]) -> Result<(), Error> {
    sessions::check_os()?;
    if ctx.filter.is_some() || !ctx.f.format.is_empty() || ctx.f.json {
        return Err(fail("bad_flag", "-p prints its own output — pick it with --output-format text|json|stream-json instead of --format, --json or --jq"));
    }
    let format = OutputFormat::parse(&ctx.f.output_format).ok_or_else(|| {
        fail("bad_flag", format!("--output-format {} is not one krowk -p writes — one of {}", ctx.f.output_format, OutputFormat::NAMES.join(", ")))
    })?;
    let permission_mode = match ctx.f.permission_mode.as_str() {
        "" => PermissionMode::Default,
        m => PermissionMode::parse(m)
            .ok_or_else(|| fail("bad_flag", format!("--permission-mode {m} is not a mode — one of {}", PermissionMode::NAMES.join(", "))))?,
    };
    let prompt = prompt_text(positionals)?;
    let registry = Registry::resolve(&load_instances()?, ctx.io.env);
    let model = match ctx.f.model.as_str() {
        "" => None,
        m => Some(registry.parse_model(m).map_err(|e| fail("bad_flag", format!("--model: {e}")))?),
    };
    let sessions_dir = log::sessions_dir(ctx.io.env)
        .ok_or_else(|| fail("store_unavailable", "no home directory in environment: set HOME (or XDG_DATA_HOME to an absolute path) so sessions have a place to live"))?;
    let resume = match ctx.f.resume.as_str() {
        "" => None,
        r => Some(resolve_resume(ctx, &sessions_dir, r)?),
    };
    let cwd = std::env::current_dir().map_err(|e| fail("no_directory", format!("the working directory cannot be read: {e}")))?;
    let toolset = toolset_flag(ctx)?;
    let effort = effort_flag(ctx)?;
    let cfg = HostConfig {
        sessions_dir,
        cwd,
        registry,
        krowk_version: super::VERSION.into(),
        pricer: pricer(ctx.io.env),
        catalog: catalog(ctx.io.env),
        credentials: super::providers::credentials_path(),
        trust: trust_gate(ctx.f.trust, super::interactive(ctx) && std::io::stdin().is_terminal() && ctx.io.err_tty, Some(ctx.env("HOME")).filter(|h| !h.trim().is_empty()).map(std::path::PathBuf::from)),
    };
    let opts = headless::Options { prompt, resume, model, permission_mode, toolset, effort, format };
    let outcome = headless::run(cfg, opts, ctx.io.stdout);
    let _ = ctx.io.stdout.flush();

    // The log is the session; krowk.db is its projection, brought up to date
    // now so the listing has it. A store that cannot take it costs the
    // listing, never the answer — `krowk sessions sync` catches up.
    if let Some(id) = &outcome.session_id
        && let Err(e) = sessions::project_native(ctx, id)
    {
        let _ = writeln!(ctx.io.stderr, "! the session is saved, but krowk.db was not updated: {} — `krowk sessions sync` retries", e.fix());
    }
    if let Some(e) = outcome.error {
        return Err(engine_error(&e.code, &e.message, e.status));
    }
    match outcome.result {
        Some(r) if r.status == TurnStatus::Failed => Err(match r.error {
            Some(e) => engine_error(&e.code, &e.message, e.http_status.unwrap_or(0)),
            None => fail("turn_failed", "the turn failed"),
        }),
        Some(r) if r.status == TurnStatus::Interrupted => {
            Err(fail("interrupted", format!("the turn was interrupted; what it produced is kept — continue with `krowk -p --resume {} …`", r.session_id)))
        }
        _ => Ok(()),
    }
}

/// The arguments; stdin only when there are none and it is not a terminal
/// (`git diff | krowk -p`). Never both: an agent that spawns krowk with a
/// stdin it never closes would otherwise hang a prompt it passed as words.
fn prompt_text(positionals: &[String]) -> Result<String, Error> {
    let mut prompt = positionals.join(" ");
    let stdin = std::io::stdin();
    if prompt.trim().is_empty() && !stdin.is_terminal() {
        // A closed or unreadable stdin is simply no input.
        let _ = stdin.lock().take(8 << 20).read_to_string(&mut prompt);
    }
    if prompt.trim().is_empty() {
        return Err(fail("empty_prompt", "-p needs a prompt: `krowk -p \"what does this repo do?\"`, or pipe one in"));
    }
    Ok(prompt.trim_end().to_string())
}

/// The harness's part of the global config.json, whose `workspace` key the
/// rest of krowk reads. A file that does not parse is an error: somebody
/// wrote it meaning something.
/// `--effort`, a rung of the harness's ladder.
pub(super) fn effort_flag(ctx: &Ctx) -> Result<Option<Effort>, Error> {
    match ctx.f.effort.trim() {
        "" => Ok(None),
        e => Ok(Some(Effort::parse(e).ok_or_else(|| fail("bad_flag", format!("--effort {e} is not a rung of the ladder — one of {}", Effort::names().join(", "))))?)),
    }
}

/// `--toolset`, checked against the presets the harness has.
pub(super) fn toolset_flag(ctx: &Ctx) -> Result<Option<String>, Error> {
    match ctx.f.toolset.trim() {
        "" => Ok(None),
        t if krowk_harness::toolset::by_name(t).is_some() => Ok(Some(t.to_string())),
        t => Err(fail("bad_flag", format!("--toolset {t} is not a toolset — one of {}", krowk_harness::toolset::names().join(", ")))),
    }
}

pub(super) fn load_instances() -> Result<instances::InstancesConfig, Error> {
    instances_from(&config_json()?)
}

pub(super) fn instances_from(v: &serde_json::Value) -> Result<instances::InstancesConfig, Error> {
    instances::from_config_json(v).map_err(|e| fail("bad_config", format!("{}: {e}", crate::config::global_path().display())))
}

/// The global config.json as JSON, or an empty object when there is none.
pub(super) fn config_json() -> Result<serde_json::Value, Error> {
    let path = crate::config::global_path();
    let raw = match std::fs::read(&path) {
        Ok(raw) => raw,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(serde_json::json!({})),
        Err(e) => return Err(fail("bad_config", format!("reading {}: {e}", path.display()))),
    };
    serde_json::from_slice(&raw).map_err(|e| fail("bad_config", format!("{} is not valid JSON: {e}", path.display())))
}

/// `--resume` takes the session id a result names, or anything `krowk
/// sessions show` takes that resolves to a krowk session.
pub(super) fn resolve_resume(ctx: &Ctx, sessions_dir: &std::path::Path, reference: &str) -> Result<String, Error> {
    let reference = reference.trim();
    if log::valid_id(reference) && sessions_dir.join(reference).join(log::EVENTS_FILE).is_file() {
        return Ok(reference.to_string());
    }
    let conn = sessions::open_store(ctx)?;
    let id = sessions::resolve_arg(ctx, &conn, &[reference.to_string()], "show")?;
    let d = sessions::load_by_id(ctx, &conn, &id)?;
    if d.session.harness != krowk_harness::project::HARNESS {
        return Err(fail(
            "no_session",
            format!("{reference:?} is a {} session — `krowk -p --resume` continues krowk's own sessions only", if d.session.harness.is_empty() { "foreign" } else { &d.session.harness }),
        ));
    }
    Ok(d.session.foreign_session_id)
}

/// R-BACK-6: `claude -p` runs a repository's hooks and MCP servers without
/// the trust dialog Claude Code shows on a terminal, so krowk asks its own
/// before a backend is spawned. A repository trusted before, or `--trust`,
/// goes ahead; a person at the terminal is asked, and a yes is remembered;
/// anything headless is refused. The home directory and `/` are never
/// offered: only `--trust`, for one run, starts a backend there. Nothing is
/// spawned until this answers.
fn trust_gate(flag: bool, ask: bool, home: Option<std::path::PathBuf>) -> trust::Gate {
    let store = trust::Store::new(krowk_api::creds::config_dir().join(trust::FILE), home);
    Arc::new(move |root: &std::path::Path| {
        if flag || store.trusts(root) {
            return Ok(());
        }
        if let Some(why) = store.refuses(root) {
            return Err(trust::untrusted(root, &format!("It cannot be trusted for good — {why}. Pass --trust to run there this once, or run krowk -p from a repository of its own.")));
        }
        if !ask {
            return Err(trust::untrusted(root, "Look at what it would run, then pass --trust to run it anyway, or run krowk -p there once on a terminal and answer its prompt."));
        }
        let runs = trust::what_runs(root);
        use std::io::Write as _;
        let mut stderr = std::io::stderr();
        let _ = writeln!(stderr, "Claude Code (`claude -p`) runs a repository's own hooks and MCP servers without asking.");
        if runs.is_empty() {
            let _ = writeln!(stderr, "{} has none of those files now, but it is not a repository you have trusted.", root.display());
        } else {
            let _ = writeln!(stderr, "{} has {}.", root.display(), runs.join(", "));
        }
        match inquire::Confirm::new(&format!("Trust {} and run Claude Code in it?", root.display())).with_default(false).prompt() {
            Ok(true) => {
                if let Err(e) = store.trust(root) {
                    let _ = writeln!(stderr, "! trusted for this run, but not remembered: {e}");
                }
                Ok(())
            }
            _ => Err(trust::untrusted(root, "Nothing was run.")),
        }
    })
}

/// Prices a model call from the models.dev cache or the embedded snapshot,
/// as every figure `krowk sessions` shows is priced. The environment is
/// captured now: the engine runs on its own thread.
pub(super) fn pricer(env: &dyn Fn(&str) -> String) -> krowk_harness::host::Pricer {
    let (cache, home) = (env("XDG_CACHE_HOME"), env("HOME"));
    Arc::new(move |provider: &str, model: &str, u: &Usage| {
        let env = |k: &str| match k {
            "XDG_CACHE_HOME" => cache.clone(),
            "HOME" => home.clone(),
            _ => String::new(),
        };
        let rates = pricing::price(&env, provider, model)?;
        Some(rates.cost(pricing::Tokens {
            input: u.input_tokens,
            output: u.output_tokens,
            cache_read: u.cache_read_tokens,
            cache_write: u.cache_write_tokens,
            reasoning: u.reasoning_tokens,
        }))
    })
}

/// What the models.dev cache says of a model — its family, limits,
/// efforts and wire API (R-PROV-2). Read from the cache only: the embedded
/// snapshot is trimmed to prices, and a model it would miss is read for its
/// family off its id instead. Captured like the pricer's environment.
pub(super) fn catalog(env: &dyn Fn(&str) -> String) -> krowk_harness::host::Catalog {
    let (cache, home) = (env("XDG_CACHE_HOME"), env("HOME"));
    Arc::new(move |provider: &str, model: &str| {
        let env = |k: &str| match k {
            "XDG_CACHE_HOME" => cache.clone(),
            "HOME" => home.clone(),
            _ => String::new(),
        };
        let raw = std::fs::read(pricing::cache_path(&env)?).ok()?;
        krowk_harness::catalog::lookup(&raw, provider, model)
    })
}

/// An engine failure as krowk's error: the code and its fix, with the HTTP
/// status the provider answered, so the exit code classifies it the way it
/// classifies a registry failure.
pub(super) fn engine_error(code: &str, message: &str, status: u16) -> Error {
    Error { status, ..fail(code, message) }
}
