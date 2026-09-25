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
//! two. An allow rule allows a command line only when it allows every one
//! of them, and never one that writes a file through a redirection. A deny
//! rule denies the line when it matches any one — as written, and again
//! with what only wraps a program stripped away (`sudo`, `env`, `xargs`,
//! `nohup`, `command`, `timeout`, a leading `VAR=value`, a path such as
//! `/bin/rm`), inside `bash -c '…'`, `sh -c`, `eval` and `find -exec`. That
//! is a best effort, not a sandbox: a command can always compute another
//! (`$(printf rm) x`), which is why the OS sandbox (Harness ticket 26) is
//! the boundary and a deny rule the policy.

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
    Ok(Rule { tool, spec, text: t.to_string(), source: source.to_string(), root: root.to_path_buf() })
}

/// The rule in Claude Code's own spelling, for `--disallowedTools`.
pub fn claude_spelling(r: &Rule) -> String {
    match &r.spec {
        Some(s) => format!("{}({s})", r.tool),
        None => r.tool.clone(),
    }
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

/// A path rule's pattern, resolved to an absolute glob.
struct PathPattern {
    /// The glob over absolute paths; none when it names files by name only.
    glob: Option<crate::tools::search::Glob>,
    /// A bare name (no `/`), matched against the file's name: anywhere for
    /// a deny rule, under the working directory for the others.
    name: Option<crate::tools::search::Glob>,
    cwd: PathBuf,
}

impl PathPattern {
    fn new(spec: &str, root: &Path, at: &Places<'_>) -> PathPattern {
        let spec = spec.trim();
        // A directory means everything under it, as in .gitignore.
        let spec = if spec.ends_with('/') { format!("{spec}**") } else { spec.to_string() };
        let join = |base: &Path, rest: &str| format!("{}/{}", base.display().to_string().trim_end_matches('/'), rest.trim_start_matches('/'));
        let (abs, name) = if let Some(rest) = spec.strip_prefix("//") {
            (Some(format!("/{rest}")), None)
        } else if let Some(rest) = spec.strip_prefix("~/") {
            (at.home.map(|h| join(h, rest)), None)
        } else if spec.starts_with('/') {
            (Some(join(root, &spec)), None)
        } else {
            let rest = spec.trim_start_matches("./");
            if rest.contains('/') { (Some(join(at.cwd, rest)), None) } else { (None, Some(rest.to_string())) }
        };
        let glob = abs.and_then(|a| crate::tools::search::Glob::new(&a).ok());
        let name = name.and_then(|n| crate::tools::search::Glob::new(&n).ok());
        PathPattern { glob, name, cwd: at.cwd.to_path_buf() }
    }

    /// Judged as the path is spelled and as it really leads, and — for a
    /// deny rule, `anywhere` — case-folded too, since macOS and Windows
    /// open `.ENV` as `.env`.
    fn matches(&self, p: &Path, anywhere: bool) -> bool {
        let real = crate::tools::real_path(p, 0).unwrap_or_else(|_| p.to_path_buf());
        let mut forms = vec![p.to_path_buf(), real];
        if anywhere {
            let folded: Vec<PathBuf> = forms.iter().map(|f| PathBuf::from(f.to_string_lossy().to_lowercase())).collect();
            forms.extend(folded);
        }
        let cwd_real = self.cwd.canonicalize().unwrap_or_else(|_| self.cwd.clone());
        forms.iter().any(|f| {
            if self.glob.as_ref().is_some_and(|g| g.matches(f)) {
                return true;
            }
            let Some(n) = &self.name else { return false };
            // A bare name is matched below the working directory, as a
            // .gitignore's is below its own; a deny rule's anywhere else too.
            let under = f.strip_prefix(&self.cwd).or_else(|_| f.strip_prefix(&cwd_real)).ok();
            match under {
                Some(rel) => rel.components().any(|c| n.matches(Path::new(c.as_os_str()))),
                None => anywhere && f.components().any(|c| n.matches(Path::new(c.as_os_str()))),
            }
        })
    }
}

/// One simple command: its words with quoting taken off, and whether it
/// writes a file through a redirection.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Simple {
    pub words: Vec<String>,
    pub writes: bool,
}

/// A command line, split the way the shell would run it: every simple
/// command, the ones inside substitutions included. `opaque` when the
/// parse could not follow it (a quote that never closes).
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Parsed {
    pub commands: Vec<Simple>,
    pub opaque: bool,
}

/// Splits a command line into simple commands.
pub fn split(cmd: &str) -> Parsed {
    let mut out = Parsed::default();
    lex(cmd, &mut out, 0);
    out
}

/// Substitutions nested deeper than this are not followed; the line is
/// opaque instead.
const MAX_NESTING: usize = 8;

fn lex(cmd: &str, out: &mut Parsed, depth: usize) {
    if depth > MAX_NESTING {
        out.opaque = true;
        return;
    }
    let chars: Vec<char> = cmd.chars().collect();
    let mut cur = Simple::default();
    let mut word = String::new();
    let mut in_word = false;
    // The next word is a redirection's target, and whether it writes.
    let mut redirect: Option<bool> = None;
    let mut i = 0;
    let finish_word = |word: &mut String, in_word: &mut bool, cur: &mut Simple, redirect: &mut Option<bool>| {
        if *in_word {
            match redirect.take() {
                Some(writes) => {
                    if writes && word != "/dev/null" && !word.starts_with('&') {
                        cur.writes = true;
                    }
                }
                None => cur.words.push(std::mem::take(word)),
            }
            word.clear();
            *in_word = false;
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
                    word.push(*n);
                }
                in_word = true;
                i += 2;
            }
            '\'' => {
                in_word = true;
                match chars[i + 1..].iter().position(|x| *x == '\'') {
                    Some(end) => {
                        word.extend(&chars[i + 1..i + 1 + end]);
                        i += end + 2;
                    }
                    None => {
                        out.opaque = true;
                        word.extend(&chars[i + 1..]);
                        i = chars.len();
                    }
                }
            }
            '"' => {
                in_word = true;
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
                            word.push(chars[i + 1]);
                            i += 2;
                        }
                        '$' if chars.get(i + 1) == Some(&'(') => {
                            let (inner, next) = balanced(&chars, i + 2);
                            lex(&inner, out, depth + 1);
                            word.push_str("$(…)");
                            i = next;
                        }
                        '`' => {
                            let end = chars[i + 1..].iter().position(|x| *x == '`').map(|e| i + 1 + e);
                            let inner: String = chars[i + 1..end.unwrap_or(chars.len())].iter().collect();
                            lex(&inner, out, depth + 1);
                            word.push_str("`…`");
                            i = end.map_or(chars.len(), |e| e + 1);
                        }
                        ch => {
                            word.push(ch);
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
                word.push_str("$(…)");
                in_word = true;
                i = next;
            }
            '`' => {
                let end = chars[i + 1..].iter().position(|x| *x == '`').map(|e| i + 1 + e);
                let inner: String = chars[i + 1..end.unwrap_or(chars.len())].iter().collect();
                if end.is_none() {
                    out.opaque = true;
                }
                lex(&inner, out, depth + 1);
                word.push_str("`…`");
                in_word = true;
                i = end.map_or(chars.len(), |e| e + 1);
            }
            '<' | '>' if chars.get(i + 1) == Some(&'(') => {
                let (inner, next) = balanced(&chars, i + 2);
                lex(&inner, out, depth + 1);
                word.push_str("<(…)");
                in_word = true;
                i = next;
            }
            '>' | '<' => {
                // A file descriptor written straight before it (`2>`) is part
                // of the redirection, not a word.
                if in_word && !word.is_empty() && word.chars().all(|d| d.is_ascii_digit()) {
                    word.clear();
                    in_word = false;
                } else {
                    finish_word(&mut word, &mut in_word, &mut cur, &mut redirect);
                }
                let writes = c == '>';
                i += 1;
                // `>>`, `>|`, `>&`, `<<`, `<<<`, `<>`, `&>` spellings.
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
                finish_word(&mut word, &mut in_word, &mut cur, &mut redirect);
                i += 2;
                if chars.get(i) == Some(&'>') {
                    i += 1;
                }
                redirect = Some(true);
            }
            ';' | '&' | '|' | '\n' | '(' | ')' => {
                finish_word(&mut word, &mut in_word, &mut cur, &mut redirect);
                finish_cmd(&mut cur, out);
                i += 1;
            }
            '#' if !in_word => {
                while i < chars.len() && chars[i] != '\n' {
                    i += 1;
                }
            }
            c if c.is_whitespace() => {
                finish_word(&mut word, &mut in_word, &mut cur, &mut redirect);
                i += 1;
            }
            c => {
                word.push(c);
                in_word = true;
                i += 1;
            }
        }
    }
    finish_word(&mut word, &mut in_word, &mut cur, &mut redirect);
    finish_cmd(&mut cur, out);
    // Shell grammar words are not commands: `if rm x; then …` runs `rm x`.
    for c in &mut out.commands {
        while c.words.first().is_some_and(|w| KEYWORDS.contains(&w.as_str())) {
            c.words.remove(0);
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
/// `eval` or `find -exec` would run from its arguments.
fn deny_forms(words: &[String], out: &mut Vec<Vec<String>>, depth: usize) {
    if words.is_empty() || depth > MAX_NESTING {
        return;
    }
    out.push(words.to_vec());
    let mut w: Vec<String> = words.to_vec();
    loop {
        // `FOO=bar cmd`
        while w.first().is_some_and(|x| x.contains('=') && !x.starts_with('=') && x.split('=').next().is_some_and(|n| n.chars().all(|c| c.is_ascii_alphanumeric() || c == '_'))) {
            w.remove(0);
        }
        let Some(first) = w.first().cloned() else { return };
        let prog = basename(&first).to_string();
        if prog != first {
            w[0] = prog.clone();
        }
        let Some((_, takes)) = WRAPPERS.iter().find(|(n, _)| *n == prog) else { break };
        w.remove(0);
        // Its options, and a value after each that takes one.
        while let Some(o) = w.first().cloned() {
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
        if prog == "timeout" && w.first().is_some_and(|d| d.chars().next().is_some_and(|c| c.is_ascii_digit())) {
            w.remove(0);
        }
        if prog == "nice" && w.first().is_some_and(|d| d.starts_with('-') || d.parse::<i32>().is_ok()) {
            w.remove(0);
        }
        if w.is_empty() {
            return;
        }
        out.push(w.clone());
    }
    out.push(w.clone());
    let prog = w[0].as_str();
    // `bash -c 'rm x'`, `eval 'rm x'`: the string is a command line.
    let nested = if SHELLS.contains(&prog) {
        w.iter().position(|a| a.starts_with('-') && !a.starts_with("--") && a.contains('c')).and_then(|i| w.get(i + 1)).cloned()
    } else if prog == "eval" {
        Some(w[1..].join(" "))
    } else {
        None
    };
    if let Some(line) = nested {
        for c in split(&line).commands {
            deny_forms(&c.words, out, depth + 1);
        }
    }
    // `find . -exec rm {} \;`
    if prog == "find" {
        let mut i = 0;
        while i < w.len() {
            if matches!(w[i].as_str(), "-exec" | "-execdir" | "-ok" | "-okdir") {
                let end = w[i + 1..].iter().position(|a| a == ";" || a == "+").map_or(w.len(), |e| i + 1 + e);
                deny_forms(&w[i + 1..end], out, depth + 1);
                i = end;
            }
            i += 1;
        }
        if w.iter().any(|a| a == "-delete") {
            out.push(vec!["rm".into()]);
        }
    }
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

/// A Bash rule against a command line: every simple command (`all`, for
/// allow and ask) or any one of them in any of its forms (deny).
fn bash_matches(spec: Option<&str>, cmd: &str, all: bool) -> bool {
    let parsed = split(cmd);
    if all {
        // A line krowk could not follow is never allowed by a rule, nor is a
        // command that writes a file through a redirection.
        if parsed.opaque || parsed.commands.is_empty() {
            return spec.is_none() && !parsed.commands.is_empty() && !parsed.opaque;
        }
        return parsed.commands.iter().all(|c| !c.writes && spec_matches(spec, &c.words));
    }
    if spec.is_none() {
        return true;
    }
    parsed.commands.iter().any(|c| {
        let mut forms = Vec::new();
        deny_forms(&c.words, &mut forms, 0);
        forms.iter().any(|f| spec_matches(spec, f))
    })
}

/// Whether one simple command of a line is covered by one of `rules`, for
/// allow rules spread over several rules (`git status && npm test` under
/// `Bash(git status)` and `Bash(npm test)`).
pub fn bash_covered(rules: &[&Rule], cmd: &str) -> bool {
    let parsed = split(cmd);
    !parsed.opaque && !parsed.commands.is_empty() && parsed.commands.iter().all(|c| !c.writes && rules.iter().any(|r| r.tool == "Bash" && spec_matches(r.spec.as_deref(), &c.words)))
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
        assert_eq!(claude_spelling(&rule("Mcp(s:t)")), "mcp__s__t");
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
}
