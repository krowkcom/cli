//! The Claude Code backend (R-BACK-1): krowk drives the user's own,
//! unmodified `claude` binary — the one compliant way to run a turn on a
//! Claude subscription rather than API tokens — as one long-lived process
//! per session:
//!
//! ```text
//! claude -p --input-format stream-json --output-format stream-json --verbose
//!        --include-partial-messages --permission-prompt-tool stdio
//!        --mcp-config '{"mcpServers":{"krowk":{"type":"sdk","name":"krowk"}}}' --strict-mcp-config
//!        --model <model> --permission-mode default|plan
//!        [--resume <claude session>] [--effort <level>]
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
//! | `can_use_tool` | claude → krowk | answered by krowk's own approval path (`approve`) |
//! | `mcp_message` | claude → krowk | a JSON-RPC message for the `krowk` MCP server, answered by `crate::bridge` |
//! | `interrupt` | krowk → claude | a `Command::Interrupt`; the turn ends at the next `result` and the process lives on |
//! | `set_model` | krowk → claude | a turn on another model of the same instance, when `initialize` listed models; otherwise a new process on `--resume` |
//!
//! Features are detected, never assumed from a version number: the
//! capabilities `system`/`init` announces (`interrupt_receipt_v1` — an
//! interrupt is acknowledged, so an unacknowledged one is given up on
//! sooner) and the `initialize` answer.
//!
//! **What krowk is asked, and what it is not.** Claude Code decides some
//! tool calls itself before it asks anyone: an allow rule in the user's or
//! the project's settings (`permissions.allow`), a `PreToolUse` hook that
//! approves, an agent's own `permissionMode`, and the read-only calls its
//! mode never asks about. Those run without krowk's `can_use_tool` — that
//! is Claude Code behaving as its user configured it, and ticket 09's rules
//! are where krowk takes them into account. What krowk does hold is the
//! mode: the process is started `--permission-mode default` (or `plan`),
//! so a `defaultMode` in settings cannot loosen it, and a turn whose
//! `system`/`init` reports a mode looser than krowk's is stopped before it
//! runs anything. Every call Claude Code does ask about is answered by
//! `approve`.
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
    // The mode is always named: left out, a `defaultMode` in the user's or
    // the project's settings (acceptEdits, bypassPermissions) would decide
    // it. Plan changes what Claude Code does, not only what it may do, so it
    // is Claude Code's to know; every looser krowk mode stays krowk's, and
    // what Claude Code asks is answered by `approve`.
    a.extend(["--permission-mode".into(), if l.plan { "plan" } else { "default" }.into()]);
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

/// A tool call Claude Code asks about, answered until the permission system
/// lands (ticket 9 replaces this evaluator). Calls Claude Code approves
/// itself — its settings' allow rules, its hooks — never reach here (see
/// the module's notes). The rule is the native loop's: reading needs no
/// mode, writing needs `acceptEdits`, running commands needs
/// `bypassPermissions`, and the file tools reach only inside the working
/// directory unless permissions are bypassed. No edit reaches `.git`,
/// `.claude` or the instance's own config directory (`protected`) unless
/// permissions are bypassed: Claude Code runs what their settings, hooks
/// and agents name. krowk's own bridged tools are always allowed. Anything
/// this build does not know is treated as running a command.
pub fn approve(mode: PermissionMode, tool: &str, input: &Value, cwd: &Path, protected: &[PathBuf]) -> Result<(), String> {
    let bypass = mode == PermissionMode::BypassPermissions;
    if tool.starts_with(&format!("mcp__{}__", bridge::SERVER)) {
        return Ok(());
    }
    let scope = crate::tools::Scope { cwd: cwd.to_path_buf(), bypass, protected: protected.to_vec() };
    let path = ["file_path", "notebook_path", "path"].iter().find_map(|k| input.get(*k).and_then(Value::as_str));
    // A glob can name where it searches as much as a path can: an absolute
    // `pattern` (Glob) or `glob` (Grep) is judged by its fixed part.
    let glob_key = match tool {
        "Glob" => Some("pattern"),
        "Grep" => Some("glob"),
        _ => None,
    };
    let globbed = glob_key.and_then(|k| input.get(k).and_then(Value::as_str)).filter(|g| Path::new(g).is_absolute()).map(glob_root);
    match tool {
        "Read" | "Grep" | "Glob" | "LS" | "NotebookRead" => {
            if let Some(g) = &globbed {
                scope.path(g)?;
            }
            path.map(|p| scope.path(p).map(drop)).unwrap_or(Ok(()))
        }
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

/// The fixed part of an absolute glob: the directories before the first
/// component that holds a wildcard.
fn glob_root(g: &str) -> String {
    let mut root = PathBuf::new();
    for c in Path::new(g).components() {
        if c.as_os_str().to_string_lossy().contains(['*', '?', '[', '{']) {
            break;
        }
        root.push(c);
    }
    root.display().to_string()
}

/// How loose a Claude Code permission mode is, against krowk's: plan asks
/// before everything, default asks before edits and commands, acceptEdits
/// only before commands; auto, bypassPermissions and any mode this build
/// does not know ask before nothing krowk can count on.
fn looser_than(claude: &str, krowk: PermissionMode) -> bool {
    let rank = match claude {
        "plan" => 0,
        "default" | "manual" | "dontAsk" => 1,
        "acceptEdits" => 2,
        _ => 3,
    };
    let allowed = match krowk {
        PermissionMode::Plan => 0,
        PermissionMode::Default => 1,
        PermissionMode::AcceptEdits => 2,
        PermissionMode::BypassPermissions => 3,
    };
    rank > allowed
}

/// The ambient variables a Claude Code process does not inherit unless its
/// instance sets them: they are the native `anthropic` instance's key and
/// base URL, and inherited they would move a subscription account onto an
/// API key or another server without anyone asking for it. An instance
/// sets a base URL in its `env` and a key through `apiKeyEnv`, which krowk
/// reads and hands the process itself.
pub const NOT_INHERITED: &[&str] = &["ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"];

/// What of `NOT_INHERITED` to remove for this instance.
pub fn cleared(b: &Backend) -> impl Iterator<Item = &'static str> + '_ {
    NOT_INHERITED.iter().copied().filter(|k| !b.env.contains_key(*k))
}

/// The environment a Claude Code command gets on top of krowk's own: the
/// inherited key and base URL taken away, the config directory, the
/// instance's `env`, and its key under the name Claude Code reads.
pub fn environment(b: &Backend) -> (Vec<&'static str>, Vec<(String, String)>) {
    let mut set: Vec<(String, String)> = Vec::new();
    if let Some(dir) = &b.config_dir {
        set.push(("CLAUDE_CONFIG_DIR".into(), dir.display().to_string()));
    }
    set.extend(b.env.iter().map(|(k, v)| (k.clone(), v.clone())));
    if let Some((to, key)) = &b.key {
        set.push((to.clone(), key.clone()));
    }
    (cleared(b).collect(), set)
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
            let ask = Answers {
                session_id: ctx.session_id.clone(),
                turn_id: ctx.turn_id.clone(),
                model: ctx.model.clone(),
                cwd: ctx.cwd.clone(),
                mode: ctx.permission_mode,
                krowk_version: self.krowk_version.clone(),
                protected: self.backend().home.iter().cloned().collect(),
            };
            // The running process serves this turn when its launch still
            // fits; a model it can switch to in place is switched to.
            if let Some(p) = slot.as_mut() {
                // The mode it is in counts too: a plan turn that approved
                // ExitPlanMode leaves Claude Code in default, which the next
                // plan turn must not inherit (nor a default turn an
                // EnterPlanMode's plan). It is launched in one of two modes,
                // and serves only a turn that wants the one it is in.
                let mode_fits = p.mode.is_empty() || (want.plan == (p.mode == "plan") && !looser_than(&p.mode, ctx.permission_mode));
                let fits = p.alive() && mode_fits && p.launch.plan == want.plan && p.launch.effort == want.effort;
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
    /// The instance's config directory: no edit reaches it.
    protected: Vec<PathBuf>,
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
                match approve(self.mode, tool, &input, &self.cwd, &self.protected) {
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
    /// The process group: Claude Code and whatever it started.
    pid: Option<u32>,
    /// The permission mode its last turn reported; empty before one has.
    mode: String,
    /// The group was stopped and reaped; nothing is left to signal.
    group_done: bool,
}

/// How long a stopped process group gets between SIGTERM and SIGKILL.
const TERM_GRACE: Duration = Duration::from_secs(2);
/// After Claude Code exits, how long what it already wrote is read.
const DRAIN_GRACE: Duration = Duration::from_millis(500);

#[cfg(unix)]
fn signal_group(pid: Option<u32>, sig: i32) {
    if let Some(pid) = pid.and_then(|p| i32::try_from(p).ok()).filter(|p| *p > 0) {
        // SAFETY: a signal to the group this process made (process_group(0)).
        unsafe {
            libc::kill(-pid, sig);
        }
    }
}

impl Drop for Proc {
    /// A process let go of without `shutdown` — a host dropped mid-turn — is
    /// killed with everything it started, never left running.
    fn drop(&mut self) {
        if !self.group_done {
            #[cfg(unix)]
            signal_group(self.pid, libc::SIGKILL);
            crate::group::release(self.pid);
        }
    }
}

/// The next JSON line, or none at the end of the stream. A line that is not
/// JSON (a warning a wrapper printed) is skipped.
async fn next_json(out: &mut Lines<BufReader<ChildStdout>>) -> Option<Value> {
    loop {
        match out.next_line().await {
            Ok(Some(l)) => {
                if let Ok(v) = serde_json::from_str::<Value>(&l) {
                    return Some(v);
                }
            }
            _ => return None,
        }
    }
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
        let (remove, set) = environment(b);
        for k in remove {
            cmd.env_remove(k);
        }
        cmd.envs(set);
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
        let pid = child.id();
        crate::group::register(pid);
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
        let mut p = Proc { child, stdin, out, stderr, binary: b.binary.clone(), launch, set_model: false, next: 0, exited: false, pid, group_done: false, mode: String::new() };
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
                let Some(msg) = next_json(&mut self.out).await else { return Err(self.died(&format!("before it answered {subtype}"))) };
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
                self.terminate().await;
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
        let mut gone = false;
        let mut drain: Option<tokio::time::Instant> = None;
        let result = loop {
            let d = deadline;
            let until = async move {
                match d {
                    Some(d) => tokio::time::sleep_until(d).await,
                    None => std::future::pending().await,
                }
            };
            let dr = drain;
            let drained = async move {
                match dr {
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
                    self.terminate().await;
                    let mut out = Vec::new();
                    t.finish(&mut out);
                    forward(events, out).await;
                    return Ok(TurnEnd::Interrupted);
                }
                // Claude Code exited — crashed, or was killed — while
                // something it started still holds its output open: what it
                // wrote is read for a moment, then the turn ends.
                _ = self.child.wait(), if !gone => {
                    gone = true;
                    drain = Some(tokio::time::Instant::now() + DRAIN_GRACE);
                }
                _ = drained => {
                    let mut out = Vec::new();
                    t.finish(&mut out);
                    forward(events, out).await;
                    let e = self.died("before the turn finished");
                    return if interrupted { Ok(TurnEnd::Interrupted) } else { Err(e) };
                }
                msg = next_json(&mut self.out) => {
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
                            // An approved ExitPlanMode or EnterPlanMode moves
                            // Claude Code to default or plan for what follows.
                            if msg.pointer("/request/subtype").and_then(Value::as_str) == Some("can_use_tool")
                                && answer.pointer("/response/response/behavior").and_then(Value::as_str) == Some("allow")
                            {
                                match msg.pointer("/request/tool_name").and_then(Value::as_str) {
                                    Some("ExitPlanMode") => self.mode = "default".into(),
                                    Some("EnterPlanMode") => self.mode = "plan".into(),
                                    _ => {}
                                }
                            }
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
                                self.mode.clone_from(&init.permission_mode);
                                // A mode looser than krowk's — a setting, a
                                // wrapper — is stopped before it runs anything.
                                if !init.permission_mode.is_empty() && looser_than(&init.permission_mode, ask.mode) {
                                    self.terminate().await;
                                    return Err(EngineError::new(
                                        "backend_permission_mode",
                                        format!(
                                            "Claude Code on {instance} came up in its `{}` permission mode, looser than krowk's `{}` for this turn, so krowk stopped it before it ran anything — check `permissions.defaultMode` in its settings and the instance's `args`, or rerun krowk with a mode that allows it",
                                            init.permission_mode,
                                            permission_name(ask.mode)
                                        ),
                                    ));
                                }
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

    /// Stops the whole process group — Claude Code and every shell and
    /// server it started: SIGTERM, a moment to write what it was writing,
    /// then SIGKILL.
    async fn terminate(&mut self) {
        self.exited = true;
        if self.group_done {
            return;
        }
        #[cfg(unix)]
        {
            signal_group(self.pid, libc::SIGTERM);
            let _ = tokio::time::timeout(TERM_GRACE, self.child.wait()).await;
            signal_group(self.pid, libc::SIGKILL);
        }
        let _ = self.child.kill().await;
        self.group_done = true;
        crate::group::release(self.pid);
    }

    async fn kill(mut self) {
        self.terminate().await;
    }

    /// Asks the process to exit by closing its stdin, gives it a moment, and
    /// then stops what is left of its group, so nothing it started outlives
    /// the session.
    async fn shutdown(mut self) {
        drop(self.stdin.take());
        let _ = tokio::time::timeout(EXIT_GRACE, self.child.wait()).await;
        self.terminate().await;
    }
}

fn permission_name(m: PermissionMode) -> &'static str {
    PermissionMode::NAMES[match m {
        PermissionMode::Default => 0,
        PermissionMode::AcceptEdits => 1,
        PermissionMode::Plan => 2,
        PermissionMode::BypassPermissions => 3,
    }]
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
        assert!(s.contains(r#"{"mcpServers":{"krowk":{"name":"krowk","type":"sdk"}}} --strict-mcp-config --model haiku --resume cc-1 --effort high --permission-mode default --add-dir /x"#) || s.contains(r#"{"mcpServers":{"krowk":{"type":"sdk","name":"krowk"}}} --strict-mcp-config --model haiku --resume cc-1 --effort high --permission-mode default --add-dir /x"#), "{s}");
        assert!(s.contains("--permission-mode default --add-dir"), "the mode is always named, so settings cannot loosen it: {s}");
        assert!(args(&Launch { plan: true, resume: None, effort: None, ..l }, &[]).join(" ").ends_with("--model haiku --permission-mode plan"));
        assert!(looser_than("bypassPermissions", PermissionMode::AcceptEdits) && looser_than("acceptEdits", PermissionMode::Default) && looser_than("auto", PermissionMode::AcceptEdits));
        assert!(looser_than("default", PermissionMode::Plan) && looser_than("somethingNew", PermissionMode::AcceptEdits));
        assert!(!looser_than("default", PermissionMode::Default) && !looser_than("plan", PermissionMode::Plan) && !looser_than("bypassPermissions", PermissionMode::BypassPermissions));
        assert_eq!([Effort::None, Effort::Medium, Effort::Max].map(effort_level), ["low", "medium", "max"]);
    }

    #[test]
    fn r_back_1_approvals_follow_krowks_modes_and_its_own_tools_always_run() {
        let cwd = std::env::temp_dir().join(format!("krowk-approve-{}", std::process::id()));
        std::fs::create_dir_all(&cwd).unwrap();
        let cwd = cwd.canonicalize().unwrap();
        let approve = |m, t: &str, i: &Value, c: &Path| super::approve(m, t, i, c, &[]);
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
        // A glob that names another directory is judged like a path.
        assert!(approve(d, "Glob", &json!({"pattern": "/etc/**/*.conf"}), &cwd).unwrap_err().contains("outside the working directory"));
        assert!(approve(d, "Grep", &json!({"pattern": "x", "glob": "/root/*"}), &cwd).unwrap_err().contains("outside the working directory"));
        assert!(approve(d, "Glob", &json!({"pattern": "**/*.rs"}), &cwd).is_ok() && approve(d, "Grep", &json!({"pattern": "/etc/"}), &cwd).is_ok(), "a relative glob, and grep's regex, are not paths");
        assert!(approve(d, "Glob", &json!({"pattern": format!("{}/src/**", cwd.display())}), &cwd).is_ok());
        assert!(approve(PermissionMode::BypassPermissions, "Glob", &json!({"pattern": "/etc/*"}), &cwd).is_ok());
        // R-BACK-6's other half: an edit that would make Claude Code run
        // something — its settings, hooks, agents — needs bypassPermissions.
        let ae = PermissionMode::AcceptEdits;
        for f in [".claude/settings.json", ".claude/settings.local.json", ".claude/agents/x.md", "sub/.claude/hooks/h.sh", ".Claude/settings.json", ".CLAUDE./agents/a.md", ".claude /settings.json"] {
            assert!(approve(ae, "Write", &json!({"file_path": f}), &cwd).unwrap_err().contains(".claude"), "{f}");
        }
        let config = cwd.join("cc-config");
        std::fs::create_dir_all(&config).unwrap();
        let err = super::approve(ae, "Edit", &json!({"file_path": config.join("settings.json").display().to_string()}), &cwd, std::slice::from_ref(&config)).unwrap_err();
        assert!(err.contains("config directory"), "{err}");
        assert!(approve(ae, "Edit", &json!({"file_path": ".GIT/config"}), &cwd).unwrap_err().contains(".git"));
        let err = super::approve(ae, "Write", &json!({"file_path": cwd.join("CC-Config/settings.json").display().to_string()}), &cwd, std::slice::from_ref(&config)).unwrap_err();
        assert!(err.contains("config directory"), "the config directory in another case: {err}");
        assert!(super::approve(PermissionMode::BypassPermissions, "Write", &json!({"file_path": ".claude/settings.json"}), &cwd, &[config]).is_ok());
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
