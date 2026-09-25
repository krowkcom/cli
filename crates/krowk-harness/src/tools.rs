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
            description: "Run a shell command with `bash -c` in the working directory and return its combined stdout and stderr, followed by the exit code. Output beyond 30000 bytes is cut from the middle. Commands time out after 120 seconds unless `timeout_ms` says otherwise.".into(),
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
            Ok(i) => read(&i, env.cwd).await,
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

/// Read scans at most this much of a file: a line count past it is not worth
/// the wait, and nothing past it is shown.
const READ_MAX_SCAN: u64 = 64 << 20;

/// Off the async runtime: a read is blocking file I/O, and the runtime also
/// has to hear an interrupt while it runs.
async fn read(i: &ReadInput, cwd: &Path) -> (String, bool) {
    let path = resolve(cwd, &i.path);
    // Both come from the model: clamped, so no value overflows the arithmetic.
    let (start, limit) = (i.offset.unwrap_or(1).clamp(1, usize::MAX / 2), i.limit.unwrap_or(READ_DEFAULT_LINES).clamp(1, usize::MAX / 2));
    tokio::task::spawn_blocking(move || read_file(&path, start, limit))
        .await
        .unwrap_or_else(|e| (format!("read failed: {e}"), true))
}

/// Opens only a regular file, and never blocks opening it: a FIFO, a device
/// or `/dev/stdin` put where a file was expected is refused, not waited on
/// or read forever. The check is repeated on the open handle, so a path
/// swapped between the two cannot slip one through.
fn open_regular(path: &Path) -> Result<(std::fs::File, u64), String> {
    let not_regular = || format!("{} is not a regular file (a directory, a device or a pipe), which read does not open", path.display());
    match std::fs::metadata(path) {
        Ok(m) if m.is_file() => {}
        Ok(_) => return Err(not_regular()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Err(format!("{} does not exist", path.display())),
        Err(e) => return Err(format!("{} could not be read: {e}", path.display())),
    }
    let mut o = std::fs::OpenOptions::new();
    o.read(true);
    #[cfg(unix)]
    std::os::unix::fs::OpenOptionsExt::custom_flags(&mut o, libc::O_NONBLOCK);
    let f = o.open(path).map_err(|e| format!("{} could not be read: {e}", path.display()))?;
    let meta = f.metadata().map_err(|e| format!("{} could not be read: {e}", path.display()))?;
    if !meta.is_file() {
        return Err(not_regular());
    }
    Ok((f, meta.len()))
}

fn read_file(path: &Path, start: usize, limit: usize) -> (String, bool) {
    use std::io::{BufRead, Read};
    let (file, size) = match open_regular(path) {
        Ok(f) => f,
        Err(e) => return (e, true),
    };
    let mut r = std::io::BufReader::with_capacity(64 << 10, file.take(READ_MAX_SCAN));
    let (mut out, mut total, mut last, mut full) = (String::new(), 0usize, start - 1, false);
    let mut line = Vec::new();
    loop {
        // One line, keeping at most what a shown line can use: a minified
        // file's one line costs its first few kilobytes of memory, not all of it.
        line.clear();
        let mut consumed = 0usize;
        loop {
            let buf = match r.fill_buf() {
                Ok(b) => b,
                Err(e) => return (format!("{} could not be read: {e}", path.display()), true),
            };
            if buf.is_empty() {
                break;
            }
            let (chunk, done) = match buf.iter().position(|b| *b == b'\n') {
                Some(i) => (&buf[..=i], true),
                None => (buf, false),
            };
            let keep = (READ_MAX_LINE * 4).saturating_sub(line.len()).min(chunk.len());
            line.extend_from_slice(&chunk[..keep]);
            let n = chunk.len();
            consumed += n;
            r.consume(n);
            if done {
                break;
            }
        }
        if consumed == 0 {
            break;
        }
        if line.contains(&0) {
            return (format!("{} is a binary file, which read does not show", path.display()), true);
        }
        total += 1;
        if total < start || total >= start.saturating_add(limit) || full {
            continue;
        }
        while matches!(line.last(), Some(b'\n' | b'\r')) {
            line.pop();
        }
        let text = String::from_utf8_lossy(&line);
        let text = if text.chars().count() > READ_MAX_LINE { text.chars().take(READ_MAX_LINE).collect::<String>() + "…" } else { text.into_owned() };
        let row = format!("{total:>6}\t{text}\n");
        if out.len() + row.len() > READ_MAX_BYTES {
            full = true;
            continue;
        }
        out += &row;
        last = total;
    }
    let scanned_all = size <= READ_MAX_SCAN;
    let count = if scanned_all { format!("{total} lines") } else { format!("more than {total} lines (read scans the first {} MB)", READ_MAX_SCAN >> 20) };
    if total == 0 {
        return (format!("{} is empty", path.display()), false);
    }
    if start > total {
        return (format!("{} has {count}, so there is nothing from line {start}", path.display()), true);
    }
    if last < total || !scanned_all {
        out += &format!("\n({} has {count}; this is {start}–{last}. Read on with offset {}.)\n", path.display(), last + 1);
    }
    (out, false)
}

/// How long the output is still read once the shell has exited: long enough
/// for what it wrote last, short enough that a process it left in the
/// background — which holds the pipes open — does not hold the call.
const BASH_DRAIN_AFTER_EXIT: Duration = Duration::from_millis(250);

/// A bounded capture of a stream: the first half of the budget kept whole,
/// the last half as a ring, and a count of what fell in between — so a
/// command that prints gigabytes costs 30 KB.
struct Capture {
    head: Vec<u8>,
    tail: std::collections::VecDeque<u8>,
    dropped: u64,
    half: usize,
}

impl Capture {
    fn new(max: usize) -> Capture {
        Capture { head: Vec::new(), tail: std::collections::VecDeque::new(), dropped: 0, half: max / 2 }
    }

    fn push(&mut self, mut b: &[u8]) {
        let room = self.half - self.head.len();
        if room > 0 {
            let n = room.min(b.len());
            self.head.extend_from_slice(&b[..n]);
            b = &b[n..];
        }
        self.tail.extend(b);
        while self.tail.len() > self.half {
            let over = self.tail.len() - self.half;
            self.tail.drain(..over);
            self.dropped += over as u64;
        }
    }

    fn render(&self) -> String {
        let (a, b) = self.tail.as_slices();
        let tail: Vec<u8> = [a, b].concat();
        if self.dropped == 0 {
            return String::from_utf8_lossy(&[self.head.as_slice(), &tail].concat()).into_owned();
        }
        format!("{}\n… {} bytes cut …\n{}", String::from_utf8_lossy(&self.head), self.dropped, String::from_utf8_lossy(&tail))
    }
}

/// Kills the command's whole process group when dropped armed: on a timeout,
/// and when the turn is interrupted and the call's future is dropped — so
/// what the command started dies with it, not just the shell.
struct GroupKill(Option<u32>);

impl Drop for GroupKill {
    fn drop(&mut self) {
        #[cfg(unix)]
        if let Some(pid) = self.0 {
            // SAFETY: kill(2) on the group this call created; a group that
            // already exited is ESRCH, which is fine.
            unsafe {
                libc::kill(-(pid as i32), libc::SIGKILL);
            }
        }
    }
}

async fn bash(i: &BashInput, cwd: &Path) -> (String, bool) {
    use tokio::io::AsyncReadExt;
    let timeout = i.timeout_ms.map_or(BASH_DEFAULT_TIMEOUT, Duration::from_millis).min(BASH_MAX_TIMEOUT);
    let mut cmd = tokio::process::Command::new("bash");
    cmd.arg("-c").arg(&i.command).current_dir(cwd);
    cmd.stdin(std::process::Stdio::null()).stdout(std::process::Stdio::piped()).stderr(std::process::Stdio::piped());
    cmd.kill_on_drop(true);
    // Its own process group, so a timeout or an interrupt kills what the
    // command started too, not just the shell.
    #[cfg(unix)]
    cmd.process_group(0);
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => return (format!("bash could not be started: {e}"), true),
    };
    let mut group = GroupKill(child.id());
    let (mut so, mut se) = (child.stdout.take().expect("piped"), child.stderr.take().expect("piped"));
    let mut cap = Capture::new(BASH_MAX_OUTPUT);
    let run = async {
        let (mut b1, mut b2) = ([0u8; 8192], [0u8; 8192]);
        let (mut so_open, mut se_open) = (true, true);
        let mut status = None;
        let mut held_open = false;
        // Fixed once, when the shell exits: a background process that keeps
        // writing must not push the window out chunk by chunk.
        let mut drain_until: Option<tokio::time::Instant> = None;
        loop {
            if !so_open && !se_open && status.is_some() {
                break;
            }
            let drained = async {
                match drain_until {
                    Some(at) => tokio::time::sleep_until(at).await,
                    None => std::future::pending().await,
                }
            };
            tokio::select! {
                r = so.read(&mut b1), if so_open => match r {
                    Ok(n) if n > 0 => cap.push(&b1[..n]),
                    _ => so_open = false,
                },
                r = se.read(&mut b2), if se_open => match r {
                    Ok(n) if n > 0 => cap.push(&b2[..n]),
                    _ => se_open = false,
                },
                s = child.wait(), if status.is_none() => {
                    status = Some(s);
                    drain_until = Some(tokio::time::Instant::now() + BASH_DRAIN_AFTER_EXIT);
                }
                _ = drained => {
                    held_open = true;
                    break;
                }
            }
        }
        (status.and_then(|s| s.ok()).and_then(|s| s.code()), held_open)
    };
    match tokio::time::timeout(timeout, run).await {
        Ok((code, held_open)) => {
            // Finished: what it left in the background is its business.
            group.0 = None;
            let text = cap.render();
            let mut tail = match code {
                Some(c) => format!("exit code {c}"),
                None => "killed by a signal".into(),
            };
            if held_open {
                tail += " (a process it started in the background still holds its output; krowk stopped reading when the shell exited)";
            }
            let body = if text.is_empty() { tail.clone() } else { format!("{}\n{tail}", text.trim_end_matches('\n')) };
            (body, code != Some(0))
        }
        Err(_) => {
            drop(group);
            (format!("the command timed out after {} ms and was killed", timeout.as_millis()), true)
        }
    }
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
        // Offsets and limits the model makes up cannot overflow.
        let max = usize::MAX as u64;
        assert_eq!(run(READ, &json!({"path": "a.txt", "offset": 2, "limit": max}), &env).await, ("     2\ttwo\n     3\tthree\n".into(), false));
        let (out, err) = run(READ, &json!({"path": "a.txt", "offset": max, "limit": max}), &env).await;
        assert!(err && out.contains("nothing from line"), "{out}");
        let _ = std::fs::remove_dir_all(d);
    }

    #[cfg(unix)]
    #[tokio::test(flavor = "current_thread")]
    async fn read_refuses_what_is_not_a_regular_file_without_blocking_or_reading_it_whole() {
        let d = dir("read-special");
        let fifo = d.join("fifo");
        let made = std::process::Command::new("mkfifo").arg(&fifo).status().unwrap();
        assert!(made.success());
        let env = ToolEnv { cwd: &d, permission_mode: PermissionMode::Default };
        for path in [fifo.display().to_string(), "/dev/zero".into(), "/dev/stdin".into(), d.display().to_string()] {
            let r = tokio::time::timeout(Duration::from_secs(2), run(READ, &json!({ "path": path }), &env)).await.expect("read never blocks");
            assert!(r.1 && r.0.contains("not a regular file"), "{path}: {r:?}");
        }
        // A long line costs its shown part, and the page stays capped.
        std::fs::write(d.join("wide"), "x".repeat(5 << 20) + "\nsecond\n").unwrap();
        let (out, err) = run(READ, &json!({"path": "wide"}), &env).await;
        assert!(!err && out.len() < 10_000 && out.contains("     2\tsecond"), "{}", out.len());
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
        assert!(out.contains("bytes cut") && out.len() < 31_000);
        // Output is bounded however much there is, not buffered whole: 200 MB
        // of it. (`yes | head`, not `timeout 1 yes`: macOS has no timeout.)
        let (out, _) = run(BASH, &json!({"command": "yes | head -c 200000000"}), &env).await;
        assert!(out.contains("bytes cut") && out.len() < 31_000, "{}", out.len());
        // A process left in the background holds the pipes; the call still
        // ends when the shell does.
        let started = std::time::Instant::now();
        let (out, err) = run(BASH, &json!({"command": "sleep 5 & echo started"}), &env).await;
        assert!(!err && out.starts_with("started\nexit code 0"), "{out}");
        assert!(started.elapsed() < Duration::from_secs(2));
        // Even one that keeps writing: the window after exit is fixed, not
        // restarted by each chunk.
        let started = std::time::Instant::now();
        let (out, err) = run(BASH, &json!({"command": "(while :; do echo x; sleep 0.1; done) & echo ok", "timeout_ms": 10000}), &env).await;
        assert!(!err && out.starts_with("ok\n"), "{out}");
        assert!(started.elapsed() < Duration::from_secs(2), "{:?}", started.elapsed());
        let _ = std::fs::remove_dir_all(d);
    }

    #[cfg(unix)]
    #[tokio::test(flavor = "current_thread")]
    async fn an_interrupted_bash_call_kills_what_the_command_started() {
        let d = dir("bash-cancel");
        let env = ToolEnv { cwd: &d, permission_mode: PermissionMode::BypassPermissions };
        let input = json!({"command": "sleep 30 & echo $! > grandchild; wait"});
        let call = run(BASH, &input, &env);
        // Dropped mid-run, as the loop drops a call when the turn is interrupted.
        assert!(tokio::time::timeout(Duration::from_millis(500), call).await.is_err());
        let pid: i32 = std::fs::read_to_string(d.join("grandchild")).unwrap().trim().parse().unwrap();
        let mut alive = true;
        for _ in 0..40 {
            // SAFETY: signal 0 only asks whether the process exists.
            alive = unsafe { libc::kill(pid, 0) } == 0 && !std::fs::read_to_string(format!("/proc/{pid}/stat")).is_ok_and(|s| s.contains(") Z "));
            if !alive {
                break;
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        assert!(!alive, "the backgrounded sleep {pid} outlived the interrupted call");
        let _ = std::fs::remove_dir_all(d);
    }
}
