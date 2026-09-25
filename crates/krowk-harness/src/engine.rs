//! The `Engine` trait: what produces a session's turns.
//!
//! An engine is handed one turn — the branch so far, the prompt, the model —
//! and runs it to the end, reporting what happens as `EngineEvent`s on a
//! channel. It owns nothing durable: the host assigns log ids, appends the
//! log, forwards live frames and adds up the cost, so every engine's turns
//! land in the same log in the same shape (R-BACK-5).
//!
//! Two families implement it:
//!
//! - The native loop (`native::NativeEngine`), where krowk owns the prompt,
//!   the tools and the loop, and a `ModelClient` speaks one provider's wire
//!   API. The Anthropic Messages client is the first; OpenAI Responses and
//!   Chat Completions are more `ModelClient`s under the same loop.
//! - Backends (a vendor harness such as `claude -p` or `codex app-server`
//!   driving its own loop), which implement `Engine` directly and translate
//!   the vendor's stream into these events.
//!
//! The rules every engine keeps:
//!
//! - Every item is announced with `ItemStarted` before any `ItemDelta`, and
//!   finished with exactly one `ItemCompleted` under the same id — or never
//!   completed at all, when a turn is interrupted mid-item and what arrived
//!   cannot stand on its own (half a tool call's input, reasoning with no
//!   signature).
//! - `ResponseCompleted` follows the items of one model call, naming them in
//!   order: they are what was one message on the provider's wire, and are
//!   replayed as one.
//! - `Context` is sent before the first model call of a turn, with the exact
//!   system prompt and tools that call carries (R-LOG-4).
//! - Interruption is cooperative: `TurnContext::cancel` flips, the engine
//!   stops at the next point it can, and returns `TurnEnd::Interrupted`.

use crate::protocol::{Delta, ErrorInfo, Item, ItemKind, ModelRef, PermissionMode, ToolDefinition, Usage, WireApi};
use std::future::Future;
use std::path::PathBuf;
use std::pin::Pin;
use tokio::sync::{mpsc, watch};

/// A boxed future that can move between threads: what a dyn-compatible
/// async trait method returns.
pub type BoxFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

/// What an engine reports while it runs a turn.
#[derive(Debug, Clone, PartialEq)]
pub enum EngineEvent {
    /// The system prompt and tools the turn's model calls carry.
    Context { system: String, tools: Vec<ToolDefinition> },
    ItemStarted { item_id: String, kind: ItemKind },
    ItemDelta { item_id: String, delta: Delta },
    ItemCompleted { item_id: String, item: Item },
    ResponseCompleted { response_id: Option<String>, model: String, usage: Usage, stop_reason: Option<String>, item_ids: Vec<String> },
}

/// Where an engine sends its events. Bounded, so a slow client slows the
/// stream rather than growing a buffer.
pub type Events = mpsc::Sender<EngineEvent>;

/// One turn, as the engine is handed it.
#[derive(Debug, Clone)]
pub struct TurnContext {
    pub session_id: String,
    pub turn_id: String,
    pub model: ModelRef,
    /// The branch so far, oldest first, ending with this turn's prompt.
    pub history: Vec<HistoryItem>,
    /// Where the session runs: tools resolve paths against it.
    pub cwd: PathBuf,
    pub permission_mode: PermissionMode,
    /// Flips to true when the turn is to stop.
    pub cancel: watch::Receiver<bool>,
}

/// An item of the branch, with the grouping the provider needs to replay it:
/// which model call produced it, and on which wire API.
#[derive(Debug, Clone, PartialEq)]
pub struct HistoryItem {
    pub item: Item,
    /// The index of the response this item came out of, unique within the
    /// history; none for items the host or a tool produced.
    pub response: Option<usize>,
    /// Who produced the response, for deciding whether its blobs replay.
    pub provider: Option<(String, WireApi)>,
}

/// How a turn that did not fail ended.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TurnEnd {
    Completed,
    Interrupted,
}

/// Why a turn failed: a code and a sentence naming the next action, and the
/// HTTP status the provider answered with, when one did (0 otherwise).
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EngineError {
    pub code: String,
    pub message: String,
    pub status: u16,
}

impl EngineError {
    pub fn new(code: &str, message: impl Into<String>) -> EngineError {
        EngineError { code: code.into(), message: message.into(), status: 0 }
    }

    pub fn with_status(self, status: u16) -> EngineError {
        EngineError { status, ..self }
    }

    pub fn info(&self) -> ErrorInfo {
        ErrorInfo { code: self.code.clone(), message: self.message.clone(), http_status: (self.status != 0).then_some(self.status) }
    }
}

impl std::fmt::Display for EngineError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}

/// What produces a session's turns. Object-safe, so the host picks one per
/// instance kind at run time.
pub trait Engine: Send + Sync {
    /// The provider this engine's model calls go to, e.g. `anthropic`.
    fn provider(&self) -> &str;
    fn wire_api(&self) -> WireApi;
    /// Runs one turn to its end. Items produced before a failure were
    /// already sent and stay in the log.
    fn run_turn<'a>(&'a self, ctx: TurnContext, events: Events) -> BoxFuture<'a, Result<TurnEnd, EngineError>>;
}

/// Resolves once `cancel` flips to true. A switch whose owner is gone never
/// flips, so it never resolves.
pub async fn cancelled(cancel: &mut watch::Receiver<bool>) {
    if cancel.wait_for(|c| *c).await.is_err() {
        std::future::pending::<()>().await;
    }
}
