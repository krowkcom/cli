//! The inline TUI that bare `krowk` opens on a terminal: a client of the
//! harness's in-process protocol (R-PROTO-1). It sends `Command`s to a
//! `Host` and draws the `StreamLine`s that come back — the same two types
//! `krowk -p` speaks, and the daemon's socket clients will — and it reads a
//! resumed session's history from its log, the protocol's persisted half.
//! Nothing here reaches into the engine.
//!
//! - `term` — the inline viewport and synchronized frames (R-TUI-1).
//! - `app` — what is shown, driven by frames and keys.
//! - `editor` — the multi-line prompt and its history.
//! - `settings` — the status bar's configuration (R-TUI-2).
//! - `net` — the connectivity probe behind the offline notice (R-OFF-1).
//!
//! The loop is event-driven end to end (R-PERF-2): it sleeps in one
//! `select!` until a key, a frame of the stream, the turn's end or a
//! deadline it set itself wakes it, and it sets deadlines only while there
//! is something to time — a frame owed (at most one per 1/60 s, R-PERF-4),
//! a running turn's clock, an interrupt not yet taken, a probe. Idle, with
//! nothing running and the API reachable, it has none, and wakes for
//! nothing but a key.

pub mod app;
pub mod editor;
pub mod net;
pub mod settings;
pub mod term;

use app::{App, Overlay};
use crossterm::event::{Event, EventStream, KeyCode, KeyEvent, KeyEventKind, KeyModifiers};
use editor::Editor;
use futures_core::Stream;
use krowk_harness::engine::EngineError;
use krowk_harness::host::{Host, HostConfig, Pricer};
use krowk_harness::log;
use krowk_harness::protocol::{Command, Effort, ModelRef, PermissionMode, RunResult, StreamLine, TurnStatus};
use net::Target;
use ratatui::layout::Size;
use settings::Settings;
use std::future::Future;
use std::io::Write;
use std::path::PathBuf;
use std::pin::Pin;
use std::time::Duration;
use term::Term;
use tokio::sync::mpsc;
use tokio::time::Instant;

/// The shortest time between two frames: 60 per second at most (R-PERF-4),
/// with a millisecond to spare, so a frame that reaches the terminal late
/// and the one after it on time still never make 61 in a second.
pub const FRAME: Duration = Duration::from_millis(17);
/// A model call silent for this long gets a connectivity probe.
const STALL: Duration = Duration::from_millis(700);
/// How often an interrupt the host could not take yet is asked again.
const INTERRUPT_RETRY: Duration = Duration::from_millis(50);

pub struct Options {
    pub host: HostConfig,
    /// A session to continue: its history is drawn first.
    pub resume: Option<String>,
    /// The model for every prompt; the session's own when absent.
    pub model: Option<ModelRef>,
    pub permission_mode: PermissionMode,
    /// The toolset preset for every prompt; the model's own when absent.
    pub toolset: Option<String>,
    /// The reasoning effort for every prompt, on krowk's ladder.
    pub effort: Option<Effort>,
    pub settings: Settings,
    /// Where prompt history is kept; none keeps it in memory only.
    pub history_file: Option<PathBuf>,
    /// Lines shown above the first prompt: config warnings and the like.
    pub notices: Vec<String>,
    pub version: String,
}

/// How the TUI ended.
pub struct Outcome {
    /// The session it ran turns in, for the caller to project into krowk.db.
    pub session_id: Option<String>,
    /// Why it could not run or stopped early.
    pub error: Option<String>,
    /// Left without waiting for the running turn — a second Ctrl-C, or a
    /// second SIGTERM/SIGHUP: the caller exits 130 once the session is
    /// recorded.
    pub abandoned: bool,
}

/// Opens the TUI on this process's terminal and runs it until the person
/// quits. Raw mode is on for the duration and off again however it ends —
/// a panic included, since the release profile aborts rather than unwinds.
pub fn run(opts: Options) -> Outcome {
    let rt = match tokio::runtime::Builder::new_current_thread()
        .enable_all()
        // A probe's name lookup runs on a blocking thread; one left idle
        // would be a wakeup when it is reaped, so none lingers.
        .thread_keep_alive(Duration::from_millis(250))
        .build()
    {
        Ok(rt) => rt,
        Err(e) => return Outcome { session_id: None, abandoned: false, error: Some(format!("the async runtime could not start: {e}")) },
    };
    if let Err(e) = crossterm::terminal::enable_raw_mode() {
        return Outcome { session_id: None, abandoned: false, error: Some(format!("the terminal could not be put in raw mode: {e}")) };
    }
    let hook = std::panic::take_hook();
    std::panic::set_hook(Box::new(move |info| {
        restore_terminal();
        hook(info);
    }));
    let outcome = rt.block_on(session(opts));
    restore_terminal();
    outcome
}

fn restore_terminal() {
    let _ = crossterm::terminal::disable_raw_mode();
    let mut out = std::io::stdout();
    let _ = out.write_all(b"\x1b[?2004l\x1b[?25h");
    let _ = out.flush();
}

type TurnFuture<'a> = Pin<Box<dyn Future<Output = Result<Option<RunResult>, EngineError>> + 'a>>;
type ProbeFuture = Pin<Box<dyn Future<Output = bool>>>;

async fn session(opts: Options) -> Outcome {
    let mut stdout = std::io::stdout();
    let _ = stdout.write_all(b"\x1b[?2004h");
    let (w, h) = crossterm::terminal::size().unwrap_or((80, 24));
    let size = Size { width: w.max(1), height: h.max(1) };
    // Asked once, before anything else reads the terminal. A cursor mid-line
    // (a prompt without a trailing newline) gets a line of its own.
    let top = match crossterm::cursor::position() {
        Ok((0, y)) => y,
        Ok((_, y)) => {
            let _ = stdout.write_all(b"\r\n");
            (y + 1).min(size.height - 1)
        }
        Err(_) => size.height - 1,
    };

    let sessions_dir = opts.host.sessions_dir.clone();
    let pricer: Pricer = opts.host.pricer.clone();
    let mut app = App::new(Editor::new(opts.history_file.clone()), size.width, opts.settings.clone(), None, Some(pricer));
    app.log_dir = Some(sessions_dir.display().to_string());
    app.permission_mode = serde_json::to_value(opts.permission_mode).ok().and_then(|v| v.as_str().map(String::from)).unwrap_or_default();
    app.say(&format!("krowk {} · {}", opts.version, opts.host.cwd.display()), app::dim());
    for n in &opts.notices {
        app.notice(n);
    }
    if let Some(id) = &opts.resume {
        match log::read_events(&sessions_dir.join(id).join(log::EVENTS_FILE)) {
            Ok(events) => {
                let head = events.last().map(|e| e.id.clone()).unwrap_or_default();
                app.replay(&log::branch(&events, &head));
                app.session_id = Some(id.clone());
                app.say(&format!("resumed session {id}"), app::dim());
            }
            Err(e) => return Outcome { session_id: None, abandoned: false, error: Some(format!("session {id} could not be read: {}", e.message())) },
        }
    }
    // The model shown before the first turn names one: the flag, else the
    // session's last, else the configured default.
    let shown = opts.model.clone().or_else(|| app.model.clone()).or_else(|| opts.host.registry.default_model().ok());
    let target = shown.as_ref().and_then(|m| opts.host.registry.get(&m.instance).ok()).and_then(|i| Target::for_url(&i.base_url, &|k| std::env::var(k).unwrap_or_default()));
    app.model = shown;

    let initial_height = app.view(Instant::now().into_std()).0.len() as u16;
    let mut term = match Term::new(stdout, size, top, initial_height) {
        Ok(mut t) => {
            t.reflows = term::reflows_from(&|k| std::env::var(k).unwrap_or_default());
            t
        }
        Err(e) => return Outcome { session_id: None, abandoned: false, error: Some(format!("the terminal could not be drawn on: {e}")) },
    };
    let host = Host::new(opts.host);
    let mut ui = Ui { host: &host, model: opts.model, permission_mode: opts.permission_mode, toolset: opts.toolset, effort: opts.effort, target, keys: None, turn: None, rx: None, abandoned: false };
    let result = ui.run(&mut app, &mut term).await;
    let _ = term.finish();
    let mut out = term.into_inner();
    if let Some(id) = &app.session_id {
        let _ = write!(out, "\x1b[2mresume this session with: krowk --resume {id}\x1b[0m\r\n");
    }
    let _ = out.flush();
    Outcome { session_id: app.session_id.clone(), error: result.err().map(|e| e.to_string()), abandoned: ui.abandoned }
}

struct Ui<'h> {
    host: &'h Host,
    model: Option<ModelRef>,
    permission_mode: PermissionMode,
    toolset: Option<String>,
    effort: Option<Effort>,
    target: Option<Target>,
    keys: Option<EventStream>,
    turn: Option<TurnFuture<'h>>,
    rx: Option<mpsc::Receiver<StreamLine>>,
    /// Set to leave now, without the running turn's end.
    abandoned: bool,
}

/// SIGTERM and SIGHUP as one stream, registered once. Either asks the TUI
/// to stop the way Ctrl-D does: the running turn is interrupted and its end
/// waited for, the terminal restored, the session recorded; a second one
/// does not wait. Nothing on Windows, where neither is sent.
struct Hangups {
    #[cfg(unix)]
    term: Option<tokio::signal::unix::Signal>,
    #[cfg(unix)]
    hup: Option<tokio::signal::unix::Signal>,
}

impl Hangups {
    fn new() -> Hangups {
        #[cfg(unix)]
        {
            use tokio::signal::unix::{signal, SignalKind};
            Hangups { term: signal(SignalKind::terminate()).ok(), hup: signal(SignalKind::hangup()).ok() }
        }
        #[cfg(not(unix))]
        Hangups {}
    }

    async fn recv(&mut self) {
        #[cfg(unix)]
        {
            async fn one(s: &mut Option<tokio::signal::unix::Signal>) {
                let got = match s {
                    Some(s) => s.recv().await.is_some(),
                    None => false,
                };
                if !got {
                    std::future::pending::<()>().await;
                }
            }
            tokio::select! {
                _ = one(&mut self.term) => {}
                _ = one(&mut self.hup) => {}
            }
        }
        #[cfg(not(unix))]
        std::future::pending::<()>().await
    }
}

/// Where the terminal says the cursor is, asked after the key reader has
/// stopped (a resize, a job stop).
///
/// Not through crossterm's `cursor::position`: that fails at once when the
/// reader's wake-up is still pending, and the answer to the query it had
/// already written then waits in crossterm's queue, where the next ask —
/// at the next resize — takes it for the answer to its own, one resize
/// stale. So the reader is let settle (a zero-length wait for an event
/// takes the wake-up, and returns once nothing else holds the terminal's
/// input), and the query is written and its answer read here, on the
/// terminal itself, one query and one answer.
#[cfg(unix)]
fn cursor_row() -> Option<u16> {
    use std::io::Read;
    use std::os::fd::AsRawFd;
    let _ = crossterm::event::poll(Duration::from_millis(20));
    let mut tty = std::fs::OpenOptions::new().read(true).write(true).open("/dev/tty").ok()?;
    tty.write_all(b"\x1b[6n").ok()?;
    tty.flush().ok()?;
    let deadline = std::time::Instant::now() + Duration::from_secs(1);
    let mut got = Vec::new();
    let mut buf = [0u8; 64];
    loop {
        if let Some(row) = parse_cursor_report(&got) {
            return Some(row);
        }
        let left = deadline.saturating_duration_since(std::time::Instant::now());
        if left.is_zero() || got.len() > 4096 {
            return None;
        }
        let mut pfd = libc::pollfd { fd: tty.as_raw_fd(), events: libc::POLLIN, revents: 0 };
        // SAFETY: one pollfd, owned here, for the length of the call.
        let ready = unsafe { libc::poll(&mut pfd, 1, left.as_millis().min(1000) as i32) };
        if ready <= 0 {
            continue;
        }
        match tty.read(&mut buf) {
            Ok(0) | Err(_) => return None,
            Ok(n) => got.extend_from_slice(&buf[..n]),
        }
    }
}

#[cfg(not(unix))]
fn cursor_row() -> Option<u16> {
    let _ = crossterm::event::poll(Duration::ZERO);
    (0..2).find_map(|_| crossterm::cursor::position().ok()).map(|(_, y)| y)
}

/// The row of the first `ESC [ row ; col R` in `bytes`, zero-based.
fn parse_cursor_report(bytes: &[u8]) -> Option<u16> {
    let text = String::from_utf8_lossy(bytes);
    let mut rest = text.as_ref();
    while let Some(i) = rest.find("\x1b[") {
        let tail = &rest[i + 2..];
        if let Some(end) = tail.find('R')
            && let Some((row, col)) = tail[..end].split_once(';')
            && let (Ok(row), Ok(_)) = (row.parse::<u16>(), col.parse::<u16>())
        {
            return Some(row.saturating_sub(1));
        }
        rest = tail;
    }
    None
}

/// The next of an optional stream, or never.
async fn next_key(keys: &mut Option<EventStream>) -> Option<std::io::Result<Event>> {
    match keys {
        Some(k) => std::future::poll_fn(|cx| Pin::new(&mut *k).poll_next(cx)).await,
        None => std::future::pending().await,
    }
}

async fn recv(rx: &mut Option<mpsc::Receiver<StreamLine>>) -> StreamLine {
    match rx {
        Some(r) => match r.recv().await {
            Some(l) => l,
            None => std::future::pending().await,
        },
        None => std::future::pending().await,
    }
}

async fn finish<F: Future + Unpin>(f: &mut Option<F>) -> F::Output {
    match f {
        Some(f) => f.await,
        None => std::future::pending().await,
    }
}

async fn until(at: Option<Instant>) {
    match at {
        Some(t) => tokio::time::sleep_until(t).await,
        None => std::future::pending().await,
    }
}

impl<'h> Ui<'h> {
    async fn run<W: Write>(&mut self, app: &mut App, term: &mut Term<W>) -> std::io::Result<()> {
        self.keys = Some(EventStream::new());
        let mut frame_at: Option<Instant> = None;
        let mut last_frame: Option<Instant> = None;
        // One probe at start, so a machine that is offline says so before
        // anybody types into it.
        let mut probe: Option<ProbeFuture> = self.target.clone().map(|t| Box::pin(async move { net::reachable(&t).await }) as ProbeFuture);
        let mut probe_at: Option<Instant> = None;
        let mut failures = 0u32;
        let mut last_activity = Instant::now();
        let mut stall_quiet_until: Option<Instant> = None;
        let mut quitting = false;
        let mut hangups = Hangups::new();
        self.draw(app, term)?;
        last_frame.replace(Instant::now());
        loop {
            let now = Instant::now();
            // Deadlines, only for what is actually pending.
            let tick_at = app.turn.as_ref().map(|t| {
                let started = Instant::from_std(t.started);
                let n = (now.saturating_duration_since(started).as_secs() + 1) as u32;
                started + app::TICK * n
            });
            let stall_at = (app.waiting_on_model() && probe.is_none() && app.offline.is_none() && self.target.is_some())
                .then(|| (last_activity + STALL).max(stall_quiet_until.unwrap_or(now)));
            let retry_at = app.turn.as_ref().filter(|t| (t.want_interrupt && !t.interrupt_sent) || !app.unsent_steers.is_empty()).map(|_| now + INTERRUPT_RETRY);
            let wake = [frame_at, tick_at, stall_at, retry_at, probe_at].into_iter().flatten().min();
            tokio::select! {
                biased;
                _ = hangups.recv() => {
                    if !app.running() || quitting {
                        self.abandoned = app.running();
                        return Ok(());
                    }
                    quitting = true;
                    self.interrupt(app).await;
                }
                ev = next_key(&mut self.keys) => match ev {
                    Some(Ok(ev)) => {
                        if self.on_event(app, term, ev, &mut quitting).await? {
                            probe_at = Some(Instant::now());
                        }
                    }
                    // The terminal is gone: nobody is left to answer.
                    Some(Err(_)) | None => return Ok(()),
                },
                line = recv(&mut self.rx) => {
                    last_activity = Instant::now();
                    if matches!(&line, StreamLine::Live(krowk_harness::protocol::LiveEvent::ItemDelta { .. })) {
                        // Bytes are arriving from the model: it is reachable,
                        // and a retry probe still scheduled is moot.
                        app.set_online();
                        failures = 0;
                        probe_at = None;
                    }
                    app.on_line(&line);
                    self.flush_requests(app).await;
                }
                r = finish(&mut self.turn) => {
                    self.turn = None;
                    // What the host sent before answering is already queued.
                    if let Some(mut rx) = self.rx.take() {
                        while let Ok(line) = rx.try_recv() {
                            app.on_line(&line);
                        }
                    }
                    // What the engine never read, as the host says on the
                    // result (its own queue, so nothing is guessed), and what
                    // was never accepted at all.
                    let (acked, unsent) = app.end_turn_parts();
                    let mut left = match &r {
                        Ok(Some(res)) => res.unread_steers.clone(),
                        _ => acked,
                    };
                    left.extend(unsent);
                    let network = match &r {
                        Ok(Some(res)) => res.error.as_ref().is_some_and(|e| e.code == "network_unreachable"),
                        Ok(None) => false,
                        Err(e) => e.code == "network_unreachable",
                    };
                    let completed = matches!(&r, Ok(Some(res)) if res.status == TurnStatus::Completed);
                    if let Err(e) = r {
                        app.error(&e.info());
                    }
                    if network && probe.is_none() {
                        probe_at = Some(Instant::now());
                    }
                    if quitting {
                        return Ok(());
                    }
                    // Steering the turn never read: after an answer it is
                    // the next prompt, as it would have been the turn's next
                    // step. After an interrupt or a failure it is not sent
                    // on its own — it goes back into the prompt, to send,
                    // edit or drop.
                    if !left.is_empty() {
                        if completed {
                            self.prompt(app, left.join("\n\n"));
                        } else {
                            app.editor.restore(&left.join("\n\n"));
                            app.notice("the steering this turn never read is back in the prompt");
                        }
                    }
                }
                ok = finish(&mut probe) => {
                    probe = None;
                    if ok {
                        app.set_online();
                        failures = 0;
                        probe_at = None;
                    } else {
                        app.set_offline(self.target.as_ref().map(Target::label).unwrap_or_default());
                        probe_at = Some(Instant::now() + net::retry_after(failures));
                        failures += 1;
                    }
                }
                _ = until(wake) => {
                    let now = Instant::now();
                    if frame_at.is_some_and(|t| t <= now) {
                        frame_at = None;
                        self.draw(app, term)?;
                        last_frame = Some(now);
                    }
                    if tick_at.is_some_and(|t| t <= now) {
                        app.touch();
                    }
                    let stalled = stall_at.is_some_and(|t| t <= now);
                    if (stalled || probe_at.is_some_and(|t| t <= now))
                        && probe.is_none()
                        && let Some(t) = self.target.clone()
                    {
                        // A stall that proves reachable is not asked about
                        // again for a while: a model can think in silence.
                        if stalled {
                            stall_quiet_until = Some(now + STALL * 4);
                        }
                        probe_at = None;
                        probe = Some(Box::pin(async move { net::reachable(&t).await }));
                    }
                    if retry_at.is_some_and(|t| t <= now) {
                        self.flush_requests(app).await;
                    }
                }
            }
            if self.abandoned || (app.quit && self.turn.is_none()) {
                return Ok(());
            }
            if app.take_dirty() && frame_at.is_none() {
                let now = Instant::now();
                frame_at = Some(last_frame.map_or(now, |t| (t + FRAME).max(now)));
            }
            // A frame that is due is drawn here too, whichever branch woke
            // the loop: a stream that never pauses must not starve the screen.
            let now = Instant::now();
            if frame_at.is_some_and(|t| t <= now) {
                frame_at = None;
                self.draw(app, term)?;
                last_frame = Some(now);
            }
        }
    }

    fn draw<W: Write>(&mut self, app: &mut App, term: &mut Term<W>) -> std::io::Result<()> {
        // The resize event can trail the resize itself; a frame drawn for
        // the old size in between would land in the wrong rows. The size is
        // one ioctl away, so every frame asks.
        if let Ok((w, h)) = crossterm::terminal::size()
            && (w.max(1), h.max(1)) != (term.size().width, term.size().height)
        {
            self.resize(app, term, w, h)?;
        }
        let (mut rows, mut caret) = app.view(std::time::Instant::now());
        // A live region taller than the terminal keeps its bottom: the
        // prompt and the status bar, over whatever is streaming.
        let skip = rows.len().saturating_sub(usize::from(term.size().height));
        rows.drain(..skip);
        caret.1 = caret.1.saturating_sub(skip as u16);
        let lines = app.take_pending();
        term.frame(&lines, &rows, caret)
    }

    fn prompt(&mut self, app: &mut App, text: String) {
        let (tx, rx) = mpsc::channel(1024);
        let cmd = Command::Prompt { session_id: app.session_id.clone(), text, model: self.model.clone(), permission_mode: self.permission_mode, toolset: self.toolset.clone(), effort: self.effort };
        self.turn = Some(Box::pin(self.host.execute(cmd, tx)));
        self.rx = Some(rx);
        app.start_turn(std::time::Instant::now());
    }

    /// An interrupt or steering the host could not take yet (the turn had
    /// not registered), asked again.
    async fn flush_requests(&mut self, app: &mut App) {
        let Some(id) = app.session_id.clone() else { return };
        let Some(t) = &app.turn else { return };
        if !t.prompt_seen {
            return;
        }
        if t.want_interrupt
            && !t.interrupt_sent
            && self.command(Command::Interrupt { session_id: id.clone() }).await.is_ok()
            && let Some(t) = &mut app.turn
        {
            t.interrupt_sent = true;
        }
        while let Some(text) = app.unsent_steers.first().cloned() {
            if self.command(Command::Steer { session_id: id.clone(), text: text.clone() }).await.is_err() {
                break;
            }
            app.unsent_steers.remove(0);
            app.steers.push(text);
        }
        app.touch();
    }

    async fn command(&self, cmd: Command) -> Result<(), EngineError> {
        let (tx, _rx) = mpsc::channel(1);
        self.host.execute(cmd, tx).await.map(|_| ())
    }

    /// One terminal event. True when it asks for a connectivity probe.
    async fn on_event<W: Write>(&mut self, app: &mut App, term: &mut Term<W>, ev: Event, quitting: &mut bool) -> std::io::Result<bool> {
        match ev {
            Event::Key(k) if k.kind != KeyEventKind::Release && k.code == KeyCode::Char('z') && k.modifiers.contains(KeyModifiers::CONTROL) => self.suspend(app, term)?,
            Event::Key(k) if k.kind != KeyEventKind::Release => return Ok(self.on_key(app, k, quitting).await),
            Event::Paste(s) => {
                app.editor.insert_str(&s);
                app.touch();
            }
            Event::Resize(w, h) => self.resize(app, term, w, h)?,
            _ => {}
        }
        Ok(false)
    }

    /// The terminal changed size: where the cursor is now is asked, with
    /// the key reader stopped so the answer reaches us, and the live region
    /// is rebuilt from there.
    fn resize<W: Write>(&mut self, app: &mut App, term: &mut Term<W>, w: u16, h: u16) -> std::io::Result<()> {
        self.keys = None;
        term.resize(Size { width: w.max(1), height: h.max(1) }, cursor_row())?;
        self.keys = Some(EventStream::new());
        app.set_width(w);
        Ok(())
    }

    /// Ctrl-Z: raw mode swallows the terminal's own, so the job stop is done
    /// here — the live region cleared and the terminal given back, then
    /// SIGTSTP to ourselves. The shell has the terminal until `fg`; on
    /// SIGCONT the stop returns, and the TUI takes the terminal again and
    /// redraws where the cursor now is. A turn running keeps running: the
    /// engine is in this process, stopped with it.
    fn suspend<W: Write>(&mut self, app: &mut App, term: &mut Term<W>) -> std::io::Result<()> {
        #[cfg(unix)]
        {
            self.keys = None;
            term.finish()?;
            restore_terminal();
            // SAFETY: raise only sends a signal to this process.
            unsafe {
                libc::raise(libc::SIGTSTP);
            }
            crossterm::terminal::enable_raw_mode()?;
            let mut out = std::io::stdout();
            let _ = out.write_all(b"\x1b[?2004h");
            let _ = out.flush();
            let (w, h) = crossterm::terminal::size().unwrap_or((term.size().width, term.size().height));
            term.resume(Size { width: w.max(1), height: h.max(1) }, cursor_row())?;
            self.keys = Some(EventStream::new());
            app.set_width(w);
        }
        #[cfg(not(unix))]
        let _ = (app, term);
        Ok(())
    }

    async fn on_key(&mut self, app: &mut App, k: KeyEvent, quitting: &mut bool) -> bool {
        let ctrl = k.modifiers.contains(KeyModifiers::CONTROL);
        let alt = k.modifiers.contains(KeyModifiers::ALT);
        app.touch();
        let e = &mut app.editor;
        match k.code {
            KeyCode::Char('c') if ctrl => {
                if app.running() {
                    if *quitting || app.turn.as_ref().is_some_and(|t| t.want_interrupt) {
                        // A second Ctrl-C does not wait for the first — but
                        // still leaves through the front door: the terminal
                        // restored, the resume line printed, the session
                        // recorded, then exit 130.
                        self.abandoned = true;
                        return false;
                    }
                    self.interrupt(app).await;
                } else if !app.editor.is_empty() {
                    app.editor.clear();
                } else {
                    app.quit = true;
                }
            }
            KeyCode::Char('d') if ctrl => {
                if e.is_empty() {
                    if app.running() {
                        *quitting = true;
                        self.interrupt(app).await;
                    } else {
                        app.quit = true;
                    }
                } else {
                    e.delete();
                }
            }
            KeyCode::Esc => {
                if app.overlay != Overlay::None {
                    app.overlay = Overlay::None;
                } else if app.running() {
                    self.interrupt(app).await;
                }
            }
            KeyCode::Char('o') if ctrl => app.overlay = if app.overlay == Overlay::Details { Overlay::None } else { Overlay::Details },
            KeyCode::F(1) => app.overlay = if app.overlay == Overlay::Keys { Overlay::None } else { Overlay::Keys },
            KeyCode::Char('?') if e.is_empty() && !ctrl && !alt => app.overlay = if app.overlay == Overlay::Keys { Overlay::None } else { Overlay::Keys },
            KeyCode::Enter if alt => e.insert('\n'),
            KeyCode::Char('j') if ctrl => e.insert('\n'),
            KeyCode::Enter => {
                if e.enter() {
                    return self.submit(app).await;
                }
            }
            KeyCode::Char('a') if ctrl => e.home(),
            KeyCode::Char('e') if ctrl => e.end(),
            KeyCode::Char('b') if ctrl => e.left(),
            KeyCode::Char('f') if ctrl => e.right(),
            KeyCode::Char('b') if alt => e.word_left(),
            KeyCode::Char('f') if alt => e.word_right(),
            KeyCode::Char('u') if ctrl => e.kill_to_start(),
            KeyCode::Char('k') if ctrl => e.kill_to_end(),
            KeyCode::Char('w') if ctrl => e.kill_word(),
            KeyCode::Char('h') if ctrl => e.backspace(),
            KeyCode::Backspace if alt || ctrl => e.kill_word(),
            KeyCode::Backspace => e.backspace(),
            KeyCode::Delete => e.delete(),
            KeyCode::Left if ctrl || alt => e.word_left(),
            KeyCode::Right if ctrl || alt => e.word_right(),
            KeyCode::Left => e.left(),
            KeyCode::Right => e.right(),
            KeyCode::Home => e.home(),
            KeyCode::End => e.end(),
            KeyCode::Up => e.up(),
            KeyCode::Down => e.down(),
            KeyCode::Tab => e.insert_str("    "),
            KeyCode::Char(c) if !ctrl => e.insert(c),
            _ => {}
        }
        false
    }

    async fn interrupt(&mut self, app: &mut App) {
        if let Some(t) = &mut app.turn {
            t.want_interrupt = true;
        }
        self.flush_requests(app).await;
    }

    /// Enter: a prompt when idle, steering while a turn runs. True when a
    /// connectivity probe is owed first — the notice is up and the person
    /// is trying again.
    async fn submit(&mut self, app: &mut App) -> bool {
        let text = app.editor.text().trim().to_string();
        if text.is_empty() {
            return false;
        }
        match text.as_str() {
            "/exit" | "/quit" => {
                app.editor.clear();
                app.quit = true;
                return false;
            }
            "/help" => {
                app.editor.clear();
                app.overlay = Overlay::Keys;
                return false;
            }
            _ => {}
        }
        let text = app.editor.take().trim_end().to_string();
        app.overlay = Overlay::None;
        if app.running() {
            app.unsent_steers.push(text);
            self.flush_requests(app).await;
            return false;
        }
        self.prompt(app, text);
        app.offline.is_some()
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn a_cursor_report_is_read_past_whatever_came_before_it() {
        assert_eq!(super::parse_cursor_report(b"\x1b[12;40R"), Some(11));
        assert_eq!(super::parse_cursor_report(b"ab\x1b[A\x1b[3;1R"), Some(2), "a key typed meanwhile is skipped");
        assert_eq!(super::parse_cursor_report(b"\x1b[3;1"), None, "not whole yet");
    }
}
