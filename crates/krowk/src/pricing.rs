//! Model prices, per million tokens, from models.dev. Two sources, one
//! lookup: the snapshot embedded at build time always works offline, and the
//! cache `pricing refresh` writes is preferred when it holds the pair. Tokens
//! are stored per turn and priced at read time, so a refresh reprices history.

use serde_json::{Map, Value};
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use std::time::Duration;

pub const MODELS_URL: &str = "https://models.dev/api.json";
/// The models.dev snapshot the embedded prices were trimmed from.
pub const SNAPSHOT_DATE: &str = "2026-09-10";
const EMBEDDED: &str = include_str!("models.json");

const REFRESH_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_BODY: u64 = 32 << 20;
const MAX_ETAG: usize = 4096;

#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct Rates {
    pub input: f64,
    pub output: f64,
    pub cache_read: f64,
    pub cache_write: f64,
    pub reasoning: f64,
    pub has_reasoning: bool,
}

#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct Tokens {
    pub input: i64,
    pub output: i64,
    pub cache_read: i64,
    pub cache_write: i64,
    pub reasoning: i64,
}

impl Rates {
    /// USD for these tokens. Reasoning is billed at the output rate when the
    /// model publishes none of its own; negative counts cost nothing.
    pub fn cost(&self, t: Tokens) -> f64 {
        let n = |x: i64| x.max(0) as f64 / 1e6;
        let reasoning = if self.has_reasoning { self.reasoning } else { self.output };
        n(t.input) * self.input + n(t.output) * self.output + n(t.cache_read) * self.cache_read + n(t.cache_write) * self.cache_write + n(t.reasoning) * reasoning
    }
}

type Table = HashMap<(String, String), Rates>;

/// $XDG_CACHE_HOME/krowk/models.json when absolute, else ~/.cache/krowk/.
pub fn cache_path(env: &dyn Fn(&str) -> String) -> Option<PathBuf> {
    let xdg = env("XDG_CACHE_HOME");
    if Path::new(&xdg).is_absolute() {
        return Some(Path::new(&xdg).join("krowk").join("models.json"));
    }
    let home = env("HOME");
    Path::new(&home).is_absolute().then(|| Path::new(&home).join(".cache").join("krowk").join("models.json"))
}

pub fn meta_path(cache: &Path) -> PathBuf {
    cache.with_file_name("models.meta.json")
}

/// The cache and the snapshot, each parsed once per process. They fail
/// independently: a corrupt cache never hides the snapshot.
static LOADED: Mutex<Option<(Option<Table>, Table)>> = Mutex::new(None);

/// The rates for a (provider, model) pair: the cache's, else the snapshot's.
pub fn price(env: &dyn Fn(&str) -> String, provider: &str, model: &str) -> Option<Rates> {
    let mut loaded = LOADED.lock().unwrap_or_else(|e| e.into_inner());
    let (cache, embedded) = loaded.get_or_insert_with(|| {
        let cache = cache_path(env).and_then(|p| std::fs::read(p).ok()).and_then(|raw| parse_rates(&raw));
        (cache, parse_rates(EMBEDDED.as_bytes()).unwrap_or_default())
    });
    let key = (provider.to_string(), model.to_string());
    cache.as_ref().and_then(|c| c.get(&key)).or_else(|| embedded.get(&key)).copied()
}

/// Either shape prices come in: the trimmed snapshot's
/// `{provider: {model: cost}}`, or models.dev's full
/// `{provider: {models: {model: {cost: …}}}}`.
fn parse_rates(raw: &[u8]) -> Option<Table> {
    let top: Map<String, Value> = serde_json::from_slice(raw).ok()?;
    let mut out = Table::new();
    for (provider, fields) in &top {
        let Some(fields) = fields.as_object() else { continue };
        let full: Vec<(&String, &Map<String, Value>)> = fields
            .get("models")
            .and_then(Value::as_object)
            .map(|models| models.iter().filter_map(|(m, v)| Some((m, v.get("cost")?.as_object().filter(|c| !c.is_empty())?))).collect())
            .unwrap_or_default();
        let entries: Vec<(&String, &Map<String, Value>)> = if full.is_empty() {
            fields.iter().filter_map(|(m, v)| Some((m, v.as_object()?))).collect()
        } else {
            full
        };
        for (model, cost) in entries {
            if let Some(r) = rates_from(cost) {
                out.insert((provider.clone(), model.clone()), r);
            }
        }
    }
    Some(out)
}

fn rates_from(cost: &Map<String, Value>) -> Option<Rates> {
    let mut r = Rates::default();
    let mut found = false;
    let mut take = |name: &str, dst: &mut f64| -> bool {
        match cost.get(name).and_then(Value::as_f64).filter(|v| *v >= 0.0) {
            Some(v) => {
                *dst = v;
                found = true;
                true
            }
            None => false,
        }
    };
    take("input", &mut r.input);
    take("output", &mut r.output);
    take("cache_read", &mut r.cache_read);
    take("cache_write", &mut r.cache_write);
    r.has_reasoning = take("reasoning", &mut r.reasoning);
    found.then_some(r)
}

/// Fetches models.dev into the cache, conditionally on the last ETag.
/// `Ok(false)` covers every way of not having new prices — unchanged, a
/// network that is not there, an answer that is not a price file — since
/// the snapshot still answers; only a cache that cannot be written is an error.
pub fn refresh(env: &dyn Fn(&str) -> String, url: &str) -> Result<bool, String> {
    refresh_within(env, url, REFRESH_TIMEOUT)
}

/// `refresh` bounded by `timeout`: sync's is tighter, since a scheduled sync
/// is not a place to wait on a slow network.
pub fn refresh_within(env: &dyn Fn(&str) -> String, url: &str, timeout: Duration) -> Result<bool, String> {
    let path = cache_path(env).ok_or("pricing: no cache directory in environment")?;
    let etag = load_etag(&path);
    let agent: ureq::Agent = ureq::Agent::config_builder()
        .timeout_global(Some(timeout))
        .max_redirects(0)
        .http_status_as_error(false)
        .tls_config(ureq::tls::TlsConfig::builder().root_certs(ureq::tls::RootCerts::PlatformVerifier).build())
        .build()
        .into();
    let mut req = agent.get(if url.is_empty() { MODELS_URL } else { url });
    if !etag.is_empty() {
        req = req.header("If-None-Match", &etag);
    }
    let Ok(mut res) = req.call() else { return Ok(false) };
    match res.status().as_u16() {
        304 => {
            stamp_meta(&path, &etag);
            Ok(false)
        }
        200 => {
            let new_etag = sanitize_etag(res.headers().get("etag").and_then(|v| v.to_str().ok()).unwrap_or_default());
            let mut body = Vec::new();
            if std::io::Read::read_to_end(&mut std::io::Read::take(res.body_mut().as_reader(), MAX_BODY + 1), &mut body).is_err()
                || body.len() as u64 > MAX_BODY
            {
                return Ok(false);
            }
            if parse_rates(&body).is_none_or(|t| t.is_empty()) {
                return Ok(false);
            }
            write_atomic(&path, &body).map_err(|e| e.to_string())?;
            stamp_meta(&path, if new_etag.is_empty() { &etag } else { &new_etag });
            *LOADED.lock().unwrap_or_else(|e| e.into_inner()) = None;
            Ok(true)
        }
        _ => Ok(false),
    }
}

/// When the cache was last fetched, from its sidecar.
pub fn fetched_at_ms(cache: &Path) -> Option<i64> {
    let meta: Value = serde_json::from_slice(&std::fs::read(meta_path(cache)).ok()?).ok()?;
    meta.get("fetched_at_ms")?.as_i64()
}

fn load_etag(cache: &Path) -> String {
    std::fs::read(meta_path(cache))
        .ok()
        .and_then(|raw| serde_json::from_slice::<Value>(&raw).ok())
        .and_then(|m| m.get("etag")?.as_str().map(sanitize_etag))
        .unwrap_or_default()
}

/// An ETag is visible ASCII and bounded, or it is not replayed.
fn sanitize_etag(etag: &str) -> String {
    if etag.is_empty() || etag.len() > MAX_ETAG || !etag.bytes().all(|b| (0x21..=0x7e).contains(&b)) {
        return String::new();
    }
    etag.to_string()
}

fn stamp_meta(cache: &Path, etag: &str) {
    let now = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map_or(0, |d| d.as_millis() as i64);
    let meta = serde_json::json!({ "etag": etag, "fetched_at_ms": now });
    let _ = write_atomic(&meta_path(cache), format!("{meta}\n").as_bytes());
}

fn write_atomic(path: &Path, data: &[u8]) -> std::io::Result<()> {
    let dir = path.parent().unwrap_or(Path::new("."));
    std::fs::create_dir_all(dir)?;
    let tmp = dir.join(format!(".tmp-{}", std::process::id()));
    std::fs::write(&tmp, data)?;
    std::fs::rename(&tmp, path).inspect_err(|_| {
        let _ = std::fs::remove_file(&tmp);
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn both_price_shapes_parse_and_reasoning_falls_back_to_output() {
        let snapshot = parse_rates(EMBEDDED.as_bytes()).unwrap();
        assert!(snapshot.contains_key(&("anthropic".into(), "claude-sonnet-4-6".into())));
        let full = parse_rates(br#"{"p":{"models":{"m":{"cost":{"input":1,"output":2,"reasoning":null}}}}}"#).unwrap();
        let r = full[&("p".into(), "m".into())];
        assert!(!r.has_reasoning);
        let cost = r.cost(Tokens { input: 1_000_000, output: 500_000, reasoning: 500_000, cache_read: -5, ..Tokens::default() });
        assert!((cost - 3.0).abs() < 1e-9);
        assert!(parse_rates(b"not json").is_none());
        assert_eq!(sanitize_etag("W/\"abc\""), "W/\"abc\"");
        assert_eq!(sanitize_etag("bad etag"), "");
    }
}
