//! Stand-ins for the OpenAI Responses API, a Chat Completions server (xAI,
//! OpenRouter) and an OAuth authorization server like xAI's, built on the
//! mock server in `mock.rs` — which the including file must declare as
//! `mod mock` beside this one. The SSE fixtures are recorded streams in each
//! API's documented shape; the authorization server follows RFC 8414
//! (metadata), 7591 (registration), 7636 (PKCE), 8628 (device codes) and
//! 6749's refresh grant, rotating the refresh token as xAI does.
#![allow(dead_code)]

use super::mock::{self, Mock, Reply, Seen};
use serde_json::{json, Value};
use std::collections::HashMap;
use std::sync::{Arc, Mutex};

pub fn fixture(name: &str) -> String {
    match name {
        "openai/turn1_apply_patch.sse" => include_str!("../fixtures/openai/turn1_apply_patch.sse"),
        "openai/turn1_answer.sse" => include_str!("../fixtures/openai/turn1_answer.sse"),
        "openai/turn2_answer.sse" => include_str!("../fixtures/openai/turn2_answer.sse"),
        "openai/failed.sse" => include_str!("../fixtures/openai/failed.sse"),
        "chat/xai_tool_call.sse" => include_str!("../fixtures/chat/xai_tool_call.sse"),
        "chat/xai_answer.sse" => include_str!("../fixtures/chat/xai_answer.sse"),
        "chat/openrouter_reasoning.sse" => include_str!("../fixtures/chat/openrouter_reasoning.sse"),
        other => panic!("no fixture {other}"),
    }
    .to_string()
}

/// The encrypted reasoning `openai/turn1_apply_patch.sse` streams.
pub const ENCRYPTED: &str = "gAAAAABo+Q2k3Rz/7xLmPq9vT2nB4k==+Zq9Hc8JtUe0yW5rN6mF7gD2lXoPiAv+EQ/QmFyZSBlbmNyeXB0ZWQgcmVhc29uaW5nLCBvcGFxdWUgdG8ga3Jvd2s=";

/// A GPT that patches README.md with apply_patch, then answers once it has
/// the result, and answers a second prompt from the cache. Chosen by what
/// the request holds, so it answers a real client in any order.
pub fn responses_script(body: &Value, _n: usize) -> Reply {
    let input = body["input"].as_array().cloned().unwrap_or_default();
    let last = input.last().cloned().unwrap_or_default();
    let prompts = input.iter().filter(|i| i["role"] == "user").count();
    if matches!(last["type"].as_str(), Some("custom_tool_call_output" | "function_call_output")) {
        Reply::sse(&fixture("openai/turn1_answer.sse"))
    } else if prompts > 1 {
        Reply::sse(&fixture("openai/turn2_answer.sse"))
    } else {
        Reply::sse(&fixture("openai/turn1_apply_patch.sse"))
    }
}

/// A Grok that edits README.md with search_replace, then answers.
pub fn chat_script(body: &Value, _n: usize) -> Reply {
    let messages = body["messages"].as_array().cloned().unwrap_or_default();
    if messages.last().is_some_and(|m| m["role"] == "tool") {
        Reply::sse(&fixture("chat/xai_answer.sse"))
    } else {
        Reply::sse(&fixture("chat/xai_tool_call.sse"))
    }
}

/// What the authorization server has issued.
#[derive(Debug, Default)]
pub struct AuthState {
    pub access: String,
    pub refresh: String,
    /// Seconds an access token lives.
    pub expires_in: i64,
    /// Polls answered `authorization_pending` before a device code is approved.
    pub pending_polls: u32,
    pub issued: u32,
    pub refreshes: u32,
    /// Authorization codes: code → (challenge, redirect URI).
    pub codes: HashMap<String, (String, String)>,
    /// Offer dynamic registration in the metadata.
    pub registration: bool,
    /// The issuer the metadata names, when not the server's own URL.
    pub issuer_override: Option<String>,
    /// How long a refresh takes to answer, so two can overlap.
    pub refresh_delay_ms: u64,
}

pub struct AuthServer {
    pub mock: Mock,
    pub state: Arc<Mutex<AuthState>>,
}

fn b64url(bytes: &[u8]) -> String {
    const A: &[u8] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
    let mut out = String::new();
    for c in bytes.chunks(3) {
        let n = (u32::from(c[0]) << 16) | (u32::from(*c.get(1).unwrap_or(&0)) << 8) | u32::from(*c.get(2).unwrap_or(&0));
        for i in 0..=c.len() {
            out.push(A[((n >> (18 - 6 * i)) & 63) as usize] as char);
        }
    }
    out
}

/// `a=1&b=%2F` read into pairs: the one decoding a form and a query need.
/// By hand, so every crate that includes this file can.
fn form(raw: &str) -> HashMap<String, String> {
    let decode = |s: &str| {
        let b = s.as_bytes();
        let mut out = Vec::new();
        let mut i = 0;
        while i < b.len() {
            match b[i] {
                b'+' => out.push(b' '),
                b'%' if i + 2 < b.len() => {
                    out.push(u8::from_str_radix(&s[i + 1..i + 3], 16).unwrap_or(b'?'));
                    i += 2;
                }
                c => out.push(c),
            }
            i += 1;
        }
        String::from_utf8_lossy(&out).into_owned()
    };
    raw.split('&').filter(|p| !p.is_empty()).map(|p| p.split_once('=').unwrap_or((p, ""))).map(|(k, v)| (decode(k), decode(v))).collect()
}

impl AuthState {
    fn issue(&mut self) -> Value {
        self.issued += 1;
        self.access = format!("xai-at-{}", self.issued);
        self.refresh = format!("xai-rt-{}", self.issued);
        json!({"access_token": self.access, "token_type": "Bearer", "expires_in": self.expires_in, "refresh_token": self.refresh, "scope": "openid offline_access"})
    }
}

/// An authorization server on a loopback port, as `issuer`.
pub fn auth_server(expires_in: i64) -> AuthServer {
    let state = Arc::new(Mutex::new(AuthState { expires_in, pending_polls: 1, registration: true, ..AuthState::default() }));
    let st = state.clone();
    let url: Arc<Mutex<String>> = Arc::default();
    let base = url.clone();
    let m = mock::serve_seen(move |seen: &Seen, _| {
        let issuer = base.lock().unwrap().clone();
        let delay = st.lock().unwrap().refresh_delay_ms;
        if delay > 0 && seen.raw.contains("grant_type=refresh_token") {
            std::thread::sleep(std::time::Duration::from_millis(delay));
        }
        let mut s = st.lock().unwrap();
        let (path, query) = seen.path.split_once('?').unwrap_or((&seen.path, ""));
        let f = form(if seen.method == "GET" { query } else { &seen.raw });
        let bad = |e: &str| Reply::json(400, &json!({"error": e}));
        match (seen.method.as_str(), path) {
            ("GET", "/.well-known/oauth-authorization-server") => {
                let mut meta = json!({
                    "issuer": s.issuer_override.clone().unwrap_or_else(|| issuer.clone()),
                    "authorization_endpoint": format!("{issuer}/oauth2/auth"),
                    "token_endpoint": format!("{issuer}/oauth2/token"),
                    "device_authorization_endpoint": format!("{issuer}/oauth2/device/code"),
                    "code_challenge_methods_supported": ["S256"],
                });
                if s.registration {
                    meta["registration_endpoint"] = json!(format!("{issuer}/oauth2/register"));
                }
                Reply::json(200, &meta)
            }
            ("POST", "/oauth2/register") if seen.body["token_endpoint_auth_method"] == "none" => Reply::json(201, &json!({"client_id": "krowk-registered"})),
            ("POST", "/oauth2/device/code") if !f.get("client_id").is_none_or(String::is_empty) => Reply::json(
                200,
                &json!({"device_code": "dev-code-1", "user_code": "KRWK-2026", "verification_uri": format!("{issuer}/device"), "verification_uri_complete": format!("{issuer}/device?user_code=KRWK-2026"), "expires_in": 600, "interval": 1}),
            ),
            ("GET", "/oauth2/auth") => {
                if f.get("response_type").map(String::as_str) != Some("code") || f.get("code_challenge_method").map(String::as_str) != Some("S256") {
                    return bad("invalid_request");
                }
                let code = format!("code-{}", s.codes.len() + 1);
                let redirect = f["redirect_uri"].clone();
                s.codes.insert(code.clone(), (f["code_challenge"].clone(), redirect.clone()));
                Reply::redirect(&format!("{redirect}?code={code}&state={}", f["state"]))
            }
            ("POST", "/oauth2/token") => match f.get("grant_type").map(String::as_str) {
                Some("urn:ietf:params:oauth:grant-type:device_code") if f.get("device_code").map(String::as_str) == Some("dev-code-1") => {
                    if s.pending_polls > 0 {
                        s.pending_polls -= 1;
                        return bad("authorization_pending");
                    }
                    Reply::json(200, &s.issue())
                }
                Some("authorization_code") => {
                    let Some((challenge, redirect)) = f.get("code").and_then(|c| s.codes.remove(c)) else { return bad("invalid_grant") };
                    let verifier = f.get("code_verifier").cloned().unwrap_or_default();
                    use sha2::Digest as _;
                    if b64url(&sha2::Sha256::digest(verifier.as_bytes())) != challenge || f.get("redirect_uri") != Some(&redirect) {
                        return bad("invalid_grant");
                    }
                    Reply::json(200, &s.issue())
                }
                Some("refresh_token") if f.get("refresh_token") == Some(&s.refresh) && !s.refresh.is_empty() => {
                    s.refreshes += 1;
                    Reply::json(200, &s.issue())
                }
                _ => bad("invalid_grant"),
            },
            _ => Reply::json(404, &json!({"error": "not_found"})),
        }
    });
    *url.lock().unwrap() = m.url.clone();
    AuthServer { mock: m, state }
}

/// A Chat Completions server that takes only the access token `auth`
/// last issued — a refreshed-away one is refused 401 — then follows the
/// Grok script.
pub fn chat_behind(auth: Arc<Mutex<AuthState>>) -> Mock {
    mock::serve_seen(move |seen, n| {
        let want = format!("Bearer {}", auth.lock().unwrap().access);
        if seen.header("authorization") != Some(want.as_str()) {
            return Reply::json(401, &json!({"code": "unauthorized", "error": "invalid or expired token"}));
        }
        chat_script(&seen.body, n)
    })
}

/// Follows a sign-in link as a browser would: the authorization server's
/// redirect, then the loopback callback it points at. Returns the page.
pub fn browse(url: &str) -> String {
    let get = |u: &str| -> (u16, Option<String>, String) {
        let rest = u.strip_prefix("http://").expect("a loopback http URL");
        let (addr, target) = rest.split_at(rest.find('/').unwrap_or(rest.len()));
        let mut conn = std::net::TcpStream::connect(addr).unwrap();
        use std::io::{Read, Write};
        write!(conn, "GET {target} HTTP/1.1\r\nhost: x\r\nconnection: close\r\n\r\n").unwrap();
        let mut out = String::new();
        let _ = conn.read_to_string(&mut out);
        let status = out.split_whitespace().nth(1).and_then(|s| s.parse().ok()).unwrap_or(0);
        let location = out.lines().find_map(|l| l.strip_prefix("location: ").map(String::from));
        (status, location, out)
    };
    let (status, location, _) = get(url);
    assert_eq!(status, 302, "the authorization server redirects back");
    get(&location.unwrap()).2
}
