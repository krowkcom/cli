//! Bare `krowk` on a terminal, the built binary, against the stand-in
//! Anthropic API. Two kinds of terminal:
//!
//! - a bare pseudo-terminal (`common/pty.rs`), where every byte the TUI
//!   writes is seen and each frame's synchronized-update bracket is
//!   timestamped — for what it sends and how often;
//! - tmux, a real terminal emulator with a scrollback, for what a person
//!   ends up looking at: `capture-pane` of the whole history.
//!
//! The tmux cases need tmux on PATH. CI installs it on Linux; a machine
//! without it skips them with a line saying so, except CI on Linux, where a
//! skip would hide a broken acceptance check and fails instead.

#![cfg(all(feature = "harness", unix))]

#[path = "../../krowk-harness/tests/common/mock.rs"]
mod mock;
#[path = "common/pty.rs"]
mod pty;

use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

struct Sandbox {
    root: PathBuf,
}

impl Sandbox {
    fn new(name: &str) -> Sandbox {
        let root = std::env::temp_dir().join(format!("krowk-tui-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        std::fs::create_dir_all(root.join("home")).unwrap();
        std::fs::create_dir_all(root.join("repo/.git")).unwrap();
        std::fs::write(root.join("repo/README.md"), "# krowk\n\nPermalinks for agent output.\n").unwrap();
        Sandbox { root: root.canonicalize().unwrap() }
    }

    fn env(&self, url: &str) -> Vec<(String, String)> {
        let home = self.root.join("home");
        vec![
            ("PATH".into(), std::env::var("PATH").unwrap_or_default()),
            ("HOME".into(), home.display().to_string()),
            ("TERM".into(), "xterm-256color".into()),
            ("KROWK_NO_UPDATE_CHECK".into(), "1".into()),
            ("ANTHROPIC_API_KEY".into(), "sk-test".into()),
            ("ANTHROPIC_BASE_URL".into(), url.into()),
        ]
    }

    fn command(&self, url: &str, args: &[&str]) -> Command {
        let mut c = Command::new(env!("CARGO_BIN_EXE_krowk"));
        c.args(args).env_clear().envs(self.env(url)).current_dir(self.root.join("repo"));
        c
    }
}

impl Drop for Sandbox {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.root);
    }
}

// ---- a bare pseudo-terminal -------------------------------------------------

#[test]
fn r_pkg_1_r_tui_3_bare_krowk_on_a_terminal_opens_the_prompt_with_only_portable_sequences() {
    let m = mock::serve(mock::readme_script);
    let b = Sandbox::new("opens");
    let mut t = pty::Pty::spawn(b.command(&m.url, &[]), 80, 24);
    assert!(t.wait_for("ask anything", Duration::from_secs(10)).is_some(), "no prompt: {:?}", t.text());
    t.write(b"read README.md and summarise it in one line\r");
    // Blank cells are skipped, not written, so only a word is sure to
    // arrive whole.
    assert!(t.wait_for("anywhere.", Duration::from_secs(10)).is_some(), "no answer: {:?}", t.text());
    assert!(t.wait_for("tokens", Duration::from_secs(5)).is_some());
    // Ctrl-D on an empty prompt quits, and says how to come back.
    t.write(b"\x04");
    let st = t.wait(Duration::from_secs(10)).expect("krowk exits on Ctrl-D");
    assert!(st.success(), "{st}");
    let out = t.text();
    assert!(out.contains("krowk --resume "), "{out:?}");
    // R-TUI-3: nothing a phone terminal, tmux or an SSH hop would not
    // pass through — no alternate screen, no mouse capture, no keyboard
    // protocol push, no full-screen clear.
    for bad in ["\x1b[?1049h", "\x1b[?47h", "\x1b[?1000h", "\x1b[?1002h", "\x1b[?1003h", "\x1b[?1006h", "\x1b[>1u", "\x1b[2J", "\x1b[3J"] {
        assert!(!out.contains(bad), "sent {bad:?}");
    }
    // R-TUI-1: every frame is bracketed, and brackets pair up.
    let (begins, ends) = (t.frames().len(), t.frame_ends().len());
    assert!(begins > 2 && begins == ends, "{begins} frames begun, {ends} ended");
    // The log is the session, and krowk.db lists it.
    let seen = m.seen.lock().unwrap();
    assert!(seen.iter().any(|s| s.body["messages"].as_array().is_some_and(|m| m.len() == 3)), "the tool loop ran");
}

#[test]
fn r_perf_4_a_500_token_a_second_stream_redraws_at_most_60_times_a_second() {
    // ~1,500 tokens, one delta every 2 ms: three seconds of streaming.
    let body = mock::text_stream(&mock::numbered_lines(125));
    let m = mock::serve(move |_, _| mock::Reply::paced(body.clone(), Duration::from_millis(2)));
    let b = Sandbox::new("fps");
    let mut t = pty::Pty::spawn(b.command(&m.url, &[]), 100, 30);
    assert!(t.wait_for("ask anything", Duration::from_secs(10)).is_some());
    let before = t.frames().len();
    t.write(b"stream\r");
    assert!(t.wait_for("00125:", Duration::from_secs(20)).is_some(), "the stream never finished");
    let frames: Vec<Instant> = t.frames()[before..].to_vec();
    let peak = pty::peak_fps(&frames);
    assert!(frames.len() >= 30, "it redrew while streaming: {} frames", frames.len());
    assert!(peak <= 60, "{peak} frames inside one second");
}

#[test]
fn r_perf_2_nothing_is_drawn_while_idle() {
    let m = mock::serve(mock::readme_script);
    let b = Sandbox::new("idle");
    let t = pty::Pty::spawn(b.command(&m.url, &[]), 80, 24);
    assert!(t.wait_for("online", Duration::from_secs(10)).is_some(), "{:?}", t.text());
    std::thread::sleep(Duration::from_millis(300));
    let before = t.output().len();
    std::thread::sleep(Duration::from_secs(2));
    assert_eq!(t.output().len(), before, "an idle TUI wrote {:?}", String::from_utf8_lossy(&t.output()[before..]));
}

/// A provider that takes the request and never answers: a turn that waits.
fn silent_provider() -> String {
    let l = TcpListener::bind("127.0.0.1:0").unwrap();
    let url = format!("http://{}", l.local_addr().unwrap());
    std::thread::spawn(move || {
        let mut held = Vec::new();
        for c in l.incoming().flatten() {
            held.push(c);
        }
    });
    url
}

fn krowk_sessions(b: &Sandbox, url: &str) -> usize {
    let out = b.command(url, &["sessions", "--json"]).stdin(std::process::Stdio::null()).output().unwrap();
    let v: serde_json::Value = serde_json::from_slice(&out.stdout).unwrap_or_default();
    v["data"]["sessions"].as_array().map_or(0, Vec::len)
}

#[test]
fn sigterm_and_sighup_restore_the_terminal_and_record_the_session() {
    for (name, sig) in [("term", libc::SIGTERM), ("hup", libc::SIGHUP)] {
        let m = mock::serve(mock::readme_script);
        let b = Sandbox::new(&format!("sig{name}"));
        let mut t = pty::Pty::spawn(b.command(&m.url, &[]), 80, 24);
        assert!(t.wait_for("ask anything", Duration::from_secs(10)).is_some());
        t.write(b"read README.md and summarise it in one line\r");
        assert!(t.wait_for("tokens", Duration::from_secs(10)).is_some(), "{:?}", t.text());
        // SAFETY: a signal to our own child.
        unsafe {
            libc::kill(t.child.id() as i32, sig);
        }
        let st = t.wait(Duration::from_secs(10)).expect("krowk exits on the signal");
        assert!(st.success(), "{name}: {st}");
        let out = t.text();
        assert!(out.contains("krowk --resume "), "{name}: the resume line: {out:?}");
        assert!(out.ends_with("\x1b[?2004l\x1b[?25h"), "{name}: bracketed paste off and the cursor back, last: {out:?}");
        assert_eq!(krowk_sessions(&b, &m.url), 1, "{name}: the session is in krowk.db");
    }
}

#[test]
fn a_second_ctrl_c_leaves_at_once_but_still_records_the_session_and_exits_130() {
    let url = silent_provider();
    let b = Sandbox::new("ctrlc2");
    let mut t = pty::Pty::spawn(b.command(&url, &[]), 80, 24);
    assert!(t.wait_for("ask anything", Duration::from_secs(10)).is_some());
    t.write(b"wait forever\r");
    assert!(t.wait_for("esc to interrupt", Duration::from_secs(10)).is_some(), "{:?}", t.text());
    t.write(b"\x03\x03");
    let st = t.wait(Duration::from_secs(10)).expect("krowk exits on the second Ctrl-C");
    assert_eq!(st.code(), Some(130), "{st}");
    let out = t.text();
    assert!(out.contains("krowk --resume ") && out.ends_with("\x1b[?2004l\x1b[?25h"), "{out:?}");
    assert_eq!(krowk_sessions(&b, &url), 1, "the session is in krowk.db");
}

#[test]
fn steering_an_interrupted_turn_never_read_goes_back_into_the_prompt_not_sent() {
    let url = silent_provider();
    let b = Sandbox::new("steerback");
    let mut t = pty::Pty::spawn(b.command(&url, &[]), 80, 24);
    assert!(t.wait_for("ask anything", Duration::from_secs(10)).is_some());
    t.write(b"wait forever\r");
    assert!(t.wait_for("esc to interrupt", Duration::from_secs(10)).is_some(), "{:?}", t.text());
    t.write(b"also this\r");
    assert!(t.wait_for("steer queued", Duration::from_secs(5)).is_some(), "{:?}", t.text());
    t.write(b"\x1b");
    assert!(t.wait_for("back in the prompt", Duration::from_secs(5)).is_some(), "{:?}", t.text());
    std::thread::sleep(Duration::from_millis(300));
    let out = t.text();
    let tail = &out[out.rfind("back in the prompt").unwrap()..];
    // Blank cells are skipped, not written: the words arrive apart.
    assert!(tail.contains("also") && tail.contains("this"), "the steer is in the prompt again: {tail:?}");
    assert!(!tail.contains("esc to interrupt"), "and no new turn was started with it: {tail:?}");
}

// ---- tmux ---------------------------------------------------------------------

struct Tmux {
    socket: String,
}

impl Tmux {
    /// None when tmux is not installed and this is not CI.
    fn start(name: &str, cols: u16, rows: u16, cwd: &Path, env: &[(String, String)], args: &[&str]) -> Option<Tmux> {
        Tmux::start_after(name, cols, rows, cwd, env, args, "")
    }

    /// As `start`, with `before` run in the shell first — output a person's
    /// terminal already holds when they type `krowk`.
    fn start_after(name: &str, cols: u16, rows: u16, cwd: &Path, env: &[(String, String)], args: &[&str], before: &str) -> Option<Tmux> {
        if Command::new("tmux").arg("-V").output().is_err() {
            assert!(!cfg!(target_os = "linux") || std::env::var_os("CI").is_none(), "tmux is not installed, and CI on Linux must run this check");
            eprintln!("skipped: tmux is not installed");
            return None;
        }
        let socket = format!("krowk-tui-{name}-{}", std::process::id());
        let conf = std::env::temp_dir().join(format!("{socket}.conf"));
        std::fs::write(&conf, "set -g history-limit 100000\nset -g status off\n").unwrap();
        let mut line = format!("cd '{}' && {before} exec env -i", cwd.display());
        for (k, v) in env {
            line += &format!(" {k}='{v}'");
        }
        line += &format!(" '{}'", env!("CARGO_BIN_EXE_krowk"));
        for a in args {
            line += &format!(" '{a}'");
        }
        let st = Command::new("tmux")
            .args(["-L", &socket, "-f"])
            .arg(&conf)
            .args(["new-session", "-d", "-s", "t", "-x", &cols.to_string(), "-y", &rows.to_string(), &line])
            .status()
            .unwrap();
        assert!(st.success(), "tmux new-session: {st}");
        Some(Tmux { socket })
    }

    fn tmux(&self, args: &[&str]) -> String {
        let out = Command::new("tmux").args(["-L", &self.socket]).args(args).output().unwrap();
        String::from_utf8_lossy(&out.stdout).into_owned()
    }

    fn keys(&self, keys: &[&str]) {
        let mut a = vec!["send-keys", "-t", "t"];
        a.extend_from_slice(keys);
        self.tmux(&a);
    }

    fn screen(&self) -> String {
        self.tmux(&["capture-pane", "-p", "-t", "t"])
    }

    fn history(&self) -> String {
        self.tmux(&["capture-pane", "-p", "-t", "t", "-S", "-", "-E", "-"])
    }

    /// The whole history with the terminal's own soft wraps joined back:
    /// each line as it was printed.
    fn history_joined(&self) -> String {
        self.tmux(&["capture-pane", "-p", "-J", "-t", "t", "-S", "-", "-E", "-"])
    }

    fn wait_for(&self, needle: &str, timeout: Duration) -> Option<Duration> {
        let t0 = Instant::now();
        while t0.elapsed() < timeout {
            if self.screen().contains(needle) {
                return Some(t0.elapsed());
            }
            std::thread::sleep(Duration::from_millis(40));
        }
        None
    }

    /// As `wait_for`, over the whole history: a fast stream scrolls a line
    /// past the screen before a poll of the screen alone can see it.
    fn wait_in_history(&self, needle: &str, timeout: Duration) -> Option<Duration> {
        let t0 = Instant::now();
        while t0.elapsed() < timeout {
            if self.history().contains(needle) {
                return Some(t0.elapsed());
            }
            std::thread::sleep(Duration::from_millis(40));
        }
        None
    }

    fn wait_gone(&self, needle: &str, timeout: Duration) -> Option<Duration> {
        let t0 = Instant::now();
        while t0.elapsed() < timeout {
            if !self.screen().contains(needle) {
                return Some(t0.elapsed());
            }
            std::thread::sleep(Duration::from_millis(40));
        }
        None
    }
}

impl Drop for Tmux {
    fn drop(&mut self) {
        self.tmux(&["kill-server"]);
        let _ = std::fs::remove_file(std::env::temp_dir().join(format!("{}.conf", self.socket)));
    }
}

fn streamed(lines: usize, pace: Duration) -> mock::Mock {
    let body = mock::text_stream(&mock::numbered_lines(lines));
    mock::serve(move |_, _| mock::Reply::paced(body.clone(), pace))
}

#[test]
fn r_tui_1_a_10k_token_answer_lands_in_tmux_scrollback_exactly_once() {
    // 850 lines of about twelve tokens: a 10k-token answer.
    let m = streamed(850, Duration::from_micros(100));
    let b = Sandbox::new("scrollback");
    let Some(tm) = Tmux::start("scrollback", 100, 30, &b.root.join("repo"), &b.env(&m.url), &[]) else { return };
    assert!(tm.wait_for("ask anything", Duration::from_secs(10)).is_some(), "{}", tm.screen());
    tm.keys(&["write it all out", "Enter"]);
    assert!(tm.wait_for("tokens", Duration::from_secs(60)).is_some(), "the answer never finished:\n{}", tm.screen());
    let history = tm.history();
    let got: Vec<&str> = history.lines().filter(|l| l.starts_with("line ")).collect();
    let want: Vec<String> = mock::numbered_lines(850).lines().map(String::from).collect();
    assert_eq!(got.len(), want.len(), "every line once, none twice");
    assert!(got.iter().zip(&want).all(|(g, w)| g.trim_end() == w), "in order, byte for byte");
    // The prompt line is in scrollback once too, and the live region is not.
    assert_eq!(history.matches("› write it all out").count(), 1, "{history}");
    assert_eq!(history.matches("esc to interrupt").count(), 0, "a live row leaked into scrollback");
}

#[test]
fn r_tui_3_a_phone_width_terminal_wraps_and_still_keeps_every_line_once() {
    let m = streamed(200, Duration::from_micros(100));
    let b = Sandbox::new("phone");
    let Some(tm) = Tmux::start("phone", 40, 20, &b.root.join("repo"), &b.env(&m.url), &[]) else { return };
    assert!(tm.wait_for("›", Duration::from_secs(10)).is_some(), "{}", tm.screen());
    tm.keys(&["go", "Enter"]);
    assert!(tm.wait_for("tokens", Duration::from_secs(60)).is_some(), "{}", tm.screen());
    let history = tm.history();
    assert!(history.lines().all(|l| l.trim_end().chars().count() <= 40), "a row wider than the terminal:\n{history}");
    // The terminal wrapped the answer itself, so joining its wraps gives
    // back every line exactly as it was streamed, once and in order.
    let joined = tm.history_joined();
    let got: Vec<&str> = joined.lines().map(str::trim_end).filter(|l| l.starts_with("line ")).collect();
    let want: Vec<String> = mock::numbered_lines(200).lines().map(String::from).collect();
    assert_eq!(got, want, "the answer, wrapped by the terminal, is in scrollback once and in order");
}

#[test]
fn r_tui_3_a_widened_terminal_rewraps_the_answer_already_in_scrollback() {
    // Printed at 40 columns, the answer's lines wrap; widened to 100 the
    // terminal joins them again, because krowk left the wrapping to it.
    let m = streamed(20, Duration::from_micros(100));
    let b = Sandbox::new("widen");
    let Some(tm) = Tmux::start("widen", 40, 30, &b.root.join("repo"), &b.env(&m.url), &[]) else { return };
    assert!(tm.wait_for("›", Duration::from_secs(10)).is_some(), "{}", tm.screen());
    tm.keys(&["go", "Enter"]);
    assert!(tm.wait_for("tokens", Duration::from_secs(30)).is_some(), "{}", tm.screen());
    tm.tmux(&["resize-window", "-t", "t", "-x", "100", "-y", "30"]);
    std::thread::sleep(Duration::from_millis(500));
    let history = tm.history();
    let whole = history.lines().filter(|l| l.trim_end().ends_with("lazy dog again") && l.starts_with("line ")).count();
    assert_eq!(whole, 20, "every line is one row again at 100 columns:\n{history}");
}

#[test]
fn r_tui_3_a_resize_mid_stream_never_repeats_a_line_or_leaves_the_live_region_behind() {
    // A narrower and shorter window mid-stream, then a larger one after.
    let m = streamed(300, Duration::from_millis(1));
    let b = Sandbox::new("resize");
    let Some(tm) = Tmux::start("resize", 100, 30, &b.root.join("repo"), &b.env(&m.url), &[]) else { return };
    assert!(tm.wait_for("ask anything", Duration::from_secs(10)).is_some(), "{}", tm.screen());
    tm.keys(&["go", "Enter"]);
    assert!(tm.wait_in_history("line 00020", Duration::from_secs(30)).is_some(), "{}", tm.screen());
    tm.tmux(&["resize-window", "-t", "t", "-x", "70", "-y", "20"]);
    assert!(tm.wait_for("tokens", Duration::from_secs(60)).is_some(), "{}", tm.screen());
    tm.tmux(&["resize-window", "-t", "t", "-x", "120", "-y", "40"]);
    std::thread::sleep(Duration::from_millis(300));
    let history = tm.history();
    // A frame already on its way when the terminal changes size lands in
    // rows that moved under it — a race every terminal program has — so the
    // lines streamed across the resize itself are not held here. Nothing is
    // ever there twice, nothing of the live region is left behind, and
    // every line from shortly after the resize on is there, in order.
    let seen: Vec<u32> = history.lines().filter_map(|l| l.strip_prefix("line ")?.get(..5)?.parse().ok()).collect();
    let mut sorted = seen.clone();
    sorted.dedup();
    assert_eq!(sorted, seen, "a line twice, or out of order:\n{history}");
    // At most the one line in flight at each of the two resizes.
    let missing: Vec<u32> = (1..=300).filter(|n| !seen.contains(n)).collect();
    assert!(missing.len() <= 2, "more than a line lost per resize: {missing:?}\n{history}");
    assert!(seen.contains(&300), "the end of the answer is there:\n{history}");
    for live in ["esc to interrupt", "type to steer"] {
        assert!(!history.contains(live), "the old live region was left in scrollback:\n{history}");
    }
    assert_eq!(history.matches("› go").count(), 1, "{history}");
    let bars = tm.screen().matches("api key").count();
    assert_eq!(bars, 1, "one status bar on screen after two resizes:\n{}", tm.screen());
}

/// The live region at its widest — the offline notice, the keys overlay,
/// the prompt and the status bar, every row split two or three ways by a
/// reflow at 40 columns — then narrowed by `steps`. Each of those rows must
/// be in scrollback exactly once afterwards.
fn narrowing(name: &str, before: &str, steps: &[&str]) {
    let port = TcpListener::bind("127.0.0.1:0").unwrap().local_addr().unwrap().port();
    let b = Sandbox::new(name);
    let Some(tm) = Tmux::start_after(name, 100, 30, &b.root.join("repo"), &b.env(&format!("http://127.0.0.1:{port}")), &[], before) else { return };
    assert!(tm.wait_for("no network connectivity", Duration::from_secs(10)).is_some(), "{}", tm.screen());
    tm.keys(&["?"]);
    assert!(tm.wait_for("? or esc closes this", Duration::from_secs(5)).is_some(), "{}", tm.screen());
    // Back to back in one tmux command: no frame in between.
    let mut args: Vec<&str> = Vec::new();
    for (i, w) in steps.iter().enumerate() {
        if i > 0 {
            args.push(";");
        }
        args.extend(["resize-window", "-t", "t", "-x", w, "-y", "30"]);
    }
    tm.tmux(&args);
    std::thread::sleep(Duration::from_millis(800));
    let history = tm.history();
    for row in ["enter send · alt-enter", "⚠ no network connectivity", "› ask anything", "· anthropic · api key"] {
        assert_eq!(history.matches(row).count(), 1, "{row:?} is in scrollback twice — the old live region was left behind:\n{history}");
    }
    assert_eq!(history.matches("krowk dev").count(), 1, "the header is still there, once:\n{history}");
    if !before.is_empty() {
        // What was on the terminal is kept, and moving the region to the
        // bottom left no gap between it and the conversation.
        let lines: Vec<&str> = history.lines().collect();
        let last = lines.iter().rposition(|l| l.starts_with("earlier output")).expect("the earlier output is kept");
        let n: usize = lines[last].trim_start_matches("earlier output ").trim().parse().unwrap();
        assert_eq!(lines.iter().filter(|l| l.starts_with("earlier output")).count(), n, "every earlier line, once:\n{history}");
        assert!(lines[last + 1].starts_with("krowk dev"), "the header follows the earlier output directly:\n{history}");
    }
}

#[test]
fn r_tui_3_narrowing_a_fresh_terminal_leaves_no_reflowed_live_region_in_scrollback() {
    narrowing("narrow-fresh", "", &["40"]);
}

#[test]
fn r_tui_3_narrowing_under_a_screenful_leaves_no_reflowed_live_region_in_scrollback() {
    narrowing("narrow-full", "for i in $(seq 1 40); do echo \"earlier output $i\"; done;", &["40"]);
}

#[test]
fn r_tui_3_narrowing_under_a_few_lines_leaves_no_reflowed_live_region_in_scrollback() {
    narrowing("narrow-few", "for i in $(seq 1 5); do echo \"earlier output $i\"; done;", &["40"]);
}

#[test]
fn r_tui_3_two_narrowings_back_to_back_leave_no_fragment() {
    narrowing("narrow-twice", "", &["70", "40"]);
}

#[test]
fn ctrl_z_suspends_to_the_shell_and_fg_brings_the_prompt_back() {
    let m = mock::serve(mock::readme_script);
    let b = Sandbox::new("ctrlz");
    // A job-control shell, as a person has, and krowk typed into it.
    if Command::new("tmux").arg("-V").output().is_err() {
        assert!(!cfg!(target_os = "linux") || std::env::var_os("CI").is_none(), "tmux is not installed, and CI on Linux must run this check");
        return;
    }
    let socket = format!("krowk-tui-ctrlz-sh-{}", std::process::id());
    let tmux = |args: &[&str]| String::from_utf8_lossy(&Command::new("tmux").args(["-L", &socket]).args(args).output().unwrap().stdout).into_owned();
    let mut envs = String::new();
    for (k, v) in b.env(&m.url) {
        envs += &format!(" {k}='{v}'");
    }
    let shell = format!("cd '{}' && exec env -i{envs} PS1='sh$ ' bash --norc --noprofile -i", b.root.join("repo").display());
    tmux(&["-f", "/dev/null", "new-session", "-d", "-s", "t", "-x", "100", "-y", "30", &shell]);
    let screen = || tmux(&["capture-pane", "-p", "-t", "t"]);
    let wait = |needle: &str| (0..250).any(|_| screen().contains(needle) || { std::thread::sleep(Duration::from_millis(40)); false });
    assert!(wait("sh$"), "{}", screen());
    tmux(&["send-keys", "-t", "t", &format!("'{}'", env!("CARGO_BIN_EXE_krowk")), "Enter"]);
    assert!(wait("ask anything"), "{}", screen());
    tmux(&["send-keys", "-t", "t", "C-z"]);
    assert!(wait("Stopped"), "Ctrl-Z did not stop krowk:\n{}", screen());
    let stopped = screen();
    assert!(!stopped.contains("ask anything"), "the live region was cleared before stopping:\n{stopped}");
    tmux(&["send-keys", "-t", "t", "fg", "Enter"]);
    assert!(wait("ask anything"), "fg did not bring the prompt back:\n{}", screen());
    tmux(&["send-keys", "-t", "t", "C-d"]);
    assert!(wait("krowk --resume") || wait("sh$"), "{}", screen());
    tmux(&["kill-server"]);
}

/// A TCP relay in front of the stand-in API that can be cut: new
/// connections refused (its port closed) and open ones left hanging with
/// nothing moving — what a dropped Wi-Fi looks like from here.
struct Relay {
    port: u16,
    cut: Arc<AtomicBool>,
}

impl Relay {
    fn new(upstream: &str) -> Relay {
        let upstream = upstream.trim_start_matches("http://").to_string();
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let port = listener.local_addr().unwrap().port();
        let cut = Arc::new(AtomicBool::new(false));
        let c = cut.clone();
        std::thread::spawn(move || {
            let mut listener = Some(listener);
            loop {
                if c.load(Ordering::SeqCst) {
                    listener = None;
                    std::thread::sleep(Duration::from_millis(10));
                    continue;
                }
                let l = listener.get_or_insert_with(|| TcpListener::bind(("127.0.0.1", port)).expect("the relay's port back"));
                l.set_nonblocking(true).unwrap();
                match l.accept() {
                    Ok((down, _)) => {
                        down.set_nonblocking(false).unwrap();
                        let Ok(up) = TcpStream::connect(&upstream) else { continue };
                        pipe(down.try_clone().unwrap(), up.try_clone().unwrap(), c.clone());
                        pipe(up, down, c.clone());
                    }
                    Err(_) => std::thread::sleep(Duration::from_millis(5)),
                }
            }
        });
        Relay { port, cut }
    }

    fn url(&self) -> String {
        format!("http://127.0.0.1:{}", self.port)
    }
}

fn pipe(mut from: TcpStream, mut to: TcpStream, cut: Arc<AtomicBool>) {
    std::thread::spawn(move || {
        let mut buf = [0u8; 8192];
        loop {
            let n = match from.read(&mut buf) {
                // One side closed: so is the other, as a real hop would.
                Ok(0) | Err(_) => {
                    let _ = to.shutdown(std::net::Shutdown::Write);
                    return;
                }
                Ok(n) => n,
            };
            // Cut: whatever is in flight is dropped, and the connection
            // stays open and silent.
            while cut.load(Ordering::SeqCst) {
                std::thread::sleep(Duration::from_millis(10));
            }
            if to.write_all(&buf[..n]).is_err() {
                return;
            }
        }
    });
}

#[test]
fn r_off_1_a_cut_network_shows_the_notice_within_two_seconds_and_nothing_hangs() {
    // A slow answer: fifty tokens a second, for long enough to cut it.
    let m = streamed(400, Duration::from_millis(20));
    let relay = Relay::new(&m.url);
    let b = Sandbox::new("offline");
    let Some(tm) = Tmux::start("offline", 100, 30, &b.root.join("repo"), &b.env(&relay.url()), &[]) else { return };
    assert!(tm.wait_for("online", Duration::from_secs(10)).is_some(), "{}", tm.screen());
    tm.keys(&["tell me everything", "Enter"]);
    assert!(tm.wait_in_history("line 00003", Duration::from_secs(10)).is_some(), "{}", tm.screen());

    relay.cut.store(true, Ordering::SeqCst);
    let shown = tm.wait_for("no network connectivity", Duration::from_secs(5)).unwrap_or_else(|| panic!("no notice:\n{}", tm.screen()));
    assert!(shown <= Duration::from_secs(2), "the notice took {shown:?}");
    assert!(tm.screen().contains("offline"), "the status bar says so too:\n{}", tm.screen());

    // Nothing hangs: Esc stops the stalled turn, and what arrived is kept.
    tm.keys(&["Escape"]);
    assert!(tm.wait_for("interrupted", Duration::from_secs(5)).is_some(), "the turn did not stop:\n{}", tm.screen());
    std::thread::sleep(Duration::from_millis(1500));
    assert!(tm.screen().contains("no network connectivity"), "the notice is persistent:\n{}", tm.screen());

    relay.cut.store(false, Ordering::SeqCst);
    let cleared = tm.wait_gone("no network connectivity", Duration::from_secs(15)).unwrap_or_else(|| panic!("the notice never cleared:\n{}", tm.screen()));
    assert!(cleared <= Duration::from_secs(12), "{cleared:?}");
    assert!(tm.screen().contains("online"), "{}", tm.screen());
}
