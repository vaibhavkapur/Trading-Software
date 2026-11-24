//! Sequence validation and the snapshot/delta recovery state machine.
//!
//! ```text
//!                 snapshot                      gap / rejected delta
//! AwaitingSnapshot ───────► Synced ─────────────────────────────► Resyncing
//!        ▲                    ▲                                       │
//!        │                    └── snapshot covering the gap, or the ──┘
//!      reset                      missing delta arriving late
//! ```
//!
//! Invariants:
//! * A delta is applied only if its `seq` is exactly `last_seq + 1`.
//! * `seq <= last_seq` is a duplicate and is ignored.
//! * `seq > last_seq + 1` is a gap: the book stops being trusted
//!   (`Resyncing`), the delta is buffered, and the caller must obtain a
//!   snapshot. Buffered deltas newer than the snapshot are replayed on top.
//! * While not `Synced` the book contents must not be presented as current.

use crate::book::{BookError, DeltaEffect, OrderBook};
use crate::ladder::{BTreeLadder, Ladder};
use crate::types::{BookDelta, BookSnapshot, FeedEvent, Seq, Side};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SyncState {
    AwaitingSnapshot,
    Synced,
    Resyncing,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Outcome {
    DeltaApplied {
        effect: DeltaEffect,
        /// Buffered deltas applied right after this one.
        drained: usize,
        /// True when this delta closed a gap without needing a snapshot
        /// (out-of-order delivery healed by the reorder buffer).
        recovered: bool,
    },
    SnapshotApplied {
        seq: Seq,
        drained: usize,
        /// False when buffered deltas still do not chain onto the snapshot.
        synced: bool,
        was: SyncState,
        /// The book was `Synced` at exactly `seq` but differed from the
        /// snapshot: silent corruption that sequence numbers did not reveal.
        drift: bool,
    },
    Buffered {
        seq: Seq,
    },
    GapDetected {
        expected: Seq,
        got: Seq,
    },
    Duplicate {
        seq: Seq,
        last_seq: Seq,
    },
    StaleSnapshot {
        seq: Seq,
        last_seq: Seq,
    },
    Rejected {
        error: BookError,
    },
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Serialize)]
pub struct SeqStats {
    pub deltas_applied: u64,
    pub snapshots_applied: u64,
    pub duplicates: u64,
    pub gaps: u64,
    pub rejected: u64,
    pub recoveries: u64,
    pub drift: u64,
    pub stale_snapshots: u64,
    pub remove_missing: u64,
    pub buffer_overflow: u64,
}

pub const DEFAULT_MAX_BUFFER: usize = 50_000;

#[derive(Debug, Clone)]
pub struct SequencedBook<L: Ladder = BTreeLadder> {
    book: OrderBook<L>,
    state: SyncState,
    last_seq: Seq,
    buffer: BTreeMap<Seq, BookDelta>,
    max_buffer: usize,
    stats: SeqStats,
}

impl<L: Ladder> SequencedBook<L> {
    pub fn new(symbol: impl Into<String>) -> Self {
        Self::with_max_buffer(symbol, DEFAULT_MAX_BUFFER)
    }

    pub fn with_max_buffer(symbol: impl Into<String>, max_buffer: usize) -> Self {
        Self {
            book: OrderBook::new(symbol),
            state: SyncState::AwaitingSnapshot,
            last_seq: 0,
            buffer: BTreeMap::new(),
            max_buffer: max_buffer.max(1),
            stats: SeqStats::default(),
        }
    }

    pub fn book(&self) -> &OrderBook<L> {
        &self.book
    }
    pub fn state(&self) -> SyncState {
        self.state
    }
    pub fn last_seq(&self) -> Seq {
        self.last_seq
    }
    pub fn stats(&self) -> SeqStats {
        self.stats
    }
    pub fn buffered(&self) -> usize {
        self.buffer.len()
    }

    /// Forgets all state, e.g. when the upstream session restarts and
    /// sequence numbers begin again. Counters are kept.
    pub fn reset(&mut self) {
        self.book = OrderBook::new(self.book.symbol().to_string());
        self.state = SyncState::AwaitingSnapshot;
        self.last_seq = 0;
        self.buffer.clear();
    }

    pub fn on_event(&mut self, ev: &FeedEvent) -> Outcome {
        match ev {
            FeedEvent::Snapshot(s) => self.on_snapshot(s),
            FeedEvent::Delta(d) => self.on_delta(d),
        }
    }

    pub fn on_delta(&mut self, d: &BookDelta) -> Outcome {
        if self.state == SyncState::AwaitingSnapshot {
            return match self.book.validate_delta(d) {
                Ok(()) => self.stash(d),
                Err(error) => self.reject(error),
            };
        }
        if d.seq <= self.last_seq {
            self.stats.duplicates += 1;
            return Outcome::Duplicate {
                seq: d.seq,
                last_seq: self.last_seq,
            };
        }
        if let Err(error) = self.book.validate_delta(d) {
            // The sequence number was consumed by something we cannot apply,
            // so the book is no longer known to be right.
            self.state = SyncState::Resyncing;
            return self.reject(error);
        }
        if d.seq == self.last_seq + 1 {
            let effect = self.apply(d);
            let was_resyncing = self.state == SyncState::Resyncing;
            let (drained, ok) = self.drain();
            let recovered = was_resyncing && ok && self.buffer.is_empty();
            if recovered {
                self.state = SyncState::Synced;
                self.stats.recoveries += 1;
            }
            return Outcome::DeltaApplied {
                effect,
                drained,
                recovered,
            };
        }
        match self.state {
            SyncState::Synced => {
                self.state = SyncState::Resyncing;
                self.stats.gaps += 1;
                let expected = self.last_seq + 1;
                self.stash(d);
                Outcome::GapDetected {
                    expected,
                    got: d.seq,
                }
            }
            _ => self.stash(d),
        }
    }

    pub fn on_snapshot(&mut self, s: &BookSnapshot) -> Outcome {
        if let Err(error) = self.book.validate_snapshot(s) {
            return self.reject(error);
        }
        let was = self.state;
        if was != SyncState::AwaitingSnapshot && s.seq < self.last_seq {
            self.stats.stale_snapshots += 1;
            return Outcome::StaleSnapshot {
                seq: s.seq,
                last_seq: self.last_seq,
            };
        }
        let drift = was == SyncState::Synced && s.seq == self.last_seq && !self.matches(s);
        if drift {
            self.stats.drift += 1;
        }
        self.book
            .apply_snapshot(s)
            .expect("snapshot was validated above");
        self.last_seq = s.seq;
        self.stats.snapshots_applied += 1;
        let (drained, ok) = self.drain();
        let synced = ok && self.buffer.is_empty();
        self.state = if synced {
            SyncState::Synced
        } else {
            SyncState::Resyncing
        };
        if synced && was == SyncState::Resyncing {
            self.stats.recoveries += 1;
        }
        Outcome::SnapshotApplied {
            seq: s.seq,
            drained,
            synced,
            was,
            drift,
        }
    }

    fn matches(&self, s: &BookSnapshot) -> bool {
        self.book.top(Side::Bid, usize::MAX) == sorted(&s.bids, true)
            && self.book.top(Side::Ask, usize::MAX) == sorted(&s.asks, false)
    }

    fn reject(&mut self, error: BookError) -> Outcome {
        self.stats.rejected += 1;
        Outcome::Rejected { error }
    }

    fn stash(&mut self, d: &BookDelta) -> Outcome {
        self.buffer.insert(d.seq, d.clone());
        while self.buffer.len() > self.max_buffer {
            // Oldest entries are the ones a fresh snapshot is most likely to
            // cover; if not, the hole is detected again after the snapshot.
            self.buffer.pop_first();
            self.stats.buffer_overflow += 1;
        }
        Outcome::Buffered { seq: d.seq }
    }

    fn apply(&mut self, d: &BookDelta) -> DeltaEffect {
        let effect = self
            .book
            .apply_delta(d)
            .expect("delta was validated before apply");
        if effect == DeltaEffect::RemoveMissing {
            self.stats.remove_missing += 1;
        }
        self.last_seq = d.seq;
        self.stats.deltas_applied += 1;
        effect
    }

    /// Applies buffered deltas that chain onto `last_seq`, discarding ones
    /// already covered. Returns `(applied, ok)`.
    fn drain(&mut self) -> (usize, bool) {
        let mut applied = 0;
        while let Some((&seq, _)) = self.buffer.first_key_value() {
            if seq <= self.last_seq {
                self.buffer.pop_first();
            } else if seq == self.last_seq + 1 {
                let (_, d) = self.buffer.pop_first().expect("peeked above");
                self.apply(&d);
                applied += 1;
            } else {
                return (applied, false);
            }
        }
        (applied, true)
    }
}

fn sorted(levels: &[crate::types::Level], descending: bool) -> Vec<crate::types::Level> {
    let mut v = levels.to_vec();
    v.sort_by_key(|l| l.price_ticks);
    if descending {
        v.reverse();
    }
    v
}
