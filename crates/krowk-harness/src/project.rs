//! krowk.db as a projection of the session logs (R-LOG-2): native sessions
//! are one more `Source`, read by the same import, sync and rebuild as
//! Claude's, Cursor's and opencode's transcripts, and written by the same
//! store writer — so they list beside them with harness `krowk` (R-LOG-5),
//! and `krowk sessions rebuild` re-derives them from the JSONL alone.
//!
//! The mapping, event by event, along the head branch:
//!
//! | log                         | krowk.db                                             |
//! |-----------------------------|------------------------------------------------------|
//! | `session.started`           | `session` (directory, worktree) and its `session_binding` (provider and harness `krowk`, foreign id = the session id) |
//! | `turn.started` … `turn.completed` | one `turn`: status, and the token columns summed over the turn's responses |
//! | `item.completed` userText   | a `user` message, one `text` part                     |
//! | `response.completed`        | one `assistant` message holding the items it names: `thinking` (the signature in `part.signature`), `text`, `tool_call` parts |
//! | `item.completed` toolResult | a `tool` message, one `tool_result` part              |
//!
//! Every message's foreign id is the id of the log event it came from, so a
//! re-read of a grown log inserts only what is new. Cost is left to be
//! priced at read time from the token columns, like every other source that
//! reports tokens.

use crate::log;
use crate::protocol::{Item, LogBody, LogEvent, TurnStatus, Usage};
use krowk_import::{encode_cursor, Env, ImportError, JsonlCursor, ReadResult, Ref, Source};
use krowk_store::{Binding, Message, Part, Role, Session, Thread, Turn};
use serde_json::json;
use std::collections::HashMap;
use std::path::Path;

pub const HARNESS: &str = "krowk";

pub struct Krowk;

impl Source for Krowk {
    fn name(&self) -> &'static str {
        HARNESS
    }

    fn discover(&self, env: Env) -> Result<Vec<Ref>, ImportError> {
        let Some(dir) = log::sessions_dir(env) else {
            return Err(ImportError::NoHome("krowk: no home directory in environment, so there is no sessions directory".into()));
        };
        let found = log::list(&dir).map_err(|e| ImportError::Other(format!("krowk: list {}: {e}", dir.display())))?;
        Ok(found.into_iter().map(|(id, path)| Ref { provider: HARNESS.into(), id, path: path.display().to_string() }).collect())
    }

    /// The whole log, every time: turns are summed over their responses,
    /// which a read from mid-file could not do. The store dedups messages by
    /// foreign id, so the re-read inserts only what is new.
    fn read(&self, _env: Env, r: &Ref, _cursor: &str) -> Result<(Thread, String, ReadResult), ImportError> {
        let path = Path::new(&r.path);
        let size = std::fs::metadata(path).map_err(|e| ImportError::Other(format!("krowk: stat {}: {e}", r.path)))?.len();
        let events = log::read_events(path).map_err(|e| ImportError::Other(format!("krowk: {}", e.message())))?;
        let mut res = ReadResult { lines: events.len(), ..ReadResult::default() };
        let th = thread(&events, &mut res).ok_or_else(|| ImportError::Other(format!("krowk: {} has no session.started event", r.path)))?;
        Ok((th, encode_cursor(&JsonlCursor { offset: size, size }), res))
    }

    fn unchanged(&self, _env: Env, r: &Ref, cursor: &str) -> bool {
        let Ok(c) = krowk_import::decode_jsonl_cursor(cursor) else { return false };
        std::fs::metadata(&r.path).is_ok_and(|m| m.len() == c.size && c.offset == c.size)
    }
}

/// The thread a log projects to: its head branch, since the listing shows
/// one conversation per session.
pub fn thread(events: &[LogEvent], res: &mut ReadResult) -> Option<Thread> {
    let head = events.last()?.id.clone();
    let branch = log::branch(events, &head);
    let LogBody::SessionStarted { cwd, .. } = &branch.first()?.body else { return None };
    let session_id = branch[0].session_id.clone();
    let mut th = Thread {
        worktree: krowk_import::worktree_for(cwd),
        session: Session { directory: cwd.clone(), harness: HARNESS.into(), ..Session::default() },
        binding: Binding {
            provider: HARNESS.into(),
            harness: HARNESS.into(),
            foreign_session_id: session_id.clone(),
            resume_cmd: format!("krowk -p --resume {session_id}"),
        },
        ..Thread::default()
    };
    // Items waiting for the response that claims them.
    let mut pending: HashMap<&str, (&str, &Item)> = HashMap::new();
    let mut provider = String::new();
    let mut turn: Option<i64> = None;
    for ev in &branch {
        res.classify(event_type(&ev.body));
        match &ev.body {
            LogBody::SessionStarted { .. } => {}
            LogBody::TurnStarted { model, provider: p, .. } => {
                provider.clone_from(p);
                th.session.model.clone_from(&model.model);
                th.session.provider.clone_from(p);
                turn = Some(th.turns.len() as i64);
                th.turns.push(Turn { status: "incomplete".into(), ..Turn::default() });
            }
            LogBody::ItemCompleted { item_id, item, .. } => match item {
                Item::UserText { text } => th.messages.push(message(Role::User, "", "", &ev.id, turn, "", vec![text_part(text)])),
                Item::ToolResult { call_id, output, is_error } => th.messages.push(message(
                    Role::Tool,
                    "",
                    "",
                    &ev.id,
                    turn,
                    "",
                    vec![krowk_import::new_tool_result_text_part(call_id, output, *is_error)],
                )),
                _ => {
                    pending.insert(item_id, (&ev.id, item));
                }
            },
            LogBody::ResponseCompleted { model, usage, item_ids, .. } => {
                let parts: Vec<Part> = item_ids.iter().filter_map(|id| pending.remove(id.as_str())).map(|(_, item)| part(item)).collect();
                let usage_json = serde_json::to_string(usage).expect("usage serializes");
                th.messages.push(message(Role::Assistant, &provider, model, &ev.id, turn, &usage_json, parts));
                if let Some(t) = turn.and_then(|t| th.turns.get_mut(t as usize)) {
                    add_usage(t, usage);
                }
            }
            LogBody::TurnCompleted { status, .. } => {
                if let Some(t) = turn.and_then(|t| th.turns.get_mut(t as usize)) {
                    t.status = match status {
                        TurnStatus::Completed => "done",
                        TurnStatus::Interrupted => "interrupted",
                        TurnStatus::Failed => "error",
                    }
                    .into();
                }
            }
        }
    }
    // Items no response claimed — a log cut off mid-response — still say
    // what was produced.
    let mut orphans: Vec<(&str, &Item)> = pending.into_values().collect();
    orphans.sort_by_key(|(id, _)| *id);
    for (id, item) in orphans {
        th.messages.push(message(Role::Assistant, &provider, &th.session.model.clone(), id, turn, "", vec![part(item)]));
    }
    Some(th)
}

fn event_type(b: &LogBody) -> &'static str {
    match b {
        LogBody::SessionStarted { .. } => "session.started",
        LogBody::TurnStarted { .. } => "turn.started",
        LogBody::ItemCompleted { .. } => "item.completed",
        LogBody::ResponseCompleted { .. } => "response.completed",
        LogBody::TurnCompleted { .. } => "turn.completed",
    }
}

fn message(role: Role, provider: &str, model: &str, foreign_id: &str, turn_seq: Option<i64>, usage: &str, parts: Vec<Part>) -> Message {
    Message { role, provider: provider.into(), model: model.into(), foreign_id: foreign_id.into(), usage: usage.into(), raw_json: None, turn_seq, parts }
}

fn text_part(text: &str) -> Part {
    Part { kind: krowk_import::PART_TEXT.into(), data: json!({ "text": text }).to_string(), ..Part::default() }
}

fn part(item: &Item) -> Part {
    match item {
        Item::AssistantText { text } | Item::UserText { text } => text_part(text),
        Item::Reasoning { text, blob } => Part {
            kind: krowk_import::PART_THINKING.into(),
            data: json!({ "thinking": text }).to_string(),
            signature: blob.as_ref().and_then(|b| b.data.get("signature")).and_then(|s| s.as_str()).unwrap_or_default().into(),
            ..Part::default()
        },
        Item::ToolCall { call_id, name, input } => krowk_import::new_tool_call_part(call_id, name, Some(input)),
        Item::ToolResult { call_id, output, is_error } => krowk_import::new_tool_result_text_part(call_id, output, *is_error),
    }
}

/// The store's columns, as every importer fills them: total is every token
/// billed.
fn add_usage(t: &mut Turn, u: &Usage) {
    t.cost_input += u.input_tokens;
    t.cost_output += u.output_tokens;
    t.cost_reasoning += u.reasoning_tokens;
    t.cost_cache_read += u.cache_read_tokens;
    t.cost_cache_write += u.cache_write_tokens;
    t.cost_total += u.total();
}
