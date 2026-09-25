//! The Anthropic client, offline, against recorded SSE streams (R-PROV-1).
//!
//! The fixtures are the Messages API's documented event stream — the
//! `message_start` … `message_stop` sequence with `thinking_delta`,
//! `signature_delta`, `input_json_delta` and `redacted_thinking` — written
//! out byte for byte, including a signature split across two deltas and one
//! with a JSON escape (`\u002b`) in it. They were not captured from a live
//! key; `live check pending` in the PR says how to capture one.

#[path = "common/mock.rs"]
mod mock;

use krowk_harness::anthropic::sse::SseParser;
use krowk_harness::anthropic::stream::Decoder;
use krowk_harness::anthropic::{request_body, AnthropicClient};
use krowk_harness::engine::{EngineEvent, HistoryItem};
use krowk_harness::instances::{InstancesConfig, Registry, Resolved};
use krowk_harness::native::{ModelClient, ModelRequest};
use krowk_harness::protocol::{Item, LogBody, LogEvent, ToolDefinition};
use serde_json::json;

/// The signature the tool-use fixture streams, as its two deltas decode.
const SIGNATURE: &str = "EqQBCkYIBxgCKkD3xG+4n0t/7i2rQzYkWmV9pL+/aX0c3R8s1QvT2nB4k==/+Zq9Hc8JtUe0yW5rN6mF7gD2lXoPiAv+EQ==";
const THINKING: &str = "The user wants a one-line summary of README.md. I should read the file first.";

fn decode(name: &str, chunk: usize) -> (Decoder, Vec<EngineEvent>) {
    let raw = mock::fixture(name);
    let mut p = SseParser::default();
    let mut d = Decoder::default();
    let mut out = Vec::new();
    for c in raw.as_bytes().chunks(chunk) {
        for ev in p.push(c) {
            out.extend(d.apply(&ev).unwrap());
        }
    }
    (d, out)
}

fn instance() -> Resolved {
    let env = |k: &str| if k == "ANTHROPIC_API_KEY" { "sk-test".to_string() } else { String::new() };
    Registry::resolve(&InstancesConfig::default(), &env).instances["anthropic"].clone()
}

#[test]
fn r_prov_1_a_tool_use_stream_decodes_into_items_whatever_the_chunking() {
    for chunk in [1, 7, 64, 1 << 20] {
        let (d, events) = decode("turn1_tool_use.sse", chunk);
        assert!(d.done);
        assert_eq!(d.response_id.as_deref(), Some("msg_01TurnOneToolUse"));
        assert_eq!(d.stop_reason.as_deref(), Some("tool_use"));
        assert_eq!((d.usage.input_tokens, d.usage.cache_write_tokens, d.usage.cache_read_tokens, d.usage.output_tokens), (12, 2350, 0, 84));
        let items: Vec<&Item> = d.items.iter().map(|(_, i)| i).collect();
        assert_eq!(items.len(), 3);
        assert!(matches!(items[0], Item::Reasoning { text, blob: Some(b) } if text == THINKING && b.data["signature"] == SIGNATURE && b.provider == "anthropic"));
        assert_eq!(items[1], &Item::AssistantText { text: "I'll read the README.".into() });
        assert_eq!(items[2], &Item::ToolCall { call_id: "toolu_01ReadReadme".into(), name: "read".into(), input: json!({ "path": "README.md" }) });
        // start, deltas and end share one stable id per item; the signature
        // is never streamed as a delta.
        for (id, _) in &d.items {
            let started = events.iter().position(|e| matches!(e, EngineEvent::ItemStarted { item_id, .. } if item_id == id)).unwrap();
            let ended = events.iter().position(|e| matches!(e, EngineEvent::ItemCompleted { item_id, .. } if item_id == id)).unwrap();
            assert!(started < ended);
        }
        let deltas = events.iter().filter(|e| matches!(e, EngineEvent::ItemDelta { .. })).count();
        assert_eq!(deltas, 2 + 2 + 3, "two thinking, two text and three input deltas");
    }
}

#[test]
fn r_log_3_a_thinking_signature_round_trips_through_the_log_byte_identical() {
    let (d, _) = decode("turn1_tool_use.sse", 5);
    // Through the log: each item as the host writes it, one JSONL line,
    // then read back the way a resumed session reads it.
    let lines: Vec<String> = d
        .items
        .iter()
        .map(|(id, item)| {
            let ev = LogEvent {
                id: id.clone(),
                parent_id: None,
                session_id: id.clone(),
                time_ms: 0,
                body: LogBody::ItemCompleted { turn_id: "t".into(), item_id: id.clone(), item: item.clone() },
            };
            serde_json::to_string(&ev).unwrap()
        })
        .collect();
    assert!(lines[0].contains(SIGNATURE), "the log line holds the signature verbatim: {}", lines[0]);
    let read_back: Vec<Item> = lines
        .iter()
        .map(|l| match serde_json::from_str::<LogEvent>(l).unwrap().body {
            LogBody::ItemCompleted { item, .. } => item,
            other => panic!("{other:?}"),
        })
        .collect();
    let mut history = vec![HistoryItem { item: Item::UserText { text: "read README.md and summarise it in one line".into() }, response: None }];
    history.extend(read_back.into_iter().map(|item| HistoryItem { item, response: Some(0) }));
    history.push(HistoryItem { item: Item::ToolResult { call_id: "toolu_01ReadReadme".into(), output: "# krowk".into(), is_error: false }, response: None });
    let req = ModelRequest { model: "claude-sonnet-4-6".into(), system: "s".into(), tools: vec![], history };
    let body = request_body(&req, &instance());
    let assistant = &body["messages"][1];
    assert_eq!(assistant["role"], "assistant");
    assert_eq!(assistant["content"][0], json!({ "type": "thinking", "thinking": THINKING, "signature": SIGNATURE }));
    assert_eq!(assistant["content"][2]["type"], "tool_use");
    let wire = body.to_string();
    assert!(wire.contains(&format!("\"signature\":\"{SIGNATURE}\"")), "the signature goes back byte for byte");

    // Decided by the blob, not by where the item sits: reasoning from a
    // response that failed before completing (no response index) replays too.
    let mut failed = req.clone();
    for h in failed.history.iter_mut() {
        h.response = None;
    }
    assert_eq!(request_body(&failed, &instance())["messages"][1]["content"][0]["signature"], SIGNATURE);

    // Another provider's blob never replays here, and its reasoning is not
    // passed off as something the model said.
    let mut foreign = req.clone();
    for h in foreign.history.iter_mut() {
        if let Item::Reasoning { blob: Some(b), .. } = &mut h.item {
            b.provider = "openai".into();
        }
    }
    let body = request_body(&foreign, &instance());
    assert!(!body.to_string().contains(SIGNATURE));
    assert!(!body.to_string().contains(THINKING), "reasoning never becomes assistant text");
    assert_eq!(body["messages"][1]["content"][0]["type"], "text");
    assert_eq!(body["messages"][1]["content"][0]["text"], "I'll read the README.");
}

#[test]
fn r_log_3_redacted_thinking_is_kept_opaque_and_replayed_as_it_came() {
    let (d, _) = decode("redacted_thinking.sse", 3);
    let Item::Reasoning { text, blob: Some(blob) } = &d.items[0].1 else { panic!("{:?}", d.items) };
    assert!(text.is_empty());
    let data = blob.data["data"].as_str().unwrap().to_string();
    assert_eq!(d.items[1].1, Item::AssistantText { text: "Done.".into() });
    let history = vec![
        HistoryItem { item: Item::UserText { text: "q".into() }, response: None },
        HistoryItem { item: d.items[0].1.clone(), response: Some(0) },
        HistoryItem { item: d.items[1].1.clone(), response: Some(0) },
        HistoryItem { item: Item::UserText { text: "again".into() }, response: None },
    ];
    let body = request_body(&ModelRequest { model: "m".into(), system: "s".into(), tools: vec![], history }, &instance());
    assert_eq!(body["messages"][1]["content"][0], json!({ "type": "redacted_thinking", "data": data }));
}

#[test]
fn r_prov_3_cache_breakpoints_sit_on_the_stable_prefix_and_the_growing_tail() {
    let tools = vec![ToolDefinition { name: "read".into(), description: "d".into(), input_schema: json!({ "type": "object" }) }];
    let turn = |text: &str| HistoryItem { item: Item::UserText { text: text.into() }, response: None };
    let said = |text: &str, r: usize| HistoryItem { item: Item::AssistantText { text: text.into() }, response: Some(r) };
    let history = vec![turn("one"), said("a", 0), turn("two"), said("b", 1), turn("three")];
    let body = request_body(&ModelRequest { model: "m".into(), system: "sys".into(), tools, history }, &instance());
    assert_eq!(body["system"][0]["cache_control"], json!({ "type": "ephemeral" }), "tools and system, cached together");
    let marked: Vec<(usize, usize)> = body["messages"]
        .as_array()
        .unwrap()
        .iter()
        .enumerate()
        .flat_map(|(i, m)| m["content"].as_array().unwrap().iter().enumerate().filter(|(_, b)| b.get("cache_control").is_some()).map(move |(j, _)| (i, j)))
        .collect();
    assert_eq!(marked, vec![(2, 0), (4, 0)], "the last message, and the previous call's last message");
    let count = body.to_string().matches("cache_control").count();
    assert!(count <= 4, "the API allows four breakpoints, found {count}");
    assert_eq!(body["thinking"], json!({ "type": "adaptive" }));
    assert_eq!(body["stream"], true);
}

#[test]
fn an_interrupted_tool_call_is_answered_before_it_is_sent_back() {
    let history = vec![
        HistoryItem { item: Item::UserText { text: "q".into() }, response: None },
        HistoryItem { item: Item::ToolCall { call_id: "toolu_x".into(), name: "read".into(), input: json!({}) }, response: Some(0) },
        HistoryItem { item: Item::UserText { text: "next".into() }, response: None },
    ];
    let body = request_body(&ModelRequest { model: "m".into(), system: "s".into(), tools: vec![], history }, &instance());
    let next = &body["messages"][2]["content"];
    assert_eq!(next[0]["type"], "tool_result");
    assert_eq!(next[0]["tool_use_id"], "toolu_x");
    assert_eq!(next[1]["text"], "next");
}

fn run_stream(mock_reply: mock::Reply) -> Result<krowk_harness::native::ModelResponse, krowk_harness::engine::EngineError> {
    let reply = std::sync::Mutex::new(Some(mock_reply));
    let m = mock::serve(move |_, _| reply.lock().unwrap().take().unwrap_or(mock::Reply { status: 500, body: "{}".into() }));
    let mut inst = instance();
    inst.base_url = m.url.clone();
    let client = AnthropicClient::new(inst, "test").unwrap();
    let rt = tokio::runtime::Builder::new_current_thread().enable_all().build().unwrap();
    rt.block_on(async {
        let (tx, mut rx) = tokio::sync::mpsc::channel(1024);
        let (_cancel_tx, cancel) = tokio::sync::watch::channel(false);
        let req = ModelRequest { model: "claude-sonnet-4-6".into(), system: "s".into(), tools: vec![], history: vec![] };
        let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
        let r = client.stream(&req, &tx, cancel).await;
        drop(tx);
        drain.await.unwrap();
        let seen = m.seen.lock().unwrap();
        if let Some(s) = seen.first() {
            assert_eq!(s.path, "/v1/messages");
            assert_eq!(s.header("x-api-key"), Some("sk-test"));
            assert_eq!(s.header("anthropic-version"), Some("2023-06-01"));
        }
        r
    })
}

#[test]
fn r_prov_1_the_client_streams_over_http_and_names_what_went_wrong() {
    let ok = run_stream(mock::Reply::sse(&mock::fixture("turn1_answer.sse"))).unwrap();
    assert_eq!(ok.usage.cache_read_tokens, 2350);
    assert!(matches!(&ok.items[0].1, Item::AssistantText { text } if text.ends_with("paste anywhere.")));
    let err = run_stream(mock::Reply::sse(&mock::fixture("overloaded.sse"))).unwrap_err();
    assert_eq!(err.code, "provider_unavailable");
    let err = run_stream(mock::Reply { status: 401, body: json!({"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}).to_string() }).unwrap_err();
    assert_eq!(err.code, "provider_auth");
    assert!(err.message.contains("ANTHROPIC_API_KEY") && err.message.contains("invalid x-api-key"), "{}", err.message);
}
