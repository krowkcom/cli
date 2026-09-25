//! The measurements, one per enforced budget id. Each runs the real built
//! binary (or, for the log, the harness's own writer) and returns the median
//! of its runs; none of them compares against an earlier run.

use crate::budgets::{Outcome, median};
use krowk_harness::log::SessionLog;
use krowk_harness::protocol::{Item, LogBody};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::Instant;

pub fn size(bin: &Path) -> Outcome {
    match std::fs::metadata(bin) {
        Ok(m) => Outcome::Measured { value: m.len() as f64, note: format!("{} bytes", m.len()) },
        Err(e) => Outcome::Error(format!("{}: {e}", bin.display())),
    }
}

/// The agent build's crates, resolved for every target — the same set
/// scripts/lean_deps_check.sh holds to crates/krowk/lean-deps.txt by name.
pub fn lean_deps() -> Outcome {
    let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".into());
    let out = Command::new(cargo).args(["tree", "-p", "krowk", "-e", "normal", "--prefix", "none", "--locked", "--target", "all"]).output();
    match out {
        Ok(o) if o.status.success() => {
            let text = String::from_utf8_lossy(&o.stdout);
            let crates: std::collections::BTreeSet<&str> = text.lines().filter_map(|l| l.split_whitespace().next()).collect();
            Outcome::Measured { value: crates.len() as f64, note: String::new() }
        }
        Ok(o) => Outcome::Error(format!("cargo tree: {}", String::from_utf8_lossy(&o.stderr).trim())),
        Err(e) => Outcome::Error(format!("cargo tree: {e}")),
    }
}

/// A clean environment for a measured process: its own home and data
/// directories, so neither the person's config nor their sessions change
/// what is measured.
pub fn sandboxed(bin: &Path, home: &Path) -> Command {
    let mut c = Command::new(bin);
    c.env_clear()
        .env("HOME", home)
        .env("XDG_DATA_HOME", home.join("data"))
        .env("XDG_CONFIG_HOME", home.join("config"))
        .env("XDG_CACHE_HOME", home.join("cache"))
        .env("PATH", std::env::var_os("PATH").unwrap_or_default())
        .current_dir(home)
        .stdin(Stdio::null());
    c
}

/// Wall time from spawn to exit, the median of `runs` new processes. One run
/// is made first and thrown away: it creates what a first run creates (an
/// empty krowk.db) and pulls the binary into the page cache, which a runner
/// cannot drop without root. "Cold" is the process — no daemon, nothing
/// loaded — not the disk.
pub fn startup(bin: &Path, args: &[&str], runs: usize, home: &Path) -> Outcome {
    if let Err(e) = std::fs::create_dir_all(home) {
        return Outcome::Error(format!("{}: {e}", home.display()));
    }
    let once = || -> Result<f64, String> {
        let t = Instant::now();
        let st = sandboxed(bin, home).args(args).stdout(Stdio::null()).stderr(Stdio::null()).status().map_err(|e| format!("{}: {e}", bin.display()))?;
        let ms = t.elapsed().as_secs_f64() * 1000.0;
        if !st.success() {
            return Err(format!("`krowk {}` exited {st}", args.join(" ")));
        }
        Ok(ms)
    };
    if let Err(e) = once() {
        return Outcome::Error(e);
    }
    let mut xs = Vec::with_capacity(runs);
    for _ in 0..runs.max(1) {
        match once() {
            Ok(ms) => xs.push(ms),
            Err(e) => return Outcome::Error(e),
        }
    }
    let (lo, hi) = spread(&mut xs);
    Outcome::Measured { value: median(&mut xs), note: format!("{} runs, {lo:.1}–{hi:.1}", xs.len()) }
}

fn spread(xs: &mut [f64]) -> (f64, f64) {
    xs.sort_by(f64::total_cmp);
    (xs[0], xs[xs.len() - 1])
}

/// One event appended through `SessionLog`, as a turn appends them: a
/// completed assistant message of 1 KiB, a line to a file opened O_APPEND,
/// no sync (the log syncs once per turn). The median of `runs` appends, in
/// microseconds, with the 99th percentile beside it.
pub fn log_append(work: &Path, runs: usize) -> Outcome {
    let sessions = work.join("log-append");
    let _ = std::fs::remove_dir_all(&sessions);
    let (mut log, _) = match SessionLog::create(&sessions, work, "0.0.0-bench") {
        Ok(l) => l,
        Err(e) => return Outcome::Error(e.message().to_string()),
    };
    let text = "The quick brown fox jumps over the lazy dog. ".repeat(23);
    let mut xs = Vec::with_capacity(runs);
    for i in 0..runs.max(1) {
        let body = LogBody::ItemCompleted { turn_id: "turn-bench".into(), item_id: format!("item-{i}"), item: Item::AssistantText { text: text.clone() } };
        let t = Instant::now();
        if let Err(e) = log.append(body) {
            return Outcome::Error(e.message().to_string());
        }
        xs.push(t.elapsed().as_secs_f64() * 1e6);
    }
    xs.sort_by(f64::total_cmp);
    let p99 = xs[(xs.len() * 99 / 100).min(xs.len() - 1)];
    Outcome::Measured { value: median(&mut xs), note: format!("{} events, p99 {p99:.1} µs", xs.len()) }
}

/// What a process did while it sat idle for the window.
#[derive(Debug, Clone)]
// Only Linux can read one; elsewhere the idle budgets are skipped.
#[cfg_attr(not(target_os = "linux"), allow(dead_code))]
pub struct Idle {
    /// utime + stime, in clock ticks, over the window.
    pub ticks: u64,
    /// Context switches of every thread over the window: each one a wakeup.
    pub wakeups: u64,
    /// Resident set at the end of the window, in MB.
    pub rss_mb: f64,
}

/// Where the measured processes keep their files: under target/, on the
/// same disk the build is on rather than a tmpfs, so the log is written the
/// way a real one is.
pub fn fresh_dir(work: &Path, name: &str) -> PathBuf {
    let d = work.join(name);
    let _ = std::fs::remove_dir_all(&d);
    d
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn r_perf_5_an_append_is_measured_through_the_session_log() {
        let dir = std::env::temp_dir().join(format!("krowk-bench-test-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let Outcome::Measured { value, note } = log_append(&dir, 50) else { panic!("the log append was not measured") };
        assert!(value > 0.0 && note.starts_with("50 events"), "{value} {note}");
        let sessions = dir.join("log-append");
        let session = std::fs::read_dir(&sessions).unwrap().next().unwrap().unwrap().path();
        let lines = std::fs::read_to_string(session.join("events.jsonl")).unwrap().lines().count();
        assert_eq!(lines, 51, "the root and fifty appends");
        let _ = std::fs::remove_dir_all(&dir);
    }
}
