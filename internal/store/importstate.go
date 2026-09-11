package store

import (
	"context"
	"database/sql"
	"fmt"
)

// import_state is the one table in the schema with no id: the key is the
// source string, which is importer.Ref.Key() — "<provider>:<id>". It holds
// the watermark a re-import resumes from, and nothing else re-derives it,
// which is why it is read and written here rather than rebuilt from the
// rows an import produced.

// ReadImportState returns the stored cursor for key, or "" when there is no
// row yet. Absent is not an error: "never imported this ref" is the normal
// state of every ref on a fresh machine, and a caller that had to tell
// sql.ErrNoRows from a real failure at every call site would eventually get
// one of them wrong.
func ReadImportState(ctx context.Context, db *sql.DB, key string) (string, error) {
	var cursor string
	err := db.QueryRowContext(ctx, `SELECT cursor FROM import_state WHERE source = ?`, key).Scan(&cursor)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: read import state %q: %w", key, err)
	}
	return cursor, nil
}

// IngestWithCursor is Ingest followed by the watermark that says how much of
// the source it covered, so a caller never has to remember to write the
// second one.
//
// The cursor is written in a transaction of its own, immediately after the
// last of Ingest's. It is not inside Ingest's final transaction, and the
// honest reason is that Ingest owns its transactions: it writes the
// worktree, session and turns in one and then the messages in chunks of
// ingestBatchSize, so there is no single "final" transaction to join — the
// last one is whichever message chunk happened to be last, and on a thread
// with no new messages there is none at all. Reaching into that to thread a
// cursor through would mean widening Ingest's transaction shape, and short
// transactions are the concurrency contract the whole writer is built on.
//
// The window this leaves is one process dying between the last ingest commit
// and the cursor commit. What follows is a re-read of a ref whose rows are
// already stored, and the store dedups: messages by (session_id, foreign_id),
// turns and events by position. So the cost of the window is work, not
// duplicate rows — which is the right way round, and the opposite of writing
// the cursor first.
//
// A failed Ingest writes no cursor at all: the counts it managed are
// returned, and the next run re-reads from the watermark it already had.
func (w *Writer) IngestWithCursor(ctx context.Context, th Thread, key, cursor string) (Result, error) {
	// Checked before the ingest, not after: a caller with no key is a
	// caller bug, and doing the whole write first only to refuse means the
	// rows land with no watermark — the one state this method exists to
	// make impossible.
	if key == "" {
		return Result{}, fmt.Errorf("store: ingest with cursor needs an import_state key")
	}
	res, err := w.Ingest(ctx, th)
	if err != nil {
		return res, err
	}
	if err := w.writeImportState(ctx, key, cursor); err != nil {
		return res, err
	}
	return res, nil
}

// writeImportState upserts one import_state row. time_updated is
// milliseconds since the Unix epoch, from the injected clock, the same as
// every other time_* column in the schema.
func (w *Writer) writeImportState(ctx context.Context, key, cursor string) error {
	if _, err := w.db.ExecContext(ctx,
		`INSERT INTO import_state (source, cursor, time_updated) VALUES (?, ?, ?)
		 ON CONFLICT(source) DO UPDATE SET cursor = excluded.cursor, time_updated = excluded.time_updated`,
		key, cursor, w.minter.NowMS()); err != nil {
		return fmt.Errorf("store: write import state %q: %w", key, err)
	}
	return nil
}
