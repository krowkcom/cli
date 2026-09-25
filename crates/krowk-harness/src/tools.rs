//! The tools the native loop offers: `read` and `bash`, for now. Each input
//! is a Rust type the tool's JSON Schema is derived from, so the definition
//! the model sees and the parser that reads its call cannot disagree.
//!
//! A tool never fails the turn. A bad input, a missing file, a timeout or a
//! refusal is a result with `isError`, which the model reads and corrects.
//!
//! `bash` runs only under `--permission-mode bypassPermissions` until the
//! permission system lands (ticket 9): anywhere else the call is refused
//! with a result that says so. `read` needs no permission, as in every
//! harness whose rules krowk follows.

use crate::protocol::{PermissionMode, ToolDefinition};
use schemars::JsonSchema;
use serde::Deserialize;
use serde_json::Value;
use std::path::{Path, PathBuf};
use std::time::Duration;
use tokio::io::AsyncReadExt;

pub const READ: &str = "read";
pub const BASH: &str = "bash";

/// A read returns at most this many lines unless asked for fewer.
const READ_DEFAULT_LINES: usize = 2000;
/// And at most this many bytes of them, whatever the line count.
const READ_MAX_BYTES: usize = 256 << 10;
/// A line longer than this is cut, so one minified file cannot fill the
/// context on its own.
const READ_MAX_LINE: usize = 2000;
const BASH_DEFAULT_TIMEOUT: Duration = Duration::from_secs(120);
const BASH_MAX_TIMEOUT: Duration = Duration::from_secs(600);
/// Output beyond this is cut from the middle: the start says what ran, the
/// end says how it finished.
const BASH_MAX_OUTPUT: usize = 30_000;

/// Read a file from the filesystem.
#[derive(Debug, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct ReadInput {
    /// The file to read: absolute, or relative to the working directory.
    pub path: String,
    /// The first line to return, counting from 1.
    #[serde(default)]
    pub offset: Option<usize>,
    /// How many lines to return (at most 2000 by default).
    #[serde(default)]
    pub limit: Option<usize>,
}

/// Run a shell command.
#[derive(Debug, Deserialize, JsonSchema)]
#[serde(deny_unknown_fields)]
pub struct BashInput {
    /// The command, run by `bash -c` in the working directory.
    pub command: String,
    /// Milliseconds before the command is killed: 120000 by default, at most 600000.
    #[serde(default)]
    pub timeout_ms: Option<u64>,
}

/// The definitions, in the order the model is shown them. The order is part
/// of the cached prefix, so it never varies.
pub fn definitions() -> Vec<ToolDefinition> {
    vec![
        ToolDefinition {
            name: READ.into(),
            description: "Read a text file. Returns its lines numbered from 1, like `cat -n`, 2000 lines at most unless `limit` says otherwise; use `offset` to read further. Prefer this to running `cat` through bash.".into(),
            input_schema: input_schema::<ReadInput>(),
        },
        ToolDefinition {
            name: BASH.into(),
            description: "Run a shell command with `bash -c` in the working directory and return its combined stdout and stderr, followed by the exit code. Output beyond 30000 characters is cut from the middle. Commands time out after 120 seconds unless `timeout_ms` says otherwise.".into(),
            input_schema: input_schema::<BashInput>(),
        },
    ]
}

/// The schema of a tool's input, as the Anthropic API wants it: an object
/// schema, without the `$schema` and `title` noise schemars adds.
fn input_schema<T: JsonSchema>() -> Value {
    let mut v = schemars::schema_for!(T).to_value();
    if let Value::Object(m) = &mut v {
        m.remove("$schema");
        m.remove("title");
        m.remove("description");
    }
    v
}

/// Everything a tool call is run with.
pub struct ToolEnv<'a> {
    pub cwd: &'a Path,
    pub permission_mode: PermissionMode,
}

/// Runs one call. Returns the output and whether it is an error.
pub async fn run(name: &str, input: &Value, env: &ToolEnv<'_>) -> (String, bool) {
    match name {
        READ => match ReadInput::deserialize(input) {
            Ok(i) => read(&i, env.cwd),
            Err(e) => (format!("invalid input for read: {e}"), true),
        },
        BASH => match BashInput::deserialize(input) {
            Ok(i) if env.permission_mode == PermissionMode::BypassPermissions => bash(&i, env.cwd).await,
            Ok(_) => (
                "bash is not allowed in this session: until krowk's permission rules land, bash runs only when krowk is started with `--permission-mode bypassPermissions`. Use the read tool, or ask the person to rerun with that flag.".into(),
                true,
            ),
            Err(e) => (format!("invalid input for bash: {e}"), true),
        },
        other => (format!("there is no tool named {other:?} — the tools are read and bash"), true),
    }
}

fn resolve(cwd: &Path, path: &str) -> PathBuf {
    let p = Path::new(path);
    if p.is_absolute() { p.to_path_buf() } else { cwd.join(p) }
}

fn read(i: &ReadInput, cwd: &Path) -> (String, bool) {
    let path = resolve(cwd, &i.path);
    let data = match std::fs::read(&path) {
        Ok(d) => d,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return (format!("{} does not exist", path.display()), true),
        Err(e) => return (format!("{} could not be read: {e}", path.display()), true),
    };
    if data.contains(&0) {
        return (format!("{} is a binary file, which read does not show", path.display()), true);
    }
    let text = String::from_utf8_lossy(&data);
    let start = i.offset.unwrap_or(1).max(1);
    let limit = i.limit.unwrap_or(READ_DEFAULT_LINES).max(1);
    let total = text.lines().count();
    if total == 0 {
        return (format!("{} is empty", path.display()), false);
    }
    if start > total {
        return (format!("{} has {total} lines, so there is nothing from line {start}", path.display()), true);
    }
    let mut out = String::new();
    let mut last = start - 1;
    for (n, line) in text.lines().enumerate().skip(start - 1).take(limit) {
        let line = if line.chars().count() > READ_MAX_LINE { line.chars().take(READ_MAX_LINE).collect::<String>() + "…" } else { line.to_string() };
        let row = format!("{:>6}\t{line}\n", n + 1);
        if out.len() + row.len() > READ_MAX_BYTES {
            break;
        }
        out += &row;
        last = n + 1;
    }
    if last < total {
        out += &format!("\n({} has {total} lines; this is {start}–{last}. Read on with offset {}.)\n", path.display(), last + 1);
    }
    (out, false)
}

async fn bash(i: &BashInput, cwd: &Path) -> (String, bool) {
    let timeout = i.timeout_ms.map_or(BASH_DEFAULT_TIMEOUT, Duration::from_millis).min(BASH_MAX_TIMEOUT);
    let mut cmd = tokio::process::Command::new("bash");
    cmd.arg("-c").arg(&i.command).current_dir(cwd);
    cmd.stdin(std::process::Stdio::null()).stdout(std::process::Stdio::piped()).stderr(std::process::Stdio::piped());
    cmd.kill_on_drop(true);
    // Its own process group, so a timeout kills what the command started
    // too, not just the shell.
    #[cfg(unix)]
    cmd.process_group(0);
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => return (format!("bash could not be started: {e}"), true),
    };
    let (mut so, mut se) = (child.stdout.take().expect("piped"), child.stderr.take().expect("piped"));
    let pid = child.id();
    let run = async {
        let (mut out, mut err) = (Vec::new(), Vec::new());
        let (a, b, status) = tokio::join!(so.read_to_end(&mut out), se.read_to_end(&mut err), child.wait());
        let _ = (a, b);
        (out, err, status)
    };
    match tokio::time::timeout(timeout, run).await {
        Ok((out, err, status)) => {
            let mut text = String::from_utf8_lossy(&out).into_owned();
            if !err.is_empty() {
                if !text.is_empty() && !text.ends_with('\n') {
                    text.push('\n');
                }
                text += &String::from_utf8_lossy(&err);
            }
            let code = status.ok().and_then(|s| s.code());
            let text = cut_middle(&text, BASH_MAX_OUTPUT);
            let tail = match code {
                Some(c) => format!("exit code {c}"),
                None => "killed by a signal".into(),
            };
            let body = if text.is_empty() { tail.clone() } else { format!("{}\n{tail}", text.trim_end_matches('\n')) };
            (body, code != Some(0))
        }
        Err(_) => {
            #[cfg(unix)]
            if let Some(pid) = pid {
                // SAFETY: kill(2) on the group this call created; a group
                // that already exited is ESRCH, which is fine.
                unsafe {
                    libc::kill(-(pid as i32), libc::SIGKILL);
                }
            }
            let _ = pid;
            (format!("the command timed out after {} ms and was killed", timeout.as_millis()), true)
        }
    }
}

fn cut_middle(s: &str, max: usize) -> String {
    if s.chars().count() <= max {
        return s.to_string();
    }
    let chars: Vec<char> = s.chars().collect();
    let half = max / 2;
    let cut = chars.len() - 2 * half;
    format!(
        "{}\n… {cut} characters cut …\n{}",
        chars[..half].iter().collect::<String>(),
        chars[chars.len() - half..].iter().collect::<String>()
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn dir(name: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("krowk-harness-tools-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    #[test]
    fn tool_definitions_are_derived_object_schemas_in_a_fixed_order() {
        let defs = definitions();
        assert_eq!(defs.iter().map(|d| d.name.as_str()).collect::<Vec<_>>(), [READ, BASH]);
        assert_eq!(defs[0].input_schema["type"], "object");
        assert_eq!(defs[0].input_schema["required"], json!(["path"]));
        assert!(defs[1].input_schema["properties"]["timeout_ms"].is_object());
        assert_eq!(definitions(), defs, "deterministic: the definitions are part of the cached prefix");
    }

    #[tokio::test(flavor = "current_thread")]
    async fn read_numbers_lines_pages_and_refuses_what_it_cannot_show() {
        let d = dir("read");
        std::fs::write(d.join("a.txt"), "one\ntwo\nthree\n").unwrap();
        std::fs::write(d.join("bin"), [0u8, 1, 2]).unwrap();
        let env = ToolEnv { cwd: &d, permission_mode: PermissionMode::Default };
        let (out, err) = run(READ, &json!({"path": "a.txt"}), &env).await;
        assert!(!err);
        assert_eq!(out, "     1\tone\n     2\ttwo\n     3\tthree\n");
        let (out, _) = run(READ, &json!({"path": "a.txt", "offset": 2, "limit": 1}), &env).await;
        assert!(out.starts_with("     2\ttwo\n") && out.contains("offset 3"), "{out}");
        assert!(run(READ, &json!({"path": "missing"}), &env).await.1);
        assert!(run(READ, &json!({"path": "bin"}), &env).await.1);
        assert!(run(READ, &json!({"file": "a.txt"}), &env).await.0.contains("invalid input"));
        let _ = std::fs::remove_dir_all(d);
    }

    #[tokio::test(flavor = "current_thread")]
    async fn bash_runs_only_when_permissions_are_bypassed_and_is_bounded() {
        let d = dir("bash");
        let refused = run(BASH, &json!({"command": "echo hi"}), &ToolEnv { cwd: &d, permission_mode: PermissionMode::Default }).await;
        assert!(refused.1 && refused.0.contains("bypassPermissions"), "{refused:?}");
        let env = ToolEnv { cwd: &d, permission_mode: PermissionMode::BypassPermissions };
        assert_eq!(run(BASH, &json!({"command": "echo hi; echo oops >&2"}), &env).await, ("hi\noops\nexit code 0".into(), false));
        assert_eq!(run(BASH, &json!({"command": "exit 3"}), &env).await, ("exit code 3".into(), true));
        let started = std::time::Instant::now();
        let (out, err) = run(BASH, &json!({"command": "sleep 5 & sleep 5", "timeout_ms": 200}), &env).await;
        assert!(err && out.contains("timed out"), "{out}");
        assert!(started.elapsed() < Duration::from_secs(3), "the whole group was killed");
        let (out, _) = run(BASH, &json!({"command": "head -c 40000 /dev/zero | tr '\\0' x"}), &env).await;
        assert!(out.contains("characters cut") && out.len() < 31_000);
        let _ = std::fs::remove_dir_all(d);
    }
}
