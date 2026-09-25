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

    /// What to probe to reach `url` the way the model's HTTP client does:
    /// the proxy, when the environment names one that applies (reqwest
    /// honours HTTPS_PROXY, HTTP_PROXY, ALL_PROXY and NO_PROXY, in either
    /// case), else the API's own host. Probing the API directly from behind
    /// a proxy-only network would call it unreachable while every request
    /// gets through.
    pub fn for_url(url: &str, env: &dyn Fn(&str) -> String) -> Option<Target> {
        let direct = Target::from_url(url)?;
        let var = |name: &str| {
            let v = env(&name.to_ascii_lowercase());
            if v.trim().is_empty() { env(name) } else { v }
        };
        if no_proxy(&var("NO_PROXY"), &direct.host) {
            return Some(direct);
        }
        let https = url.starts_with("https://");
        let proxy = [if https { "HTTPS_PROXY" } else { "HTTP_PROXY" }, "ALL_PROXY"].into_iter().map(var).find(|v| !v.trim().is_empty());
        match proxy {
            None => Some(direct),
            Some(p) => {
                let p = p.trim();
                // A proxy written without a scheme is an http one; socks
                // proxies are probed by their address like any other.
                let p = if p.contains("://") { p.replacen("socks5h://", "http://", 1).replacen("socks5://", "http://", 1) } else { format!("http://{p}") };
                Target::from_url(&p).or(Some(direct))
            }
        }
    }

    pub fn label(&self) -> String {
        format!("{}:{}", self.host, self.port)
    }
}

/// Whether NO_PROXY exempts `host`: `*`, the host itself, or a domain it is
/// under (`example.com` and `.example.com` both cover `api.example.com`).
fn no_proxy(list: &str, host: &str) -> bool {
    let host = host.trim_end_matches('.').to_ascii_lowercase();
    list.split(',').map(|e| e.trim().trim_start_matches('.').to_ascii_lowercase()).filter(|e| !e.is_empty()).any(|e| {
        let e = e.split_once(':').map_or(e.as_str(), |(h, _)| h).to_string();
        e == "*" || host == e || host.ends_with(&format!(".{e}"))
    })
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
    fn r_off_1_behind_a_proxy_the_proxy_is_probed_unless_no_proxy_exempts_the_host() {
        let env = |pairs: &'static [(&'static str, &'static str)]| move |k: &str| pairs.iter().find(|(n, _)| *n == k).map(|(_, v)| v.to_string()).unwrap_or_default();
        let t = |url: &str, e: &dyn Fn(&str) -> String| Target::for_url(url, e).map(|t| t.label());
        let none = env(&[]);
        assert_eq!(t("https://api.anthropic.com", &none).as_deref(), Some("api.anthropic.com:443"));
        let https = env(&[("HTTPS_PROXY", "http://proxy.corp:3128")]);
        assert_eq!(t("https://api.anthropic.com", &https).as_deref(), Some("proxy.corp:3128"));
        assert_eq!(t("http://127.0.0.1:8788", &https).as_deref(), Some("127.0.0.1:8788"), "HTTPS_PROXY is for https only");
        let lower = env(&[("https_proxy", "proxy.lower:8080"), ("HTTPS_PROXY", "http://proxy.upper:1")]);
        assert_eq!(t("https://api.anthropic.com", &lower).as_deref(), Some("proxy.lower:8080"), "lower case first, no scheme is http");
        let all = env(&[("ALL_PROXY", "socks5://10.0.0.9:1080")]);
        assert_eq!(t("http://api.local", &all).as_deref(), Some("10.0.0.9:1080"));
        let exempt = env(&[("HTTPS_PROXY", "http://proxy.corp:3128"), ("NO_PROXY", "localhost, .anthropic.com")]);
        assert_eq!(t("https://api.anthropic.com", &exempt).as_deref(), Some("api.anthropic.com:443"));
        assert_eq!(t("https://example.com", &exempt).as_deref(), Some("proxy.corp:3128"));
        let star = env(&[("HTTPS_PROXY", "http://proxy.corp:3128"), ("no_proxy", "*")]);
        assert_eq!(t("https://api.anthropic.com", &star).as_deref(), Some("api.anthropic.com:443"));
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
