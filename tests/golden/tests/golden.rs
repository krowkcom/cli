//! Black-box cases for any krowk binary: what it printed, how it exited, and
//! what it left on disk.
//!
//! The binary under test is `$KROWK_BIN`, defaulting to the Go build at
//! bin/krowk. Cases are recorded from the implementation that ships today and
//! then held against the Rust one:
//!
//!     make golden          # Go, the oracle
//!     make golden-rust     # the port, against the same expectations
//!     make golden-update   # re-record from Go
//!
//! A case is a directory under cases/ holding a `cmd` script and the
//! `expected` transcript it produces. Optional `home/` and `work/` directories
//! are copied into the case's fresh HOME and working directory first, with
//! `{{HOME}}`, `{{WORK}}` and `{{WORK_SLUG}}` (the work path with `/` spelled
//! `-`, as Cursor names a project) filled in, in file contents and names
//! alike. A path component spelled `dot-x` is copied as `.x`, because the
//! repository ignores `.claude/` at any depth. `cmd` is one directive or
//! command per line:
//!
//!     # a comment
//!     @registry                 start the stand-in registry; KROWK_API_URL points at it
//!     @fixture name             copy fixtures/name/{home,work} in as above, then run its
//!                               setup.sh (if any) in work/ with $FIXTURE naming the directory
//!     @env KEY=VALUE            set for every later command ({home} {work} {registry} expand)
//!     @unenv KEY
//!     @file path content        write work/path; content takes \n \t \xNN escapes
//!     @stdin content            feed the next command's stdin (same escapes)
//!     @cat path                 print a file into the transcript ({home} {work} expand)
//!     @ls path                  print a directory tree into the transcript
//!     @sh command               run /bin/sh -c in work/ for setup; not recorded ({case} expands too)
//!     krowk args...             run the binary; shell-style quoting
//!     krowk-mcp args...         run $KROWK_MCP_BIN, default bin/krowk-mcp
//!     @tty krowk args...        run with stdout and stderr on one pseudo-terminal, as a
//!                               person at a shell would; ESC and CR print as \e and \r
//!
//! Every command runs with an empty environment apart from PATH, HOME, TMPDIR,
//! KROWK_NO_UPDATE_CHECK and KROWK_TEST_NOW_MS (the store's clock, frozen at
//! 2026-01-01), with KROWK_API_URL on a port nothing listens on
//! and every proxy variable pointing there too, so nothing on the machine — a
//! key, a krowk.db, a harness transcript — leaks in, and no case reaches the
//! network (models.dev, GitHub) by accident.
//!
//! What is random or clock-bound is masked before comparing: slugs, tokens,
//! uuids, timestamps, dates, the registry port and the temp paths. Values that
//! repeat keep their identity (`<art_1>` twice is the same artifact), so a
//! case still proves that one command's output feeds the next.
//!
//! `GOLDEN_UPDATE=1` rewrites `expected` instead of checking it, and
//! `GOLDEN_CASE=substr` runs only the matching cases.

use regex::{Captures, Regex};
use std::collections::HashMap;
use std::fs;
use std::io::{BufRead, BufReader, Write};
use std::net::TcpListener;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::LazyLock;

fn workspace_path(rel: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../..").join(rel)
}

fn bin_from(var: &str, default: &str) -> PathBuf {
    let path = std::env::var_os(var).map(PathBuf::from).unwrap_or_else(|| workspace_path(default));
    let path = if path.is_relative() { workspace_path(&path.to_string_lossy()) } else { path };
    assert!(path.exists(), "no binary at {} — run `make golden` or set {var}", path.display());
    path
}

#[test]
fn golden() {
    let krowk = bin_from("KROWK_BIN", "bin/krowk");
    let mcp = bin_from("KROWK_MCP_BIN", "bin/krowk-mcp");
    let registry = bin_from("KROWK_REGISTRY_BIN", "bin/devregistry");
    // The build stamps its version from `git describe`, so it differs by
    // commit and by build; masked wherever it appears.
    let version = Command::new(&krowk).arg("--version").env_clear().output().unwrap().stdout;
    let version = String::from_utf8_lossy(&version).split_whitespace().last().unwrap_or_default().to_string();
    let update = std::env::var_os("GOLDEN_UPDATE").is_some();
    let only = std::env::var("GOLDEN_CASE").unwrap_or_default();

    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("cases");
    let mut cases: Vec<PathBuf> = fs::read_dir(&root).unwrap().map(|e| e.unwrap().path()).filter(|p| p.join("cmd").exists()).collect();
    cases.sort();
    assert!(!cases.is_empty(), "no cases under {}", root.display());

    let mut failed = Vec::new();
    let mut ran = 0;
    for case in cases.iter().filter(|c| c.to_string_lossy().contains(&only)) {
        ran += 1;
        let name = case.file_name().unwrap().to_string_lossy().into_owned();
        let got = run_case(case, ran, &Bins { krowk: &krowk, version: &version, mcp: &mcp, registry: &registry });
        let path = case.join("expected");
        if update {
            fs::write(&path, &got).unwrap();
            continue;
        }
        let want = fs::read_to_string(&path).unwrap_or_default();
        if want != got {
            failed.push(format!("── {name}\n{}", first_difference(&want, &got)));
        }
    }
    assert!(ran > 0, "GOLDEN_CASE={only} matched no case");
    assert!(
        failed.is_empty(),
        "{} of {ran} case(s) differ against {}:\n\n{}",
        failed.len(),
        krowk.display(),
        failed.join("\n\n")
    );
}

fn first_difference(want: &str, got: &str) -> String {
    let (w, g): (Vec<_>, Vec<_>) = (want.lines().collect(), got.lines().collect());
    let at = w.iter().zip(&g).position(|(a, b)| a != b).unwrap_or(w.len().min(g.len()));
    let from = at.saturating_sub(3);
    let show = |lines: &[&str]| lines.iter().skip(from).take(at - from + 6).map(|l| format!("  {l}")).collect::<Vec<_>>().join("\n");
    format!("first difference at line {}\nwant:\n{}\ngot:\n{}", at + 1, show(&w), show(&g))
}

struct Registry(Child);

impl Drop for Registry {
    fn drop(&mut self) {
        let _ = self.0.kill();
        let _ = self.0.wait();
    }
}

fn start_registry(bin: &Path) -> (Registry, String) {
    // ponytail: bind-then-release port pick; a race with another process is
    // possible but has not happened, retry on bind failure if it ever does.
    let port = TcpListener::bind("127.0.0.1:0").unwrap().local_addr().unwrap().port();
    let mut child = Command::new(bin)
        .args(["--addr", &format!("127.0.0.1:{port}")])
        .env_clear()
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::inherit())
        .spawn()
        .unwrap();
    // The stand-in binds before it prints its banner, so one line of stdout
    // means it is listening.
    let mut line = String::new();
    BufReader::new(child.stdout.take().unwrap()).read_line(&mut line).unwrap();
    assert!(line.contains("listening"), "stand-in registry did not start: {line:?}");
    (Registry(child), format!("http://127.0.0.1:{port}"))
}

struct Bins<'a> {
    krowk: &'a Path,
    version: &'a str,
    mcp: &'a Path,
    registry: &'a Path,
}

fn run_case(case: &Path, n: usize, bins: &Bins) -> String {
    // Under /tmp rather than $TMPDIR, and with no dash in the name: Cursor
    // spells a project path with `/` as `-` and decodes it back, so a dash
    // anywhere in the scratch path would make a fixture decode to nowhere.
    let scratch = PathBuf::from("/tmp").join(format!("krowkgolden{}x{n}", std::process::id()));
    let _ = fs::remove_dir_all(&scratch);
    // Canonical, so a path the binary resolves (macOS puts temp under a
    // /var -> /private/var link) still masks.
    fs::create_dir_all(&scratch).unwrap();
    let scratch = scratch.canonicalize().unwrap();
    let (home, work, tmp) = (scratch.join("home"), scratch.join("work"), scratch.join("tmp"));
    for dir in [&home, &work, &tmp] {
        fs::create_dir_all(dir).unwrap();
    }
    let tokens = [
        ("{{HOME}}", home.display().to_string()),
        ("{{WORK}}", work.display().to_string()),
        ("{{WORK_SLUG}}", work.display().to_string().trim_start_matches('/').replace('/', "-")),
    ];
    copy_tree(&case.join("home"), &home, &tokens);
    copy_tree(&case.join("work"), &work, &tokens);

    let mut env: Vec<(String, String)> = vec![
        ("PATH".into(), "/usr/bin:/bin".into()),
        ("HOME".into(), home.display().to_string()),
        ("TMPDIR".into(), tmp.display().to_string()),
        ("KROWK_NO_UPDATE_CHECK".into(), "1".into()),
        ("KROWK_API_URL".into(), "http://127.0.0.1:9/v1".into()),
        ("HTTPS_PROXY".into(), "http://127.0.0.1:9".into()),
        ("HTTP_PROXY".into(), "http://127.0.0.1:9".into()),
        ("NO_PROXY".into(), "127.0.0.1,localhost".into()),
        // The store's clock, frozen so every time column — and every listing
        // ordered by one — is the same on each run and in both builds.
        ("KROWK_TEST_NOW_MS".into(), "1767225600000".into()),
    ];
    let mut registry: Option<(Registry, String)> = None;
    let mut stdin: Option<Vec<u8>> = None;
    let mut out = String::new();

    let script = fs::read_to_string(case.join("cmd")).unwrap();
    for line in script.lines().map(str::trim).filter(|l| !l.is_empty() && !l.starts_with('#')) {
        let url = registry.as_ref().map(|r| r.1.clone()).unwrap_or_default();
        let expand = |s: &str| {
            s.replace("{home}", &home.display().to_string())
                .replace("{work}", &work.display().to_string())
                .replace("{registry}", &url)
                .replace("{case}", &case.display().to_string())
        };
        let (word, rest) = line.split_once(' ').unwrap_or((line, ""));
        match word {
            "@registry" => {
                let started = start_registry(bins.registry);
                set(&mut env, "KROWK_API_URL", &format!("{}/v1", started.1));
                registry = Some(started);
            }
            "@env" => {
                let (k, v) = rest.split_once('=').expect("@env KEY=VALUE");
                set(&mut env, k, &expand(v));
            }
            "@unenv" => env.retain(|(k, _)| k != rest),
            "@file" => {
                let (path, content) = rest.split_once(' ').unwrap_or((rest, ""));
                let path = work.join(path);
                fs::create_dir_all(path.parent().unwrap()).unwrap();
                fs::write(path, unescape(content)).unwrap();
            }
            "@stdin" => stdin = Some(unescape(rest)),
            "@cat" => {
                let path = PathBuf::from(expand(rest));
                let body = fs::read(&path).map(|b| String::from_utf8_lossy(&b).into_owned()).unwrap_or_else(|e| format!("<{e}>"));
                out.push_str(&format!("@cat {rest}\n{}", ensure_newline(&body)));
            }
            "@ls" => {
                let mut entries = Vec::new();
                list_tree(&PathBuf::from(expand(rest)), Path::new(""), &mut entries);
                out.push_str(&format!("@ls {rest}\n{}", entries.iter().map(|e| e.clone() + "\n").collect::<String>()));
            }
            "@fixture" => {
                let dir = case.parent().unwrap().parent().unwrap().join("fixtures").join(rest);
                assert!(dir.is_dir(), "{}: no fixture {rest}", case.display());
                copy_tree(&dir.join("home"), &home, &tokens);
                copy_tree(&dir.join("work"), &work, &tokens);
                if dir.join("setup.sh").exists() {
                    let result = Command::new("/bin/sh")
                        .arg(dir.join("setup.sh"))
                        .current_dir(&work)
                        .env_clear()
                        .envs(env.iter().map(|(k, v)| (k, v)))
                        .env("FIXTURE", &dir)
                        .output()
                        .unwrap();
                    assert!(result.status.success(), "fixture {rest} setup failed:\n{}", String::from_utf8_lossy(&result.stderr));
                }
            }
            "@sh" => {
                let result = Command::new("/bin/sh")
                    .args(["-c", &expand(rest)])
                    .current_dir(&work)
                    .env_clear()
                    .envs(env.iter().map(|(k, v)| (k, v)))
                    .output()
                    .unwrap();
                assert!(result.status.success(), "{}: @sh {rest} failed:\n{}", case.display(), String::from_utf8_lossy(&result.stderr));
            }
            "@tty" => {
                let (word, rest) = rest.split_once(' ').unwrap_or((rest, ""));
                assert_eq!(word, "krowk", "{}: @tty runs krowk only", case.display());
                let args: Vec<String> = shlex::split(rest).expect("unbalanced quotes").iter().map(|a| expand(a)).collect();
                let (screen, code) = run_on_tty(bins.krowk, &args, &work, &env, case, line);
                out.push_str(&format!("$ {line}\ntty:\n{}exit: {code}\n", ensure_newline(&visible(&screen))));
            }
            "krowk" | "krowk-mcp" => {
                let args: Vec<String> = shlex::split(rest).expect("unbalanced quotes").iter().map(|a| expand(a)).collect();
                let mut child = Command::new(if word == "krowk" { bins.krowk } else { bins.mcp })
                    .args(&args)
                    .current_dir(&work)
                    .env_clear()
                    .envs(env.iter().map(|(k, v)| (k, v)))
                    .stdin(if stdin.is_some() { Stdio::piped() } else { Stdio::null() })
                    .stdout(Stdio::piped())
                    .stderr(Stdio::piped())
                    .spawn()
                    .unwrap();
                if let Some(bytes) = stdin.take() {
                    child.stdin.take().unwrap().write_all(&bytes).unwrap();
                }
                // A command that waits on something no case provides — a
                // browser approval, a network that is not there — must fail
                // the case, not hang the suite.
                let pid = child.id();
                let (tx, rx) = std::sync::mpsc::channel();
                std::thread::spawn(move || tx.send(child.wait_with_output()));
                let result = match rx.recv_timeout(std::time::Duration::from_secs(20)) {
                    Ok(result) => result.unwrap(),
                    Err(_) => {
                        let _ = Command::new("kill").args(["-9", &pid.to_string()]).status();
                        panic!("{}: `{line}` still running after 20s", case.display());
                    }
                };
                out.push_str(&format!("$ {line}\n"));
                if !result.stdout.is_empty() {
                    out.push_str(&format!("stdout:\n{}", ensure_newline(&String::from_utf8_lossy(&result.stdout))));
                }
                if !result.stderr.is_empty() {
                    out.push_str(&format!("stderr:\n{}", ensure_newline(&String::from_utf8_lossy(&result.stderr))));
                }
                out.push_str(&format!("exit: {}\n", result.status.code().map_or("signal".into(), |c| c.to_string())));
            }
            other => panic!("{}: unknown directive {other:?}", case.display()),
        }
        out.push('\n');
    }

    let url = registry.as_ref().map(|r| r.1.clone());
    drop(registry);
    let _ = fs::remove_dir_all(&scratch);
    normalize(&out, &scratch, url.as_deref(), bins)
}

/// Runs the binary with stdout and stderr on one pseudo-terminal and returns
/// everything drawn on it. A pty rather than a pipe is the only way to see
/// what a person sees: colour, the human format by default, the spinner.
#[cfg(unix)]
fn run_on_tty(bin: &Path, args: &[String], work: &Path, env: &[(String, String)], case: &Path, line: &str) -> (String, String) {
    use std::os::fd::FromRawFd;
    let (mut master, mut slave) = (0, 0);
    let mut size = libc::winsize { ws_row: 40, ws_col: 120, ws_xpixel: 0, ws_ypixel: 0 };
    let ok = unsafe { libc::openpty(&mut master, &mut slave, std::ptr::null_mut(), std::ptr::null_mut(), &mut size) };
    assert_eq!(ok, 0, "openpty failed");
    let (master, slave) = unsafe { (fs::File::from_raw_fd(master), fs::File::from_raw_fd(slave)) };
    let mut child = Command::new(bin)
        .args(args)
        .current_dir(work)
        .env_clear()
        .envs(env.iter().map(|(k, v)| (k, v)))
        .stdin(Stdio::null())
        .stdout(slave.try_clone().unwrap())
        .stderr(slave)
        .spawn()
        .unwrap();
    // The child holds the only slave handles now, so the master reads to
    // EOF (EIO on Linux) exactly when the child exits.
    let reader = std::thread::spawn(move || {
        let (mut master, mut screen) = (master, Vec::new());
        let _ = std::io::Read::read_to_end(&mut master, &mut screen);
        screen
    });
    let (pid, deadline) = (child.id(), std::time::Instant::now() + std::time::Duration::from_secs(20));
    let status = loop {
        if let Some(status) = child.try_wait().unwrap() {
            break status;
        }
        if std::time::Instant::now() > deadline {
            let _ = Command::new("kill").args(["-9", &pid.to_string()]).status();
            panic!("{}: `{line}` still running after 20s", case.display());
        }
        std::thread::sleep(std::time::Duration::from_millis(10));
    };
    let screen = String::from_utf8_lossy(&reader.join().unwrap()).into_owned();
    (screen, status.code().map_or("signal".into(), |c| c.to_string()))
}

// A terminal turns \n into \r\n on the way out, which says nothing about the
// binary; what does is every other control character, spelled out.
fn visible(screen: &str) -> String {
    screen.replace("\r\n", "\n").replace('\x1b', "\\e").replace('\r', "\\r")
}

fn set(env: &mut Vec<(String, String)>, key: &str, value: &str) {
    env.retain(|(k, _)| k != key);
    env.push((key.into(), value.into()));
}

fn ensure_newline(s: &str) -> String {
    if s.ends_with('\n') { s.to_string() } else { format!("{s}\n[no newline at end]\n") }
}

fn unescape(s: &str) -> Vec<u8> {
    let (mut out, bytes) = (Vec::new(), s.as_bytes());
    let mut i = 0;
    while i < bytes.len() {
        match (bytes[i], bytes.get(i + 1)) {
            (b'\\', Some(b'n')) => { out.push(b'\n'); i += 2 }
            (b'\\', Some(b't')) => { out.push(b'\t'); i += 2 }
            (b'\\', Some(b'\\')) => { out.push(b'\\'); i += 2 }
            (b'\\', Some(b'x')) => {
                out.push(u8::from_str_radix(&s[i + 2..i + 4], 16).expect("\\xNN"));
                i += 4
            }
            (b, _) => { out.push(b); i += 1 }
        }
    }
    out
}

fn fill(s: &str, tokens: &[(&str, String)]) -> String {
    tokens.iter().fold(s.to_string(), |s, (k, v)| s.replace(k, v))
}

fn copy_tree(from: &Path, to: &Path, tokens: &[(&str, String)]) {
    let Ok(entries) = fs::read_dir(from) else { return };
    for entry in entries.map(Result::unwrap) {
        let name = entry.file_name().to_string_lossy().into_owned();
        let name = name.strip_prefix("dot-").map(|n| format!(".{n}")).unwrap_or(name);
        let dest = to.join(fill(&name, tokens));
        if entry.file_type().unwrap().is_dir() {
            fs::create_dir_all(&dest).unwrap();
            copy_tree(&entry.path(), &dest, tokens);
        } else {
            let bytes = fs::read(entry.path()).unwrap();
            let bytes = match String::from_utf8(bytes) {
                Ok(text) => fill(&text, tokens).into_bytes(),
                Err(e) => e.into_bytes(),
            };
            fs::write(dest, bytes).unwrap();
        }
    }
}

fn list_tree(dir: &Path, rel: &Path, out: &mut Vec<String>) {
    let Ok(entries) = fs::read_dir(dir) else {
        out.push(format!("<missing {}>", rel.display()));
        return;
    };
    let mut entries: Vec<_> = entries.map(Result::unwrap).collect();
    entries.sort_by_key(|e| e.file_name());
    for entry in entries {
        let rel = rel.join(entry.file_name());
        let meta = entry.metadata().unwrap();
        if meta.is_dir() {
            out.push(format!("{}/", rel.display()));
            list_tree(&entry.path(), &rel, out);
        } else {
            #[cfg(unix)]
            let mode = format!(" {:o}", std::os::unix::fs::PermissionsExt::mode(&meta.permissions()) & 0o777);
            #[cfg(not(unix))]
            let mode = String::new();
            out.push(format!("{}{mode}", rel.display()));
        }
    }
}

// Masks, most specific first. Numbered kinds keep identity within a case.
static NUMBERED: LazyLock<Vec<(Regex, &str)>> = LazyLock::new(|| {
    [
        (r"krowk_claim_[0-9a-f]{64}", "claim"),
        (r"krowk_sk_[a-z0-9]{32}", "sk"),
        (r"\b(art|run|aut|key|ws)_[a-z0-9]{8,}\b", ""),
        (r"\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b", "uuid"),
    ]
    .into_iter()
    .map(|(re, kind)| (Regex::new(re).unwrap(), kind))
    .collect()
});

static PLAIN: LazyLock<Vec<(Regex, &str)>> = LazyLock::new(|| {
    [
        (r#""version": ?"[^"]*""#, r#""version": "<version>""#),
        (r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})", "<time>"),
        (r"\b(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) \d{1,2}(, \d{4})?\b", "<date>"),
        (r"\b\d+ ?(s|m|h|d|w|mo|y|seconds?|minutes?|hours?|days?|weeks?|months?|years?) ago\b", "<ago>"),
        // A private artifact's byte key is region then a bare 24-character
        // secret, with no prefix for the numbered masks to key on.
        (r"/weur/[a-z0-9]{24}/", "/weur/<secret>/"),
        (r#""duration_ms": \d+"#, r#""duration_ms": <n>"#),
        // Which language and toolchain built the binary is the one line of
        // doctor output the port is expected to change.
        (r#""runtime": "[^"]*""#, r#""runtime": "<runtime>""#),
        (r"(?m)^runtime +\S.*$", "runtime         <runtime>"),
        // A transport failure's wording is the HTTP library's, not krowk's:
        // the error code beside it is the behavior, the detail is not.
        (r#"(Get|Post|Put|Patch|Delete|Head) \\?"http[^"\\]*\\?": [^"\n]*"#, "<transport error>"),
        (r"\b\d+(\.\d+)?ms\b", "<n>ms"),
        // How many spinner frames draw is how long the command took.
        (r"(\\r\\e\[K)?([⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏] [^\\\n]*(\\r\\e\[K)?)+", "<spinner>"),
    ]
    .into_iter()
    .map(|(re, to)| (Regex::new(re).unwrap(), to))
    .collect()
});

fn normalize(text: &str, scratch: &Path, registry: Option<&str>, bins: &Bins) -> String {
    let mut text = text.replace(&scratch.display().to_string(), "<scratch>");
    for (bin, name) in [(bins.mcp, "<krowk-mcp>"), (bins.krowk, "<krowk>")] {
        text = text.replace(&bin.display().to_string(), name);
    }
    if !bins.version.is_empty() {
        text = text.replace(bins.version, "<version>");
    }
    if let Some(url) = registry {
        let port = url.rsplit(':').next().unwrap();
        text = text.replace(&format!("127.0.0.1:{port}"), "<registry>").replace(&format!("localhost:{port}"), "<registry>");
    }
    for (re, to) in PLAIN.iter() {
        text = re.replace_all(&text, *to).into_owned();
    }
    // What a jq library says about a failed expression is its own wording; the
    // error code and krowk's sentence around it are the behavior. A message
    // carrying a credential is left alone, so a leak still shows as a diff.
    static JQ: LazyLock<Regex> =
        LazyLock::new(|| Regex::new(r#"--jq: ((?:[^"\\]|\\.)*?)(\. The command itself succeeded|")"#).unwrap());
    text = JQ
        .replace_all(&text, |c: &Captures| {
            let secret = c[1].contains("krowk_sk_") || c[1].contains("krowk_claim_");
            let unsupported = c[1].starts_with("halt and halt_error");
            if secret || unsupported { c[0].to_string() } else { format!("--jq: <jq error>{}", &c[2]) }
        })
        .into_owned();
    // Epoch milliseconds are masked only near the clock: a fixture's own
    // timestamps are behavior and stay, the moment a command ran is not.
    let now = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_millis() as i64;
    static MILLIS: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"\b\d{13}\b").unwrap());
    text = MILLIS
        .replace_all(&text, |c: &Captures| {
            let ms: i64 = c[0].parse().unwrap();
            if (ms - now).abs() < 2 * 86_400_000 { "<now_ms>".to_string() } else { c[0].to_string() }
        })
        .into_owned();
    for (re, kind) in NUMBERED.iter() {
        let mut seen: HashMap<String, usize> = HashMap::new();
        text = re
            .replace_all(&text, |c: &Captures| {
                let label = if kind.is_empty() { c[1].to_string() } else { kind.to_string() };
                let next = seen.iter().filter(|(k, _)| k.starts_with(&format!("{label}\0"))).count() + 1;
                let n = *seen.entry(format!("{label}\0{}", &c[0])).or_insert(next);
                format!("<{label}_{n}>")
            })
            .into_owned();
    }
    text
}
