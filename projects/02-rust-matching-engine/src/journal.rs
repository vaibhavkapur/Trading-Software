//! `journal`: append-only command/event logs, snapshots and replay.
//!
//! Logs are JSON Lines. A record is written with a single `write` call
//! including its trailing newline, so after a crash the only possible damage
//! is a torn final line, which readers detect and drop. Damage anywhere else
//! is reported as corruption rather than skipped.

use crate::matching_core::{Engine, EngineSnapshot, Event, Seq, SequenceError, SequencedCommand};
use serde::de::DeserializeOwned;
use serde::Serialize;
use std::fmt;
use std::fs::{self, File, OpenOptions};
use std::io::{self, Write};
use std::path::{Path, PathBuf};

pub const COMMANDS_FILE: &str = "commands.jsonl";
pub const EVENTS_FILE: &str = "events.jsonl";
pub const SNAPSHOT_DIR: &str = "snapshots";

#[derive(Debug)]
pub enum JournalError {
    Io(io::Error),
    Corrupt {
        path: PathBuf,
        line: usize,
        message: String,
    },
    Sequence(SequenceError),
    Snapshot(String),
}

impl fmt::Display for JournalError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            JournalError::Io(e) => write!(f, "journal I/O error: {e}"),
            JournalError::Corrupt {
                path,
                line,
                message,
            } => write!(f, "corrupt log {}:{line}: {message}", path.display()),
            JournalError::Sequence(e) => write!(f, "{e}"),
            JournalError::Snapshot(m) => write!(f, "invalid snapshot: {m}"),
        }
    }
}

impl std::error::Error for JournalError {}

impl From<io::Error> for JournalError {
    fn from(e: io::Error) -> Self {
        JournalError::Io(e)
    }
}

impl From<SequenceError> for JournalError {
    fn from(e: SequenceError) -> Self {
        JournalError::Sequence(e)
    }
}

/// A byte sink that can be forced to stable storage.
pub trait Durable: Write + Send {
    fn sync(&mut self) -> io::Result<()> {
        Ok(())
    }
}

impl Durable for File {
    fn sync(&mut self) -> io::Result<()> {
        self.sync_data()
    }
}

impl Durable for Vec<u8> {}
impl Durable for io::Sink {}

/// Appends serialisable records as JSON Lines.
pub struct JsonlWriter {
    sink: Box<dyn Durable>,
    buf: Vec<u8>,
}

impl JsonlWriter {
    pub fn new(sink: Box<dyn Durable>) -> Self {
        JsonlWriter {
            sink,
            buf: Vec::with_capacity(4096),
        }
    }

    pub fn open_append(path: &Path) -> io::Result<Self> {
        let file = OpenOptions::new().create(true).append(true).open(path)?;
        Ok(Self::new(Box::new(file)))
    }

    /// Writes one record with a single `write_all`.
    pub fn append<T: Serialize>(&mut self, record: &T) -> io::Result<()> {
        self.append_all(std::slice::from_ref(record))
    }

    /// Writes a batch of records with a single `write_all`.
    pub fn append_all<T: Serialize>(&mut self, records: &[T]) -> io::Result<()> {
        if records.is_empty() {
            return Ok(());
        }
        self.buf.clear();
        for record in records {
            serde_json::to_writer(&mut self.buf, record).map_err(io::Error::other)?;
            self.buf.push(b'\n');
        }
        self.sink.write_all(&self.buf)
    }

    pub fn sync(&mut self) -> io::Result<()> {
        self.sink.flush()?;
        self.sink.sync()
    }
}

/// Result of reading a JSON Lines log.
#[derive(Debug)]
pub struct LogContents<T> {
    pub records: Vec<T>,
    /// Byte length of the valid prefix (everything up to the last newline).
    pub valid_len: u64,
    /// True if a partial final line was found (and ignored).
    pub torn_tail: bool,
}

/// Reads a whole log. A missing file is an empty log.
pub fn read_jsonl<T: DeserializeOwned>(path: &Path) -> Result<LogContents<T>, JournalError> {
    let bytes = match fs::read(path) {
        Ok(b) => b,
        Err(e) if e.kind() == io::ErrorKind::NotFound => Vec::new(),
        Err(e) => return Err(e.into()),
    };
    let valid_len = bytes.iter().rposition(|b| *b == b'\n').map_or(0, |i| i + 1);
    let body = &bytes[..valid_len];
    let body = body.strip_suffix(b"\n").unwrap_or(body);
    let mut records = Vec::new();
    if !body.is_empty() {
        for (i, line) in body.split(|b| *b == b'\n').enumerate() {
            let record = serde_json::from_slice(line).map_err(|e| JournalError::Corrupt {
                path: path.to_path_buf(),
                line: i + 1,
                message: e.to_string(),
            })?;
            records.push(record);
        }
    }
    Ok(LogContents {
        records,
        valid_len: valid_len as u64,
        torn_tail: valid_len != bytes.len(),
    })
}

/// Reads a log and, if it ends in a torn line, truncates the file back to its
/// valid prefix so that it can be appended to again.
pub fn read_and_repair<T: DeserializeOwned>(path: &Path) -> Result<LogContents<T>, JournalError> {
    let contents = read_jsonl(path)?;
    if contents.torn_tail {
        let file = OpenOptions::new().write(true).open(path)?;
        file.set_len(contents.valid_len)?;
        file.sync_all()?;
    }
    Ok(contents)
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct ReplayStats {
    pub applied: u64,
    /// Records at or below the engine's sequence (already applied, e.g.
    /// covered by a snapshot or delivered twice).
    pub skipped: u64,
}

/// Feeds sequenced commands into an engine. Records the engine has already
/// applied are skipped (idempotent redelivery); a gap is an error.
pub fn replay<'a>(
    engine: &mut Engine,
    commands: impl IntoIterator<Item = &'a SequencedCommand>,
    mut on_event: impl FnMut(&Event),
) -> Result<ReplayStats, SequenceError> {
    let mut stats = ReplayStats::default();
    let mut out = Vec::new();
    for sc in commands {
        if sc.seq <= engine.last_seq() {
            stats.skipped += 1;
            continue;
        }
        out.clear();
        engine.process(sc, &mut out)?;
        out.iter().for_each(&mut on_event);
        stats.applied += 1;
    }
    Ok(stats)
}

fn snapshot_path(dir: &Path, seq: Seq) -> PathBuf {
    dir.join(format!("snapshot-{seq:020}.json"))
}

/// Writes a snapshot atomically (temp file, fsync, rename) and returns its path.
pub fn write_snapshot(dir: &Path, snap: &EngineSnapshot) -> Result<PathBuf, JournalError> {
    fs::create_dir_all(dir)?;
    let path = snapshot_path(dir, snap.last_seq);
    let tmp = path.with_extension("json.tmp");
    let mut file = File::create(&tmp)?;
    serde_json::to_writer(&mut file, snap).map_err(io::Error::other)?;
    file.write_all(b"\n")?;
    file.sync_all()?;
    fs::rename(&tmp, &path)?;
    Ok(path)
}

/// Loads the newest snapshot that parses and validates. Unreadable snapshots
/// are skipped in favour of older ones (the command log can always make up
/// the difference).
pub fn load_latest_snapshot(dir: &Path) -> Result<Option<Engine>, JournalError> {
    let entries = match fs::read_dir(dir) {
        Ok(e) => e,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(None),
        Err(e) => return Err(e.into()),
    };
    let mut paths: Vec<PathBuf> = entries
        .filter_map(|e| e.ok().map(|e| e.path()))
        .filter(|p| {
            p.file_name()
                .and_then(|n| n.to_str())
                .is_some_and(|n| n.starts_with("snapshot-") && n.ends_with(".json"))
        })
        .collect();
    paths.sort();
    for path in paths.iter().rev() {
        let loaded = fs::read(path)
            .map_err(|e| e.to_string())
            .and_then(|b| serde_json::from_slice::<EngineSnapshot>(&b).map_err(|e| e.to_string()))
            .and_then(|s| Engine::from_snapshot(&s));
        match loaded {
            Ok(engine) => return Ok(Some(engine)),
            Err(e) => eprintln!("warning: ignoring snapshot {}: {e}", path.display()),
        }
    }
    Ok(None)
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RecoveryReport {
    /// Sequence of the snapshot recovery started from, if any.
    pub snapshot_seq: Option<Seq>,
    pub replay: ReplayStats,
    /// A torn final line was found in the command log and truncated away.
    pub torn_tail: bool,
}

/// Rebuilds engine state from a data directory: newest valid snapshot plus
/// the tail of the command log. `on_event` sees every event regenerated by
/// the tail replay.
pub fn recover(
    dir: &Path,
    on_event: impl FnMut(&Event),
) -> Result<(Engine, RecoveryReport), JournalError> {
    let snapshot = load_latest_snapshot(&dir.join(SNAPSHOT_DIR))?;
    let snapshot_seq = snapshot.as_ref().map(Engine::last_seq);
    let mut engine = snapshot.unwrap_or_default();
    let log = read_and_repair::<SequencedCommand>(&dir.join(COMMANDS_FILE))?;
    let stats = replay(&mut engine, &log.records, on_event)?;
    engine.check_invariants().map_err(JournalError::Snapshot)?;
    Ok((
        engine,
        RecoveryReport {
            snapshot_seq,
            replay: stats,
            torn_tail: log.torn_tail,
        },
    ))
}
