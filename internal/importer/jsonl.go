package importer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// maxLineBytes bounds one JSONL line. Claude writes a whole tool result onto
// one line, so the bound has to be generous; it exists so a file that is not
// really line-oriented cannot be read into memory as a single "line".
const maxLineBytes = 16 << 20

// ErrSkipLine is what a per-line callback returns to say "this line is not
// for me" without failing the file. Anything else it returns stops the read
// — a callback that cannot cope is different from a line that cannot parse.
var ErrSkipLine = errors.New("skip this line")

// ReadJSONL walks the complete lines of f from cur onward, hands each one to
// fn, and returns the watermark to resume from.
//
// Three things make this more than a bufio.Scanner.
//
// A stale cursor is detected and ignored. If the file is shorter now than
// when the cursor was taken, or the offset is past the end, the file was
// rewritten rather than appended to and the read restarts at zero. Resuming
// into a rewritten file would silently import the wrong bytes, which is a
// worse failure than re-importing the right ones — the store dedups by
// foreign id, so a rescan converges.
//
// A cursor that landed mid-line is backed up. Nothing in this package ever
// takes such a cursor, but a crash, a hand-edited row or an older build
// could leave one, and seeking to it would hand fn half a JSON object. The
// read rewinds to the start of the line the offset sits in and re-reads it
// whole; the store's foreign-id dedup absorbs the repeat.
//
// A trailing line with no newline is not consumed. A transcript is written
// by a process still running: the last line may be half-flushed, and the
// returned offset stops before it so the next read sees it complete rather
// than parsing a truncated object and recording a skip that was never a
// real error.
//
// A line that is not valid JSON is counted in Result.Skipped with its line
// number and byte offset, and the read continues. One corrupt line in a
// transcript loses that line, not the session.
func ReadJSONL(f *os.File, cur JSONLCursor, fn func(lineNo int, line []byte) error) (JSONLCursor, Result, error) {
	var res Result

	info, err := f.Stat()
	if err != nil {
		return cur, res, err
	}
	size := info.Size()

	start := startOffset(f, cur, size)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return cur, res, err
	}

	reader := bufio.NewReaderSize(f, 64<<10)
	offset := start
	lineNo := 0

	for {
		line, err := readLine(reader)
		if len(line) > 0 && err == nil {
			lineNo++
			lineStart := offset
			offset += int64(len(line))
			payload := trimEOL(line)
			// An empty line is not a failure worth reporting: writers pad
			// transcripts with them, and a "skipped" count full of blanks
			// would bury the lines that actually failed.
			if len(payload) > 0 {
				res.Lines++
				if !json.Valid(payload) {
					res.Skip(lineNo, lineStart, "invalid json")
					continue
				}
				if ferr := fn(lineNo, payload); ferr != nil {
					if errors.Is(ferr, ErrSkipLine) {
						res.Skip(lineNo, lineStart, ferr.Error())
						continue
					}
					// The cursor returned stops at the last line the
					// callback accepted, so a retry re-reads this one
					// rather than skipping past the failure.
					return JSONLCursor{Offset: lineStart, Size: size}, res, fmt.Errorf("line %d: %w", lineNo, ferr)
				}
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Whatever is left has no newline: a partial write. Leave
				// the offset in front of it.
				break
			}
			if errors.Is(err, bufio.ErrBufferFull) {
				return JSONLCursor{Offset: offset, Size: size}, res, fmt.Errorf("line %d: %w", lineNo+1, errLineTooLong)
			}
			return JSONLCursor{Offset: offset, Size: size}, res, err
		}
		break
	}

	return JSONLCursor{Offset: offset, Size: size}, res, nil
}

// errLineTooLong is a line past maxLineBytes. It stops the file rather than
// skipping the line, because a file whose lines do not end is not a JSONL
// file and reading on would be reading noise.
var errLineTooLong = errors.New("line exceeds the maximum length")

// startOffset decides where a read begins: the cursor, zero if the cursor
// cannot be trusted, or the start of the line the cursor landed inside.
func startOffset(f *os.File, cur JSONLCursor, size int64) int64 {
	if cur.Offset <= 0 {
		return 0
	}
	// Shorter than when the watermark was taken, or a watermark past the
	// end: the file was rewritten, so nothing before the offset is known to
	// be the same bytes any more.
	if cur.Size > size || cur.Offset > size {
		return 0
	}
	if cur.Offset == size {
		return cur.Offset
	}
	// A cursor this package took always sits just after a newline. One that
	// does not came from somewhere else; back up to the start of its line
	// rather than parse from its middle.
	var b [1]byte
	if _, err := f.ReadAt(b[:], cur.Offset-1); err != nil {
		return 0
	}
	if b[0] == '\n' {
		return cur.Offset
	}
	return lineStartBefore(f, cur.Offset)
}

// lineStartBefore scans backwards from off for the byte after the previous
// newline, so a mid-line resume re-reads that whole line. No newline before
// it means the offset was inside the first line, and the read starts at zero.
func lineStartBefore(f *os.File, off int64) int64 {
	const chunk = 8 << 10
	buf := make([]byte, chunk)
	end := off
	for end > 0 {
		n := int64(chunk)
		if end < n {
			n = end
		}
		readAt := end - n
		if _, err := f.ReadAt(buf[:n], readAt); err != nil && !errors.Is(err, io.EOF) {
			return 0
		}
		for i := n - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				return readAt + i + 1
			}
		}
		end = readAt
	}
	return 0
}

// readLine returns one line including its newline, so the caller can advance
// the byte offset by exactly what it consumed. io.EOF with bytes in hand
// means an unterminated trailing line, which the caller declines to consume.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			line = append(line, chunk...)
			if len(line) > maxLineBytes {
				return line, bufio.ErrBufferFull
			}
			continue
		}
		line = append(line, chunk...)
		if err != nil {
			return line, err
		}
		if len(line) > maxLineBytes {
			return line, bufio.ErrBufferFull
		}
		return line, nil
	}
}

// trimEOL strips the line terminator, including the carriage return a
// transcript written on Windows carries.
func trimEOL(line []byte) []byte {
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line
}
