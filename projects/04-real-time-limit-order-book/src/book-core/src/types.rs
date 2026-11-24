//! Normalized domain types shared by every service.
//!
//! Prices are integer ticks and quantities are integer lots; no floating point
//! is used for book arithmetic. Timestamps are microseconds since the Unix
//! epoch in UTC (`i64`, which stays inside JavaScript's safe-integer range).

use serde::{Deserialize, Serialize};

pub type PriceTicks = i64;
pub type Qty = u64;
pub type Seq = u64;
/// Microseconds since the Unix epoch, UTC.
pub type TsMicros = i64;

/// Version of the normalized schema below. Bump on any breaking change.
pub const NORMALIZED_SCHEMA_VERSION: u32 = 1;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "UPPERCASE")]
pub enum Side {
    Bid,
    Ask,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "UPPERCASE")]
pub enum Action {
    /// Replace the aggregate quantity resting at a price level.
    Set,
    /// Remove the price level.
    Delete,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct Level {
    pub price_ticks: PriceTicks,
    pub qty: Qty,
}

/// Full-depth image of one book. `seq` is the sequence number of the last
/// delta the image already includes; the next delta to apply is `seq + 1`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct BookSnapshot {
    pub symbol: String,
    pub seq: Seq,
    /// Best (highest) first.
    pub bids: Vec<Level>,
    /// Best (lowest) first.
    pub asks: Vec<Level>,
    /// Venue time at which the image was captured.
    pub captured_at: TsMicros,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct BookDelta {
    pub symbol: String,
    pub seq: Seq,
    pub side: Side,
    pub price_ticks: PriceTicks,
    #[serde(rename = "quantity")]
    pub qty: Qty,
    pub action: Action,
    /// Venue event time.
    pub event_ts: TsMicros,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "lowercase")]
pub enum FeedEvent {
    Snapshot(BookSnapshot),
    Delta(BookDelta),
}

impl FeedEvent {
    pub fn symbol(&self) -> &str {
        match self {
            FeedEvent::Snapshot(s) => &s.symbol,
            FeedEvent::Delta(d) => &d.symbol,
        }
    }

    pub fn venue_ts(&self) -> TsMicros {
        match self {
            FeedEvent::Snapshot(s) => s.captured_at,
            FeedEvent::Delta(d) => d.event_ts,
        }
    }
}
