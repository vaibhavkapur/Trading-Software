//! `feed-adapter`: capture, decoding and transport-facing concerns.
//!
//! * [`wire`] – the venue dialect and its normalization to ticks/lots.
//! * [`decimal`] – exact decimal parsing.
//! * [`sim`] – deterministic simulated venue with fault injection.
//! * [`replay`] – capture files and manifests.

pub mod decimal;
pub mod replay;
pub mod rng;
pub mod sim;
pub mod wire;

pub use replay::{
    manifest_path, read_capture, read_manifest, write_frame, write_manifest, Manifest,
};
pub use sim::{
    checksum_hex, parse_fault_plan, Checkpoint, Fault, Frame, SimConfig, SimSession, SimSymbol,
    VIRTUAL_EPOCH_US,
};
pub use wire::{DecodeError, Instrument, Normalizer, RawMsg, WIRE_SCHEMA};
