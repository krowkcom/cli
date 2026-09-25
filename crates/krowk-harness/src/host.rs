//! The host: where commands are executed and the log is written. Clients
//! talk to it only through `Command` and `StreamLine`, so the in-process
//! client `krowk -p` uses today and the daemon's socket clients later speak
//! the same protocol. The in-process transport is a function call and two
//! channels.
//!
//! The host is the one writer of a session's log (R-SYNC-2): an engine
//! reports events, and the host gives each logged one its id and parent,
//! appends it, and only then forwards it — a client never sees an event the
//! log does not have.

use crate::anthropic::AnthropicClient;
use crate::catalog::ModelInfo;
use crate::chat::{ChatClient, Credential};
use crate::engine::{Engine, EngineError, EngineEvent, HistoryItem, Steers, TurnContext, TurnEnd};
use crate::instances::{Auth, Registry, Resolved};
use crate::oauth;
use crate::openai::ResponsesClient;
use crate::log::{self, LogError, SessionLog};
use crate::native::{self, NativeEngine};
use crate::toolset;
use crate::protocol::{
    Billing, Command, ContextRecord, Effort, Item, LiveEvent, LogBody, LogEvent, ModelRef, PermissionMode, RunResult, StreamLine, TurnStatus, Usage, WireApi,
};
use crate::claude::ClaudeEngine;
use crate::trust;
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Instant;
use tokio::sync::{mpsc, watch};

/// Prices a model call: (provider, model, usage) to USD, or none when the
/// model has no price. Supplied by the caller, which owns the price cache.
pub type Pricer = Arc<dyn Fn(&str, &str, &Usage) -> Option<f64> + Send + Sync>;

/// What the catalog knows of a model: (provider, model) to its family,
/// limits, efforts and wire API, or none when the catalog does not know it.
/// The family picks the toolset preset, the wire API the client, and the
/// efforts what the ladder maps onto. Supplied by the caller, which owns the
/// models.dev cache.
pub type Catalog = Arc<dyn Fn(&str, &str) -> Option<ModelInfo> + Send + Sync>;

pub struct HostConfig {
    /// Where session logs live: `log::sessions_dir`.
    pub sessions_dir: PathBuf,
    /// The working directory a new session starts in. A resumed session
    /// keeps the one it started in, so its system prompt stays byte-identical.
    pub cwd: PathBuf,
    pub registry: Registry,
    pub krowk_version: String,
    pub pricer: Pricer,
    pub catalog: Catalog,
    /// krowk's provider credentials file, where OAuth logins live.
    pub credentials: PathBuf,
    /// Asked before a backend is spawned in a repository (R-BACK-6).
    pub trust: trust::Gate,
}

pub struct Host {
    cfg: HostConfig,
    /// Each session with a turn running: its cancel switch and the queue
    /// its steering waits in.
    running: Mutex<HashMap<String, Running>>,
    /// Each session's backend engine, with the instance it runs on: its
    /// process outlives a turn and serves the session's next one.
    backends: Mutex<HashMap<String, Backend>>,
    /// How long a session's backend process is kept without a turn.
    backend_idle: std::time::Duration,
}

struct Running {
    cancel: watch::Sender<bool>,
    steers: Steers,
}

/// A session's backend engine, the instance it was made for, and when a
/// turn last finished on it.
struct Backend {
    instance: String,
    engine: Arc<dyn Engine>,
    used: Instant,
}

/// A backend process kept this long without a turn is let go: a long-lived
/// host (the daemon, the TUI) holds many sessions, and an idle `claude` is a
/// few hundred megabytes. The next turn starts it again on `--resume`.
pub const BACKEND_IDLE: std::time::Duration = std::time::Duration::from_secs(15 * 60);

fn log_failure(e: LogError) -> EngineError {
    match e {
        LogError::NotFound(m) => EngineError::new("no_session", m),
        LogError::Busy(m) => EngineError::new("session_busy", m),
        LogError::Io(m) => EngineError::new("session_log_failed", m),
    }
}

impl Host {
    pub fn new(cfg: HostConfig) -> Host {
        Host { cfg, running: Mutex::new(HashMap::new()), backends: Mutex::new(HashMap::new()), backend_idle: BACKEND_IDLE }
    }

    /// Keeps an idle session's backend process this long instead of
    /// `BACKEND_IDLE`.
    pub fn with_backend_idle(self, idle: std::time::Duration) -> Host {
        Host { backend_idle: idle, ..self }
    }

    /// Lets go of every backend process idle for longer than the host keeps
    /// one, except a session's with a turn running. Swept when a prompt
    /// arrives, so an idle host costs nothing to keep tidy.
    async fn evict_idle(&self) {
        let running: Vec<String> = self.running.lock().unwrap_or_else(|e| e.into_inner()).keys().cloned().collect();
        let idle: Vec<Arc<dyn Engine>> = {
            let mut backends = self.backends.lock().unwrap_or_else(|e| e.into_inner());
            let stale: Vec<String> = backends.iter().filter(|(id, b)| b.used.elapsed() >= self.backend_idle && !running.contains(id)).map(|(id, _)| id.clone()).collect();
            stale.iter().filter_map(|id| backends.remove(id)).map(|b| b.engine).collect()
        };
        for e in idle {
            e.shutdown().await;
        }
    }

    /// Lets every backend process go cleanly. A host dropped without this
    /// still stops them (they are killed with their handles), just less
    /// politely.
    pub async fn shutdown(&self) {
        let engines: Vec<Arc<dyn Engine>> = self.backends.lock().unwrap_or_else(|e| e.into_inner()).drain().map(|(_, b)| b.engine).collect();
        for e in engines {
            e.shutdown().await;
        }
    }

    /// The session's backend engine on `instance`: the one already running
    /// it, else a new one (and a process started by its first turn).
    fn backend_for(&self, session_id: &str, instance: &Resolved) -> Result<Arc<dyn Engine>, EngineError> {
        let mut backends = self.backends.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(b) = backends.get(session_id)
            && b.instance == instance.name
        {
            return Ok(b.engine.clone());
        }
        // A session moved to another instance: the old engine's process is
        // stopped with it when the last handle goes (its drop kills it).
        let e: Arc<dyn Engine> = Arc::new(ClaudeEngine::new(instance.clone(), &self.cfg.krowk_version)?);
        backends.insert(session_id.to_string(), Backend { instance: instance.name.clone(), engine: e.clone(), used: Instant::now() });
        Ok(e)
    }

    pub fn registry(&self) -> &Registry {
        &self.cfg.registry
    }

    /// Executes one command. A `prompt` streams its events to `out` and
    /// answers with its result — a turn that failed is still a result, with
    /// `isError`. An error here means no turn ran.
    pub async fn execute(&self, cmd: Command, out: mpsc::Sender<StreamLine>) -> Result<Option<RunResult>, EngineError> {
        match cmd {
            Command::Prompt { session_id, text, model, permission_mode, toolset, effort } => {
                self.prompt(session_id.as_deref(), text, model, permission_mode, toolset.as_deref(), effort, out).await.map(Some)
            }
            Command::Interrupt { session_id } => {
                let running = self.running.lock().unwrap_or_else(|e| e.into_inner());
                match running.get(&session_id) {
                    Some(r) => {
                        let _ = r.cancel.send(true);
                        Ok(None)
                    }
                    None => Err(EngineError::new("no_running_turn", format!("session {session_id} has no turn running, so there is nothing to interrupt"))),
                }
            }
            // Queued for the engine's next step; it comes back in the log as
            // a `userText` item where the turn took it.
            Command::Steer { session_id, text } => {
                if text.trim().is_empty() {
                    return Err(EngineError::new("empty_prompt", "the steering text is empty"));
                }
                let running = self.running.lock().unwrap_or_else(|e| e.into_inner());
                match running.get(&session_id) {
                    Some(r) if r.steers.push(text).is_ok() => Ok(None),
                    // The turn has taken its last input and is ending.
                    Some(_) => Err(EngineError::new("turn_ending", format!("the turn in session {session_id} is finishing and reads no more input — send it as the next prompt"))),
                    None => Err(EngineError::new("no_running_turn", format!("session {session_id} has no turn running to steer — send it as a prompt instead"))),
                }
            }
            Command::Approve { .. } | Command::SwitchModel { .. } | Command::Fork { .. } => {
                Err(EngineError::new("not_implemented", "this command is part of the protocol but not served by this build yet"))
            }
        }
    }

    #[allow(clippy::too_many_arguments)]
    async fn prompt(
        &self,
        session_id: Option<&str>,
        text: String,
        model: Option<ModelRef>,
        permission_mode: PermissionMode,
        toolset: Option<&str>,
        effort: Option<Effort>,
        out: mpsc::Sender<StreamLine>,
    ) -> Result<RunResult, EngineError> {
        let started = Instant::now();
        self.evict_idle().await;
        if text.trim().is_empty() {
            return Err(EngineError::new("empty_prompt", "the prompt is empty — pass it as an argument, or on stdin"));
        }
        // Everything that can refuse the prompt is settled before a new
        // session is created, so a refusal leaves no empty session behind.
        let opened = match session_id {
            Some(id) => Some(SessionLog::open(&self.cfg.sessions_dir, id).map_err(log_failure)?),
            None => None,
        };
        let past = opened.as_ref().map(|(log, events)| replay(&log::branch(events, log.head().unwrap_or_default()))).unwrap_or_default();
        let model = match model {
            Some(m) => m,
            None => match past.model.clone() {
                Some(m) => m,
                None => self.cfg.registry.default_model().map_err(|e| EngineError::new("bad_config", e))?,
            },
        };
        let instance = self.cfg.registry.get(&model.instance).map_err(|e| EngineError::new("no_instance", e))?.clone();
        let info = (self.cfg.catalog)(&instance.provider, &model.model);
        let family = info.as_ref().and_then(|i| i.family.clone());
        let (preset, _) = toolset::choose(toolset, self.cfg.registry.toolset.as_deref(), family.as_deref(), &model.model).map_err(|e| EngineError::new("bad_toolset", e))?;
        let wire = instance.wire_for(info.as_ref().and_then(|i| i.wire_api));
        let cwd_before = past.cwd.clone().unwrap_or_else(|| self.cfg.cwd.clone());
        let native = match &instance.backend {
            None => Some(engine_for(&instance, wire, &self.cfg.credentials, &self.cfg.krowk_version)?),
            // A backend runs the repository's own hooks and MCP servers, so
            // it is not started in one nobody trusted; and a binary that is
            // not there is named before a session exists for it.
            Some(b) => {
                if b.path.is_none() {
                    return Err(EngineError::new(
                        "backend_not_found",
                        format!("{} was not found — install Claude Code (https://claude.com/claude-code), or name the binary with `krowk providers add claude --binary <path>`", b.binary),
                    ));
                }
                (self.cfg.trust)(&trust::root(&cwd_before))?;
                None
            }
        };
        let effort = effort.or(instance.effort);
        let mut log = match opened {
            Some((log, _)) => log,
            None => {
                let (log, root) = SessionLog::create(&self.cfg.sessions_dir, &self.cfg.cwd, &self.cfg.krowk_version).map_err(log_failure)?;
                let _ = out.send(StreamLine::Log(root)).await;
                log
            }
        };
        let session_id = log.session_id.clone();
        let cwd = cwd_before;
        let engine: Arc<dyn Engine> = match native {
            Some(e) => Arc::from(e),
            None => self.backend_for(&session_id, &instance)?,
        };
        // The Claude session to resume: the branch's last, if it ran on this
        // instance — another account's config directory does not have it.
        let backend_session = past.backend.as_ref().filter(|b| b.instance == model.instance).map(|b| b.session_id.clone());

        let turn_id = krowk_store::new_id();
        let mut w = Writer { log: &mut log, out: &out, turn_id: turn_id.clone(), preset, wire, backend: past.backend.clone() };
        w.log(LogBody::TurnStarted { turn_id: turn_id.clone(), model: model.clone(), provider: engine.provider().into(), wire_api: engine.wire_api(), permission_mode, effort }).await?;
        let prompt_item = Item::UserText { text };
        w.log(LogBody::ItemCompleted { turn_id: turn_id.clone(), item_id: krowk_store::new_id(), item: prompt_item.clone() }).await?;
        let mut history = past.items;
        history.push(HistoryItem { item: prompt_item, response: None });

        let (cancel_tx, cancel) = watch::channel(false);
        let steers = Steers::default();
        self.running.lock().unwrap_or_else(|e| e.into_inner()).insert(session_id.clone(), Running { cancel: cancel_tx, steers: steers.clone() });
        let ctx = TurnContext { session_id: session_id.clone(), turn_id: turn_id.clone(), model: model.clone(), history, cwd, permission_mode, preset, effort, model_info: info, cancel, steers: steers.clone(), backend_session };
        let mut tally = Tally::default();
        let outcome = w.drive(engine.as_ref(), ctx, &mut tally, &model, &instance, &self.cfg.pricer).await;
        // Refused from here on, not queued for a turn that is over; what an
        // interrupted or failed turn never took goes back on its result.
        let unread_steers = steers.close();
        self.running.lock().unwrap_or_else(|e| e.into_inner()).remove(&session_id);
        if let Some(b) = self.backends.lock().unwrap_or_else(|e| e.into_inner()).get_mut(&session_id) {
            b.used = Instant::now();
        }

        let (status, error) = match outcome {
            Ok(TurnEnd::Completed) => (TurnStatus::Completed, None),
            Ok(TurnEnd::Interrupted) => (TurnStatus::Interrupted, None),
            Err(e) => (TurnStatus::Failed, Some(e.info())),
        };
        let duration_ms = started.elapsed().as_millis() as u64;
        w.log(LogBody::TurnCompleted { turn_id: turn_id.clone(), status, usage: tally.usage, duration_ms, error: error.clone() }).await?;
        log.sync().map_err(log_failure)?;
        let result = RunResult {
            session_id,
            turn_id,
            status,
            is_error: status == TurnStatus::Failed,
            result: tally.last_text,
            model,
            usage: tally.usage,
            cost_usd: if tally.unpriced { None } else { Some(tally.cost) },
            duration_ms,
            num_model_calls: tally.calls,
            error,
            unread_steers,
        };
        let _ = out.send(StreamLine::Live(LiveEvent::Result(result.clone()))).await;
        Ok(result)
    }
}

/// The engine an instance runs a model on, over the wire API chosen for
/// it. A credential that is missing is refused here, before a session is
/// created for a turn that could not run.
fn engine_for(instance: &Resolved, wire: WireApi, credentials: &std::path::Path, krowk_version: &str) -> Result<Box<dyn Engine>, EngineError> {
    if let Some(fix) = instance.missing_key() {
        return Err(EngineError::new("not_authenticated", fix));
    }
    let credential = match &instance.auth {
        Auth::ApiKey => Credential::Key(instance.api_key.clone()),
        Auth::Keyless | Auth::Vendor => Credential::None,
        Auth::OAuth { .. } => Credential::OAuth(Arc::new(oauth::Tokens::open(oauth::Store::new(credentials.to_path_buf()), &instance.name)?)),
    };
    match wire {
        WireApi::AnthropicMessages => Ok(Box::new(NativeEngine { client: AnthropicClient::new(instance.clone(), krowk_version)? })),
        WireApi::OpenaiResponses => Ok(Box::new(NativeEngine { client: ResponsesClient::new(instance.clone(), krowk_version)? })),
        WireApi::ChatCompletions => Ok(Box::new(NativeEngine { client: ChatClient::new(instance.clone(), credential, krowk_version)? })),
        WireApi::ClaudeCode => Err(EngineError::new("bad_config", format!("{} runs Claude Code as a backend, not a native wire API", instance.name))),
    }
}

/// What the branch so far says: its items, grouped the way they were on the
/// wire, the model it last ran on, where it runs, and the backend session
/// behind it.
#[derive(Default)]
struct Past {
    items: Vec<HistoryItem>,
    model: Option<ModelRef>,
    cwd: Option<PathBuf>,
    backend: Option<BackendRecord>,
}

/// The last `backend.session` of a branch, and the instance it ran on.
#[derive(Debug, Clone, PartialEq)]
struct BackendRecord {
    instance: String,
    session_id: String,
    transcript: Option<String>,
    billing: Option<Billing>,
}

fn replay(branch: &[&LogEvent]) -> Past {
    let mut past = Past::default();
    let mut at: HashMap<&str, usize> = HashMap::new();
    let mut responses = 0usize;
    for ev in branch {
        match &ev.body {
            LogBody::SessionStarted { cwd, .. } => past.cwd = Some(PathBuf::from(cwd)),
            LogBody::TurnStarted { model, .. } => past.model = Some(model.clone()),
            LogBody::BackendSession { vendor_session_id, transcript_path, billing, .. } => {
                past.backend = Some(BackendRecord {
                    instance: past.model.as_ref().map(|m| m.instance.clone()).unwrap_or_default(),
                    session_id: vendor_session_id.clone(),
                    transcript: transcript_path.clone(),
                    billing: *billing,
                });
            }
            LogBody::ItemCompleted { item_id, item, .. } => {
                at.insert(item_id, past.items.len());
                past.items.push(HistoryItem { item: item.clone(), response: None });
            }
            LogBody::ResponseCompleted { item_ids, .. } => {
                for id in item_ids {
                    if let Some(&i) = at.get(id.as_str()) {
                        past.items[i].response = Some(responses);
                    }
                }
                responses += 1;
            }
            LogBody::TurnCompleted { .. } => {}
        }
    }
    past
}

/// A turn's running totals.
#[derive(Default)]
struct Tally {
    usage: Usage,
    cost: f64,
    unpriced: bool,
    calls: u32,
    last_text: String,
}

/// Appends and forwards, in that order.
struct Writer<'a> {
    log: &'a mut SessionLog,
    out: &'a mpsc::Sender<StreamLine>,
    turn_id: String,
    preset: &'static toolset::Preset,
    wire: WireApi,
    /// The backend session last logged: one that did not change is not
    /// logged again.
    backend: Option<BackendRecord>,
}

impl Writer<'_> {
    async fn log(&mut self, body: LogBody) -> Result<LogEvent, EngineError> {
        let ev = self.log.append(body).map_err(log_failure)?;
        let _ = self.out.send(StreamLine::Log(ev.clone())).await;
        Ok(ev)
    }

    async fn live(&self, ev: LiveEvent) {
        let _ = self.out.send(StreamLine::Live(ev)).await;
    }

    /// Runs the engine and handles its events as they come. A log that
    /// cannot be written stops the turn: the log is the session.
    async fn drive(&mut self, engine: &dyn Engine, ctx: TurnContext, tally: &mut Tally, model: &ModelRef, instance: &Resolved, pricer: &crate::host::Pricer) -> Result<TurnEnd, EngineError> {
        let (tx, mut rx) = mpsc::channel(256);
        let session_id = ctx.session_id.clone();
        let run = engine.run_turn(ctx, tx);
        tokio::pin!(run);
        let mut outcome: Option<Result<TurnEnd, EngineError>> = None;
        let mut texts: HashMap<String, String> = HashMap::new();
        loop {
            tokio::select! {
                biased;
                Some(ev) = rx.recv() => {
                    self.handle(ev, &session_id, tally, &mut texts, model, instance, pricer).await?;
                }
                r = &mut run, if outcome.is_none() => outcome = Some(r),
                else => break,
            }
        }
        outcome.expect("the loop ends only after the engine does")
    }

    #[allow(clippy::too_many_arguments)]
    async fn handle(&mut self, ev: EngineEvent, session_id: &str, tally: &mut Tally, texts: &mut HashMap<String, String>, model: &ModelRef, instance: &Resolved, pricer: &crate::host::Pricer) -> Result<(), EngineError> {
        let turn_id = self.turn_id.clone();
        match ev {
            EngineEvent::Context { system, tools } => {
                let rec = ContextRecord {
                    turn_id,
                    time_ms: krowk_store::now_ms(),
                    model: model.clone(),
                    provider: instance.provider.clone(),
                    wire_api: self.wire,
                    // A backend brings its own tools; the preset is krowk's.
                    toolset: if self.wire == WireApi::ClaudeCode { crate::claude::BACKEND.into() } else { self.preset.name.into() },
                    system_tokens: native::estimate_tokens(&system),
                    tools_tokens: native::tools_tokens(&tools),
                    system,
                    tools,
                };
                self.log.record_context(&rec).map_err(log_failure)?;
            }
            EngineEvent::ItemStarted { item_id, kind } => {
                self.live(LiveEvent::ItemStarted { session_id: session_id.into(), turn_id, item_id, item: kind }).await;
            }
            EngineEvent::ItemDelta { item_id, delta } => {
                self.live(LiveEvent::ItemDelta { session_id: session_id.into(), turn_id, item_id, delta }).await;
            }
            EngineEvent::ItemCompleted { item_id, item } => {
                if let Item::AssistantText { text } = &item {
                    texts.insert(item_id.clone(), text.clone());
                }
                self.log(LogBody::ItemCompleted { turn_id, item_id, item }).await?;
            }
            EngineEvent::ResponseCompleted { response_id, model: answered, usage, stop_reason, item_ids } => {
                tally.calls += 1;
                tally.usage += usage;
                // Priced by the id the request named; the answering model's
                // id is the fallback, since a provider may name a snapshot.
                match pricer(&instance.provider, &model.model, &usage).or_else(|| pricer(&instance.provider, &answered, &usage)) {
                    Some(usd) => tally.cost += usd,
                    None => tally.unpriced = true,
                }
                let said: Vec<&str> = item_ids.iter().filter_map(|id| texts.get(id).map(String::as_str)).collect();
                if !said.is_empty() {
                    tally.last_text = said.join("\n\n");
                }
                self.log(LogBody::ResponseCompleted { turn_id, response_id, model: answered, usage, stop_reason, item_ids }).await?;
            }
            EngineEvent::BackendSession { backend, session_id: vendor, transcript, billing } => {
                let rec = BackendRecord { instance: model.instance.clone(), session_id: vendor.clone(), transcript: transcript.clone(), billing };
                if self.backend.as_ref() != Some(&rec) {
                    self.log(LogBody::BackendSession { turn_id, backend, vendor_session_id: vendor, transcript_path: transcript, billing }).await?;
                    self.backend = Some(rec);
                }
            }
        }
        Ok(())
    }
}
