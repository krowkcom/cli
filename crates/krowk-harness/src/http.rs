//! What every wire client shares: the HTTP client, the retry rule, how an
//! error answer and a dead connection are told apart and worded, and the
//! loop that reads a stream of server-sent events into a decoder.
//!
//! Redirects are never followed. A request carries a key or a token, and a
//! redirect is a request the client did not choose to make — to wherever
//! the answer says — with the credential attached.

use crate::engine::{EngineError, EngineEvent, Events};
use crate::native::ModelResponse;
use crate::sse::{SseEvent, SseParser};
use serde_json::Value;
use std::sync::Arc;
use std::time::Duration;
use tokio::sync::watch;

/// Attempts per call when the API answers busy (429, 5xx, 529) or cannot be
/// reached, before any of the response has streamed.
pub const ATTEMPTS: u32 = 3;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(20);
/// A stream that sends nothing — not even a keep-alive — for this long is dead.
const IDLE_TIMEOUT: Duration = Duration::from_secs(300);

/// rustls on ring with the platform verifier: the same trust decisions the
/// rest of krowk makes through ureq.
pub fn client() -> Result<reqwest::Client, EngineError> {
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

/// Who a request went to, for the words of a failure.
#[derive(Debug, Clone, Copy)]
pub struct Peer<'a> {
    /// The provider as a person names it: `Anthropic`, `OpenAI`, `xAI`.
    pub vendor: &'a str,
    pub instance: &'a str,
    pub base_url: &'a str,
}

/// How a request came out.
pub enum Answer {
    /// 2xx: the stream is coming.
    Streaming(reqwest::Response),
    /// Any other status, after the retries a busy one gets.
    Refused(reqwest::Response),
    Interrupted,
}

/// Sends a request, retrying a busy answer or a connection that failed
/// twice before anything streamed, honouring `retry-after`.
pub async fn send(build: &(dyn Fn() -> reqwest::RequestBuilder + Sync), cancel: &watch::Receiver<bool>, peer: Peer<'_>) -> Result<Answer, EngineError> {
    for attempt in 1..=ATTEMPTS {
        let mut cancel_wait = cancel.clone();
        let r = tokio::select! {
            r = build().send() => r,
            _ = crate::engine::cancelled(&mut cancel_wait) => return Ok(Answer::Interrupted),
        };
        let wait = match r {
            Ok(r) if r.status().is_success() => return Ok(Answer::Streaming(r)),
            Ok(r) if attempt < ATTEMPTS && retryable(r.status().as_u16()) => retry_delay(&r, attempt),
            Ok(r) => return Ok(Answer::Refused(r)),
            Err(e) if attempt < ATTEMPTS && (e.is_connect() || e.is_timeout()) => Duration::from_secs(u64::from(attempt)),
            Err(e) => return Err(transport_error(&e, peer)),
        };
        let mut cancel_wait = cancel.clone();
        tokio::select! {
            _ = tokio::time::sleep(wait) => {}
            _ = crate::engine::cancelled(&mut cancel_wait) => return Ok(Answer::Interrupted),
        }
    }
    unreachable!("the last attempt returns")
}

pub fn retryable(status: u16) -> bool {
    matches!(status, 408 | 409 | 429 | 500 | 502 | 503 | 504 | 529)
}

/// `retry-after` when the API gives one (capped), else a short backoff.
fn retry_delay(r: &reqwest::Response, attempt: u32) -> Duration {
    let named = r.headers().get("retry-after").and_then(|v| v.to_str().ok()).and_then(|v| v.trim().parse::<u64>().ok());
    Duration::from_secs(named.unwrap_or(u64::from(attempt) * 2).min(30))
}

/// An error answer's status and what its body says, in whichever of the
/// shapes providers use: `{"error": {"type"|"code", "message"}}`,
/// `{"error": "…"}`, or `{"message": "…"}` — else the body's start.
pub async fn refusal(r: reqwest::Response) -> (u16, String) {
    let status = r.status().as_u16();
    // An error body is a sentence, not a stream: one that does not arrive
    // promptly is not worth holding the turn (or an interrupt) for.
    let body = tokio::time::timeout(Duration::from_secs(10), r.text()).await.ok().and_then(Result::ok).unwrap_or_default();
    let said = serde_json::from_str::<Value>(&body).ok().and_then(|v| {
        let s = |v: &Value, k: &str| v.get(k).and_then(Value::as_str).map(String::from);
        let (kind, message) = match v.get("error") {
            Some(Value::String(m)) => (None, Some(m.clone())),
            Some(e) => (s(e, "type").or_else(|| s(e, "code")), s(e, "message")),
            None => (s(&v, "code"), s(&v, "message")),
        };
        match (kind, message) {
            (Some(k), Some(m)) if !m.is_empty() => Some(format!("{k}: {m}")),
            (_, Some(m)) if !m.is_empty() => Some(m),
            (Some(k), _) => Some(k),
            _ => None,
        }
    });
    (status, said.unwrap_or_else(|| if body.trim().is_empty() { format!("HTTP {status}") } else { clip(body.trim(), 300) }))
}

/// A refusal as the engine's error, worded for the provider. `auth_fix`
/// says what to do about a credential it refused.
pub fn status_error(status: u16, said: &str, peer: Peer<'_>, model: &str, auth_fix: &str) -> EngineError {
    let v = peer.vendor;
    let e = match status {
        401 => EngineError::new("provider_auth", format!("{v} refused the credentials of the {} instance ({said}) — {auth_fix}", peer.instance)),
        403 => EngineError::new("provider_forbidden", format!("{v} refused the request ({said}) — the {} instance may not have access to {model}", peer.instance)),
        404 => EngineError::new("model_not_found", format!("{v} does not know the model {model:?} ({said}) — pass a current model id with --model")),
        429 => EngineError::new("rate_limited", format!("{v} is rate-limiting the {} instance ({said}) — wait and retry, or use another instance", peer.instance)),
        s if s >= 500 => EngineError::new("provider_unavailable", format!("{v} answered HTTP {s} ({said}) — retry shortly")),
        _ => EngineError::new("provider_invalid_request", format!("{v} refused the request ({said})")),
    };
    e.with_status(status)
}

pub fn transport_error(e: &reqwest::Error, peer: Peer<'_>) -> EngineError {
    // reqwest's own text names a URL and a cause chain; the cause is the news.
    let mut cause = e.to_string();
    let mut src = std::error::Error::source(e);
    while let Some(s) = src {
        cause = s.to_string();
        src = s.source();
    }
    // R-OFF-1: the sentence a person reads first says what is wrong in the
    // words the TUI's notice uses, never a generic failure.
    if e.is_timeout() {
        return EngineError::new(
            "network_unreachable",
            format!("no network connectivity: {} stopped answering ({cause}) — check the network, then run the prompt again with --resume", peer.base_url),
        );
    }
    EngineError::new(
        "network_unreachable",
        format!("no network connectivity: {} could not be reached ({cause}) — check the network, or the base URL of the {} instance", peer.base_url, peer.instance),
    )
}

/// One wire API's stream, decoded into items as its events arrive.
pub trait Decode {
    /// Folds one event in; returns what the client should be told.
    fn apply(&mut self, ev: &SseEvent) -> Result<Vec<EngineEvent>, EngineError>;
    /// The response is whole.
    fn done(&self) -> bool;
    /// The connection closed without the end marker. `Some` when what
    /// arrived is a whole response all the same — a server that sends no
    /// `[DONE]` — with the events that finish it; `None` when it was cut.
    fn ended(&mut self) -> Option<Vec<EngineEvent>> {
        None
    }
    /// What survives an interrupt, completed.
    fn interrupt(&mut self) -> Vec<EngineEvent>;
    fn finish(self, requested_model: &str, interrupted: bool) -> ModelResponse;
}

/// Reads a streaming answer into `dec` to its end, forwarding every item
/// event as it happens.
pub async fn read_stream<D: Decode>(mut resp: reqwest::Response, mut dec: D, events: &Events, cancel: &watch::Receiver<bool>, model: &str, peer: Peer<'_>) -> Result<ModelResponse, EngineError> {
    let mut parser = SseParser::default();
    let mut cancel_wait = cancel.clone();
    loop {
        let chunk = tokio::select! {
            c = resp.chunk() => c.map_err(|e| transport_error(&e, peer))?,
            _ = crate::engine::cancelled(&mut cancel_wait) => {
                for ev in dec.interrupt() {
                    let _ = events.send(ev).await;
                }
                return Ok(dec.finish(model, true));
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
            if dec.done() {
                break;
            }
        }
        if dec.done() {
            return Ok(dec.finish(model, false));
        }
        if ended {
            if let Some(last) = dec.ended() {
                for ev in last {
                    let _ = events.send(ev).await;
                }
                return Ok(dec.finish(model, false));
            }
            return Err(EngineError::new(
                "provider_unavailable",
                format!("the {} stream ended before the response did — the connection dropped; run the prompt again with --resume", peer.vendor),
            ));
        }
    }
}

pub fn clip(s: &str, n: usize) -> String {
    if s.chars().count() <= n { s.to_string() } else { s.chars().take(n).collect::<String>() + "…" }
}
