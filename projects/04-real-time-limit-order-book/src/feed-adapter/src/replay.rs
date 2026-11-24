//! Capture files and their manifests.
//!
//! A capture is newline-delimited JSON, one [`Frame`] per line: the venue's
//! raw bytes plus the local receive timestamp. Keeping both lets a replay feed
//! the pipeline exactly what the live session saw, in the same order and with
//! the same receive times, which is what makes replays reproducible.

use crate::sim::{Checkpoint, Frame};
use crate::wire::Instrument;
use serde::{Deserialize, Serialize};
use std::io::{self, BufRead, Write};
use std::path::{Path, PathBuf};

pub const MANIFEST_VERSION: u32 = 1;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Manifest {
    pub manifest_version: u32,
    pub wire_schema: String,
    /// Always `"simulated"` for captures produced by this repository.
    pub data_origin: String,
    pub instruments: Vec<Instrument>,
    pub seed: u64,
    pub events: u64,
    pub faults: String,
    pub frames: u64,
    /// Digest of everything the engine published while the capture was made.
    /// A replay must reproduce it exactly.
    pub output_digest: String,
    pub checkpoint_depth: usize,
    pub checkpoints: Vec<Checkpoint>,
}

/// `foo.ndjson` → `foo.manifest.json`.
pub fn manifest_path(capture: &Path) -> PathBuf {
    capture.with_extension("manifest.json")
}

pub fn write_frame(w: &mut impl Write, frame: &Frame) -> io::Result<()> {
    serde_json::to_writer(&mut *w, frame)?;
    w.write_all(b"\n")
}

pub fn read_capture(path: &Path) -> io::Result<Vec<Frame>> {
    let file = std::fs::File::open(path)?;
    let mut frames = Vec::new();
    for (i, line) in io::BufReader::new(file).lines().enumerate() {
        let line = line?;
        if line.trim().is_empty() {
            continue;
        }
        let frame: Frame = serde_json::from_str(&line).map_err(|e| {
            io::Error::new(
                io::ErrorKind::InvalidData,
                format!("{}:{}: {e}", path.display(), i + 1),
            )
        })?;
        frames.push(frame);
    }
    Ok(frames)
}

pub fn read_manifest(path: &Path) -> io::Result<Manifest> {
    let text = std::fs::read_to_string(path)?;
    serde_json::from_str(&text).map_err(|e| {
        io::Error::new(
            io::ErrorKind::InvalidData,
            format!("{}: {e}", path.display()),
        )
    })
}

pub fn write_manifest(path: &Path, manifest: &Manifest) -> io::Result<()> {
    let mut text = serde_json::to_string_pretty(manifest)?;
    text.push('\n');
    std::fs::write(path, text)
}
