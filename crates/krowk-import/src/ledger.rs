//! Provider-side usage ledgers: what a provider metered, read beside the
//! transcripts so an execution the client never saw still counts.
//!
//! A ledger is a JSONL file in `ledger/` beside krowk.db
//! (`~/.local/share/krowk/ledger/<name>.jsonl`), one metered execution per
//! line, in the provider's words:
//!
//! ```json
//! {"id":"gen_01…","provider":"opencode","model":"qwen3.5-plus","time":"2026-09-10T11:32:05Z",
//!  "input_tokens":84,"output_tokens":3422,"reasoning_tokens":3405,"cost_usd":0.0041}
//! ```
//!
//! `id`, `provider` and `model` are required; `id` is the provider's own id
//! for the execution and is what a re-import dedups on — never a derived one.
//! `output_tokens` is every completion token billed, reasoning included;
//! `reasoning_tokens` says how many of those were reasoning.
//! `cache_read_tokens`, `cache_write_tokens`, `time` (RFC 3339) or `time_ms`,
//! and `cost_usd` (only when the provider states it) are optional.
//!
//! Each file is one session (harness `ledger`, bound on the file name), one
//! turn per row with its message linked to it. Whether a row is also in a
//! local transcript is not decided here — `krowk_store::reconcile_ledger`
//! does that against the whole store after every import. The file is read
//! whole: turns are positional, and rows are few.

use crate::{
    check_os, decode_line, encode_cursor, home_path, jsonl_unchanged, open_home, read_jsonl, Env, ImportError, JsonlCursor,
    LineError, ReadResult, Ref, Source,
};
use krowk_store::{Binding, LedgerUsage, Message, Role, Session, Thread, Worktree, LEDGER_HARNESS};
use serde::Deserialize;
use serde_json::{json, Value};
use std::collections::HashSet;
use std::path::{Path, PathBuf};

const LEDGER_DIR: &str = "ledger";
const LEDGER_EXT: &str = "jsonl";
const VCS_NONE: &str = "none";

pub struct Ledger;

/// `ledger/` beside krowk.db, wherever the store resolves to.
pub fn ledger_dir(env: Env) -> Option<PathBuf> {
    krowk_store::db_path(env).and_then(|db| db.parent().map(|d| d.join(LEDGER_DIR)))
}

impl Source for Ledger {
    fn name(&self) -> &'static str {
        crate::PROVIDER_LEDGER
    }

    /// Every `*.jsonl` in the ledger directory, sorted by name. No directory
    /// is no ledger, which is the usual answer.
    fn discover(&self, env: Env) -> Result<Vec<Ref>, ImportError> {
        check_os()?;
        let Some(dir) = ledger_dir(env) else { return Ok(Vec::new()) };
        if std::fs::symlink_metadata(&dir).is_err() {
            return Ok(Vec::new());
        }
        let dir = home_path(env, &dir.display().to_string()).map_err(|e| context(e, "ledger: resolve directory"))?;
        let mut names: Vec<String> = std::fs::read_dir(&dir)
            .map_err(|e| ImportError::Other(format!("ledger: list {}: {e}", dir.display())))?
            .filter_map(Result::ok)
            .filter(|e| e.file_type().is_ok_and(|t| t.is_file()))
            .filter_map(|e| e.file_name().into_string().ok())
            .filter(|n| Path::new(n).extension().is_some_and(|x| x == LEDGER_EXT) && !n.starts_with('.'))
            .collect();
        names.sort();
        Ok(names
            .into_iter()
            .map(|n| Ref {
                provider: self.name().into(),
                id: n.trim_end_matches(&format!(".{LEDGER_EXT}")).to_string(),
                path: dir.join(&n).display().to_string(),
            })
            .collect())
    }

    fn read(&self, env: Env, r: &Ref, _cursor: &str) -> Result<(Thread, String, ReadResult), ImportError> {
        let (mut file, path) = open_home(env, &r.path, 0).map_err(|e| context(e, &format!("ledger: open {}", r.path)))?;
        let mut rows: Vec<Row> = Vec::new();
        let mut seen = HashSet::new();
        let (next, mut res) = read_jsonl(&mut file, JsonlCursor::default(), |_, raw| {
            let row = Row::parse(raw)?;
            // The provider's id is the execution; a second line naming it is
            // the same execution exported twice.
            if seen.insert(row.id.clone()) {
                rows.push(row);
            }
            Ok(())
        })
        .map_err(|e| context(e, &format!("ledger: read {}", r.path)))?;
        if rows.is_empty() {
            return Err(ImportError::Other(format!("ledger: {} holds no usage rows", r.path)));
        }
        res.classified.insert("usage".into(), rows.len());
        let dir = path.parent().map(|p| p.display().to_string()).unwrap_or_default();
        Ok((thread(r, &dir, &rows), encode_cursor(&next), res))
    }

    fn unchanged(&self, env: Env, r: &Ref, cursor: &str) -> bool {
        jsonl_unchanged(env, r, cursor)
    }
}

fn context(e: ImportError, what: &str) -> ImportError {
    match e {
        ImportError::Other(m) => ImportError::Other(format!("{what}: {m}")),
        other => other,
    }
}

/// One metered execution.
#[derive(Debug, Clone, PartialEq)]
struct Row {
    id: String,
    provider: String,
    model: String,
    time_ms: Option<i64>,
    usage: LedgerUsage,
    raw: String,
}

impl Row {
    fn parse(raw: &[u8]) -> Result<Row, LineError> {
        #[derive(Deserialize)]
        struct Wire {
            id: Option<String>,
            provider: Option<String>,
            model: Option<String>,
            time: Option<String>,
            time_ms: Option<i64>,
            input_tokens: Option<i64>,
            output_tokens: Option<i64>,
            reasoning_tokens: Option<i64>,
            cache_read_tokens: Option<i64>,
            cache_write_tokens: Option<i64>,
            cost_usd: Option<f64>,
        }
        let value = decode_line(raw).map_err(|_| LineError::Skip("invalid json".into()))?;
        let w: Wire = serde_json::from_value(value).map_err(|e| LineError::Skip(format!("not a usage row: {e}")))?;
        let text = |v: Option<String>, name: &str| {
            let v = v.unwrap_or_default().trim().to_string();
            if v.is_empty() { Err(LineError::Skip(format!("usage row has no {name}"))) } else { Ok(v) }
        };
        let (id, provider, model) = (text(w.id, "id")?, text(w.provider, "provider")?, text(w.model, "model")?);
        let count = |v: Option<i64>, name: &str| match v {
            Some(n) if n < 0 => Err(LineError::Skip(format!("usage row has a negative {name}"))),
            v => Ok(v.unwrap_or(0)),
        };
        let usage = LedgerUsage {
            input: count(w.input_tokens, "input_tokens")?,
            output: count(w.output_tokens, "output_tokens")?,
            reasoning: count(w.reasoning_tokens, "reasoning_tokens")?,
            cache_read: count(w.cache_read_tokens, "cache_read_tokens")?,
            cache_write: count(w.cache_write_tokens, "cache_write_tokens")?,
            cost_usd: match w.cost_usd {
                Some(c) if !c.is_finite() || c < 0.0 => return Err(LineError::Skip("usage row has a negative or non-finite cost_usd".into())),
                c => c,
            },
        };
        if usage.reasoning > usage.output {
            return Err(LineError::Skip("usage row has more reasoning_tokens than output_tokens, which include them".into()));
        }
        let time_ms = match (w.time_ms, w.time) {
            (Some(ms), _) => Some(ms),
            (None, Some(t)) => Some(
                t.parse::<jiff::Timestamp>().map_err(|e| LineError::Skip(format!("usage row time {t:?} is not RFC 3339: {e}")))?.as_millisecond(),
            ),
            (None, None) => None,
        };
        Ok(Row { id, provider, model, time_ms, usage, raw: String::from_utf8_lossy(raw).into_owned() })
    }

    /// The usage block the store reconciles from: the provider's counts,
    /// under the names `LedgerUsage` reads back.
    fn usage_json(&self) -> String {
        let u = &self.usage;
        let mut v = json!({
            "input_tokens": u.input,
            "output_tokens": u.output,
            "reasoning_tokens": u.reasoning,
            "cache_read_tokens": u.cache_read,
            "cache_write_tokens": u.cache_write,
        });
        if let Some(c) = u.cost_usd {
            v["cost_usd"] = json!(c);
        }
        if let Some(t) = self.time_ms {
            v["time_ms"] = json!(t);
        }
        Value::to_string(&v)
    }
}

fn thread(r: &Ref, dir: &str, rows: &[Row]) -> Thread {
    let first = &rows[0];
    Thread {
        worktree: Worktree { path: dir.to_string(), vcs: VCS_NONE.into(), name: LEDGER_DIR.into() },
        session: Session {
            directory: String::new(),
            title: format!("{} usage ledger ({})", first.provider, r.id),
            model: first.model.clone(),
            provider: first.provider.clone(),
            harness: LEDGER_HARNESS.into(),
        },
        binding: Binding { provider: LEDGER_HARNESS.into(), harness: LEDGER_HARNESS.into(), foreign_session_id: r.id.clone(), ..Binding::default() },
        turns: rows.iter().map(|row| row.usage.turn_columns()).collect(),
        messages: rows
            .iter()
            .enumerate()
            .map(|(i, row)| Message {
                role: Role::Assistant,
                provider: row.provider.clone(),
                model: row.model.clone(),
                foreign_id: row.id.clone(),
                usage: row.usage_json(),
                raw_json: Some(row.raw.clone()),
                turn_seq: Some(i as i64),
                parts: Vec::new(),
            })
            .collect(),
        ..Thread::default()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Tmp(PathBuf);
    impl Drop for Tmp {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.0);
        }
    }

    fn home(name: &str) -> (Tmp, impl Fn(&str) -> String) {
        let dir = std::env::temp_dir().join(format!("krowk-ledger-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let dir = dir.canonicalize().unwrap();
        let h = dir.display().to_string();
        (Tmp(dir), move |k: &str| if k == "HOME" { h.clone() } else { String::new() })
    }

    #[test]
    fn a_ledger_reads_one_turn_per_execution_and_refuses_what_it_cannot_count() {
        let (tmp, env) = home("read");
        assert!(Ledger.discover(&env).unwrap().is_empty(), "no directory is no ledger");
        let dir = tmp.0.join(".local/share/krowk/ledger");
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(
            dir.join("zen.jsonl"),
            [
                r#"{"id":"g1","provider":"opencode","model":"qwen3.5-plus","time":"2026-09-10T11:32:05Z","input_tokens":84,"output_tokens":3422,"reasoning_tokens":3405,"cost_usd":0.0041}"#,
                r#"{"id":"g1","provider":"opencode","model":"qwen3.5-plus","input_tokens":84,"output_tokens":3422}"#,
                r#"{"id":"g2","provider":"opencode","model":"glm-5.3-flash","input_tokens":26,"output_tokens":1854}"#,
                r#"{"provider":"opencode","model":"m","input_tokens":1}"#,
                r#"{"id":"g3","provider":"opencode","model":"m","input_tokens":-1}"#,
                r#"{"id":"g4","provider":"opencode","model":"m","output_tokens":1,"reasoning_tokens":2}"#,
                "",
            ]
            .join("\n"),
        )
        .unwrap();
        std::fs::write(dir.join("notes.txt"), "x").unwrap();
        let refs = Ledger.discover(&env).unwrap();
        assert_eq!(refs.iter().map(|r| r.key()).collect::<Vec<_>>(), vec!["ledger:zen".to_string()]);
        let (th, cursor, res) = Ledger.read(&env, &refs[0], "").unwrap();
        assert_eq!((th.turns.len(), th.messages.len(), res.skipped_count), (2, 2, 3));
        assert_eq!(th.binding.foreign_session_id, "zen");
        assert_eq!(th.session.harness, LEDGER_HARNESS);
        assert_eq!((th.turns[0].cost_output, th.turns[0].cost_reasoning, th.turns[0].cost_usd_micros), (17, 3405, Some(4100)));
        assert_eq!(th.turns[1].cost_usd_micros, None, "no stated cost is priced at read time, never a silent 0");
        assert_eq!(th.messages[1].turn_seq, Some(1));
        let u: Value = serde_json::from_str(&th.messages[0].usage).unwrap();
        assert_eq!(u["time_ms"], 1_789_039_925_000_i64);
        assert!(Ledger.unchanged(&env, &refs[0], &cursor));

        std::fs::write(dir.join("empty.jsonl"), "\n").unwrap();
        let empty = Ledger.discover(&env).unwrap().into_iter().find(|r| r.id == "empty").unwrap();
        assert!(Ledger.read(&env, &empty, "").is_err(), "a file with no rows is a failure to report, not an empty session");
    }
}
