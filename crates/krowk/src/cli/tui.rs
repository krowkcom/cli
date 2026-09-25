//! Bare `krowk` on a terminal: the inline TUI (R-PKG-1). The TUI itself is
//! `krowk-tui`; this is the command line around it — when it opens, the
//! flags it takes (`--model`, `--permission-mode`, `--toolset`, `--resume [id]`), the
//! config it reads, and the session it leaves in krowk.db.

use super::flags::Flags;
use super::{prompt, sessions, Ctx, Io};
use crate::output::Format;
use krowk_api::{fail, Error};
use krowk_harness::host::HostConfig;
use krowk_harness::instances::Registry;
use krowk_harness::log;
use krowk_harness::protocol::PermissionMode;

/// Whether this invocation opens the TUI: nothing asked but `krowk` itself
/// (and its TUI flags), the human format, and a person at both ends of the
/// terminal — stdin to type into and stdout to draw on. A dumb terminal
/// cannot draw it. Anything else keeps what bare `krowk` always did.
pub(super) fn wanted(io: &Io, f: &Flags, format: Format, positionals: &[String], jq_given: bool) -> bool {
    positionals.is_empty()
        && !f.help
        && !f.version
        && !f.print
        && !f.quiet
        && !jq_given
        && format == Format::Human
        && io.tty
        && io.stdin_tty
        && (io.env)("TERM") != "dumb"
}

pub(super) fn run(ctx: &mut Ctx) -> Result<(), Error> {
    sessions::check_os()?;
    let permission_mode = match ctx.f.permission_mode.as_str() {
        "" => PermissionMode::Default,
        m => PermissionMode::parse(m)
            .ok_or_else(|| fail("bad_flag", format!("--permission-mode {m} is not a mode — one of {}", PermissionMode::NAMES.join(", "))))?,
    };
    let config = prompt::config_json()?;
    let registry = Registry::resolve(&prompt::instances_from(&config)?, ctx.io.env);
    let model = match ctx.f.model.as_str() {
        "" => None,
        m => Some(registry.parse_model(m).map_err(|e| fail("bad_flag", format!("--model: {e}")))?),
    };
    let sessions_dir = log::sessions_dir(ctx.io.env)
        .ok_or_else(|| fail("store_unavailable", "no home directory in environment: set HOME (or XDG_DATA_HOME to an absolute path) so sessions have a place to live"))?;
    let resume = if ctx.f.resume_pick {
        Some(pick(ctx)?)
    } else {
        match ctx.f.resume.as_str() {
            "" => None,
            r => Some(prompt::resolve_resume(ctx, &sessions_dir, r)?),
        }
    };
    let (settings, notices) = krowk_tui::settings::from_config(&config);
    let cwd = std::env::current_dir().map_err(|e| fail("no_directory", format!("the working directory cannot be read: {e}")))?;
    // Beside krowk.db and the session logs, so it goes where they go.
    let history_file = sessions_dir.parent().map(|d| d.join("tui-history.jsonl"));
    let toolset = prompt::toolset_flag(ctx)?;
    let host = HostConfig {
        sessions_dir,
        cwd,
        registry,
        krowk_version: super::VERSION.into(),
        pricer: prompt::pricer(ctx.io.env),
        families: prompt::families(ctx.io.env),
    };
    let outcome = krowk_tui::run(krowk_tui::Options {
        host,
        resume,
        model,
        permission_mode,
        toolset,
        settings,
        history_file,
        notices,
        version: super::VERSION.into(),
    });
    // As after `krowk -p`: the log is the session, krowk.db its listing.
    if let Some(id) = &outcome.session_id
        && let Err(e) = sessions::project_native(ctx, id)
    {
        let _ = writeln!(ctx.io.stderr, "! the session is saved, but krowk.db was not updated: {} — `krowk sessions sync` retries", e.fix());
    }
    // Left without waiting for a turn (a second Ctrl-C, SIGTERM or SIGHUP):
    // recorded above, and exits the way an interrupted command does.
    if outcome.abandoned {
        let _ = ctx.io.stdout.flush();
        std::process::exit(130);
    }
    match outcome.error {
        Some(e) => Err(fail("tui_failed", e)),
        None => Ok(()),
    }
}

/// `krowk --resume`: the sessions picker, over krowk's own sessions.
fn pick(ctx: &Ctx) -> Result<String, Error> {
    let conn = sessions::open_store(ctx)?;
    let rows = krowk_store::list_sessions(&conn, krowk_harness::project::HARNESS, "", sessions::DEFAULT_SESSION_LIMIT as i64)
        .map_err(|e| sessions::store_fail(&e, &sessions::db_path_string(ctx)))?;
    if rows.is_empty() {
        return Err(fail("no_session", "there is no krowk session to resume yet — run `krowk` to start one"));
    }
    let id = sessions::pick_session(&rows, krowk_store::now_ms())?;
    let d = sessions::load_by_id(ctx, &conn, &id)?;
    Ok(d.session.foreign_session_id)
}
