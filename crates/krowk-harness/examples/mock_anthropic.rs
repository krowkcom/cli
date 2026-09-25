//! The stand-in Anthropic API the tests use, as a process, for driving a
//! built `krowk -p` by hand without a key:
//!
//! ```text
//! cargo run -p krowk-harness --example mock_anthropic -- 8788 &
//! ANTHROPIC_API_KEY=sk-test ANTHROPIC_BASE_URL=http://127.0.0.1:8788 krowk -p "read README.md and summarise it in one line"
//! ```
//!
//! It follows the README script (a read call, the answer, then the
//! follow-up for a resumed session) and prints one line per request. With
//! `edit` after the port it follows the edit script instead: one call to
//! whichever edit tool the request offers, rewording README.md, then the
//! answer — run krowk with `--permission-mode acceptEdits` for it to land.

#[path = "../tests/common/mock.rs"]
mod mock;

fn main() {
    let port = std::env::args().nth(1).unwrap_or_else(|| "8788".into());
    let edit = std::env::args().nth(2).as_deref() == Some("edit");
    let listener = std::net::TcpListener::bind(format!("127.0.0.1:{port}")).expect("bind the port");
    let m = mock::serve_on(listener, move |body, n| {
        let msgs = body["messages"].as_array().map_or(0, Vec::len);
        let breakpoints = body.to_string().matches("cache_control").count();
        let signed = body.to_string().contains("\"signature\":");
        let tools: Vec<&str> = body["tools"].as_array().into_iter().flatten().filter_map(|t| t["name"].as_str()).collect();
        eprintln!("mock: request {n}: model {} · {msgs} messages · {breakpoints} cache breakpoints · thinking replayed: {signed} · tools {}", body["model"], tools.join(","));
        if edit { mock::edit_script(body, n) } else { mock::readme_script(body, n) }
    });
    eprintln!("mock: Anthropic stand-in on {}", m.url);
    loop {
        std::thread::park();
    }
}
