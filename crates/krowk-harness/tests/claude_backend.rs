//! The Claude Code backend through the host, against a fake `claude` (a
//! script replaying recorded stream-json, `fixtures/claude/fake-claude`), so
//! no test needs a real login: a turn that calls krowk's injected
//! session_info tool, a denied Bash, one process serving the whole session
//! and switching models in place, an interrupt the session survives, a
//! resume from a new host, two instances with their own config
//! directories, and the refusals — an untrusted repository, a missing
//! login, a missing binary.

#![cfg(unix)]

use krowk_harness::host::{Host, HostConfig};
use krowk_harness::instances::{InstanceKind, InstancesConfig, Registry};
use krowk_harness::log;
use krowk_harness::protocol::{
    Billing, Command, ContextRecord, Item, LiveEvent, LogBody, LogEvent, ModelRef, PermissionMode, RunResult, StreamLine, TurnStatus, WireApi,
};
use krowk_harness::trust;
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use tokio::sync::mpsc;

const VENDOR_SESSION: &str = "fa4e0000-0000-4000-8000-000000000001";

/// One test at a time in this binary: a process another test is spawning
/// holds a copy of every open descriptor between its fork and its exec —
/// a session log's lock included — so a turn re-opening its log in that
/// instant would read another krowk's lock. One krowk runs one session's
/// turns; tests running several at once in one process are the exception.
static SERIAL: std::sync::Mutex<()> = std::sync::Mutex::new(());

fn fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/fixtures/claude").join(name)
}

struct Home {
    root: PathBuf,
    _serial: std::sync::MutexGuard<'static, ()>,
}

impl Home {
    fn new(name: &str) -> Home {
        let guard = SERIAL.lock().unwrap_or_else(|e| e.into_inner());
        let root = std::env::temp_dir().join(format!("krowk-claude-backend-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        std::fs::create_dir_all(root.join("home")).unwrap();
        std::fs::create_dir_all(root.join("repo/.git")).unwrap();
        std::fs::create_dir_all(root.join("bin")).unwrap();
        // The fake, installed as `claude`, executable whatever git kept.
        let bin = root.join("bin/claude");
        std::fs::copy(fixture("fake-claude"), &bin).unwrap();
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&bin, std::fs::Permissions::from_mode(0o755)).unwrap();
        Home { root: root.canonicalize().unwrap(), _serial: guard }
    }

    fn log_file(&self) -> PathBuf {
        self.root.join("fake.log")
    }

    fn fake_log(&self) -> String {
        std::fs::read_to_string(self.log_file()).unwrap_or_default()
    }

    /// A signed-in Claude Code account: the fake's marker, never a credential.
    fn signed_in(&self, dir: &str) -> PathBuf {
        let d = self.root.join(dir);
        std::fs::create_dir_all(&d).unwrap();
        std::fs::write(d.join("fake-login"), "").unwrap();
        d
    }

    /// An instance: the fake, a config directory, and the scenario in its
    /// environment — the instance's own env, as a router's base URL would be.
    fn instance(&self, config_dir: &Path, scenario: Option<&str>) -> InstanceKind {
        let mut env = BTreeMap::new();
        env.insert("FAKE_CLAUDE_LOG".to_string(), self.log_file().display().to_string());
        if let Some(s) = scenario {
            env.insert("FAKE_CLAUDE_SCENARIO".to_string(), fixture(s).display().to_string());
        }
        InstanceKind::ClaudeCode { binary: Some(self.root.join("bin/claude").display().to_string()), config_dir: Some(config_dir.display().to_string()), env, args: Vec::new(), effort: None }
    }

    fn env(&self) -> impl Fn(&str) -> String + '_ {
        move |k| match k {
            "HOME" => self.root.join("home").display().to_string(),
            "PATH" => std::env::var("PATH").unwrap_or_default(),
            _ => String::new(),
        }
    }

    fn host(&self, instances: Vec<(&str, InstanceKind)>, gate: trust::Gate) -> Host {
        let cfg = InstancesConfig { instances: instances.into_iter().map(|(n, k)| (n.to_string(), k)).collect(), ..Default::default() };
        Host::new(HostConfig {
            sessions_dir: log::sessions_dir(&self.env()).unwrap(),
            cwd: self.root.join("repo"),
            registry: Registry::resolve(&cfg, &self.env()),
            krowk_version: "test".into(),
            pricer: Arc::new(|_, _, _| None),
            catalog: Arc::new(|_, _| None),
            credentials: self.root.join("home/.config/krowk/providers/credentials.json"),
            trust: gate,
        })
    }

    fn events(&self, session_id: &str) -> Vec<LogEvent> {
        log::read_events(&log::sessions_dir(&self.env()).unwrap().join(session_id).join(log::EVENTS_FILE)).unwrap()
    }
}

impl Drop for Home {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.root);
    }
}

fn rt() -> tokio::runtime::Runtime {
    tokio::runtime::Builder::new_current_thread().enable_all().build().unwrap()
}

fn prompt(session_id: Option<&str>, text: &str, model: &str, mode: PermissionMode) -> Command {
    let (instance, model) = model.split_once('/').unwrap();
    Command::Prompt {
        session_id: session_id.map(String::from),
        text: text.into(),
        model: Some(ModelRef { instance: instance.into(), model: model.into() }),
        permission_mode: mode,
        toolset: None,
        effort: None,
    }
}

/// Runs one command to its end, with every stream line it produced.
async fn run(host: &Host, cmd: Command) -> (Vec<StreamLine>, Result<Option<RunResult>, krowk_harness::engine::EngineError>) {
    let (tx, mut rx) = mpsc::channel(4096);
    let collect = async {
        let mut lines = Vec::new();
        while let Some(l) = rx.recv().await {
            lines.push(l);
        }
        lines
    };
    let (r, lines) = tokio::join!(host.execute(cmd, tx), collect);
    (lines, r)
}

fn completed(lines: &[StreamLine]) -> Vec<Item> {
    lines.iter().filter_map(|l| if let StreamLine::Log(LogEvent { body: LogBody::ItemCompleted { item, .. }, .. }) = l { Some(item.clone()) } else { None }).collect()
}

fn processes(fake_log: &str) -> usize {
    fake_log.lines().filter(|l| l.starts_with("argv -p ")).count()
}

#[test]
fn r_back_1_a_session_runs_on_one_claude_process_calls_krowks_tool_and_is_denied_bash() {
    let home = Home::new("session");
    let dir = home.signed_in("cfg-work");
    let host = home.host(vec![("claude:work", home.instance(&dir, Some("session_info.jsonl")))], trust::allow_all());
    rt().block_on(async {
        // Turn 1: the model calls mcp__krowk__session_info, which krowk
        // answers over the control protocol.
        let (lines, r) = run(&host, prompt(None, "what session is this?", "claude:work/sonnet", PermissionMode::Default)).await;
        let first = r.unwrap().unwrap();
        assert_eq!(first.status, TurnStatus::Completed, "{:?}", first.error);
        assert_eq!(first.result, "The session_info tool answered.");
        assert_eq!(first.num_model_calls, 2);
        assert_eq!(first.model, ModelRef { instance: "claude:work".into(), model: "sonnet".into() });
        let items = completed(&lines);
        let Item::Reasoning { blob: Some(b), text } = &items[1] else { panic!("{items:?}") };
        assert_eq!((b.provider.as_str(), b.wire_api, text.as_str()), ("anthropic", WireApi::ClaudeCode, "The person wants the session id."));
        assert!(matches!(&items[2], Item::ToolCall { name, .. } if name == "mcp__krowk__session_info"));
        let Item::ToolResult { output, is_error: false, .. } = &items[3] else { panic!("{items:?}") };
        assert!(output.contains(&format!("krowk session: {}", first.session_id)) && output.contains("instance: claude:work"), "the tool ran inside krowk, for this session: {output}");
        assert!(lines.iter().any(|l| matches!(l, StreamLine::Live(LiveEvent::ItemDelta { .. }))), "deltas stream live");

        // Turn 2, same session: Bash is asked of krowk, and refused under
        // the default mode.
        let (_, r) = run(&host, prompt(Some(&first.session_id), "clean the build", "claude:work/sonnet", PermissionMode::Default)).await;
        let second = r.unwrap().unwrap();
        assert_eq!((second.status, second.result.as_str()), (TurnStatus::Completed, "I was not allowed to run that."));
        let events = home.events(&first.session_id);
        let denied = events.iter().any(|e| matches!(&e.body, LogBody::ItemCompleted { item: Item::ToolResult { is_error: true, output, .. }, .. } if output.contains("bypassPermissions")));
        assert!(denied, "the refusal is what Claude Code was told, and what the log has");

        // Turn 3 on another model of the same instance: switched in place.
        let (_, r) = run(&host, prompt(Some(&first.session_id), "and now?", "claude:work/haiku", PermissionMode::Default)).await;
        assert_eq!(r.unwrap().unwrap().result, "ok");
        host.shutdown().await;
    });

    let fake = home.fake_log();
    assert_eq!(processes(&fake), 1, "one process served the three turns:\n{fake}");
    assert!(fake.contains("set_model haiku"), "{fake}");
    assert!(fake.contains(&format!("config {}", dir.display())), "the instance's CLAUDE_CONFIG_DIR");
    let argv = fake.lines().find(|l| l.starts_with("argv -p ")).unwrap();
    for want in ["--input-format stream-json", "--output-format stream-json", "--verbose", "--include-partial-messages", "--permission-prompt-tool stdio", r#""type":"sdk""#, "--strict-mcp-config", "--model sonnet"] {
        assert!(argv.contains(want), "{want} missing from {argv}");
    }
    assert!(fake.contains(r#""behavior":"allow""#), "krowk's own tool is allowed");
    assert!(fake.contains(r#""name":"session_info""#), "tools/list named it");
    assert!(fake.lines().last().unwrap() == "eof", "shutdown closed stdin and the process exited");

    // R-BACK-5: the log has the Claude session behind it, once, with the
    // transcript Claude Code wrote and what it is billed to (R-INST-3).
    let sid = home.events_first_session();
    let events = home.events(&sid);
    let backend: Vec<&LogBody> = events.iter().map(|e| &e.body).filter(|b| matches!(b, LogBody::BackendSession { .. })).collect();
    assert_eq!(backend.len(), 1, "logged when new, not again: {backend:?}");
    let LogBody::BackendSession { backend, vendor_session_id, transcript_path, billing, .. } = backend[0] else { unreachable!() };
    assert_eq!((backend.as_str(), vendor_session_id.as_str(), *billing), ("claude-code", VENDOR_SESSION, Some(Billing::Subscription)));
    let transcript = PathBuf::from(transcript_path.as_ref().unwrap());
    assert!(transcript.starts_with(&dir) && transcript.is_file(), "{transcript:?}");
    let turns: Vec<&LogBody> = events.iter().map(|e| &e.body).filter(|b| matches!(b, LogBody::TurnStarted { .. })).collect();
    assert!(turns.iter().all(|t| matches!(t, LogBody::TurnStarted { wire_api: WireApi::ClaudeCode, provider, .. } if provider == "anthropic")));

    // Every line validates against the generated schema.
    let raw = std::fs::read_to_string(log::sessions_dir(&home.env()).unwrap().join(&sid).join(log::EVENTS_FILE)).unwrap();
    let schema: serde_json::Value = serde_json::from_str(&std::fs::read_to_string(Path::new(env!("CARGO_MANIFEST_DIR")).join("schema/log-event.schema.json")).unwrap()).unwrap();
    let validator = jsonschema::validator_for(&schema).unwrap();
    for l in raw.lines() {
        assert!(validator.is_valid(&serde_json::from_str(l).unwrap()), "{l}");
    }
    // The context record: Claude Code's prompt is its own; the tools it
    // announced include krowk's, with their schema.
    let ctx: Vec<ContextRecord> = std::fs::read_to_string(log::sessions_dir(&home.env()).unwrap().join(&sid).join(log::CONTEXT_FILE)).unwrap().lines().map(|l| serde_json::from_str(l).unwrap()).collect();
    assert_eq!(ctx[0].toolset, "claude-code");
    let info = ctx[0].tools.iter().find(|t| t.name == "mcp__krowk__session_info").unwrap();
    assert_eq!(info.input_schema["type"], "object");
}

impl Home {
    fn events_first_session(&self) -> String {
        log::list(&log::sessions_dir(&self.env()).unwrap()).unwrap()[0].0.clone()
    }
}

#[test]
fn r_back_1_an_interrupt_stops_the_turn_and_the_session_continues() {
    let home = Home::new("interrupt");
    let dir = home.signed_in("cfg");
    let host = home.host(vec![("claude", home.instance(&dir, Some("interrupt.jsonl")))], trust::allow_all());
    rt().block_on(async {
        let (tx, mut rx) = mpsc::channel(4096);
        let exec = host.execute(prompt(None, "count to a thousand", "claude/haiku", PermissionMode::Default), tx);
        tokio::pin!(exec);
        let mut session: Option<String> = None;
        let mut interrupted = false;
        let result = loop {
            tokio::select! {
                biased;
                Some(line) = rx.recv() => {
                    if let StreamLine::Log(ev) = &line {
                        session.get_or_insert(ev.session_id.clone());
                    }
                    if matches!(line, StreamLine::Live(LiveEvent::ItemDelta { .. })) && !interrupted {
                        interrupted = true;
                        let (itx, _irx) = mpsc::channel(1);
                        host.execute(Command::Interrupt { session_id: session.clone().unwrap() }, itx).await.unwrap();
                    }
                }
                r = &mut exec => break r.unwrap().unwrap(),
            }
        };
        assert_eq!(result.status, TurnStatus::Interrupted);
        assert_eq!(result.result, "1\n2\n3\n", "what streamed before the interrupt is kept");
        // The session continues, on the same process.
        let (_, r) = run(&host, prompt(Some(&result.session_id), "go on", "claude/haiku", PermissionMode::Default)).await;
        let next = r.unwrap().unwrap();
        assert_eq!((next.status, next.result.as_str()), (TurnStatus::Completed, "Carrying on from 3."));
        host.shutdown().await;
    });
    let fake = home.fake_log();
    assert!(fake.contains("\ninterrupt\n"), "the control protocol's interrupt: {fake}");
    assert_eq!(processes(&fake), 1, "the process lived through it");
}

#[test]
fn r_back_5_a_new_host_resumes_the_claude_session_the_log_holds() {
    let home = Home::new("resume");
    let dir = home.signed_in("cfg");
    let inst = || vec![("claude:work", home.instance(&dir, None))];
    let sid = rt().block_on(async {
        let host = home.host(inst(), trust::allow_all());
        let (_, r) = run(&host, prompt(None, "hello", "claude:work/sonnet", PermissionMode::Default)).await;
        host.shutdown().await;
        r.unwrap().unwrap().session_id
    });
    // A second krowk: a new host, a new process, on --resume.
    rt().block_on(async {
        let host = home.host(inst(), trust::allow_all());
        let (_, r) = run(&host, prompt(Some(&sid), "again", "claude:work/sonnet", PermissionMode::Default)).await;
        assert_eq!(r.unwrap().unwrap().status, TurnStatus::Completed);
        host.shutdown().await;
    });
    let fake = home.fake_log();
    assert_eq!(processes(&fake), 2);
    assert!(fake.contains(&format!("resume {VENDOR_SESSION}")), "{fake}");
    let backend = home.events(&sid).iter().filter(|e| matches!(e.body, LogBody::BackendSession { .. })).count();
    assert_eq!(backend, 1, "the same Claude session is not logged twice");
}

#[test]
fn r_inst_1_two_claude_instances_run_with_their_own_config_directories() {
    let home = Home::new("instances");
    let (work, personal) = (home.signed_in("cfg-work"), home.signed_in("cfg-personal"));
    let host = home.host(vec![("claude:work", home.instance(&work, None)), ("claude:personal", home.instance(&personal, None))], trust::allow_all());
    rt().block_on(async {
        for model in ["claude:work/sonnet", "claude:personal/sonnet"] {
            let (_, r) = run(&host, prompt(None, "hello", model, PermissionMode::Default)).await;
            let r = r.unwrap().unwrap();
            assert_eq!((r.status, r.model.instance.as_str()), (TurnStatus::Completed, model.split_once('/').unwrap().0));
        }
        host.shutdown().await;
    });
    let fake = home.fake_log();
    assert!(fake.contains(&format!("config {}", work.display())) && fake.contains(&format!("config {}", personal.display())), "{fake}");
    assert_eq!(processes(&fake), 2, "one process per session");
    assert!(work.join("projects").is_dir() && personal.join("projects").is_dir(), "each account keeps its own transcripts");
}

#[test]
fn r_back_6_an_untrusted_repository_is_refused_before_claude_is_spawned() {
    let home = Home::new("untrusted");
    let dir = home.signed_in("cfg");
    let asked = Arc::new(std::sync::Mutex::new(Vec::new()));
    let seen = asked.clone();
    let gate: trust::Gate = Arc::new(move |root: &Path| {
        seen.lock().unwrap().push(root.to_path_buf());
        Err(trust::untrusted(root, "Pass --trust."))
    });
    let host = home.host(vec![("claude", home.instance(&dir, None))], gate);
    let err = rt().block_on(async { run(&host, prompt(None, "hello", "claude/sonnet", PermissionMode::Default)).await.1 }).unwrap_err();
    assert_eq!(err.code, "untrusted_directory");
    assert_eq!(*asked.lock().unwrap(), [home.root.join("repo")], "asked with the repository's root");
    assert_eq!(processes(&home.fake_log()), 0, "claude was never started");
    assert!(log::list(&log::sessions_dir(&home.env()).unwrap()).unwrap_or_default().is_empty(), "a refusal leaves no session behind");
}

#[test]
fn a_missing_login_or_binary_is_named_with_its_fix() {
    let home = Home::new("refusals");
    let dir = home.root.join("cfg-nobody");
    std::fs::create_dir_all(&dir).unwrap();
    let host = home.host(vec![("claude:personal", home.instance(&dir, None))], trust::allow_all());
    let r = rt().block_on(async { run(&host, prompt(None, "hello", "claude:personal/sonnet", PermissionMode::Default)).await.1 }).unwrap().unwrap();
    assert_eq!(r.status, TurnStatus::Failed);
    let e = r.error.unwrap();
    assert_eq!(e.code, "not_authenticated");
    assert!(e.message.contains("krowk providers add claude --name personal") && e.message.contains("Claude's own login"), "{}", e.message);

    let missing = InstanceKind::ClaudeCode { binary: Some(home.root.join("bin/nope").display().to_string()), config_dir: None, env: BTreeMap::new(), args: Vec::new(), effort: None };
    let host = home.host(vec![("claude:gone", missing)], trust::allow_all());
    let err = rt().block_on(async { run(&host, prompt(None, "hello", "claude:gone/sonnet", PermissionMode::Default)).await.1 }).unwrap_err();
    assert_eq!(err.code, "backend_not_found");
    assert!(err.message.contains("--binary"), "{}", err.message);
}

#[test]
fn r_inst_3_a_session_is_bound_to_its_instance_and_says_whether_it_bills_a_subscription_or_a_key() {
    let home = Home::new("billing");
    let dir = home.signed_in("cfg");
    let mut keyed = home.instance(&dir, None);
    if let InstanceKind::ClaudeCode { env, .. } = &mut keyed {
        env.insert("ANTHROPIC_API_KEY".into(), "sk-test-not-a-real-key".into());
    }
    let host = home.host(vec![("claude:sub", home.instance(&dir, None)), ("claude:keyed", keyed)], trust::allow_all());
    let billed = |model: &str| {
        let r = rt().block_on(async { run(&host, prompt(None, "hello", model, PermissionMode::Default)).await.1 }).unwrap().unwrap();
        let events = home.events(&r.session_id);
        let instance = events.iter().find_map(|e| if let LogBody::TurnStarted { model, .. } = &e.body { Some(model.instance.clone()) } else { None });
        let billing = events.iter().find_map(|e| if let LogBody::BackendSession { billing, .. } = &e.body { *billing } else { None });
        (instance.unwrap(), billing)
    };
    assert_eq!(billed("claude:sub/sonnet"), ("claude:sub".to_string(), Some(Billing::Subscription)));
    assert_eq!(billed("claude:keyed/sonnet"), ("claude:keyed".to_string(), Some(Billing::ApiKey)));
    rt().block_on(host.shutdown());
}
