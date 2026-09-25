//! The stand-in Anthropic API the tests use, as a process, for driving a
//! built `krowk -p` — or the TUI — by hand without a key:
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
//!
//! `read-edit` reads README.md, then rewords it with whichever edit tool is
//! offered, then answers: a turn with a read, a diff and thinking in it.
//! `--pace MS` sends any scripted reply one event every MS milliseconds, so
//! its thinking and text can be watched arriving. `--fail STATUS` answers
//! every request with that HTTP status and an Anthropic error body.
//!
//! `publish FILE` calls `publish` on FILE (in the working directory), then
//! answers: run krowk with `--dev` and the stand-in registry to see the
//! artifact land. `reread` reads README.md forever, each call reading 20,000
//! tokens from cache — about $0.0067 at Sonnet's prices — so `--max-usd` and
//! `--max-tokens` have something to stop.
//!
//! `--long LINES` answers every prompt instead with that many numbered lines
//! (about twelve tokens each), streamed at `--rate TOKENS` a second (default
//! 500): the long, steady answer the TUI's scrollback and redraw checks
//! stream.

#[path = "../tests/common/mock.rs"]
mod mock;

fn main() {
    let mut args = std::env::args().skip(1);
    let mut port = "8788".to_string();
    let (mut long, mut rate, mut edit, mut read_edit, mut pace, mut fail) = (0usize, 500u64, false, false, 0u64, 0u16);
    let (mut publish, mut reread): (Option<String>, bool) = (None, false);
    while let Some(a) = args.next() {
        match a.as_str() {
            "--long" => long = args.next().and_then(|v| v.parse().ok()).expect("--long LINES"),
            "--rate" => rate = args.next().and_then(|v| v.parse().ok()).expect("--rate TOKENS"),
            "--pace" => pace = args.next().and_then(|v| v.parse().ok()).expect("--pace MS"),
            "--fail" => fail = args.next().and_then(|v| v.parse().ok()).expect("--fail STATUS"),
            "edit" => edit = true,
            "read-edit" => read_edit = true,
            "publish" => publish = Some(args.next().expect("publish FILE")),
            "reread" => reread = true,
            p => port = p.to_string(),
        }
    }
    let listener = std::net::TcpListener::bind(format!("127.0.0.1:{port}")).expect("bind the port");
    let m = mock::serve_on(listener, move |body, n| {
        let msgs = body["messages"].as_array().map_or(0, Vec::len);
        let breakpoints = body.to_string().matches("cache_control").count();
        let signed = body.to_string().contains("\"signature\":");
        let tools: Vec<&str> = body["tools"].as_array().into_iter().flatten().filter_map(|t| t["name"].as_str()).collect();
        eprintln!("mock: request {n}: model {} · {msgs} messages · {breakpoints} cache breakpoints · thinking replayed: {signed} · tools {}", body["model"], tools.join(","));
        if long > 0 {
            return mock::Reply::paced(mock::text_stream(&mock::numbered_lines(long)), std::time::Duration::from_micros(1_000_000 / rate.max(1)));
        }
        if fail > 0 {
            let kind = if fail == 529 { "overloaded_error" } else if fail == 401 { "authentication_error" } else { "api_error" };
            return mock::Reply::json(fail, &serde_json::json!({"type": "error", "error": {"type": kind, "message": "the stand-in was told to fail"}}));
        }
        let answered = body["messages"].as_array().and_then(|m| m.last()).and_then(|m| m["content"].as_array()).is_some_and(|c| c.iter().any(|b| b["type"] == "tool_result"));
        let reply = if reread {
            let call = mock::tool_use(&format!("toolu_{n:02}"), "read", &serde_json::json!({"path": "README.md"}));
            mock::Reply::sse(&call.replace("\"cache_read_input_tokens\":0", "\"cache_read_input_tokens\":20000"))
        } else if let Some(file) = publish.as_ref().filter(|_| !answered) {
            mock::Reply::sse(&mock::tool_use(&format!("toolu_pub{n}"), "publish", &serde_json::json!({"files": [file], "caption": "published by the stand-in"})))
        } else if publish.is_some() {
            mock::Reply::sse(&mock::fixture("turn2_answer.sse"))
        } else if read_edit {
            read_edit_script(body)
        } else if edit {
            mock::edit_script(body, n)
        } else {
            mock::readme_script(body, n)
        };
        if pace > 0 && reply.status == 200 {
            return mock::Reply::paced(reply.body, std::time::Duration::from_millis(pace));
        }
        reply
    });
    eprintln!("mock: Anthropic stand-in on {}", m.url);
    loop {
        std::thread::park();
    }
}

/// Read, then edit, then answer, by how many tool results the request has.
fn read_edit_script(body: &serde_json::Value) -> mock::Reply {
    let results = body["messages"].as_array().into_iter().flatten().flat_map(|m| m["content"].as_array().into_iter().flatten()).filter(|b| b["type"] == "tool_result").count();
    match results {
        0 => mock::Reply::sse(&mock::fixture("turn1_tool_use.sse")),
        1 => mock::edit_script(&serde_json::json!({"messages": [], "tools": body["tools"]}), 0),
        _ => mock::Reply::sse(&mock::fixture("turn1_answer.sse")),
    }
}
