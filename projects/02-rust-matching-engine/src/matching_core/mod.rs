//! `matching-core`: pure deterministic book state and command processing.

pub mod book;
pub mod engine;
pub mod types;

pub use book::{Book, OrderNode, Top};
pub use engine::{BookSnapshot, Engine, EngineSnapshot, OrderSnapshot, SequenceError};
pub use types::*;
