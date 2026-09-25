//! Claude Code's permission rule syntax, parsed and matched (R-PERM-1):
//! `Tool` or `Tool(specifier)`, as `permissions.allow`, `ask` and `deny`
//! hold them in `.claude/settings.json`.
//!
//! | rule | matches |
//! |---|---|
//! | `Bash`, `Bash(*)` | any command |
//! | `Bash(npm test)` | exactly that command |
//! | `Bash(git:*)` | `git`, and `git` followed by anything — the legacy prefix form |
//! | `Bash(git diff *)` | the command against a wildcard; ` *` at the end also matches nothing |
//! | `Read(glob)`, `Edit(glob)` | a path, gitignore-style: `//abs`, `~/home`, `/project-root`, `./cwd` or bare (the working directory), bare names at any depth |
//! | `WebFetch(domain:example.com)` | a fetch of that host (`*.example.com`: and its subdomains) |
//! | `Mcp(server:tool)`, `mcp__server__tool` | an MCP tool; `Mcp(server)`, `mcp__server` or `…__*`: any of the server's |
//! | any other name, e.g. `WebSearch`, `Skill(name)` | the tool of that name (and, with a specifier, its subject) |
//!
//! `Edit` rules cover every tool that changes a file (`Write`, `MultiEdit`,
//! `NotebookEdit` and krowk's own edit tools), as in Claude Code; `Read`
//! rules cover `Grep`, `Glob` and `LS`.
//!
//! **A command is judged as the shell will run it.** It is split into its
//! simple commands — at `;`, `&&`, `||`, `|`, `&`, newlines — and every
//! command substitution (`$(…)`, backticks, `<(…)`) is a command of its
//! own, so `git status && rm -rf x` is two commands and `echo $(rm x)` is
//! two. Quoting is read as bash reads it: `'…'`, `"…"`, `\`, ANSI-C
//! `$'…'` (its escapes decoded, `\'` inside it not the end) and `$"…"`; an
//! escape it does not model (`\cX`) or a quote that never closes makes the
//! line **opaque**. So does a program name the shell computes — an unquoted
//! `$`, a substitution, a brace or a glob character left in it after
//! unquoting (`$(printf rm)`, `$X`, `r{m,}`, `/bin/r?`), and the same in the
//! string handed to `bash -c` or `eval`. No allow rule covers an opaque
//! line, and where a Bash deny rule applies the evaluator asks about one
//! even under bypassPermissions (headless: refuses it).
//!
//! An allow rule allows a line only when it allows every one of its
//! commands, never one that writes a file through a redirection (`>`,
//! `>>`, `>|`, `&>`, `<>`), never git handed config or an alias that can
//! name a program (`-c`, `--config-env`, `--exec-path`, `alias.*`), and a
//! login shell around one command line (`bash -lc '…'`, as Codex runs
//! commands) is judged by that line. A deny rule denies the line when it
//! matches any one command — as written, and again with what only wraps a
//! program stripped away (`sudo`, `env`, `xargs`, `nohup`, `command`,
//! `timeout`, a leading `VAR=value`, a path such as `/bin/rm`), inside
//! `bash -c '…'`, `sh -c`, `eval` and `find -exec`.
//!
//! **What this does not see**, and the OS sandbox (Harness ticket 26) is
//! for: a program the command itself runs by another name (a script, an
//! interpreter — `python -c 'import os; os.remove(…)'`, `perl -e`), a
//! program reached through an alias or function defined in the same line,
//! `PATH` pointed elsewhere before an ordinary-looking name, and writes a
//! program makes itself (`cp`, `tee`, `sed -i`) — a command's own writes
//! are not judged against `Edit` deny rules, only its redirections are
//! refused an allow. A deny rule is the policy; the sandbox is the boundary.

use std::path::{Path, PathBuf};

/// What a rule tells the evaluator to do with a call it matches.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Allow,
    Ask,
    Deny,
}

/// One rule, as written, and where it came from.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Rule {
    /// The tool, by Claude Code's name: `Bash`, `Read`, `Edit`, `WebFetch`,
    /// `mcp__server__tool`, …
    pub tool: String,
    pub spec: Option<String>,
    /// The rule as it was written, for messages.
    pub text: String,
    /// The file (or grant) it came from, for messages.
    pub source: String,
    /// The directory `/pattern` path rules are relative to: the project
    /// root the file belongs to.
    pub root: PathBuf,
}

/// Claude Code's name for a tool, whichever way a rule or a call spells it:
/// krowk's own tools (`bash`, `read`, `str_replace`, …) and Claude's names
/// in any case.
pub fn canonical(tool: &str) -> String {
    let lower = tool.to_ascii_lowercase();
    match lower.as_str() {
        "bash" => "Bash",
        "read" => "Read",
        "write" => "Write",
        "edit" | "str_replace" | "search_replace" | "apply_patch" => "Edit",
        "multiedit" => "MultiEdit",
        "notebookedit" => "NotebookEdit",
        "notebookread" => "NotebookRead",
        "grep" => "Grep",
        "glob" => "Glob",
        "ls" => "LS",
        "webfetch" => "WebFetch",
        "websearch" => "WebSearch",
        "skill" => "Skill",
        "task" => "Task",
        "publish" => "Publish",
        "mcp" => "Mcp",
        _ => return tool.to_string(),
    }
    .to_string()
}

/// Parses `Tool` or `Tool(spec)`. `Tool()` and `Tool(*)` are the bare tool.
pub fn parse(text: &str, source: &str, root: &Path) -> Result<Rule, String> {
    let t = text.trim();
    if t.is_empty() {
        return Err("an empty rule".into());
    }
    let (tool, spec) = match t.find('(') {
        Some(open) => {
            if !t.ends_with(')') {
                return Err(format!("the rule {t:?} opens a `(` it does not close at its end"));
            }
            let spec = t[open + 1..t.len() - 1].trim();
            (&t[..open], (!spec.is_empty() && spec != "*").then(|| spec.to_string()))
        }
        None => (t, None),
    };
    let tool = tool.trim();
    if tool.is_empty() || !tool.chars().all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-' || c == '*') {
        return Err(format!("the rule {t:?} does not start with a tool name"));
    }
    // `Mcp(server:tool)` is `mcp__server__tool`: one form to match.
    let (tool, spec) = match (canonical(tool).as_str(), spec) {
        ("Mcp", Some(s)) => {
            let (server, t2) = s.split_once(':').unwrap_or((&s, "*"));
            (if t2 == "*" || t2.is_empty() { format!("mcp__{server}") } else { format!("mcp__{server}__{t2}") }, None)
        }
        ("Mcp", None) => ("mcp__*".to_string(), None),
        (name, spec) => (name.to_string(), spec),
    };
    if let Some(s) = &spec
        && matches!(tool.as_str(), "Read" | "Edit" | "Write" | "MultiEdit" | "NotebookEdit" | "NotebookRead" | "Grep" | "Glob" | "LS" | "Publish")
    {
        check_path_spec(s).map_err(|e| format!("the rule {t:?}: {e}"))?;
    }
    Ok(Rule { tool, spec, text: t.to_string(), source: source.to_string(), root: root.to_path_buf() })
}

/// The rule in Claude Code's own spelling, for `--disallowedTools`.
///
/// Claude Code reads a `/path` rule relative to the settings file it came
/// from, which a command-line rule has none of, so a project-root rule is
/// handed over anchored absolutely (`//<root>/path`). What Claude Code has
/// no tool for — krowk's own `Publish` (Claude Code's is
/// `mcp__krowk__publish`, judged in krowk's bridge whatever Claude Code
/// decides) and "every MCP tool", which would take krowk's own tools with
/// it — is left out: krowk holds those itself.
pub fn claude_spelling(r: &Rule) -> Option<String> {
    if r.tool == "Publish" || r.tool == "mcp__*" || r.tool.starts_with("mcp__krowk") {
        return None;
    }
    let path_tool = matches!(r.tool.as_str(), "Read" | "Edit" | "Write" | "MultiEdit" | "NotebookEdit" | "NotebookRead" | "Grep" | "Glob" | "LS");
    Some(match &r.spec {
        Some(s) if path_tool && s.starts_with('/') && !s.starts_with("//") => format!("{}(/{}/{})", r.tool, r.root.display().to_string().trim_end_matches('/'), s.trim_start_matches('/')),
        Some(s) => format!("{}({s})", r.tool),
        None => r.tool.clone(),
    })
}

/// What a call is, in the terms rules are written in.
#[derive(Debug, Clone, PartialEq)]
pub enum Access {
    /// Reads these paths (absolute, as spelled).
    Read(Vec<PathBuf>),
    /// Changes these paths.
    Edit(Vec<PathBuf>),
    /// Uploads these files to a public link (`publish`): judged as an edit
    /// by the mode, and as a read of them by a `Read` deny rule.
    Publish(Vec<PathBuf>),
    /// Runs this command line.
    Bash(String),
    /// Fetches this URL.
    Fetch(String),
    /// Calls this MCP tool.
    Mcp { server: String, tool: String },
    /// Loads a skill's instructions (the `subject` is its name) from its
    /// `SKILL.md`, when krowk knows where that is: `Skill(name)` rules
    /// judge it by name, and a `Read` deny rule by the file.
    Skill(Option<PathBuf>),
    /// Needs no permission at all: krowk's own bridged tools, a todo list.
    Free,
    /// Anything else, by name — asked about unless a rule says otherwise.
    Other,
}

/// A call, as the evaluator judges it.
#[derive(Debug, Clone, PartialEq)]
pub struct Call {
    /// Claude Code's name for the tool.
    pub tool: String,
    pub access: Access,
    /// What a specifier of an `Other` tool's rule is matched against: a
    /// skill's name, a subagent's type.
    pub subject: Option<String>,
}

/// Where paths in rules are resolved from.
#[derive(Debug, Clone)]
pub struct Places<'a> {
    pub cwd: &'a Path,
    pub home: Option<&'a Path>,
}

/// Whether `r` matches every unit of `call` (`all`, for allow and ask
/// rules) or any unit of it (`!all`, for deny rules). A unit is a path, or
/// one simple command of a command line.
pub fn matches(r: &Rule, call: &Call, at: &Places<'_>, all: bool) -> bool {
    match &call.access {
        Access::Bash(cmd) => r.tool == "Bash" && bash_matches(r.spec.as_deref(), cmd, all),
        Access::Skill(file) => {
            let by_name = r.tool == "Skill" && r.spec.as_deref().is_none_or(|s| call.subject.as_deref().is_some_and(|n| wildcard(s, n)));
            let read = Call { tool: "Read".into(), access: Access::Read(file.iter().cloned().collect()), subject: None };
            by_name || (!all && r.tool == "Read" && file.is_some() && matches(r, &read, at, false))
        }
        Access::Read(paths) | Access::Edit(paths) | Access::Publish(paths) => {
            let family = matches!(call.access, Access::Edit(_)) && matches!(r.tool.as_str(), "Edit" | "Write" | "MultiEdit" | "NotebookEdit");
            let family = family || (matches!(call.access, Access::Read(_)) && r.tool == "Read");
            // What a deny rule keeps from being read is not uploaded either.
            let family = family || (matches!(call.access, Access::Publish(_)) && !all && r.tool == "Read");
            if !family && r.tool != call.tool {
                return false;
            }
            let Some(spec) = &r.spec else { return true };
            let pat = PathPattern::new(spec, &r.root, at);
            let hit = |p: &PathBuf| pat.matches(p, !all);
            if paths.is_empty() {
                return false;
            }
            if all { paths.iter().all(hit) } else { paths.iter().any(hit) }
        }
        Access::Fetch(url) => {
            if r.tool != "WebFetch" {
                return false;
            }
            match r.spec.as_deref().map(str::trim) {
                None => true,
                Some(s) => match s.strip_prefix("domain:") {
                    Some(d) => host_of(url).is_some_and(|h| domain_matches(d.trim(), &h)),
                    None => wildcard(s, url),
                },
            }
        }
        Access::Mcp { server, tool } => mcp_matches(&r.tool, server, tool),
        Access::Free | Access::Other => {
            r.tool == call.tool && r.spec.as_deref().is_none_or(|s| call.subject.as_deref().is_some_and(|subj| wildcard(s, subj)))
        }
    }
}

fn mcp_matches(rule_tool: &str, server: &str, tool: &str) -> bool {
    let Some(rest) = rule_tool.strip_prefix("mcp__") else { return false };
    if rest == "*" {
        return true;
    }
    match rest.split_once("__") {
        None => rest == server,
        Some((s, t)) => s == server && (t == "*" || t == tool),
    }
}

fn host_of(url: &str) -> Option<String> {
    let u = url::Url::parse(url).ok()?;
    u.host_str().map(|h| h.trim_end_matches('.').to_ascii_lowercase())
}

fn domain_matches(pattern: &str, host: &str) -> bool {
    let pattern = pattern.to_ascii_lowercase();
    match pattern.strip_prefix("*.") {
        Some(base) => host == base || host.ends_with(&format!(".{base}")),
        None => host == pattern,
    }
}

/// `*` matches any run of characters, everything else itself. Linear-ish:
/// it backtracks only to the last star.
pub fn wildcard(pattern: &str, s: &str) -> bool {
    let (p, s): (Vec<char>, Vec<char>) = (pattern.chars().collect(), s.chars().collect());
    let (mut pi, mut si, mut star) = (0usize, 0usize, None::<(usize, usize)>);
    while si < s.len() {
        if pi < p.len() && p[pi] == '*' {
            star = Some((pi, si));
            pi += 1;
        } else if pi < p.len() && p[pi] == s[si] {
            pi += 1;
            si += 1;
        } else if let Some((sp, ss)) = star {
            star = Some((sp, ss + 1));
            pi = sp + 1;
            si = ss + 1;
        } else {
            return false;
        }
    }
    p[pi..].iter().all(|c| *c == '*')
}

/// A path rule's pattern: the directory it is anchored at, and a glob over
/// what is below it. The anchor — the project root, the working directory,
/// the home — is compared as a path, never compiled into the glob, so a
/// directory named `a[1]` or `{x}` is itself and not a pattern.
struct PathPattern {
    /// Anchored at this directory, with `glob` over the rest (compiled as
    /// `/rest`, so it matches the whole of what is below).
    anchored: Option<(PathBuf, Glob, Glob)>,
    /// A bare name (no `/`), matched against the file's name: anywhere for
    /// a deny rule, under the working directory for the others. And folded.
    name: Option<(Glob, Glob)>,
    cwd: PathBuf,
}

use crate::tools::search::Glob;

/// Where a path rule's specifier is anchored, and the glob below it.
enum Anchor<'s> {
    At(PathBuf, String),
    /// Nowhere yet: a `~/` rule with no home known.
    Unknown,
    Name(&'s str),
}

fn anchor<'s>(spec: &'s str, root: &Path, at: &Places<'_>) -> Anchor<'s> {
    if let Some(rest) = spec.strip_prefix("//") {
        Anchor::At(PathBuf::from("/"), rest.to_string())
    } else if let Some(rest) = spec.strip_prefix("~/") {
        at.home.map_or(Anchor::Unknown, |h| Anchor::At(h.to_path_buf(), rest.to_string()))
    } else if let Some(rest) = spec.strip_prefix('/') {
        Anchor::At(root.to_path_buf(), rest.to_string())
    } else {
        let rest = spec.trim_start_matches("./");
        if rest.contains('/') { Anchor::At(at.cwd.to_path_buf(), rest.to_string()) } else { Anchor::Name(rest) }
    }
}

/// A path rule's specifier as the file it came from gave it, a directory
/// meaning everything under it (as in .gitignore).
fn normal(spec: &str) -> String {
    let spec = spec.trim();
    if spec.ends_with('/') { format!("{spec}**") } else { spec.to_string() }
}

/// Whether a path rule's specifier compiles. A settings file whose rule
/// does not is refused when it is loaded: a deny rule that could never
/// match would stop holding without a word.
pub fn check_path_spec(spec: &str) -> Result<(), String> {
    let spec = normal(spec);
    let glob = match anchor(&spec, Path::new("/"), &Places { cwd: Path::new("/"), home: Some(Path::new("/")) }) {
        Anchor::At(_, rest) => format!("/{rest}"),
        Anchor::Name(n) => n.to_string(),
        Anchor::Unknown => return Ok(()),
    };
    Glob::new(&glob).map(drop).map_err(|e| format!("the path pattern {spec:?} does not compile: {e}"))
}

impl PathPattern {
    fn new(spec: &str, root: &Path, at: &Places<'_>) -> PathPattern {
        let spec = normal(spec);
        let both = |g: &str| Some((Glob::new(g).ok()?, Glob::new(&g.to_lowercase()).ok()?));
        let (anchored, name) = match anchor(&spec, root, at) {
            Anchor::At(dir, rest) => (both(&format!("/{rest}")).map(|(g, f)| (dir, g, f)), None),
            Anchor::Unknown => (None, None),
            Anchor::Name(n) => (None, both(n)),
        };
        PathPattern { anchored, name, cwd: at.cwd.to_path_buf() }
    }

    /// Judged as the path is spelled and as it really leads, and — for a
    /// deny rule, `anywhere` — case-folded on both sides, since macOS and
    /// Windows open `.ENV` as `.env` and `Secrets/` as `secrets/`.
    fn matches(&self, p: &Path, anywhere: bool) -> bool {
        let real = crate::tools::real_path(p, 0).unwrap_or_else(|_| p.to_path_buf());
        let fold = |q: &Path| PathBuf::from(q.to_string_lossy().to_lowercase());
        let canon = |d: &Path| d.canonicalize().unwrap_or_else(|_| d.to_path_buf());
        let below = |f: &Path, dir: &Path, folded: bool| -> Option<String> {
            let dirs = [dir.to_path_buf(), canon(dir)];
            dirs.iter().find_map(|d| {
                let (f, d) = if folded { (fold(f), fold(d)) } else { (f.to_path_buf(), d.clone()) };
                f.strip_prefix(&d).ok().map(|r| format!("/{}", r.to_string_lossy()))
            })
        };
        let forms = [p.to_path_buf(), real];
        let folds: &[bool] = if anywhere { &[false, true] } else { &[false] };
        forms.iter().any(|f| {
            folds.iter().any(|&folded| {
                if let Some((dir, g, gf)) = &self.anchored
                    && let Some(rel) = below(f, dir, folded)
                    && (if folded { gf } else { g }).matches(Path::new(&rel))
                {
                    return true;
                }
                let Some((n, nf)) = &self.name else { return false };
                let n = if folded { nf } else { n };
                let name_of = |c: std::path::Component<'_>| {
                    let s = c.as_os_str().to_string_lossy();
                    if folded { s.to_lowercase() } else { s.into_owned() }
                };
                // A bare name is matched below the working directory, as a
                // .gitignore's is below its own; a deny rule's anywhere else too.
                match below(f, &self.cwd, folded) {
                    Some(rel) => Path::new(&rel).components().any(|c| n.matches(Path::new(&name_of(c)))),
                    None => anywhere && f.components().any(|c| n.matches(Path::new(&name_of(c)))),
                }
            })
        })
    }
}

/// One simple command: its words with quoting taken off, which of them
/// the shell would still expand (`dynamic`), and whether it writes a file
/// through a redirection.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Simple {
    pub words: Vec<String>,
    /// Parallel to `words`: the word had an unquoted `$`, a substitution, a
    /// brace or a glob character in it, so what the shell runs is not the
    /// text krowk read.
    pub dynamic: Vec<bool>,
    pub writes: bool,
}

/// A command line, split the way the shell would run it: every simple
/// command, the ones inside substitutions included. `opaque` when krowk
/// cannot say what it runs: a quote that never closes, an escape it does
/// not model, or a program name the shell would expand.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Parsed {
    pub commands: Vec<Simple>,
    pub opaque: bool,
}

/// Splits a command line into simple commands.
pub fn split(cmd: &str) -> Parsed {
    let mut out = Parsed::default();
    lex(cmd, &mut out, 0);
    // A program whose name the shell computes — `$X`, `$(printf rm)`,
    // `r{m,}`, `/bin/r?` — is not the program krowk read, so no rule can
    // judge the line.
    for c in &out.commands {
        let mut forms = Vec::new();
        if deny_forms(&pairs(c), &mut forms, 0) {
            out.opaque = true;
        }
    }
    out
}

fn pairs(c: &Simple) -> Vec<(String, bool)> {
    c.words.iter().cloned().zip(c.dynamic.iter().copied().chain(std::iter::repeat(false))).collect()
}

/// Substitutions nested deeper than this are not followed; the line is
/// opaque instead.
const MAX_NESTING: usize = 8;

/// A word being read.
#[derive(Default)]
struct Word {
    text: String,
    started: bool,
    dynamic: bool,
}

/// Bash's `$'…'` (ANSI-C quoting) from just after the opening quote: its
/// text with the escapes decoded, and the index after the closing quote.
/// None when it never closes or uses an escape krowk does not model — the
/// caller makes the line opaque rather than guess where it ends.
fn ansi_c(chars: &[char], mut i: usize) -> Option<(String, usize)> {
    let mut s = String::new();
    let hex = |chars: &[char], i: usize, max: usize| -> (u32, usize) {
        let mut v = 0u32;
        let mut n = 0;
        while n < max && chars.get(i + n).is_some_and(|c| c.is_ascii_hexdigit()) {
            v = v * 16 + chars[i + n].to_digit(16).unwrap_or(0);
            n += 1;
        }
        (v, n)
    };
    while i < chars.len() {
        match chars[i] {
            '\'' => return Some((s, i + 1)),
            '\\' => {
                let e = *chars.get(i + 1)?;
                i += 2;
                match e {
                    'a' => s.push('\u{7}'),
                    'b' => s.push('\u{8}'),
                    'e' | 'E' => s.push('\u{1b}'),
                    'f' => s.push('\u{c}'),
                    'n' => s.push('\n'),
                    'r' => s.push('\r'),
                    't' => s.push('\t'),
                    'v' => s.push('\u{b}'),
                    '\\' | '\'' | '"' | '?' => s.push(e),
                    '0'..='7' => {
                        let mut v = e.to_digit(8).unwrap_or(0);
                        let mut n = 0;
                        while n < 2 && chars.get(i).is_some_and(|c| ('0'..='7').contains(c)) {
                            v = v * 8 + chars[i].to_digit(8).unwrap_or(0);
                            i += 1;
                            n += 1;
                        }
                        s.push(char::from_u32(v)?);
                    }
                    'x' | 'u' | 'U' => {
                        let (v, n) = hex(chars, i, match e { 'x' => 2, 'u' => 4, _ => 8 });
                        if n == 0 {
                            return None;
                        }
                        i += n;
                        s.push(char::from_u32(v)?);
                    }
                    // `\cX`, and anything else: not modelled.
                    _ => return None,
                }
            }
            c => {
                s.push(c);
                i += 1;
            }
        }
    }
    None
}

fn lex(cmd: &str, out: &mut Parsed, depth: usize) {
    if depth > MAX_NESTING {
        out.opaque = true;
        return;
    }
    let chars: Vec<char> = cmd.chars().collect();
    let mut cur = Simple::default();
    let mut w = Word::default();
    // The next word is a redirection's target, and whether it writes.
    let mut redirect: Option<bool> = None;
    let mut i = 0;
    let finish_word = |w: &mut Word, cur: &mut Simple, redirect: &mut Option<bool>| {
        if w.started {
            match redirect.take() {
                Some(writes) => {
                    if writes && (w.text != "/dev/null" || w.dynamic) && !w.text.starts_with('&') {
                        cur.writes = true;
                    }
                }
                None => {
                    cur.words.push(std::mem::take(&mut w.text));
                    cur.dynamic.push(w.dynamic);
                }
            }
            *w = Word::default();
        }
    };
    // `[` and `[[` alone are the test command, not a glob.
    let settle = |w: &mut Word| {
        if w.text == "[" || w.text == "[[" || w.text == "]" || w.text == "]]" || w.text == "{" || w.text == "}" {
            w.dynamic = false;
        }
    };
    let finish_cmd = |cur: &mut Simple, out: &mut Parsed| {
        let c = std::mem::take(cur);
        if !c.words.is_empty() || c.writes {
            out.commands.push(c);
        }
    };
    while i < chars.len() {
        let c = chars[i];
        match c {
            '\\' => {
                if chars.get(i + 1) == Some(&'\n') {
                    i += 2;
                    continue;
                }
                if let Some(n) = chars.get(i + 1) {
                    w.text.push(*n);
                }
                w.started = true;
                i += 2;
            }
            '\'' => {
                w.started = true;
                match chars[i + 1..].iter().position(|x| *x == '\'') {
                    Some(end) => {
                        w.text.extend(&chars[i + 1..i + 1 + end]);
                        i += end + 2;
                    }
                    None => {
                        out.opaque = true;
                        w.text.extend(&chars[i + 1..]);
                        i = chars.len();
                    }
                }
            }
            // `$'…'`: ANSI-C quoting, where `\'` does not end the string.
            '$' if chars.get(i + 1) == Some(&'\'') => {
                w.started = true;
                match ansi_c(&chars, i + 2) {
                    Some((s, next)) => {
                        w.text.push_str(&s);
                        i = next;
                    }
                    None => {
                        out.opaque = true;
                        i = chars.len();
                    }
                }
            }
            // `$"…"` is a double-quoted string the shell may translate: read
            // as one, and marked as what the shell decides.
            '"' | '$' if c == '"' || chars.get(i + 1) == Some(&'"') => {
                if c == '$' {
                    w.dynamic = true;
                    i += 1;
                }
                w.started = true;
                i += 1;
                let mut closed = false;
                while i < chars.len() {
                    match chars[i] {
                        '"' => {
                            closed = true;
                            i += 1;
                            break;
                        }
                        '\\' if matches!(chars.get(i + 1), Some('"' | '\\' | '$' | '`' | '\n')) => {
                            w.text.push(chars[i + 1]);
                            i += 2;
                        }
                        '$' if chars.get(i + 1) == Some(&'(') => {
                            let (inner, next) = balanced(&chars, i + 2);
                            lex(&inner, out, depth + 1);
                            w.text.push_str("$(…)");
                            w.dynamic = true;
                            i = next;
                        }
                        '`' => {
                            let end = chars[i + 1..].iter().position(|x| *x == '`').map(|e| i + 1 + e);
                            let inner: String = chars[i + 1..end.unwrap_or(chars.len())].iter().collect();
                            lex(&inner, out, depth + 1);
                            w.text.push_str("`…`");
                            w.dynamic = true;
                            i = end.map_or(chars.len(), |e| e + 1);
                        }
                        '$' => {
                            w.text.push('$');
                            w.dynamic = true;
                            i += 1;
                        }
                        ch => {
                            w.text.push(ch);
                            i += 1;
                        }
                    }
                }
                if !closed {
                    out.opaque = true;
                }
            }
            '$' if chars.get(i + 1) == Some(&'(') => {
                let (inner, next) = balanced(&chars, i + 2);
                lex(&inner, out, depth + 1);
                w.text.push_str("$(…)");
                w.started = true;
                w.dynamic = true;
                i = next;
            }
            '`' => {
                let end = chars[i + 1..].iter().position(|x| *x == '`').map(|e| i + 1 + e);
                let inner: String = chars[i + 1..end.unwrap_or(chars.len())].iter().collect();
                if end.is_none() {
                    out.opaque = true;
                }
                lex(&inner, out, depth + 1);
                w.text.push_str("`…`");
                w.started = true;
                w.dynamic = true;
                i = end.map_or(chars.len(), |e| e + 1);
            }
            '<' | '>' if chars.get(i + 1) == Some(&'(') => {
                let (inner, next) = balanced(&chars, i + 2);
                lex(&inner, out, depth + 1);
                w.text.push_str("<(…)");
                w.started = true;
                w.dynamic = true;
                i = next;
            }
            '>' | '<' => {
                // A file descriptor written straight before it (`2>`) is part
                // of the redirection, not a word.
                if w.started && !w.text.is_empty() && !w.dynamic && w.text.chars().all(|d| d.is_ascii_digit()) {
                    w = Word::default();
                } else {
                    { settle(&mut w); finish_word(&mut w, &mut cur, &mut redirect); }
                }
                // `>`, `>>`, `>|` and `<>` (opened read-write) write; `<`,
                // `<<` and `<<<` read.
                let writes = c == '>' || chars.get(i + 1) == Some(&'>');
                i += 1;
                while i < chars.len() && matches!(chars[i], '>' | '<' | '|') {
                    i += 1;
                }
                if chars.get(i) == Some(&'&') {
                    // `>&2`: a descriptor, not a file.
                    i += 1;
                    let fd_end = chars[i..].iter().position(|x| !x.is_ascii_digit() && *x != '-').map_or(chars.len(), |e| i + e);
                    if fd_end > i {
                        i = fd_end;
                        continue;
                    }
                }
                redirect = Some(writes);
            }
            '&' if chars.get(i + 1) == Some(&'>') => {
                { settle(&mut w); finish_word(&mut w, &mut cur, &mut redirect); }
                i += 2;
                if chars.get(i) == Some(&'>') {
                    i += 1;
                }
                redirect = Some(true);
            }
            ';' | '&' | '|' | '\n' | '(' | ')' => {
                { settle(&mut w); finish_word(&mut w, &mut cur, &mut redirect); }
                finish_cmd(&mut cur, out);
                i += 1;
            }
            '#' if !w.started => {
                while i < chars.len() && chars[i] != '\n' {
                    i += 1;
                }
            }
            c if c.is_whitespace() => {
                { settle(&mut w); finish_word(&mut w, &mut cur, &mut redirect); }
                i += 1;
            }
            c => {
                // Unquoted, these are the shell's to expand: a parameter,
                // a brace expansion, a glob.
                if matches!(c, '$' | '{' | '}' | '[' | '*' | '?') {
                    w.dynamic = true;
                }
                w.text.push(c);
                w.started = true;
                i += 1;
            }
        }
    }
    { settle(&mut w); finish_word(&mut w, &mut cur, &mut redirect); }
    finish_cmd(&mut cur, out);
    // Shell grammar words are not commands: `if rm x; then …` runs `rm x`.
    for c in &mut out.commands {
        while c.words.first().is_some_and(|w| KEYWORDS.contains(&w.as_str())) {
            c.words.remove(0);
            c.dynamic.remove(0);
        }
    }
    out.commands.retain(|c| !c.words.is_empty() || c.writes);
}

const KEYWORDS: &[&str] = &["if", "then", "else", "elif", "fi", "do", "done", "while", "until", "!", "{", "}", "case", "esac", "in"];

/// The text up to the `)` that closes one opened just before `from`, and
/// the index after it; quotes inside are respected.
fn balanced(chars: &[char], from: usize) -> (String, usize) {
    let mut depth = 1;
    let mut i = from;
    let mut quote: Option<char> = None;
    while i < chars.len() {
        let c = chars[i];
        match quote {
            Some(q) if c == q => quote = None,
            Some(_) if c == '\\' => i += 1,
            Some(_) => {}
            None => match c {
                '\'' | '"' => quote = Some(c),
                '\\' => i += 1,
                '(' => depth += 1,
                ')' => {
                    depth -= 1;
                    if depth == 0 {
                        return (chars[from..i].iter().collect(), i + 1);
                    }
                }
                _ => {}
            },
        }
        i += 1;
    }
    (chars[from.min(chars.len())..].iter().collect(), chars.len())
}

/// Programs that run another program named by their arguments, with the
/// options of theirs that take a value.
const WRAPPERS: &[(&str, &[&str])] = &[
    ("sudo", &["-u", "-g", "-h", "-p", "-C", "-D", "-R", "-T", "-U"]),
    ("doas", &["-u", "-C"]),
    ("command", &[]),
    ("builtin", &[]),
    ("exec", &["-a"]),
    ("nohup", &[]),
    ("nice", &["-n"]),
    ("ionice", &["-c", "-n", "-p"]),
    ("time", &["-f", "-o"]),
    ("env", &["-u", "-C", "-S"]),
    ("xargs", &["-I", "-i", "-n", "-P", "-L", "-l", "-d", "-E", "-e", "-s", "-a", "--max-args", "--max-procs", "--delimiter", "--replace", "--arg-file"]),
    ("timeout", &["-s", "-k", "--signal", "--kill-after"]),
    ("stdbuf", &["-i", "-o", "-e"]),
    ("unbuffer", &[]),
    ("setsid", &[]),
    ("chronic", &[]),
    ("busybox", &[]),
    ("watch", &["-n", "-d"]),
];

const SHELLS: &[&str] = &["sh", "bash", "zsh", "dash", "ksh", "fish", "ash"];

fn basename(w: &str) -> &str {
    w.rsplit('/').next().unwrap_or(w)
}

/// Every form a deny rule is matched against: the command as written, then
/// with what only wraps a program stripped, and the commands a shell,
/// `eval` or `find -exec` would run from its arguments. Each word comes with
/// whether the shell still expands it. Returns true when some form's
/// program is a word the shell computes — or a nested command line is — so
/// what runs cannot be known from the text.
fn deny_forms(words: &[(String, bool)], out: &mut Vec<Vec<String>>, depth: usize) -> bool {
    if words.is_empty() {
        return false;
    }
    if depth > MAX_NESTING {
        return true;
    }
    let texts = |w: &[(String, bool)]| w.iter().map(|(t, _)| t.clone()).collect::<Vec<String>>();
    out.push(texts(words));
    let mut unknown = false;
    let mut w: Vec<(String, bool)> = words.to_vec();
    loop {
        // `FOO=bar cmd`
        while w.first().is_some_and(|(x, _)| x.contains('=') && !x.starts_with('=') && x.split('=').next().is_some_and(|n| n.chars().all(|c| c.is_ascii_alphanumeric() || c == '_'))) {
            w.remove(0);
        }
        let Some((first, dynamic)) = w.first().cloned() else { return unknown };
        if dynamic {
            return true;
        }
        let prog = basename(&first).to_string();
        if prog != first {
            w[0].0 = prog.clone();
        }
        let Some((_, takes)) = WRAPPERS.iter().find(|(n, _)| *n == prog) else { break };
        w.remove(0);
        // Its options, and a value after each that takes one.
        while let Some((o, _)) = w.first().cloned() {
            if prog == "env" && o.contains('=') {
                w.remove(0);
                continue;
            }
            if !o.starts_with('-') || o == "-" {
                break;
            }
            w.remove(0);
            if o == "--" {
                break;
            }
            if takes.contains(&o.as_str()) && !w.is_empty() {
                w.remove(0);
            }
        }
        // `timeout 5 cmd`: its duration.
        if prog == "timeout" && w.first().is_some_and(|(d, _)| d.chars().next().is_some_and(|c| c.is_ascii_digit())) {
            w.remove(0);
        }
        if prog == "nice" && w.first().is_some_and(|(d, _)| d.starts_with('-') || d.parse::<i32>().is_ok()) {
            w.remove(0);
        }
        if w.is_empty() {
            return unknown;
        }
        out.push(texts(&w));
    }
    out.push(texts(&w));
    let prog = w[0].0.clone();
    // `bash -c 'rm x'`, `eval 'rm x'`: the string is a command line.
    let nested: Option<(String, bool)> = if SHELLS.contains(&prog.as_str()) {
        w.iter().position(|(a, _)| a.starts_with('-') && !a.starts_with("--") && a.contains('c')).and_then(|i| w.get(i + 1)).cloned()
    } else if prog == "eval" {
        Some((texts(&w[1..]).join(" "), w[1..].iter().any(|(_, d)| *d)))
    } else {
        None
    };
    if let Some((line, dynamic)) = nested {
        let inner = split(&line);
        unknown |= dynamic || inner.opaque;
        for c in inner.commands {
            unknown |= deny_forms(&pairs(&c), out, depth + 1);
        }
    }
    // `find . -exec rm {} \;`
    if prog == "find" {
        let mut i = 0;
        while i < w.len() {
            if matches!(w[i].0.as_str(), "-exec" | "-execdir" | "-ok" | "-okdir") {
                let end = w[i + 1..].iter().position(|(a, _)| a == ";" || a == "+").map_or(w.len(), |e| i + 1 + e);
                unknown |= deny_forms(&w[i + 1..end], out, depth + 1);
                i = end;
            }
            i += 1;
        }
        if w.iter().any(|(a, _)| a == "-delete") {
            out.push(vec!["rm".into()]);
        }
    }
    unknown
}

/// One simple command's words against a Bash rule's specifier.
fn spec_matches(spec: Option<&str>, words: &[String]) -> bool {
    let Some(spec) = spec else { return true };
    let spec = spec.trim();
    let subject = words.join(" ");
    if let Some(prefix) = spec.strip_suffix(":*") {
        let want = split(prefix).commands.into_iter().next().map(|c| c.words).unwrap_or_default();
        return !want.is_empty() && words.len() >= want.len() && words[..want.len()] == want[..];
    }
    if spec.contains('*') {
        let norm = spec.split_whitespace().collect::<Vec<_>>().join(" ");
        return wildcard(&norm, &subject) || norm.strip_suffix(" *").is_some_and(|base| wildcard(base, &subject));
    }
    let want = split(spec).commands.into_iter().next().map(|c| c.words).unwrap_or_default();
    want == words
}

/// The simple commands an allow rule has to cover, or none when no allow
/// rule may cover the line: one krowk could not follow, a command writing a
/// file through a redirection, or a git that is told what to run on the
/// command line. A login shell wrapped around one command line — what Codex
/// asks about, `bash -lc 'git status'` — is its command line, read by the
/// same splitter.
fn allow_units(cmd: &str, depth: usize) -> Option<Vec<Simple>> {
    let parsed = split(cmd);
    if parsed.opaque || parsed.commands.is_empty() || depth > MAX_NESTING {
        return None;
    }
    let mut out = Vec::new();
    for c in parsed.commands {
        if c.writes || uncoverable(&c.words) {
            return None;
        }
        let shell = c.words.len() == 3
            && SHELLS.contains(&basename(&c.words[0]))
            && c.words[1].starts_with('-')
            && c.words[1][1..].chars().all(|f| matches!(f, 'l' | 'c'))
            && c.words[1].contains('c');
        if shell && !c.dynamic[2] {
            out.extend(allow_units(&c.words[2], depth + 1)?);
        } else {
            out.push(c);
        }
    }
    Some(out)
}

/// A command no allow rule covers, whatever it says: git handed config on
/// its command line (`-c`, `--config-env`, `--exec-path`) or setting an
/// alias, either of which can make it run any program — so `Bash(git:*)`
/// does not stretch to `git -c core.pager='rm -rf ~' log`.
fn uncoverable(words: &[String]) -> bool {
    let Some(prog) = words.first() else { return false };
    if basename(prog) != "git" {
        return false;
    }
    words[1..].iter().any(|a| a == "-c" || (a.starts_with("-c") && a.len() > 2 && !a.starts_with("--")) || a.starts_with("--config-env") || a.starts_with("--exec-path") || a.starts_with("alias.") || a.contains(".alias."))
}

/// A Bash rule against a command line: every simple command (`all`, for
/// allow and ask) or any one of them in any of its forms (deny).
fn bash_matches(spec: Option<&str>, cmd: &str, all: bool) -> bool {
    if all {
        return allow_units(cmd, 0).is_some_and(|units| units.iter().all(|c| spec_matches(spec, &c.words)));
    }
    if spec.is_none() {
        return true;
    }
    split(cmd).commands.iter().any(|c| {
        let mut forms = Vec::new();
        deny_forms(&pairs(c), &mut forms, 0);
        forms.iter().any(|f| spec_matches(spec, f))
    })
}

/// Whether every simple command of a line is covered by one of `rules`,
/// for allow rules spread over several rules (`git status && npm test`
/// under `Bash(git status)` and `Bash(npm test)`).
pub fn bash_covered(rules: &[&Rule], cmd: &str) -> bool {
    allow_units(cmd, 0).is_some_and(|units| units.iter().all(|c| rules.iter().any(|r| r.tool == "Bash" && spec_matches(r.spec.as_deref(), &c.words))))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn rule(t: &str) -> Rule {
        parse(t, "test", Path::new("/proj")).unwrap()
    }

    fn bash(t: &str, cmd: &str, all: bool) -> bool {
        matches(&rule(t), &Call { tool: "Bash".into(), access: Access::Bash(cmd.into()), subject: None }, &Places { cwd: Path::new("/proj/sub"), home: Some(Path::new("/home/me")) }, all)
    }

    #[test]
    fn r_perm_1_rules_parse_in_claude_codes_syntax() {
        let r = rule("Bash(git:*)");
        assert_eq!((r.tool.as_str(), r.spec.as_deref()), ("Bash", Some("git:*")));
        assert_eq!(rule("bash").tool, "Bash", "krowk's tool names are Claude's");
        assert_eq!(rule("Bash(*)").spec, None, "the bare tool");
        assert_eq!(rule("Mcp(github:create_issue)").tool, "mcp__github__create_issue");
        assert_eq!(rule("Mcp(github)").tool, "mcp__github");
        assert_eq!(rule("Mcp(github:*)").tool, "mcp__github");
        assert_eq!(rule("str_replace(src/**)").tool, "Edit");
        assert!(parse("Bash(git", "t", Path::new("/")).is_err() && parse("", "t", Path::new("/")).is_err() && parse("(x)", "t", Path::new("/")).is_err());
        // Every shape, as Claude Code's --disallowedTools reads it.
        for (written, claude) in [
            ("Mcp(s:t)", Some("mcp__s__t")),
            ("Mcp(s)", Some("mcp__s")),
            ("Mcp", None),
            ("Bash(git:*)", Some("Bash(git:*)")),
            ("bash", Some("Bash")),
            ("str_replace(src/**)", Some("Edit(src/**)")),
            ("Read(//etc/**)", Some("Read(//etc/**)")),
            ("Read(~/.ssh/**)", Some("Read(~/.ssh/**)")),
            ("Read(/secrets/**)", Some("Read(//proj/secrets/**)")),
            ("Read(.env)", Some("Read(.env)")),
            ("WebFetch(domain:x.io)", Some("WebFetch(domain:x.io)")),
            ("Skill(deploy)", Some("Skill(deploy)")),
            ("Publish(*.png)", None),
        ] {
            assert_eq!(claude_spelling(&rule(written)).as_deref(), claude, "{written}");
        }
    }

    #[test]
    fn r_perm_1_bash_rules_match_the_way_claude_code_documents_them() {
        assert!(bash("Bash(git:*)", "git", true) && bash("Bash(git:*)", "git status", true) && !bash("Bash(git:*)", "gitk", true));
        assert!(bash("Bash(npm run test:*)", "npm run test -- --watch", true) && !bash("Bash(npm run test:*)", "npm run build", true));
        assert!(bash("Bash(npm test)", "npm test", true) && bash("Bash(npm test)", "npm  'test'", true) && !bash("Bash(npm test)", "npm test x", true));
        assert!(bash("Bash(ls *)", "ls -la", true) && bash("Bash(ls *)", "ls", true) && !bash("Bash(ls *)", "lsof", true));
        assert!(bash("Bash(* --version)", "node --version", true));
        assert!(bash("Bash", "anything at all", true));
        // Compound lines: every command must be allowed.
        assert!(!bash("Bash(git status:*)", "git status && rm -rf /", true));
        assert!(!bash("Bash(echo:*)", "echo $(rm -rf x)", true), "a substitution is a command of its own");
        assert!(!bash("Bash(echo:*)", "echo `rm x`", true));
        assert!(!bash("Bash(echo:*)", "echo hi > ~/.bashrc", true), "a redirection that writes is never allowed by a rule");
        assert!(bash("Bash(echo:*)", "echo hi > /dev/null 2>&1", true));
        assert!(!bash("Bash(echo:*)", "echo 'unterminated", true));
        let both = [rule("Bash(git status)"), rule("Bash(npm test)")];
        assert!(bash_covered(&both.iter().collect::<Vec<_>>(), "git status && npm test"));
        assert!(!bash_covered(&both.iter().collect::<Vec<_>>(), "git status && npm publish"));
    }

    #[test]
    fn r_perm_1_a_deny_rule_sees_through_wrappers_and_nesting() {
        for cmd in [
            "rm -rf build",
            "  rm x",
            "/bin/rm x",
            "\\rm x",
            "'rm' x",
            "sudo rm x",
            "sudo -u root rm x",
            "env FOO=1 rm x",
            "FOO=1 rm x",
            "nohup rm x &",
            "timeout 5 rm x",
            "xargs -n1 rm < list",
            "ls | xargs rm",
            "git status; rm x",
            "true && rm x",
            "false || rm x",
            "echo $(rm x)",
            "echo \"$(rm x)\"",
            "echo `rm x`",
            "bash -c 'rm x'",
            "sh -lc \"cd /tmp && rm x\"",
            "eval rm x",
            "find . -name '*.o' -exec rm {} \\;",
            "find . -delete",
            "if true; then rm x; fi",
            "(cd sub && rm x)",
            "command rm x",
        ] {
            assert!(bash("Bash(rm:*)", cmd, false), "{cmd:?} slipped past Bash(rm:*)");
        }
        for cmd in ["ls", "git rm --cached x", "echo rm", "grep rm file", "npm run rmdir-thing"] {
            assert!(!bash("Bash(rm:*)", cmd, false), "{cmd:?} is not rm");
        }
    }

    #[test]
    fn r_perm_1_path_rules_resolve_like_claude_codes() {
        let at = Places { cwd: Path::new("/proj/sub"), home: Some(Path::new("/home/me")) };
        let read = |t: &str, p: &str, all: bool| matches(&rule(t), &Call { tool: "Read".into(), access: Access::Read(vec![PathBuf::from(p)]), subject: None }, &at, all);
        let edit = |t: &str, p: &str| matches(&rule(t), &Call { tool: "Write".into(), access: Access::Edit(vec![PathBuf::from(p)]), subject: None }, &at, true);
        assert!(read("Read(//etc/**)", "/etc/hosts", true) && !read("Read(//etc/**)", "/proj/etc/x", true));
        assert!(read("Read(~/.ssh/**)", "/home/me/.ssh/id_rsa", true));
        assert!(read("Read(/docs/**)", "/proj/docs/a.md", true), "/ is the project root");
        assert!(read("Read(./src/**)", "/proj/sub/src/a.rs", true) && read("Read(src/**)", "/proj/sub/src/a.rs", true), "relative is the working directory");
        assert!(read("Read(.env)", "/proj/sub/deep/.env", true), "a bare name at any depth");
        assert!(!read("Read(.env)", "/elsewhere/.env", true), "an allow rule's bare name stays under the working directory");
        assert!(read("Read(.env)", "/elsewhere/.env", false) && read("Read(.env)", "/proj/sub/.ENV", false), "a deny rule's bare name holds anywhere, in any case");
        assert!(read("Read(secrets/)", "/proj/sub/secrets/a/b", true), "a directory is everything under it");
        assert!(edit("Edit(src/**)", "/proj/sub/src/x.rs") && edit("Write(src/**)", "/proj/sub/src/x.rs"), "Edit rules cover every tool that writes");
        assert!(!read("Edit(src/**)", "/proj/sub/src/x.rs", true), "and not reads");
        assert!(read("Read", "/anything", true));
        let grep = matches(&rule("Read(//etc/**)"), &Call { tool: "Grep".into(), access: Access::Read(vec!["/etc".into(), "/etc/hosts".into()]), subject: None }, &at, false);
        assert!(grep, "Read rules cover Grep");
    }

    #[test]
    fn r_perm_1_webfetch_and_mcp_rules() {
        let at = Places { cwd: Path::new("/"), home: None };
        let fetch = |t: &str, u: &str| matches(&rule(t), &Call { tool: "WebFetch".into(), access: Access::Fetch(u.into()), subject: None }, &at, true);
        assert!(fetch("WebFetch(domain:example.com)", "https://example.com/a") && !fetch("WebFetch(domain:example.com)", "https://evil.example.com.attacker.io/"));
        assert!(fetch("WebFetch(domain:*.example.com)", "https://docs.example.com/") && !fetch("WebFetch(domain:example.com)", "https://docs.example.com/"));
        let mcp = |t: &str, s: &str, tool: &str| matches(&rule(t), &Call { tool: format!("mcp__{s}__{tool}"), access: Access::Mcp { server: s.into(), tool: tool.into() }, subject: None }, &at, true);
        assert!(mcp("Mcp(github:create_issue)", "github", "create_issue") && !mcp("Mcp(github:create_issue)", "github", "delete_repo"));
        assert!(mcp("mcp__github", "github", "x") && mcp("mcp__github__*", "github", "x") && mcp("Mcp(github)", "github", "x") && !mcp("mcp__github", "gitlab", "x"));
    }

    fn units(cmd: &str) -> Vec<Vec<String>> {
        split(cmd).commands.into_iter().map(|c| c.words).collect()
    }

    #[test]
    fn r_perm_1_ansi_c_and_locale_quoting_split_as_bash_splits_them() {
        // `\'` inside $'…' does not end it: bash reads one word, then `;`,
        // then a second command. A splitter that ended the string early
        // would hide the `;` and the rm behind it.
        for cmd in [r"echo $'it\'s'; rm -rf x", r#"echo $"it's"; rm -rf x"#, r"echo $'\x41\101\u0041'; rm -rf x"] {
            let u = units(cmd);
            assert_eq!(u.len(), 2, "{cmd}: {u:?}");
            assert_eq!(u[1], ["rm", "-rf", "x"], "{cmd}");
            assert!(!bash("Bash(echo:*)", cmd, true), "{cmd}: an allow on the first program does not cover the second");
            assert!(bash("Bash(rm:*)", cmd, false), "{cmd}: a deny on the second matches");
        }
        assert_eq!(units(r"echo $'\x41\101\u0041'")[0][1], "AAA", "escapes decoded as bash decodes them");
        assert_eq!(units(r"$'\x72m' -rf x")[0][0], "rm", "a name spelled in escapes is still the name");
        assert!(bash("Bash(rm:*)", r"$'\x72m' -rf x", false));
        // An escape krowk does not model, or a string that never closes: opaque.
        assert!(split(r"echo $'\cA'; rm x").opaque && split(r"echo $'open").opaque);
    }

    #[test]
    fn r_perm_1_a_program_name_the_shell_computes_makes_the_line_opaque() {
        for cmd in ["$(printf rm) -rf x", "`echo rm` x", "$CMD x", "r$IFS'm' x", "${X}rm x", "r{m,} x", "/bin/r? x", "/bin/r[m] x", "/bin/r* x", "sudo $X y", "bash -c \"$CMD\"", "eval $CMD", "env FOO=1 $X", "find . -exec $X {} \\;"] {
            assert!(split(cmd).opaque, "{cmd:?} should be opaque");
            assert!(!bash("Bash", cmd, true), "{cmd:?}: no allow rule covers what krowk cannot read");
        }
        for cmd in ["ls *.rs", "echo $HOME", "rm -- \"$f\"", "[ -f x ] && echo y", "if [[ -n $x ]]; then ls; fi", "find . -exec rm {} \\;", "{ ls; }"] {
            assert!(!split(cmd).opaque, "{cmd:?}: expansions in arguments, and test brackets, are fine");
        }
    }

    #[test]
    fn r_perm_1_a_read_write_redirection_writes() {
        for cmd in ["cat <> f", "echo x 1<>f", "echo x > f", "echo x >> f", "echo x &> f", "echo x >| f"] {
            assert!(split(cmd).commands[0].writes, "{cmd}");
            assert!(!bash("Bash(echo:*)", cmd, true) && !bash("Bash(cat:*)", cmd, true), "{cmd}");
        }
        for cmd in ["cat < f", "cat <<EOF", "grep x <<< y", "echo x > /dev/null 2>&1"] {
            assert!(!split(cmd).commands[0].writes, "{cmd}");
        }
    }

    #[test]
    fn r_perm_1_git_told_what_to_run_is_never_covered_by_an_allow_rule() {
        for cmd in ["git -c core.pager='rm -rf ~' log", "git -ccore.sshCommand=x fetch", "git --config-env=core.editor=E commit", "git --exec-path=/tmp/x status", "git config alias.x '!rm -rf .'", "git config --global alias.st status"] {
            assert!(!bash("Bash(git:*)", cmd, true) && !bash("Bash", cmd, true), "{cmd}");
        }
        assert!(bash("Bash(git:*)", "git log --oneline -5", true));
    }

    #[test]
    fn r_perm_1_a_login_shell_around_one_command_line_is_that_line() {
        // What Codex asks about: its command, wrapped in a login shell.
        assert!(bash("Bash(git status)", "/bin/bash -lc 'git status'", true));
        assert!(bash("Bash(git:*)", "bash -lc 'git log && git status'", true));
        assert!(!bash("Bash(git:*)", "bash -lc 'git status; rm -rf x'", true), "every command inside it");
        assert!(!bash("Bash(git status)", "bash -lc \"$CMD\"", true), "not one the shell computes");
        assert!(!bash("Bash(git status)", "bash -lc 'git status' extra", true), "only exactly a shell, a flag and a line");
        assert!(bash("Bash(rm:*)", "bash -lc 'rm -rf x'", false));
    }

    #[test]
    fn r_perm_1_path_rules_anchor_at_directories_with_brackets_and_fold_case_for_deny() {
        let odd = Path::new("/w/a[1]{x}");
        let at = Places { cwd: odd, home: Some(Path::new("/h/[me]")) };
        let r = |t: &str| parse(t, "test", odd).unwrap();
        let hit = |t: &str, p: &str, all: bool| matches(&r(t), &Call { tool: "Read".into(), access: Access::Read(vec![PathBuf::from(p)]), subject: None }, &at, all);
        assert!(hit("Read(/secrets/**)", "/w/a[1]{x}/secrets/k", true), "the root is a directory, not a pattern");
        assert!(!hit("Read(/secrets/**)", "/w/a1x/secrets/k", true));
        assert!(hit("Read(./src/**)", "/w/a[1]{x}/src/a.rs", true));
        assert!(hit("Read(~/.ssh/**)", "/h/[me]/.ssh/id", true));
        // A deny rule's pattern is folded too, not only the path.
        assert!(hit("Read(/Secrets/**)", "/w/a[1]{x}/SECRETS/k", false) && hit("Read(/Secrets/**)", "/w/A[1]{X}/secrets/k", false));
        assert!(!hit("Read(/Secrets/**)", "/w/a[1]{x}/secrets/k", true), "an allow rule is not folded");
        // A pattern that does not compile fails the load, never a dead deny.
        assert!(parse("Read(/src/{a,b)", "f", odd).unwrap_err().contains("does not compile"));
        assert!(parse("Read(src/[)", "f", odd).is_ok() || parse("Read(src/[)", "f", odd).unwrap_err().contains("does not compile"));
    }

}
