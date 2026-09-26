//! The device name the status line opens with: `<user>/<short hostname>`,
//! read once when the TUI starts — never again, so a frame costs no
//! syscall and nothing is timed to refresh it.

/// `<user>/<host>`: the user from `$USER` (else `$LOGNAME`, `$USERNAME`,
/// else the password database), the host up to its first dot. Either alone
/// when the other cannot be read; none when neither can.
pub fn name(env: &dyn Fn(&str) -> String) -> Option<String> {
    let user = ["USER", "LOGNAME", "USERNAME"].into_iter().map(env).find(|v| !v.is_empty()).or_else(os_user);
    let host = os_host().or_else(|| Some(env("COMPUTERNAME")));
    label(user.as_deref(), host.as_deref())
}

/// The two put together, each cleaned of anything a terminal would act on.
pub fn label(user: Option<&str>, host: Option<&str>) -> Option<String> {
    let tidy = |s: &str| s.chars().filter(|c| !c.is_control()).collect::<String>().trim().to_string();
    let user = user.map(tidy).filter(|u| !u.is_empty());
    let host = host.map(|h| tidy(h.split('.').next().unwrap_or(h))).filter(|h| !h.is_empty());
    match (user, host) {
        (Some(u), Some(h)) => Some(format!("{u}/{h}")),
        (Some(one), None) | (None, Some(one)) => Some(one),
        (None, None) => None,
    }
}

#[cfg(unix)]
fn os_host() -> Option<String> {
    let mut buf = [0u8; 256];
    // SAFETY: the buffer is ours and its length is passed with it.
    if unsafe { libc::gethostname(buf.as_mut_ptr().cast(), buf.len()) } != 0 {
        return None;
    }
    let end = buf.iter().position(|b| *b == 0).unwrap_or(buf.len());
    Some(String::from_utf8_lossy(&buf[..end]).into_owned()).filter(|h| !h.is_empty())
}

#[cfg(unix)]
fn os_user() -> Option<String> {
    let mut pw: libc::passwd = unsafe { std::mem::zeroed() };
    let mut buf = vec![0u8; 4096];
    let mut out: *mut libc::passwd = std::ptr::null_mut();
    // SAFETY: getpwuid_r writes only into `pw` and `buf`, both ours and
    // sized as passed; `out` is null or points at `pw`.
    let rc = unsafe { libc::getpwuid_r(libc::getuid(), &mut pw, buf.as_mut_ptr().cast(), buf.len(), &mut out) };
    if rc != 0 || out.is_null() || pw.pw_name.is_null() {
        return None;
    }
    // SAFETY: a NUL-terminated string inside `buf`.
    let name = unsafe { std::ffi::CStr::from_ptr(pw.pw_name) };
    Some(name.to_string_lossy().into_owned()).filter(|n| !n.is_empty())
}

#[cfg(not(unix))]
fn os_host() -> Option<String> {
    None
}

#[cfg(not(unix))]
fn os_user() -> Option<String> {
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_device_is_the_user_and_the_short_hostname() {
        assert_eq!(label(Some("elvinas"), Some("primevise-arch-1.local")).as_deref(), Some("elvinas/primevise-arch-1"));
        assert_eq!(label(Some("elvinas"), None).as_deref(), Some("elvinas"));
        assert_eq!(label(Some(""), Some("box")).as_deref(), Some("box"));
        assert_eq!(label(None, Some("")), None);
        assert_eq!(label(Some("a\x1b[2Jb"), Some("h")).as_deref(), Some("a[2Jb/h"), "nothing a terminal would act on");
        let env = |k: &str| if k == "LOGNAME" { "who".to_string() } else { String::new() };
        assert!(name(&env).is_some_and(|n| n.starts_with("who")), "the first of the variables set");
    }
}
