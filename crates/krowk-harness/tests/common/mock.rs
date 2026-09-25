//! A stand-in for the Anthropic Messages API: an HTTP/1.1 server on a
//! loopback port that answers each request with a recorded SSE body, and
//! keeps every request it was sent so a test can pin what krowk asked.
//! Standard library only, and a thread per server, so the harness tests,
//! the CLI's tests and the evidence example all run the same one.
#![allow(dead_code)]

use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpListener;
use std::sync::{Arc, Mutex};

/// One request as it arrived.
#[derive(Debug, Clone)]
pub struct Seen {
    pub path: String,
    pub headers: Vec<(String, String)>,
    pub body: serde_json::Value,
}

impl Seen {
    pub fn header(&self, name: &str) -> Option<&str> {
        self.headers.iter().find(|(k, _)| k.eq_ignore_ascii_case(name)).map(|(_, v)| v.as_str())
    }
}

/// What the server answers a request with.
pub struct Reply {
    pub status: u16,
    pub body: String,
}

impl Reply {
    pub fn sse(body: &str) -> Reply {
        Reply { status: 200, body: body.to_string() }
    }
}

pub struct Mock {
    pub url: String,
    pub seen: Arc<Mutex<Vec<Seen>>>,
}

/// Serves `answer(request, n)` for the n-th request (from 0), forever.
pub fn serve(answer: impl Fn(&serde_json::Value, usize) -> Reply + Send + 'static) -> Mock {
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind a loopback port");
    serve_on(listener, answer)
}

pub fn serve_on(listener: TcpListener, answer: impl Fn(&serde_json::Value, usize) -> Reply + Send + 'static) -> Mock {
    let url = format!("http://{}", listener.local_addr().unwrap());
    let seen: Arc<Mutex<Vec<Seen>>> = Arc::default();
    let log = seen.clone();
    std::thread::spawn(move || {
        for (n, conn) in listener.incoming().enumerate() {
            let Ok(mut conn) = conn else { continue };
            let mut reader = BufReader::new(conn.try_clone().unwrap());
            let mut line = String::new();
            if reader.read_line(&mut line).is_err() {
                continue;
            }
            let path = line.split_whitespace().nth(1).unwrap_or_default().to_string();
            let mut headers = Vec::new();
            let mut length = 0usize;
            loop {
                let mut h = String::new();
                if reader.read_line(&mut h).unwrap_or(0) == 0 || h == "\r\n" {
                    break;
                }
                if let Some((k, v)) = h.trim_end().split_once(':') {
                    if k.eq_ignore_ascii_case("content-length") {
                        length = v.trim().parse().unwrap_or(0);
                    }
                    headers.push((k.trim().to_string(), v.trim().to_string()));
                }
            }
            let mut body = vec![0u8; length];
            let _ = reader.read_exact(&mut body);
            let body: serde_json::Value = serde_json::from_slice(&body).unwrap_or(serde_json::Value::Null);
            let reply = answer(&body, n);
            log.lock().unwrap().push(Seen { path, headers, body });
            let kind = if reply.status == 200 { "text/event-stream" } else { "application/json" };
            let head = format!("HTTP/1.1 {} X\r\ncontent-type: {kind}\r\ncontent-length: {}\r\nconnection: close\r\n\r\n", reply.status, reply.body.len());
            let _ = conn.write_all(head.as_bytes());
            let _ = conn.write_all(reply.body.as_bytes());
            let _ = conn.flush();
        }
    });
    Mock { url, seen }
}

/// A recorded fixture by name. Compiled in, so every crate that includes
/// this file finds them wherever it builds.
pub fn fixture(name: &str) -> String {
    match name {
        "turn1_tool_use.sse" => include_str!("../fixtures/anthropic/turn1_tool_use.sse"),
        "turn1_answer.sse" => include_str!("../fixtures/anthropic/turn1_answer.sse"),
        "turn2_answer.sse" => include_str!("../fixtures/anthropic/turn2_answer.sse"),
        "redacted_thinking.sse" => include_str!("../fixtures/anthropic/redacted_thinking.sse"),
        "overloaded.sse" => include_str!("../fixtures/anthropic/overloaded.sse"),
        other => panic!("no fixture {other}"),
    }
    .to_string()
}

/// The script the end-to-end runs follow, chosen by what the request holds
/// rather than by its number, so it answers a real client in any order: a
/// conversation whose last message carries a tool result gets the answer,
/// a second prompt gets the follow-up, and a first prompt reads README.md.
pub fn readme_script(body: &serde_json::Value, _n: usize) -> Reply {
    let messages = body["messages"].as_array().cloned().unwrap_or_default();
    let last = messages.last().cloned().unwrap_or_default();
    let has_result = last["content"].as_array().is_some_and(|c| c.iter().any(|b| b["type"] == "tool_result"));
    let prompts = messages.iter().filter(|m| m["role"] == "user" && m["content"].as_array().is_some_and(|c| c.iter().any(|b| b["type"] == "text"))).count();
    if has_result {
        Reply::sse(&fixture("turn1_answer.sse"))
    } else if prompts > 1 {
        Reply::sse(&fixture("turn2_answer.sse"))
    } else {
        Reply::sse(&fixture("turn1_tool_use.sse"))
    }
}
