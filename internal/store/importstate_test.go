package store

import (
	"context"
	"testing"
	"time"
)

// A key nobody has imported reads as the empty cursor rather than as an
// error, which is what lets a caller decode it straight into a zero
// watermark.
func TestReadImportStateIsEmptyBeforeAnyImport(t *testing.T) {
	db, _ := openWriterDB(t, nil)

	cursor, err := ReadImportState(context.Background(), db, "claude:never-seen")
	if err != nil {
		t.Fatalf("ReadImportState: %v", err)
	}
	if cursor != "" {
		t.Errorf("cursor = %q, want empty", cursor)
	}
}

// IngestWithCursor writes the rows and the watermark, and the watermark is
// readable straight back.
func TestIngestWithCursorStoresTheWatermark(t *testing.T) {
	db, w := openWriterDB(t, nil)
	ctx := context.Background()
	const key = "claude:s1"

	res, err := w.IngestWithCursor(ctx, sampleThread("anthropic", "s1"), key, `{"offset":42,"size":42}`)
	if err != nil {
		t.Fatalf("IngestWithCursor: %v", err)
	}
	if res.Messages.Inserted != 3 {
		t.Errorf("messages inserted = %d, want 3", res.Messages.Inserted)
	}
	cursor, err := ReadImportState(ctx, db, key)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != `{"offset":42,"size":42}` {
		t.Errorf("cursor = %q", cursor)
	}

	var updated int64
	if err := db.QueryRow(`SELECT time_updated FROM import_state WHERE source = ?`, key).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if updated != frozenTime.UnixMilli() {
		t.Errorf("time_updated = %d, want the injected clock's %d milliseconds",
			updated, frozenTime.UnixMilli())
	}
}

// A second call moves the cursor and time_updated and leaves one row: the
// key is the primary key, so a re-import must update rather than accumulate.
func TestIngestWithCursorUpsertsOneRowPerSource(t *testing.T) {
	later := frozenTime.Add(time.Minute)
	db, w := openWriterDB(t, nil)
	ctx := context.Background()
	const key = "claude:s1"
	th := sampleThread("anthropic", "s1")

	if _, err := w.IngestWithCursor(ctx, th, key, `{"offset":1,"size":1}`); err != nil {
		t.Fatal(err)
	}
	// A second writer on a later clock, which is what a second run is.
	w2 := NewWriter(db, func() time.Time { return later })
	res, err := w2.IngestWithCursor(ctx, th, key, `{"offset":2,"size":2}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages.Inserted != 0 {
		t.Errorf("a re-ingest inserted %d messages, want 0", res.Messages.Inserted)
	}

	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM import_state`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("import_state holds %d rows for one source", rows)
	}
	var cursor string
	var updated int64
	if err := db.QueryRow(`SELECT cursor, time_updated FROM import_state WHERE source = ?`, key).
		Scan(&cursor, &updated); err != nil {
		t.Fatal(err)
	}
	if cursor != `{"offset":2,"size":2}` {
		t.Errorf("cursor = %q, want the second one", cursor)
	}
	if updated != later.UnixMilli() {
		t.Errorf("time_updated = %d, want %d", updated, later.UnixMilli())
	}
}

// A failed Ingest writes no cursor: the next run has to re-read from the
// watermark it already had, not from one describing rows that never landed.
func TestIngestWithCursorWritesNoCursorWhenTheIngestFails(t *testing.T) {
	db, w := openWriterDB(t, nil)
	ctx := context.Background()
	th := sampleThread("anthropic", "s1")
	th.Messages[0].Role = "nonsense"

	if _, err := w.IngestWithCursor(ctx, th, "claude:s1", `{"offset":7,"size":7}`); err == nil {
		t.Fatal("a thread with a bad role ingested")
	}
	cursor, err := ReadImportState(ctx, db, "claude:s1")
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "" {
		t.Errorf("a failed ingest stored cursor %q", cursor)
	}
}

// An empty key is refused rather than writing every source's cursor into one
// row named "".
func TestIngestWithCursorNeedsAKey(t *testing.T) {
	_, w := openWriterDB(t, nil)
	if _, err := w.IngestWithCursor(context.Background(), sampleThread("anthropic", "s1"), "", "{}"); err == nil {
		t.Fatal("an empty import_state key was accepted")
	}
}
