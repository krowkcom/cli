//! Runs every case under tests/golden/cases against one krowk binary and
//! compares what it printed and how it exited.
//!
//! The binary is `$KROWK_BIN`, defaulting to the Go build at bin/krowk, so the
//! cases are recorded from the implementation that ships today and then held
//! against the Rust one: `KROWK_BIN=target/release/krowk cargo test -p krowk-golden`.
//!
//! A case is a directory holding `args` (one argument per line), and the
//! expected `stdout`, `stderr` and `exit`. `GOLDEN_UPDATE=1` rewrites the
//! expectations from the binary instead of checking them.
//!
//! Each case runs with an empty environment and a fresh HOME, so nothing on
//! the machine that runs it — a key, a krowk.db, a harness transcript — can
//! leak into the output, and the API URL points at a port nothing listens on
//! so no case reaches the network by accident.

use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};

#[test]
fn golden() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR"));
    let bin = std::env::var_os("KROWK_BIN")
        .map(PathBuf::from)
        .unwrap_or_else(|| root.join("../../bin/krowk"));
    assert!(bin.exists(), "no binary at {} — run `make build` or set KROWK_BIN", bin.display());
    let update = std::env::var_os("GOLDEN_UPDATE").is_some();

    let mut cases: Vec<_> = fs::read_dir(root.join("cases")).unwrap().map(|e| e.unwrap().path()).collect();
    cases.sort();
    assert!(!cases.is_empty(), "no cases under tests/golden/cases");

    let mut failed = Vec::new();
    for case in &cases {
        let name = case.file_name().unwrap().to_string_lossy().into_owned();
        let home = std::env::temp_dir().join(format!("krowk-golden-{}-{name}", std::process::id()));
        let _ = fs::remove_dir_all(&home);
        fs::create_dir_all(&home).unwrap();

        let args = fs::read_to_string(case.join("args")).unwrap_or_default();
        let out = Command::new(&bin)
            .args(args.lines().filter(|l| !l.is_empty()))
            .env_clear()
            .env("PATH", "/usr/bin:/bin")
            .env("HOME", &home)
            .env("KROWK_API_URL", "http://127.0.0.1:9")
            .env("KROWK_NO_UPDATE_CHECK", "1")
            .stdin(Stdio::null())
            .output()
            .unwrap();
        let _ = fs::remove_dir_all(&home);

        let home = home.to_string_lossy();
        let got = [
            ("stdout", normalize(&out.stdout, &home)),
            ("stderr", normalize(&out.stderr, &home)),
            ("exit", format!("{}\n", out.status.code().unwrap_or(-1))),
        ];
        for (file, got) in got {
            let path = case.join(file);
            if update {
                fs::write(&path, &got).unwrap();
            } else if fs::read_to_string(&path).unwrap_or_default() != got {
                failed.push(format!("{name}/{file}:\n{got}"));
            }
        }
    }
    assert!(failed.is_empty(), "{} mismatch(es) against {}:\n\n{}", failed.len(), bin.display(), failed.join("\n"));
}

// The version is whatever `git describe` said when the binary was built, and
// the home is a fresh temp dir per run; neither is behavior.
fn normalize(bytes: &[u8], home: &str) -> String {
    let text = String::from_utf8_lossy(bytes).replace(home, "<home>");
    text.lines()
        .map(|line| match line.find("\"version\": \"") {
            Some(i) => format!("{}\"version\": \"<version>\"{}", &line[..i], if line.ends_with(',') { "," } else { "" }),
            None => line.to_string(),
        })
        .map(|line| line + "\n")
        .collect()
}
