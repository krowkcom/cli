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
use crate::look::{self, SEP};
use crate::settings::{Item as StatusItem, Settings};
use krowk_harness::host::Pricer;
use krowk_harness::protocol::{ApprovalRequest, Billing, Delta, ErrorInfo, Item, ItemKind, LiveEvent, LogBody, LogEvent, ModelRef, RunResult, StreamLine, TurnStatus, Usage};
use ratatui::style::{Color, Modifier, Style};
use ratatui::text::{Line, Span};
use std::time::{Duration, Instant};
use unicode_width::{UnicodeWidthChar, UnicodeWidthStr};

/// Rows the prompt may take before it scrolls within itself.
const MAX_INPUT_ROWS: usize = 8;
/// Rows of an unfinished line shown while it streams.
const MAX_LIVE_ROWS: usize = 3;

pub use look::dim;
use look::{bold, error as red, warning as yellow};

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

/// A tool call the model made whose result has not come back yet: it is
/// shown once, with its outcome, when it does.
#[derive(Debug)]
struct Call {
    call_id: String,
    name: String,
    input: serde_json::Value,
    started: Instant,
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
    /// The host's own figure for the session and its subagents, from the
    /// last `cost` frame of the running turn (R-BUDGET-2): shown instead of
    /// adding the turn's result on top.
    costed: bool,
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
    /// Tool calls waiting for their results, oldest first.
    calls: Vec<Call>,
    /// Whether the answer being shown has a fenced code block open.
    fence: bool,
    /// When the reasoning streaming now began.
    thinking_since: Option<Instant>,
    /// What a backend reported its session is billed to, and on which
    /// instance (R-INST-3).
    billing: Option<(String, Billing)>,
    /// The instances that run a vendor's backend: their billing is the
    /// vendor's to report, so none is assumed before it has.
    pub vendor_instances: Vec<String>,
    /// Tool calls waiting for the person's say, oldest first (R-PERM-2):
    /// the first is shown over the prompt until it is answered, here or by
    /// another client.
    pub approvals: Vec<ApprovalRequest>,
    /// When the approval shown now came up: keys typed in the moment
    /// before are not taken as its answer.
    pub approval_shown: Option<Instant>,
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
            costed: false,
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
            calls: Vec::new(),
            fence: false,
            thinking_since: None,
            billing: None,
            vendor_instances: Vec::new(),
            approvals: Vec::new(),
            approval_shown: None,
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
        // Unprefixed text — the answer itself, most of scrollback — goes
        // out as one line for the terminal to wrap, so it rewraps when the
        // window does and copies whole. Prefixed items wrap here, under
        // their hanging indent.
        if first.is_empty() && rest.is_empty() {
            let line = Line::from(Span::styled(clean(text), style));
            self.last_blank = line.width() == 0;
            self.pending.push(line);
            self.dirty = true;
            return;
        }
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

    /// A failed request or turn: the headline in the warning colour, which
    /// names the next action, and its code dim.
    pub fn error(&mut self, e: &ErrorInfo) {
        self.gap();
        self.push_wrapped(look::WARN, "  ", &e.message, yellow(), yellow());
        self.push_wrapped("  ", "  ", &format!("({})", e.code), dim(), dim());
    }

    /// One line of an answer, in light markdown, for the terminal to wrap.
    fn push_md(&mut self, text: &str) {
        let line = look::markdown_line(&clean(text), &mut self.fence);
        self.last_blank = line.width() == 0;
        self.pending.push(line);
        self.dirty = true;
    }

    /// A tool call and what came of it, as one block: `◆ Verb arg (detail)`
    /// — the bullet green, or red when it failed — then what is worth
    /// seeing of its result: a failure's first lines, a command's output
    /// cut to its head and tail, an edit's lines as removed and added.
    fn commit_tool(&mut self, name: &str, input: &serde_json::Value, output: &str, is_error: bool) {
        self.gap();
        let width = usize::from(self.width.max(8));
        let (verb, arg) = look::tool_title(name, input);
        let lines: Vec<&str> = output.lines().collect();
        let edit = if is_error { None } else { look::edit_lines(name, input) };
        let mut head = vec![Span::styled(look::TOOL, if is_error { red() } else { look::success() }), Span::styled(verb.clone(), bold())];
        if !arg.is_empty() {
            head.push(Span::raw(" "));
            head.push(Span::styled(clip(&arg, width.saturating_sub(verb.width() + 14)), look::path()));
        }
        match (&edit, name, is_error) {
            (_, _, true) => head.push(Span::styled(" (failed)", red())),
            (Some((del, add)), _, _) => {
                head.push(Span::styled(format!(" +{}", add.len()), look::success()));
                head.push(Span::styled(format!("/-{}", del.len()), red()));
            }
            (None, "read" | "grep" | "glob" | "write", _) => head.push(Span::styled(format!(" ({} lines)", lines.len()), dim())),
            _ => {}
        }
        self.push_line(Line::from(head));
        let body_width = width.saturating_sub(2);
        if is_error {
            for l in lines.iter().filter(|l| !l.trim().is_empty()).take(3) {
                self.push_line(Line::from(vec![Span::raw("  "), Span::styled(clip(l, body_width), red())]));
            }
            return;
        }
        if let Some((del, add)) = edit {
            const SHOWN: usize = 8;
            for (rows, band) in [(del, look::delete_band()), (add, look::insert_band())] {
                for l in rows.iter().take(SHOWN) {
                    self.push_line(Line::from(vec![Span::raw("  "), Span::styled(clip(l, body_width), band)]));
                }
                if rows.len() > SHOWN {
                    self.push_line(Line::from(Span::styled(format!("  … +{} lines", rows.len() - SHOWN), dim())));
                }
            }
            return;
        }
        if name == "bash" {
            let shown: Vec<&str> = if lines.len() <= 5 { lines.clone() } else { lines[..2].iter().chain(&lines[lines.len() - 3..]).copied().collect() };
            for (i, l) in shown.iter().enumerate() {
                if lines.len() > 5 && i == 2 {
                    self.push_line(Line::from(Span::styled(format!("  … +{} lines", lines.len() - 5), dim())));
                }
                self.push_line(Line::from(vec![Span::raw("  "), Span::styled(clip(l, body_width), dim())]));
            }
        }
    }

    fn push_line(&mut self, line: Line<'static>) {
        self.last_blank = line.width() == 0;
        self.pending.push(line);
        self.dirty = true;
    }

    /// Calls still waiting when a turn ends are shown as they stand.
    fn flush_calls(&mut self) {
        for c in std::mem::take(&mut self.calls) {
            self.commit_tool(&c.name, &c.input, "no result — the turn stopped first", true);
        }
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
                    ItemKind::AssistantText => {
                        self.fence = false;
                        LiveKind::Text
                    }
                    ItemKind::Reasoning => {
                        self.thinking_since = Some(Instant::now());
                        LiveKind::Reasoning
                    }
                    ItemKind::ToolCall { name, .. } => LiveKind::Call(name.clone()),
                    ItemKind::ToolResult { .. } => LiveKind::Result,
                    ItemKind::UserText => return,
                };
                self.live = Some(Live { id: item_id.clone(), kind, tail: String::new(), committed: false });
                self.dirty = true;
            }
            StreamLine::Live(LiveEvent::ItemDelta { item_id, delta: Delta::Text { text }, .. }) => self.on_text(item_id, text),
            StreamLine::Live(LiveEvent::ItemDelta { .. }) => {}
            // The session's spend after each metered call, subagents
            // included, priced by the host: the status bar shows it as is.
            StreamLine::Live(LiveEvent::Cost { cost_usd, .. }) => {
                match cost_usd {
                    Some(usd) => {
                        self.cost = *usd;
                        self.unpriced = false;
                    }
                    None => self.unpriced = true,
                }
                self.costed = true;
                self.dirty = true;
            }
            // For the person alone (a claim command): shown, never logged.
            StreamLine::Live(LiveEvent::Notice { text, .. }) => self.notice(text),
            StreamLine::Live(LiveEvent::ApprovalRequested(req)) => {
                if self.approvals.is_empty() {
                    self.approval_shown = Some(Instant::now());
                }
                self.approvals.push(req.clone());
                self.dirty = true;
            }
            StreamLine::Live(LiveEvent::ApprovalResolved { request_id, .. }) => self.answered(request_id),
            StreamLine::Live(LiveEvent::Result(r)) => {
                self.approvals.clear();
                self.on_result(r);
            }
        }
    }

    /// A request was answered, here or elsewhere: the next one, if any,
    /// comes up with its own moment before keys answer it.
    pub fn answered(&mut self, request_id: &str) {
        let head = self.approvals.first().is_some_and(|r| r.request_id == request_id);
        self.approvals.retain(|r| r.request_id != request_id);
        if head {
            self.approval_shown = (!self.approvals.is_empty()).then(Instant::now);
        }
        self.dirty = true;
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
                        self.push_md(l);
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
            LogBody::BackendSession { billing, .. } => {
                if let (Some(b), Some(m)) = (billing, &self.model) {
                    self.billing = Some((m.instance.clone(), *b));
                    self.dirty = true;
                }
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
            LogBody::ResponseCompleted { usage, model, .. } | LogBody::SubagentResponse { usage, model, .. } => {
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
            // The run the session's evidence goes under: the log's to keep.
            LogBody::RunOpened { .. } => {}
            LogBody::TurnCompleted { status, usage, duration_ms, error, .. } => {
                self.turns += 1;
                self.finish_live();
                self.flush_calls();
                match status {
                    TurnStatus::Completed => {
                        let took = look::duration(Duration::from_millis(*duration_ms));
                        self.push_wrapped("", "", &format!("Worked for {took} · {} tokens", tokens(usage.total())), dim(), dim());
                    }
                    TurnStatus::Interrupted => {
                        let took = look::duration(Duration::from_millis(*duration_ms));
                        self.push_wrapped(look::STOPPED, "  ", &format!("interrupted after {took} — what arrived is kept"), yellow(), yellow());
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
                self.push_wrapped(look::PROMPT, "  ", text, look::prompt(), bold());
            }
            Item::AssistantText { text } => {
                if streamed && live {
                    self.finish_live();
                } else if !text.is_empty() {
                    self.gap();
                    self.fence = false;
                    for l in text.split('\n') {
                        self.push_md(l);
                    }
                }
                self.live = None;
            }
            // Thinking is shown collapsed, as how long it took.
            Item::Reasoning { .. } => {
                if streamed {
                    self.live = None;
                }
                let took = if live { self.thinking_since.take().map(|t| format!(" for {}", look::duration(t.elapsed()))) } else { None };
                self.gap();
                self.push_line(Line::from(vec![
                    Span::styled(look::TOOL, dim()),
                    Span::styled(format!("Thought{}", took.unwrap_or_default()), dim().add_modifier(Modifier::ITALIC)),
                ]));
            }
            Item::ToolCall { call_id, name, input } => {
                if streamed {
                    self.live = None;
                }
                self.calls.push(Call { call_id: call_id.clone(), name: name.clone(), input: input.clone(), started: Instant::now() });
            }
            Item::ToolResult { call_id, output, is_error } => {
                if streamed {
                    self.live = None;
                }
                if let Some(t) = &mut self.turn {
                    t.tool_running = false;
                }
                match self.calls.iter().position(|c| &c.call_id == call_id) {
                    Some(i) => {
                        let c = self.calls.remove(i);
                        self.commit_tool(&c.name, &c.input, output, *is_error);
                    }
                    None => self.commit_tool("tool", &serde_json::Value::Null, output, *is_error),
                }
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
                self.push_md(l);
            }
        }
    }

    fn on_result(&mut self, r: &RunResult) {
        self.session_id = Some(r.session_id.clone());
        // A turn that made a call has already said where the session stands.
        if !std::mem::take(&mut self.costed) {
            match r.cost_usd {
                Some(usd) => self.cost += usd,
                None => self.unpriced = true,
            }
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
                    if !tail.is_empty() {
                        rows.push(Line::from(Span::styled(clip(&format!("  {tail}"), width), dim().add_modifier(Modifier::ITALIC))));
                    }
                }
                LiveKind::Call(name) => rows.push(Line::from(vec![Span::styled(look::TOOL, dim()), Span::styled(clip(name, width.saturating_sub(2)), dim())])),
                LiveKind::Result => {}
            }
        }
        // Calls out, their results not back: each as it will be shown.
        for c in &self.calls {
            let (verb, arg) = look::tool_title(&c.name, &c.input);
            let text = clip(&format!("{verb} {arg}"), width.saturating_sub(2));
            rows.push(Line::from(vec![Span::styled(look::TOOL, look::accent()), Span::styled(text, dim())]));
        }
        if let Some(t) = &self.turn {
            let since = now.saturating_duration_since(t.started);
            let frame = look::SPINNER[(since.as_millis() / look::SPIN_FRAME.as_millis()) as usize % look::SPINNER.len()];
            let label = match (&self.live, t.want_interrupt) {
                (_, true) => "Interrupting…".to_string(),
                (Some(Live { kind: LiveKind::Reasoning, .. }), _) => "Thinking…".to_string(),
                (Some(Live { kind: LiveKind::Text, .. }), _) => "Responding…".to_string(),
                _ if t.tool_running => match self.calls.first() {
                    Some(c) => format!("Running for {}…", look::duration(now.saturating_duration_since(c.started))),
                    None => "Running…".to_string(),
                },
                _ => "Working…".to_string(),
            };
            let label_style = if t.want_interrupt { red() } else { look::accent() };
            let right = format!(" {}{SEP}esc to interrupt", look::duration(since));
            rows.push(Line::from(vec![
                Span::styled(format!("{frame} "), look::accent()),
                Span::styled(clip(&label, width.saturating_sub(right.width() + 2)), label_style),
                Span::styled(clip(&right, width.saturating_sub(label.width() + 2)), dim()),
            ]));
            for s in self.steers.iter().chain(&self.unsent_steers) {
                let first = s.lines().next().unwrap_or_default();
                rows.push(Line::from(vec![Span::styled(look::STEER, look::accent()), Span::styled(clip(&format!("steer queued: {first}"), width.saturating_sub(2)), dim())]));
            }
        }
        if let Some(target) = &self.offline {
            let text = format!("{}no network connectivity — {target} cannot be reached; krowk keeps retrying", look::WARN);
            for row in wrap(&text, width) {
                rows.push(Line::from(Span::styled(row, yellow().add_modifier(Modifier::BOLD))));
            }
        }
        if let Some(req) = self.approvals.first() {
            rows.extend(approval_rows(req, self.approvals.len(), width));
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
            let prefix = if i == 0 { Span::styled(look::PROMPT, look::prompt()) } else { Span::raw("  ") };
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
                // Offline is the one item not dim: it is news.
                let clipped = clip(&bar, width);
                let line = match clipped.find("offline") {
                    Some(i) if self.offline.is_some() => Line::from(vec![
                        Span::styled(clipped[..i].to_string(), dim()),
                        Span::styled("offline".to_string(), yellow()),
                        Span::styled(clipped[i + "offline".len()..].to_string(), dim()),
                    ]),
                    _ => Line::from(Span::styled(clipped, dim())),
                };
                rows.push(line);
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
                // Which instance, and whether it runs on a subscription or
                // an API key: a backend's as it reported it, a native
                // instance's key otherwise.
                StatusItem::Instance => {
                    if let Some(m) = &self.model {
                        match &self.billing {
                            Some((i, b)) if *i == m.instance => parts.push(format!("{} · {}", m.instance, if *b == Billing::Subscription { "subscription" } else { "api key" })),
                            _ if self.vendor_instances.contains(&m.instance) => parts.push(m.instance.clone()),
                            _ => parts.push(format!("{} · api key", m.instance)),
                        }
                    }
                }
                StatusItem::Cost => parts.push(if self.unpriced && self.cost == 0.0 { "$—".into() } else { format!("${:.2}", self.cost) }),
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
        parts.join(SEP)
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
        let (mut left, mut unsent) = self.end_turn_parts();
        left.append(&mut unsent);
        left
    }

    /// The turn is over: the steering the host accepted and this client has
    /// not seen come back in the log, and the steering never sent (the host
    /// refused it, or the turn ended first).
    pub fn end_turn_parts(&mut self) -> (Vec<String>, Vec<String>) {
        self.turn = None;
        self.finish_live();
        self.flush_calls();
        self.dirty = true;
        (std::mem::take(&mut self.steers), std::mem::take(&mut self.unsent_steers))
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

fn tokens(n: i64) -> String {
    match n {
        n if n >= 1_000_000 => format!("{:.1}M", n as f64 / 1e6),
        n if n >= 10_000 => format!("{:.0}k", n as f64 / 1e3),
        n if n >= 1_000 => format!("{:.1}k", n as f64 / 1e3),
        n => n.to_string(),
    }
}

/// The spinner's frame, redrawn while a turn runs (never while idle).
pub const TICK: Duration = look::SPIN_FRAME;

/// An approval request, as it is shown over the prompt: what the call would
/// do, why it is asked, and the keys that answer it — `s` and `p` only when
/// the call can be remembered.
fn approval_rows(req: &ApprovalRequest, waiting: usize, width: usize) -> Vec<Line<'static>> {
    let more = if waiting > 1 { format!(" (1 of {waiting})") } else { String::new() };
    let summary = shown(&req.summary, MAX_APPROVAL_TEXT);
    let mut rows: Vec<Line<'static>> = wrap(&format!("{}allow {summary}?{more}", look::TOOL), width).into_iter().map(|l| Line::from(Span::styled(l, yellow().add_modifier(Modifier::BOLD)))).collect();
    rows.extend(wrap(&format!("  {}", shown(&req.reason, MAX_APPROVAL_TEXT)), width).into_iter().map(|l| Line::from(Span::styled(l, dim()))));
    let keys = if req.remember.is_empty() {
        "  y allow once · n deny".to_string()
    } else {
        format!("  y allow once · s allow {} for this session · p … for this project · n deny", shown(&req.remember.join(", "), MAX_APPROVAL_TEXT / 2))
    };
    rows.push(Line::from(Span::styled(clip(&keys, width), look::accent())));
    rows
}

/// How much of a model-supplied string an approval shows.
const MAX_APPROVAL_TEXT: usize = 400;

/// A string the model supplied, as the approval prompt may show it: on one
/// line (a newline is `⏎`, so a command cannot draw a line of its own that
/// looks like the prompt's keys), without control or formatting characters
/// (escapes, bidi overrides, zero-width marks), and at most `max`
/// characters.
fn shown(s: &str, max: usize) -> String {
    let flat: String = s
        .chars()
        .filter_map(|c| match c {
            '\n' | '\r' => Some('⏎'),
            '\t' => Some(' '),
            c if c.is_control() => None,
            '\u{200B}'..='\u{200F}' | '\u{202A}'..='\u{202E}' | '\u{2060}'..='\u{2069}' | '\u{FEFF}' => None,
            c => Some(c),
        })
        .collect();
    // Cut from the middle: the start says what runs, and the end is where
    // a long command hides what it does last.
    let n = flat.chars().count();
    if n <= max {
        return flat;
    }
    let tail = max * 3 / 10;
    let head = max - tail;
    let chars: Vec<char> = flat.chars().collect();
    format!("{} … {}", chars[..head].iter().collect::<String>(), chars[n - tail..].iter().collect::<String>())
}

#[cfg(test)]
mod tests {
    use super::*;
    use krowk_harness::protocol::{PermissionMode, WireApi};

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
            ev(LogBody::TurnStarted { turn_id: "t".into(), model: model.clone(), provider: "anthropic".into(), wire_api: krowk_harness::protocol::WireApi::AnthropicMessages, permission_mode: PermissionMode::Default, effort: None }),
            ev(LogBody::ItemCompleted { turn_id: "t".into(), item_id: "1".into(), item: Item::UserText { text: "hi".into() } }),
            ev(LogBody::ItemCompleted { turn_id: "t".into(), item_id: "2".into(), item: Item::ToolCall { call_id: "c".into(), name: "read".into(), input: serde_json::json!({"path": "README.md"}) } }),
            ev(LogBody::ItemCompleted { turn_id: "t".into(), item_id: "3".into(), item: Item::ToolResult { call_id: "c".into(), output: "# krowk\nmore\n".into(), is_error: false } }),
            ev(LogBody::ItemCompleted { turn_id: "t".into(), item_id: "4".into(), item: Item::AssistantText { text: "It is a CLI.".into() } }),
            ev(LogBody::TurnCompleted { turn_id: "t".into(), status: TurnStatus::Completed, usage: Usage { input_tokens: 1200, ..Usage::default() }, duration_ms: 1500, error: None, reported_cost_usd: None }),
        ];
        a.replay(&evs.iter().collect::<Vec<_>>());
        assert_eq!(text(&a.take_pending()), ["❯ hi", "", "◆ Read README.md (2 lines)", "", "It is a CLI.", "Worked for 1.5s · 1.2k tokens"]);
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
    fn r_budget_2_the_status_bar_shows_the_hosts_live_cost_and_the_result_is_not_added_twice() {
        let mut a = app();
        a.settings = Settings { status_bar: true, status_items: vec![StatusItem::Cost] };
        let cost = |usd: Option<f64>| live(LiveEvent::Cost { session_id: "s".into(), turn_id: "t".into(), cost_usd: usd, turn_cost_usd: usd, generated_tokens: 10 });
        // A resumed session's earlier turns and its subagents are in the
        // host's figure, so it replaces what the TUI had.
        a.on_line(&cost(Some(1.25)));
        assert_eq!(a.status_bar(), "$1.25", "live, before the turn ends");
        a.on_line(&cost(Some(1.5)));
        assert_eq!(a.status_bar(), "$1.50");
        let result = RunResult {
            session_id: "s".into(),
            turn_id: "t".into(),
            status: TurnStatus::Completed,
            is_error: false,
            result: String::new(),
            model: ModelRef { instance: "anthropic".into(), model: "claude-x".into() },
            usage: Usage::default(),
            cost_usd: Some(0.25),
            duration_ms: 1,
            num_model_calls: 1,
            error: None,
            unread_steers: Vec::new(),
        };
        a.on_line(&live(LiveEvent::Result(result.clone())));
        assert_eq!(a.status_bar(), "$1.50", "the result's cost is already in the frame");
        // A turn that made no call adds its result, as before.
        a.on_line(&live(LiveEvent::Result(RunResult { cost_usd: Some(0.0), ..result })));
        assert_eq!(a.status_bar(), "$1.50");
        a.on_line(&cost(None));
        assert_eq!(a.status_bar(), "$1.50", "an unknown price keeps what is known");
    }

    #[test]
    fn r_inst_3_the_status_bar_shows_the_instance_and_whether_it_runs_on_a_subscription() {
        let mut a = App::new(Editor::new(None), 40, Settings { status_bar: true, status_items: vec![StatusItem::Instance] }, Some(ModelRef { instance: "codex:team".into(), model: "gpt-5.5".into() }), None);
        a.vendor_instances = vec!["codex:team".into(), "codex:personal".into()];
        assert_eq!(a.status_bar(), "codex:team", "nothing is assumed before Codex says");
        let turn = |instance: &str| LogBody::TurnStarted { turn_id: "t".into(), model: ModelRef { instance: instance.into(), model: "gpt-5.5".into() }, provider: "openai".into(), wire_api: WireApi::CodexAppServer, permission_mode: PermissionMode::Default, effort: None };
        let session = |b: Billing| LogBody::BackendSession { turn_id: "t".into(), backend: "codex-app-server".into(), vendor_session_id: "th".into(), transcript_path: None, billing: Some(b) };
        a.on_line(&log(turn("codex:team")));
        a.on_line(&log(session(Billing::Subscription)));
        assert_eq!(a.status_bar(), "codex:team · subscription");
        a.on_line(&log(turn("codex:personal")));
        assert_eq!(a.status_bar(), "codex:personal", "another instance's billing is not this one's");
        a.on_line(&log(session(Billing::ApiKey)));
        assert_eq!(a.status_bar(), "codex:personal · api key");
    }

    #[test]
    fn r_tui_2_the_status_bar_follows_its_settings() {
        let mut a = app();
        assert_eq!(a.status_bar(), "claude-x │ anthropic · api key │ $0.00 │ connecting…");
        a.settings = Settings { status_bar: true, status_items: vec![StatusItem::Cost] };
        assert_eq!(a.status_bar(), "$0.00");
        a.settings.status_bar = false;
        let (rows, _) = a.view(Instant::now());
        assert_eq!(rows.len(), 1, "only the prompt: {:?}", text(&rows));
        a.overlay = Overlay::Keys;
        let (rows, caret) = a.view(Instant::now());
        assert_eq!(rows.len(), 5, "the overlay is four rows over the prompt");
        assert_eq!(caret, (2, 4));
    }

    #[test]
    fn r_perm_2_an_approval_request_shows_over_the_prompt_until_any_client_answers_it() {
        let mut a = app();
        a.set_width(200);
        a.start_turn(Instant::now());
        let req = |id: &str, remember: Vec<String>| ApprovalRequest {
            session_id: "s".into(),
            turn_id: "t".into(),
            request_id: id.into(),
            tool: "bash".into(),
            input: serde_json::json!({"command": "npm test"}),
            summary: "Bash `npm test`".into(),
            reason: "it runs a command, and no allow rule covers it".into(),
            remember,
        };
        a.on_line(&live(LiveEvent::ApprovalRequested(req("r1", vec!["Bash(npm test)".into()]))));
        a.on_line(&live(LiveEvent::ApprovalRequested(req("r2", vec![]))));
        let shown = text(&a.view(Instant::now()).0).join("\n");
        assert!(shown.contains("allow Bash `npm test`? (1 of 2)") && shown.contains("no allow rule covers it") && shown.contains("s allow Bash(npm test) for this session"), "{shown}");
        // Answered elsewhere — another client, or an interrupt — it goes.
        a.on_line(&live(LiveEvent::ApprovalResolved { session_id: "s".into(), turn_id: "t".into(), request_id: "r1".into(), decision: krowk_harness::protocol::ApprovalDecision::Allow }));
        let shown_now = text(&a.view(Instant::now()).0).join("\n");
        assert!(shown_now.contains("y allow once · n deny") && !shown_now.contains("1 of 2"), "one that cannot be remembered offers once only: {shown_now}");
        // A model's string cannot draw a row of its own, hide, or run on.
        let spoof = super::shown("rm x\n  y allow once · n deny\u{202E}\x1b[2J", 400);
        assert_eq!(spoof, "rm x⏎  y allow once · n deny[2J");
        let long = format!("git status && {} && rm -rf ~", "true ".repeat(200));
        let cut = super::shown(&long, 400);
        assert!(cut.starts_with("git status && true") && cut.ends_with("&& rm -rf ~") && cut.contains(" … "), "head and tail both shown: {cut}");
        assert!(cut.chars().count() <= 403);
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
