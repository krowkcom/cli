//! What the TUI shows, as state: the lines waiting to go into scrollback,
//! the item streaming right now, the prompt, the status bar. Everything here
//! is driven by protocol frames (`StreamLine`) and key presses, and nothing
//! here touches the terminal or the engine — `lib.rs` does both — so the
//! whole of it is testable against plain values.
//!
//! What is finished is handed to scrollback once and forgotten: the app
//! keeps the line being typed, never the conversation (R-PERF-3). A
//! streamed answer is committed a line at a time as each line completes;
//! only the unfinished tail is drawn in the live region.

use crate::editor::Editor;
use crate::settings::{Item as StatusItem, Settings};
use krowk_harness::host::Pricer;
use krowk_harness::protocol::{Delta, ErrorInfo, Item, ItemKind, LiveEvent, LogBody, LogEvent, ModelRef, RunResult, StreamLine, TurnStatus, Usage};
use ratatui::style::{Color, Modifier, Style};
use ratatui::text::{Line, Span};
use std::time::{Duration, Instant};
use unicode_width::{UnicodeWidthChar, UnicodeWidthStr};

/// Rows the prompt may take before it scrolls within itself.
const MAX_INPUT_ROWS: usize = 8;
/// Rows of an unfinished line shown while it streams.
const MAX_LIVE_ROWS: usize = 3;

pub fn dim() -> Style {
    Style::new().add_modifier(Modifier::DIM)
}

fn bold() -> Style {
    Style::new().add_modifier(Modifier::BOLD)
}

fn red() -> Style {
    Style::new().fg(Color::Red)
}

fn yellow() -> Style {
    Style::new().fg(Color::Yellow)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Overlay {
    None,
    Keys,
    Details,
}

/// The item streaming now.
#[derive(Debug)]
struct Live {
    id: String,
    kind: LiveKind,
    /// For text: what has arrived since the last complete line.
    tail: String,
    /// Whether any of it has been committed yet.
    committed: bool,
}

#[derive(Debug, PartialEq)]
enum LiveKind {
    Text,
    Reasoning,
    Call(String),
    Result,
}

/// A turn in flight.
#[derive(Debug)]
pub struct Turn {
    pub started: Instant,
    /// An interrupt was asked for; `interrupt_sent` once the host took it.
    pub want_interrupt: bool,
    pub interrupt_sent: bool,
    /// A tool is running: silence is the tool's, not the network's.
    pub tool_running: bool,
    /// Whether the session's first frame has arrived: steering needs its id.
    pub prompt_seen: bool,
}

pub struct App {
    pub editor: Editor,
    pending: Vec<Line<'static>>,
    width: u16,
    last_blank: bool,
    live: Option<Live>,
    pub turn: Option<Turn>,
    pub session_id: Option<String>,
    pub model: Option<ModelRef>,
    /// Session totals: this run's turns plus a resumed session's past ones.
    cost: f64,
    unpriced: bool,
    usage: Usage,
    turns: u32,
    /// Some(target) while the API cannot be reached.
    pub offline: Option<String>,
    /// Whether connectivity has been established at all yet.
    pub online_known: bool,
    pub overlay: Overlay,
    settings: Settings,
    /// Steering the host has queued and the engine not yet taken, oldest
    /// first; each leaves when its item comes back in the log.
    pub steers: Vec<String>,
    /// Steering typed before the turn could take it; sent when it can.
    pub unsent_steers: Vec<String>,
    pub permission_mode: String,
    pub log_dir: Option<String>,
    pub quit: bool,
    dirty: bool,
    pricer: Option<Pricer>,
    /// The provider and model of the turn being replayed, for pricing.
    replay_model: Option<(String, String)>,
}

impl App {
    pub fn new(editor: Editor, width: u16, settings: Settings, model: Option<ModelRef>, pricer: Option<Pricer>) -> App {
        App {
            editor,
            pending: Vec::new(),
            width,
            last_blank: true,
            live: None,
            turn: None,
            session_id: None,
            model,
            cost: 0.0,
            unpriced: false,
            usage: Usage::default(),
            turns: 0,
            offline: None,
            online_known: false,
            overlay: Overlay::None,
            settings,
            steers: Vec::new(),
            unsent_steers: Vec::new(),
            permission_mode: "default".into(),
            log_dir: None,
            quit: false,
            dirty: true,
            pricer,
            replay_model: None,
        }
    }

    pub fn set_width(&mut self, w: u16) {
        self.width = w.max(1);
        self.dirty = true;
    }

    pub fn touch(&mut self) {
        self.dirty = true;
    }

    /// Whether a redraw is owed, and clears it.
    pub fn take_dirty(&mut self) -> bool {
        std::mem::take(&mut self.dirty)
    }

    /// The lines owed to scrollback, oldest first.
    pub fn take_pending(&mut self) -> Vec<Line<'static>> {
        std::mem::take(&mut self.pending)
    }

    pub fn running(&self) -> bool {
        self.turn.is_some()
    }

    /// Whether the engine is waiting on the model rather than a tool: the
    /// silence a stalled connection makes.
    pub fn waiting_on_model(&self) -> bool {
        self.turn.as_ref().is_some_and(|t| !t.tool_running)
    }

    // ---- scrollback --------------------------------------------------------

    /// A line of plain text, wrapped to the width, into scrollback.
    pub fn say(&mut self, text: &str, style: Style) {
        self.push_wrapped("", "", text, style, style);
    }

    /// A blank line before a new block, unless there is one already.
    fn gap(&mut self) {
        if !self.last_blank {
            self.pending.push(Line::default());
            self.last_blank = true;
        }
    }

    fn push_wrapped(&mut self, first: &str, rest: &str, text: &str, prefix_style: Style, style: Style) {
        let width = usize::from(self.width);
        for (i, row) in wrap(&clean(text), width.saturating_sub(first.width().max(rest.width())).max(1)).into_iter().enumerate() {
            let prefix = if i == 0 { first } else { rest };
            let line = if prefix.is_empty() { Line::from(Span::styled(row, style)) } else { Line::from(vec![Span::styled(prefix.to_string(), prefix_style), Span::styled(row, style)]) };
            self.last_blank = line.width() == 0;
            self.pending.push(line);
        }
        self.dirty = true;
    }

    pub fn notice(&mut self, text: &str) {
        self.gap();
        self.push_wrapped("! ", "  ", text, yellow(), yellow());
    }

    pub fn error(&mut self, e: &ErrorInfo) {
        self.gap();
        self.push_wrapped("✗ ", "  ", &e.message, red(), red());
        self.push_wrapped("  ", "  ", &format!("({})", e.code), dim(), dim());
    }

    // ---- the protocol --------------------------------------------------------

    /// One frame of the stream.
    pub fn on_line(&mut self, line: &StreamLine) {
        match line {
            StreamLine::Log(ev) => self.on_log(ev, true),
            StreamLine::Live(LiveEvent::ItemStarted { item_id, item, .. }) => {
                if let Some(t) = &mut self.turn {
                    t.tool_running = matches!(item, ItemKind::ToolResult { .. });
                }
                let kind = match item {
                    ItemKind::AssistantText => LiveKind::Text,
                    ItemKind::Reasoning => LiveKind::Reasoning,
                    ItemKind::ToolCall { name, .. } => LiveKind::Call(name.clone()),
                    ItemKind::ToolResult { .. } => LiveKind::Result,
                    ItemKind::UserText => return,
                };
                self.live = Some(Live { id: item_id.clone(), kind, tail: String::new(), committed: false });
                self.dirty = true;
            }
            StreamLine::Live(LiveEvent::ItemDelta { item_id, delta: Delta::Text { text }, .. }) => self.on_text(item_id, text),
            StreamLine::Live(LiveEvent::ItemDelta { .. }) => {}
            StreamLine::Live(LiveEvent::Result(r)) => self.on_result(r),
        }
    }

    fn on_text(&mut self, item_id: &str, text: &str) {
        let Some(live) = self.live.as_mut().filter(|l| l.id == item_id) else { return };
        match live.kind {
            LiveKind::Text => {
                live.tail.push_str(text);
                // Each line that is now whole goes to scrollback, once.
                if let Some(end) = live.tail.rfind('\n') {
                    let done: String = live.tail.drain(..=end).collect();
                    let first = !live.committed;
                    live.committed = true;
                    if first {
                        self.gap();
                    }
                    for l in done.trim_end_matches('\n').split('\n') {
                        self.push_wrapped("", "", l, Style::new(), Style::new());
                    }
                }
            }
            // Reasoning is shown while it streams, as its last line only.
            LiveKind::Reasoning => {
                live.tail.push_str(text);
                if let Some(end) = live.tail.rfind('\n') {
                    live.tail.drain(..=end);
                }
            }
            _ => {}
        }
        self.dirty = true;
    }

    /// A logged event. `live` is false while replaying a resumed session,
    /// when nothing streamed first.
    pub fn on_log(&mut self, ev: &LogEvent, live: bool) {
        match &ev.body {
            LogBody::SessionStarted { .. } => {
                self.session_id = Some(ev.session_id.clone());
            }
            LogBody::TurnStarted { model, provider, permission_mode, .. } => {
                self.session_id = Some(ev.session_id.clone());
                self.model = Some(model.clone());
                self.replay_model = Some((provider.clone(), model.model.clone()));
                self.permission_mode = serde_json::to_value(permission_mode).ok().and_then(|v| v.as_str().map(String::from)).unwrap_or_default();
                if let Some(t) = &mut self.turn {
                    t.prompt_seen = true;
                }
            }
            LogBody::ItemCompleted { item_id, item, .. } => self.on_item(item_id, item, live),
            LogBody::ResponseCompleted { usage, model, .. } => {
                self.usage += *usage;
                // A live turn's cost arrives with its result; a replayed
                // one is priced here, the way the host priced it.
                if !live {
                    let priced = match (&self.pricer, &self.replay_model) {
                        (Some(p), Some((provider, asked))) => p(provider, asked, usage).or_else(|| p(provider, model, usage)),
                        _ => None,
                    };
                    match priced {
                        Some(usd) => self.cost += usd,
                        None => self.unpriced = true,
                    }
                }
            }
            LogBody::TurnCompleted { status, usage, duration_ms, error, .. } => {
                self.turns += 1;
                self.finish_live();
                match status {
                    TurnStatus::Completed => {
                        let secs = *duration_ms as f64 / 1000.0;
                        self.push_wrapped("  ", "  ", &format!("{secs:.1}s · {} tokens", tokens(usage.total())), dim(), dim());
                    }
                    TurnStatus::Interrupted => {
                        self.push_wrapped("  ", "  ", "⎿ interrupted — what arrived is kept", yellow(), yellow());
                    }
                    TurnStatus::Failed => {
                        if let Some(e) = error {
                            self.error(e);
                        }
                    }
                }
            }
        }
        self.dirty = true;
    }

    fn on_item(&mut self, item_id: &str, item: &Item, live: bool) {
        let streamed = self.live.as_ref().is_some_and(|l| l.id == item_id);
        match item {
            Item::UserText { text } => {
                if let Some(i) = self.steers.iter().position(|s| s == text) {
                    self.steers.remove(i);
                }
                self.finish_live();
                self.gap();
                self.push_wrapped("› ", "  ", text, bold().fg(Color::Cyan), bold());
            }
            Item::AssistantText { text } => {
                if streamed && live {
                    self.finish_live();
                } else if !text.is_empty() {
                    self.gap();
                    for l in text.split('\n') {
                        self.push_wrapped("", "", l, Style::new(), Style::new());
                    }
                }
                self.live = None;
            }
            Item::Reasoning { .. } => {
                if streamed {
                    self.live = None;
                }
            }
            Item::ToolCall { name, input, .. } => {
                if streamed {
                    self.live = None;
                }
                self.gap();
                let what = call_summary(input);
                let text = if what.is_empty() { name.clone() } else { format!("{name} {what}") };
                let width = usize::from(self.width).saturating_sub(2);
                self.push_wrapped("● ", "  ", &clip(&text, width), Style::new().fg(Color::Green), bold());
            }
            Item::ToolResult { output, is_error, .. } => {
                if streamed {
                    self.live = None;
                }
                if let Some(t) = &mut self.turn {
                    t.tool_running = false;
                }
                let lines = output.lines().count();
                let first = output.lines().find(|l| !l.trim().is_empty()).unwrap_or("(no output)").trim();
                let more = if lines > 1 { format!(" … +{} lines", lines - 1) } else { String::new() };
                let width = usize::from(self.width).saturating_sub(4 + more.width());
                let style = if *is_error { red() } else { dim() };
                self.push_wrapped("  ⎿ ", "    ", &(clip(first, width) + &more), style, style);
            }
        }
    }

    /// Whatever of the streaming text never ended in a newline goes to
    /// scrollback now, and the live item is done.
    fn finish_live(&mut self) {
        let Some(live) = self.live.take() else { return };
        if live.kind == LiveKind::Text && !live.tail.is_empty() {
            if !live.committed {
                self.gap();
            }
            for l in live.tail.split('\n') {
                self.push_wrapped("", "", l, Style::new(), Style::new());
            }
        }
    }

    fn on_result(&mut self, r: &RunResult) {
        self.session_id = Some(r.session_id.clone());
        match r.cost_usd {
            Some(usd) => self.cost += usd,
            None => self.unpriced = true,
        }
        self.dirty = true;
    }

    /// A resumed session's branch, into scrollback before the first prompt.
    pub fn replay(&mut self, branch: &[&LogEvent]) {
        for ev in branch {
            self.on_log(ev, false);
        }
    }

    // ---- the live region -------------------------------------------------------

    /// The live region's rows, and where the caret goes among them.
    pub fn view(&self, now: Instant) -> (Vec<Line<'static>>, (u16, u16)) {
        let width = usize::from(self.width.max(1));
        let mut rows: Vec<Line<'static>> = Vec::new();
        if let Some(live) = &self.live {
            match &live.kind {
                LiveKind::Text if !live.tail.is_empty() => {
                    let wrapped = wrap(&clean(&live.tail), width);
                    let skip = wrapped.len().saturating_sub(MAX_LIVE_ROWS);
                    rows.extend(wrapped.into_iter().skip(skip).map(Line::from));
                }
                LiveKind::Text => {}
                LiveKind::Reasoning => {
                    let tail = clean(live.tail.trim());
                    let text = if tail.is_empty() { "thinking…".to_string() } else { format!("thinking… {tail}") };
                    rows.push(Line::from(Span::styled(clip(&text, width), dim().add_modifier(Modifier::ITALIC))));
                }
                LiveKind::Call(name) => rows.push(Line::from(Span::styled(clip(&format!("● {name} …"), width), dim()))),
                LiveKind::Result => rows.push(Line::from(Span::styled(clip("  ⎿ running…", width), dim()))),
            }
        }
        if let Some(t) = &self.turn {
            let secs = now.saturating_duration_since(t.started).as_secs();
            let what = if t.want_interrupt { "interrupting…".to_string() } else { format!("working {secs}s · esc to interrupt") };
            rows.push(Line::from(Span::styled(clip(&what, width), dim())));
            for s in self.steers.iter().chain(&self.unsent_steers) {
                let first = s.lines().next().unwrap_or_default();
                rows.push(Line::from(Span::styled(clip(&format!("↳ steer queued: {first}"), width), dim())));
            }
        }
        if let Some(target) = &self.offline {
            let text = format!("⚠ no network connectivity — {target} cannot be reached; krowk keeps retrying");
            for row in wrap(&text, width) {
                rows.push(Line::from(Span::styled(row, yellow().add_modifier(Modifier::BOLD))));
            }
        }
        match self.overlay {
            Overlay::None => {}
            Overlay::Keys => rows.extend(self.keys_overlay(width)),
            Overlay::Details => rows.extend(self.details_overlay(width)),
        }
        // The prompt, scrolled to keep the caret in view.
        let (input, (crow, ccol)) = self.editor.layout(self.width.saturating_sub(2).max(1));
        let first = (crow as usize + 1).saturating_sub(MAX_INPUT_ROWS);
        let top = rows.len() as u16;
        for (i, row) in input.iter().enumerate().skip(first).take(MAX_INPUT_ROWS) {
            let prefix = if i == 0 { Span::styled("› ", bold().fg(Color::Cyan)) } else { Span::raw("  ") };
            if i == 0 && self.editor.is_empty() {
                let hint = if self.running() { "type to steer the running turn" } else { "ask anything · ? for keys · ctrl-d to quit" };
                rows.push(Line::from(vec![prefix, Span::styled(clip(hint, width.saturating_sub(2)), dim())]));
            } else {
                rows.push(Line::from(vec![prefix, Span::raw(row.clone())]));
            }
        }
        let caret = (ccol + 2, top + (crow as usize - first) as u16);
        if self.settings.status_bar {
            let bar = self.status_bar();
            if !bar.is_empty() {
                rows.push(Line::from(Span::styled(clip(&bar, width), dim())));
            }
        }
        (rows, caret)
    }

    pub fn status_bar(&self) -> String {
        let mut parts: Vec<String> = Vec::new();
        for item in &self.settings.status_items {
            match item {
                StatusItem::Model => {
                    if let Some(m) = &self.model {
                        parts.push(m.model.clone());
                    }
                }
                StatusItem::Instance => {
                    if let Some(m) = &self.model {
                        parts.push(format!("{} · api key", m.instance));
                    }
                }
                StatusItem::Cost => parts.push(if self.unpriced && self.cost == 0.0 { "$—".into() } else { format!("${:.4}", self.cost) }),
                StatusItem::Connectivity => parts.push(
                    match (&self.offline, self.online_known) {
                        (Some(_), _) => "offline",
                        (None, true) => "online",
                        (None, false) => "connecting…",
                    }
                    .into(),
                ),
                StatusItem::Session => {
                    if let Some(id) = &self.session_id {
                        parts.push(id.chars().take(8).collect());
                    }
                }
            }
        }
        parts.join(" · ")
    }

    fn keys_overlay(&self, width: usize) -> Vec<Line<'static>> {
        [
            "enter send · alt-enter, ctrl-j or \\ then enter: new line",
            "↑ ↓ lines, then history · ctrl-a/e line start/end · ctrl-u/k/w kill",
            "esc or ctrl-c interrupt · type while it runs to steer",
            "ctrl-o session details · ctrl-d or /exit quit · ? or esc closes this",
        ]
        .iter()
        .map(|l| Line::from(Span::styled(clip(l, width), Style::new().fg(Color::Blue))))
        .collect()
    }

    fn details_overlay(&self, width: usize) -> Vec<Line<'static>> {
        let u = &self.usage;
        let mut lines = vec![
            format!("session {}", self.session_id.as_deref().unwrap_or("(new — starts with the first prompt)")),
            format!("{} turns · permission mode {}", self.turns, self.permission_mode),
            format!(
                "tokens: {} in · {} out · {} cache read · {} cache write · {} reasoning",
                tokens(u.input_tokens),
                tokens(u.output_tokens),
                tokens(u.cache_read_tokens),
                tokens(u.cache_write_tokens),
                tokens(u.reasoning_tokens)
            ),
        ];
        if let (Some(dir), Some(id)) = (&self.log_dir, &self.session_id) {
            lines.push(format!("log {dir}/{id}/events.jsonl"));
        }
        lines.into_iter().flat_map(|l| wrap(&l, width)).map(|l| Line::from(Span::styled(l, Style::new().fg(Color::Blue)))).collect()
    }

    /// A turn has started: `Command::Prompt` is on its way.
    pub fn start_turn(&mut self, now: Instant) {
        self.turn = Some(Turn { started: now, want_interrupt: false, interrupt_sent: false, tool_running: false, prompt_seen: false });
        self.dirty = true;
    }

    /// The turn is over. Steering it never took is returned, to be sent as
    /// the next prompt rather than lost.
    pub fn end_turn(&mut self) -> Vec<String> {
        self.turn = None;
        self.finish_live();
        self.dirty = true;
        let mut left = std::mem::take(&mut self.steers);
        left.append(&mut self.unsent_steers);
        left
    }

    pub fn set_offline(&mut self, target: String) {
        if self.offline.as_deref() != Some(target.as_str()) {
            self.offline = Some(target);
            self.dirty = true;
        }
    }

    pub fn set_online(&mut self) {
        if self.offline.is_some() || !self.online_known {
            self.offline = None;
            self.online_known = true;
            self.dirty = true;
        }
    }
}

/// Model output is shown, never obeyed: tabs become spaces, and every other
/// control character — an escape sequence above all — is dropped.
pub fn clean(s: &str) -> String {
    s.chars().filter_map(|c| if c == '\t' { Some(' ') } else if c == '\n' || !c.is_control() { Some(c) } else { None }).collect()
}

/// `s` as rows at most `width` columns wide: a break at the last space
/// that fits, else mid-word. Newlines are the caller's.
pub fn wrap(s: &str, width: usize) -> Vec<String> {
    let width = width.max(1);
    let mut rows = Vec::new();
    for line in s.split('\n') {
        let mut rest = line;
        loop {
            if rest.width() <= width {
                rows.push(rest.to_string());
                break;
            }
            let mut cols = 0;
            let mut cut = 0;
            let mut space = None;
            for (i, c) in rest.char_indices() {
                let w = c.width().unwrap_or(0);
                if cols + w > width {
                    break;
                }
                cols += w;
                cut = i + c.len_utf8();
                if c == ' ' {
                    space = Some(cut);
                }
            }
            let cut = match space {
                Some(sp) if sp > 0 => sp,
                _ if cut == 0 => rest.chars().next().map_or(rest.len(), char::len_utf8),
                _ => cut,
            };
            rows.push(rest[..cut].to_string());
            rest = &rest[cut..];
        }
    }
    rows
}

/// `s` cut to `width` columns, with an ellipsis when it was longer.
pub fn clip(s: &str, width: usize) -> String {
    let s = clean(s).replace('\n', " ");
    if s.width() <= width {
        return s;
    }
    let mut out = String::new();
    let mut cols = 0;
    for c in s.chars() {
        let w = c.width().unwrap_or(0);
        if cols + w + 1 > width {
            break;
        }
        out.push(c);
        cols += w;
    }
    out.push('…');
    out
}

/// What a tool call is about, in a few words: its command or its path.
fn call_summary(input: &serde_json::Value) -> String {
    for key in ["command", "path", "file_path", "pattern", "url"] {
        if let Some(s) = input.get(key).and_then(|v| v.as_str()) {
            return s.to_string();
        }
    }
    match input {
        serde_json::Value::Object(m) if m.is_empty() => String::new(),
        v => v.to_string(),
    }
}

fn tokens(n: i64) -> String {
    match n {
        n if n >= 1_000_000 => format!("{:.1}M", n as f64 / 1e6),
        n if n >= 10_000 => format!("{:.0}k", n as f64 / 1e3),
        n if n >= 1_000 => format!("{:.1}k", n as f64 / 1e3),
        n => n.to_string(),
    }
}

/// How long the turn has run, redrawn once a second while it does.
pub const TICK: Duration = Duration::from_secs(1);

#[cfg(test)]
mod tests {
    use super::*;
    use krowk_harness::protocol::PermissionMode;

    fn app() -> App {
        App::new(Editor::new(None), 40, Settings::default(), Some(ModelRef { instance: "anthropic".into(), model: "claude-x".into() }), None)
    }

    fn text(lines: &[Line]) -> Vec<String> {
        lines.iter().map(|l| l.spans.iter().map(|s| s.content.as_ref()).collect::<String>()).collect()
    }

    fn live(ev: LiveEvent) -> StreamLine {
        StreamLine::Live(ev)
    }

    fn log(body: LogBody) -> StreamLine {
        StreamLine::Log(LogEvent { id: "e".into(), parent_id: None, session_id: "s".into(), time_ms: 0, body })
    }

    fn delta(id: &str, t: &str) -> StreamLine {
        live(LiveEvent::ItemDelta { session_id: "s".into(), turn_id: "t".into(), item_id: id.into(), delta: Delta::Text { text: t.into() } })
    }

    #[test]
    fn r_perf_4_a_streamed_answer_reaches_scrollback_a_line_at_a_time_and_once() {
        let mut a = app();
        a.start_turn(Instant::now());
        a.on_line(&live(LiveEvent::ItemStarted { session_id: "s".into(), turn_id: "t".into(), item_id: "i".into(), item: ItemKind::AssistantText }));
        a.on_line(&delta("i", "first li"));
        assert!(a.take_pending().is_empty(), "nothing is committed before its line ends");
        let (rows, _) = a.view(Instant::now());
        assert_eq!(text(&rows)[0], "first li", "the unfinished line is live");
        a.on_line(&delta("i", "ne\nsecond\nthi"));
        assert_eq!(text(&a.take_pending()), ["first line", "second"]);
        a.on_line(&delta("i", "rd"));
        a.on_line(&log(LogBody::ItemCompleted { turn_id: "t".into(), item_id: "i".into(), item: Item::AssistantText { text: "first line\nsecond\nthird".into() } }));
        assert_eq!(text(&a.take_pending()), ["third"], "the tail, and nothing twice");
    }

    #[test]
    fn a_replayed_session_prints_its_conversation() {
        let mut a = app();
        let ev = |body| LogEvent { id: "e".into(), parent_id: None, session_id: "s".into(), time_ms: 0, body };
        let model = ModelRef { instance: "anthropic".into(), model: "claude-y".into() };
        let evs = [
            ev(LogBody::TurnStarted { turn_id: "t".into(), model: model.clone(), provider: "anthropic".into(), wire_api: krowk_harness::protocol::WireApi::AnthropicMessages, permission_mode: PermissionMode::Default }),
            ev(LogBody::ItemCompleted { turn_id: "t".into(), item_id: "1".into(), item: Item::UserText { text: "hi".into() } }),
            ev(LogBody::ItemCompleted { turn_id: "t".into(), item_id: "2".into(), item: Item::ToolCall { call_id: "c".into(), name: "read".into(), input: serde_json::json!({"path": "README.md"}) } }),
            ev(LogBody::ItemCompleted { turn_id: "t".into(), item_id: "3".into(), item: Item::ToolResult { call_id: "c".into(), output: "# krowk\nmore\n".into(), is_error: false } }),
            ev(LogBody::ItemCompleted { turn_id: "t".into(), item_id: "4".into(), item: Item::AssistantText { text: "It is a CLI.".into() } }),
            ev(LogBody::TurnCompleted { turn_id: "t".into(), status: TurnStatus::Completed, usage: Usage { input_tokens: 1200, ..Usage::default() }, duration_ms: 1500, error: None }),
        ];
        a.replay(&evs.iter().collect::<Vec<_>>());
        assert_eq!(text(&a.take_pending()), ["› hi", "", "● read README.md", "  ⎿ # krowk … +1 lines", "", "It is a CLI.", "  1.5s · 1.2k tokens"]);
        assert_eq!(a.model, Some(model), "the session's model is the one shown");
    }

    #[test]
    fn r_off_1_the_notice_is_persistent_in_the_live_region() {
        let mut a = app();
        a.set_offline("api.anthropic.com:443".into());
        let (rows, _) = a.view(Instant::now());
        let all = text(&rows).join("\n");
        assert!(all.contains("no network connectivity"), "{all}");
        assert!(a.status_bar().contains("offline"));
        a.set_online();
        let (rows, _) = a.view(Instant::now());
        assert!(!text(&rows).join("\n").contains("no network"));
        assert!(a.status_bar().contains("online"));
    }

    #[test]
    fn r_tui_2_the_status_bar_follows_its_settings() {
        let mut a = app();
        assert_eq!(a.status_bar(), "claude-x · anthropic · api key · $0.0000 · connecting…");
        a.settings = Settings { status_bar: true, status_items: vec![StatusItem::Cost] };
        assert_eq!(a.status_bar(), "$0.0000");
        a.settings.status_bar = false;
        let (rows, _) = a.view(Instant::now());
        assert_eq!(rows.len(), 1, "only the prompt: {:?}", text(&rows));
        a.overlay = Overlay::Keys;
        let (rows, caret) = a.view(Instant::now());
        assert_eq!(rows.len(), 5, "the overlay is four rows over the prompt");
        assert_eq!(caret, (2, 4));
    }

    #[test]
    fn steering_left_untaken_comes_back_for_the_next_prompt() {
        let mut a = app();
        a.start_turn(Instant::now());
        a.steers.push("also check the tests".into());
        a.steers.push("and the docs".into());
        a.on_line(&log(LogBody::ItemCompleted { turn_id: "t".into(), item_id: "x".into(), item: Item::UserText { text: "also check the tests".into() } }));
        assert_eq!(a.end_turn(), ["and the docs"]);
    }

    #[test]
    fn output_is_cleaned_and_wrapped() {
        assert_eq!(clean("a\x1b[2Jb\tc"), "a[2Jb c");
        assert_eq!(wrap("the quick brown fox", 10), ["the quick ", "brown fox"]);
        assert_eq!(wrap("abcdefghijkl", 5), ["abcde", "fghij", "kl"]);
        assert_eq!(wrap("", 5), [""]);
        assert_eq!(clip("hello world", 6), "hello…");
    }
}
