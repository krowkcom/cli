// Package store is the local session store: the SQLite file (`krowk.db`) that
// will hold the sessions, messages and parts krowk syncs to the registry. This
// change ships none of that. It ships the two things every row in that file
// will need before there is a file at all — how a row is named, and how a time
// is written — so that both are decided once and tested on their own.
//
// An id is a UUIDv7 (RFC 9562) in canonical lowercase hyphenated form, and it
// carries no type prefix. Two reasons, both about somebody else's ids:
//
//   - The registry's primary keys are uuidv7. Matching that shape means a
//     synced session lands in a native `uuid` column rather than in text, so
//     the two halves compare and index the same way.
//   - The `foreign_id` columns beside ours already hold prefixed ids from the
//     agents we mirror — opencode mints `ses_`/`msg_`/`prt_`, Anthropic mints
//     `msg_…`. A krowk prefix would sit next to those and read as if it meant
//     the same kind of thing.
//
// UUIDv7 also sorts by time as a string, which is why the counter in Minter
// exists: ids minted inside one millisecond still sort in the order they were
// issued by one minter, so a listing ordered by primary key is a listing
// ordered by age — across processes, whose counters seed independently, that
// order is only as fine as a millisecond. That
// is index locality and a readable recent-first list, not the transcript
// order — within a session the order of record is the seq column, because the
// transcripts we import do not keep time straight and a clock is not a
// sequence.
//
// The timestamp an id carries is monotonic rather than a reading of the clock:
// when a millisecond's counter fills, or the clock steps back, it advances past
// the last value used, so it can run ahead of the real time. It is a sort key,
// not a measurement — what a time column stores comes from NowMS.
//
// Every time column in this store is milliseconds since the Unix epoch, UTC, as
// an int64 — never a string, never seconds, never nanoseconds. The injected
// clock is the only clock this package reads: NowMS and the timestamps in ids
// both come from it, so a test that freezes time freezes both.
package store
