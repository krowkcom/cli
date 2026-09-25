//! krowk's own agent engine: the model loop behind `krowk -p`.
//!
//! - `protocol` — the typed commands, events and log lines every client
//!   speaks; the JSON Schema in `schema/` is generated from it.
//! - `engine` — the `Engine` trait native providers and vendor backends
//!   implement, and the `EngineEvent`s they report.
//! - `native` — krowk's own loop over a `ModelClient`; `anthropic` is the
//!   first client, for the Messages API.
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
pub mod engine;
pub mod headless;
pub mod host;
pub mod instances;
pub mod log;
pub mod native;
pub mod project;
pub mod protocol;
pub mod schema;
pub mod tools;
pub mod toolset;
