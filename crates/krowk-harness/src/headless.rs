//! `krowk -p`: one prompt, run headless through the in-process protocol.
//! The runner is a client like any other — it sends `Command::Prompt`,
//! reads `StreamLine`s, and on Ctrl-C sends `Command::Interrupt` — so what
//! it prints is exactly what the protocol says.
//!
//! - `text` prints the turn's final answer.
//! - `json` prints the `result` event.
//! - `stream-json` prints every line of the stream as it happens: the
//!   logged events exactly as the log has them, the live
//!   `item.started`/`item.delta` frames, and the `result` last.

use crate::engine::EngineError;
use crate::host::{Host, HostConfig};
use crate::protocol::{Command, LiveEvent, ModelRef, PermissionMode, RunResult, StreamLine};
use std::io::Write;
use tokio::sync::mpsc;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum OutputFormat {
    Text,
    Json,
    StreamJson,
}

impl OutputFormat {
    pub const NAMES: [&'static str; 3] = ["text", "json", "stream-json"];

    pub fn parse(s: &str) -> Option<OutputFormat> {
        Some(match s {
            "" | "text" => OutputFormat::Text,
            "json" => OutputFormat::Json,
            "stream-json" => OutputFormat::StreamJson,
            _ => return None,
        })
    }
}

pub struct Options {
    pub prompt: String,
    pub resume: Option<String>,
    pub model: Option<ModelRef>,
    pub permission_mode: PermissionMode,
    pub format: OutputFormat,
}

/// How the run came out. `result` is set whenever a turn ran, failed or
/// not; `error` is set when none could.
pub struct Outcome {
    pub session_id: Option<String>,
    pub result: Option<RunResult>,
    pub error: Option<EngineError>,
}

/// Runs the prompt to its end on a runtime of its own: the rest of krowk is
/// blocking, and this is the one place it waits on async work.
pub fn run(cfg: HostConfig, opts: Options, stdout: &mut dyn Write) -> Outcome {
    let rt = match tokio::runtime::Builder::new_current_thread().enable_all().build() {
        Ok(rt) => rt,
        Err(e) => return Outcome { session_id: None, result: None, error: Some(EngineError::new("runtime_unavailable", format!("the async runtime could not start: {e}"))) },
    };
    rt.block_on(drive(Host::new(cfg), opts, stdout))
}

async fn drive(host: Host, opts: Options, stdout: &mut dyn Write) -> Outcome {
    let format = opts.format;
    let (tx, mut rx) = mpsc::channel::<StreamLine>(1024);
    let cmd = Command::Prompt { session_id: opts.resume, text: opts.prompt, model: opts.model, permission_mode: opts.permission_mode };
    let exec = host.execute(cmd, tx);
    tokio::pin!(exec);
    let mut done: Option<Result<Option<RunResult>, EngineError>> = None;
    let mut session_id: Option<String> = None;
    let mut interrupts = 0u32;
    loop {
        let ctrl_c = tokio::signal::ctrl_c();
        tokio::select! {
            biased;
            Some(line) = rx.recv() => {
                if session_id.is_none() {
                    session_id = Some(line_session(&line).to_string());
                }
                if format == OutputFormat::StreamJson {
                    let _ = writeln!(stdout, "{}", serde_json::to_string(&line).expect("a stream line serializes"));
                    let _ = stdout.flush();
                }
            }
            r = &mut exec, if done.is_none() => done = Some(r),
            _ = ctrl_c, if done.is_none() => {
                interrupts += 1;
                // The first Ctrl-C asks the turn to stop and keeps what it
                // made; a second one does not wait.
                if interrupts > 1 {
                    std::process::exit(130);
                }
                if let Some(id) = &session_id {
                    let (itx, _irx) = mpsc::channel(1);
                    let _ = host.execute(Command::Interrupt { session_id: id.clone() }, itx).await;
                }
            }
            else => break,
        }
    }
    match done.expect("the loop ends only once the command has") {
        Ok(Some(result)) => {
            match format {
                OutputFormat::Text => {
                    if !result.result.is_empty() {
                        let _ = writeln!(stdout, "{}", result.result);
                    }
                }
                OutputFormat::Json => {
                    let _ = writeln!(stdout, "{}", serde_json::to_string(&LiveEvent::Result(result.clone())).expect("a result serializes"));
                }
                OutputFormat::StreamJson => {}
            }
            Outcome { session_id: Some(result.session_id.clone()), result: Some(result), error: None }
        }
        Ok(None) => Outcome { session_id, result: None, error: None },
        Err(e) => Outcome { session_id, result: None, error: Some(e) },
    }
}

fn line_session(line: &StreamLine) -> &str {
    match line {
        StreamLine::Log(ev) => &ev.session_id,
        StreamLine::Live(LiveEvent::ItemStarted { session_id, .. } | LiveEvent::ItemDelta { session_id, .. }) => session_id,
        StreamLine::Live(LiveEvent::Result(r)) => &r.session_id,
    }
}
