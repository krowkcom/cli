//! krowk's own agent engine: the model loop behind `krowk -p`.
//!
//! - `protocol` — the typed commands, events and log lines every client
//!   speaks; the JSON Schema in `schema/` is generated from it.
//! - `engine` — the `Engine` trait native providers and vendor backends
//!   implement, and the `EngineEvent`s they report.
//! - `native` — krowk's own loop over a `ModelClient`, one per wire API:
//!   `anthropic` (Messages), `openai` (Responses), `chat` (Chat
//!   Completions: xAI, OpenRouter and anything compatible). `http` and
//!   `sse` are what the three share.
//! - `catalog` — what the models.dev cache says of a model; `effort` — the
//!   one reasoning-effort ladder, mapped per model.
//! - `oauth` — the SuperGrok login and its tokens.
//! - `claude` — the Claude Code backend: the user's own `claude` binary,
//!   driven over stream-json and its control protocol; `bridge` — krowk's
//!   tools offered to a backend as an MCP server; `trust` — which
//!   repositories a backend may run in.
//! - `codex` — the Codex backend: the user's own `codex`, driven as `codex
//!   app-server` over JSON-RPC, with krowk's tools as its dynamic tools.
//! - `tools` — read, write, the edit tools, bash, grep and glob.
//! - `toolset` — the preset registry: which edit tool a model is offered.
//! - `host` — executes commands, writes the log, prices the turn.
//! - `log` — the append-only JSONL session log and its layout on disk.
//! - `project` — the log as a `krowk_import::Source`, so krowk.db lists
//!   native sessions beside imported ones.
//! - `instances` — the named provider instances in krowk's config.
//! - `headless` — `krowk -p`.
//!
//! Canon `engineering/harness.md` describes all of it for readers who will
//! not open the code.

pub mod anthropic;
pub mod bridge;
pub mod catalog;
pub mod chat;
pub mod claude;
pub mod codex;
pub mod effort;
pub mod engine;
pub mod headless;
pub mod host;
pub mod http;
pub mod instances;
pub mod log;
pub mod native;
pub mod oauth;
pub mod openai;
pub mod project;
pub mod protocol;
pub mod schema;
pub mod sse;
pub mod tools;
pub mod toolset;
pub mod trust;
