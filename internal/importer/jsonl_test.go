package importer

import (
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

// A callback returning ErrSkipLine is a skip, not a failure — that is how a
// source declines a line type it does not import while still accounting for
// it.
func TestReadJSONLCallbackSkipAndFail(t *testing.T) {
	path := writeFile(t, "a.jsonl", "{\"i\":1}\n{\"i\":2}\n")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	_, res, err := ReadJSONL(f, JSONLCursor{}, func(lineNo int, _ []byte) error {
		if lineNo == 1 {
			return fmt.Errorf("uninteresting: %w", ErrSkipLine)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ReadJSONL: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Line != 1 {
		t.Fatalf("Skipped = %+v, want line 1", res.Skipped)
	}

	// Any other callback error stops the file, with the cursor left in
	// front of the offending line so a retry re-reads it.
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	boom := errors.New("boom")
	cur, _, err := ReadJSONL(f, JSONLCursor{}, func(lineNo int, _ []byte) error {
		if lineNo == 2 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if cur.Offset != 8 {
		t.Fatalf("cursor = %+v, want offset 8 (in front of line 2)", cur)
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
