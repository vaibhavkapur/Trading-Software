//! `book-core`: level-2 aggregation and sequence invariants.
//!
//! No I/O, no clocks, no floating point: every function is deterministic so
//! the same input stream always reproduces the same book.

pub mod book;
pub mod ladder;
pub mod sequencer;
pub mod types;

pub use book::{checksum_levels, BookError, DeltaEffect, Integrity, OrderBook};
pub use ladder::{BTreeLadder, Ladder, VecLadder};
pub use sequencer::{Outcome, SeqStats, SequencedBook, SyncState};
pub use types::*;
