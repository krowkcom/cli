//! The registry client. Uploading is three calls, because the bytes never pass
//! through the registry:
//!
//!  1. POST /v1/artifacts                       declare the file, get a presigned PUT
//!  2. PUT  <presigned url>                     bytes go straight to object storage
//!  3. PUT  /v1/artifacts/{slug}/finalization   the registry verifies what landed
//!
//! plus a fourth that recovers step two: POST /v1/artifacts/{slug}/upload mints
//! the presigned PUT again when the first one lapsed, over the same slug.
//!
//! A presigned URL names a foreign host by design, which means a response body
//! would otherwise choose where this process sends a request from its own
//! network position. So uploads are always PUT, never follow a redirect off
//! the API's origin, refuse plaintext and internal hosts — and the address
//! check is made on what is actually dialled, in the resolver, so a name that
//! answers public for a check and internal for the dial gets nowhere.

use crate::error::{fail, Error};
use crate::spec::Spec;
use crate::types::*;
use serde::de::DeserializeOwned;
use serde_json::{json, Map, Value};
use std::collections::BTreeMap;
use std::io::Read;
use std::net::{IpAddr, SocketAddr, ToSocketAddrs};
use std::sync::Arc;
use std::time::Duration;
use ureq::http::Uri;

const MAX_ATTEMPTS: u32 = 3;
const MAX_BODY: u64 = 1 << 20;

/// One CLI invocation's client. It holds no state between calls.
pub struct Client {
    pub base_url: String,
    pub token: String,
    agent: ureq::Agent,
    /// Swapped in tests so backoff costs no wall clock.
    pub sleep: fn(Duration),
}

impl Client {
    /// A client against `base_url` (the public registry when empty).
    pub fn new(base_url: &str, token: &str) -> Client {
        let base_url = if base_url.is_empty() { crate::DEFAULT_BASE_URL } else { base_url }.trim_end_matches('/').to_string();
        let proxy = ureq::Proxy::try_from_env();
        let guard = Guard { base: base_url.clone(), proxy: proxy.as_ref().map(|p| (p.host().to_string(), p.port())) };
        let config = ureq::Agent::config_builder()
            .http_status_as_error(false)
            // Redirects are judged here, hop by hop, never followed blind.
            .max_redirects(0)
            .timeout_global(Some(Duration::from_secs(300)))
            .timeout_connect(Some(Duration::from_secs(30)))
            .proxy(proxy)
            .tls_config(ureq::tls::TlsConfig::builder().root_certs(ureq::tls::RootCerts::PlatformVerifier).build())
            .user_agent(format!("krowk-cli/{}", option_env!("KROWK_VERSION").unwrap_or("dev")))
            .build();
        let resolver = GuardResolver { guard: Arc::new(guard), inner: ureq::unversioned::resolver::DefaultResolver::default() };
        let agent = ureq::Agent::with_parts(config, ureq::unversioned::transport::DefaultConnector::default(), resolver);
        Client { base_url, token: token.to_string(), agent, sleep: std::thread::sleep }
    }

    /// Whether calls carry a key. Runs, and so all run metadata, need one.
    pub fn authenticated(&self) -> bool {
        !self.token.is_empty()
    }

    /// Whether the registry is reached over plaintext — a local or private one,
    /// which the caller chose.
    pub fn insecure(&self) -> bool {
        self.base_url.starts_with("http://")
    }

    fn keyless(&self) -> Client {
        Client { sleep: self.sleep, ..Client::new(&self.base_url, "") }
    }

    /// Reads back the key the client holds. A key the registry will not take
    /// gets the same 401 here as anywhere, so a success is the answer — as long
    /// as a key came back at all.
    pub fn verify_key(&self) -> Result<Key, Error> {
        let (mut key, status): (Key, u16) = self.call("GET", "/key", None, MAX_ATTEMPTS, None)?;
        key.status = status;
        if key.key_id.is_empty() {
            return Err(malformed(
                status,
                "the registry answered the key lookup without naming a key — check KROWK_API_URL points at the API host, not the website",
            ));
        }
        Ok(key)
    }

    /// Opens a browser login. Keyless on purpose: the endpoint exists for a
    /// machine with no key, and sending one would meter it as that key's.
    pub fn start_cli_authorization(&self) -> Result<CliAuthorization, Error> {
        let (auth, status): (CliAuthorization, u16) = self.call("POST", "/cli/authorizations", None, MAX_ATTEMPTS, None)?;
        if auth.slug.is_empty() || auth.code.is_empty() {
            return Err(malformed(
                status,
                "the registry opened a browser login without naming a code to confirm or a slug to poll — check KROWK_API_URL points at the API host, not the website",
            ));
        }
        Ok(auth)
    }

    pub fn read_cli_authorization(&self, slug: &str) -> Result<CliAuthorization, Error> {
        self.get(&format!("/cli/authorizations/{}", slug_path(slug)))
    }

    /// Declares, uploads and finalizes one file.
    pub fn push(&self, spec: &Spec) -> Result<Artifact, Error> {
        let mut prepared = self.prepare_artifact(spec)?;
        // Checked before the bytes move — the only moment it can be. A registry
        // that ignored the field would publish what the caller asked to keep in.
        if !spec.visibility.is_empty() && prepared.visibility != spec.visibility {
            let got = if prepared.visibility.is_empty() { "no visibility at all" } else { &prepared.visibility };
            return Err(fail(
                "visibility_not_applied",
                format!(
                    "this registry did not apply the visibility the upload asked for — declared {}, got {got}. Nothing was uploaded. \
                     Check KROWK_API_URL points at a registry that supports private artifacts, or push it public",
                    spec.visibility
                ),
            ));
        }
        self.put_bytes(&mut prepared, spec)?;
        let mut done = self.finalize_artifact(&prepared.slug)?;
        done.claim_token = prepared.claim_token;
        Ok(done)
    }

    /// Records the artifact and returns the presigned upload, under an
    /// Idempotency-Key so a retry after a lost response is the same declare.
    pub fn prepare_artifact(&self, spec: &Spec) -> Result<Artifact, Error> {
        let body = json!({ "artifact": spec });
        Ok(self.call("POST", "/artifacts", Some(body), MAX_ATTEMPTS, Some(idempotency_key()))?.0)
    }

    /// Mints the upload again over the same slug. A keyless caller's authority
    /// is the claim token, sent instead of a key and in the body, never the
    /// query string that ends up in access logs.
    pub fn presign_upload(&self, slug: &str, claim_token: &str) -> Result<Artifact, Error> {
        let path = format!("/artifacts/{}/upload", slug_path(slug));
        if claim_token.is_empty() {
            return Ok(self.call("POST", &path, None, MAX_ATTEMPTS, None)?.0);
        }
        Ok(self.keyless().call("POST", &path, Some(json!({ "claim_token": claim_token })), MAX_ATTEMPTS, None)?.0)
    }

    pub fn finalize_artifact(&self, slug: &str) -> Result<Artifact, Error> {
        Ok(self.call("PUT", &format!("/artifacts/{}/finalization", slug_path(slug)), None, MAX_ATTEMPTS, None)?.0)
    }

    pub fn show_artifact(&self, slug: &str) -> Result<Artifact, Error> {
        self.get(&format!("/artifacts/{}", slug_path(slug)))
    }

    pub fn list_artifacts(&self, before: &str, limit: i64) -> Result<Page, Error> {
        self.get(&paged("/artifacts", before, limit))
    }

    pub fn list_runs(&self, before: &str, limit: i64) -> Result<RunPage, Error> {
        self.get(&paged("/runs", before, limit))
    }

    pub fn show_run(&self, slug: &str) -> Result<Run, Error> {
        self.get(&format!("/runs/{}", slug_path(slug)))
    }

    /// What one run produced — a collection of the run, so an unknown slug is
    /// a 404 rather than an empty page.
    pub fn list_run_artifacts(&self, run: &str, before: &str, limit: i64) -> Result<Page, Error> {
        self.get(&paged(&format!("/runs/{}/artifacts", slug_path(run)), before, limit))
    }

    /// Spends a claim token to move an anonymous artifact into the key's workspace.
    pub fn claim_artifact(&self, slug: &str, claim_token: &str) -> Result<Artifact, Error> {
        let body = json!({ "claim_token": claim_token });
        Ok(self.call("POST", &format!("/artifacts/{}/claim", slug_path(slug)), Some(body), MAX_ATTEMPTS, None)?.0)
    }

    /// Puts an artifact under a run afterwards.
    pub fn attach_run(&self, artifact: &str, run: &str) -> Result<Artifact, Error> {
        let body = json!({ "run": run });
        Ok(self.call("PUT", &format!("/artifacts/{}/run", slug_path(artifact)), Some(body), MAX_ATTEMPTS, None)?.0)
    }

    /// Removes an artifact's bytes and leaves a tombstone. A claim token is the
    /// authority for a keyless takedown, and is sent instead of any key: offered
    /// both, the registry reads the key and looks in the wrong workspace.
    pub fn take_down_artifact(&self, slug: &str, claim_token: &str) -> Result<(), Error> {
        let path = format!("/artifacts/{}", slug_path(slug));
        if claim_token.is_empty() {
            return self.call::<Value>("DELETE", &path, None, MAX_ATTEMPTS, None).map(|_| ());
        }
        self.keyless().call::<Value>("DELETE", &path, Some(json!({ "claim_token": claim_token })), MAX_ATTEMPTS, None).map(|_| ())
    }

    /// Opens a run, under an Idempotency-Key so a retry is the same run.
    pub fn create_run(&self, metadata: &Value) -> Result<Run, Error> {
        let body = json!({ "run": { "metadata": metadata } });
        Ok(self.call("POST", "/runs", Some(body), MAX_ATTEMPTS, Some(idempotency_key()))?.0)
    }

    pub fn finish_run(&self, slug: &str) -> Result<Run, Error> {
        Ok(self.call("PUT", &format!("/runs/{}/completion", slug_path(slug)), None, MAX_ATTEMPTS, None)?.0)
    }

    /// The service descriptor at the host root, one level above /v1 — how
    /// doctor tells a registry from something else that answers.
    pub fn root(&self) -> Result<Service, Error> {
        let url = format!("{}/", self.base_url.strip_suffix("/v1").unwrap_or(&self.base_url));
        Ok(self.request_url("GET", &url, None, MAX_ATTEMPTS, None)?.0)
    }

    fn get<T: DeserializeOwned>(&self, path: &str) -> Result<T, Error> {
        Ok(self.call("GET", path, None, MAX_ATTEMPTS, None)?.0)
    }

    fn call<T: DeserializeOwned>(
        &self,
        method: &str,
        path: &str,
        body: Option<Value>,
        attempts: u32,
        idempotency: Option<String>,
    ) -> Result<(T, u16), Error> {
        self.request_url(method, &format!("{}{path}", self.base_url), body, attempts, idempotency)
    }

    /// One registry request with retries; the idempotency key, when there is one,
    /// is the same on every attempt — that is what tells the registry attempt two
    /// is attempt one again.
    fn request_url<T: DeserializeOwned>(
        &self,
        method: &str,
        url: &str,
        body: Option<Value>,
        attempts: u32,
        idempotency: Option<String>,
    ) -> Result<(T, u16), Error> {
        let payload = body.map(|b| serde_json::to_vec(&b).expect("request body serializes"));
        let mut last = None;
        for attempt in 1..=attempts {
            match self.once(method, url, payload.as_deref(), idempotency.as_deref()) {
                Ok((status, bytes)) => {
                    let value = if bytes.iter().all(u8::is_ascii_whitespace) { Value::Null } else {
                        serde_json::from_slice(&bytes).map_err(|_| malformed_success(status))?
                    };
                    // A 204 carries nothing, which reads as an empty record.
                    let value = if value.is_null() { Value::Object(Map::new()) } else { value };
                    return serde_json::from_value(value).map(|v| (v, status)).map_err(|_| malformed_success(status));
                }
                Err(e) => {
                    if !e.retryable() || attempt == attempts {
                        return Err(e);
                    }
                    (self.sleep)(backoff(&e, attempt));
                    last = Some(e);
                }
            }
        }
        Err(last.expect("at least one attempt"))
    }

    /// One attempt: the status and body of a success, or the failure flattened.
    fn once(&self, method: &str, url: &str, payload: Option<&[u8]>, idempotency: Option<&str>) -> Result<(u16, Vec<u8>), Error> {
        let mut url = url.to_string();
        for hop in 0..=10 {
            let mut req = ureq::http::Request::builder().method(method).uri(&url).header("Accept", "application/json");
            if payload.is_some() {
                req = req.header("Content-Type", "application/json");
            }
            if !self.token.is_empty() {
                req = req.header("Authorization", format!("Bearer {}", self.token));
            }
            if let Some(key) = idempotency {
                req = req.header("Idempotency-Key", key);
            }
            let req = req.body(payload.map(<[u8]>::to_vec).unwrap_or_default()).map_err(|e| fail("bad_request", e.to_string()))?;
            let mut res = self.agent.run(req).map_err(|e| self.transport(&url, e, "network_unreachable"))?;
            let status = res.status().as_u16();
            let mut bytes = Vec::new();
            let _ = res.body_mut().as_reader().take(MAX_BODY).read_to_end(&mut bytes);
            if (300..400).contains(&status) {
                let location = header(&res, "location");
                match self.next_hop(&url, &location, hop) {
                    Ok(Some(next)) => {
                        url = next;
                        continue;
                    }
                    Ok(None) => return Err(unexpected_redirect(status, &url, &location)),
                    Err(e) => return Err(e),
                }
            }
            if status >= 400 {
                let mut err = response_error(status, &bytes, &header(&res, "retry-after"));
                if err.code() == "unauthorized" {
                    // A 401 with no key sent is a missing key, not a rejected one;
                    // a rejected one is when the self-check earns its keep —
                    // except on the self-check itself.
                    let fix = if self.token.is_empty() {
                        "this endpoint needs an API key — run `krowk auth login --token krowk_sk_...`, or set KROWK_TOKEN".to_string()
                    } else if !url.ends_with("/key") {
                        let hint = "run `krowk auth verify` to see what this key is allowed to do";
                        match err.fix() {
                            f if f.is_empty() => hint.into(),
                            f => format!("{f} — {hint}"),
                        }
                    } else {
                        err.fix()
                    };
                    err.body.insert("fix".into(), json!(fix));
                }
                return Err(err);
            }
            return Ok((status, bytes));
        }
        Err(fail("too_many_redirects", "gave up after 10 redirects — the registry is looping"))
    }

    /// Where a redirect off `from` may go: only a request that began on the API's
    /// own origin follows one, it stays on that host, and never downgrades from
    /// https — the token would go in the clear.
    fn next_hop(&self, from: &str, location: &str, hop: usize) -> Result<Option<String>, Error> {
        let Some(next) = resolve_location(from, location) else { return Ok(None) };
        let (Some(base), Some(from_u), Some(next_u)) = (parse(&self.base_url), parse(from), parse(&next)) else { return Ok(None) };
        if !self.on_api_origin(&from_u) {
            return Err(fail(
                "upload_redirected",
                format!(
                    "the upload target {} redirected to {} — a presigned URL is where the bytes belong, so this is not followed",
                    from_u.authority, next_u.authority
                ),
            ));
        }
        if next_u.host != base.host {
            return Err(fail(
                "untrusted_redirect",
                format!(
                    "the registry redirected a request from its own origin to {} — a request to the API's origin stays there",
                    next_u.authority
                ),
            ));
        }
        if from_u.scheme == "https" && next_u.scheme != "https" {
            return Err(fail(
                "insecure_redirect",
                format!("the registry redirected from https to {} — refusing, the token would go in the clear", next_u.scheme),
            ));
        }
        if hop >= 10 {
            return Err(fail(
                "too_many_redirects",
                format!("gave up after 10 redirects from {} — the registry is looping", from_u.authority),
            ));
        }
        Ok(self.on_api_origin(&next_u).then_some(next))
    }

    fn on_api_origin(&self, u: &Url) -> bool {
        parse(&self.base_url).is_some_and(|base| on_origin(&base, u))
    }

    fn transport(&self, url: &str, e: ureq::Error, code: &str) -> Error {
        if let ureq::Error::Other(inner) = &e
            && let Some(refused) = inner.downcast_ref::<Error>()
        {
            return refused.clone();
        }
        let mut body = BTreeMap::new();
        body.insert("error".into(), json!(code));
        body.insert("detail".into(), json!(e.to_string()));
        if code == "network_unreachable" {
            body.insert("endpoint".into(), json!(url));
            body.insert(
                "fix".into(),
                json!(format!("cannot reach {} — check the network, or point KROWK_API_URL at a reachable registry", self.base_url)),
            );
            body.insert("retryable".into(), json!(false));
        } else {
            body.insert(
                "fix".into(),
                json!("the registry issued an upload URL but the bytes could not be sent to it — check the network"),
            );
            body.insert("retryable".into(), json!(true));
        }
        Error { status: 0, body }
    }

    /// Streams the file to storage with exactly the headers the URL was signed
    /// for. The presign is perishable: a lapsed window or a refused signature
    /// gets a fresh one over the same slug rather than three retries proving
    /// the old one dead and advice to push again, which would mint a new link.
    pub fn put_bytes(&self, prepared: &mut Artifact, spec: &Spec) -> Result<(), Error> {
        let Some(upload) = prepared.upload.as_ref().filter(|u| !u.url.is_empty()) else {
            return Err(fail("no_upload_url", "the registry accepted the artifact but did not say where to put the bytes"));
        };
        let mut endpoint = self.storage_origin(&upload.url)?;
        let mut last = None;
        for attempt in 1..=MAX_ATTEMPTS {
            // Our clock's judgement of the registry's deadline, so a registry
            // that will not mint a fresh one is not fatal: storage decides.
            if lapsed(prepared.upload.as_ref())
                && let Ok(fresh) = self.represign(prepared)
            {
                endpoint = fresh;
            }
            match self.put_once(&endpoint, prepared.upload.as_ref(), spec) {
                Ok(()) => return Ok(()),
                Err(e) => {
                    let signature_refused = e.status == 403 && e.code() == "storage_rejected_upload";
                    if signature_refused && !prepared.slug.is_empty() && attempt < MAX_ATTEMPTS {
                        // The registry's account of why these bytes have no URL
                        // beats storage's 403.
                        endpoint = self.represign(prepared)?;
                        last = Some(e);
                        continue;
                    }
                    if !e.retryable() || attempt == MAX_ATTEMPTS {
                        return Err(e);
                    }
                    (self.sleep)(backoff(&e, attempt));
                    last = Some(e);
                }
            }
        }
        Err(last.expect("at least one attempt"))
    }

    fn represign(&self, prepared: &mut Artifact) -> Result<String, Error> {
        let fresh = self.presign_upload(&prepared.slug, &prepared.claim_token)?;
        let Some(upload) = fresh.upload.filter(|u| !u.url.is_empty()) else {
            return Err(fail(
                "no_upload_url",
                format!("the registry re-presigned {} without saying where to put the bytes", prepared.slug),
            ));
        };
        let endpoint = self.storage_origin(&upload.url)?;
        // Headers and all: sending the old answer's headers with the new URL is
        // a signature mismatch.
        prepared.upload = Some(upload);
        Ok(endpoint)
    }

    /// Always PUT, whatever the registry's `method` says: letting a response
    /// body choose the method would hand a compromised registry the whole
    /// request this process makes.
    fn put_once(&self, endpoint: &str, upload: Option<&Upload>, spec: &Spec) -> Result<(), Error> {
        let mut file = std::fs::File::open(&spec.path)
            .map_err(|e| fail("file_unreadable", format!("cannot read `{}`: {}", spec.path, crate::spec::go_os_error("open", &spec.path, &e))))?;
        let mut req = ureq::http::Request::builder().method("PUT").uri(endpoint);
        for (k, v) in upload.and_then(|u| u.headers.as_ref()).into_iter().flatten() {
            // Content-Length is signed too, and comes off the body's own size.
            if !k.eq_ignore_ascii_case("content-length") {
                req = req.header(k.as_str(), v.as_str());
            }
        }
        req = req.header("Content-Length", spec.byte_size.to_string());
        let body = ureq::SendBody::from_reader(&mut file);
        let req = req.body(body).map_err(|e| fail("bad_upload_url", e.to_string()))?;
        let mut res = self.agent.run(req).map_err(|e| self.transport(endpoint, e, "storage_unreachable"))?;
        let status = res.status().as_u16();
        if (300..400).contains(&status) {
            let location = header(&res, "location");
            // A presigned URL is a final destination; the API's own origin may
            // redirect, but an upload is never carried off it.
            if !self.on_api_origin(&parse(endpoint).unwrap_or_default()) {
                let next = resolve_location(endpoint, &location).and_then(|n| parse(&n)).map(|u| u.authority).unwrap_or(location.clone());
                return Err(fail(
                    "upload_redirected",
                    format!(
                        "the upload target {} redirected to {next} — a presigned URL is where the bytes belong, so this is not followed",
                        parse(endpoint).map(|u| u.authority).unwrap_or_default()
                    ),
                ));
            }
            return Err(unexpected_redirect(status, endpoint, &location));
        }
        let mut snippet = Vec::new();
        let _ = res.body_mut().as_reader().take(2048).read_to_end(&mut snippet);
        if status < 400 {
            return Ok(());
        }
        // Storage answers in XML, not our envelope, so its body is a snippet
        // rather than a code this side would be inventing.
        let mut body = BTreeMap::new();
        body.insert("error".into(), json!("storage_rejected_upload"));
        body.insert("detail".into(), json!(clip(String::from_utf8_lossy(&snippet).trim(), 300)));
        body.insert(
            "fix".into(),
            json!("object storage refused the bytes — most often the file changed after it was measured, so it no longer matches the size and digest the URL was signed for"),
        );
        body.insert("retryable".into(), json!(status >= 500));
        Err(Error { status, body })
    }

    /// The storage host a presigned URL names, if it is one worth sending bytes
    /// to: http(s) only; a local registry's targets are local by definition; the
    /// API's own origin is trusted on the API's terms; anything else must be
    /// https and outside this machine and its network.
    pub fn storage_origin(&self, raw: &str) -> Result<String, Error> {
        let not_http = || fail("bad_upload_url", format!("the registry returned an upload URL that is not http(s): {raw}"));
        let u = parse(raw).filter(|u| !u.host.is_empty() && (u.scheme == "http" || u.scheme == "https")).ok_or_else(not_http)?;
        if self.is_local() || self.on_api_origin(&u) {
            return Ok(raw.to_string());
        }
        if u.scheme != "https" {
            return Err(fail(
                "insecure_upload_url",
                format!("the registry asked for a plaintext upload to {} — refusing to send the artifact over http", u.authority),
            ));
        }
        if internal_host(&u.host) {
            return Err(fail(
                "untrusted_endpoint",
                format!("the registry pointed the upload at {}, which is inside this machine or its network — refusing", u.authority),
            ));
        }
        Ok(raw.to_string())
    }

    fn is_local(&self) -> bool {
        parse(&self.base_url).is_some_and(|b| is_loopback(&b.host))
    }
}

fn header(res: &ureq::http::Response<ureq::Body>, name: &str) -> String {
    res.headers().get(name).and_then(|v| v.to_str().ok()).unwrap_or_default().to_string()
}

fn unexpected_redirect(status: u16, endpoint: &str, location: &str) -> Error {
    let mut body = BTreeMap::new();
    body.insert("error".into(), json!("unexpected_redirect"));
    body.insert("endpoint".into(), json!(endpoint));
    body.insert("location".into(), json!(location));
    body.insert(
        "fix".into(),
        json!("the server redirected this request — following it could carry the request past the origin checks, so it is not followed"),
    );
    body.insert("retryable".into(), json!(false));
    Error { status, body }
}

fn malformed(status: u16, fix: &str) -> Error {
    let mut e = fail("malformed_response", fix);
    e.status = status;
    e
}

fn malformed_success(status: u16) -> Error {
    let mut e = malformed(
        status,
        "the registry answered with a success status and a body this client could not read — check KROWK_API_URL points at the API host, not the website",
    );
    e.body.remove("retryable");
    e
}

/// The registry's envelope — {"error": {"code", "message", "details"}} —
/// flattened, with the fix this client knows for the code.
fn response_error(status: u16, payload: &[u8], retry_after: &str) -> Error {
    let mut body = BTreeMap::new();
    let parsed: Option<Value> = serde_json::from_slice(payload).ok();
    let envelope = parsed.as_ref().and_then(|v| v.get("error")).filter(|e| e.get("code").and_then(Value::as_str).is_some_and(|c| !c.is_empty()));
    if let Some(e) = envelope {
        body.insert("error".into(), e["code"].clone());
        if let Some(m) = e.get("message").and_then(Value::as_str).filter(|m| !m.is_empty()) {
            body.insert("message".into(), json!(m));
        }
        if let Some(d) = e.get("details").filter(|d| d.as_object().is_some_and(|o| !o.is_empty())) {
            body.insert("details".into(), d.clone());
        }
    } else {
        body.insert("error".into(), json!(format!("http_{status}")));
        let snippet = String::from_utf8_lossy(payload);
        let snippet = snippet.trim();
        if !snippet.is_empty() {
            // An HTML body is a page — most often Rails' own 404 — and quoting
            // it buries the one fact that matters.
            let detail = if snippet.starts_with('<') { "the registry answered with an HTML page rather than JSON".to_string() } else { clip(snippet, 300) };
            body.insert("detail".into(), json!(detail));
        }
    }
    if !retry_after.is_empty() {
        body.insert("retry_after".into(), json!(retry_after));
    }
    let mut err = Error { status, body };
    let code = err.code();
    let fix = fix_for(&code, status);
    if !fix.is_empty() {
        err.body.insert("fix".into(), json!(fix));
    }
    if let Some(retryable) = retryable_for(&code) {
        err.body.insert("retryable".into(), json!(retryable));
    }
    err
}

/// The registry's codes turned into the next thing to do.
fn fix_for(code: &str, status: u16) -> String {
    match code {
        "unauthorized" => "the registry rejected the key — check KROWK_TOKEN, or run `krowk auth login --token krowk_sk_...`",
        "run_needs_key" => "attaching an upload to a run needs an API key — authenticate, or upload without --run",
        "upload_missing" => "the bytes had not landed when the upload was finalized — retry the upload",
        "checksum_mismatch" => "the file changed while it was being uploaded — retry the upload",
        "empty_upload" => "what arrived held no bytes — check the file is not being written while it is uploaded",
        "already_finalized" => {
            "this artifact's bytes are already stored and a link cannot be pointed at new ones — push again for a new artifact, which is a new link"
        }
        "expired" => {
            "this artifact was anonymous and has passed its expiry — upload it again, and claim it with a key: a Pro workspace keeps it, a free one gets another 24 hours"
        }
        "taken_down" => "this artifact was taken down and its bytes are gone for good — upload it again if the link is still needed",
        "storage_unavailable" => "object storage is temporarily unreachable — retry shortly",
        "not_found" => {
            "no such artifact or run in this workspace — check the slug, and that the key matches the workspace it was uploaded to"
        }
        "no_such_endpoint" => {
            "the registry has no such endpoint — check KROWK_API_URL names the API host and version, and that the method is the one this call uses"
        }
        "parameter_missing" | "invalid" | "bad_request" => "",
        "method_not_allowed" => {
            "the registry does not answer that HTTP method — read the Allow header on this response, if it carries one, for the methods it does"
        }
        "not_acceptable" => "the registry answers JSON only — send `application/json` in Accept and Content-Type, or send neither",
        "internal_server_error" => "the registry failed on its side — retry, and report it if it persists",
        "unexpected_error" => "the registry refused this in a way it has no specific code for — check the status, and report it if it persists",
        _ if status >= 500 => "the registry failed on its side — retry, and report it if it persists",
        _ if status == 404 => {
            "the registry has no such endpoint — check KROWK_API_URL names the API host and version, since it routes by hostname and the wrong host answers 404"
        }
        _ => "",
    }
    .into()
}

/// Where the code knows better than the status whether a retry could help.
fn retryable_for(code: &str) -> Option<bool> {
    match code {
        "upload_missing" | "storage_unavailable" => Some(true),
        "invalid" | "parameter_missing" | "unauthorized" | "not_found" | "expired" | "taken_down" | "checksum_mismatch"
        | "empty_upload" | "run_needs_key" | "already_finalized" | "bad_request" | "no_such_endpoint" => Some(false),
        _ => None,
    }
}

/// Retry-After when the registry sends one (seconds, or an HTTP date), capped
/// at a minute; else 500ms, 1s, 2s.
fn backoff(e: &Error, attempt: u32) -> Duration {
    e.body.get("retry_after").and_then(Value::as_str).and_then(retry_after).unwrap_or(Duration::from_millis(250 << attempt))
}

/// The wait a failure asked for, capped: a "come back in a week" must not wedge
/// the CLI for a week.
pub fn retry_after(v: &str) -> Option<Duration> {
    const MAX: Duration = Duration::from_secs(60);
    let v = v.trim();
    if let Ok(secs) = v.parse::<i64>() {
        return (secs > 0).then(|| Duration::from_secs(secs as u64).min(MAX));
    }
    let at = jiff::fmt::rfc2822::parse(v).ok()?.timestamp();
    let wait = at.duration_since(jiff::Timestamp::now());
    (wait.is_positive()).then(|| Duration::try_from(wait).unwrap_or(MAX).min(MAX))
}

/// The wait a failed call asked for, for a caller pacing a loop of its own.
pub fn retry_after_for(e: &Error) -> Option<Duration> {
    e.body.get("retry_after").and_then(Value::as_str).and_then(retry_after)
}

fn lapsed(upload: Option<&Upload>) -> bool {
    upload
        .and_then(|u| u.expires_at.parse::<jiff::Timestamp>().ok())
        .is_some_and(|at| jiff::Timestamp::now() > at)
}

fn clip(s: &str, n: usize) -> String {
    if s.len() <= n {
        return s.to_string();
    }
    let mut cut = n;
    while !s.is_char_boundary(cut) {
        cut -= 1;
    }
    format!("{}…", &s[..cut])
}

/// 128 random bits shaped as a v4 UUID. Unguessable, not merely unique: on a
/// keyless push it is the only thing a retry presents to prove it made the
/// original call.
fn idempotency_key() -> String {
    let mut b = [0u8; 16];
    getrandom(&mut b);
    b[6] = (b[6] & 0x0f) | 0x40;
    b[8] = (b[8] & 0x3f) | 0x80;
    let h: String = b.iter().map(|x| format!("{x:02x}")).collect();
    format!("{}-{}-{}-{}-{}", &h[0..8], &h[8..12], &h[12..16], &h[16..20], &h[20..32])
}

/// The operating system's random source.
fn getrandom(buf: &mut [u8]) {
    use std::io::Read as _;
    std::fs::File::open("/dev/urandom").and_then(|mut f| f.read_exact(buf)).expect("the OS random source is readable");
}

/// A slug as one URL path segment: `#`, `?` and `/` in what a caller typed
/// would otherwise address a different endpoint — on a takedown, destroy a
/// different artifact.
pub fn slug_path(slug: &str) -> String {
    escape(slug, b"-._~$&+=:@!'()*")
}

fn escape(s: &str, keep: &[u8]) -> String {
    s.bytes()
        .map(|b| if b.is_ascii_alphanumeric() || keep.contains(&b) { (b as char).to_string() } else { format!("%{b:02X}") })
        .collect()
}

/// The cursor and page size every listing takes.
fn paged(path: &str, before: &str, limit: i64) -> String {
    let mut query = Vec::new();
    if !before.is_empty() {
        query.push(format!("before={}", escape(before, b"-._~")));
    }
    if limit > 0 {
        query.push(format!("limit={limit}"));
    }
    if query.is_empty() { path.into() } else { format!("{path}?{}", query.join("&")) }
}

/// The parts of a URL the boundary judges.
#[derive(Debug, Default, Clone)]
struct Url {
    scheme: String,
    host: String,
    port: u16,
    /// host[:port] as written, for messages.
    authority: String,
}

fn parse(raw: &str) -> Option<Url> {
    let uri: Uri = raw.parse().ok()?;
    let scheme = uri.scheme_str()?.to_ascii_lowercase();
    let authority = uri.authority()?;
    let host = authority.host().trim_start_matches('[').trim_end_matches(']').to_ascii_lowercase();
    let port = authority.port_u16().unwrap_or(if scheme == "https" { 443 } else { 80 });
    Some(Url { scheme, host, port, authority: authority.as_str().rsplit('@').next().unwrap_or_default().to_string() })
}

/// Whether `u` is on `base`'s origin, counting the https upgrade of an http
/// base — same host, default https port, upgrade direction only.
fn on_origin(base: &Url, u: &Url) -> bool {
    if base.host.is_empty() || u.host != base.host {
        return false;
    }
    (u.scheme == base.scheme && u.port == base.port) || (base.scheme == "http" && u.scheme == "https" && u.port == 443)
}

fn resolve_location(from: &str, location: &str) -> Option<String> {
    if location.is_empty() {
        return None;
    }
    if location.contains("://") {
        return Some(location.to_string());
    }
    let u = parse(from)?;
    let origin = format!("{}://{}", u.scheme, u.authority);
    Some(if location.starts_with('/') { format!("{origin}{location}") } else { format!("{origin}/{location}") })
}

fn is_loopback(host: &str) -> bool {
    host == "localhost" || host.parse::<IpAddr>().is_ok_and(|ip| ip.is_loopback())
}

/// A host only reachable from where this process sits. A name is resolved,
/// because "metadata.internal" is a name; one that will not resolve is left to
/// fail on its own.
fn internal_host(host: &str) -> bool {
    if let Ok(ip) = host.parse::<IpAddr>() {
        return reserved_ip(ip);
    }
    (host, 443).to_socket_addrs().map(|mut addrs| addrs.any(|a| reserved_ip(a.ip()))).unwrap_or(false)
}

/// Loopback, private, link-local, unspecified, multicast — and what the
/// standard predicates leave out: carrier-grade NAT (where Tailscale lives),
/// benchmarking, IETF protocol assignments, 240.0.0.0/4, and NAT64, which on
/// an IPv6-only runner is how the metadata service is reached without naming a
/// link-local address at all.
pub fn reserved_ip(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(v4) => {
            let [a, b, c, _] = v4.octets();
            v4.is_loopback()
                || v4.is_private()
                || v4.is_link_local()
                || v4.is_unspecified()
                || v4.is_multicast()
                || v4.is_broadcast()
                || (a == 100 && (64..128).contains(&b))
                || (a == 198 && (b == 18 || b == 19))
                || (a == 192 && b == 0 && c == 0)
                || a >= 240
        }
        IpAddr::V6(v6) => {
            if let Some(v4) = v6.to_ipv4_mapped() {
                return reserved_ip(IpAddr::V4(v4));
            }
            let s = v6.segments();
            v6.is_loopback()
                || v6.is_unspecified()
                || v6.is_multicast()
                || (s[0] & 0xfe00) == 0xfc00
                || (s[0] & 0xffc0) == 0xfe80
                || (s[0] == 0x64 && s[1] == 0xff9b && s[2..6] == [0, 0, 0, 0])
        }
    }
}

/// What the resolver needs to judge an address: the configured registry, and
/// the proxy requests go through.
#[derive(Debug)]
struct Guard {
    base: String,
    proxy: Option<(String, u16)>,
}

impl Guard {
    /// Whether a connection to `addr`, for a request to `uri`, may proceed. The
    /// exemptions are what the user chose: a local registry, the registry's own
    /// host, and the proxy the request goes through.
    fn permit(&self, uri: &Uri, addr: SocketAddr) -> Result<(), Error> {
        let base = parse(&self.base);
        if base.as_ref().is_some_and(|b| is_loopback(&b.host)) || !reserved_ip(addr.ip()) {
            return Ok(());
        }
        let host = uri.host().unwrap_or_default().trim_start_matches('[').trim_end_matches(']').to_ascii_lowercase();
        if base.as_ref().is_some_and(|b| b.host == host) {
            return Ok(());
        }
        if self.proxy.as_ref().is_some_and(|(h, _)| h.eq_ignore_ascii_case(&host)) {
            return Ok(());
        }
        Err(fail("untrusted_endpoint", format!("refusing to connect to {addr}, which is inside this machine or its network")))
    }
}

/// The default resolver, with every answer judged before it can be dialled.
#[derive(Debug)]
struct GuardResolver {
    guard: Arc<Guard>,
    inner: ureq::unversioned::resolver::DefaultResolver,
}

impl ureq::unversioned::resolver::Resolver for GuardResolver {
    fn resolve(
        &self,
        uri: &Uri,
        config: &ureq::config::Config,
        timeout: ureq::unversioned::transport::NextTimeout,
    ) -> Result<ureq::unversioned::resolver::ResolvedSocketAddrs, ureq::Error> {
        let addrs = self.inner.resolve(uri, config, timeout)?;
        for addr in addrs.iter() {
            self.guard.permit(uri, *addr).map_err(|e| ureq::Error::Other(Box::new(e)))?;
        }
        Ok(addrs)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reserved_addresses_are_the_ones_inside_a_network() {
        for ip in ["127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "240.0.0.1", "::1", "fe80::1", "fd00::1", "64:ff9b::a9fe:a9fe", "::ffff:10.0.0.1"] {
            assert!(reserved_ip(ip.parse().unwrap()), "{ip}");
        }
        for ip in ["1.1.1.1", "8.8.8.8", "2606:4700::1111", "100.128.0.1"] {
            assert!(!reserved_ip(ip.parse().unwrap()), "{ip}");
        }
    }

    #[test]
    fn upload_targets_must_be_public_https_unless_the_user_chose_otherwise() {
        let prod = Client::new("https://api.krowk.com/v1", "");
        assert!(prod.storage_origin("https://bucket.r2.cloudflarestorage.com/x").is_ok());
        assert_eq!(prod.storage_origin("http://bucket.example.com/x").unwrap_err().code(), "insecure_upload_url");
        assert_eq!(prod.storage_origin("https://10.0.0.1/x").unwrap_err().code(), "untrusted_endpoint");
        assert_eq!(prod.storage_origin("file:///etc/passwd").unwrap_err().code(), "bad_upload_url");
        assert!(prod.storage_origin("https://api.krowk.com/_storage/x").is_ok());
        let local = Client::new("http://127.0.0.1:8787/v1", "");
        assert!(local.storage_origin("http://127.0.0.1:8787/_storage/x").is_ok());
        let private = Client::new("http://10.0.0.5/v1", "");
        assert!(private.storage_origin("https://10.0.0.5/_storage/x").is_ok());
    }

    #[test]
    fn a_slug_is_one_path_segment_and_listings_page_the_same_way() {
        assert_eq!(slug_path("art_abc"), "art_abc");
        assert_eq!(slug_path("a/b?c#d"), "a%2Fb%3Fc%23d");
        assert_eq!(paged("/runs", "run_x", 10), "/runs?before=run_x&limit=10");
        assert_eq!(paged("/runs", "", 0), "/runs");
    }

    #[test]
    fn registry_failures_flatten_with_the_fix_this_client_knows() {
        let e = response_error(404, br#"{"error":{"code":"not_found","message":"No such artifact."}}"#, "");
        assert_eq!((e.code(), e.status, e.retryable()), ("not_found".into(), 404, false));
        assert!(e.fix().starts_with("no such artifact"));
        let html = response_error(502, b"<html>bad gateway</html>", "3");
        assert_eq!((html.code().as_str(), html.body["detail"].as_str()), ("http_502", Some("the registry answered with an HTML page rather than JSON")));
        assert_eq!(backoff(&html, 1), Duration::from_secs(3));
        assert_eq!(retry_after("99999"), Some(Duration::from_secs(60)));
        assert_eq!(retry_after("-1"), None);
    }

    #[test]
    fn idempotency_keys_are_v4_uuids_and_differ() {
        let (a, b) = (idempotency_key(), idempotency_key());
        assert_ne!(a, b);
        assert_eq!((a.len(), &a[14..15]), (36, "4"));
    }
}
