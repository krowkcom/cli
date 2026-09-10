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

// ErrAbortFile is what a per-line callback returns to stop the read.
//
// It exists because the polarity here has to be the other way round from
// the obvious one. A callback's ordinary failure is a line it could not make
// sense of — valid JSON in a shape it did not expect, a role it has never
// heard of — and one of those is exactly as survivable as a line that would
// not parse at all: skip it, count it, carry on. Making that fatal instead
// would pin the cursor to the line forever, so a single odd line would cost
// the entire rest of the transcript on every future import.
//
// So a callback error is a skip unless it says otherwise, and ErrAbortFile
// is how it says otherwise: the file is not what it claimed to be, or the
// failure was not the line's fault at all (a full disk, a cancelled
// context). The cursor then stops in front of the line, so a retry
// re-reads it rather than stepping over it.
var ErrAbortFile = errors.New("abort this file")

// ReadJSONL walks the complete lines of f from cur onward, hands each one to
// fn, and returns the watermark to resume from.
//
// Several things make this more than a bufio.Scanner.
//
// A shrunken file is detected and rescanned. If the file is shorter now
// than when the cursor was taken, or the offset is past the end, it was
// rewritten rather than appended to, and nothing before the offset is known
// to be the same bytes any more. Restarting at zero is cheap because the
// store dedups by foreign id, so a rescan converges instead of duplicating.
//
// A file rewritten to the same length or longer is not detected, and is not
// meant to be — see JSONLCursor for why that is the transcripts' bargain
// rather than an oversight.
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
// A line the read cannot use is counted in Result.Skipped with its line
// number and byte offset, and the read continues. That covers a line that
// is not valid JSON, a complete line past maxLineBytes, and a line fn
// itself rejected. All three are the same failure from the cursor's point
// of view: stopping on any of them would pin the cursor to that line's
// start, every retry would fail in the same place, and one bad line would
// cost the whole rest of the file, permanently. One corrupt line in a
// transcript should lose that line, not the session.
//
// Only fn returning ErrAbortFile stops the read, and the two cases where
// the file itself is not line-oriented: an I/O error, and an unterminated
// tail already past maxLineBytes.
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
		payload, consumed, tooLong, err := readLine(reader)
		if err != nil && !errors.Is(err, io.EOF) {
			return JSONLCursor{Offset: offset, Size: size}, res, err
		}
		if errors.Is(err, io.EOF) {
			// Whatever is left has no newline: a partial write. Leave the
			// offset in front of it so a later append completes it.
			if tooLong {
				// Except when the tail is already past the cap. Nothing
				// will complete a line that long, and buffering on in the
				// hope that something does is the exact cost the cap
				// exists to refuse. The cursor still stops in front of it,
				// so a caller that fixes the file resumes cleanly.
				return JSONLCursor{Offset: offset, Size: size}, res, fmt.Errorf("line %d: %w", lineNo+1, errLineTooLong)
			}
			break
		}
		lineNo++
		lineStart := offset
		offset += consumed

		// A complete line past the cap is skipped, not fatal. Failing here
		// would leave the cursor at the line start forever, so one absurd
		// line would mean the rest of the file was never imported again.
		if tooLong {
			res.Lines++
			res.Skip(lineNo, lineStart, errLineTooLong.Error())
			continue
		}
		// An empty line is not a failure worth reporting: writers pad
		// transcripts with them, and a "skipped" count full of blanks
		// would bury the lines that actually failed.
		if len(payload) == 0 {
			continue
		}
		res.Lines++
		if !json.Valid(payload) {
			res.Skip(lineNo, lineStart, "invalid json")
			continue
		}
		if ferr := fn(lineNo, payload); ferr != nil {
			if errors.Is(ferr, ErrAbortFile) {
				// The cursor stops at the last line the callback accepted,
				// so a retry re-reads this one rather than stepping over
				// the failure.
				return JSONLCursor{Offset: lineStart, Size: size}, res, fmt.Errorf("line %d: %w", lineNo, ferr)
			}
			res.Skip(lineNo, lineStart, ferr.Error())
			continue
		}
	}

	return JSONLCursor{Offset: offset, Size: size}, res, nil
}

// errLineTooLong is a line past maxLineBytes. A complete one is skipped
// like any other unusable line; an unterminated tail that is already that
// long stops the read, because a file whose lines do not end is not a JSONL
// file and buffering on would be buffering noise.
var errLineTooLong = errors.New("line exceeds the maximum length")

// startOffset decides where a read begins: the cursor, zero if the cursor
// cannot be trusted, or the start of the line the cursor landed inside.
func startOffset(f *os.File, cur JSONLCursor, size int64) int64 {
	if cur.Offset <= 0 {
		return 0
	}
	// Shorter than when the watermark was taken, or a watermark past the
	// end: the file was rewritten, so nothing before the offset is known to
	// be the same bytes any more. A rewrite that left the file the same
	// length or longer is invisible here, by the design JSONLCursor
	// documents.
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

// readLine reads one line and reports three things: the payload without its
// terminator, how many bytes were consumed (which is what the byte offset
// advances by, so the terminator counts), and whether the line ran past
// maxLineBytes.
//
// An over-long line is consumed to its end and its payload dropped rather
// than returned: the caller cannot use it, and holding on to it is the
// unbounded allocation the cap is there to prevent. io.EOF with bytes in
// hand means an unterminated trailing line, which the caller declines to
// consume.
func readLine(r *bufio.Reader) (payload []byte, consumed int64, tooLong bool, err error) {
	for {
		chunk, rerr := r.ReadSlice('\n')
		consumed += int64(len(chunk))
		if !tooLong {
			// Tested before the append, not after: appending first would
			// let the buffer peak at a chunk past the cap, which is the
			// allocation the cap exists to bound.
			if int64(len(payload)+len(chunk)) > maxLineBytes {
				tooLong = true
				payload = nil
			} else {
				payload = append(payload, chunk...)
			}
		}
		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue
		}
		if rerr != nil {
			return payload, consumed, tooLong, rerr
		}
		return trimEOL(payload), consumed, tooLong, nil
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
