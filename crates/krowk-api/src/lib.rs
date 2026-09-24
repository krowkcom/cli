//! Wire types and the HTTP client for api.krowk.com: artifacts, runs, CLI
//! authorizations, and later the session sync resources.
//!
//! The contract is owned by krowk-canon and served by krowk-registry (Rails),
//! so these types are one of three implementations of it, not its source. Any
//! change to a path, method or error shape lands in all three together.
