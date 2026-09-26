//! A yes-or-no card asked before the TUI takes the terminal: the trust
//! question, in the TUI's look — a title, what it is about, and one key to
//! answer — rather than a line prompt waiting for Enter.

use crossterm::event::{self, Event, KeyCode, KeyEventKind, KeyModifiers};
use std::io::Write;

/// Draws `title` over `body` on stderr and waits for one key: `y` is yes;
/// `n`, Esc, Enter and Ctrl-C are no. The answer is left on screen, one
/// line where the keys were.
pub fn confirm(title: &str, body: &[String], yes: &str, no: &str) -> bool {
    let mut err = std::io::stderr();
    let _ = write!(err, "\r\n  \x1b[1;35m{}\x1b[0m\r\n", clean(title));
    for line in body {
        let _ = write!(err, "  \x1b[2m{}\x1b[0m\r\n", clean(line));
    }
    let _ = write!(err, "\r\n  \x1b[1my\x1b[0m \x1b[2m{}\x1b[0m   \x1b[1mn\x1b[0m \x1b[2m{}\x1b[0m", clean(yes), clean(no));
    let _ = err.flush();
    let answer = read_answer();
    let (glyph, word) = if answer { ("\x1b[32m✓\x1b[0m", yes) } else { ("\x1b[2m✗\x1b[0m", no) };
    let _ = write!(err, "\r\x1b[2K  {glyph} \x1b[2m{}\x1b[0m\r\n", clean(word));
    let _ = err.flush();
    answer
}

fn read_answer() -> bool {
    if crossterm::terminal::enable_raw_mode().is_err() {
        return false;
    }
    let answer = loop {
        match event::read() {
            Ok(Event::Key(k)) if k.kind != KeyEventKind::Release => match k.code {
                KeyCode::Char('c') if k.modifiers.contains(KeyModifiers::CONTROL) => break false,
                KeyCode::Char('y' | 'Y') => break true,
                KeyCode::Char('n' | 'N') | KeyCode::Esc | KeyCode::Enter => break false,
                _ => {}
            },
            Ok(_) => {}
            Err(_) => break false,
        }
    };
    let _ = crossterm::terminal::disable_raw_mode();
    answer
}

/// Text only: a path or a vendor's name never carries an escape sequence.
fn clean(s: &str) -> String {
    s.chars().filter(|c| !c.is_control()).collect()
}
