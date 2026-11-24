//! `stream-api`: the engine that ties the crates together, the HTTP/WebSocket
//! surface, Prometheus metrics, the optional ClickHouse sink, and offline
//! record/replay drivers.

pub mod clickhouse;
pub mod engine;
pub mod metrics;
pub mod offline;
pub mod server;

pub use engine::{BookView, Engine, EngineConfig, FeedHealth, Output, StatusEvent, StatusKind};
