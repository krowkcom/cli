//! os.CreateTemp: a fresh file beside the one it will replace, created
//! exclusively with the given mode, named prefix + random + suffix.

use std::path::{Path, PathBuf};

pub fn create(dir: &Path, prefix: &str, suffix: &str, mode: u32) -> std::io::Result<PathBuf> {
    let _ = mode;
    for attempt in 0..10_000u32 {
        let nanos = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map_or(0, |d| d.subsec_nanos());
        let path = dir.join(format!("{prefix}{}{suffix}", nanos.wrapping_add(attempt.wrapping_mul(7919)) ^ std::process::id()));
        let mut open = std::fs::OpenOptions::new();
        open.write(true).create_new(true);
        #[cfg(unix)]
        std::os::unix::fs::OpenOptionsExt::mode(&mut open, mode);
        match open.open(&path) {
            Ok(_) => return Ok(path),
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(e) => return Err(e),
        }
    }
    Err(std::io::Error::other("could not create a temporary file"))
}
