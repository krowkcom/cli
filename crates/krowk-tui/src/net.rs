//! Connectivity (R-OFF-1): whether the model's API can be reached at all,
//! asked by opening a TCP connection to its host and closing it again. It
//! answers "is anything there", which is the question a stalled stream
//! raises and the one a person needs answered: a TLS handshake or a request
//! would prove more and cost a key's rate limit.
//!
//! When it is asked is the caller's (see `lib.rs`): once at start, when a
//! model call has been silent for 0.7 s, and every few seconds while the
//! answer is no. Never while idle and connected — nothing polls then
//! (R-PERF-2).

use std::time::Duration;

/// How long one probe waits for the connection before calling the host
/// unreachable. With the 0.7 s of silence before it, a cut network is
/// reported inside two seconds with room to spare, however it fails: a
/// refusal at once, packets dropped at this timeout.
pub const PROBE_TIMEOUT: Duration = Duration::from_millis(800);

/// A host and port to probe, from an instance's base URL.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Target {
    pub host: String,
    pub port: u16,
}

impl Target {
    /// `https://api.anthropic.com` → api.anthropic.com:443. None for what
    /// is not an http(s) URL.
    pub fn from_url(url: &str) -> Option<Target> {
        let (scheme, rest) = url.split_once("://")?;
        let default = match scheme {
            "https" => 443,
            "http" => 80,
            _ => return None,
        };
        let authority = rest.split(['/', '?', '#']).next()?;
        let authority = authority.rsplit_once('@').map_or(authority, |(_, a)| a);
        // [v6]:port, host:port, or a bare host.
        let (host, port) = if let Some(v6) = authority.strip_prefix('[') {
            let (h, tail) = v6.split_once(']')?;
            (h.to_string(), tail.strip_prefix(':').map(str::parse).transpose().ok()?.unwrap_or(default))
        } else {
            match authority.rsplit_once(':') {
                Some((h, p)) => (h.to_string(), p.parse().ok()?),
                None => (authority.to_string(), default),
            }
        };
        (!host.is_empty()).then_some(Target { host, port })
    }

    pub fn label(&self) -> String {
        format!("{}:{}", self.host, self.port)
    }
}

/// Whether a connection to `t` opens within `PROBE_TIMEOUT`.
pub async fn reachable(t: &Target) -> bool {
    let connect = tokio::net::TcpStream::connect((t.host.as_str(), t.port));
    matches!(tokio::time::timeout(PROBE_TIMEOUT, connect).await, Ok(Ok(_)))
}

/// The wait before the next probe while unreachable: two seconds, doubling
/// to ten, so a notice clears within ten seconds of the network returning
/// without a laptop on a train dialling out every second.
pub fn retry_after(failures: u32) -> Duration {
    Duration::from_secs((2u64 << failures.min(3)).min(10))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn r_off_1_targets_come_from_base_urls() {
        let t = |u: &str| Target::from_url(u).map(|t| t.label());
        assert_eq!(t("https://api.anthropic.com").as_deref(), Some("api.anthropic.com:443"));
        assert_eq!(t("http://127.0.0.1:8788/v1").as_deref(), Some("127.0.0.1:8788"));
        assert_eq!(t("http://user:pw@proxy.local").as_deref(), Some("proxy.local:80"));
        assert_eq!(t("https://[::1]:9000/x").as_deref(), Some("::1:9000"));
        assert_eq!(t("unix:///tmp/sock"), None);
        assert_eq!(t("https://"), None);
    }

    #[test]
    fn r_off_1_probes_back_off_to_ten_seconds() {
        assert_eq!(retry_after(0), Duration::from_secs(2));
        assert_eq!(retry_after(1), Duration::from_secs(4));
        assert_eq!(retry_after(2), Duration::from_secs(8));
        assert_eq!(retry_after(9), Duration::from_secs(10));
    }

    #[test]
    fn r_off_1_a_closed_port_is_unreachable_and_an_open_one_is_not() {
        let rt = tokio::runtime::Builder::new_current_thread().enable_all().build().unwrap();
        rt.block_on(async {
            let l = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let port = l.local_addr().unwrap().port();
            assert!(reachable(&Target { host: "127.0.0.1".into(), port }).await);
            drop(l);
            assert!(!reachable(&Target { host: "127.0.0.1".into(), port }).await);
        });
    }
}
