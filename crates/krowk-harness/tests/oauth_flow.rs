//! The SuperGrok login (R-PROV-4) against a stand-in authorization server:
//! the device code flow, the browser flow with PKCE and a loopback
//! redirect, the credentials file's permissions, and refresh — including two
//! krowk processes refreshing one rotating refresh token.

#[path = "common/mock.rs"]
mod mock;
#[path = "common/providers.rs"]
mod providers;

use krowk_harness::oauth::{self, DevicePrompt, Login, Store, Tokens};
use std::path::PathBuf;

fn dir(name: &str) -> PathBuf {
    let d = std::env::temp_dir().join(format!("krowk-oauth-{name}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&d);
    d
}

fn login(issuer: &str) -> Login {
    Login { issuer: issuer.into(), client_id: None, scope: "openid offline_access".into() }
}

#[cfg(unix)]
fn mode(p: &std::path::Path) -> u32 {
    use std::os::unix::fs::PermissionsExt;
    std::fs::metadata(p).unwrap().permissions().mode() & 0o777
}

#[tokio::test]
async fn r_prov_4_a_device_login_is_stored_in_a_credentials_file_created_0600() {
    let auth = providers::auth_server(3600);
    let http = krowk_harness::http::client().unwrap();
    let mut shown: Vec<DevicePrompt> = Vec::new();
    let stored = oauth::login_device(&http, &login(&auth.mock.url), &mut |p| shown.push(p.clone())).await.unwrap();
    assert_eq!(shown.len(), 1);
    assert_eq!(shown[0].user_code, "KRWK-2026");
    assert!(shown[0].verification_uri_complete.as_deref().unwrap().ends_with("/device?user_code=KRWK-2026"));
    assert_eq!((stored.access_token.as_str(), stored.refresh_token.as_deref(), stored.client_id.as_str()), ("xai-at-1", Some("xai-rt-1"), "krowk-registered"));
    assert!(stored.expires_at_ms.unwrap() > krowk_store::now_ms() + 3_000_000);
    assert!(!format!("{stored:?}").contains("xai-at-1"), "a token never prints");
    {
        let seen = auth.mock.seen.lock().unwrap();
        let polls = seen.iter().filter(|s| s.path == "/oauth2/token").count();
        assert_eq!(polls, 2, "one authorization_pending, then the token");
    }

    let d = dir("device");
    let store = Store::new(d.join("krowk").join(oauth::CREDENTIALS_FILE));
    store.save("supergrok", &stored).unwrap();
    #[cfg(unix)]
    {
        assert_eq!(mode(&store.path), 0o600, "the credentials file is created 0600");
        assert_eq!(mode(store.path.parent().unwrap()), 0o700);
    }
    // A second login lands beside the first; neither is lost.
    store.save("grok:team", &stored).unwrap();
    assert_eq!(store.names().unwrap(), ["grok:team", "supergrok"]);
    #[cfg(unix)]
    assert_eq!(mode(&store.path), 0o600, "and stays 0600 when rewritten");
    assert!(store.remove("grok:team").unwrap() && !store.remove("grok:team").unwrap());
    // A file krowk cannot read is never written over.
    std::fs::write(&store.path, "not json").unwrap();
    assert!(store.save("supergrok", &stored).unwrap_err().message.contains("refusing to write over it"));
    assert_eq!(std::fs::read_to_string(&store.path).unwrap(), "not json");
    let _ = std::fs::remove_dir_all(&d);
}

#[tokio::test]
async fn r_prov_4_a_browser_login_uses_pkce_and_a_loopback_redirect() {
    let auth = providers::auth_server(3600);
    auth.state.lock().unwrap().registration = false;
    let http = krowk_harness::http::client().unwrap();
    let page = std::sync::Arc::new(std::sync::Mutex::new(String::new()));
    let seen_page = page.clone();
    let mut opened = String::new();
    let l = Login { client_id: Some("krowk-test".into()), ..login(&auth.mock.url) };
    let stored = oauth::login_pkce(&http, &l, &mut |url: &str| {
        opened = url.to_string();
        let url = url.to_string();
        let page = seen_page.clone();
        std::thread::spawn(move || {
            // A probe with the wrong state first: ignored, the login waits on.
            let port = url.split("redirect_uri=http%3A%2F%2F127.0.0.1%3A").nth(1).unwrap().split("%2F").next().unwrap().to_string();
            let mut probe = std::net::TcpStream::connect(format!("127.0.0.1:{port}")).unwrap();
            use std::io::{Read, Write};
            write!(probe, "GET /callback?code=forged&state=wrong HTTP/1.1\r\nhost: x\r\n\r\n").unwrap();
            let mut answer = String::new();
            let _ = probe.read_to_string(&mut answer);
            assert!(answer.starts_with("HTTP/1.1 400"), "{answer}");
            *page.lock().unwrap() = providers::browse(&url);
        });
    })
    .await
    .unwrap();
    assert_eq!(stored.access_token, "xai-at-1");
    assert!(page.lock().unwrap().contains("krowk is signed in"));
    for want in ["response_type=code", "client_id=krowk-test", "code_challenge_method=S256", "code_challenge=", "state=", "scope=openid+offline_access"] {
        assert!(opened.contains(want), "{want} in {opened}");
    }
    // The verifier went to the token endpoint, never in the URL.
    {
        let seen = auth.mock.seen.lock().unwrap();
        let exchange = seen.iter().find(|s| s.path == "/oauth2/token").unwrap();
        assert!(exchange.raw.contains("code_verifier=") && exchange.raw.contains("grant_type=authorization_code"));
        assert!(!opened.contains("code_verifier"));
    }
    // No client id and no registration: a login says what to pass.
    let e = oauth::login_device(&http, &login(&auth.mock.url), &mut |_| {}).await.unwrap_err();
    assert!(e.message.contains("--client-id"), "{}", e.message);
    // Plain http anywhere but loopback is refused before anything is sent.
    let e = oauth::login_device(&http, &login("http://auth.example"), &mut |_| {}).await.unwrap_err();
    assert!(e.message.contains("https"), "{}", e.message);
}

#[tokio::test]
async fn r_prov_4_an_expired_token_is_refreshed_once_across_processes_and_rotation_is_kept() {
    // Tokens that are already inside the expiry margin when issued.
    let auth = providers::auth_server(0);
    let http = krowk_harness::http::client().unwrap();
    let first = oauth::login_device(&http, &login(&auth.mock.url), &mut |_| {}).await.unwrap();
    let d = dir("refresh");
    let store = Store::new(d.join(oauth::CREDENTIALS_FILE));
    store.save("supergrok", &first).unwrap();
    // Two sessions (two krowk processes) holding the same login.
    let a = Tokens::open(store.clone(), "supergrok").unwrap();
    let b = Tokens::open(store.clone(), "supergrok").unwrap();
    assert_eq!(a.bearer(&http, false).await.unwrap(), "xai-at-2");
    assert_eq!(store.load("supergrok").unwrap().unwrap().refresh_token.as_deref(), Some("xai-rt-2"), "the rotated refresh token is saved");
    // B's copy is stale, and its refresh token already spent: it reads the
    // file under the lock and refreshes with the live one.
    assert_eq!(b.bearer(&http, false).await.unwrap(), "xai-at-3");
    assert_eq!(auth.state.lock().unwrap().refreshes, 2, "no refresh was made with a spent token");
    #[cfg(unix)]
    assert_eq!(mode(&store.path), 0o600);
    // A refresh the server refuses asks for a new login, and names no token.
    auth.state.lock().unwrap().refresh = "revoked".into();
    let e = a.bearer(&http, true).await.unwrap_err();
    assert_eq!(e.code, "not_authenticated");
    assert!(e.message.contains("krowk providers add supergrok") && !e.message.contains("xai-rt") && !e.message.contains("xai-at"), "{}", e.message);
    // An instance with no login says how to sign in.
    assert!(Tokens::open(store.clone(), "grok:team").unwrap_err().message.contains("krowk providers add supergrok --name grok:team"));
    let _ = std::fs::remove_dir_all(&d);
}
