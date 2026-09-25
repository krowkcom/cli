//! Model prices, per million tokens, from models.dev. Two sources, one
//! lookup: the snapshot embedded at build time always works offline, and the
//! cache `pricing refresh` writes is preferred when it holds the pair. Tokens
//! are stored per turn and priced at read time, so a refresh reprices history.

use serde_json::value::RawValue;
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

/// Where a price came from, and so how current it is: the refreshed cache as
/// of its last fetch, or the snapshot embedded at build time. Either way the
/// rate is today's, never the one in force when the tokens were spent.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum Basis {
    Cache { fetched_at_ms: Option<i64> },
    Snapshot,
}

impl Basis {
    /// The footnote a priced figure carries.
    pub fn note(&self) -> String {
        match self {
            Basis::Cache { fetched_at_ms: Some(ms) } => format!("models.dev prices fetched {}", date_of(*ms)),
            Basis::Cache { fetched_at_ms: None } => "models.dev prices from the refreshed cache".into(),
            Basis::Snapshot => format!("models.dev snapshot {SNAPSHOT_DATE} embedded in this build"),
        }
    }
}

/// Every basis a set of figures was priced on, as one footnote.
pub fn basis_note(bases: &std::collections::BTreeSet<Basis>) -> String {
    let notes: Vec<String> = bases.iter().map(Basis::note).collect();
    if notes.is_empty() {
        return String::new();
    }
    format!("priced at current rates ({}), not the rates in force at the time", notes.join(" and "))
}

fn date_of(ms: i64) -> String {
    jiff::Timestamp::from_millisecond(ms).map(|t| t.strftime("%Y-%m-%d").to_string()).unwrap_or_else(|_| "at an unknown date".into())
}

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

/// The cache (with when it was fetched) and the snapshot, each parsed once
/// per process. They fail independently: a corrupt cache never hides the
/// snapshot.
type Loaded = (Option<(Table, Option<i64>)>, Table);
static LOADED: Mutex<Option<Loaded>> = Mutex::new(None);

/// The rates for a (provider, model) pair: the cache's, else the snapshot's.
pub fn price(env: &dyn Fn(&str) -> String, provider: &str, model: &str) -> Option<Rates> {
    price_with_basis(env, provider, model).map(|(r, _)| r)
}

/// `price`, and which of the two sources answered.
pub fn price_with_basis(env: &dyn Fn(&str) -> String, provider: &str, model: &str) -> Option<(Rates, Basis)> {
    let mut loaded = LOADED.lock().unwrap_or_else(|e| e.into_inner());
    let (cache, embedded) = loaded.get_or_insert_with(|| {
        let cache = cache_path(env).and_then(|p| {
            let table = parse_rates(&std::fs::read(&p).ok()?)?;
            Some((table, fetched_at_ms(&p)))
        });
        (cache, parse_rates(EMBEDDED.as_bytes()).unwrap_or_default())
    });
    let key = (provider.to_string(), model.to_string());
    if let Some((table, fetched)) = cache
        && let Some(r) = table.get(&key)
    {
        return Some((*r, Basis::Cache { fetched_at_ms: *fetched }));
    }
    embedded.get(&key).map(|r| (*r, Basis::Snapshot))
}

/// Either shape prices come in: the trimmed snapshot's
/// `{provider: {model: cost}}`, or models.dev's full
/// `{provider: {models: {model: {cost: …}}}}`. Typed, with every
/// field but `cost` skipped unread: the full file is megabytes, and it is
/// parsed on every `sessions` listing.
fn parse_rates(raw: &[u8]) -> Option<Table> {
    // One bad entry costs that entry, never its provider.
    #[derive(serde::Deserialize)]
    struct FullModel {
        cost: Option<Map<String, Value>>,
    }
    let top: HashMap<String, &RawValue> = serde_json::from_slice(raw).ok()?;
    let mut out = Table::new();
    for (provider, fields) in top {
        let Ok(fields) = serde_json::from_str::<HashMap<String, &RawValue>>(fields.get()) else { continue };
        let full: Vec<(String, Map<String, Value>)> = fields
            .get("models")
            .and_then(|m| serde_json::from_str::<HashMap<String, &RawValue>>(m.get()).ok())
            .map(|models| {
                models
                    .into_iter()
                    .filter_map(|(m, v)| Some((m, serde_json::from_str::<FullModel>(v.get()).ok()?.cost.filter(|c| !c.is_empty())?)))
                    .collect()
            })
            .unwrap_or_default();
        let entries: Vec<(String, Map<String, Value>)> = if full.is_empty() {
            fields.iter().filter_map(|(m, v)| Some((m.clone(), serde_json::from_str::<Map<String, Value>>(v.get()).ok()?))).collect()
        } else {
            full
        };
        for (model, cost) in entries {
            if let Some(r) = rates_from(&cost) {
                out.insert((provider.clone(), model), r);
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
        let bad_neighbours = parse_rates(br#"{"p":{"models":{"m":{"cost":{"input":1}},"n":null,"o":{"cost":"free"}}}}"#).unwrap();
        assert_eq!(bad_neighbours.len(), 1, "a bad entry costs itself, not its provider");
        assert_eq!(sanitize_etag("W/\"abc\""), "W/\"abc\"");
        assert_eq!(sanitize_etag("bad etag"), "");
    }
}
