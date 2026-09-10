package importer

import (
	"encoding/json"
	"fmt"
)

// Cursor is a watermark, serialised as JSON into import_state.cursor under
// the key Ref.Key returns. There are two shapes because there are two kinds
// of source, and pretending otherwise would mean storing a byte offset for a
// SQLite file that has no meaningful one.
//
// The interface is deliberately tiny. A cursor is written by whoever took it
// and read by whoever resumes from it; nothing in between needs to know
// which kind it holds, and a caller that does know decodes the concrete type
// directly.
// Encode is the whole interface. Zero is a method on the concrete cursors
// rather than part of the contract, because deciding whether to resume is
// something a caller does once it knows which kind of source it is driving.
type Cursor interface {
	// Encode is the JSON to store. It never fails for the shapes in this
	// package, but the signature admits it so a future cursor carrying a
	// map does not have to change every call site.
	Encode() (string, error)
}

// JSONLCursor is how far a line-oriented transcript has been read: Offset
// bytes consumed, taken when the file was Size bytes long.
//
// Size is what makes a shrink detectable. A file shorter than Size now is a
// file that was rewritten rather than appended to, and the only safe reading
// of it is the whole thing.
//
// It detects nothing else, and is not meant to. The watermark assumes an
// append-only transcript: a rewrite that leaves the file the same length or
// longer at the same path is invisible here, and a resumed read will trust
// bytes it never saw. That is accepted rather than overlooked. The
// transcripts krowk imports are append-only in practice — Claude and cursor
// both only ever add lines to a session file — and the cost of being wrong
// is bounded by the store, which dedups messages on
// (session_id, foreign_id): a session whose file really was rewritten
// re-imports what changed and converges instead of duplicating. Hashing a
// prefix on every read to close the gap would cost more than the failure it
// prevents.
type JSONLCursor struct {
	Offset int64 `json:"offset"`
	Size   int64 `json:"size"`
}

// Encode renders the cursor for import_state.cursor.
func (c JSONLCursor) Encode() (string, error) { return encodeCursor(c) }

// Zero reports a fresh cursor. Offset is the whole test: a zero offset reads
// from the top whatever Size claims.
func (c JSONLCursor) Zero() bool { return c.Offset <= 0 }

// SQLiteCursor is how far a SQLite-backed source has been read: the largest
// row update time already imported. A byte offset is meaningless against a
// file a database engine rewrites in place, so the watermark has to be
// something the rows themselves carry.
//
// TimeUpdated is milliseconds, matching every time_* column in the store, so
// nothing has to remember which unit this particular number is in.
type SQLiteCursor struct {
	TimeUpdated int64 `json:"time_updated"`
}

// Encode renders the cursor for import_state.cursor.
func (c SQLiteCursor) Encode() (string, error) { return encodeCursor(c) }

// Zero reports a fresh cursor.
func (c SQLiteCursor) Zero() bool { return c.TimeUpdated <= 0 }

// encodeCursor is the one place cursor JSON is produced, so both shapes are
// stored the same way.
func encodeCursor(c Cursor) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode cursor: %w", err)
	}
	return string(b), nil
}

// DecodeJSONLCursor reads a stored cursor. An empty string is the absent
// import_state row and decodes to the zero cursor rather than an error:
// "never read this file" is a normal state, not a corrupt one.
func DecodeJSONLCursor(s string) (JSONLCursor, error) {
	var c JSONLCursor
	if s == "" {
		return c, nil
	}
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return JSONLCursor{}, fmt.Errorf("decode jsonl cursor: %w", err)
	}
	// A negative watermark cannot have been taken by this package, so it
	// came from a hand-edited row or a different schema. Rescanning is the
	// conservative reading; refusing would strand the source forever.
	if c.Offset < 0 {
		c.Offset = 0
	}
	if c.Size < 0 {
		c.Size = 0
	}
	return c, nil
}

// DecodeSQLiteCursor reads a stored cursor, with the same reading of an
// empty string as DecodeJSONLCursor.
func DecodeSQLiteCursor(s string) (SQLiteCursor, error) {
	var c SQLiteCursor
	if s == "" {
		return c, nil
	}
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return SQLiteCursor{}, fmt.Errorf("decode sqlite cursor: %w", err)
	}
	if c.TimeUpdated < 0 {
		c.TimeUpdated = 0
	}
	return c, nil
}
