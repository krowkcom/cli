//! The tool bridge: krowk's own tools, offered to a vendor backend over MCP
//! (R-BACK-1). A backend runs its own loop with its own tools; what it
//! cannot have without krowk — `publish` once ticket 8 lands, the subagent
//! bridge later, and today the one trivial `session_info` that proves the
//! path — is injected as one MCP server named `krowk`, so the backend's
//! model calls it as `mcp__krowk__<tool>`.
//!
//! The bridge is transport-free: `handle` answers one JSON-RPC message with
//! one JSON-RPC answer, and each backend carries the messages its own way,
//! on the pipes krowk already drives it over — so there is no socket, no
//! port and no second process for anything else on the host to reach, and a
//! call runs inside the krowk process that owns the session. Claude Code
//! carries MCP inside its control protocol (an `sdk` server). `codex
//! app-server` has its own in-band channel instead: the tools are declared
//! as the thread's dynamic tools, in one `krowk` namespace, and each call
//! arrives as an `item/tool/call` request, which `crate::codex` answers through
//! `handle` as a `tools/call`.
//!
//! Every tool in `EXPOSED` is offered to every backend session, and only
//! those: a tool is exposed by being listed there, with its input a Rust
//! type its schema is derived from, as the native tools' are.

use crate::protocol::{ModelRef, ToolDefinition};
use schemars::JsonSchema;
use serde::Deserialize;
use serde_json::{json, Value};
use std::path::Path;

/// The MCP server's name: the backend sees each tool as `mcp__krowk__<name>`.
pub const SERVER: &str = "krowk";

/// The MCP revision answered when the client names none.
const MCP_VERSION: &str = "2025-06-18";

/// What a bridged tool call runs with: the krowk session it belongs to.
pub struct BridgeEnv<'a> {
    pub session_id: &'a str,
    pub turn_id: &'a str,
    pub model: &'a ModelRef,
    pub cwd: &'a Path,
    /// The backend running the session, e.g. `claude-code`.
    pub backend: &'a str,
    pub krowk_version: &'a str,
}

/// A krowk tool offered to backends.
pub struct Exposed {
    pub name: &'static str,
    pub description: &'static str,
    pub input_schema: fn() -> Value,
    /// Runs one call: the output text, and whether it is an error.
    pub run: fn(&BridgeEnv<'_>, &Value) -> (String, bool),
}

/// Every tool a backend is offered, in the order it lists them.
pub const EXPOSED: &[Exposed] = &[Exposed {
    name: "session_info",
    description: "What krowk knows about this session: its krowk session id, the turn, the instance and model it runs on, and its working directory. Takes no input.",
    input_schema: crate::tools::input_schema::<SessionInfoInput>,
    run: session_info,
}];

/// session_info takes nothing.
#[derive(Debug, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct SessionInfoInput {}

fn session_info(env: &BridgeEnv<'_>, input: &Value) -> (String, bool) {
    // A model that sends `null` or nothing at all asked for the same thing.
    let input = if input.is_null() { json!({}) } else { input.clone() };
    if let Err(e) = SessionInfoInput::deserialize(&input) {
        return (format!("invalid input for session_info: {e}"), true);
    }
    let out = format!(
        "krowk session: {}\nturn: {}\ninstance: {}\nmodel: {}\nbackend: {}\nworking directory: {}\nkrowk: {}\nresume with: krowk -p --resume {} \"…\"",
        env.session_id,
        env.turn_id,
        env.model.instance,
        env.model.model,
        env.backend,
        env.cwd.display(),
        env.krowk_version,
        env.session_id,
    );
    (out, false)
}

/// The exposed tools as definitions, for a context record.
pub fn definitions() -> Vec<ToolDefinition> {
    EXPOSED.iter().map(|t| ToolDefinition { name: format!("mcp__{SERVER}__{}", t.name), description: t.description.into(), input_schema: (t.input_schema)(), grammar: None }).collect()
}

/// Answers one JSON-RPC message from the backend's MCP client. A
/// notification is acknowledged with an empty result, which is what a
/// control channel that expects an answer to every request is sent.
pub fn handle(msg: &Value, env: &BridgeEnv<'_>) -> Value {
    let id = msg.get("id").cloned();
    let method = msg.get("method").and_then(Value::as_str).unwrap_or_default();
    let params = msg.get("params").cloned().unwrap_or(Value::Null);
    let result = match method {
        "initialize" => Ok(json!({
            "protocolVersion": params.get("protocolVersion").and_then(Value::as_str).unwrap_or(MCP_VERSION),
            "capabilities": {"tools": {}},
            "serverInfo": {"name": SERVER, "version": env.krowk_version},
        })),
        "tools/list" => Ok(json!({
            "tools": EXPOSED.iter().map(|t| json!({"name": t.name, "description": t.description, "inputSchema": (t.input_schema)()})).collect::<Vec<_>>(),
        })),
        "tools/call" => {
            let name = params.get("name").and_then(Value::as_str).unwrap_or_default();
            let args = params.get("arguments").cloned().unwrap_or(Value::Null);
            match EXPOSED.iter().find(|t| t.name == name) {
                Some(t) => {
                    let (text, is_error) = (t.run)(env, &args);
                    Ok(json!({"content": [{"type": "text", "text": text}], "isError": is_error}))
                }
                // An unknown tool is answered as a tool error the model can
                // read, naming the ones there are.
                None => Ok(json!({
                    "content": [{"type": "text", "text": format!("krowk has no tool {name:?} — it offers {}", EXPOSED.iter().map(|t| t.name).collect::<Vec<_>>().join(", "))}],
                    "isError": true,
                })),
            }
        }
        "ping" => Ok(json!({})),
        m if m.starts_with("notifications/") => Ok(json!({})),
        m => Err((-32601, format!("krowk's MCP server does not implement {m:?}"))),
    };
    let mut answer = json!({"jsonrpc": "2.0"});
    if let Some(id) = id {
        answer["id"] = id;
    }
    match result {
        Ok(r) => answer["result"] = r,
        Err((code, message)) => answer["error"] = json!({"code": code, "message": message}),
    }
    answer
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn r_back_1_the_bridge_lists_and_runs_session_info_and_refuses_what_it_lacks() {
        let model = ModelRef { instance: "claude:work".into(), model: "haiku".into() };
        let env = BridgeEnv { session_id: "s-1", turn_id: "t-1", model: &model, cwd: Path::new("/repo"), backend: "claude-code", krowk_version: "test" };
        let init = handle(&json!({"jsonrpc": "2.0", "id": 0, "method": "initialize", "params": {"protocolVersion": "2025-11-25"}}), &env);
        assert_eq!(init["id"], 0);
        assert_eq!(init["result"]["protocolVersion"], "2025-11-25", "the client's revision is answered");
        assert_eq!(init["result"]["serverInfo"]["name"], "krowk");
        let list = handle(&json!({"jsonrpc": "2.0", "id": 1, "method": "tools/list"}), &env);
        assert_eq!(list["result"]["tools"][0]["name"], "session_info");
        assert_eq!(list["result"]["tools"][0]["inputSchema"]["type"], "object");
        let call = handle(&json!({"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": {"name": "session_info", "arguments": {}}}), &env);
        let text = call["result"]["content"][0]["text"].as_str().unwrap();
        assert!(text.contains("krowk session: s-1") && text.contains("instance: claude:work") && text.contains("backend: claude-code"), "{text}");
        assert_eq!(call["result"]["isError"], false);
        let bad = handle(&json!({"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "session_info", "arguments": {"x": 1}}}), &env);
        assert_eq!(bad["result"]["isError"], true);
        let missing = handle(&json!({"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": {"name": "nope"}}), &env);
        assert!(missing["result"]["content"][0]["text"].as_str().unwrap().contains("offers session_info"));
        let note = handle(&json!({"jsonrpc": "2.0", "method": "notifications/initialized"}), &env);
        assert!(note.get("id").is_none() && note["result"] == json!({}));
        assert_eq!(handle(&json!({"jsonrpc": "2.0", "id": 5, "method": "resources/list"}), &env)["error"]["code"], -32601);
        assert_eq!(definitions()[0].name, "mcp__krowk__session_info");
    }
}
