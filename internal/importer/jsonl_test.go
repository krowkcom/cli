package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile lays down a transcript and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// readAll collects every line ReadJSONL hands out.
func readAll(t *testing.T, path string, cur JSONLCursor) ([]string, JSONLCursor, Result) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	var got []string
	next, res, err := ReadJSONL(f, cur, func(_ int, line []byte) error {
		got = append(got, string(line))
		return nil
	})
	if err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	return got, next, res
}

func TestReadJSONLResumesFromCursor(t *testing.T) {
	path := writeFile(t, "a.jsonl", "{\"i\":1}\n{\"i\":2}\n")
	got, cur, _ := readAll(t, path, JSONLCursor{})
	if len(got) != 2 {
		t.Fatalf("first read got %d lines, want 2", len(got))
	}
	if cur.Offset != 16 || cur.Size != 16 {
		t.Fatalf("cursor = %+v, want {16 16}", cur)
	}
	// Nothing appended: a resumed read is empty and the cursor stands.
	got, cur2, _ := readAll(t, path, cur)
	if len(got) != 0 {
		t.Fatalf("resumed read got %v, want nothing", got)
	}
	if cur2 != cur {
		t.Fatalf("cursor moved on an unchanged file: %+v", cur2)
	}
	// Append and the resumed read sees only the new line.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString("{\"i\":3}\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()
	got, _, _ = readAll(t, path, cur)
	if len(got) != 1 || got[0] != `{"i":3}` {
		t.Fatalf("after append got %v, want the third line only", got)
	}
}

// Acceptance: a stale cursor whose size exceeds the current file size
// restarts from offset 0.
func TestReadJSONLStaleCursorAfterTruncateRestarts(t *testing.T) {
	path := writeFile(t, "a.jsonl", "{\"i\":1}\n{\"i\":2}\n{\"i\":3}\n")
	_, cur, _ := readAll(t, path, JSONLCursor{})
	if cur.Offset == 0 {
		t.Fatal("first read left a zero cursor")
	}

	// The file is rewritten shorter between reads: the same path, different
	// bytes, and the old offset now points into content it never saw.
	if err := os.WriteFile(path, []byte("{\"i\":9}\n"), 0o600); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	got, next, res := readAll(t, path, cur)
	if len(got) != 1 || got[0] != `{"i":9}` {
		t.Fatalf("got %v, want a full rescan of the shortened file", got)
	}
	if next.Offset != 8 || next.Size != 8 {
		t.Fatalf("cursor = %+v, want {8 8}", next)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("rescan skipped %v, want nothing", res.Skipped)
	}
}

func TestReadJSONLOffsetPastEndRestarts(t *testing.T) {
	path := writeFile(t, "a.jsonl", "{\"i\":1}\n")
	// Size agrees with the file but the offset is past the end: also a
	// rewrite, and also a rescan.
	got, _, _ := readAll(t, path, JSONLCursor{Offset: 999, Size: 8})
	if len(got) != 1 {
		t.Fatalf("got %v, want a rescan", got)
	}
}

// Acceptance: a read resumed mid-line re-reads from the last complete line
// and no half-line parse error surfaces.
func TestReadJSONLMidLineCursorRewinds(t *testing.T) {
	content := "{\"i\":1}\n{\"i\":2}\n{\"i\":3}\n"
	path := writeFile(t, "a.jsonl", content)
	for _, off := range []int64{9, 10, 15} { // inside the second line
		got, next, res := readAll(t, path, JSONLCursor{Offset: off, Size: int64(len(content))})
		if len(res.Skipped) != 0 {
			t.Fatalf("offset %d: skipped %v, want no half-line errors", off, res.Skipped)
		}
		want := []string{`{"i":2}`, `{"i":3}`}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("offset %d: got %v, want %v", off, got, want)
		}
		if next.Offset != int64(len(content)) {
			t.Fatalf("offset %d: next cursor %+v, want offset %d", off, next, len(content))
		}
	}
}

func TestReadJSONLMidFirstLineRestartsAtZero(t *testing.T) {
	content := "{\"i\":1}\n{\"i\":2}\n"
	path := writeFile(t, "a.jsonl", content)
	got, _, res := readAll(t, path, JSONLCursor{Offset: 3, Size: int64(len(content))})
	if len(got) != 2 {
		t.Fatalf("got %v, want both lines", got)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("skipped %v, want nothing", res.Skipped)
	}
}

// Acceptance: a line that fails JSON decoding is counted in Result.Skipped
// with its line number and never aborts the file.
func TestReadJSONLBadLineIsSkippedNotFatal(t *testing.T) {
	path := writeFile(t, "a.jsonl", "{\"i\":1}\nnot json at all\n{\"i\":3}\n\n{\"i\":5}\n")
	got, next, res := readAll(t, path, JSONLCursor{})
	if len(got) != 3 {
		t.Fatalf("got %v, want the three valid lines", got)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("Skipped = %+v, want exactly the bad line", res.Skipped)
	}
	if res.Skipped[0].Line != 2 {
		t.Fatalf("Skipped[0].Line = %d, want 2", res.Skipped[0].Line)
	}
	if res.Skipped[0].Offset != 8 {
		t.Fatalf("Skipped[0].Offset = %d, want 8", res.Skipped[0].Offset)
	}
	if res.Skipped[0].Reason == "" {
		t.Fatal("Skipped[0].Reason is empty, want a reason")
	}
	// The blank line is not a skip, and the whole file was still consumed.
	if res.Lines != 4 {
		t.Fatalf("Lines = %d, want 4 non-blank lines", res.Lines)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if next.Offset != info.Size() {
		t.Fatalf("cursor stopped at %d, want the whole file (%d)", next.Offset, info.Size())
	}
}

// The callback's polarity is the point of this test: an ordinary error is a
// skip, because a line the callback could not make sense of is exactly as
// survivable as one that would not parse, and only ErrAbortFile stops the
// file.
func TestReadJSONLCallbackErrorIsSkippedNotFatal(t *testing.T) {
	// The middle line is valid JSON in a shape a source would reject: the
	// case that used to pin the cursor forever.
	path := writeFile(t, "a.jsonl", "{\"i\":1}\n{\"role\":5}\n{\"i\":3}\n")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	var seen []int
	cur, res, err := ReadJSONL(f, JSONLCursor{}, func(lineNo int, line []byte) error {
		var probe struct {
			Role string `json:"role"`
		}
		if uerr := json.Unmarshal(line, &probe); uerr != nil {
			return uerr
		}
		seen = append(seen, lineNo)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 3 {
		t.Fatalf("callback saw lines %v, want 1 and 3", seen)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Line != 2 {
		t.Fatalf("Skipped = %+v, want the wrong-shape line at line 2", res.Skipped)
	}
	if res.Skipped[0].Offset != 8 {
		t.Fatalf("Skipped[0].Offset = %d, want 8", res.Skipped[0].Offset)
	}
	if res.Skipped[0].Reason == "" {
		t.Fatal("Skipped[0].Reason is empty, want the callback's own error")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if cur.Offset != info.Size() {
		t.Fatalf("cursor = %+v, want the whole file (%d)", cur, info.Size())
	}
}

// ErrAbortFile is the escape hatch: the file stops, and the cursor stops in
// front of the offending line so a retry re-reads it.
func TestReadJSONLCallbackAbortStopsWithCursorBeforeLine(t *testing.T) {
	path := writeFile(t, "a.jsonl", "{\"i\":1}\n{\"i\":2}\n{\"i\":3}\n")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	cur, res, err := ReadJSONL(f, JSONLCursor{}, func(lineNo int, _ []byte) error {
		if lineNo == 2 {
			return fmt.Errorf("disk full: %w", ErrAbortFile)
		}
		return nil
	})
	if !errors.Is(err, ErrAbortFile) {
		t.Fatalf("err = %v, want ErrAbortFile", err)
	}
	if cur.Offset != 8 {
		t.Fatalf("cursor = %+v, want offset 8 (in front of line 2)", cur)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("Skipped = %+v, want nothing: an abort is not a skip", res.Skipped)
	}
	// The line that aborted is re-read from the returned cursor rather than
	// stepped over.
	got, _, _ := readAll(t, path, cur)
	if len(got) != 2 || got[0] != `{"i":2}` {
		t.Fatalf("retry got %v, want line 2 onwards", got)
	}
}

// A half-flushed trailing line is left for the next read, not parsed.
func TestReadJSONLTrailingPartialLineNotConsumed(t *testing.T) {
	path := writeFile(t, "a.jsonl", "{\"i\":1}\n{\"i\":2")
	got, cur, res := readAll(t, path, JSONLCursor{})
	if len(got) != 1 {
		t.Fatalf("got %v, want the complete line only", got)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("Skipped = %+v, want nothing: a partial write is not a bad line", res.Skipped)
	}
	if cur.Offset != 8 {
		t.Fatalf("cursor = %+v, want offset 8 so the partial line completes later", cur)
	}

	// The writer finishes the line; the next read picks it up whole.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, err := f.WriteString("}\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()
	got, _, res = readAll(t, path, cur)
	if len(got) != 1 || got[0] != `{"i":2}` {
		t.Fatalf("got %v, want the completed line", got)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("Skipped = %+v, want nothing", res.Skipped)
	}
}

func TestReadJSONLEmptyFile(t *testing.T) {
	path := writeFile(t, "a.jsonl", "")
	got, cur, res := readAll(t, path, JSONLCursor{})
	if len(got) != 0 || cur.Offset != 0 || cur.Size != 0 || res.Lines != 0 {
		t.Fatalf("empty file: got %v cur %+v res %+v", got, cur, res)
	}
}

func TestReadJSONLCarriageReturnsTrimmed(t *testing.T) {
	path := writeFile(t, "a.jsonl", "{\"i\":1}\r\n")
	got, _, res := readAll(t, path, JSONLCursor{})
	if len(res.Skipped) != 0 {
		t.Fatalf("Skipped = %+v, want nothing", res.Skipped)
	}
	if len(got) != 1 || got[0] != `{"i":1}` {
		t.Fatalf("got %q, want the line without its CR", got)
	}
}

// A complete line past maxLineBytes is a skip, not a wall: the read must get
// past it, or one absurd line would cost the whole rest of the file on every
// retry from then on.
func TestReadJSONLOverLongLineIsSkippedAndReadContinues(t *testing.T) {
	long := `{"pad":"` + strings.Repeat("x", 17<<20) + `"}`
	content := "{\"i\":1}\n" + long + "\n{\"i\":3}\n"
	path := writeFile(t, "long.jsonl", content)

	got, cur, res := readAll(t, path, JSONLCursor{})
	want := []string{`{"i":1}`, `{"i":3}`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %d lines (%v), want both short lines", len(got), got)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Line != 2 {
		t.Fatalf("Skipped = %+v, want the long line at line 2", res.Skipped)
	}
	if res.Skipped[0].Offset != 8 {
		t.Fatalf("Skipped[0].Offset = %d, want 8", res.Skipped[0].Offset)
	}
	// The cursor is past the whole file, so the next read is empty rather
	// than stuck on the same line.
	if cur.Offset != int64(len(content)) {
		t.Fatalf("cursor = %+v, want offset %d", cur, len(content))
	}
	got, _, res = readAll(t, path, cur)
	if len(got) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("resumed read got %v / %+v, want nothing", got, res.Skipped)
	}
}

// An unterminated tail already past the cap is the one fatal case: nothing
// is going to complete a line that long, and buffering on is the cost the
// cap refuses. The cursor still stops in front of it.
func TestReadJSONLOverLongUnterminatedTailIsFatal(t *testing.T) {
	path := writeFile(t, "tail.jsonl", "{\"i\":1}\n{\"pad\":\""+strings.Repeat("x", 17<<20))
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	cur, res, err := ReadJSONL(f, JSONLCursor{}, func(_ int, _ []byte) error { return nil })
	if err == nil {
		t.Fatal("ReadJSONL succeeded, want a refusal")
	}
	if !strings.Contains(err.Error(), "maximum length") {
		t.Fatalf("err = %v, want the line-length refusal", err)
	}
	if cur.Offset != 8 {
		t.Fatalf("cursor = %+v, want offset 8 (in front of the tail)", cur)
	}
	if res.Lines != 1 {
		t.Fatalf("Lines = %d, want the one good line", res.Lines)
	}
}

// A corrupt file must be reported, not survived at any cost: a Result that
// grew a struct per bad line would turn a 64 MiB file of junk into an
// out-of-memory failure, which is a much worse way to say "this file is
// junk" than a number is.
func TestReadJSONLManyBadLinesAreCountedNotAccumulated(t *testing.T) {
	const bad = 1000
	var b strings.Builder
	// Each reason will be the callback's error carrying the line, so this
	// also exercises the reason bound.
	long := strings.Repeat("q", 4096)
	for i := 0; i < bad; i++ {
		b.WriteString("not json " + long + "\n")
	}
	b.WriteString("{\"i\":1}\n")
	path := writeFile(t, "junk.jsonl", b.String())

	got, cur, res := readAll(t, path, JSONLCursor{})
	if len(got) != 1 {
		t.Fatalf("got %v, want the one good line", got)
	}
	if res.SkippedCount != bad {
		t.Fatalf("SkippedCount = %d, want %d", res.SkippedCount, bad)
	}
	if len(res.Skipped) != maxSkippedRetained {
		t.Fatalf("len(Skipped) = %d, want the retained sample of %d", len(res.Skipped), maxSkippedRetained)
	}
	// The retained entries are the first ones, and they still carry the
	// line number the acceptance bullet asks for.
	for i, sl := range res.Skipped {
		if sl.Line != i+1 {
			t.Fatalf("Skipped[%d].Line = %d, want %d", i, sl.Line, i+1)
		}
		if len(sl.Reason) > maxSkipReasonBytes {
			t.Fatalf("Skipped[%d].Reason is %d bytes, want it bounded", i, len(sl.Reason))
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if cur.Offset != info.Size() {
		t.Fatalf("cursor = %+v, want the whole file (%d)", cur, info.Size())
	}
}

// A returned cursor whose Size was below its Offset would look, on the next
// read, exactly like the shrink that triggers a full rescan — so it is an
// invariant, not an accident of when the stat happened.
func TestReadJSONLCursorSizeNeverBelowOffset(t *testing.T) {
	path := writeFile(t, "a.jsonl", "{\"i\":1}\n{\"i\":2}\n")
	_, cur, _ := readAll(t, path, JSONLCursor{})
	if cur.Size < cur.Offset {
		t.Fatalf("cursor = %+v, want Size >= Offset", cur)
	}
	// And the invariant holds however the offset was arrived at, including
	// a read the callback aborted partway.
	if got := cursorAt(500, 100); got.Size != 500 {
		t.Fatalf("cursorAt(500, 100) = %+v, want Size raised to the offset", got)
	}
}
