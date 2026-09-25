//! A whole headless session against a stand-in Anthropic API: a prompt that
//! reads a file, a resumed follow-up, and what both leave behind — the log,
//! its context record, and the rows krowk.db derives from it.

#[path = "common/mock.rs"]
mod mock;

use krowk_harness::headless::{self, OutputFormat};
use krowk_harness::host::HostConfig;
use krowk_harness::instances::{InstancesConfig, Registry};
use krowk_harness::log;
use krowk_harness::protocol::{ContextRecord, Item, LiveEvent, LogBody, LogEvent, PermissionMode, RunResult, StreamLine, TurnStatus, Usage};
use krowk_import::Source;
use std::path::{Path, PathBuf};
use std::sync::Arc;

const SIGNATURE: &str = "EqQBCkYIBxgCKkD3xG+4n0t/7i2rQzYkWmV9pL+/aX0c3R8s1QvT2nB4k==/+Zq9Hc8JtUe0yW5rN6mF7gD2lXoPiAv+EQ==";

struct Home {
    root: PathBuf,
    url: String,
}

impl Home {
    fn new(name: &str, url: &str) -> Home {
        let root = std::env::temp_dir().join(format!("krowk-harness-e2e-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        std::fs::create_dir_all(root.join("home")).unwrap();
        std::fs::create_dir_all(root.join("repo/.git")).unwrap();
        std::fs::write(root.join("repo/README.md"), "# krowk\n\nPermalinks for agent output.\n").unwrap();
        Home { root, url: url.into() }
    }

    fn env(&self) -> impl Fn(&str) -> String + '_ {
        move |k| match k {
            "HOME" => self.root.join("home").display().to_string(),
            "ANTHROPIC_API_KEY" => "sk-test".into(),
            "ANTHROPIC_BASE_URL" => self.url.clone(),
            _ => String::new(),
        }
    }

    fn repo(&self) -> PathBuf {
        self.root.join("repo")
    }

    fn config(&self) -> HostConfig {
        HostConfig {
            sessions_dir: log::sessions_dir(&self.env()).unwrap(),
            cwd: self.repo(),
            registry: Registry::resolve(&InstancesConfig::default(), &self.env()),
            krowk_version: "test".into(),
            // Sonnet's list prices, per million tokens.
            pricer: Arc::new(|_, model, u: &Usage| {
                (model == "claude-sonnet-4-6").then(|| {
                    (u.input_tokens as f64 * 3.0 + u.output_tokens as f64 * 15.0 + u.cache_read_tokens as f64 * 0.3 + u.cache_write_tokens as f64 * 3.75) / 1e6
                })
            }),
        }
    }

    fn run(&self, prompt: &str, resume: Option<&str>) -> (Vec<StreamLine>, RunResult) {
        let mut out = Vec::new();
        let reg = Registry::resolve(&InstancesConfig::default(), &self.env());
        let opts = headless::Options {
            prompt: prompt.into(),
            resume: resume.map(String::from),
            model: Some(reg.parse_model("claude-sonnet-4-6").unwrap()),
            permission_mode: PermissionMode::Default,
            format: OutputFormat::StreamJson,
        };
        let outcome = headless::run(self.config(), opts, &mut out);
        assert!(outcome.error.is_none(), "{:?}", outcome.error);
        let lines: Vec<StreamLine> = String::from_utf8(out).unwrap().lines().map(|l| serde_json::from_str(l).unwrap_or_else(|e| panic!("{e}: {l}"))).collect();
        (lines, outcome.result.unwrap())
    }
}

impl Drop for Home {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.root);
    }
}

fn schema(name: &str) -> jsonschema::Validator {
    let raw = std::fs::read_to_string(Path::new(env!("CARGO_MANIFEST_DIR")).join("schema").join(name)).unwrap();
    jsonschema::validator_for(&serde_json::from_str(&raw).unwrap()).unwrap()
}

#[test]
fn a_prompt_reads_a_file_streams_its_items_and_a_resume_continues_on_the_cache() {
    let m = mock::serve(mock::readme_script);
    let home = Home::new("session", &m.url);

    // The first prompt: a read tool call, then the answer.
    let (lines, first) = home.run("read README.md and summarise it in one line", None);
    assert_eq!(first.status, TurnStatus::Completed);
    assert_eq!(first.result, "krowk turns agent output — screenshots, logs, diffs — into permalinks you can paste anywhere.");
    assert_eq!(first.num_model_calls, 2);
    assert_eq!(first.usage, Usage { input_tokens: 21, output_tokens: 107, cache_read_tokens: 2350, cache_write_tokens: 3760, reasoning_tokens: 0 });
    assert!(first.cost_usd.is_some_and(|c| c > 0.0));
    let started = lines.iter().filter(|l| matches!(l, StreamLine::Live(LiveEvent::ItemStarted { .. }))).count();
    let deltas = lines.iter().filter(|l| matches!(l, StreamLine::Live(LiveEvent::ItemDelta { .. }))).count();
    assert!(started >= 5 && deltas >= 7, "start and delta frames stream: {started} {deltas}");
    assert!(matches!(lines.last(), Some(StreamLine::Live(LiveEvent::Result(r))) if *r == first), "the stream ends with the result");
    let result_read = lines.iter().any(|l| {
        matches!(l, StreamLine::Log(LogEvent { body: LogBody::ItemCompleted { item: Item::ToolResult { output, is_error: false, .. }, .. }, .. }) if output.contains("Permalinks for agent output."))
    });
    assert!(result_read, "the read tool ran against the working directory");

    // The second call carried the tool result, and the thinking block with
    // its signature untouched.
    {
        let seen = m.seen.lock().unwrap();
        assert_eq!(seen.len(), 2);
        let msgs = seen[1].body["messages"].as_array().unwrap();
        assert_eq!(msgs[1]["content"][0]["signature"], SIGNATURE);
        assert_eq!(msgs[2]["content"][0]["type"], "tool_result");
        assert!(seen[1].body["system"][0]["cache_control"].is_object());
    }

    // A resumed prompt continues the same session, rebuilt from the log.
    let (_, second) = home.run("what language is it written in?", Some(&first.session_id));
    assert_eq!(second.session_id, first.session_id);
    assert_eq!(second.result, "It is written in Rust.");
    assert!(second.usage.cache_read_tokens > 0, "the resumed call reads the cached prefix");
    {
        let seen = m.seen.lock().unwrap();
        let msgs = seen[2].body["messages"].as_array().unwrap();
        assert_eq!(msgs.len(), 5, "prompt, tool call, tool result, answer, follow-up");
        assert_eq!(msgs[1]["content"][0]["signature"], SIGNATURE, "the signature survives the log byte for byte");
        assert_eq!(seen[2].body["system"], seen[0].body["system"], "the cached prefix is byte-identical across the resume");
        assert_eq!(seen[2].body["tools"], seen[0].body["tools"]);
    }

    // R-LOG-1: every line validates against the generated schema, carries a
    // UUIDv7 id, and hangs from the line before it; only the root has no parent.
    let dir = log::sessions_dir(&home.env()).unwrap().join(&first.session_id);
    let raw = std::fs::read_to_string(dir.join(log::EVENTS_FILE)).unwrap();
    let validator = schema("log-event.schema.json");
    let events: Vec<LogEvent> = raw
        .lines()
        .map(|l| {
            let v: serde_json::Value = serde_json::from_str(l).unwrap();
            assert!(validator.is_valid(&v), "{l}");
            serde_json::from_value(v).unwrap()
        })
        .collect();
    assert!(events.iter().all(|e| log::valid_id(&e.id)));
    assert!(events[0].parent_id.is_none() && events[0].id == first.session_id);
    for w in events.windows(2) {
        assert_eq!(w[1].parent_id.as_deref(), Some(w[0].id.as_str()));
    }
    let turns = events.iter().filter(|e| matches!(e.body, LogBody::TurnCompleted { .. })).count();
    assert_eq!(turns, 2);
    // Every stream line validates against the stream schema too.
    let stream = schema("stream-line.schema.json");
    for l in &lines {
        assert!(stream.is_valid(&serde_json::to_value(l).unwrap()), "{l:?}");
    }

    // R-LOG-4: each turn's exact system prompt and tools, beside the log.
    let ctx: Vec<ContextRecord> = std::fs::read_to_string(dir.join(log::CONTEXT_FILE)).unwrap().lines().map(|l| serde_json::from_str(l).unwrap()).collect();
    assert_eq!(ctx.len(), 2);
    assert_eq!(ctx[0].system, seen_system(&m));
    assert_eq!(ctx[0].tools.iter().map(|t| t.name.as_str()).collect::<Vec<_>>(), ["read", "bash"]);

    // R-LOG-2, R-LOG-5: the log projects into krowk.db, and a rebuild from
    // the JSONL alone gives the same rows.
    let env = home.env();
    let listed = project_all(&env);
    let row = listed.iter().find(|r| r.harness == "krowk").expect("a krowk session in the listing");
    assert_eq!((row.turn_count, row.foreign_session_id.as_str()), (2, first.session_id.as_str()));
    assert_eq!(row.title, "read README.md and summarise it in one line");
    let before = detail(&env, &row.id);
    std::fs::remove_file(krowk_store::db_path(&env).unwrap()).unwrap();
    let rebuilt = project_all(&env);
    let row2 = rebuilt.iter().find(|r| r.harness == "krowk").unwrap();
    assert_eq!(detail(&env, &row2.id), before, "rebuilt from the JSONL alone");
}

fn seen_system(m: &mock::Mock) -> String {
    m.seen.lock().unwrap()[0].body["system"][0]["text"].as_str().unwrap().to_string()
}

fn project_all(env: &dyn Fn(&str) -> String) -> Vec<krowk_store::SessionRow> {
    let conn = krowk_store::open(env).unwrap();
    let w = krowk_store::Writer::new(&conn);
    let src = krowk_harness::project::Krowk;
    for r in src.discover(env).unwrap() {
        let (th, cursor, _) = src.read(env, &r, "").unwrap();
        w.ingest_with_cursor(&th, &r.key(), &cursor).unwrap();
        assert!(src.unchanged(env, &r, &cursor));
    }
    krowk_store::list_sessions(&conn, "", "", 50).unwrap()
}

/// The session's content with the store's own ids and clocks left out.
fn detail(env: &dyn Fn(&str) -> String, id: &str) -> String {
    let conn = krowk_store::open(env).unwrap();
    let d = krowk_store::load_session_detail(&conn, id).unwrap();
    let turns: Vec<String> = d.turns.iter().map(|t| format!("{} {} {} {} {} {}", t.status, t.input, t.output, t.cache_read, t.cache_write, t.model)).collect();
    let msgs: Vec<String> = d
        .messages
        .iter()
        .map(|m| format!("{} {} [{}]", m.role, m.model, m.parts.iter().map(|p| format!("{}:{}:{}", p.kind, p.tool_call_id, p.data)).collect::<Vec<_>>().join(", ")))
        .collect();
    format!("{}\n{}\n{}", d.session.title, turns.join("\n"), msgs.join("\n"))
}
