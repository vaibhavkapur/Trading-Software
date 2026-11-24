//! A deterministic, price-time-priority exchange core.
//!
//! **Simulated / paper trading only.** Nothing in this crate connects to a
//! real venue.
//!
//! Layers, from the inside out:
//!
//! * [`matching_core`] – pure book state and command processing.
//! * [`sequencer`] – assigns the total order and venue timestamps.
//! * [`journal`] – append-only command/event logs, snapshots, replay.
//! * [`pipeline`] – sequencer → write-ahead journal → engine, single writer.
//! * [`gateway`] – line-delimited JSON over TCP (the only async code).
//! * [`loadgen`] / [`stats`] – seeded traffic generator and latency summaries
//!   used by the `me-bench` binary.

pub mod gateway;
pub mod journal;
pub mod loadgen;
pub mod matching_core;
pub mod pipeline;
pub mod sequencer;
pub mod stats;

pub use matching_core::*;
