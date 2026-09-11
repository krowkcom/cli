# Changelog

What changed in each release of the krowk CLI, for the people upgrading rather
than for the people who wrote it. Newest first.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
the versions are the `v*` tags a release is cut from. Entries land under
`[Unreleased]` as the work merges, and move under a version when it is tagged.

## [Unreleased]

### Added

- `krowk sessions import --from <claude|cursor|opencode|all>`, which reads the
  agent transcripts on this machine into the local store at
  `~/.local/share/krowk/krowk.db`. `--dry-run` discovers and counts without
  writing a row, and `--limit N` caps how many transcripts each source reads
  (0, the default, is all). Each ref's watermark is written to `import_state`
  with the rows it describes, so a second run inserts nothing: re-running the
  command is the intended way to keep the store current, not a thing to be
  careful about. One import at a time per store, enforced by an exclusive
  `flock` on `import.lock` beside the database, taken before the store is
  opened — a second one fails at once, naming that file, with exit 6, rather
  than meeting the first inside SQLite and surfacing `database is locked`,
  which tells a caller nothing. A collision with something that is not a
  krowk import — anything else writing the database — is re-worded the
  same way, naming the store and saying it is busy — and only for
  failures that came out of krowk's own store, since opencode reads a SQLite
  database of its own and its lock message is about that file. A `--dry-run` takes no lock
  and does not open the store at all: discovery only needs the environment,
  so counting what would be imported no longer creates `krowk.db`. A single
  transcript that will not read is counted in `files_failed` and listed in
  `errors` while the rest of the import continues and the exit stays 0; a
  source whose *discovery* fails, or which discovered transcripts and lost
  every one of them, is a broken import rather than a lossy one, so the other
  sources still run and the command exits 1 at the end. `--json` answers with
  a row per source — files, `sessions_seen`, `messages_seen`, `parts_seen`,
  the matching `*_inserted` counts, `skipped_by_type`, `skipped_lines`,
  `errors_truncated` and `duration_ms` — and the human output is one line
  each. The `*_seen` numbers are what that run read, not what the store
  holds, and they are not comparable across sources: claude and opencode
  re-read a whole transcript every run while cursor resumes from its byte
  offset and reads only what was appended. `*_inserted` is the number that
  means the same thing everywhere. `errors` stays capped at ten reasons,
  with `errors_truncated` counting the ones left out, each reason bounded to
  512 bytes; `skipped_by_type` names at most 32 raw types and sums the rest —
  along with any name over 64 bytes, which is summed rather than cut so two
  long names cannot collide into one — under `krowk:other`, a bucket named
  with a colon because no agent's raw type carries one and which sits beside
  the 32 rather than being one of them. The names are chosen in sorted order,
  so two runs over the same machine name the same types. `--from` and
  `--dry-run` are refused on every other command by name, rather than being
  accepted and ignored by a shared flag set. All three are strings a transcript
  supplied, and a report is not a place to pass those through at whatever
  length they arrived. The lock file itself is opened `O_NOFOLLOW` and
  refused if it is not a regular file, so a symlink or a fifo at that path
  is not something krowk locks and runs on. On
  Windows it exits non-zero with `sessions is not supported on Windows in v1`
  before it resolves a store path, so nothing is created on a machine the
  command does not run on; with no home in the environment it fails closed on
  `store.Open`'s own hint rather than writing a database into the working
   directory.
- `krowk sessions` and `krowk sessions show <id>`, which read the imported
  threads back. Bare `krowk sessions` lists every thread newest-first from
  session columns only — title, harness, model, turn count, priced cost and
  recency — with `--harness`, `--worktree`, `--limit N` (default 50) and
  `--all`, and never touches message or part blobs, so the default page
  stays instant on a 10k-session store. On a terminal it offers a picker
  that prints `krowk sessions show <id>`; piped, `--json`, `--quiet` or in
  CI the table (or envelope) is the answer and no picker appears. `show`
  takes a full id, an unambiguous id prefix of at least 8 chars, or a
  foreign session id via the binding, then renders turns, messages and
  parts in seq order with tool results labelled by their twin call's name
  (`unknown tool` when the call is missing) and thinking collapsed to one
  line unless `--thinking`. Costs are priced at display time from the
  embedded models.dev snapshot and footnote its date; an unpriced pair shows `—`,
  never 0. An untitled thread lists by the first 80 chars of its first
  user text, stored at import; threads imported before that fallback stay
  untitled until a re-import carries user text.
- The Cursor importer, `internal/importer/cursor`, which reads Cursor's
  agent transcripts out of `~/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl`
  and produces the canonical `store.Thread`. The thing it is careful about
  is that Cursor lines carry `{role, message.content[]}` only — no message
  id, no timestamp, no session id, no `cwd`, no model — so the hard cases are
   position-keyed dedup and a worktree with no `cwd`. Messages keep
   `foreign_id` NULL so the store appends them unconditionally — the
   byte-offset `JSONLCursor`, which `Read` honors (delta reads return only
   new lines) and the caller must hold and never re-send, is the only dedup —
     unlike the Claude and opencode readers that re-read whole sessions; a
  re-import with the stored cursor inserts nothing, and appending one line
  inserts exactly one message and extends the cumulative turn list by one
  (turns are always cumulative over the whole file, messages stay delta). The worktree decodes the slug
  (`home-elvinas-Repositories-krowk-cli` → `/home/elvinas/Repositories/krowk-cli`)
  only when that directory exists on disk, walking up for `.git` as the
  Claude reader does; otherwise the session files under `cursor:<slug>` with
  `vcs` of `none` and a counted `worktree-fallback`. `repo.json` beside
  `agent-transcripts` contributes one `cursor_repo` session event carrying
  the repo id, never a path. `text` blocks land as `text`, `tool_use` as
   `tool_call` (with `cursor:<line>` as the call id when the block names
   none, which is every block observed — a second id-less `tool_use` on the
   same line takes `cursor:<line>:<k>`), `tool_result` as `tool_result`, and
  `turn_ended` lines are classified furniture; embedded `<timestamp>` tags
  stay in the user text, unparsed. The binding stays on `cursor` with an
  empty resume command (unknown in v1). The fixture is redacted, with a
  golden regenerated by `-update`.
- The opencode importer, `internal/importer/opencode`, which reads opencode's
  sessions out of the single SQLite database at
  `~/.local/share/opencode/opencode.db` and produces the canonical
  `store.Thread`. The thing it is careful about is the database staying
  read-only: opencode holds it open in WAL mode while it runs, so every open
  is one connection through `file:<path>?mode=ro` with no pragma ever issued,
  and a test hashes the file before and after a `Discover` plus a `Read` and
  fails on any change or any `-wal`/`-shm` sidecar. `Discover` returns one ref
  per session row (sorted, keyed `opencode:<session_id>`) so each session
  carries its own `SQLiteCursor` watermark; an absent database is an empty
  list, not an error. `Read` re-reads the whole session every time and says
  so, for the same reason the Claude reader does — turns are cumulative
  positional lists costed over a span, and the store's `foreign_id` dedup
  makes the re-read free — with the cursor as the largest message
  `time_updated` seen. A finished tool row becomes the canonical twin on the
  same message (`tool_call` plus `tool_result` sharing the call id, built by
  the contract's constructors), while a tool still running stays a lone call;
  `reasoning` lands as `thinking`, `step-start`/`step-finish` as `step`, and
  anything unrecognised as counted `unknown` rather than dropped. Turn costs
  sum the message token columns with dollars rounded once per turn into
  micros, a child session binds its `parent_id` as `Thread.Parent`, and the
  worktree comes from the project row's `worktree`/`vcs` rather than being
  guessed. The fixture is checked in as SQL that builds a temp database,
  never as a binary `.db`.
- The Claude importer, `internal/importer/claude`, which reads Claude
  Code's JSONL transcripts out of `~/.claude/projects` and produces the
  canonical `store.Thread`. The thing it is careful about is not losing
  anything: on a real machine a third of the lines are `attachment` and
  another slice is `mode`, `last-prompt`, `queue-operation`, `atis-latch`,
  `pr-link`, `permission-mode`, `cost-state`, `file-history-snapshot`,
  `file-history-delta`, `ai-title`, `frame-link`, `continued-in` and
  `agent-name`, so a reader that matched `user` and `assistant` would drop
  most of the file and report a clean import. Every line lands in exactly
  one of four places — a message, a session event, `Result.Classified` or
  `Result.Skipped` — including a line type this build has never met, which
  is classified under its own name rather than ignored, and a test adds the
  four up against the fixture's line count. An `attachment` becomes a
  `session_event` only when it carries a hook event; the rest are context
  Claude injected, and importing them as user messages would put words in a
  person's mouth and double the turn count. The worktree comes from the
  line's `cwd` and never from the directory slug, because
  `-home-elvinas--buzz` is not invertible: the checkout is the git toplevel
  found by walking up from `cwd`, and a session run outside version control
  gets its own directory with `vcs` of `none`. A subagent transcript under
  `<session>/subagents/agent-*.jsonl` becomes a session of its own, bound
  on its agent id and pointed at the conversation that dispatched it, and
  `Discover` returns a parent immediately before its children so a caller
  ingesting in order never leaves the link unset. `session.provider` is
  `anthropic` and `session.harness` is `claude`, while the binding stays on
  `claude` because that half of the key is already persisted. `Read` reads
  from the top of the file every time and says so: turns are cumulative
  positional lists whose costs are summed over a whole span, so a read
  resumed from the middle could neither number them nor cost them, and the
  store's `foreign_id` dedup makes the re-read free — and the store now
  refreshes the last stored turn, so a session imported while it was still
  being used converges on its real costs rather than keeping the partial
  ones. A message line missing its `uuid` gets a synthesised foreign id so
  it dedups like any other, and a `user` line whose content is null
  produces no parts and so opens no turn. A user-role line that no person
  typed does not open one either: an agent reporting back to the
  conversation that dispatched it arrives with the user role and real prose
  in it, so a line whose `origin.kind` is anything but `human`, whose
  `promptSource` is `system`, or whose text opens with
  `<local-command-stdout>`, `<bash-stdout>`, `<task-notification>` or
  `<system-reminder>` is treated as meta — which is worth a fifth of the
  turn count on a machine that dispatches subagents, and the same factor on
  every per-turn cost. `promptSource: "sdk"` and `<command-name>` are
  deliberately not on that list: both are a person, reached through
  something other than the terminal. Turns carry no `turn_id` link
  back to their messages, because the contract carries none: a turn is the
  costing unit and nothing else yet.
- `store.Thread.Parent`, a binding naming the session a session was spawned
  from. On ingest it is resolved through `session_binding` and written to
  `session.parent_id`, but only when the parent is already in the store and
  only when the column is still NULL — a child imported before its parent
  keeps a NULL and a later ingest of the same child fills it in, and a
  parent already recorded is never silently repointed.

- The importer contract, so three importers cannot become three exporters:
  `internal/importer` fixes the vocabulary the Claude, cursor and opencode
  readers all have to speak. A `Source` is `Name`, `Discover` and `Read`,
  producing a `store.Thread` plus the watermark to resume from; the
  watermark is a byte offset into a named file (`JSONLCursor{offset, size}`)
  or a row update time (`SQLiteCursor{time_updated}`), serialised as JSON
  into `import_state.cursor` under `"<provider>:<ref>"`, never a
  modification time — a transcript that got shorter is a rescan, not an
  append, and the watermark says plainly that it assumes an append-only
  file and leans on the store's `foreign_id` dedup for the rest. The
  `part.type` set is closed here at `text`, `thinking`,
  `tool_call`, `tool_result`, `image`, `file`, `patch`, `step` and
  `unknown`, with `tool_call` and `tool_result` data shapes fixed and paired
  by `tool_call_id`, and a block this build does not recognise landing as
  `unknown` carrying its raw payload and counted rather than dropped.
  Reading a transcript goes through `OpenHome`, which resolves under the
  home directory, refuses a path that leaves it, refuses a symlink whose
  target is outside it — including one whose target does not exist yet,
  which would otherwise be approved on the strength of a file nobody has
  created — and refuses a file over 64 MiB or anything that is not a
  regular file. That is the same "home is trusted, a checkout is not" rule
  `internal/harness` applies to configs, and "a checkout" means outside
  home: a symlink into a repository that itself lives under home is
  followed, because home is trusted in full and a path is untrusted for
  where it is rather than for what happens to be checked out there.
  `ReadJSONL` resumes from a cursor, restarts from the top when the file
  has been rewritten shorter, rewinds to the last complete line when an
  offset landed mid-line, and leaves a half-flushed trailing line for the
  next read. A line it cannot use — unparseable, past the 16 MiB line cap,
  or rejected by the source reading it — is counted in
  `Result.SkippedCount` and the read carries on, because stopping would pin
  the cursor to that line and one bad line would cost the rest of the file
  on every attempt from then on. The first hundred of those lines are also
  described individually in `Result.Skipped`, with the line number and byte
  offset; the count stays exact past that, so a file that is nothing but
  junk is reported as a number rather than accumulating a record per line
  until the importer runs out of memory. Only three things stop a read: an I/O
  error, an unterminated final line that is already past the line cap, and
  a source explicitly returning `ErrAbortFile` for a failure that was not
  the line's fault. The turn rule is one shared function: a turn opens at a
  user message that a person actually sent, so hook output, attachments and
  `tool_result`-only user lines no longer report ten turns where somebody
  asked one question.
  `Discover` returns `ErrUnsupportedOS` on Windows rather than reporting an
  empty machine, and a `Source` handed a cursor of the wrong kind returns
  `ErrCursorType` rather than silently rescanning from the top. Nothing is
  user-visible yet; no command imports anything.

- The canonical thread model plus the store `Writer`: `internal/store` now
  defines the one shape every importer produces — a `Thread` carrying its
  `Worktree`, `Session` display fields, `Binding` identity and the
  transcript in order (`Turn`, `Message` with `Part`s, `Event`) — with
  `provider`/`harness`/`type` as open strings and `role` as the closed
  `user|assistant|system|tool|error` enum the schema CHECK pins.
  `Writer.Ingest` writes a thread idempotently: the worktree is upserted by
  path, the session is found by `(provider, foreign_session_id)` (two
  threads naming one key converge on one session and one binding, even in a
  lost create race), messages carrying a known `foreign_id` are skipped
  with their parts and the rest appended with `seq` after the current max,
  while turns and events are cumulative positional lists (position `i` is
  `seq` `i`, so a re-sent prefix is skipped — except for the last stored
  turn, whose status and costs are refreshed from the re-sent list,
  because an importer that read a live transcript caught that turn in
  flight and the fraction of its cost it saw would otherwise stand
  forever; every earlier turn is settled, since a turn closes only when
  the next one opens), and a session found by its binding has its
  `worktree_id` re-pointed as well as its display fields refreshed, so a
  session first filed under a placeholder directory moves once a later
  import works out where it really ran. Message batches commit 500
  per transaction, so a concurrent push waits on `busy_timeout` instead of
  meeting a lock held for a whole import. Every minted id passes
  `ValidateID`, every `time_*` comes from the injected clock, and the
  `Writer` never reads the environment. Nothing is user-visible yet; no
  command writes these tables.

- Model prices from models.dev: a new `internal/pricing` package answers
  per-(provider, model) USD-per-1M-token rates from an embedded snapshot
  (796 bytes, regenerated by `go generate ./internal/pricing` from the
  committed fixture, never the live network) with a
  `$XDG_CACHE_HOME/krowk/models.json` cache winning when present and
  parseable. `krowk pricing refresh` refreshes that cache with a conditional
  GET (5 s timeout, silent and non-fatal on failure); the lookup itself never
  touches the network, keys (provider, model) so provider-specific prices
  stay apart, and derives display-time costs with reasoning tokens falling
  back to the output rate when a model publishes none. Nothing prices
  anything yet — sessions list/show will footnote the snapshot date.

- The session store now carries a 10k-message cold-open budget: `internal/store`
  builds ten thousand messages (each with a blob part) in a temp database and
  fails the gate if a cold `Open` on that fixture exceeds 50ms (500ms under
  `-short`, where loaded CI runners flake on tight wall-clock asserts). The
  same test pins the listing-shape probe — `SELECT COUNT(*) FROM message` —
  and fails if it ever names `raw_json` or touches the `part` table, so later
  listings cannot drag blobs by accident. Fixture setup sits outside the
  measured window. Nothing is user-visible yet; no command reads these rows.

- `krowk doctor` now reports the local session store's health as a `store`
  StatusCheck, sibling to the harness checks: the `krowk.db` path, the schema
  version and the steady-state pragmas (WAL, NORMAL, foreign keys). It passes
  on a fresh open and fails with an `internal/store` hint — never a reinstall
  — when HOME is missing, the file is unreadable, or the migration is stale.

- The v1 session-store schema: `001_init.sql` now defines the eight tables
  the plan promised — `worktree`, `session`, `session_binding`,
  `session_event`, `turn`, `message`, `part` and `import_state` — instead of
  a comment. Every `id` is a `TEXT PRIMARY KEY` holding a uuidv7 minted by
  `internal/store` (now spelled `store.ParseID` at the boundary, as an alias
  of `ValidateID`, so the two can never disagree). `seq` is the only
  ordering key; dedup is a unique index, never a derived id: a second import
  of the same Claude session converges on one row through
  `UNIQUE(provider, foreign_session_id)`, and a re-imported message through
  the partial `UNIQUE(session_id, foreign_id) WHERE foreign_id IS NOT NULL`,
  which lets NULLs repeat. Foreign keys run `ON DELETE CASCADE` from
  `session` down, so deleting a session takes its bindings, events, turns,
  messages and parts with it but never its worktree. Every `time_*` column
  is `INTEGER` milliseconds; `role` is a closed `CHECK`
  (`user|assistant|system|tool|error`) while `provider`, `harness` and `type`
  stay open strings. The gate tests name every table and every unique index,
  assert every own `*_id` column carries a cascading FK with a leading index
  (`foreign_session_id`, `foreign_id` and `tool_call_id` hold other systems'
  ids and must carry none), and pin the deliberate absences: no
  `turn_attempt`, `attachment`, `model_cache`, `convention` or `migrations`
  table, and no unique gate on one assistant message per turn. Nothing is
  user-visible yet; no command writes these tables.

- A new `internal/store` package begins the local session store — the
  `krowk.db` SQLite file that will hold the sessions, messages and parts krowk
  syncs. This change ships only the two decisions every row in it depends on:
  how a row is named, and how a time is written. Nothing is user-visible yet,
  and no command touches it.

  Ids are UUIDv7 in canonical lowercase hyphenated form, with no type prefix.
  The registry's primary keys are already uuidv7, so an id of the same shape
  lands in a native `uuid` column on sync instead of in text, and the two
  halves index and compare the same way. The prefix was dropped on purpose:
  the `foreign_id` columns beside ours hold ids that already carry one —
  opencode mints `ses_`/`msg_`/`prt_`, Anthropic mints `msg_…` — and a krowk
  prefix sitting next to those would read as though it meant the same kind of
  thing. They are minted from the standard library, no dependency added, with
  a 12-bit per-millisecond counter in `rand_a` (RFC 9562 §6.2) so that ids
  issued inside one millisecond by one process still sort in the order they
  were issued — across processes the order is millisecond-granular — and a
  clock that steps backwards cannot hand out an id that sorts before one
  already given away. That keeps a recent-first listing a plain `ORDER BY` on
  the primary key; the order of messages inside a session will be a `seq`
  column, not the id. The timestamp inside an id is monotonic rather than a
  clock reading — when a millisecond's counter fills, or the clock steps back,
  it advances past the last value used, so it can run ahead of the real time.
  It orders rows; the time columns record when things happened. `ValidateID`
  refuses at the store boundary anything this package would not have minted, so
  a v4 uuid from a Claude transcript, an opencode `ses_…` or a registry slug
  cannot enter as one of our own ids.

  Every time column in the store is milliseconds since the Unix epoch, UTC, as
  an int64 — not seconds, not nanoseconds, not a string — and the clock is
  injected, so a test freezes the id timestamps and the time columns together.

- The session store now has its SQLite driver: `github.com/ncruces/go-sqlite3`,
  the cgo-free build of unmodified SQLite (Wasm through wazero), wired in as
  the `database/sql` driver `internal/store` alone imports. It was picked
  because the release binary must stay static with no toolchain, which rules
  out cgo; of the pure-Go drivers this is the lighter one. No optional
  extensions are enabled until a product asks for one. Nothing is user-visible
  yet, and no command opens the database; that arrives with `store.Open`.

- `store.Open` opens the session store: one global file at
  `$XDG_DATA_HOME/krowk/krowk.db` (a relative `XDG_DATA_HOME` is ignored, as
  the basedir spec says), otherwise `~/.local/share/krowk/krowk.db`. Global on
  purpose — a cwd-based path would split the store per repo. The environment
  is injected, and a missing or relative home means Open fails with a hint
  instead of inventing a path at `/` or the working directory. The parent
  directory is created when missing and the database file is created `0600`
  before SQLite touches it — a file or sidecar (`-wal`, `-shm`) an earlier
  run left world-readable is tightened back to `0600` on open, so the store
  itself stays private to the user. (Side files SQLite creates after `Open`
  returns follow the process umask; the write path owns those.) Every connection the pool
  opens carries the same pragmas via the DSN: `journal_mode=WAL` so readers
  never block the writer, `synchronous=NORMAL` (safe under WAL — a power cut
  can lose the last moments, never corrupt the file), `foreign_keys=1` because
  SQLite ships with them off and every schema here assumes them on, and a 10s
  `busy_timeout` so a briefly locked database waits instead of failing.
  Nothing is user-visible yet; no command opens the database.

- `store.Open` now versions the session store: the first open of a fresh
  `krowk.db` applies `001_init.sql` in one transaction and stamps
  `PRAGMA user_version = 1`; a reopen runs no DDL. A file at any other
  version, at version 0 with tables `Open` never wrote, or at version 1
  with a table missing — or a file that is not a database at all — fails
  to open with a hint to run `krowk sessions rebuild` (delete the file and
  re-import): there is no silent repair and no in-place migration path in
  v1, because every row is still re-derivable from transcripts on disk. A
  refused file is left byte-identical, mode bits included: the version
  check runs read-only before the read-write open, the opened file is
  pinned by device-and-inode identity before it is tightened, and the
  read-write handle itself carries no persistent pragma until the re-check
  accepts. The steady handle re-verifies check-only and never
  re-initialises a regressed file.
  Two first-launch opens racing each other converge instead of erroring —
  the loser adopts the winner's schema. The store directory is tightened
  to `0700` like the database file. There is still no `migrations` table;
  it arrives with the first state the source files do not hold. Nothing is
  user-visible yet; no command opens the database.

- A harness registry (`internal/harness`) that detects which coding agents are
  installed and asks each one whether krowk is actually wired into it. Claude
  Code is the first: detected by a `~/.claude/` directory or a `claude` binary,
  then checked for the krowk MCP server in every scope it can be registered in
  — user or local scope in `~/.claude.json`, project scope in a checked-in
  `.mcp.json` — and for the krowk skill. `CLAUDE_CONFIG_DIR` is honoured, as
  the installer already does. Nothing surfaces this yet; `krowk doctor` and
  `krowk setup` will, and they will agree because they will be reading the
  same checks.

- The installer no longer overwrites a Claude Code skill directory it did not
  write. `scripts/install.sh` now leaves two files beside the skill it
  installs — `.managed-by-krowk-cli`, which says krowk manages the directory,
  and `.installed-version`, which says with what — and writes only where that
  marker says it wrote before: it creates a directory that is not there,
  adopts an empty one, refreshes one carrying a marker it wrote, and otherwise
  says why and leaves the directory exactly as it found it. A directory that
  cannot be listed is left alone too, as is one belonging to another user (on
  Unix, where there is a uid to compare).
  If you have your own `~/.claude/skills/krowk/`, move it aside and re-run to
  have krowk manage it.

  Upgrading from an earlier krowk needs nothing: a skill directory holding
  nothing but the `SKILL.md` a previous installer wrote is adopted and marked
  on the next run, since that is the only file that installer wrote and the
  one it overwrote anyway. Nothing is written through a symlink any more, in
  either the skill directory's own name or a managed file's — a symlinked
  `~/.claude` or `~/.claude/skills` still resolves, as it should — and a file
  is never truncated in
  place: both halves of krowk write every managed file to a sibling temporary
  file and rename it into place, so a reader sees the old file or the whole
  new one, and a second hard link to somebody's file keeps its contents.

  `internal/harness` carries the same gate for Go — `ClaimDir`,
  `WriteManagedFile`, `StampVersion`, `InstalledVersion`, `DirOwned`,
  `IsManagedCopy` — where
  every read is bounded and, on Unix, goes through an `O_NOFOLLOW`,
  non-blocking open, so a symlink, a FIFO or an oversized file in a managed
  name is refused rather than followed, waited on or half-read. Windows is
  weaker and says so in the code: the marker is read by checking the path and
  then opening it, with a small window in between, and there is no ownership
  check at all — closing either needs the Win32 API. No
  command calls any of it yet: it is what `krowk setup` will go through, so
  that one rule decides every write krowk makes into your home directory.
  `krowk doctor` will report a skill it did not write as installed either way,
  and say whether the next install will adopt it or leave it alone for good.

### Fixed

- The opencode importer, still unreleased, holds its review findings: the
  database path rides percent-encoded in the read-only DSN, so `?#&` in a
  directory can no longer escape the path and override `mode=ro`; `Read`
  refuses a ref naming anything but the known database instead of opening
  the hinted path. The watermark is the largest timestamp successfully
  imported over message and part rows alike, and skipped rows no longer move
  it, so a failed row is retried rather than forgotten. Oversized-row prefix
  scans are anchored to the top level of the blob (tokens scoped to the
  `tokens` object), so a nested field name in prose cannot flip a role or
  inflate a turn; cap checks count bytes, not characters. A turn whose
  messages carried no cost keeps a NULL dollar cost instead of a guessed
  zero, a child session's parent binding carries its resume command, and
  worktree `vcs` passes through only `git` (anything else is `none`) with
  the path cleaned. A tool status outside `completed`/`error` still twins
  nothing, but is now classified under its own name instead of vanishing
  silently, as is a message role outside `user`/`assistant`/`system`. A
  database that vanishes between listing and reading is an empty machine,
  not an error, and a permission refusal reports the same empty answer
  whether it lands on the stat or the query.

## [0.9.0] - 2026-09-06

### Added

- `krowk push --private` uploads where only your workspace can read it, and
  `krowk_push` takes `private: true` for the same thing. The image still embeds
  anywhere: a private artifact's bytes sit on the CDN under a key whose secret
  segment is the whole of the authorization, which is what lets GitHub, Jira or
  Slack — fetching an embed server-side and anonymously, carrying nobody's
  session — render it at all. What changes is the card. `krowk.com/a/{slug}`
  opens only for a signed-in workspace member and answers everyone else exactly
  as it answers a slug that was never minted, and the API read is gated the same
  way, so nothing unfurls a private link.

  It needs an API key and is refused rather than published without one: a
  keyless upload lands in the shared anonymous workspace, which nobody is a
  member of, so there is nothing for it to be private to. The refusal comes
  before anything is sent — an agent told afterwards that its `--private` was
  dropped would have already published the file.

- Every artifact now reports its own `visibility`, on every read, as a name
  rather than a flag. A visibility this build has not heard of is described by
  name and promised nothing, rather than being described as private:
  understating who can read an artifact is the dangerous way to be wrong about
  a privacy feature. `shared` is the visibility whose card a keyless holder
  of the link *does* see, via `share_url`; this build knows it, links it, and
  labels what it unfurls.

- A push that asks for a visibility now checks it was applied before it sends
  the bytes. A registry predating the field accepts the declare and answers
  without it, which is a silent downgrade to public — and once the bytes are on
  a CDN there is nothing left to refuse. Nothing is uploaded, and the artifact
  the declare made is a pending row that expires on its own.

### Changed

- Claiming is plan-aware everywhere krowk explains it. The registry now keeps a
  claimed artifact only when the workspace is on a paid plan; claiming into a
  free workspace moves the artifact and restamps a fresh 24-hour expiry, and a
  keyed upload into a free workspace expires in 24 hours just as an anonymous
  one does. The `expired` fix line, the claim breadcrumb, `krowk help`, the MCP
  tool description and the README no longer promise that a key alone keeps an
  upload. The stand-in registry behind `--dev` follows the same rules: a key
  with `free` in it is a free workspace, every other key is a paid one.

- A `.webm` or `.mkv` file carrying a video track is declared as video, never
  audio. Go's extension table answers `audio/webm` for `.webm`, so a screen
  recording uploaded on the extension alone landed as audio and previewed as
  audio. The uploader now sniffs the Matroska head for a video track and
  declares `video/webm` (or the `.mkv` video kind) when one is there; audio-only
  files keep the extension answer.

- The paste labels stop promising what a private card cannot do. `Paste into
  Slack, Basecamp — they unfurl the link themselves` is true of a public
  artifact and false of a private one, so a private push is labelled for the
  audience that can actually open it, and the breadcrumb that used to say "hand
  this link on — it is public and needs no key to read" says who it opens for
  instead. The markdown label still promises the image, because the image still
  renders. Human output names the visibility beside the size when it is not the
  public default, and `krowk uploads list` names it per row.

- `--format url`, and any `--destination` the registry's table says wants the
  bare link, warn on stderr when what they printed is a private card. That form
  exists to be unfurled and a private card unfurls nowhere, so
  printing one silently would be the same broken promise in a different place.
  The warning is on stderr rather than in the output, because the output is
  about to be pasted.

- The bundled stand-in registry (`go run ./internal/devregistry`) enforces the
  same contract, so a client developed against it behaves the same in
  production: visibility is declared, validated and served; a private artifact's
  metadata answers its own workspace and answers everyone else `404`; its card
  page is indistinguishable from a slug that never existed; its byte URL names
  neither the workspace nor the artifact; and `PUT /v1/artifacts/{slug}/visibility`
  moves an artifact between public, private and shared, re-keying the bytes and killing
  the old URL in every direction. A shared artifact carries `share_url`
  (`{origin}/a/{slug}?share=krowk_share_{24 base36}`) on every artifact
  response, null otherwise; entering shared mints a fresh token and leaving
  clears it; a keyless declare or visibility change naming shared is refused as
  `shared_needs_key`; and `GET /v1/artifacts/{slug}?share={token}` answers 200
  for the matching token and 404 for anything else.

  One behaviour it had wrong is fixed with it: a **public** artifact's metadata
  read is now scoped to no workspace, keyed or not — matching the registry,
  where a scope a reader escapes by dropping the `Authorization` header would
  protect nothing.

  Its `Idempotency-Key` digest is also now taken over the declared artifact as
  an object — the permitted parameters, canonicalized with keys sorted at every
  level — rather than over a list of fields somebody had to remember to extend,
  which is how `metadata` had fallen out of it. Two things follow that a field
  list could not express: a parameter left out stays distinct from one sent
  empty, and a client that re-serialized its own body between attempts gets its
  first answer back rather than a second artifact.

## [0.8.2] - 2026-08-29

### Added

- A spinner on the one thing krowk does that takes long enough to look hung. An
  upload of a few hundred kilobytes over a slow link is four seconds of a
  terminal that has printed nothing, which is indistinguishable from one that has
  stopped, so a single line on stderr says what is being sent and keeps moving —
  naming each file in turn when there are several. It is not progress and does
  not pretend to be: krowk hands the file to object storage in one request and is
  never told how much of it has landed, so a percentage would be a number krowk
  invented. It erases itself before the durable line is printed, so nothing about
  it reaches the scrollback and a transcript reads as though the wait never
  happened. Shown only when stderr is a terminal and the answer is prose:
  `--json`, `--quiet`, `--destination`, the paste formats and any piped stream
  are read rather than watched, and escape codes in a captured file help nobody.

### Changed

- Human output now reads as a person would say it, while the JSON envelope keeps
  every code and every fix string exactly as it was — agents parse the envelope,
  and none of this reaches them.
  - A failure leads with the fix as a sentence rather than with the wire code:
    `✗ Re-encode below 100 MB or push frames separately.` where it used to open
    on `artifact_too_large`. The code is still there, dimmed, one line down and
    in the envelope; a command the fix names is pulled onto its own line so it
    can be copied rather than picked out of prose, and a fix that names two
    things to do says both, one per line.
  - A success reads as a confirmation rather than as the record read back at
    somebody who already knows what they pushed:
    `✓ Uploaded shot.png → https://krowk.com/a/art_2e1d`, with the size, the run
    and the expiry dimmed on the line under it. `claim`, `uploads delete` and
    `runs start` / `runs finish` got the same treatment — `✓ Took art_2e1d down`,
    `✓ Finished run run_7f`, and no wire timestamp read out at a person.
  - An expiry is said the way somebody would say it out loud: `expires tomorrow`
    rather than `expires in 24h`, counted in midnights and in the reader's own
    zone, so an upload at eleven at night expires tomorrow however few hours that
    is. The MCP server still prints the exact duration, since an agent does
    better with a number.
- `krowk help` is now laid out for reading rather than for completeness: the
  commands are grouped under `PUSH & PASTE`, `RUNS`, `UPLOADS` and
  `ACCOUNT & SYSTEM` in two aligned columns, under a `USAGE` block that leads
  with the one command that matters, and it closes on what to type next. The
  groups are written down once and filled from the same catalog `krowk help
  --json` is rendered from, so a command cannot exist in one and not the other.
  The machine surface is unchanged.

## [0.8.1] - 2026-08-28

### Added

- The Krowk mark at the top of `krowk` and `krowk help`: the four-by-four grid
  of squares from the logo, drawn in half block characters so that two rows of
  the grid share one row of text — a character cell is twice as tall as it is
  wide, so a square of the grid drawn as a whole character would stretch the
  mark, and half a character keeps it square at the smallest size it can be
  drawn at. A blank line above and below so it is not jammed against the chrome
  or the words, and no colour, so it takes the foreground of whatever theme is
  running. Under it, `Krowk` and the version on one line and what krowk is on
  the next. It opens what a person reads and nothing else — the JSON surface
  stays a data structure, and one command's help stays an answer to the
  narrower question.
- A moving major tag for the GitHub Action: `uses: krowkcom/cli@v0` follows the
  0.x line rather than freezing a workflow on one patch release, and each
  release moves it once the archives and the npm packages are up. A release tag
  still pins the action and the CLI together, and an explicit `version` input
  still wins over both. The tag has no release of its own, so it installs the
  latest — and the action now refuses to install across a major line rather
  than hand a workflow pinned to `@v0` a 1.x binary with a changed command
  surface.

### Changed

- `krowk` on its own now greets rather than printing the manual. Typing the name
  to see what happens used to answer with every flag, every exit code and every
  paragraph of prose — about 150 lines, and neither "what is this" nor "what do
  I type" was any easier to find for it. It is now the mark, what krowk is, and
  three lines: the first upload, the key that makes uploads keep, and
  `krowk help` for the rest. Those three are the ones the installer signs off
  with, so a first run says what the install said. `krowk help` and
  `krowk --help` are unchanged and still answer in full, and a program reading
  `krowk` — piped, `--json`, or with a `--jq` expression — still gets the whole
  surface, since prose is no use to it and the surface is what it came for.

### Fixed

- The release workflow no longer triggers on every `v*` tag, only on a
  three-component version. The moving major tag is a `v*` tag too, and a
  release run for `v0` would have tried to cut a release of version "0" and
  publish it to npm.

## [0.8.0] - 2026-08-27

### Added

- `--link`, for the links a piece of work is about — the issue it fixes, the
  spec it implements, the discussion behind it. Repeat it for more than one, up
  to twenty, and label each with `--link-title` and classify it with
  `--link-rel` (`tracks`, `fixes`, `spec`, `discussion`, `source`,
  `supersedes`, or a word of your own); both describe the `--link` before them.
  They land on the run as `krowk.links`, an array of `{url, title, rel}`
  objects, so a reader can name a link instead of showing a raw URL. A link
  that is not an absolute `http(s)` URL, one with a space in it, a title over
  140 characters or a rel over 64, either of them carrying a tab, a newline or
  another control character, a twenty-first link, or a set of links
  large enough to crowd out the detected metadata is refused rather than
  trimmed — metadata is stored verbatim and nothing downstream validates it
  again, so a shortened URL would be a link to somewhere else for as long as
  the record lives. `--reference` is
  unchanged and is now the place for identifiers that are not URLs, such as a
  bare ticket key.
- The same links on the MCP `krowk_push` tool, as a `links` array whose schema
  names the suggested `rel` values, so an agent picks from the vocabulary
  rather than inventing one.
- A GitHub Action, `uses: krowkcom/cli@<tag>`, wrapping the CLI for CI: give it
  files or globs, it installs the binary, pushes them, and hands back `urls`,
  a `markdown` paste block ready for a PR comment, the `run-slug` and the
  `json` envelope with its claim tokens stripped — with the links also written
  to the job's step summary. The pull request, repo and commit are detected
  from the runner's environment, the same way they are locally. Pinning the
  action to a release tag pins the binary to that release, an explicit
  `version` input wins over the tag, a directory a glob swept up is named
  rather than handed to krowk, and a `**` glob on a bash too old for one
  (macOS ships 3.2) fails saying exactly that while plain globs keep working.

### Fixed

- Run metadata passed to a push that names an existing run is now reported as
  dropped instead of vanishing. `krowk push shot.png --run run_… --link …`
  records nothing on that run — a run carries the metadata it was opened with —
  and both the CLI and the MCP tool now say so in `notes`, naming the flags and
  the run. `--caption` and `--metadata` are unaffected: they land on the
  artifact. The keyless note gained `--title` for the same reason: it was
  dropped and unmentioned.
- The branch a run records in CI. GitHub checks out a detached HEAD, where
  git can only answer the literal `HEAD` — the branch is read from the
  runner's environment instead, preferring a pull request's source branch
  over the synthetic `412/merge` ref, and taking nothing from a tag push. A
  local detached HEAD records no branch at all now, which is the truth of it.

## [0.7.0] - 2026-08-26

### Removed

- The `registry serve` command. It was a development tool, but it sat in the
  public help and the surface JSON, where an agent reading `krowk --help` would
  take it for a way to host uploads — and host them on a process whose links
  die with it. The stand-in still exists for developing krowk itself: run it
  with `make mock` (`go run ./internal/devregistry`) and point commands at it
  with `--dev` as before.

## [0.6.0] - 2026-08-24

Upgrading from 0.4.1 over npm? This carries everything 0.5.0 did as well —
`--jq`, pasted links where a slug is asked for, and the rest — because 0.5.0
was released on GitHub but never published to npm.

### Changed

- Every paste form krowk prints now comes from the registry verbatim — the
  block, the bare link, and the `destinations` table beside them in the JSON
  envelope. Nothing is assembled in the CLI any more, which is what lets the
  look of a krowk reference change in a single registry deploy, including for
  the installs that already exist. `--format markdown` therefore prints the
  whole block rather than a one-line embed, and several files come back as
  several blocks separated by a blank line rather than one line each.
- The bundled agent skill now says plainly what it only implied: never paste a
  bare artifact link anywhere, use `paste.markdown` / `paste.url` and pick
  between them with the served `paste.destinations` table, and reach for
  `--destination` where the destination is known. It also nudges: an unclaimed
  artifact pasted into a pull request, an issue or a doc becomes a broken image
  once it expires, so the claim step is surfaced to the person before the paste
  rather than after. A test holds the skill to those lines.
- `--title` no longer relabels a pasted link. It is the title of the work and
  lands on the run, as it always did; what a pasted link says about a file is
  now that file's `--caption`, which is recorded on the artifact and read back
  by whatever renders it. `krowk_push` over MCP follows the same rule.

### Added

- `--destination <tool>` on `push` prints what that tool wants pasted into it —
  the krowk block for `github`, `linear` and the other markdown surfaces, the
  bare card link for `slack`, `basecamp` and the others that unfurl one
  themselves. A tool krowk has not been told about gets the block, and the push
  still succeeds: a block where it does not render is informative text, a bare
  link is a link nobody can tell anything about. Which tool wants which form is
  the registry's table, served with the artifact and readable at
  `paste.destinations`, so a tool proving out reaches installs that predate it —
  there is no list of tools inside the CLI. It cannot be combined with
  `--format`, `--json` or `--jq`, which ask for a different rendering of the same
  result; that is refused as `bad_flag` rather than silently ranked.
- `--caption '<text>'` on `push` records what a file shows on the artifact
  itself, as `krowk.caption`, so whatever renders the link later — a card page,
  a pull request comment, an integration — reads the caption off the record
  instead of being told it again at every destination. It is per file and
  repeatable: `krowk push before.png after.png --caption 'Cart before the fix'
  --caption 'Cart after the fix'` captions each one, a single caption covers a
  whole set, and a count that matches neither is refused as `bad_flag` rather
  than guessed at. Distinct from `--title`, which stays a label for the work and
  lands on the run. A keyless push drops it as it drops all metadata, and says
  so in `notes`.
- Ordinary `krowk push` output now ends with the ready-to-paste krowk block, so
  the last thing on screen is the thing worth copying rather than a bare link.
  `--quiet` still prints the record and nothing suggested.
- The paste envelope `krowk registry serve` answers with now carries the krowk
  block itself — the image, the caption from `krowk.caption`, the link through
  to the card, and the expiry of an unclaimed upload — beside the bare link and
  the destination table. It is what the production registry serves, so a paste
  built against the local stand-in looks like a paste built against production.

## [0.5.0] - 2026-08-21

### Added

- `--jq '<expression>'` filters a result inside krowk, with jq compiled in — no
  jq binary to install and no pipe to build. It works on every command, implies
  `--json`, and reads what the command rendered: the envelope normally, the bare
  record under `--quiet`. A string result prints without its quotes, so
  `URL=$(krowk push shot.png --jq '.data.artifacts[0].url')` is the whole
  ceremony; anything else prints as JSON, one value per line. `krowk help --json`
  is filterable too, which is the shortest way for an agent to read the surface.
  A failure is filtered like any other result, in whatever shape the command
  rendered it: `--jq '.error.error'` reads the code out of an envelope, and
  `--quiet --jq '.error'` reads it out of the bare body. An expression that does
  not parse is refused as `bad_jq` before the command sends anything, and one
  that does not fit the result it was pointed at answers `jq_failed` afterwards,
  saying that the command itself succeeded so that a wrapper retrying on a
  non-zero exit does not repeat the work. A failure `--jq` caused is always
  reported whole, since filtering the complaint with the expression behind it
  would bury it. `auth token`, `registry serve` and `--version` print no JSON,
  and refuse the flag rather than ignore it — the surface says which commands
  those are, under `no_json`. Neither can it be combined with `--format human`,
  `markdown` or `url`, since one of the two would have to be discarded.
  `doctor` and `upgrade` answer with a bare record and no envelope, as they
  always have, so filter those as `--jq '.token_source'`.
- Every command that names a record now takes the link as readily as the slug.
  Paste the card page, `https://krowk.com/a/art_…`, the CDN URL under it, or the
  markdown line carrying both, into `uploads show`, `uploads attach`,
  `uploads delete`, `claim`, `runs show`, `runs finish` or `--run` — the slug is
  read out of it, and the MCP tools take one the same way. Anything that is not
  link-shaped is passed on untouched, so slugs behave exactly as they did.
- A link carrying no slug of the kind the command wants now fails as
  `bad_artifact` or `bad_run` (exit 1) before anything is sent, instead of going
  out and coming back as a record that does not exist. A card link handed to
  `runs show` names the artifact it carries, and a link carrying two different
  artifacts is refused rather than acted on — the takedown has no undo.

### Changed

- A `--format` nobody has heard of is now refused even when `--json` or `--jq`
  was passed as well. It used to be accepted and ignored, so a caller who meant
  `--format markdown` and mistyped it was told nothing.
- A claim token is trimmed before it is sent, on `claim` and on
  `uploads delete`, so one copied with a trailing space or newline works instead
  of failing as an unauthorised claim.
- A blank record where one is required is now the command's own missing-argument
  failure rather than a request. `--run "  "` — a shell expanding an unset
  variable — no longer reads as "no run at all", where it used to open a fresh
  run on a push and widen `uploads list` to the whole workspace. A blank
  artifact answers `no_artifact` (exit 1) on both the CLI and the MCP server,
  which previously answered `missing_claim` (exit 3, a credential to fix).
- Failures about a pasted value no longer quote it back. A URL is where
  credentials travel, and a refusal is written to stderr and into the JSON
  envelope.

[Unreleased]: https://github.com/krowkcom/cli/compare/v0.9.0...HEAD
[0.9.0]: https://github.com/krowkcom/cli/compare/v0.8.2...v0.9.0
[0.8.2]: https://github.com/krowkcom/cli/compare/v0.8.1...v0.8.2
[0.8.1]: https://github.com/krowkcom/cli/compare/v0.8.0...v0.8.1
[0.8.0]: https://github.com/krowkcom/cli/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/krowkcom/cli/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/krowkcom/cli/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/krowkcom/cli/compare/v0.4.1...v0.5.0
