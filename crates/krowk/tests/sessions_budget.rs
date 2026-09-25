//! `sessions budget` against a runaway-reasoning run, as the binary answers
//! it: a Claude turn sent with `max_tokens: 1200` whose model thought for
//! 3,422 tokens. A guard reading the cap would pass it; the guard reads the
//! usage the provider metered, and trips.

#![cfg(feature = "sessions")]

use serde_json::Value;
use std::path::{Path, PathBuf};
use std::process::Command;

#[test]
fn a_runaway_reasoning_run_trips_the_guard_on_metered_tokens() {
    let home = scratch();
    let dir = home.join(".claude/projects/-work");
    std::fs::create_dir_all(&dir).unwrap();
    std::fs::create_dir_all(home.join("work")).unwrap();
    let cwd = home.join("work").display().to_string();
    let sid = "66666666-6666-4666-8666-666666666666";
    let user = serde_json::json!({ "type": "user", "uuid": "u1", "sessionId": sid, "cwd": cwd, "message": { "role": "user", "content": "one word: what is zen?" } });
    let asst = serde_json::json!({
        "type": "assistant", "uuid": "a1", "sessionId": sid, "cwd": cwd,
        "message": { "id": "msg_1", "role": "assistant", "model": "claude-sonnet-4-6", "content": [{ "type": "text", "text": "Sitting." }],
            "usage": { "input_tokens": 84, "output_tokens": 3422, "output_tokens_details": { "thinking_tokens": 3405 } } }
    });
    std::fs::write(dir.join(format!("{sid}.jsonl")), format!("{user}\n{asst}\n")).unwrap();
    let (ok, out) = krowk(&home, &["sessions", "import", "--from", "claude", "--json"]);
    assert!(ok, "{out}");

    // The cap it was sent with (1200) plus its input is inside 2,000; what
    // was metered is not.
    let (code, out) = run(&home, &["sessions", "budget", sid, "--max-tokens", "2000", "--json"]);
    assert_eq!(code, 4, "{out}");
    let err: Value = serde_json::from_str(&out).unwrap();
    assert_eq!(err["error"]["error"], "budget_exceeded", "{err}");
    let details = &err["error"]["details"];
    assert_eq!((details["metered"]["total"].as_i64(), details["within"].as_bool()), (Some(3506), Some(false)), "{err}");
    assert_eq!(details["tripped"][0], serde_json::json!({ "limit": "max_tokens", "metered": 3506, "max": 2000 }));
    assert!(err["error"]["fix"].as_str().unwrap().contains("stop the run"), "{err}");
    assert_eq!((details["metered"]["output"].as_i64(), details["metered"]["reasoning"].as_i64()), (Some(17), Some(3405)), "the thinking split is kept");

    // Priced from the embedded snapshot: 84 × $3/M + 3,422 × $15/M.
    let (code, out) = run(&home, &["sessions", "budget", sid, "--max-usd", "0.06", "--max-tokens", "4000", "--json"]);
    assert_eq!(code, 0, "{out}");
    let ok: Value = serde_json::from_str(&out).unwrap();
    assert!((ok["data"]["cost_usd"].as_f64().unwrap() - (84.0 * 3.0 + 3422.0 * 15.0) / 1e6).abs() < 1e-12, "{ok}");
    let (code, _) = run(&home, &["sessions", "budget", sid, "--max-usd", "0.05"]);
    assert_eq!(code, 4, "$0.051582 is over $0.05");
    let (code, out) = run(&home, &["sessions", "budget", sid]);
    assert_eq!(code, 1, "no limit is a usage error: {out}");
    let _ = std::fs::remove_dir_all(home.parent().unwrap());
}

fn scratch() -> PathBuf {
    let dir = std::env::temp_dir().join(format!("krowk-budget-it-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    let home = dir.join("home");
    std::fs::create_dir_all(&home).unwrap();
    home.canonicalize().unwrap()
}

fn run(home: &Path, args: &[&str]) -> (i32, String) {
    let out = Command::new(env!("CARGO_BIN_EXE_krowk"))
        .args(args)
        .env_clear()
        .env("PATH", std::env::var("PATH").unwrap_or_default())
        .env("HOME", home)
        .env("KROWK_NO_UPDATE_CHECK", "1")
        .current_dir(home)
        .output()
        .unwrap();
    (out.status.code().unwrap_or(-1), format!("{}{}", String::from_utf8_lossy(&out.stdout), String::from_utf8_lossy(&out.stderr)))
}

fn krowk(home: &Path, args: &[&str]) -> (bool, String) {
    let (code, out) = run(home, args);
    (code == 0, out)
}
