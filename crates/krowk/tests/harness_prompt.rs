//! `krowk -p`, the built binary, against the stand-in Anthropic API: the
//! three output formats, a resumed session reading the cache, the session
//! listed beside an imported Claude one, and a rebuild that re-derives it
//! from the log alone.

#![cfg(feature = "harness")]

#[path = "../../krowk-harness/tests/common/mock.rs"]
mod mock;

use serde_json::Value;
use std::path::PathBuf;
use std::process::{Command, Output, Stdio};

struct Sandbox {
    root: PathBuf,
    url: String,
}

impl Sandbox {
    fn new(name: &str, url: &str) -> Sandbox {
        let root = std::env::temp_dir().join(format!("krowk-harness-cli-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        std::fs::create_dir_all(root.join("home")).unwrap();
        std::fs::create_dir_all(root.join("repo/.git")).unwrap();
        std::fs::write(root.join("repo/README.md"), "# krowk\n\nPermalinks for agent output.\n").unwrap();
        let root = root.canonicalize().unwrap();
        Sandbox { root, url: url.into() }
    }

    fn krowk(&self, args: &[&str]) -> Output {
        self.krowk_with(args, "sk-test")
    }

    fn krowk_with(&self, args: &[&str], key: &str) -> Output {
        Command::new(env!("CARGO_BIN_EXE_krowk"))
            .args(args)
            .env_clear()
            .env("PATH", std::env::var("PATH").unwrap_or_default())
            .env("HOME", self.root.join("home"))
            .env("KROWK_NO_UPDATE_CHECK", "1")
            .env("ANTHROPIC_API_KEY", key)
            .env("ANTHROPIC_BASE_URL", &self.url)
            .current_dir(self.root.join("repo"))
            .stdin(Stdio::null())
            .output()
            .unwrap()
    }

    fn ok(&self, args: &[&str]) -> String {
        let out = self.krowk(args);
        let stdout = String::from_utf8_lossy(&out.stdout).into_owned();
        assert!(out.status.success(), "krowk {args:?}: {stdout}{}", String::from_utf8_lossy(&out.stderr));
        stdout
    }

    fn json(&self, args: &[&str]) -> Value {
        let s = self.ok(args);
        serde_json::from_str(&s).unwrap_or_else(|e| panic!("krowk {args:?}: {e}: {s}"))
    }
}

impl Drop for Sandbox {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.root);
    }
}

fn krowk_sessions(b: &Sandbox) -> Vec<Value> {
    b.json(&["sessions", "--json"])["data"]["sessions"].as_array().unwrap().clone()
}

#[test]
fn r_log_5_krowk_p_answers_streams_resumes_and_lists_beside_imported_sessions() {
    let m = mock::serve(mock::readme_script);
    let b = Sandbox::new("session", &m.url);

    // text: the answer, after a read of the README in the working directory.
    let text = b.ok(&["-p", "read README.md and summarise it in one line", "--model", "claude-sonnet-4-6"]);
    assert_eq!(text, "krowk turns agent output — screenshots, logs, diffs — into permalinks you can paste anywhere.\n");
    {
        let seen = m.seen.lock().unwrap();
        let result = &seen[1].body["messages"][2]["content"][0];
        assert_eq!(result["type"], "tool_result");
        assert!(result["content"].as_str().unwrap().contains("Permalinks for agent output."), "{result}");
    }

    // The session is in krowk.db straight away, harness krowk.
    let listed = krowk_sessions(&b);
    let ours = listed.iter().find(|s| s["harness"] == "krowk").expect("the -p session is listed");
    let session_id = ours["foreign_session_id"].as_str().unwrap().to_string();
    assert_eq!(ours["title"], "read README.md and summarise it in one line");

    // stream-json on a resume, by the krowk.db id: start/delta/end, then the result.
    let stream = b.ok(&["-p", "what language is it written in?", "--resume", ours["id"].as_str().unwrap(), "--output-format", "stream-json"]);
    let lines: Vec<Value> = stream.lines().map(|l| serde_json::from_str(l).unwrap()).collect();
    let types: Vec<&str> = lines.iter().map(|l| l["type"].as_str().unwrap()).collect();
    for want in ["turn.started", "item.started", "item.delta", "item.completed", "response.completed", "turn.completed"] {
        assert!(types.contains(&want), "{want} missing from {types:?}");
    }
    let result = lines.last().unwrap();
    assert_eq!(result["type"], "result");
    assert_eq!(result["sessionId"], session_id.as_str(), "the same session continued");
    assert!(result["usage"]["cacheReadTokens"].as_i64().unwrap() > 0, "{result}");
    for k in ["inputTokens", "outputTokens", "cacheWriteTokens", "reasoningTokens"] {
        assert!(result["usage"][k].is_i64(), "{k} in {result}");
    }
    assert!(result["costUsd"].as_f64().unwrap() > 0.0 && result["durationMs"].is_u64());

    // json: the result event alone.
    let one = b.json(&["-p", "hello", "--output-format", "json", "--model", "anthropic/claude-sonnet-4-6"]);
    assert_eq!((one["type"].as_str(), one["status"].as_str()), (Some("result"), Some("completed")));

    // An imported Claude session lists beside the native ones.
    let project = b.root.join("home/.claude/projects/-repo");
    std::fs::create_dir_all(&project).unwrap();
    let line = serde_json::json!({"type": "user", "sessionId": "c1a0de00-0000-4000-8000-000000000001", "uuid": "u1", "cwd": b.root.join("repo"), "message": {"role": "user", "content": "hello from claude"}});
    std::fs::write(project.join("c1a0de00-0000-4000-8000-000000000001.jsonl"), format!("{line}\n")).unwrap();
    b.ok(&["sessions", "import", "--from", "all", "--json"]);
    let listed = krowk_sessions(&b);
    let harnesses: Vec<&str> = listed.iter().map(|s| s["harness"].as_str().unwrap()).collect();
    assert_eq!(harnesses.iter().filter(|h| **h == "krowk").count(), 2, "{harnesses:?}");
    assert!(harnesses.contains(&"claude"), "{harnesses:?}");

    // R-LOG-2: a rebuild deletes krowk.db and re-derives the native
    // sessions from their JSONL alone.
    let before = b.json(&["sessions", "show", &session_id, "--json"])["data"].clone();
    b.ok(&["sessions", "rebuild", "--yes", "--json"]);
    let after = b.json(&["sessions", "show", &session_id, "--json"])["data"].clone();
    let strip = |v: &Value| {
        let mut v = v.clone();
        v.as_object_mut().unwrap().remove("id");
        v
    };
    assert_eq!(strip(&after), strip(&before));
    assert_eq!(after["turns"].as_array().unwrap().len(), 2);
    assert_eq!(after["harness"], "krowk");
}

#[test]
fn p_flags_are_refused_elsewhere_and_bad_values_are_named() {
    let b = Sandbox::new("flags", "http://127.0.0.1:9");
    let out = b.krowk(&["sessions", "--model", "x"]);
    assert_eq!(out.status.code(), Some(1));
    assert!(String::from_utf8_lossy(&out.stderr).contains("only a flag of `krowk -p`"));
    let out = b.krowk(&["-p", "hi", "--permission-mode", "yolo"]);
    assert!(String::from_utf8_lossy(&out.stderr).contains("bypassPermissions"));
    let out = b.krowk(&["-p", "hi", "--output-format", "xml"]);
    assert!(String::from_utf8_lossy(&out.stderr).contains("stream-json"));
    let out = b.krowk(&["-p"]);
    assert!(String::from_utf8_lossy(&out.stderr).contains("needs a prompt"));
    // No key: refused before a session exists, exit 3, the variable named.
    let out = b.krowk_with(&["-p", "hi"], "");
    assert_eq!(out.status.code(), Some(3));
    assert!(String::from_utf8_lossy(&out.stderr).contains("ANTHROPIC_API_KEY"));
    assert!(!b.root.join("home/.local/share/krowk/sessions").exists(), "a refused prompt leaves no session behind");
    // Nothing listening: named as unreachable, exit 6.
    let out = b.krowk(&["-p", "hi", "--model", "claude-sonnet-4-6"]);
    assert_eq!(out.status.code(), Some(6), "{}", String::from_utf8_lossy(&out.stderr));
}
