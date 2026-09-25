//! The idle checks (R-PERF-2, R-PERF-3), read from Linux /proc: a process
//! that is waiting on nothing but an event should be charged no CPU and
//! woken not once. Linux only; elsewhere the budgets are reported skipped,
//! and the pinned runner, which is Linux, holds them.

use crate::measure::{Idle, sandboxed};
use std::io::Read;
use std::net::TcpListener;
use std::path::Path;
use std::process::Stdio;
use std::sync::mpsc;
use std::time::{Duration, Instant};

/// `krowk -p` with a turn in flight against a provider that takes the
/// request and never answers: the engine waiting on the network, the most
/// idle a turn gets. Sampled from /proc once the request has arrived, then
/// again after `window`; anything the process did in between is polling.
pub fn engine_idle(bin: &Path, home: &Path, window: Duration) -> Result<Idle, String> {
    std::fs::create_dir_all(home).map_err(|e| format!("{}: {e}", home.display()))?;
    let listener = TcpListener::bind("127.0.0.1:0").map_err(|e| format!("bind the silent provider: {e}"))?;
    let url = format!("http://{}", listener.local_addr().map_err(|e| e.to_string())?);
    let (arrived, request) = mpsc::channel();
    // The connection is held open, unanswered, for as long as the thread
    // lives; it ends with the bench.
    std::thread::spawn(move || {
        if let Ok((mut conn, _)) = listener.accept() {
            let mut got = Vec::new();
            let mut buf = [0u8; 16 * 1024];
            while !request_complete(&got) {
                match conn.read(&mut buf) {
                    Ok(0) | Err(_) => return,
                    Ok(n) => got.extend_from_slice(&buf[..n]),
                }
            }
            let _ = arrived.send(());
            std::thread::park();
            drop(conn);
        }
    });
    let stderr = home.join("stderr");
    let mut child = sandboxed(bin, home)
        .args(["-p", "say hello"])
        .env("ANTHROPIC_API_KEY", "sk-bench")
        .env("ANTHROPIC_BASE_URL", &url)
        .stdout(Stdio::null())
        .stderr(std::fs::File::create(&stderr).map_err(|e| e.to_string())?)
        .spawn()
        .map_err(|e| format!("{}: {e}", bin.display()))?;
    let pid = child.id();
    let result = (|| {
        let deadline = Instant::now() + Duration::from_secs(30);
        loop {
            if request.recv_timeout(Duration::from_millis(50)).is_ok() {
                break;
            }
            if let Ok(Some(st)) = child.try_wait() {
                return Err(format!("`krowk -p` exited {st} before its request arrived: {}", std::fs::read_to_string(&stderr).unwrap_or_default().trim()));
            }
            if Instant::now() > deadline {
                return Err("`krowk -p` sent no request within 30 s".into());
            }
        }
        // The request is out; what is left of startup (the write returning,
        // the await parking) settles well inside this.
        std::thread::sleep(Duration::from_millis(500));
        let (t0, w0) = (proc_ticks(pid)?, proc_switches(pid)?);
        std::thread::sleep(window);
        let (t1, w1) = (proc_ticks(pid)?, proc_switches(pid)?);
        let rss_mb = proc_rss_kib(pid)? as f64 * 1024.0 / 1e6;
        if let Ok(Some(st)) = child.try_wait() {
            return Err(format!("`krowk -p` exited {st} while it should have been waiting"));
        }
        Ok(Idle { ticks: t1.saturating_sub(t0), wakeups: w1.saturating_sub(w0), rss_mb })
    })();
    let _ = child.kill();
    let _ = child.wait();
    result
}

/// Headers in, and as much body as Content-Length says.
fn request_complete(got: &[u8]) -> bool {
    let Some(end) = got.windows(4).position(|w| w == b"\r\n\r\n") else { return false };
    let head = String::from_utf8_lossy(&got[..end]).to_ascii_lowercase();
    let len = head.lines().find_map(|l| l.strip_prefix("content-length:")).and_then(|v| v.trim().parse::<usize>().ok()).unwrap_or(0);
    got.len() >= end + 4 + len
}

/// utime + stime from /proc/<pid>/stat: every thread, since the process's
/// line sums them. The fields are counted after the command name's closing
/// parenthesis, which may itself contain spaces.
fn proc_ticks(pid: u32) -> Result<u64, String> {
    let stat = std::fs::read_to_string(format!("/proc/{pid}/stat")).map_err(|e| format!("/proc/{pid}/stat: {e}"))?;
    parse_ticks(&stat).ok_or_else(|| format!("/proc/{pid}/stat: unreadable line {stat:?}"))
}

fn parse_ticks(stat: &str) -> Option<u64> {
    let rest = &stat[stat.rfind(')')? + 1..];
    let f: Vec<&str> = rest.split_whitespace().collect();
    // After the name: state is field 3, utime 14 and stime 15.
    Some(f.get(11)?.parse::<u64>().ok()? + f.get(12)?.parse::<u64>().ok()?)
}

/// Voluntary and involuntary context switches, summed over every thread.
fn proc_switches(pid: u32) -> Result<u64, String> {
    let tasks = std::fs::read_dir(format!("/proc/{pid}/task")).map_err(|e| format!("/proc/{pid}/task: {e}"))?;
    let mut n = 0;
    for t in tasks.flatten() {
        let status = std::fs::read_to_string(t.path().join("status")).unwrap_or_default();
        n += status_field(&status, "voluntary_ctxt_switches:").unwrap_or(0) + status_field(&status, "nonvoluntary_ctxt_switches:").unwrap_or(0);
    }
    Ok(n)
}

fn proc_rss_kib(pid: u32) -> Result<u64, String> {
    let status = std::fs::read_to_string(format!("/proc/{pid}/status")).map_err(|e| format!("/proc/{pid}/status: {e}"))?;
    status_field(&status, "VmRSS:").ok_or_else(|| format!("/proc/{pid}/status has no VmRSS"))
}

fn status_field(status: &str, key: &str) -> Option<u64> {
    status.lines().find_map(|l| l.strip_prefix(key)).and_then(|v| v.split_whitespace().next()?.parse().ok())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn r_perf_2_ticks_are_read_past_a_name_with_spaces() {
        let stat = "4242 (krowk -p x) S 1 4242 4242 0 -1 4194304 700 0 0 0 17 5 0 0 20 0 1 0 100 7000000 1787 18446744073709551615";
        assert_eq!(parse_ticks(stat), Some(22));
    }

    #[test]
    fn r_perf_2_switches_and_rss_are_read_from_status() {
        let status = "Name:\tkrowk\nVmRSS:\t    7148 kB\nvoluntary_ctxt_switches:\t5\nnonvoluntary_ctxt_switches:\t2\n";
        assert_eq!(status_field(status, "VmRSS:"), Some(7148));
        assert_eq!(status_field(status, "voluntary_ctxt_switches:"), Some(5));
        assert_eq!(status_field(status, "nonvoluntary_ctxt_switches:"), Some(2));
    }

    #[test]
    fn a_request_is_complete_with_its_whole_body() {
        assert!(!request_complete(b"POST / HTTP/1.1\r\ncontent-length: 4\r\n\r\nab"));
        assert!(request_complete(b"POST / HTTP/1.1\r\nContent-Length: 4\r\n\r\nabcd"));
        assert!(!request_complete(b"POST / HTTP/1.1\r\n"));
    }
}
