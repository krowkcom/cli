//! The terminal: an inline ratatui viewport at the bottom of the normal
//! screen, and everything finished written above it into the terminal's own
//! scrollback (R-TUI-1). No alternate screen, no mouse capture, no keyboard
//! protocol extensions: what a phone terminal, tmux or an SSH session does
//! not understand is never sent (R-TUI-3).
//!
//! Three rules keep scrollback exact:
//!
//! - **One write per frame, inside synchronized output.** Every byte ratatui
//!   produces goes into a frame buffer, and the frame reaches the terminal
//!   as one write bracketed by CSI ?2026h … CSI ?2026l, so a terminal that
//!   supports it never shows half a frame, and one that does not ignores
//!   the brackets. One frame is one bracket pair, which is what the redraw
//!   budget counts.
//! - **Lines enter scrollback exactly once.** A finished line is inserted
//!   above the viewport through a scroll region (ratatui's
//!   `scrolling-regions`), never redrawn afterwards; the viewport is the only
//!   thing ever repainted.
//! - **The terminal is asked where the cursor is only when nothing else is
//!   reading it**: at start, and after a resize, with the key reader stopped
//!   (the caller drops it). Everything in between — a taller or shorter live
//!   region above all — is computed from where the viewport already is. A
//!   screen-clearing resize, which ratatui does on a horizontal shrink,
//!   would erase the visible part of the conversation, so the resize is
//!   handled here instead: the old live region is cleared from its top down
//!   and the viewport rebuilt in place.

use crossterm::cursor::MoveTo;
use crossterm::terminal::{Clear, ClearType as CtClear};
use crossterm::{queue, QueueableCommand};
use ratatui::backend::{Backend, ClearType, CrosstermBackend, WindowSize};
use ratatui::buffer::{Buffer, Cell};
use ratatui::layout::{Position, Size};
use ratatui::text::Line;
use ratatui::{Terminal, TerminalOptions, Viewport};
use std::cell::RefCell;
use std::io::{self, Write};
use std::rc::Rc;

/// Begin and end synchronized update (DEC private mode 2026).
pub const SYNC_BEGIN: &[u8] = b"\x1b[?2026h";
pub const SYNC_END: &[u8] = b"\x1b[?2026l";

/// Where a frame's bytes collect until the frame is done. ratatui flushes
/// its writer after almost every operation; here that is a no-op, so the
/// frame leaves in one piece.
#[derive(Clone, Default)]
pub struct FrameBuf(Rc<RefCell<Vec<u8>>>);

impl Write for FrameBuf {
    fn write(&mut self, b: &[u8]) -> io::Result<usize> {
        self.0.borrow_mut().extend_from_slice(b);
        Ok(b.len())
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

impl FrameBuf {
    fn take(&self) -> Vec<u8> {
        std::mem::take(&mut *self.0.borrow_mut())
    }
}

/// crossterm's backend writing into the frame buffer, with the two things
/// that would otherwise talk to the terminal answered from what is known:
/// its size (so ratatui never resizes behind our back) and the cursor (so
/// ratatui never queries it while the key reader owns the input).
pub struct Back {
    inner: CrosstermBackend<FrameBuf>,
    size: Size,
    cursor: Position,
}

impl Backend for Back {
    type Error = io::Error;

    fn draw<'a, I>(&mut self, content: I) -> io::Result<()>
    where
        I: Iterator<Item = (u16, u16, &'a Cell)>,
    {
        self.inner.draw(content)
    }

    fn append_lines(&mut self, n: u16) -> io::Result<()> {
        self.inner.append_lines(n)
    }

    fn hide_cursor(&mut self) -> io::Result<()> {
        self.inner.hide_cursor()
    }

    fn show_cursor(&mut self) -> io::Result<()> {
        self.inner.show_cursor()
    }

    fn get_cursor_position(&mut self) -> io::Result<Position> {
        Ok(self.cursor)
    }

    fn set_cursor_position<P: Into<Position>>(&mut self, position: P) -> io::Result<()> {
        let p = position.into();
        self.cursor = p;
        self.inner.set_cursor_position(p)
    }

    fn clear(&mut self) -> io::Result<()> {
        self.inner.clear()
    }

    fn clear_region(&mut self, clear_type: ClearType) -> io::Result<()> {
        self.inner.clear_region(clear_type)
    }

    fn size(&self) -> io::Result<Size> {
        Ok(self.size)
    }

    fn window_size(&mut self) -> io::Result<WindowSize> {
        Ok(WindowSize { columns_rows: self.size, pixels: Size::default() })
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }

    fn scroll_region_up(&mut self, region: std::ops::Range<u16>, n: u16) -> io::Result<()> {
        self.inner.scroll_region_up(region, n)
    }

    fn scroll_region_down(&mut self, region: std::ops::Range<u16>, n: u16) -> io::Result<()> {
        self.inner.scroll_region_down(region, n)
    }
}

/// The live region and the scrollback above it.
pub struct Term<W: Write> {
    terminal: Terminal<Back>,
    buf: FrameBuf,
    out: W,
    size: Size,
    height: u16,
    /// The row the cursor was left on, relative to the viewport's top: the
    /// input caret. After a resize the terminal still knows where the cursor
    /// is, and the viewport's top is found from it.
    caret_row: u16,
    caret_col: u16,
    /// How many columns each live row last drawn actually used: after a
    /// narrowing resize, a terminal that reflows splits each wider row
    /// into several, and this is how many.
    widths: Vec<u16>,
    /// Where the region was, and how wide the terminal, when the last frame
    /// reached it: what a resize is measured against, however many resizes
    /// come before the next frame.
    drawn_top: u16,
    drawn_width: u16,
    /// Whether this terminal reflows lines on a narrowing resize. Most do;
    /// xterm and the Linux console truncate instead (see `reflows_from`).
    pub reflows: bool,
    /// Frames written, for the tests and the redraw budget's evidence.
    pub frames: u64,
}

impl<W: Write> Term<W> {
    /// A viewport `height` rows tall at the bottom of the screen. `top` is
    /// where the cursor was when the TUI started: what is above it on screen
    /// is moved down to sit right above the viewport (see `anchor`).
    pub fn new(mut out: W, size: Size, top: u16, height: u16) -> io::Result<Term<W>> {
        let buf = FrameBuf::default();
        let height = height.clamp(1, size.height.max(1));
        let top = anchor(&buf, size, top, height)?;
        // Out now, not with the first frame: a resize before that frame
        // measures against a screen that has already moved.
        out.write_all(&buf.take())?;
        out.flush()?;
        let terminal = build(&buf, size, top, height)?;
        let mut t = Term { terminal, buf, out, size, height, caret_row: 0, caret_col: 0, widths: Vec::new(), drawn_top: top, drawn_width: size.width, reflows: true, frames: 0 };
        t.drawn_top = t.top();
        Ok(t)
    }

    pub fn width(&self) -> u16 {
        self.size.width
    }

    pub fn size(&self) -> Size {
        self.size
    }

    fn top(&mut self) -> u16 {
        self.terminal.get_frame().area().y
    }

    /// Changes the live region's height in place: its top stays where it is,
    /// and a taller one pushes what is above it up into scrollback the way
    /// a new line would.
    fn set_height(&mut self, height: u16) -> io::Result<()> {
        let height = height.clamp(1, self.size.height.max(1));
        if height == self.height {
            return Ok(());
        }
        let top = self.top();
        self.rebuild(top, height)
    }

    fn rebuild(&mut self, top: u16, height: u16) -> io::Result<()> {
        let mut w = self.buf.clone();
        queue!(w, MoveTo(0, top), Clear(CtClear::FromCursorDown))?;
        self.terminal = build(&self.buf, self.size, top, height)?;
        self.height = height;
        Ok(())
    }

    /// The terminal changed size. `cursor_row` is where the terminal says
    /// the cursor is now, when it could be asked. The old live region is
    /// found from it and cleared from its top down, then drawn afresh.
    ///
    /// Where its top is depends on what the terminal did to it. One that
    /// truncates (xterm, the Linux console) leaves every row where it was, so
    /// the top is the cursor less the caret's row. One that reflows (tmux,
    /// kitty, VTE, iTerm, WezTerm, Windows Terminal) splits each row wider
    /// than the new width into several and keeps the cursor on the caret, so
    /// the top is the cursor less the rows the region above the caret now
    /// takes — cleared from there, the status bar, overlay or notice that
    /// was split leaves nothing behind.
    ///
    /// Everything is measured against the last frame the terminal actually
    /// got: bytes queued by an earlier resize and never flushed are dropped,
    /// so two resizes before a frame are one resize from what is on screen.
    ///
    /// A reflow never pushes the region into history, because the region
    /// sits at the bottom of the screen (`anchor`): tmux and the others keep
    /// the bottom of their grid on screen, so the rows a reflow adds push
    /// out what is above the region — conversation, already in scrollback's
    /// order — and never the region itself, unless the reflowed region is
    /// taller than the whole screen.
    pub fn resize(&mut self, size: Size, cursor_row: Option<u16>) -> io::Result<()> {
        self.buf.take();
        self.size = size;
        let height = self.height.clamp(1, size.height.max(1));
        let narrowed = size.width < self.drawn_width;
        let top = match cursor_row {
            Some(row) if narrowed && self.reflows => row.saturating_sub(self.reflowed_above_caret(size.width)),
            Some(row) => row.saturating_sub(self.caret_row),
            None => self.drawn_top,
        };
        let top = top.min(size.height.saturating_sub(height));
        self.rebuild(top, height)
    }

    /// Rows the live region above the caret takes once reflowed to `width`.
    pub fn reflowed_above_caret(&self, width: u16) -> u16 {
        let width = width.max(1);
        let rows = |w: u16| w.div_ceil(width).max(1);
        let above: u16 = self.widths.iter().take(usize::from(self.caret_row)).map(|w| rows(*w)).sum();
        above + self.caret_col / width
    }

    /// One frame: `lines` into scrollback, then the live region redrawn as
    /// `rows` with the cursor at `caret` (column, row), all as one
    /// synchronized write.
    pub fn frame(&mut self, lines: &[Line<'static>], rows: &[Line<'static>], caret: (u16, u16)) -> io::Result<()> {
        self.set_height(rows.len() as u16)?;
        let width = self.size.width;
        // A chunk at a time, so the scratch buffer of a long history never
        // holds more than a screenful of cells.
        for chunk in lines.chunks(64) {
            let n = chunk.len() as u16;
            self.terminal.insert_before(n, |buf: &mut Buffer| {
                for (i, line) in chunk.iter().enumerate() {
                    buf.set_line(0, i as u16, line, width);
                }
            })?;
        }
        let shown = usize::from(self.height);
        self.terminal.draw(|f| {
            let area = f.area();
            for (i, row) in rows.iter().enumerate().take(usize::from(area.height)) {
                f.buffer_mut().set_line(area.x, area.y + i as u16, row, width);
            }
            f.set_cursor_position((area.x + caret.0.min(width.saturating_sub(1)), area.y + caret.1.min(area.height.saturating_sub(1))));
        })?;
        self.widths = rows.iter().take(shown).map(|r| (r.width() as u16).min(width)).collect();
        let top = self.top();
        if let Some(Position { x, y }) = completed_cursor(&mut self.terminal) {
            self.caret_row = y.saturating_sub(top);
            self.caret_col = x;
        }
        self.drawn_top = top;
        self.drawn_width = width;
        self.flush()
    }

    fn flush(&mut self) -> io::Result<()> {
        let body = self.buf.take();
        if body.is_empty() {
            return Ok(());
        }
        let mut frame = Vec::with_capacity(body.len() + SYNC_BEGIN.len() + SYNC_END.len());
        frame.extend_from_slice(SYNC_BEGIN);
        frame.extend_from_slice(&body);
        frame.extend_from_slice(SYNC_END);
        self.out.write_all(&frame)?;
        self.out.flush()?;
        self.frames += 1;
        Ok(())
    }

    /// Clears the live region and leaves the cursor at its top, at the
    /// start of a line, so the shell prompt that follows lands right under
    /// the conversation.
    pub fn finish(&mut self) -> io::Result<()> {
        let top = self.top();
        let mut w = self.buf.clone();
        w.queue(MoveTo(0, top))?;
        w.queue(Clear(CtClear::FromCursorDown))?;
        w.queue(crossterm::cursor::Show)?;
        self.flush()
    }

    /// Back after a job stop: the live region starts afresh on the row the
    /// cursor is on now (the shell may have printed below it).
    pub fn resume(&mut self, size: Size, cursor_row: Option<u16>) -> io::Result<()> {
        self.buf.take();
        self.size = size;
        let height = self.height.clamp(1, size.height.max(1));
        let top = cursor_row.unwrap_or(size.height.saturating_sub(height)).min(size.height.saturating_sub(height));
        let top = anchor(&self.buf, size, top, height)?;
        self.rebuild(top, height)?;
        self.drawn_top = self.top();
        self.drawn_width = size.width;
        Ok(())
    }

    pub fn into_inner(self) -> W {
        self.out
    }
}

fn completed_cursor(t: &mut Terminal<Back>) -> Option<Position> {
    t.backend_mut().get_cursor_position().ok()
}

/// Whether the terminal the environment names reflows on a narrowing
/// resize. Real xterm (which sets XTERM_VERSION) and the Linux console
/// truncate; everything else in use today reflows, tmux and screen included.
pub fn reflows_from(env: &dyn Fn(&str) -> String) -> bool {
    let inside_mux = !env("TMUX").is_empty() || env("TERM").starts_with("screen") || env("TERM").starts_with("tmux");
    inside_mux || !(env("TERM") == "linux" || !env("XTERM_VERSION").is_empty())
}

/// Moves the viewport to the bottom of the screen: the rows above `top`
/// (the shell's output, the command line) scroll down to sit right above
/// it, and the blank rows below the cursor become blank rows at the top of
/// the screen. Nothing on screen is lost or drawn twice — only blank rows
/// are scrolled out of the region. A region at the bottom is what keeps a
/// reflowing resize from pushing it into history (see `Term::resize`).
fn anchor(buf: &FrameBuf, size: Size, top: u16, height: u16) -> io::Result<u16> {
    let bottom = size.height.saturating_sub(height);
    // Already at the bottom, or below it: ratatui makes room by scrolling
    // the screen up, as a new line would.
    if top >= bottom {
        return Ok(top);
    }
    if top > 0 {
        CrosstermBackend::new(buf.clone()).scroll_region_down(0..bottom, bottom - top)?;
    }
    Ok(bottom)
}

fn build(buf: &FrameBuf, size: Size, top: u16, height: u16) -> io::Result<Terminal<Back>> {
    let back = Back { inner: CrosstermBackend::new(buf.clone()), size, cursor: Position { x: 0, y: top } };
    let mut back = back;
    // ratatui reserves the viewport's rows by printing newlines from the
    // cursor, so the real cursor has to be at the top first.
    back.inner.set_cursor_position(Position { x: 0, y: top })?;
    Terminal::with_options(back, TerminalOptions { viewport: Viewport::Inline(height) })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn frames(bytes: &[u8]) -> usize {
        bytes.windows(SYNC_BEGIN.len()).filter(|w| *w == SYNC_BEGIN).count()
    }

    #[test]
    fn r_tui_1_every_frame_is_one_synchronized_write() {
        let mut t = Term::new(Vec::new(), Size { width: 40, height: 10 }, 0, 3).unwrap();
        t.frame(&[Line::from("one"), Line::from("two")], &[Line::from("› hi"), Line::default(), Line::default()], (4, 0)).unwrap();
        t.frame(&[], &[Line::from("› hi there"), Line::default(), Line::default(), Line::default()], (10, 0)).unwrap();
        let out = t.into_inner();
        assert_eq!(frames(&out), 2, "one bracket pair per frame");
        let text = String::from_utf8_lossy(&out);
        assert!(text.starts_with("\x1b[?2026h") && text.ends_with("\x1b[?2026l"), "{text:?}");
        assert!(!text.contains("\x1b[2J"), "the screen is never cleared whole: {text:?}");
    }

    /// A 90-wide overlay row, the prompt with the caret at column 50, and a
    /// 90-wide status bar, on a 100x30 terminal whose cursor started at row 10.
    fn drawn() -> Term<Vec<u8>> {
        let mut t = Term::new(Vec::new(), Size { width: 100, height: 30 }, 10, 3).unwrap();
        let bar = Line::from("x".repeat(90));
        t.frame(&[], &[bar.clone(), Line::from("y".repeat(60)), bar], (50, 1)).unwrap();
        t
    }

    fn after_resize(t: &mut Term<Vec<u8>>, steps: &[(u16, u16)]) -> String {
        let before = t.out.len();
        for (w, row) in steps {
            t.resize(Size { width: *w, height: 30 }, Some(*row)).unwrap();
        }
        t.flush().unwrap();
        String::from_utf8_lossy(&t.out[before..]).into_owned()
    }

    #[test]
    fn r_tui_3_the_region_starts_at_the_bottom_with_what_was_above_it_moved_down() {
        let t = Term::new(Vec::new(), Size { width: 40, height: 10 }, 4, 3).unwrap();
        assert_eq!(t.drawn_top, 7, "the bottom three rows");
        let out = String::from_utf8_lossy(&t.out).into_owned();
        // Rows 1-7 scrolled down by 3: the four rows above the cursor land
        // right above the viewport, and only blank rows leave the region.
        assert_eq!(out, "\x1b[1;7r\x1b[3T\x1b[r", "{out:?}");
        let t = Term::new(Vec::new(), Size { width: 40, height: 10 }, 0, 3).unwrap();
        assert!(t.out.is_empty(), "nothing above the cursor: nothing to move");
    }

    #[test]
    fn r_tui_3_a_narrowed_region_is_measured_as_the_terminal_reflows_it() {
        let mut t = drawn();
        assert_eq!(t.drawn_top, 27);
        // At 40 columns the 90-wide row above the caret takes three rows,
        // and the caret itself has moved one row down its own line.
        assert_eq!(t.reflowed_above_caret(40), 3 + 1);
        assert_eq!(t.reflowed_above_caret(100), 1, "unchanged at the old width");
        // Reflowed at the bottom of the grid, the region is 3 + 2 + 3 rows,
        // 22 to 29, and the caret on row 26: its top is 26 - 4 = 22.
        let out = after_resize(&mut t, &[(40, 26)]);
        assert!(out.starts_with("\x1b[?2026h\x1b[23;1H\x1b[J"), "{out:?}");
    }

    #[test]
    fn r_tui_3_a_terminal_that_truncates_keeps_the_region_where_it_was() {
        let mut t = drawn();
        t.reflows = false;
        let out = after_resize(&mut t, &[(40, 28)]);
        assert!(out.starts_with("\x1b[?2026h\x1b[28;1H\x1b[J"), "{out:?}");
    }

    #[test]
    fn r_tui_3_two_resizes_before_a_frame_are_one_from_what_is_on_screen() {
        let mut t = drawn();
        // 100 -> 70 (the region reflows to 2 + 1 + 2 rows, caret on row 26),
        // then 70 -> 40 before any frame: measured from the frame drawn at
        // 100, and the first resize's clear is never sent.
        let out = after_resize(&mut t, &[(70, 26), (40, 26)]);
        assert_eq!(out.matches("\x1b[J").count(), 1, "one clear, not two: {out:?}");
        assert!(out.starts_with("\x1b[?2026h\x1b[23;1H\x1b[J"), "{out:?}");
    }

    #[test]
    fn only_xterm_and_the_console_are_taken_to_truncate() {
        let env = |pairs: &'static [(&'static str, &'static str)]| move |k: &str| pairs.iter().find(|(n, _)| *n == k).map(|(_, v)| v.to_string()).unwrap_or_default();
        assert!(reflows_from(&env(&[("TERM", "xterm-256color")])), "gnome, alacritty and the rest say xterm too");
        assert!(!reflows_from(&env(&[("TERM", "xterm-256color"), ("XTERM_VERSION", "XTerm(390)")])));
        assert!(!reflows_from(&env(&[("TERM", "linux")])));
        assert!(reflows_from(&env(&[("TERM", "linux"), ("TMUX", "/tmp/tmux-1000/default,1,0")])), "tmux reflows whatever it runs in");
    }
}
