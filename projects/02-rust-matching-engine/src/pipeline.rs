//! The single-writer path: sequencer → write-ahead command journal →
//! matching core → event journal.
//!
//! A command is appended to the command log *before* it is applied, so any
//! event a client has seen can be regenerated from the log. If a journal
//! write fails the pipeline halts (fail-stop): it refuses further commands
//! rather than continue with state that is not backed by the log.

use crate::journal::{
    self, Durable, JournalError, JsonlWriter, RecoveryReport, COMMANDS_FILE, EVENTS_FILE,
    SNAPSHOT_DIR,
};
use crate::matching_core::{Command, Engine, Event, Seq};
use crate::sequencer::{Clock, Sequencer};
use std::fmt;
use std::io;
use std::path::{Path, PathBuf};

#[derive(Debug)]
pub enum PipelineError {
    /// A journal write failed; the command was not acknowledged. The pipeline
    /// is now halted.
    JournalWrite(io::Error),
    /// A previous journal failure halted the pipeline; restart to recover.
    Halted,
}

impl fmt::Display for PipelineError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            PipelineError::JournalWrite(e) => write!(f, "journal write failed, halting: {e}"),
            PipelineError::Halted => {
                write!(
                    f,
                    "engine halted after a journal failure; restart to recover"
                )
            }
        }
    }
}

impl std::error::Error for PipelineError {}

#[derive(Clone, Debug, Default)]
pub struct PipelineOptions {
    /// `fsync` the command log after every command (durable against power
    /// loss, much slower). Without it, records survive a process crash but
    /// not necessarily an OS crash.
    pub fsync: bool,
    /// Write a snapshot every N commands (0 = never).
    pub snapshot_every: u64,
}

pub struct Pipeline {
    sequencer: Sequencer<Box<dyn Clock + Send>>,
    engine: Engine,
    cmd_log: JsonlWriter,
    evt_log: Option<JsonlWriter>,
    events: Vec<Event>,
    halted: bool,
    opts: PipelineOptions,
    snapshot_dir: Option<PathBuf>,
    since_snapshot: u64,
}

impl Pipeline {
    /// A pipeline over arbitrary sinks, starting from an empty engine.
    pub fn with_sinks(
        clock: Box<dyn Clock + Send>,
        cmd_log: Box<dyn Durable>,
        evt_log: Option<Box<dyn Durable>>,
    ) -> Self {
        Pipeline {
            sequencer: Sequencer::new(clock),
            engine: Engine::new(),
            cmd_log: JsonlWriter::new(cmd_log),
            evt_log: evt_log.map(JsonlWriter::new),
            events: Vec::with_capacity(64),
            halted: false,
            opts: PipelineOptions::default(),
            snapshot_dir: None,
            since_snapshot: 0,
        }
    }

    /// A pipeline that journals nowhere. For tests and one-shot CLI runs.
    pub fn in_memory(clock: Box<dyn Clock + Send>) -> Self {
        Self::with_sinks(clock, Box::new(io::sink()), None)
    }

    /// Opens (or creates) a data directory, recovering state from the newest
    /// snapshot plus the command-log tail, and bringing the event log up to
    /// date with any events it is missing.
    pub fn open(
        dir: &Path,
        clock: Box<dyn Clock + Send>,
        opts: PipelineOptions,
    ) -> Result<(Self, RecoveryReport), JournalError> {
        std::fs::create_dir_all(dir)?;
        let evt_path = dir.join(EVENTS_FILE);
        let last_event_seq = journal::read_and_repair::<Event>(&evt_path)?
            .records
            .last()
            .map_or(0, |e| e.seq);
        let mut missing = Vec::new();
        let (engine, report) = journal::recover(dir, |e| {
            if e.seq > last_event_seq {
                missing.push(*e);
            }
        })?;
        let mut evt_log = JsonlWriter::open_append(&evt_path)?;
        evt_log.append_all(&missing)?;
        evt_log.sync()?;
        let cmd_log = JsonlWriter::open_append(&dir.join(COMMANDS_FILE))?;
        let pipeline = Pipeline {
            sequencer: Sequencer::resume(engine.last_seq(), engine.last_ts_ns(), clock),
            engine,
            cmd_log,
            evt_log: Some(evt_log),
            events: Vec::with_capacity(64),
            halted: false,
            opts,
            snapshot_dir: Some(dir.join(SNAPSHOT_DIR)),
            since_snapshot: 0,
        };
        Ok((pipeline, report))
    }

    pub fn engine(&self) -> &Engine {
        &self.engine
    }

    pub fn is_halted(&self) -> bool {
        self.halted
    }

    /// Sequences, journals and applies one command. Returns the command's
    /// sequence number and the events it produced.
    pub fn submit(&mut self, cmd: Command) -> Result<(Seq, &[Event]), PipelineError> {
        if self.halted {
            return Err(PipelineError::Halted);
        }
        let sc = self.sequencer.sequence(cmd);
        let mut logged = self.cmd_log.append(&sc);
        if logged.is_ok() && self.opts.fsync {
            logged = self.cmd_log.sync();
        }
        if let Err(e) = logged {
            self.halted = true;
            return Err(PipelineError::JournalWrite(e));
        }
        self.events.clear();
        self.engine
            .process(&sc, &mut self.events)
            .expect("sequencer and engine advance in lockstep");
        if let Some(log) = &mut self.evt_log {
            // The command is already durable, so the events can be rebuilt on
            // restart; still, stop here rather than run with a lagging feed.
            if let Err(e) = log.append_all(&self.events) {
                self.halted = true;
                return Err(PipelineError::JournalWrite(e));
            }
        }
        self.since_snapshot += 1;
        if self.opts.snapshot_every > 0 && self.since_snapshot >= self.opts.snapshot_every {
            if let Err(e) = self.snapshot() {
                eprintln!("warning: snapshot failed (continuing on the command log): {e}");
            }
        }
        Ok((sc.seq, &self.events))
    }

    /// Writes a snapshot now. Returns `None` for pipelines without a data
    /// directory.
    pub fn snapshot(&mut self) -> Result<Option<PathBuf>, JournalError> {
        self.since_snapshot = 0;
        let Some(dir) = &self.snapshot_dir else {
            return Ok(None);
        };
        // Everything the snapshot covers must be in the logs first.
        self.cmd_log.sync()?;
        if let Some(log) = &mut self.evt_log {
            log.sync()?;
        }
        journal::write_snapshot(dir, &self.engine.snapshot()).map(Some)
    }
}
