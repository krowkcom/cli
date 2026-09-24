//! Line-delimited JSON, read from a byte offset so a transcript that grew is
//! read from where the last import stopped — rewound to the start of the line
//! the offset falls in, and to the very start when the file shrank.

use crate::{home_path, Env, ImportError, JsonlCursor, ReadResult, Ref};
use std::io::{BufRead, BufReader, Read, Seek, SeekFrom};

/// A line longer than this is skipped, not read into memory whole.
const MAX_LINE_BYTES: usize = 16 << 20;

/// What a line callback can say: skip this line and why, or stop reading the
/// whole file.
pub enum LineError {
    Skip(String),
    Abort(String),
}

/// Calls `f` with each non-empty, valid JSON line from `cursor` on, and
/// returns the cursor to resume from.
pub fn read_jsonl(
    file: &mut std::fs::File,
    cursor: JsonlCursor,
    mut f: impl FnMut(usize, &[u8]) -> Result<(), LineError>,
) -> Result<(JsonlCursor, ReadResult), ImportError> {
    let mut res = ReadResult::default();
    let size = file.metadata()?.len();
    let start = start_offset(file, cursor, size);
    file.seek(SeekFrom::Start(start))?;
    let mut reader = BufReader::with_capacity(64 << 10, file);
    let (mut offset, mut line_no) = (start, 0usize);
    let at = |offset: u64| JsonlCursor { offset, size: size.max(offset) };
    loop {
        let mut line = Vec::new();
        let (consumed, too_long, terminated) = read_line(&mut reader, &mut line)?;
        if !terminated {
            // No newline: a partial write still being flushed. The offset
            // stays in front of it so a later append completes it — unless
            // it is already past the cap, when nothing will.
            if too_long {
                return Err(ImportError::Other(format!("line {}: line exceeds the maximum length", line_no + 1)));
            }
            break;
        }
        line_no += 1;
        let line_start = offset;
        offset += consumed as u64;
        // A complete line past the cap is skipped, not fatal.
        if too_long {
            res.lines += 1;
            res.skip(line_no, line_start, "line exceeds the maximum length");
            continue;
        }
        let payload = trim_eol(&line);
        // Writers pad transcripts with blank lines; not worth reporting.
        if payload.is_empty() {
            continue;
        }
        res.lines += 1;
        // Validity as Go judges it: invalid UTF-8 inside a string is not a
        // syntax error, it is replaced, so such a line still imports.
        if serde_json::from_str::<serde::de::IgnoredAny>(&String::from_utf8_lossy(payload)).is_err() {
            res.skip(line_no, line_start, "invalid json");
            continue;
        }
        match f(line_no, payload) {
            Ok(()) => {}
            Err(LineError::Skip(why)) => res.skip(line_no, line_start, &why),
            Err(LineError::Abort(why)) => return Err(ImportError::Other(format!("line {line_no}: {why}: abort this file"))),
        }
    }
    Ok((at(offset), res))
}

/// Reads one line into `out`, keeping at most MAX_LINE_BYTES of it; returns
/// the bytes consumed, whether the line was too long to keep, and whether it
/// ended in a newline.
fn read_line(r: &mut impl BufRead, out: &mut Vec<u8>) -> Result<(usize, bool, bool), ImportError> {
    let (mut consumed, mut too_long) = (0usize, false);
    loop {
        let buf = r.fill_buf()?;
        if buf.is_empty() {
            return Ok((consumed, too_long, false));
        }
        let (chunk, done) = match buf.iter().position(|b| *b == b'\n') {
            Some(i) => (&buf[..=i], true),
            None => (buf, false),
        };
        if !too_long {
            if out.len() + chunk.len() > MAX_LINE_BYTES {
                too_long = true;
                out.clear();
            } else {
                out.extend_from_slice(chunk);
            }
        }
        let n = chunk.len();
        r.consume(n);
        consumed += n;
        if done {
            return Ok((consumed, too_long, true));
        }
    }
}

fn trim_eol(line: &[u8]) -> &[u8] {
    let mut end = line.len();
    while end > 0 && matches!(line[end - 1], b'\n' | b'\r') {
        end -= 1;
    }
    &line[..end]
}

/// Where to start: 0 for no cursor or a file that shrank, else the start of
/// the line the offset falls in.
fn start_offset(file: &mut std::fs::File, cursor: JsonlCursor, size: u64) -> u64 {
    if cursor.offset == 0 || cursor.size > size || cursor.offset > size {
        return 0;
    }
    if cursor.offset == size {
        return cursor.offset;
    }
    let mut b = [0u8; 1];
    if file.seek(SeekFrom::Start(cursor.offset - 1)).is_err() || file.read_exact(&mut b).is_err() {
        return 0;
    }
    if b[0] == b'\n' {
        return cursor.offset;
    }
    line_start_before(file, cursor.offset)
}

fn line_start_before(file: &mut std::fs::File, off: u64) -> u64 {
    const CHUNK: u64 = 8 << 10;
    let mut end = off;
    let mut buf = vec![0u8; CHUNK as usize];
    while end > 0 {
        let n = CHUNK.min(end);
        let at = end - n;
        if file.seek(SeekFrom::Start(at)).is_err() || file.read_exact(&mut buf[..n as usize]).is_err() {
            return 0;
        }
        if let Some(i) = buf[..n as usize].iter().rposition(|b| *b == b'\n') {
            return at + i as u64 + 1;
        }
        end = at;
    }
    0
}

/// A JSONL transcript is unchanged when it is still the size its cursor
/// recorded — a sync leaves it unread.
pub fn jsonl_unchanged(env: Env, r: &Ref, cursor: &str) -> bool {
    let Ok(c) = crate::decode_jsonl_cursor(cursor) else { return false };
    if c.offset == 0 {
        return false;
    }
    home_path(env, &r.path).ok().and_then(|p| std::fs::metadata(p).ok()).is_some_and(|m| m.len() == c.size)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn file(body: &str) -> (std::fs::File, std::path::PathBuf) {
        let path = std::env::temp_dir().join(format!("krowk-jsonl-{}-{}", std::process::id(), body.len()));
        std::fs::write(&path, body).unwrap();
        (std::fs::File::open(&path).unwrap(), path)
    }

    #[test]
    fn a_grown_file_resumes_mid_line_at_that_line_and_a_shrunk_one_from_zero() {
        let (mut f, path) = file("{\"a\":1}\n{\"b\":2}\nnot json\n\n{\"c\":3}\n{\"partial");
        let mut seen = Vec::new();
        let (cur, res) = read_jsonl(&mut f, JsonlCursor::default(), |_, l| {
            seen.push(String::from_utf8_lossy(l).into_owned());
            Ok(())
        })
        .unwrap();
        assert_eq!((seen.len(), res.lines, res.skipped_count), (3, 4, 1));
        assert_eq!(cur.offset, cur.size - 9, "an unterminated tail is left for the next read");
        // An offset in the middle of the second line rewinds to its start.
        let mut again = Vec::new();
        read_jsonl(&mut f, JsonlCursor { offset: 10, size: cur.size }, |_, l| {
            again.push(String::from_utf8_lossy(l).into_owned());
            Ok(())
        })
        .unwrap();
        assert_eq!(again[0], "{\"b\":2}");
        let (_, res) = read_jsonl(&mut f, JsonlCursor { offset: 5, size: cur.size + 100 }, |_, _| Ok(())).unwrap();
        assert_eq!(res.lines, 4, "a file smaller than the cursor recorded is read from the start");
        let _ = std::fs::remove_file(path);
    }
}
