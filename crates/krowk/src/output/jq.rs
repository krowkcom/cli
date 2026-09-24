//! --jq: a jq expression run over the JSON a command rendered, in process.
//!
//! What it does not buy is a second contract: every command renders what it
//! would have, and the filter reads that. The process environment is out of
//! reach — `env` and `$ENV` answer `{}` — because KROWK_TOKEN lives there and
//! an expression is a string that travels: into a skill file, a CI job, a
//! command one agent hands another.

use krowk_api::{fail, Error};
use jaq_core::load::{Arena, File, Loader};
use jaq_core::{data, Compiler, Ctx, Vars};
use jaq_json::Val;
use regex_lite::Regex;
use std::sync::LazyLock;
use std::time::Duration;

/// One run's ceiling. An expression that recurses without a base case would
/// otherwise wedge an unattended container with no diagnostic.
const DEADLINE: Duration = Duration::from_secs(30);

/// A --jq expression that compiled.
#[derive(Debug)]
pub struct Filter {
    expr: String,
}

/// Parses and compiles before the command does anything, so a typo is refused
/// before an upload lands. `given` is whether the flag appeared at all: an
/// empty `--jq "$FIELD"` is a shell that expanded a variable into nothing, and
/// answering it with the whole envelope would hand over what it asked a field of.
pub fn compile(expr: &str, given: bool) -> Result<Option<Filter>, Error> {
    if !given {
        return Ok(None);
    }
    if expr.trim().is_empty() {
        return Err(fail("bad_jq", "a blank --jq is not an expression — pass one, e.g. --jq '.data.artifacts[].slug'"));
    }
    with_filter(expr, |_| ()).map_err(|why| fail("bad_jq", format!("--jq: {why}")))?;
    Ok(Some(Filter { expr: expr.to_string() }))
}

/// Whether a failure is one --jq caused — the one kind a filter may not be run
/// over, since it would bury the news.
pub fn is_filter_failure(err: &Error) -> bool {
    matches!(err.code().as_str(), "bad_jq" | "jq_failed" | "jq_unsupported")
}

type Compiled = jaq_core::compile::Filter<jaq_core::Native<data::JustLut<Val>>>;

/// Loads and compiles `expr` with jq's standard library, `env` answering `{}`,
/// and hands the result to `f`. The error is a sentence about where it gave up.
fn with_filter<T>(expr: &str, f: impl FnOnce(&Compiled) -> T) -> Result<T, String> {
    let arena = Arena::default();
    let env_def = jaq_core::load::parse("def env: {};", |p| p.defs()).unwrap_or_default();
    let defs = jaq_core::defs().chain(jaq_std::defs()).chain(jaq_json::defs()).chain(env_def);
    let funs = jaq_core::funs().chain(jaq_std::funs()).chain(jaq_json::funs()).filter(|(name, _, _)| *name != "env");
    let modules = Loader::new(defs).load(&arena, File { code: expr, path: () }).map_err(|errs| {
        errs.into_iter()
            .map(|(_, e)| match e {
                jaq_core::load::Error::Io(v) => v.into_iter().map(|(_, s)| s).collect::<Vec<_>>().join("; "),
                jaq_core::load::Error::Lex(v) => {
                    v.iter().map(|(want, at)| format!("expected {} at `{}`", want.as_str(), clip(at))).collect::<Vec<_>>().join("; ")
                }
                jaq_core::load::Error::Parse(v) => {
                    v.iter().map(|(want, at)| format!("expected {} at `{}`", want.as_str(), clip(at))).collect::<Vec<_>>().join("; ")
                }
            })
            .collect::<Vec<_>>()
            .join("; ")
    })?;
    let filter = Compiler::default().with_funs(funs).with_global_vars(["$ENV"]).compile(modules).map_err(|errs| {
        errs.into_iter()
            .flat_map(|(_, undefined)| undefined)
            .map(|(name, why)| format!("undefined {} `{name}`", why.as_str()))
            .collect::<Vec<_>>()
            .join("; ")
    })?;
    Ok(f(&filter))
}

fn clip(s: &str) -> String {
    if s.is_empty() {
        return "the end".into();
    }
    s.chars().take(20).collect()
}

impl Filter {
    /// Runs over one rendered result and returns what it wrote — one value per
    /// line, a string as itself — and how many values were not null. A caller
    /// deciding whether the filter had anything to say needs that count: an
    /// expression written for a result answers `null` over a failure.
    ///
    /// Nothing is returned when a later value fails, so half a listing never
    /// reaches stdout ahead of an exit code.
    pub fn write(&self, rendered: &str, tty: bool) -> Result<(String, usize), Error> {
        let (expr, rendered) = (self.expr.clone(), rendered.to_string());
        let (tx, rx) = std::sync::mpsc::channel();
        // ponytail: a filter past the deadline is abandoned, not stopped; the
        // process is about to exit with the failure anyway.
        std::thread::spawn(move || {
            let _ = tx.send(run(&expr, &rendered, tty));
        });
        rx.recv_timeout(DEADLINE).unwrap_or_else(|_| {
            Err(fail(
                "jq_failed",
                "--jq did not finish within 30s — an expression that never ends, such as one recursing without a base case",
            ))
        })
    }
}

fn run(expr: &str, rendered: &str, tty: bool) -> Result<(String, usize), Error> {
    let input = jaq_json::read::parse_single(rendered.as_bytes())
        .map_err(|e| fail("jq_failed", format!("--jq had no JSON result to filter: {e}")))?;
    with_filter(expr, |filter| {
        let ctx = Ctx::<data::JustLut<Val>>::new(&filter.lut, Vars::new([Val::obj(Default::default())]));
        let (mut out, mut said) = (String::new(), 0);
        for v in filter.id.run((ctx, input)) {
            let v = match v {
                Ok(v) => v,
                Err(exn) => {
                    return Err(match exn.get_err() {
                        Ok(e) => fail("jq_failed", format!("--jq: {}", without_credentials(&e.to_string()))),
                        // halt and halt_error would choose the exit code, and
                        // krowk's exit codes are what happened to the artifact.
                        Err(_) => fail(
                            "jq_failed",
                            "--jq: halt and halt_error are not honoured — in jq they choose the process's exit code, \
                             and krowk's exit codes are what happened to the artifact",
                        ),
                    })
                }
            };
            if !matches!(v, Val::Null) {
                said += 1;
            }
            let line = match &v {
                Val::TStr(s) => {
                    let s = String::from_utf8_lossy(s);
                    if tty { terminal_safe_string(&s) } else { s.into_owned() }
                }
                other => {
                    let encoded = other.to_string();
                    if tty { terminal_safe_json(&encoded) } else { encoded }
                }
            };
            out.push_str(&line);
            out.push('\n');
        }
        Ok((out, said))
    })
    .map_err(|why| fail("bad_jq", format!("--jq: {why}")))?
}

/// The two secrets krowk mints, redacted to their prefix, and the message
/// capped: `error(.)` hands back the whole document otherwise.
fn without_credentials(message: &str) -> String {
    static SECRET: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"(krowk_(?:sk|claim)_)[A-Za-z0-9_-]+").unwrap());
    let message = SECRET.replace_all(message, "${1}[redacted]");
    const LONGEST: usize = 400;
    if message.len() <= LONGEST {
        return message.into_owned();
    }
    let mut cut = LONGEST;
    while !message.is_char_boundary(cut) {
        cut -= 1;
    }
    format!("{} …", &message[..cut])
}

fn unprintable(c: char) -> bool {
    c.is_control() || crate::termclean::reordering(c)
}

/// A string safe to print on a terminal without changing what it says: left
/// alone when nothing in it could repaint the row, escaped when something can.
fn terminal_safe_string(s: &str) -> String {
    if !s.chars().any(unprintable) {
        return s.to_string();
    }
    let mut out = String::new();
    for c in s.chars() {
        match c {
            '\n' => out.push_str("\\n"),
            '\t' => out.push_str("\\t"),
            '\r' => out.push_str("\\r"),
            c if (c as u32) < 0x80 && unprintable(c) => out.push_str(&format!("\\x{:02x}", c as u32)),
            c if unprintable(c) => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out
}

/// The same job on an encoded compound result: JSON's structure is printable
/// ASCII, so anything replaced is inside a string, spelled as JSON spells it.
fn terminal_safe_json(encoded: &str) -> String {
    encoded.chars().map(|c| if unprintable(c) { format!("\\u{:04x}", c as u32) } else { c.to_string() }).collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn filter(expr: &str) -> Filter {
        compile(expr, true).unwrap().unwrap()
    }

    #[test]
    fn strings_print_raw_and_compound_values_as_json() {
        let doc = r#"{"data":{"artifacts":[{"slug":"art_1","byte_size":5}]}}"#;
        assert_eq!(filter(".data.artifacts[0].slug").write(doc, false).unwrap(), ("art_1\n".into(), 1));
        let (out, said) = filter(".data.artifacts[0] | {byte_size}").write(doc, false).unwrap();
        assert_eq!((serde_json::from_str::<serde_json::Value>(&out).unwrap()["byte_size"].as_i64(), said), (Some(5), 1));
        assert_eq!(filter(".data.artifacts[] | select(.byte_size > 100)").write(doc, false).unwrap(), (String::new(), 0));
        assert_eq!(filter(".missing").write(doc, false).unwrap(), ("null\n".into(), 0));
    }

    #[test]
    fn the_environment_is_out_of_reach() {
        assert_eq!(filter("env").write("{}", false).unwrap().0, "{}\n");
        assert_eq!(filter("$ENV.HOME").write("{}", false).unwrap().0, "null\n");
    }

    #[test]
    fn failures_are_classified_and_never_carry_a_credential() {
        assert_eq!(compile("", true).unwrap_err().code(), "bad_jq");
        assert_eq!(compile(".[[[", true).unwrap_err().code(), "bad_jq");
        assert!(compile(".", false).unwrap().is_none());
        let e = filter("error").write(r#""krowk_sk_abc_def-1 leaked""#, false).unwrap_err();
        assert_eq!(e.code(), "jq_failed");
        assert!(!e.fix().contains("abc_def") && e.fix().contains("krowk_sk_[redacted]"), "{}", e.fix());
        assert!(filter("halt_error").write("1", false).unwrap_err().fix().contains("not honoured"));
    }

    #[test]
    fn a_terminal_never_sees_what_could_repaint_it() {
        assert_eq!(filter(".").write(r#""\u001b[31mred""#, true).unwrap().0, "\\x1b[31mred\n");
        assert_eq!(filter(".").write(r#""plain  name""#, true).unwrap().0, "plain  name\n");
        assert_eq!(filter("[.]").write(r#""a\u202eb""#, true).unwrap().0, "[\"a\\u202eb\"]\n");
    }
}
