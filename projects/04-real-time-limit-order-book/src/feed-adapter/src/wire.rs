//! Venue wire format (`simvenue.v1`) and its normalization into the
//! `book-core` schema.
//!
//! The simulated venue speaks the dialect most public level-2 feeds use:
//! newline-delimited JSON, decimal strings for price and size, `buy`/`sell`
//! sides, and size `"0"` meaning "remove the level". Normalization turns that
//! into integer ticks/lots, `BID`/`ASK`, and explicit `SET`/`DELETE` actions.

use crate::decimal::{format_units, to_units, DecimalError};
use book_core::{Action, BookDelta, BookSnapshot, FeedEvent, Level, Side};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;

pub const WIRE_SCHEMA: &str = "simvenue.v1";

/// A raw message exactly as the venue sends it.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "lowercase")]
pub enum RawMsg {
    Snapshot {
        symbol: String,
        seq: u64,
        /// Venue time, microseconds since the Unix epoch (UTC).
        ts: i64,
        /// `[price, size]` pairs, best first.
        bids: Vec<[String; 2]>,
        asks: Vec<[String; 2]>,
    },
    Delta {
        symbol: String,
        seq: u64,
        ts: i64,
        side: String,
        price: String,
        size: String,
    },
    /// Liveness only; carries no book change.
    Heartbeat { ts: i64 },
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Instrument {
    pub symbol: String,
    /// Minimum price increment as a decimal string, e.g. `"0.01"`.
    pub tick_size: String,
    /// Minimum size increment as a decimal string, e.g. `"1"`.
    pub lot_size: String,
}

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum DecodeError {
    #[error("malformed frame: {0}")]
    Json(String),
    #[error("unknown symbol `{0}`")]
    UnknownSymbol(String),
    #[error("unknown side `{0}`")]
    UnknownSide(String),
    #[error("bad price: {0}")]
    Price(DecimalError),
    #[error("bad size: {0}")]
    Size(DecimalError),
    #[error("price must be greater than zero")]
    NonPositivePrice,
}

#[derive(Debug, Clone, Default)]
pub struct Normalizer {
    instruments: HashMap<String, Instrument>,
}

impl Normalizer {
    pub fn new(instruments: impl IntoIterator<Item = Instrument>) -> Self {
        Self {
            instruments: instruments
                .into_iter()
                .map(|i| (i.symbol.clone(), i))
                .collect(),
        }
    }

    pub fn instrument(&self, symbol: &str) -> Option<&Instrument> {
        self.instruments.get(symbol)
    }

    pub fn symbols(&self) -> Vec<String> {
        let mut v: Vec<String> = self.instruments.keys().cloned().collect();
        v.sort();
        v
    }

    /// Decodes one frame. `Ok(None)` is a heartbeat.
    pub fn decode(&self, line: &str) -> Result<Option<FeedEvent>, DecodeError> {
        let raw: RawMsg =
            serde_json::from_str(line).map_err(|e| DecodeError::Json(e.to_string()))?;
        self.normalize(raw)
    }

    pub fn normalize(&self, raw: RawMsg) -> Result<Option<FeedEvent>, DecodeError> {
        match raw {
            RawMsg::Heartbeat { .. } => Ok(None),
            RawMsg::Snapshot {
                symbol,
                seq,
                ts,
                bids,
                asks,
            } => {
                let inst = self.lookup(&symbol)?;
                let levels = |raw: Vec<[String; 2]>| -> Result<Vec<Level>, DecodeError> {
                    raw.iter()
                        .map(|[p, s]| {
                            Ok(Level {
                                price_ticks: price(inst, p)?,
                                qty: size(inst, s)?,
                            })
                        })
                        .collect()
                };
                Ok(Some(FeedEvent::Snapshot(BookSnapshot {
                    bids: levels(bids)?,
                    asks: levels(asks)?,
                    symbol,
                    seq,
                    captured_at: ts,
                })))
            }
            RawMsg::Delta {
                symbol,
                seq,
                ts,
                side,
                price: p,
                size: s,
            } => {
                let inst = self.lookup(&symbol)?;
                let side = match side.as_str() {
                    "buy" | "bid" => Side::Bid,
                    "sell" | "ask" => Side::Ask,
                    _ => return Err(DecodeError::UnknownSide(side)),
                };
                let qty = size(inst, &s)?;
                Ok(Some(FeedEvent::Delta(BookDelta {
                    price_ticks: price(inst, &p)?,
                    action: if qty == 0 {
                        Action::Delete
                    } else {
                        Action::Set
                    },
                    qty,
                    symbol,
                    seq,
                    side,
                    event_ts: ts,
                })))
            }
        }
    }

    fn lookup(&self, symbol: &str) -> Result<&Instrument, DecodeError> {
        self.instruments
            .get(symbol)
            .ok_or_else(|| DecodeError::UnknownSymbol(symbol.to_string()))
    }
}

fn price(inst: &Instrument, s: &str) -> Result<i64, DecodeError> {
    let ticks = to_units(s, &inst.tick_size).map_err(DecodeError::Price)?;
    if ticks <= 0 {
        return Err(DecodeError::NonPositivePrice);
    }
    Ok(ticks)
}

fn size(inst: &Instrument, s: &str) -> Result<u64, DecodeError> {
    to_units(s, &inst.lot_size)
        .map(|v| v as u64)
        .map_err(DecodeError::Size)
}

/// Encodes normalized values back into the venue dialect (used by the
/// simulator so that the decode path is exercised end to end).
pub fn encode_level(inst: &Instrument, l: Level) -> [String; 2] {
    [
        format_units(l.price_ticks, &inst.tick_size).expect("valid instrument"),
        format_units(l.qty as i64, &inst.lot_size).expect("valid instrument"),
    ]
}

pub fn encode_side(side: Side) -> &'static str {
    match side {
        Side::Bid => "buy",
        Side::Ask => "sell",
    }
}
