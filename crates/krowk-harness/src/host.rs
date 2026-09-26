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
use crate::budget::Budget;
use crate::agents;
use crate::engine::{BoxFuture, Engine, EngineError, EngineEvent, Events, HistoryItem, Steers, TurnContext, TurnEnd};
use crate::subagent::{AgentRun, AgentsConfig, ParentTurn, Spawn, Subagents};
use crate::evidence::{Evidence, Publisher};
use crate::instances::{Auth, Registry, Resolved};
use crate::oauth;
use crate::openai::ResponsesClient;
use crate::log::{self, LogError, SessionLog};
use crate::native::{self, NativeEngine};
use crate::toolset;
use crate::protocol::{
    Billing, BudgetLimits, Command, ContextRecord, Effort, Item, LiveEvent, LogBody, LogEvent, ModelRef, PermissionMode, RunResult, StreamLine, TurnStatus, Usage, WireApi,
};
use crate::claude::ClaudeEngine;
use crate::codex::CodexEngine;
use crate::trust;
use crate::{compat, permissions};
use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Instant;
use tokio::sync::{mpsc, watch, Semaphore};

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
    /// Pushes what `publish` is handed to krowk's registry (R-EVID-1);
    /// none, and the tool says it cannot run.
    pub publisher: Option<Publisher>,
    /// The person's own settings and where krowk keeps its own: what the
    /// permission rules, instructions, skills and hooks are read from, and
    /// whether a client answers approval requests (R-PERM-1, R-PERM-2).
    pub permissions: permissions::Config,
    /// The person's agent definitions and the model listing a subagent's
    /// model is chosen from (R-SUB-1, R-SUB-5).
    pub agents: AgentsConfig,
}

/// What executes commands. Cheap to share: its state is behind one `Arc`,
/// which a turn's subagents hold too, since each is a turn of this host.
pub struct Host {
    shared: Arc<Shared>,
}

pub(crate) struct Shared {
    pub(crate) cfg: HostConfig,
    /// Each session with a turn running — a subagent's too: its cancel
    /// switch and the queue its steering waits in.
    running: Mutex<HashMap<String, Running>>,
    /// Each session's backend engine, with the instance it runs on: its
    /// process outlives a turn and serves the session's next one.
    backends: Mutex<HashMap<String, Backend>>,
    /// How long a session's backend process is kept without a turn.
    backend_idle: std::time::Duration,
    /// Approval requests waiting for a client's answer, every session's —
    /// a subagent's included.
    approvals: permissions::Approvals,
    /// What a person allowed for the rest of each session. A subagent
    /// shares its parent's: a grant for the session holds in its subagents.
    grants: Mutex<HashMap<String, permissions::SessionGrants>>,
    /// The sessions this host has run a turn of: a session's first turn
    /// here is its SessionStart.
    started: Mutex<std::collections::HashSet<String>>,
}

struct Running {
    cancel: Arc<watch::Sender<bool>>,
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
/// host (the daemon, the TUI) holds many sessions, and an idle `claude` or
/// `codex app-server` is a few hundred megabytes. The next turn starts it
/// again on the vendor's resume.
pub const BACKEND_IDLE: std::time::Duration = std::time::Duration::from_secs(15 * 60);

/// How long letting go of one backend may take: its own polite stop (stdin
/// closed, five seconds, then its group stopped) and a margin.
pub const SHUTDOWN_GRACE: std::time::Duration = std::time::Duration::from_secs(10);

fn log_failure(e: LogError) -> EngineError {
    match e {
        LogError::NotFound(m) => EngineError::new("no_session", m),
        LogError::Busy(m) => EngineError::new("session_busy", m),
        LogError::Io(m) => EngineError::new("session_log_failed", m),
    }
}

impl Host {
    pub fn new(cfg: HostConfig) -> Host {
        Host {
            shared: Arc::new(Shared {
                cfg,
                running: Mutex::new(HashMap::new()),
                backends: Mutex::new(HashMap::new()),
                backend_idle: BACKEND_IDLE,
                approvals: permissions::Approvals::default(),
                grants: Mutex::new(HashMap::new()),
                started: Mutex::new(std::collections::HashSet::new()),
            }),
        }
    }

    /// Keeps an idle session's backend process this long instead of
    /// `BACKEND_IDLE`.
    pub fn with_backend_idle(mut self, idle: std::time::Duration) -> Host {
        Arc::get_mut(&mut self.shared).expect("a new host is not shared yet").backend_idle = idle;
        self
    }

    /// Lets every backend process go cleanly. A host dropped without this
    /// still stops them (they are killed with their handles), just less
    /// politely.
    ///
    /// Bounded: an engine that cannot be let go of in `SHUTDOWN_GRACE` — its
    /// lock held by a turn nobody polls any more — has its process group
    /// killed instead, so a host going away never waits on one.
    pub async fn shutdown(&self) {
        let engines: Vec<Arc<dyn Engine>> = self.shared.backends.lock().unwrap_or_else(|e| e.into_inner()).drain().map(|(_, b)| b.engine).collect();
        let mut stuck = false;
        for e in engines {
            stuck |= tokio::time::timeout(SHUTDOWN_GRACE, e.shutdown()).await.is_err();
        }
        if stuck {
            crate::group::kill_all();
        }
    }

    pub fn registry(&self) -> &Registry {
        &self.shared.cfg.registry
    }

    /// Executes one command. A `prompt` streams its events to `out` and
    /// answers with its result — a turn that failed is still a result, with
    /// `isError`. An error here means no turn ran.
    pub async fn execute(&self, cmd: Command, out: mpsc::Sender<StreamLine>) -> Result<Option<RunResult>, EngineError> {
        let shared = &self.shared;
        match cmd {
            Command::Prompt { session_id, text, model, permission_mode, toolset, effort, budget } => {
                shared.prompt(session_id.as_deref(), text, model, permission_mode, toolset.as_deref(), effort, budget.unwrap_or_default(), out).await.map(Some)
            }
            // A subagent's session is a running turn like any other, so it
            // is interrupted alone, by its own id (R-SUB-2).
            Command::Interrupt { session_id } => {
                let running = shared.running.lock().unwrap_or_else(|e| e.into_inner());
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
                let running = shared.running.lock().unwrap_or_else(|e| e.into_inner());
                match running.get(&session_id) {
                    Some(r) if r.steers.push(text).is_ok() => Ok(None),
                    // The turn has taken its last input and is ending.
                    Some(_) => Err(EngineError::new("turn_ending", format!("the turn in session {session_id} is finishing and reads no more input — send it as the next prompt"))),
                    None => Err(EngineError::new("no_running_turn", format!("session {session_id} has no turn running to steer — send it as a prompt instead"))),
                }
            }
            // Whichever client answers first decides; the turn that asked
            // tells every client it was answered (`approval.resolved`). A
            // subagent's request is answered under the subagent's session.
            Command::Approve { session_id, request_id, decision } => {
                shared.approvals.answer(&session_id, &request_id, decision).map_err(|e| EngineError::new("no_approval_request", e))?;
                Ok(None)
            }
            Command::SwitchModel { .. } | Command::Fork { .. } => {
                Err(EngineError::new("not_implemented", "this command is part of the protocol but not served by this build yet"))
            }
        }
    }
}

/// One turn, settled: the log it appends to, what the branch so far says,
/// and everything it runs with.
struct TurnPlan {
    log: SessionLog,
    past: Past,
    text: String,
    model: ModelRef,
    /// The instance's provider: what the turn's calls are priced under.
    provider: String,
    engine: Arc<dyn Engine>,
    preset: &'static toolset::Preset,
    wire: WireApi,
    info: Option<ModelInfo>,
    permission_mode: PermissionMode,
    effort: Option<Effort>,
    cwd: PathBuf,
    backend_session: Option<String>,
    budget: Budget,
    evidence: Option<Evidence>,
    /// The rules the turn is judged by, its instructions, skills and hooks,
    /// and the session's grants: a subagent's are its parent's.
    policy: permissions::Policy,
    compat: compat::Compat,
    grants: permissions::SessionGrants,
    /// A subagent's definition.
    agent: Option<AgentRun>,
    /// Whether the turn may start subagents: a session's own native turn,
    /// never a subagent's.
    spawns: bool,
    /// A subagent's parent turn: where its spend is also reported.
    parent: Option<ParentLink>,
    /// Flips when the parent's turn is interrupted.
    parent_cancel: Option<watch::Receiver<bool>>,
    /// Whether the result goes to the client as a `result` frame: a
    /// subagent's goes back to the tool call instead.
    announce: bool,
    started: Instant,
}

/// The parent turn a subagent's spend is reported to.
#[derive(Clone)]
struct ParentLink {
    budget: Budget,
    session_id: String,
    turn_id: String,
}

impl Shared {
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
        let e: Arc<dyn Engine> = match instance.wire_api {
            WireApi::CodexAppServer => Arc::new(CodexEngine::new(instance.clone(), &self.cfg.krowk_version)?),
            _ => Arc::new(ClaudeEngine::new(instance.clone(), &self.cfg.krowk_version)?),
        };
        backends.insert(session_id.to_string(), Backend { instance: instance.name.clone(), engine: e.clone(), used: Instant::now() });
        Ok(e)
    }

    #[allow(clippy::too_many_arguments)]
    async fn prompt(
        self: &Arc<Self>,
        session_id: Option<&str>,
        text: String,
        model: Option<ModelRef>,
        permission_mode: PermissionMode,
        toolset: Option<&str>,
        effort: Option<Effort>,
        limits: BudgetLimits,
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
        let cwd = past.cwd.clone().unwrap_or_else(|| self.cfg.cwd.clone());
        let native = match &instance.backend {
            None => Some(engine_for(&instance, wire, &self.cfg.credentials, &self.cfg.krowk_version)?),
            // A backend runs the repository's own hooks and MCP servers, so
            // it is not started in one nobody trusted; and a binary that is
            // not there is named before a session exists for it.
            Some(b) => {
                if b.path.is_none() {
                    let (install, add) = match instance.wire_api {
                        WireApi::CodexAppServer => ("Codex (https://developers.openai.com/codex)", "codex"),
                        _ => ("Claude Code (https://claude.com/claude-code)", "claude"),
                    };
                    return Err(EngineError::new("backend_not_found", format!("{} was not found — install {install}, or name the binary with `krowk providers add {add} --binary <path>`", b.binary)));
                }
                if let Some(fix) = instance.missing_key() {
                    return Err(EngineError::new("not_authenticated", fix));
                }
                (self.cfg.trust)(&trust::root(&cwd))?;
                None
            }
        };
        let effort = effort.or(instance.effort);
        // The rules, instructions, skills and hooks for where the session
        // runs. A settings file that does not parse refuses the prompt: a
        // deny rule it held would otherwise silently stop holding.
        let mut policy = permissions::Policy::load(&self.cfg.permissions, &cwd).map_err(|e| EngineError::new("bad_settings", format!("{e} — fix the file, then send the prompt again")))?;
        let mut compat = compat::Compat::load(&self.cfg.permissions, &cwd, policy.loaded.hooks.clone());
        policy.read_dirs = compat.skills.iter().map(|k| k.dir.clone()).collect();
        let (log, events) = match opened {
            Some(opened) => opened,
            None => {
                let (log, root) = SessionLog::create(&self.cfg.sessions_dir, &self.cfg.cwd, &self.cfg.krowk_version).map_err(log_failure)?;
                let _ = out.send(StreamLine::Log(root.clone())).await;
                (log, vec![root])
            }
        };
        let session_id = log.session_id.clone();
        let spawns = native.is_some();
        let engine: Arc<dyn Engine> = match native {
            Some(e) => Arc::from(e),
            None => self.backend_for(&session_id, &instance)?,
        };
        // The vendor session to resume (Claude Code's session, Codex's
        // thread): the branch's last, if it ran on this instance — another
        // account's config directory does not have it.
        let backend_session = past.backend.as_ref().filter(|b| b.instance == model.instance).map(|b| b.session_id.clone());
        // Everything the session and its subagents have spent, from their
        // logs; this turn's calls are added as they are metered.
        let budget = Budget::new(limits, &session_id, &self.cfg.sessions_dir, self.cfg.pricer.clone(), &instance.provider, &model.model, &events);
        drop(events);
        let evidence = self.cfg.publisher.clone().map(|p| Evidence::new(p, &session_id, past.run.clone()));
        let first_here = self.started.lock().unwrap_or_else(|e| e.into_inner()).insert(session_id.clone());
        compat.session_start = first_here.then_some(if past.items.is_empty() { "startup" } else { "resume" });
        compat.transcript = self.cfg.sessions_dir.join(&session_id).join(log::EVENTS_FILE).display().to_string();
        let grants = self.grants.lock().unwrap_or_else(|e| e.into_inner()).entry(session_id.clone()).or_default().clone();
        let plan = TurnPlan {
            log,
            past,
            text,
            model,
            provider: instance.provider.clone(),
            engine,
            preset,
            wire,
            info,
            permission_mode,
            effort,
            cwd,
            backend_session,
            budget,
            evidence,
            policy,
            compat,
            grants,
            agent: None,
            spawns,
            parent: None,
            parent_cancel: None,
            announce: true,
            started,
        };
        self.turn(plan, out).await
    }

    /// A subagent (R-SUB-1): a child session of the parent turn `spawn`
    /// describes, answering its tool call `call_id` with one turn on
    /// `model`. Its lines go to the parent's client; its result comes back
    /// here, for the tool call.
    #[allow(clippy::too_many_arguments)]
    pub(crate) async fn subagent(self: &Arc<Self>, spawn: &Spawn, call_id: &str, description: &str, prompt: &str, model: ModelRef, run: AgentRun, events: &Events) -> Result<RunResult, EngineError> {
        let instance = self.cfg.registry.get(&model.instance).map_err(|e| EngineError::new("no_instance", e))?.clone();
        // A vendor runs its own agents, with its own tools: it could not be
        // held to the allowlist, so a subagent is always krowk's own loop.
        if instance.backend.is_some() {
            return Err(EngineError::new(
                "bad_subagent_model",
                format!("{model} runs on a vendor's own agent, and subagents run on krowk's own loop — name an API model in the agent definition or in `subagents.model`"),
            ));
        }
        let info = (self.cfg.catalog)(&instance.provider, &model.model);
        let family = info.as_ref().and_then(|i| i.family.clone());
        let (preset, _) = toolset::choose(None, self.cfg.registry.toolset.as_deref(), family.as_deref(), &model.model).map_err(|e| EngineError::new("bad_toolset", e))?;
        let wire = instance.wire_for(info.as_ref().and_then(|i| i.wire_api));
        let engine = engine_for(&instance, wire, &self.cfg.credentials, &self.cfg.krowk_version)?;
        let p = &spawn.parent;
        let (log, root) = SessionLog::create_child(&self.cfg.sessions_dir, &p.cwd, &self.cfg.krowk_version, Some(&p.session_id), run.name.as_deref()).map_err(log_failure)?;
        let _ = spawn.out.send(StreamLine::Log(root.clone())).await;
        let child = log.session_id.clone();
        let _ = events.send(EngineEvent::SubagentStarted { call_id: call_id.into(), session_id: child.clone(), description: description.into(), agent: run.name.clone(), model: model.clone() }).await;
        let budget = Budget::for_subagent(&p.budget, &child, &instance.provider, &model.model, std::slice::from_ref(&root));
        let plan = TurnPlan {
            log,
            past: Past { cwd: Some(p.cwd.clone()), ..Past::default() },
            text: prompt.into(),
            model,
            provider: instance.provider.clone(),
            engine: Arc::from(engine),
            preset,
            wire,
            info,
            permission_mode: p.permission_mode,
            effort: instance.effort,
            cwd: p.cwd.clone(),
            backend_session: None,
            budget,
            evidence: p.evidence.as_ref().map(|e| e.for_subagent(events.clone())),
            // The parent's rules, instructions, skills and hooks, and its
            // session's grants: a subagent is judged as its parent would be,
            // in its parent's mode, and asks under its own session id.
            policy: p.gate.policy().clone(),
            compat: compat::Compat { session_start: None, transcript: self.cfg.sessions_dir.join(&child).join(log::EVENTS_FILE).display().to_string(), ..(*p.compat).clone() },
            grants: p.grants.clone(),
            agent: Some(run),
            spawns: false,
            parent: Some(ParentLink { budget: p.budget.clone(), session_id: p.session_id.clone(), turn_id: p.turn_id.clone() }),
            parent_cancel: Some(p.cancel.clone()),
            announce: false,
            started: Instant::now(),
        };
        // Boxed: a subagent's turn is a turn of this host, inside the
        // parent's.
        let turn: BoxFuture<'_, Result<RunResult, EngineError>> = Box::pin(self.turn(plan, spawn.out.clone()));
        turn.await
    }

    /// Runs one settled turn to its end and logs it whole.
    async fn turn(self: &Arc<Self>, mut plan: TurnPlan, out: mpsc::Sender<StreamLine>) -> Result<RunResult, EngineError> {
        let session_id = plan.log.session_id.clone();
        let turn_id = krowk_store::new_id();
        let engine = plan.engine.clone();
        let model = plan.model.clone();
        let mut w = Writer {
            log: &mut plan.log,
            out: &out,
            turn_id: turn_id.clone(),
            preset: plan.preset,
            wire: plan.wire,
            provider: plan.provider.clone(),
            backend: plan.past.backend.clone(),
            parent: plan.parent.clone(),
        };
        w.log(LogBody::TurnStarted { turn_id: turn_id.clone(), model: model.clone(), provider: engine.provider().into(), wire_api: engine.wire_api(), permission_mode: plan.permission_mode, effort: plan.effort }).await?;
        let prompt_item = Item::UserText { text: std::mem::take(&mut plan.text) };
        w.log(LogBody::ItemCompleted { turn_id: turn_id.clone(), item_id: krowk_store::new_id(), item: prompt_item.clone() }).await?;
        let mut history = std::mem::take(&mut plan.past.items);
        history.push(HistoryItem { item: prompt_item, response: None });

        let gate = permissions::Gate::new(
            plan.policy.clone(),
            plan.permission_mode,
            plan.grants.clone(),
            self.cfg.permissions.approvals.then(|| self.approvals.clone()),
            self.cfg.permissions.grants_file(),
            &session_id,
            &turn_id,
        );
        let compat = Arc::new(std::mem::take(&mut plan.compat));
        let (cancel_tx, cancel) = watch::channel(false);
        let cancel_tx = Arc::new(cancel_tx);
        let steers = Steers::default();
        self.running.lock().unwrap_or_else(|e| e.into_inner()).insert(session_id.clone(), Running { cancel: cancel_tx.clone(), steers: steers.clone() });
        // The agent definitions a subagent can be started from, read afresh
        // each turn from the repository and the person's own.
        let subagents = plan.spawns.then(|| {
            let (defs, problems) = agents::discover(&trust::root(&plan.cwd), &self.cfg.agents.user_dirs);
            let spawn = Spawn {
                host: self.clone(),
                parent: ParentTurn {
                    session_id: session_id.clone(),
                    turn_id: turn_id.clone(),
                    model: model.clone(),
                    provider: plan.provider.clone(),
                    cwd: plan.cwd.clone(),
                    permission_mode: plan.permission_mode,
                    budget: plan.budget.clone(),
                    evidence: plan.evidence.clone(),
                    gate: gate.clone(),
                    compat: compat.clone(),
                    grants: plan.grants.clone(),
                    cancel: cancel.clone(),
                },
                out: out.clone(),
                gate: Arc::new(Semaphore::new(self.cfg.registry.subagents.max_parallel())),
                defs,
                spent: std::sync::Mutex::new((0.0, false)),
            };
            (Subagents(Arc::new(spawn)), problems)
        });
        let subagents = match subagents {
            Some((s, problems)) => {
                for why in problems {
                    w.live(LiveEvent::Notice { session_id: session_id.clone(), turn_id: turn_id.clone(), text: format!("an agent definition was skipped — {why}") }).await;
                }
                Some(s)
            }
            None => None,
        };
        let budget = plan.budget.clone();
        let spawned = subagents.clone();
        let ctx = TurnContext {
            session_id: session_id.clone(),
            turn_id: turn_id.clone(),
            model: model.clone(),
            history,
            cwd: plan.cwd.clone(),
            permission_mode: plan.permission_mode,
            preset: plan.preset,
            effort: plan.effort,
            model_info: plan.info.clone(),
            cancel,
            steers: steers.clone(),
            backend_session: plan.backend_session.clone(),
            budget: budget.clone(),
            evidence: plan.evidence.clone(),
            gate,
            compat,
            subagents,
            agent: plan.agent.clone(),
        };
        let mut tally = Tally::default();
        // A backend's calls are the vendor's to make: its turn is not begun
        // when the session is already at its budget, and is interrupted when
        // a metered call takes it past (`Writer::handle`). The native loop
        // asks before each call itself.
        let watch = (!engine.checks_budget()).then(|| (budget.clone(), cancel_tx.clone()));
        let before = match &watch {
            Some((b, _)) => b.admit_turn().await,
            None => Ok(()),
        };
        // A subagent stops when its parent's turn is interrupted, the way
        // it stops for its own interrupt: keeping what it made.
        let parent_cancel = plan.parent_cancel.as_mut();
        let follow = async {
            if let Some(pc) = parent_cancel {
                crate::engine::cancelled(pc).await;
                let _ = cancel_tx.send(true);
            }
            std::future::pending::<()>().await
        };
        let outcome = match before {
            Ok(()) => tokio::select! {
                biased;
                o = w.drive(engine.as_ref(), ctx, &mut tally, &model, &budget, watch.as_ref()) => o,
                _ = follow => unreachable!("following the parent never ends"),
            },
            Err(e) => Err(e),
        };
        // A backend turn the budget interrupted failed on it, whatever the
        // vendor made of the interrupt.
        let outcome = match tally.tripped.take() {
            Some(e) => Err(e),
            None => outcome,
        };
        // Refused from here on, not queued for a turn that is over; what an
        // interrupted or failed turn never took goes back on its result.
        let unread_steers = steers.close();
        self.running.lock().unwrap_or_else(|e| e.into_inner()).remove(&session_id);
        self.approvals.forget_session(&session_id);
        if let Some(b) = self.backends.lock().unwrap_or_else(|e| e.into_inner()).get_mut(&session_id) {
            b.used = Instant::now();
        }

        let (status, error) = match outcome {
            Ok(TurnEnd::Completed) => (TurnStatus::Completed, None),
            Ok(TurnEnd::Interrupted) => (TurnStatus::Interrupted, None),
            Err(e) => (TurnStatus::Failed, Some(e.info())),
        };
        let duration_ms = plan.started.elapsed().as_millis() as u64;
        // The turn's cost is its subagents' too, as a backend's own
        // subagents' are part of its turn (R-SUB-4).
        let (children_usd, children_unpriced) = spawned.as_ref().map_or((0.0, false), Subagents::spent);
        w.log(LogBody::TurnCompleted { turn_id: turn_id.clone(), status, usage: tally.usage, duration_ms, error: error.clone(), reported_cost_usd: tally.reported }).await?;
        plan.log.sync().map_err(log_failure)?;
        let result = RunResult {
            session_id,
            turn_id,
            status,
            is_error: status == TurnStatus::Failed,
            result: tally.last_text,
            model,
            usage: tally.usage,
            cost_usd: if tally.unpriced || children_unpriced { None } else { Some(tally.cost + children_usd) },
            duration_ms,
            num_model_calls: tally.calls,
            error,
            unread_steers,
        };
        if plan.announce {
            let _ = out.send(StreamLine::Live(LiveEvent::Result(result.clone()))).await;
        }
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
        WireApi::CodexAppServer => Err(EngineError::new("bad_config", format!("{} runs Codex as a backend, not a native wire API", instance.name))),
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
    /// The krowk run its evidence goes under, once `publish` opened one.
    run: Option<String>,
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
            LogBody::RunOpened { run, .. } => past.run = Some(run.clone()),
            // The subagents' own logs hold their conversations; the todo
            // list is read back from the calls that set it.
            LogBody::TurnCompleted { .. } | LogBody::SubagentResponse { .. } | LogBody::SubagentStarted { .. } | LogBody::TodosUpdated { .. } => {}
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
    /// Why the host interrupted a backend's turn: the budget it went past.
    tripped: Option<EngineError>,
    /// What the backend said the turn cost.
    reported: Option<f64>,
}

/// Appends and forwards, in that order.
struct Writer<'a> {
    log: &'a mut SessionLog,
    out: &'a mpsc::Sender<StreamLine>,
    turn_id: String,
    preset: &'static toolset::Preset,
    wire: WireApi,
    /// The provider the turn's calls go to, for its context record.
    provider: String,
    /// The backend session last logged: one that did not change is not
    /// logged again.
    backend: Option<BackendRecord>,
    /// A subagent's parent turn, whose spend the subagent's calls add to.
    parent: Option<ParentLink>,
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

    /// Where a metered call left the turn and the session: the result's
    /// cost, and R-BUDGET-2's frame for the status bar.
    async fn spent(&self, session_id: &str, turn_id: String, tally: &mut Tally, spent: &crate::budget::Snapshot) {
        tally.cost = spent.turn.known_usd;
        tally.unpriced = !spent.turn.unpriced.is_empty();
        self.live(LiveEvent::Cost {
            session_id: session_id.into(),
            turn_id,
            cost_usd: spent.total.cost(),
            turn_cost_usd: spent.turn.cost(),
            generated_tokens: spent.total.generated(),
        })
        .await;
        // A subagent spends during its parent's turn: the parent's figure,
        // its whole tree counted again, moves with it (R-SUB-4).
        if let Some(p) = &self.parent {
            let tree = p.budget.refreshed().await;
            self.live(LiveEvent::Cost {
                session_id: p.session_id.clone(),
                turn_id: p.turn_id.clone(),
                cost_usd: tree.total.cost(),
                turn_cost_usd: tree.turn.cost(),
                generated_tokens: tree.total.generated(),
            })
            .await;
        }
    }

    /// Runs the engine and handles its events as they come. A log that
    /// cannot be written stops the turn: the log is the session.
    async fn drive(&mut self, engine: &dyn Engine, ctx: TurnContext, tally: &mut Tally, model: &ModelRef, budget: &Budget, watch: Option<&(Budget, Arc<watch::Sender<bool>>)>) -> Result<TurnEnd, EngineError> {
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
                    self.handle(ev, &session_id, tally, &mut texts, model, budget).await?;
                    // A backend past its budget is stopped the way a person
                    // stops it, keeping what it made.
                    if let Some((b, cancel)) = watch
                        && tally.tripped.is_none()
                        && let Some(e) = b.over()
                    {
                        tally.tripped = Some(e);
                        let _ = cancel.send(true);
                    }
                }
                r = &mut run, if outcome.is_none() => outcome = Some(r),
                else => break,
            }
        }
        outcome.expect("the loop ends only after the engine does")
    }

    async fn handle(&mut self, ev: EngineEvent, session_id: &str, tally: &mut Tally, texts: &mut HashMap<String, String>, model: &ModelRef, budget: &Budget) -> Result<(), EngineError> {
        let turn_id = self.turn_id.clone();
        match ev {
            EngineEvent::Context { system, tools } => {
                let rec = ContextRecord {
                    turn_id,
                    time_ms: krowk_store::now_ms(),
                    model: model.clone(),
                    provider: self.provider.clone(),
                    wire_api: self.wire,
                    // A backend brings its own tools; the preset is krowk's.
                    toolset: match self.wire {
                        WireApi::ClaudeCode => crate::claude::BACKEND.into(),
                        WireApi::CodexAppServer => crate::codex::BACKEND.into(),
                        _ => self.preset.name.into(),
                    },
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
                let spent = budget.record(&answered, &usage);
                let said: Vec<&str> = item_ids.iter().filter_map(|id| texts.get(id).map(String::as_str)).collect();
                if !said.is_empty() {
                    tally.last_text = said.join("\n\n");
                }
                self.log(LogBody::ResponseCompleted { turn_id: turn_id.clone(), response_id, model: answered, usage, stop_reason, item_ids }).await?;
                self.spent(session_id, turn_id, tally, &spent).await;
            }
            EngineEvent::BackendSession { backend, session_id: vendor, transcript, billing } => {
                let rec = BackendRecord { instance: model.instance.clone(), session_id: vendor.clone(), transcript: transcript.clone(), billing };
                if self.backend.as_ref() != Some(&rec) {
                    self.log(LogBody::BackendSession { turn_id, backend, vendor_session_id: vendor, transcript_path: transcript, billing }).await?;
                    self.backend = Some(rec);
                }
            }
            EngineEvent::RunOpened { run } => {
                self.log(LogBody::RunOpened { turn_id, run }).await?;
            }
            // Metered, logged apart from the conversation, and counted.
            EngineEvent::SubagentResponse { response_id, model: answered, usage } => {
                tally.usage += usage;
                let spent = budget.record_subagent(&answered, &usage);
                self.log(LogBody::SubagentResponse { turn_id: turn_id.clone(), response_id, model: answered, usage }).await?;
                self.spent(session_id, turn_id, tally, &spent).await;
            }
            EngineEvent::ReportedCost { usd } => {
                tally.reported = Some(usd);
                let spent = budget.reported(usd);
                self.spent(session_id, turn_id, tally, &spent).await;
            }
            EngineEvent::Notice { text } => {
                self.live(LiveEvent::Notice { session_id: session_id.into(), turn_id, text }).await;
            }
            EngineEvent::Approval(req) => self.live(LiveEvent::ApprovalRequested(req)).await,
            EngineEvent::ApprovalResolved { request_id, decision } => {
                self.live(LiveEvent::ApprovalResolved { session_id: session_id.into(), turn_id, request_id, decision }).await;
            }
            EngineEvent::Todos { todos } => {
                self.log(LogBody::TodosUpdated { turn_id, todos }).await?;
            }
            EngineEvent::SubagentStarted { call_id, session_id: child, description, agent, model } => {
                self.log(LogBody::SubagentStarted { turn_id, call_id, subagent_session_id: child, description, agent, model }).await?;
            }
        }
        Ok(())
    }
}
