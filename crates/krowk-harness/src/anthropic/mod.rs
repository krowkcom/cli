//! krowk's own streaming client for the Anthropic Messages API — no SDK and
//! no multi-provider crate, because those flatten away exactly what this
//! keeps: thinking signatures, redacted thinking, and where the cache
//! breakpoints go (R-PROV-1).
//!
//! Prompt caching is on, and placed rather than hoped for (R-PROV-3). The
//! prompt renders tools → system → messages, and a breakpoint caches
//! everything before it, so there are three:
//!
//! 1. on the system block — tools and system, identical for the whole
//!    session;
//! 2. on the last block of the last message — this call's whole prefix,
//!    which the next call in the tool loop, or the next turn, reads back;
//! 3. on the last block of the user message before that — where the previous
//!    call put its breakpoint 2, so its cache entry is an explicit read
//!    point even when a turn appends more than the API's lookback reaches.
//!
//! The history is only ever appended to: nothing earlier is re-rendered
//! differently from one call to the next, which is what keeps both the
//! cache and replayed thinking valid.

pub mod sse;
pub mod stream;

use crate::engine::{BoxFuture, EngineError, Events, HistoryItem};
use crate::instances::{Resolved, Thinking};
use crate::native::{ModelClient, ModelRequest, ModelResponse};
use crate::protocol::{Item, ProviderBlob, WireApi};
use serde_json::{json, Map, Value};
use std::sync::Arc;
use std::time::Duration;
use tokio::sync::watch;

pub const API_VERSION: &str = "2023-06-01";
/// Attempts per call when the API answers busy (429, 5xx, 529) or cannot be
/// reached, before any of the response has streamed.
const ATTEMPTS: u32 = 3;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(20);
/// A stream that sends nothing — not even a ping — for this long is dead.
const IDLE_TIMEOUT: Duration = Duration::from_secs(300);

pub struct AnthropicClient {
    http: reqwest::Client,
    instance: Resolved,
    user_agent: String,
}

impl AnthropicClient {
    pub fn new(instance: Resolved, krowk_version: &str) -> Result<AnthropicClient, EngineError> {
        Ok(AnthropicClient { http: http_client()?, instance, user_agent: format!("krowk/{krowk_version}") })
    }
}

/// rustls on ring with the platform verifier: the same trust decisions the
/// rest of krowk makes through ureq.
fn http_client() -> Result<reqwest::Client, EngineError> {
    let tls_failed = |e: rustls::Error| EngineError::new("tls_unavailable", format!("the TLS configuration could not be built: {e}"));
    let provider = Arc::new(rustls::crypto::ring::default_provider());
    let tls = rustls::ClientConfig::builder_with_provider(provider)
        .with_safe_default_protocol_versions()
        .map_err(tls_failed)?;
    let tls = rustls_platform_verifier::BuilderVerifierExt::with_platform_verifier(tls).map_err(tls_failed)?.with_no_client_auth();
    reqwest::Client::builder()
        .use_preconfigured_tls(tls)
        .connect_timeout(CONNECT_TIMEOUT)
        .read_timeout(IDLE_TIMEOUT)
        .redirect(reqwest::redirect::Policy::none())
        .build()
        .map_err(|e| EngineError::new("tls_unavailable", format!("the HTTP client could not be built: {e}")))
}

/// The request body for one call. Public for the fixture tests, which pin
/// its shape: replayed thinking and the cache breakpoints above all.
pub fn request_body(req: &ModelRequest, instance: &Resolved) -> Value {
    let ephemeral = || json!({ "type": "ephemeral" });
    let mut messages = messages(&req.history);
    let user_turns: Vec<usize> = messages.iter().enumerate().filter(|(_, m)| m["role"] == "user").map(|(i, _)| i).collect();
    for &i in user_turns.iter().rev().take(2) {
        if let Some(last) = messages[i]["content"].as_array_mut().and_then(|c| c.last_mut()) {
            last["cache_control"] = ephemeral();
        }
    }
    let tools: Vec<Value> = req.tools.iter().map(|t| json!({ "name": t.name, "description": t.description, "input_schema": t.input_schema })).collect();
    let mut body = Map::new();
    body.insert("model".into(), json!(req.model));
    body.insert("max_tokens".into(), json!(instance.max_tokens));
    body.insert("stream".into(), json!(true));
    body.insert("system".into(), json!([{ "type": "text", "text": req.system, "cache_control": ephemeral() }]));
    if !tools.is_empty() {
        body.insert("tools".into(), Value::Array(tools));
    }
    if instance.thinking == Thinking::Adaptive {
        body.insert("thinking".into(), json!({ "type": "adaptive" }));
    }
    body.insert("messages".into(), Value::Array(messages));
    Value::Object(body)
}

/// The branch as Messages-API messages. Items of one response are one
/// assistant message; everything a person or a tool produced between them
/// is one user message, tool results first, as the API requires.
fn messages(history: &[HistoryItem]) -> Vec<Value> {
    let mut out: Vec<(String, Vec<Value>)> = Vec::new();
    let push = |out: &mut Vec<(String, Vec<Value>)>, role: &str, block: Value| match out.last_mut() {
        Some((r, blocks)) if r == role => blocks.push(block),
        _ => out.push((role.into(), vec![block])),
    };
    for h in history {
        match &h.item {
            Item::UserText { text } => push(&mut out, "user", json!({ "type": "text", "text": text })),
            Item::ToolResult { call_id, output, is_error } => {
                let block = json!({ "type": "tool_result", "tool_use_id": call_id, "content": output, "is_error": is_error });
                // Results lead their message: insert before any text already there.
                match out.last_mut() {
                    Some((r, blocks)) if r == "user" => {
                        let at = blocks.iter().take_while(|b| b["type"] == "tool_result").count();
                        blocks.insert(at, block);
                    }
                    _ => out.push(("user".into(), vec![block])),
                }
            }
            Item::AssistantText { text } if !text.is_empty() => push(&mut out, "assistant", json!({ "type": "text", "text": text })),
            Item::AssistantText { .. } => {}
            Item::ToolCall { call_id, name, input } => push(&mut out, "assistant", json!({ "type": "tool_use", "id": call_id, "name": name, "input": input })),
            // Replayed only when the blob itself says it is ours — never
            // by where the item sits, which a response that failed before
            // completing does not record. Reasoning that cannot replay is
            // left out of the request, never turned into assistant text:
            // that would put words in the model's mouth it never said.
            Item::Reasoning { text, blob } => {
                if let Some(block) = blob.as_ref().filter(|b| b.provider == stream::PROVIDER && b.wire_api == WireApi::AnthropicMessages).and_then(|b| replay(b, text)) {
                    push(&mut out, "assistant", block);
                }
            }
        }
    }
    close_dangling_calls(&mut out);
    out.into_iter().map(|(role, content)| json!({ "role": role, "content": content })).collect()
}

/// A thinking block rebuilt from its blob, the signature untouched.
fn replay(blob: &ProviderBlob, text: &str) -> Option<Value> {
    match blob.data.get("type")?.as_str()? {
        "thinking" => Some(json!({ "type": "thinking", "thinking": text, "signature": blob.data.get("signature")? })),
        "redacted_thinking" => Some(json!({ "type": "redacted_thinking", "data": blob.data.get("data")? })),
        _ => None,
    }
}

/// Every tool_use needs its tool_result in the very next message. A turn
/// interrupted between the two leaves a call with none, which is answered
/// here rather than sent back unanswered.
fn close_dangling_calls(out: &mut Vec<(String, Vec<Value>)>) {
    let mut i = 0;
    while i < out.len() {
        if out[i].0 == "assistant" {
            let calls: Vec<String> = out[i].1.iter().filter(|b| b["type"] == "tool_use").filter_map(|b| b["id"].as_str().map(String::from)).collect();
            if !calls.is_empty() {
                if out.get(i + 1).is_none_or(|(r, _)| r != "user") {
                    out.insert(i + 1, ("user".into(), Vec::new()));
                }
                let next = &mut out[i + 1].1;
                let missing: Vec<&String> = calls.iter().filter(|id| !next.iter().any(|b| b["tool_use_id"] == id.as_str())).collect();
                for (n, id) in missing.into_iter().enumerate() {
                    next.insert(n, json!({ "type": "tool_result", "tool_use_id": id, "content": "no result was recorded: the turn stopped first", "is_error": true }));
                }
            }
        }
        i += 1;
    }
    out.retain(|(_, blocks)| !blocks.is_empty());
}

impl ModelClient for AnthropicClient {
    fn provider(&self) -> &str {
        stream::PROVIDER
    }

    fn wire_api(&self) -> WireApi {
        WireApi::AnthropicMessages
    }

    fn stream<'a>(&'a self, req: &'a ModelRequest, events: &'a Events, cancel: watch::Receiver<bool>) -> BoxFuture<'a, Result<ModelResponse, EngineError>> {
        Box::pin(async move {
            let inst = &self.instance;
            if inst.api_key.is_empty() {
                return Err(EngineError::new(
                    "not_authenticated",
                    format!("no API key for the {} instance — set {} (krowk reads the key from the environment, never from a file)", inst.name, inst.api_key_env),
                ));
            }
            let body = request_body(req, inst).to_string();
            let url = format!("{}/v1/messages", inst.base_url);
            let mut resp = None;
            for attempt in 1..=ATTEMPTS {
                let sent = self
                    .http
                    .post(&url)
                    .header("x-api-key", &inst.api_key)
                    .header("anthropic-version", API_VERSION)
                    .header("content-type", "application/json")
                    .header("accept", "text/event-stream")
                    .header("user-agent", &self.user_agent)
                    .body(body.clone())
                    .send();
                let mut cancel_wait = cancel.clone();
                let r = tokio::select! {
                    r = sent => r,
                    _ = crate::engine::cancelled(&mut cancel_wait) => return Ok(ModelResponse { model: req.model.clone(), interrupted: true, ..ModelResponse::default() }),
                };
                let retry_after = match r {
                    Ok(r) if r.status().is_success() => {
                        resp = Some(r);
                        break;
                    }
                    Ok(r) if attempt < ATTEMPTS && retryable(r.status().as_u16()) => retry_delay(&r, attempt),
                    Ok(r) => return Err(http_error(r, inst, &req.model).await),
                    Err(e) if attempt < ATTEMPTS && (e.is_connect() || e.is_timeout()) => Duration::from_secs(u64::from(attempt)),
                    Err(e) => return Err(transport_error(&e, inst)),
                };
                let mut cancel_wait = cancel.clone();
                tokio::select! {
                    _ = tokio::time::sleep(retry_after) => {}
                    _ = crate::engine::cancelled(&mut cancel_wait) => return Ok(ModelResponse { model: req.model.clone(), interrupted: true, ..ModelResponse::default() }),
                }
            }
            let mut resp = resp.expect("the loop breaks with a response or returns");
            let mut parser = sse::SseParser::default();
            let mut dec = stream::Decoder::default();
            let mut cancel_wait = cancel.clone();
            loop {
                let chunk = tokio::select! {
                    c = resp.chunk() => c.map_err(|e| transport_error(&e, inst))?,
                    _ = crate::engine::cancelled(&mut cancel_wait) => {
                        for ev in dec.interrupt() {
                            let _ = events.send(ev).await;
                        }
                        return Ok(finish(dec, req, true));
                    }
                };
                let ended = chunk.is_none();
                let evs = match chunk {
                    Some(bytes) => parser.push(&bytes),
                    None => parser.finish().into_iter().collect(),
                };
                for sse in &evs {
                    for ev in dec.apply(sse)? {
                        let _ = events.send(ev).await;
                    }
                }
                if dec.done {
                    return Ok(finish(dec, req, false));
                }
                if ended {
                    return Err(EngineError::new(
                        "provider_unavailable",
                        "the Anthropic stream ended before the response did — the connection dropped; run the prompt again with --resume",
                    ));
                }
            }
        })
    }
}

fn finish(dec: stream::Decoder, req: &ModelRequest, interrupted: bool) -> ModelResponse {
    ModelResponse {
        response_id: dec.response_id,
        model: if dec.model.is_empty() { req.model.clone() } else { dec.model },
        usage: dec.usage,
        stop_reason: dec.stop_reason,
        items: dec.items,
        interrupted,
    }
}

fn retryable(status: u16) -> bool {
    matches!(status, 408 | 409 | 429 | 500 | 502 | 503 | 504 | 529)
}

/// `retry-after` when the API gives one (capped), else a short backoff.
fn retry_delay(r: &reqwest::Response, attempt: u32) -> Duration {
    let named = r.headers().get("retry-after").and_then(|v| v.to_str().ok()).and_then(|v| v.trim().parse::<u64>().ok());
    Duration::from_secs(named.unwrap_or(u64::from(attempt) * 2).min(30))
}

async fn http_error(r: reqwest::Response, inst: &Resolved, model: &str) -> EngineError {
    let status = r.status().as_u16();
    let body = r.text().await.unwrap_or_default();
    let (kind, message) = serde_json::from_str::<Value>(&body)
        .ok()
        .and_then(|v| {
            let e = v.get("error")?;
            Some((e.get("type")?.as_str()?.to_string(), e.get("message").and_then(Value::as_str).unwrap_or_default().to_string()))
        })
        .unwrap_or_else(|| (format!("HTTP {status}"), body.chars().take(300).collect()));
    let said = if message.is_empty() { kind.clone() } else { format!("{kind}: {message}") };
    let e = match status {
        401 => EngineError::new("provider_auth", format!("Anthropic refused the API key of the {} instance ({said}) — check {}", inst.name, inst.api_key_env)),
        403 => EngineError::new("provider_forbidden", format!("Anthropic refused the request ({said}) — the key of the {} instance may not have access to {model}", inst.name)),
        404 => EngineError::new("model_not_found", format!("Anthropic does not know the model {model:?} ({said}) — pass a current model id with --model")),
        429 => EngineError::new("rate_limited", format!("Anthropic is rate-limiting the {} instance ({said}) — wait and retry, or use another instance", inst.name)),
        s if s >= 500 => EngineError::new("provider_unavailable", format!("Anthropic answered HTTP {s} ({said}) — retry shortly")),
        _ => EngineError::new("provider_invalid_request", format!("Anthropic refused the request ({said})")),
    };
    e.with_status(status)
}

fn transport_error(e: &reqwest::Error, inst: &Resolved) -> EngineError {
    // reqwest's own text names a URL and a cause chain; the cause is the news.
    let mut cause = e.to_string();
    let mut src = std::error::Error::source(e);
    while let Some(s) = src {
        cause = s.to_string();
        src = s.source();
    }
    if e.is_timeout() {
        return EngineError::new("network_unreachable", format!("{} stopped answering ({cause}) — check the network, then run the prompt again with --resume", inst.base_url));
    }
    EngineError::new("network_unreachable", format!("{} could not be reached ({cause}) — check the network, or the base URL of the {} instance", inst.base_url, inst.name))
}
