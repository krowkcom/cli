// Package opencode reads opencode's transcripts off this machine and turns
// them into the canonical store.Thread that internal/importer fixes.
//
// Opencode keeps every session in one SQLite database rather than in
// per-session files, so this source opens that database and queries it —
// one Thread per session, with Discover returning one ref per session row
// so each session carries its own watermark and Ref.Key stays
// "opencode:<session_id>".
//
// # The database is opened read-only and stayed read-only
//
// The open is a single database/sql connection through the bundled driver
// with a file: DSN carrying mode=ro and SetMaxOpenConns(1). The path rides
// percent-encoded in the DSN's file component, so ?#& in a directory cannot
// escape the path and override the read-only mode. Read-only is load
// bearing, not polite: opencode holds the database open in WAL mode while
// it runs, and anything but mode=ro risks creating -wal/-shm sidecars or
// taking a lock against the running agent. No PRAGMA is ever issued —
// journal_mode especially is never touched — and a test hashes the file
// before and after a Discover plus a Read to prove it.
//
// # Ids are opencode's, order is time
//
// Opencode mints ses_/msg_/prt_ ids and this package never derives any:
// Message.ForeignID is message.id, Part.ForeignID is part.id, and the
// binding is the session id. Transcript order is time_created ascending
// (rowid breaking ties), never the ids, which sort in creation order only
// by accident of the mint.
//
// # Read always reads the whole session
//
// The cursor it takes and returns is importer.SQLiteCursor, the largest
// message time_updated seen. Like the Claude reader this one re-reads from
// the start every time and says so: turns are cumulative positional lists
// whose costs are summed over a whole span, so a read resumed from the
// middle could neither number them nor cost them, and the store's
// foreign_id dedup makes the re-read free. A cursor of the wrong concrete
// kind is refused with importer.ErrCursorType and handed straight back.
//
// # A turn is a costing unit, not a link
//
// Turns are split with importer.SplitTurns over one candidate per message.
// Opencode user rows are always prompts: there is no Claude-style injected
// furniture wearing the user role (attachments, reminders, hook output
// filed as user lines), so Meta is never set and no user row is filtered.
// The only thing that does not open a turn is a tool_result-only message,
// which SplitTurns already excludes — and that shape does not occur in
// this source anyway, because opencode keeps tool results on assistant
// messages rather than sending them back under the user role. Huge summary
// rows keep their prompt status too: a user row over the raw cap still
// opens its turn with identity kept and raw dropped. Each turn's
// token columns are summed over its span and its dollar cost is
// round(sum(cost)*1e6) micros, because message.data prices in dollars and
// the store costs in micros; a span where no message carried a cost keeps
// a nil dollar cost rather than a guessed zero.
//
// # Tool calls twin with their results
//
// An opencode tool part carries both the call and its result in one row:
// {tool, callID, state:{status,input,output}}. A finished (completed or
// error) tool becomes two canonical parts on the same message — a
// tool_call built by importer.NewToolCallPart and a tool_result built by
// importer.NewToolResultPart sharing the call id — because the contract
// pairs results to calls by tool_call_id and nothing else joins them. A
// tool still running or pending becomes the call alone. Every other part
// type keeps its raw payload through Result.NormalizePart: text stays
// text, reasoning becomes thinking, file stays file, patch stays patch,
// step-start and step-finish become step, and whatever opencode adds next
// lands as unknown and counted rather than dropped.
package opencode
