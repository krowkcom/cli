//! The Claude Code backend (R-BACK-1): krowk drives the user's own,
//! unmodified `claude` binary — the one compliant way to run a turn on a
//! Claude subscription rather than API tokens — as one long-lived process
//! per session:
//!
//! ```text
//! claude -p --input-format stream-json --output-format stream-json --verbose
//!        --include-partial-messages --permission-prompt-tool stdio
//!        --mcp-config '{"mcpServers":{"krowk":{"type":"sdk","name":"krowk"}}}' --strict-mcp-config
//!        --model <model> [--resume <claude session>] [--effort <level>] [--permission-mode plan]
//! ```
//!
//! Each prompt is one `user` line on the process's stdin, and its turn is
//! everything up to the `result` line; the stream in between is translated
//! into krowk's events by `stream`, so a backend turn lands in the same log
//! in the same shape as a native one (R-BACK-5). The process stays up
//! between turns of the session, kept by the host; a new krowk process
//! starts a new one on `--resume` with the Claude session id the log holds.
//!
//! **The control protocol** rides the same pipes, as `control_request` /
//! `control_response` lines. It is not publicly documented; the reference is
//! Anthropic's open-source `claude-agent-sdk-python` (`_internal/query.py`),
//! and what this build speaks was checked against `claude` 2.1.280:
//!
//! | subtype | direction | what krowk does |
//! |---|---|---|
//! | `initialize` | krowk → claude | first, on every process; its answer lists the `models`, which is how in-place model switching is detected |
//! | `can_use_tool` | claude → krowk | answered by krowk's own approval path (`approve`), never by Claude Code's |
//! | `mcp_message` | claude → krowk | a JSON-RPC message for the `krowk` MCP server, answered by `crate::bridge` |
//! | `interrupt` | krowk → claude | a `Command::Interrupt`; the turn ends at the next `result` and the process lives on |
//! | `set_model` | krowk → claude | a turn on another model of the same instance, when `initialize` listed models; otherwise a new process on `--resume` |
//!
//! Features are detected, never assumed from a version number: the
//! capabilities `system`/`init` announces (`interrupt_receipt_v1` — an
//! interrupt is acknowledged, so an unacknowledged one is given up on
//! sooner) and the `initialize` answer.
//!
//! **Compliance** (R-BACK-2) is structural: krowk starts the binary as the
//! user installed it, sends it no system prompt and no headers, sets only
//! `CLAUDE_CONFIG_DIR` and the instance's own environment, and never opens
//! Claude's credentials — the login is Claude Code's, made with `claude auth
//! login` and reported by `claude auth status` (`auth`). A compliance test
//! holds every source file and every run to that.

pub mod auth;
pub mod stream;

use crate::bridge::{self, BridgeEnv};
use crate::engine::{cancelled, BoxFuture, Engine, EngineError, EngineEvent, Events, TurnContext, TurnEnd};
use crate::instances::{Backend, Resolved};
use crate::protocol::{Billing, Effort, Item, ModelRef, PermissionMode, ToolDefinition, WireApi};
use serde_json::{json, Value};
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use stream::{Init, Translator};
use tokio::io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader, Lines};
use tokio::process::{Child, ChildStdin, ChildStdout};

/// The binary a `claude-code` instance runs when its definition names none.
pub const BINARY: &str = "claude";
/// The backend's name in the log.
pub const BACKEND: &str = "claude-code";

/// How long the process has to answer `initialize`: long enough for a cold
/// start with plugins and hooks, short enough that a binary that is not
/// Claude Code is named rather than waited on.
const INITIALIZE_TIMEOUT: Duration = Duration::from_secs(60);
/// After an interrupt, how long the turn waits for Claude Code's `result`
/// before the process is stopped; the session continues on `--resume`.
const INTERRUPT_GRACE: Duration = Duration::from_secs(10);
/// With `interrupt_receipt_v1`, how long an interrupt may go unacknowledged.
const RECEIPT_GRACE: Duration = Duration::from_secs(5);
/// How long a process that was asked to exit (stdin closed) gets.
const EXIT_GRACE: Duration = Duration::from_secs(5);
/// The end of stderr kept for a failure's words.
const STDERR_TAIL: usize = 4096;

/// What a turn's context record says of the system prompt: it is Claude
/// Code's, and krowk neither sees nor changes it.
pub const SYSTEM_NOTE: &str = "(Claude Code's own system prompt: krowk sends none and does not see it)";

/// The launch settings a process is bound to. A turn that needs others —
/// plan mode, another effort, or another model where `set_model` is not
/// available — gets a new process on `--resume`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Launch {
    pub model: String,
    pub resume: Option<String>,
    pub effort: Option<&'static str>,
    pub plan: bool,
}

/// The `--mcp-config` that injects krowk's tools: one in-process (`sdk`)
/// server, answered over the control protocol.
pub fn mcp_config() -> String {
    json!({"mcpServers": {bridge::SERVER: {"type": "sdk", "name": bridge::SERVER}}}).to_string()
}

/// The whole argument list, krowk's own first and the instance's after.
pub fn args(l: &Launch, extra: &[String]) -> Vec<String> {
    let mut a: Vec<String> = [
        "-p",
        "--input-format",
        "stream-json",
        "--output-format",
        "stream-json",
        "--verbose",
        "--include-partial-messages",
        "--permission-prompt-tool",
        "stdio",
        "--mcp-config",
    ]
    .iter()
    .map(|s| s.to_string())
    .collect();
    a.push(mcp_config());
    a.push("--strict-mcp-config".into());
    a.extend(["--model".into(), l.model.clone()]);
    if let Some(r) = &l.resume {
        a.extend(["--resume".into(), r.clone()]);
    }
    if let Some(e) = l.effort {
        a.extend(["--effort".into(), e.into()]);
    }
    // Plan mode changes what Claude Code does, not only what it may do, so
    // it is Claude Code's to know. Every other mode stays krowk's: approvals
    // are asked of krowk and answered by `approve`.
    if l.plan {
        a.extend(["--permission-mode".into(), "plan".into()]);
    }
    a.extend(extra.iter().cloned());
    a
}

/// krowk's ladder onto Claude Code's `--effort` levels (low … max): below
/// low is low, since Claude Code has no switch for thinking off.
pub fn effort_level(e: Effort) -> &'static str {
    match e {
        Effort::None | Effort::Minimal | Effort::Low => "low",
        Effort::Medium => "medium",
        Effort::High => "high",
        Effort::Xhigh => "xhigh",
        Effort::Max => "max",
    }
}

/// Where Claude Code keeps a session's transcript: `projects/<cwd with
/// every character but ASCII letters and digits made a dash>/<id>.jsonl` in
/// its config directory — else wherever a transcript of that id already is
/// (a long path is shortened in newer versions).
pub fn transcript_path(config_dir: &Path, cwd: &str, session_id: &str) -> PathBuf {
    let projects = config_dir.join("projects");
    let slug: String = cwd.chars().map(|c| if c.is_ascii_alphanumeric() { c } else { '-' }).collect();
    let file = format!("{session_id}.jsonl");
    let direct = projects.join(&slug).join(&file);
    if direct.exists() {
        return direct;
    }
    std::fs::read_dir(&projects)
        .ok()
        .and_then(|dirs| dirs.flatten().map(|d| d.path().join(&file)).find(|p| p.is_file()))
        .unwrap_or(direct)
}

/// A tool call's approval, until the permission system lands (ticket 9
/// replaces this evaluator; the question already comes to krowk). The rule
/// is the native loop's: reading needs no mode, writing needs
/// `acceptEdits`, running commands needs `bypassPermissions`, and the file
/// tools reach only inside the working directory unless permissions are
/// bypassed. krowk's own bridged tools are always allowed. Anything this
/// build does not know is treated as running a command.
pub fn approve(mode: PermissionMode, tool: &str, input: &Value, cwd: &Path) -> Result<(), String> {
    let bypass = mode == PermissionMode::BypassPermissions;
    if tool.starts_with(&format!("mcp__{}__", bridge::SERVER)) {
        return Ok(());
    }
    let scope = crate::tools::Scope { cwd: cwd.to_path_buf(), bypass };
    let path = ["file_path", "notebook_path", "path"].iter().find_map(|k| input.get(*k).and_then(Value::as_str));
    match tool {
        "Read" | "Grep" | "Glob" | "LS" | "NotebookRead" => path.map(|p| scope.path(p).map(drop)).unwrap_or(Ok(())),
        "TodoWrite" | "ToolSearch" | "Task" | "Agent" | "EnterPlanMode" | "ExitPlanMode" => Ok(()),
        "Write" | "Edit" | "MultiEdit" | "NotebookEdit" => {
            if !matches!(mode, PermissionMode::AcceptEdits | PermissionMode::BypassPermissions) {
                return Err(format!("{tool} changes files, which this session does not allow: until krowk's permission rules land, edits run only when krowk is started with `--permission-mode acceptEdits` or `bypassPermissions`. Say what you would change instead."));
            }
            path.map(|p| scope.edit_path(p).map(drop)).unwrap_or(Ok(()))
        }
        "AskUserQuestion" if !bypass => Err("krowk is running this turn without a person to ask: decide, say what you assumed, and carry on.".into()),
        _ if bypass => Ok(()),
        _ => Err(format!("{tool} is not allowed in this session: until krowk's permission rules land, it runs only when krowk is started with `--permission-mode bypassPermissions`. Use Read, Grep and Glob, or ask the person to rerun with that flag.")),
    }
}

/// The engine for one session on one `claude-code` instance. The host keeps
/// it between turns, so the process it starts serves the whole session.
pub struct ClaudeEngine {
    instance: Resolved,
    krowk_version: String,
    proc: tokio::sync::Mutex<Option<Proc>>,
}

impl ClaudeEngine {
    pub fn new(instance: Resolved, krowk_version: &str) -> Result<ClaudeEngine, EngineError> {
        if instance.backend.is_none() {
            return Err(EngineError::new("bad_config", format!("{} is not a Claude Code instance", instance.name)));
        }
        Ok(ClaudeEngine { instance, krowk_version: krowk_version.into(), proc: tokio::sync::Mutex::new(None) })
    }

    fn backend(&self) -> &Backend {
        self.instance.backend.as_ref().expect("checked in new")
    }
}

impl Engine for ClaudeEngine {
    fn provider(&self) -> &str {
        &self.instance.provider
    }

    fn wire_api(&self) -> WireApi {
        WireApi::ClaudeCode
    }

    fn run_turn<'a>(&'a self, mut ctx: TurnContext, events: Events) -> BoxFuture<'a, Result<TurnEnd, EngineError>> {
        Box::pin(async move {
            let mut slot = self.proc.lock().await;
            let want = Launch {
                model: ctx.model.model.clone(),
                resume: ctx.backend_session.clone(),
                effort: ctx.effort.map(effort_level),
                plan: ctx.permission_mode == PermissionMode::Plan,
            };
            let ask = Answers { session_id: ctx.session_id.clone(), turn_id: ctx.turn_id.clone(), model: ctx.model.clone(), cwd: ctx.cwd.clone(), mode: ctx.permission_mode, krowk_version: self.krowk_version.clone() };
            // The running process serves this turn when its launch still
            // fits; a model it can switch to in place is switched to.
            if let Some(p) = slot.as_mut() {
                let fits = p.alive() && p.launch.plan == want.plan && p.launch.effort == want.effort;
                let switched = fits && (p.launch.model == want.model || (p.set_model && p.set_model(&want.model, &ask).await));
                if !switched && let Some(old) = slot.take() {
                    old.shutdown().await;
                }
            }
            if slot.is_none() {
                *slot = Some(Proc::spawn(self.backend(), &ctx.cwd, want, &ask).await?);
            }
            let p = slot.as_mut().expect("spawned above");
            let prompt = match ctx.history.last().map(|h| &h.item) {
                Some(Item::UserText { text }) => text.clone(),
                _ => return Err(EngineError::new("empty_prompt", "a backend turn needs the prompt as its last item")),
            };
            let outcome = p.turn(&prompt, &mut ctx, &ask, &events, self.backend(), &self.instance.name).await;
            // A process that died, or a turn that failed partway, is not
            // trusted with the next turn: that one starts clean on --resume.
            if (outcome.is_err() || !p.alive())
                && let Some(old) = slot.take()
            {
                old.kill().await;
            }
            outcome
        })
    }

    fn shutdown(&self) -> BoxFuture<'_, ()> {
        Box::pin(async move {
            if let Some(p) = self.proc.lock().await.take() {
                p.shutdown().await;
            }
        })
    }
}

/// What a control request is answered with: the turn it arrived in.
struct Answers {
    session_id: String,
    turn_id: String,
    model: ModelRef,
    cwd: PathBuf,
    mode: PermissionMode,
    krowk_version: String,
}

impl Answers {
    /// The answer to one of Claude Code's control requests.
    fn answer(&self, request_id: &str, req: &Value) -> Value {
        let subtype = req.get("subtype").and_then(Value::as_str).unwrap_or_default();
        let success = |response: Value| json!({"type": "control_response", "response": {"subtype": "success", "request_id": request_id, "response": response}});
        match subtype {
            "can_use_tool" => {
                let tool = req.get("tool_name").and_then(Value::as_str).unwrap_or_default();
                let input = req.get("input").cloned().unwrap_or_else(|| json!({}));
                match approve(self.mode, tool, &input, &self.cwd) {
                    Ok(()) => success(json!({"behavior": "allow", "updatedInput": input})),
                    Err(message) => success(json!({"behavior": "deny", "message": message})),
                }
            }
            "mcp_message" if req.get("server_name").and_then(Value::as_str) == Some(bridge::SERVER) => {
                let env = BridgeEnv { session_id: &self.session_id, turn_id: &self.turn_id, model: &self.model, cwd: &self.cwd, backend: BACKEND, krowk_version: &self.krowk_version };
                success(json!({"mcp_response": bridge::handle(req.get("message").unwrap_or(&Value::Null), &env)}))
            }
            // No hooks are registered, so none should be called back.
            "hook_callback" => success(json!({})),
            other => json!({"type": "control_response", "response": {"subtype": "error", "request_id": request_id, "error": format!("krowk does not answer the control request {other:?}")}}),
        }
    }
}

/// One running `claude` process.
struct Proc {
    child: Child,
    stdin: Option<ChildStdin>,
    out: Lines<BufReader<ChildStdout>>,
    stderr: Arc<Mutex<String>>,
    binary: String,
    launch: Launch,
    /// `initialize` listed models: `set_model` switches in place.
    set_model: bool,
    next: u64,
    exited: bool,
}

fn tail(s: &str) -> String {
    let s = s.trim();
    let cut = s.char_indices().rev().nth(STDERR_TAIL).map(|(i, _)| i).unwrap_or(0);
    s[cut..].to_string()
}

impl Proc {
    async fn spawn(b: &Backend, cwd: &Path, launch: Launch, ask: &Answers) -> Result<Proc, EngineError> {
        let mut cmd = tokio::process::Command::new(b.path.as_deref().unwrap_or(Path::new(&b.binary)));
        cmd.args(args(&launch, &b.args)).current_dir(cwd).stdin(Stdio::piped()).stdout(Stdio::piped()).stderr(Stdio::piped()).kill_on_drop(true);
        if let Some(dir) = &b.config_dir {
            cmd.env("CLAUDE_CONFIG_DIR", dir);
        }
        cmd.envs(&b.env);
        // Its own process group: a Ctrl-C at the terminal is krowk's to turn
        // into an interrupt, not a signal that kills Claude Code mid-write.
        #[cfg(unix)]
        cmd.process_group(0);
        let mut child = cmd.spawn().map_err(|e| {
            if e.kind() == std::io::ErrorKind::NotFound {
                EngineError::new("backend_not_found", format!("{} was not found — install Claude Code (https://claude.com/claude-code), or name the binary with `krowk providers add claude --binary <path>`", b.binary))
            } else {
                EngineError::new("backend_failed", format!("{} could not be started: {e}", b.binary))
            }
        })?;
        let stdin = child.stdin.take();
        let out = BufReader::new(child.stdout.take().expect("piped")).lines();
        let stderr = Arc::new(Mutex::new(String::new()));
        if let Some(mut err) = child.stderr.take() {
            let keep = stderr.clone();
            tokio::spawn(async move {
                let mut buf = [0u8; 4096];
                while let Ok(n) = err.read(&mut buf).await {
                    if n == 0 {
                        break;
                    }
                    let mut s = keep.lock().unwrap_or_else(|e| e.into_inner());
                    s.push_str(&String::from_utf8_lossy(&buf[..n]));
                    if s.len() > 4 * STDERR_TAIL {
                        let t = tail(&s);
                        *s = t;
                    }
                }
            });
        }
        let mut p = Proc { child, stdin, out, stderr, binary: b.binary.clone(), launch, set_model: false, next: 0, exited: false };
        let init = p.request(json!({"subtype": "initialize", "hooks": null}), ask, INITIALIZE_TIMEOUT).await?;
        p.set_model = init.get("models").is_some_and(Value::is_array);
        Ok(p)
    }

    fn alive(&mut self) -> bool {
        !self.exited && matches!(self.child.try_wait(), Ok(None))
    }

    fn died(&mut self, while_doing: &str) -> EngineError {
        self.exited = true;
        let status = self.child.try_wait().ok().flatten().map(|s| format!(" ({s})")).unwrap_or_default();
        let said = tail(&self.stderr.lock().unwrap_or_else(|e| e.into_inner()));
        let said = if said.is_empty() { String::new() } else { format!(": {said}") };
        EngineError::new("backend_exited", format!("claude exited{status} {while_doing}{said} — the session is kept; continue it with --resume"))
    }

    async fn send(&mut self, v: &Value) -> Result<(), EngineError> {
        let mut line = v.to_string();
        line.push('\n');
        let Some(stdin) = self.stdin.as_mut() else { return Err(self.died("before krowk could write to it")) };
        if stdin.write_all(line.as_bytes()).await.is_err() || stdin.flush().await.is_err() {
            return Err(self.died("while krowk was writing to it"));
        }
        Ok(())
    }

    /// The next JSON line, or none at the end of the stream. A line that is
    /// not JSON (a warning a wrapper printed) is skipped.
    async fn recv(&mut self) -> Option<Value> {
        loop {
            match self.out.next_line().await {
                Ok(Some(l)) => {
                    if let Ok(v) = serde_json::from_str::<Value>(&l) {
                        return Some(v);
                    }
                }
                _ => return None,
            }
        }
    }

    fn request_id(&mut self) -> String {
        self.next += 1;
        format!("krowk_{}", self.next)
    }

    /// Sends a control request and waits for its answer, answering Claude
    /// Code's own requests meanwhile — `initialize` is answered only after
    /// the MCP server has been.
    async fn request(&mut self, req: Value, ask: &Answers, within: Duration) -> Result<Value, EngineError> {
        let id = self.request_id();
        let subtype = req["subtype"].as_str().unwrap_or_default().to_string();
        self.send(&json!({"type": "control_request", "request_id": id, "request": req})).await?;
        let wait = async {
            loop {
                let Some(msg) = self.recv().await else { return Err(self.died(&format!("before it answered {subtype}"))) };
                match msg["type"].as_str() {
                    Some("control_request") => {
                        let answer = ask.answer(msg["request_id"].as_str().unwrap_or_default(), &msg["request"]);
                        self.send(&answer).await?;
                    }
                    Some("control_response") if msg.pointer("/response/request_id").and_then(Value::as_str) == Some(id.as_str()) => {
                        let r = &msg["response"];
                        if r["subtype"] == "error" {
                            return Err(EngineError::new("backend_failed", format!("claude refused {subtype}: {}", r["error"].as_str().unwrap_or("no reason given"))));
                        }
                        return Ok(r.get("response").cloned().unwrap_or(Value::Null));
                    }
                    _ => {}
                }
            }
        };
        let answered = tokio::time::timeout(within, wait).await;
        match answered {
            Ok(r) => r,
            Err(_) => {
                let _ = self.child.start_kill();
                self.exited = true;
                Err(EngineError::new("backend_unresponsive", format!("{} did not answer {subtype} within {} seconds — is it Claude Code?", self.binary, within.as_secs())))
            }
        }
    }

    /// Switches the model in place; false when Claude Code refused, and the
    /// caller starts a new process instead.
    async fn set_model(&mut self, model: &str, ask: &Answers) -> bool {
        match self.request(json!({"subtype": "set_model", "model": model}), ask, INITIALIZE_TIMEOUT).await {
            Ok(_) => {
                self.launch.model = model.into();
                true
            }
            Err(_) => false,
        }
    }

    /// Runs one turn: the prompt in, the stream out, until `result`.
    async fn turn(&mut self, prompt: &str, ctx: &mut TurnContext, ask: &Answers, events: &Events, b: &Backend, instance: &str) -> Result<TurnEnd, EngineError> {
        self.send(&json!({"type": "user", "message": {"role": "user", "content": prompt}, "parent_tool_use_id": null, "session_id": ""})).await?;
        let mut t = Translator::default();
        let mut interrupted = false;
        let mut receipt_required = false;
        let mut receipt: Option<String> = None;
        let mut deadline: Option<tokio::time::Instant> = None;
        let mut announced = false;
        let result = loop {
            let d = deadline;
            let until = async move {
                match d {
                    Some(d) => tokio::time::sleep_until(d).await,
                    None => std::future::pending().await,
                }
            };
            tokio::select! {
                biased;
                _ = cancelled(&mut ctx.cancel), if !interrupted => {
                    interrupted = true;
                    let id = self.request_id();
                    receipt = Some(id.clone());
                    self.send(&json!({"type": "control_request", "request_id": id, "request": {"subtype": "interrupt"}})).await?;
                    deadline = Some(tokio::time::Instant::now() + if receipt_required { RECEIPT_GRACE } else { INTERRUPT_GRACE });
                }
                _ = until => {
                    // Claude Code did not stop in time: the process goes, the
                    // turn keeps what it made, the session continues on --resume.
                    let _ = self.child.start_kill();
                    self.exited = true;
                    let mut out = Vec::new();
                    t.finish(&mut out);
                    forward(events, out).await;
                    return Ok(TurnEnd::Interrupted);
                }
                msg = self.recv() => {
                    let Some(msg) = msg else {
                        let mut out = Vec::new();
                        t.finish(&mut out);
                        forward(events, out).await;
                        let e = self.died("before the turn finished");
                        if interrupted {
                            return Ok(TurnEnd::Interrupted);
                        }
                        return Err(e);
                    };
                    match msg["type"].as_str() {
                        Some("control_request") => {
                            let answer = ask.answer(msg["request_id"].as_str().unwrap_or_default(), &msg["request"]);
                            self.send(&answer).await?;
                        }
                        Some("control_response") => {
                            // The interrupt's receipt: once acknowledged, the
                            // result is waited for the full grace.
                            if receipt.is_some() && msg.pointer("/response/request_id").and_then(Value::as_str) == receipt.as_deref() {
                                receipt = None;
                                deadline = Some(tokio::time::Instant::now() + INTERRUPT_GRACE);
                            }
                        }
                        _ => {
                            let out = t.apply(&msg)?;
                            forward(events, out).await;
                            if !announced && let Some(init) = t.init.clone() {
                                announced = true;
                                receipt_required = init.capabilities.iter().any(|c| c == "interrupt_receipt_v1");
                                announce(events, &init, b, instance).await?;
                            }
                            if let Some(o) = t.outcome.take() {
                                break o;
                            }
                        }
                    }
                }
            }
        };
        if interrupted {
            return Ok(TurnEnd::Interrupted);
        }
        if result.subtype == "success" && !result.is_error {
            return Ok(TurnEnd::Completed);
        }
        let said = if !result.text.trim().is_empty() { result.text.trim().to_string() } else if !result.errors.is_empty() { result.errors.join("; ") } else { result.subtype.clone() };
        let lower = said.to_lowercase();
        if lower.contains("/login") || lower.contains("not logged in") || lower.contains("invalid api key") || result.api_status == Some(401) {
            let add = match instance.split_once(':') {
                Some((_, name)) => format!("krowk providers add claude --name {name}"),
                None => "krowk providers add claude".into(),
            };
            return Err(EngineError::new("not_authenticated", format!("Claude Code is not signed in for the {instance} instance ({said}) — sign in with `{add}`, which runs Claude's own login")).with_status(401));
        }
        Err(EngineError::new("backend_failed", format!("Claude Code could not finish the turn: {said}")).with_status(result.api_status.unwrap_or(0)))
    }

    async fn kill(mut self) {
        let _ = self.child.kill().await;
    }

    /// Asks the process to exit by closing its stdin, and stops it if it
    /// does not.
    async fn shutdown(mut self) {
        drop(self.stdin.take());
        if tokio::time::timeout(EXIT_GRACE, self.child.wait()).await.is_err() {
            let _ = self.child.kill().await;
        }
    }
}

async fn forward(events: &Events, out: Vec<EngineEvent>) {
    for ev in out {
        let _ = events.send(ev).await;
    }
}

/// The turn's context and the Claude session behind it, from `init`.
async fn announce(events: &Events, init: &Init, b: &Backend, instance: &str) -> Result<(), EngineError> {
    let bridged = bridge::definitions();
    let tools = init
        .tools
        .iter()
        .map(|name| bridged.iter().find(|d| &d.name == name).cloned().unwrap_or_else(|| ToolDefinition { name: name.clone(), description: String::new(), input_schema: json!({}), grammar: None }))
        .collect();
    let _ = events.send(EngineEvent::Context { system: SYSTEM_NOTE.into(), tools }).await;
    if let Some((_, status)) = init.mcp_servers.iter().find(|(n, _)| n == bridge::SERVER)
        && status != "connected"
    {
        return Err(EngineError::new("backend_failed", format!("Claude Code on {instance} did not connect krowk's tools (the krowk MCP server is {status})")));
    }
    let billing = match init.api_key_source.as_str() {
        "" => None,
        "none" => Some(Billing::Subscription),
        _ => Some(Billing::ApiKey),
    };
    let transcript = (!init.session_id.is_empty()).then(|| b.home.as_ref().map(|h| transcript_path(h, &init.cwd, &init.session_id).display().to_string())).flatten();
    if !init.session_id.is_empty() {
        let _ = events.send(EngineEvent::BackendSession { backend: BACKEND.into(), session_id: init.session_id.clone(), transcript, billing }).await;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn r_back_1_the_process_is_launched_with_the_stream_json_protocol_and_krowks_mcp_server() {
        let l = Launch { model: "haiku".into(), resume: Some("cc-1".into()), effort: Some("high"), plan: false };
        let a = args(&l, &["--add-dir".into(), "/x".into()]);
        let s = a.join(" ");
        assert!(s.starts_with("-p --input-format stream-json --output-format stream-json --verbose --include-partial-messages --permission-prompt-tool stdio --mcp-config "), "{s}");
        assert!(s.contains(r#"{"mcpServers":{"krowk":{"name":"krowk","type":"sdk"}}} --strict-mcp-config --model haiku --resume cc-1 --effort high --add-dir /x"#) || s.contains(r#"{"mcpServers":{"krowk":{"type":"sdk","name":"krowk"}}} --strict-mcp-config --model haiku --resume cc-1 --effort high --add-dir /x"#), "{s}");
        assert!(!s.contains("--permission-mode"), "approvals are krowk's: no mode is handed over but plan");
        assert!(args(&Launch { plan: true, resume: None, effort: None, ..l }, &[]).join(" ").ends_with("--model haiku --permission-mode plan"));
        assert_eq!([Effort::None, Effort::Medium, Effort::Max].map(effort_level), ["low", "medium", "max"]);
    }

    #[test]
    fn r_back_1_approvals_follow_krowks_modes_and_its_own_tools_always_run() {
        let cwd = std::env::temp_dir().join(format!("krowk-approve-{}", std::process::id()));
        std::fs::create_dir_all(&cwd).unwrap();
        let d = PermissionMode::Default;
        assert!(approve(d, "mcp__krowk__session_info", &json!({}), &cwd).is_ok());
        assert!(approve(d, "Read", &json!({"file_path": "README.md"}), &cwd).is_ok());
        assert!(approve(d, "Read", &json!({"file_path": "/etc/passwd"}), &cwd).unwrap_err().contains("outside the working directory"));
        assert!(approve(d, "Edit", &json!({"file_path": "a.txt"}), &cwd).unwrap_err().contains("acceptEdits"));
        assert!(approve(PermissionMode::AcceptEdits, "Edit", &json!({"file_path": "a.txt"}), &cwd).is_ok());
        assert!(approve(PermissionMode::AcceptEdits, "Write", &json!({"file_path": ".git/config"}), &cwd).unwrap_err().contains(".git"));
        assert!(approve(PermissionMode::AcceptEdits, "Bash", &json!({"command": "ls"}), &cwd).unwrap_err().contains("bypassPermissions"));
        assert!(approve(PermissionMode::BypassPermissions, "Bash", &json!({"command": "ls"}), &cwd).is_ok());
        assert!(approve(d, "WebFetch", &json!({"url": "https://x"}), &cwd).is_err(), "an unknown tool is a command");
        let _ = std::fs::remove_dir_all(&cwd);
    }

    #[test]
    fn r_back_5_the_transcript_is_found_where_claude_code_keeps_it() {
        let home = std::env::temp_dir().join(format!("krowk-transcript-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&home);
        assert_eq!(transcript_path(&home, "/tmp/a.b/c", "s1"), home.join("projects/-tmp-a-b-c/s1.jsonl"), "where it will be");
        std::fs::create_dir_all(home.join("projects/-shortened-123")).unwrap();
        std::fs::write(home.join("projects/-shortened-123/s2.jsonl"), "").unwrap();
        assert_eq!(transcript_path(&home, "/a/very/long/path", "s2"), home.join("projects/-shortened-123/s2.jsonl"), "or where it is");
        let _ = std::fs::remove_dir_all(&home);
    }
}
