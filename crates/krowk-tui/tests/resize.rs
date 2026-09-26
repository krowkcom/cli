//! The renderer against tmux, a real terminal with a scrollback, with the
//! window resized exactly between two frames — the race a person dragging
//! a window edge mid-answer runs, made to happen every time. Its own test
//! binary: the tmux it forks must not hold another test's sockets open.

#![cfg(unix)]

use krowk_tui::term::Term;
use ratatui::layout::Size;
use ratatui::text::Line;
use std::cell::RefCell;
use std::io::Write;
use std::process::Command;
use std::rc::Rc;
use std::time::{Duration, Instant};

/// What the renderer sends, held until the test passes it to the pane.
#[derive(Clone, Default)]
struct Out(Rc<RefCell<Vec<u8>>>);

impl Write for Out {
    fn write(&mut self, b: &[u8]) -> std::io::Result<usize> {
        self.0.borrow_mut().extend_from_slice(b);
        Ok(b.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

/// A tmux pane showing, raw, what is sent to it.
struct Pane {
    socket: String,
    fifo: std::path::PathBuf,
    input: std::fs::File,
    marks: u32,
}

impl Pane {
    fn start(name: &str, cols: u16, rows: u16) -> Option<Pane> {
        if Command::new("tmux").arg("-V").output().is_err() {
            assert!(!cfg!(target_os = "linux") || std::env::var_os("CI").is_none(), "tmux is not installed, and CI on Linux must run this check");
            eprintln!("skipped: tmux is not installed");
            return None;
        }
        let socket = format!("krowk-term-{name}-{}", std::process::id());
        let tmp = std::env::temp_dir();
        let (fifo, conf) = (tmp.join(format!("{socket}.fifo")), tmp.join(format!("{socket}.conf")));
        let _ = std::fs::remove_file(&fifo);
        assert!(Command::new("mkfifo").arg(&fifo).status().unwrap().success());
        std::fs::write(&conf, "set -g history-limit 10000\nset -g status off\n").unwrap();
        let cmd = format!("stty raw -echo; exec cat '{}'", fifo.display());
        let (x, y) = (cols.to_string(), rows.to_string());
        let st = Command::new("tmux").args(["-L", &socket, "-f"]).arg(&conf).args(["new-session", "-d", "-s", "t", "-x", &x, "-y", &y, &cmd]).status().unwrap();
        let _ = std::fs::remove_file(&conf);
        assert!(st.success());
        let input = std::fs::OpenOptions::new().write(true).open(&fifo).unwrap();
        Some(Pane { socket, fifo, input, marks: 0 })
    }

    fn tmux(&self, args: &[&str]) -> String {
        let out = Command::new("tmux").arg("-L").arg(&self.socket).args(args).output().unwrap();
        String::from_utf8_lossy(&out.stdout).into_owned()
    }

    /// What the renderer sent since the last time, then a title the pane is
    /// waited on to show: it has read everything before it.
    fn send(&mut self, out: &Out) {
        self.marks += 1;
        let mark = format!("mark-{}", self.marks);
        self.input.write_all(&std::mem::take(&mut *out.0.borrow_mut())).unwrap();
        write!(self.input, "\x1b]2;{mark}\x07").unwrap();
        self.input.flush().unwrap();
        let t0 = Instant::now();
        while self.tmux(&["display", "-p", "-t", "t", "#{pane_title}"]).trim() != mark {
            assert!(t0.elapsed() < Duration::from_secs(10), "the pane never read {mark}");
            std::thread::sleep(Duration::from_millis(10));
        }
    }

    fn cursor_row(&self) -> u16 {
        self.tmux(&["display", "-p", "-t", "t", "#{cursor_y}"]).trim().parse().unwrap()
    }

    fn resize(&self, cols: u16, rows: u16) {
        self.tmux(&["resize-window", "-t", "t", "-x", &cols.to_string(), "-y", &rows.to_string()]);
    }

    fn history(&self) -> String {
        self.tmux(&["capture-pane", "-p", "-t", "t", "-S", "-", "-E", "-"])
    }
}

impl Drop for Pane {
    fn drop(&mut self) {
        self.tmux(&["kill-server"]);
        let _ = std::fs::remove_file(&self.fifo);
    }
}

/// Forty lines in scrollback and the forty-first streaming, at 100x30; the
/// frame that finishes it is read by the pane before the window shrinks to
/// 70x20, or after. Then the resize is handled and line 42 comes. Every
/// line must be in scrollback once, and the live region only on screen.
fn shrunk_around_a_frame(name: &str, frame_read_first: bool) {
    let Some(mut pane) = Pane::start(name, 100, 30) else { return };
    let line = |n: u32| Line::from(format!("line {n:05}: the quick brown fox jumps over the lazy dog again"));
    let live = |tail: Option<&str>| {
        let mut rows: Vec<Line<'static>> = tail.map(|t| Line::from(t.to_string())).into_iter().collect();
        rows.extend([Line::from("⠹ Responding… 0.3s │ esc to interrupt"), Line::from("❯ "), Line::from("claude-opus-5-5 │ anthropic · api key │ $0.05 │ online")]);
        let caret = (2, rows.len() as u16 - 2);
        (rows, caret)
    };
    let out = Out::default();
    let mut t = Term::new(out.clone(), Size { width: 100, height: 30 }, 0, 4).unwrap();
    let (rows, caret) = live(Some("line 00041: the quick"));
    t.frame(&(1..=40).map(line).collect::<Vec<_>>(), &rows, caret).unwrap();
    pane.send(&out);
    let (rows, caret) = live(None);
    t.frame(&[line(41)], &rows, caret).unwrap();
    if frame_read_first {
        pane.send(&out);
        pane.resize(70, 20);
    } else {
        pane.resize(70, 20);
        pane.send(&out);
    }
    t.resize(Size { width: 70, height: 20 }, Some(pane.cursor_row())).unwrap();
    t.frame(&[line(42)], &rows, caret).unwrap();
    pane.send(&out);
    let history = pane.history();
    let seen: Vec<u32> = history.lines().filter_map(|l| l.strip_prefix("line ")?.get(..5)?.parse().ok()).collect();
    assert_eq!(seen, (1..=42).collect::<Vec<_>>(), "every line once, in order:\n{history}");
    assert_eq!(history.matches("esc to interrupt").count(), 1, "the old live region was left behind:\n{history}");
}

#[test]
fn r_tui_3_a_shorter_screen_scrolls_the_conversation_up_rather_than_clearing_it() {
    // tmux takes the rows below the caret first, so the region no longer
    // fits under its top: rows are made for it, and none is cleared.
    shrunk_around_a_frame("shrink", true);
}

#[test]
fn r_tui_3_a_frame_the_terminal_reads_after_it_resized_still_lands_on_the_region() {
    // Drawn for 100x30 and read at 70x20: its moves from the caret still
    // start at the region's top, where absolute rows would clamp to the
    // bottom and leave the old region, and the line it held, above it.
    shrunk_around_a_frame("race", false);
}
