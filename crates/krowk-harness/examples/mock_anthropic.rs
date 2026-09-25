//! The stand-in Anthropic API the tests use, as a process, for driving a
//! built `krowk -p` by hand without a key:
//!
//! ```text
//! cargo run -p krowk-harness --example mock_anthropic -- 8788 &
//! ANTHROPIC_API_KEY=sk-test ANTHROPIC_BASE_URL=http://127.0.0.1:8788 krowk -p "read README.md and summarise it in one line"
//! ```
//!
//! It follows the README script (a read call, the answer, then the
//! follow-up for a resumed session) and prints one line per request.

#[path = "../tests/common/mock.rs"]
mod mock;

fn main() {
    let port = std::env::args().nth(1).unwrap_or_else(|| "8788".into());
    let listener = std::net::TcpListener::bind(format!("127.0.0.1:{port}")).expect("bind the port");
    let m = mock::serve_on(listener, |body, n| {
        let msgs = body["messages"].as_array().map_or(0, Vec::len);
        let breakpoints = body.to_string().matches("cache_control").count();
        let signed = body.to_string().contains("\"signature\":");
        eprintln!("mock: request {n}: model {} · {msgs} messages · {breakpoints} cache breakpoints · thinking replayed: {signed}", body["model"]);
        mock::readme_script(body, n)
    });
    eprintln!("mock: Anthropic stand-in on {}", m.url);
    loop {
        std::thread::park();
    }
}
