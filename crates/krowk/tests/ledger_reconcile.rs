//! A request the client killed still ran, and still billed, on the provider's
//! side. This holds the whole path to that fact: a stand-in provider that
//! finishes a request after its client has given up and meters it into its
//! ledger, then `sessions import` and the listing, run as the binary does.
//!
//! The binary runs with an empty environment and a temporary HOME, so the
//! store and the ledger directory are the test's own.

#![cfg(feature = "sessions")]

use serde_json::Value;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::time::Duration;

#[test]
fn a_request_killed_client_side_is_still_counted_from_the_provider_ledger() {
    let home = scratch();
    let ledger = home.join(".local/share/krowk/ledger/fake.jsonl");
    std::fs::create_dir_all(ledger.parent().unwrap()).unwrap();

    // The provider: it reads the request, keeps generating past the client's
    // patience, then meters the completion — as a provider does whether or
    // not anybody is still listening — and answers into a closed socket.
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = listener.local_addr().unwrap();
    let meter = ledger.clone();
    let provider = std::thread::spawn(move || {
        let (mut conn, _) = listener.accept().unwrap();
        let mut buf = [0u8; 4096];
        let _ = conn.read(&mut buf);
        std::thread::sleep(Duration::from_millis(400));
        let row = r#"{"id":"gen_ghost","provider":"fake","model":"qwen3.5-plus","time":"2026-09-10T11:32:05Z","input_tokens":84,"output_tokens":3422,"reasoning_tokens":3405,"cost_usd":0.0041}"#;
        std::fs::OpenOptions::new().create(true).append(true).open(&meter).unwrap().write_all(format!("{row}\n").as_bytes()).unwrap();
        let body = r#"{"usage":{"prompt_tokens":84,"completion_tokens":3422}}"#;
        let _ = write!(conn, "HTTP/1.1 200 OK\r\ncontent-length: {}\r\n\r\n{body}", body.len());
    });

    // The client gives up after 100 ms and goes away without an answer.
    let mut client = TcpStream::connect(addr).unwrap();
    let req = r#"{"model":"qwen3.5-plus","max_tokens":1200,"messages":[{"role":"user","content":"hi"}]}"#;
    write!(client, "POST /v1/chat/completions HTTP/1.1\r\nhost: {addr}\r\ncontent-length: {}\r\n\r\n{req}", req.len()).unwrap();
    client.set_read_timeout(Some(Duration::from_millis(100))).unwrap();
    let mut answer = Vec::new();
    let err = client.read_to_end(&mut answer).expect_err("the client times out before the provider answers");
    assert!(matches!(err.kind(), std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut), "{err}");
    assert!(answer.is_empty(), "the client saw no usage block");
    drop(client);
    provider.join().unwrap();

    let imported = krowk(&home, &["sessions", "import", "--from", "ledger"]);
    assert_eq!(imported["data"]["ledger"], serde_json::json!({ "observed": 0, "unobserved": 1 }), "{imported}");

    let listed = krowk(&home, &["sessions"]);
    let sessions = listed["data"]["sessions"].as_array().unwrap();
    assert_eq!(sessions.len(), 1, "{listed}");
    assert_eq!((sessions[0]["harness"].as_str(), sessions[0]["turns"].as_i64()), (Some("ledger"), Some(1)));

    let id = sessions[0]["id"].as_str().unwrap();
    let shown = krowk(&home, &["sessions", "show", id]);
    let turn = &shown["data"]["turns"][0];
    assert_eq!(turn["status"], "unobserved", "{shown}");
    assert_eq!((turn["input_tokens"].as_i64(), turn["output_tokens"].as_i64()), (Some(84), Some(17)), "reasoning is split out of output");
    assert_eq!(turn["cost_usd_micros"], 4100, "the provider's stated cost is kept");

    // A second import converges on the same one row.
    let again = krowk(&home, &["sessions", "import", "--from", "ledger"]);
    assert_eq!(again["data"]["providers"][0]["messages_inserted"], 0);
    assert_eq!(again["data"]["ledger"]["unobserved"], 1);
    let _ = std::fs::remove_dir_all(home.parent().unwrap());
}

fn scratch() -> PathBuf {
    let dir = std::env::temp_dir().join(format!("krowk-ledger-it-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    let home = dir.join("home");
    std::fs::create_dir_all(&home).unwrap();
    home.canonicalize().unwrap()
}

fn krowk(home: &Path, args: &[&str]) -> Value {
    let out = Command::new(env!("CARGO_BIN_EXE_krowk"))
        .args(args)
        .arg("--json")
        .env_clear()
        .env("PATH", std::env::var("PATH").unwrap_or_default())
        .env("HOME", home)
        .env("KROWK_NO_UPDATE_CHECK", "1")
        .current_dir(home)
        .output()
        .unwrap();
    let stdout = String::from_utf8_lossy(&out.stdout);
    assert!(out.status.success(), "krowk {args:?}: {stdout}{}", String::from_utf8_lossy(&out.stderr));
    serde_json::from_str(&stdout).unwrap_or_else(|e| panic!("krowk {args:?}: {e}: {stdout}"))
}
