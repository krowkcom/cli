// Package cursor reads Cursor's agent transcripts off this machine and turns
// them into the canonical store.Thread that internal/importer fixes.
//
// Layout: <home>/.cursor/projects/<slug>/agent-transcripts/<id>/<id>.jsonl,
// with a repo.json beside agent-transcripts holding {"id": "<uuid>"} — a repo
// id only, not a path.
//
// # A line is a role or furniture
//
// Observed shapes are {"role":"user"|"assistant","message":{"content":[...]}},
// with content blocks {"type":"text","text":...} and {"type":"tool_use",
// "name":...,"input":{...}} — the tool_use blocks never carry an "id" field —
// and {"type":"turn_ended","status":...} with no role at all. ZERO
// tool_result blocks were observed on the census machine, so the tool_result
// shape below is leniency over unobserved shapes, documented as such. Every
// line lands in exactly one of three places — a message, Result.Classified,
// or Result.Skipped — and a test asserts those three add up to the line
// count, with the repo.json event held apart: it is not a line, so it is
// asserted separately rather than folded into the sum.
//
// # Position-keyed dedup, because there are no ids
//
// Nothing in a Cursor transcript names a message: no uuid per line, no id on
// tool_use blocks. So Message.ForeignID is always "" (stored NULL), and the
// store appends such a message unconditionally — the caller must hold the
// JSONL byte-offset cursor and never re-send. That is why this source,
// unlike claude/opencode, honors the cursor: Read passes it to
// importer.ReadJSONL and delta reads return only new lines for messages.
// The tool_call_id for an id-less tool_use is "cursor:<absolute line>": the
// count of newlines before the resume offset plus the per-read line number,
// which is the file's own line number no matter where the read started —
// stable for full reads and delta reads alike, so a delta continuation
// never reuses an id an earlier read already emitted. A second id-less
// tool_use on the SAME line takes "cursor:<line>:<k>" (k=1,2,…): the id keys
// the line yet real transcripts average ~3 tool_use per assistant line, and
// single-call lines are byte-identical to before. Pairing only ever happens
// within one Read pass. Do not hand-edit cursors: a mid-line resume re-reads
// the overlapping line, and a NULL-id re-read ingests twice — claude
// absorbs the repeat through foreign-id dedup, but cursor lines are
// indistinguishable from legitimately new lines, so the duplicate lands as
// a new message. A crash or hand edit that leaves a mid-line cursor may
// therefore duplicate one message.
//
// A transcript rewritten shorter rescans from zero and re-imports its
// messages as new rows: dedup is impossible without ids, which is inherent
// to position-keyed NULL-id imports rather than a gap the cursor could
// close. The rescan is still marked a full read, so the repo sidecar is
// re-emitted rather than lost. A rewrite that leaves the file the same
// length or longer is invisible to the cursor — the documented bargain of
// JSONLCursor — and for NULL-id imports that means a re-import silently
// duplicates, where claude converges through foreign ids. Callers must not
// trust a cursor across a rewrite.
//
// # The transcript never names a time, a directory, or a model
//
// Thread carries no per-message time and the store stamps ingest time, which
// coincides with the import — so the file mtime is deliberately not read: it
// is noted as the no-better-source clock, not taken as a timestamp. User text opens
// with <timestamp>...</timestamp> tags; they are left in the text, not
// parsed. Session.Directory stays "" and Model stays "": the transcript
// names neither. Provider is "cursor" throughout, the model-vendor fallback,
// and Harness is "cursor". ResumeCmd is "" — resuming is unknown in v1.
//
// # The slug is not a path until the filesystem says so
//
// The slug decodes as "/" + strings.ReplaceAll(slug, "-", "/"), but that
// transform is not invertible — a dash in a real directory is
// indistinguishable from a separator — so the decoded path is used ONLY if
// that directory exists on disk, and then walked up for a .git entry the way
// the Claude reader does. Otherwise the worktree is "cursor:<slug>" with vcs
// "none", counted via acc.Classify as "worktree-fallback". Name is
// filepath.Base(path) — for the fallback that Base is the whole
// "cursor:<slug>", which is fine: it names the slug rather than inventing a
// directory.
//
// # repo.json is one event, on full reads only
//
// repo.json gives ONE session_event of type "cursor_repo" with data
// {"repo_id": ...}. Missing or unreadable repo.json is no event, not an
// error. The event is emitted only when the incoming cursor is zero: turns
// and events are positional cumulative lists in the store, so re-emitting it
// on every delta read would append a duplicate per import. A thread returned
// with err != nil must not be ingested; the sidecar still rides full reads,
// error partials included.
//
// # Turns are always cumulative, messages are delta
//
// Every Read returns turns over the whole file: a resumed read runs a
// second full scan from zero for candidates only (its messages, events and
// Result are discarded, so Unknown is not double-counted), at ~1.2x parse
// cost on files that are small. The store's turn list is a positional
// cumulative list — insertTurnTail skips the known prefix — so a delta
// computed over the delta would insert nothing and lose the new prompt's
// turn. Messages stay delta: re-reading the whole file would duplicate
// every NULL-id row, so only new lines become messages, and the repo.json
// event rides full reads only.
//
// # tool_result is a guess held leniently
//
// With zero tool_results observed, both likely shapes are supported:
// {"type":"tool_result","tool_use_id"|"callID"|"id":..., "content"|"output"...,
// "isError"|"is_error"...}, with content a string, blocks, or raw. A result
// whose id matches no call in this Read is still emitted, counted via
// Classify as "tool_result:unlinked" — dropping it would lose transcript
// over a pairing this package cannot verify. Linkage is reconciled after the
// pass, once every call has been seen, so a result preceding its call in the
// same Read still links. Cross-read links classify unlinked by design: the
// pairing set is per-read, and real transcripts carry zero results.
package cursor
