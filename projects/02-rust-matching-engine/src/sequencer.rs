//! `sequencer`: assigns the total order and venue timestamps to commands.
//!
//! The sequencer is the only component that reads a clock. Timestamps are
//! forced to be non-decreasing, so a wall clock stepping backwards (NTP
//! adjustment, VM pause) cannot make venue time run in reverse.

use crate::matching_core::{Command, Seq, SequencedCommand};
use std::time::{SystemTime, UNIX_EPOCH};

pub trait Clock {
    /// Current time in nanoseconds since the Unix epoch (UTC).
    fn now_ns(&mut self) -> u64;
}

impl<C: Clock + ?Sized> Clock for Box<C> {
    fn now_ns(&mut self) -> u64 {
        (**self).now_ns()
    }
}

/// Wall clock, UTC.
#[derive(Clone, Copy, Debug, Default)]
pub struct SystemClock;

impl Clock for SystemClock {
    fn now_ns(&mut self) -> u64 {
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_nanos() as u64)
            .unwrap_or(0)
    }
}

/// Deterministic clock that advances by a fixed step on every read. Used by
/// the CLI, fixtures and tests so that outputs are reproducible.
#[derive(Clone, Copy, Debug)]
pub struct LogicalClock {
    next: u64,
    step: u64,
}

impl LogicalClock {
    pub fn new(start: u64, step: u64) -> Self {
        LogicalClock { next: start, step }
    }
}

impl Default for LogicalClock {
    /// Ticks 1, 2, 3, ... so `ts_ns == seq` for a fresh sequencer.
    fn default() -> Self {
        LogicalClock::new(1, 1)
    }
}

impl Clock for LogicalClock {
    fn now_ns(&mut self) -> u64 {
        let now = self.next;
        self.next += self.step;
        now
    }
}

pub struct Sequencer<C: Clock> {
    next_seq: Seq,
    last_ts_ns: u64,
    clock: C,
}

impl<C: Clock> Sequencer<C> {
    pub fn new(clock: C) -> Self {
        Self::resume(0, 0, clock)
    }

    /// Continues after `last_seq`/`last_ts_ns` (e.g. following recovery).
    pub fn resume(last_seq: Seq, last_ts_ns: u64, clock: C) -> Self {
        Sequencer {
            next_seq: last_seq + 1,
            last_ts_ns,
            clock,
        }
    }

    pub fn next_seq(&self) -> Seq {
        self.next_seq
    }

    pub fn sequence(&mut self, cmd: Command) -> SequencedCommand {
        let ts_ns = self.clock.now_ns().max(self.last_ts_ns);
        self.last_ts_ns = ts_ns;
        let seq = self.next_seq;
        self.next_seq += 1;
        SequencedCommand { seq, ts_ns, cmd }
    }
}
