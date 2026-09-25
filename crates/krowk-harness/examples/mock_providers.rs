//! Stand-ins for the OpenAI Responses API, xAI's Chat Completions and an
//! xAI-like OAuth server, as one process, for driving a built `krowk -p`
//! and `krowk providers` by hand without a key or a subscription:
//!
//! ```text
//! cargo run -p krowk-harness --example mock_providers -- 8790 &
//! OPENAI_API_KEY=sk-test OPENAI_BASE_URL=http://127.0.0.1:8790/v1 \
//!   krowk -p --model openai/gpt-5.4 --permission-mode acceptEdits "reword the README tagline"
//! ```
//!
//! The Responses stand-in is on the port given, Chat Completions on the
//! next (it takes only the token the OAuth server last issued), and the
//! OAuth server — an issuer with device and browser logins — on a free
//! port it prints. It prints one line per request.

#[path = "../tests/common/mock.rs"]
mod mock;
#[path = "../tests/common/providers.rs"]
mod providers;

fn main() {
    let port: u16 = std::env::args().nth(1).and_then(|p| p.parse().ok()).unwrap_or(8790);
    let bind = |p: u16| std::net::TcpListener::bind(format!("127.0.0.1:{p}")).expect("bind the port");
    let responses = mock::serve_on(bind(port), |body, n| {
        let input = body["input"].as_array().map_or(0, Vec::len);
        eprintln!(
            "mock responses: request {n}: model {} · {input} input items · store {} · prompt_cache_key {} · effort {} · encrypted reasoning replayed: {}",
            body["model"],
            body["store"],
            body["prompt_cache_key"],
            body["reasoning"]["effort"],
            body.to_string().contains(providers::ENCRYPTED)
        );
        providers::responses_script(body, n)
    });
    let auth = providers::auth_server(3600);
    let state = auth.state.clone();
    let chat = mock::serve_seen_on(bind(port + 1), move |seen, n| {
        let want = format!("Bearer {}", state.lock().unwrap().access);
        let ok = seen.header("authorization") == Some(want.as_str());
        eprintln!("mock chat: request {n}: model {} · token accepted: {ok} · x-grok-conv-id {:?}", seen.body["model"], seen.header("x-grok-conv-id"));
        if !ok {
            return mock::Reply::json(401, &serde_json::json!({"error": "invalid or expired token"}));
        }
        providers::chat_script(&seen.body, n)
    });
    eprintln!("mock: Responses on {}/v1, Chat Completions on {}, OAuth issuer {}", responses.url, chat.url, auth.mock.url);
    loop {
        std::thread::park();
    }
}
