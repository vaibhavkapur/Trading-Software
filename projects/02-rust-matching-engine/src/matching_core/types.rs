//! Wire and domain types shared by every layer.
//!
//! All money and size arithmetic is integer: prices are in ticks, quantities
//! in lots. There is no floating point anywhere in the execution path.

use serde::de::{self, Visitor};
use serde::{Deserialize, Deserializer, Serialize, Serializer};
use std::fmt;

pub type OrderId = u64;
pub type AccountId = u32;
pub type Qty = u64;
pub type Price = i64;
pub type Seq = u64;

/// Largest quantity accepted on a single order.
pub const MAX_QTY: Qty = 1_000_000_000;
/// Largest limit price (in ticks) accepted on a single order.
pub const MAX_PRICE_TICKS: Price = 1_000_000_000_000;

/// Instrument symbol: 1–8 ASCII characters from `A-Z 0-9 . -`, stored inline
/// so that orders and events never allocate.
#[derive(Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct Symbol([u8; Symbol::MAX_LEN]);

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SymbolError(pub String);

impl fmt::Display for SymbolError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "invalid symbol: {}", self.0)
    }
}

impl std::error::Error for SymbolError {}

impl Symbol {
    pub const MAX_LEN: usize = 8;

    pub fn new(s: &str) -> Result<Self, SymbolError> {
        let bytes = s.as_bytes();
        if bytes.is_empty() || bytes.len() > Self::MAX_LEN {
            return Err(SymbolError(format!(
                "{s:?} must be 1..={} characters",
                Self::MAX_LEN
            )));
        }
        if !bytes
            .iter()
            .all(|b| b.is_ascii_uppercase() || b.is_ascii_digit() || *b == b'.' || *b == b'-')
        {
            return Err(SymbolError(format!(
                "{s:?} may only contain A-Z, 0-9, '.' and '-'"
            )));
        }
        let mut raw = [0u8; Self::MAX_LEN];
        raw[..bytes.len()].copy_from_slice(bytes);
        Ok(Symbol(raw))
    }

    pub fn as_str(&self) -> &str {
        let len = self.0.iter().position(|&b| b == 0).unwrap_or(Self::MAX_LEN);
        // Construction guarantees ASCII.
        std::str::from_utf8(&self.0[..len]).expect("symbol is ASCII")
    }
}

impl fmt::Display for Symbol {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

impl fmt::Debug for Symbol {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Symbol({})", self.as_str())
    }
}

impl Serialize for Symbol {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for Symbol {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct V;
        impl Visitor<'_> for V {
            type Value = Symbol;
            fn expecting(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
                f.write_str("a symbol string of 1-8 characters [A-Z0-9.-]")
            }
            fn visit_str<E: de::Error>(self, v: &str) -> Result<Symbol, E> {
                Symbol::new(v).map_err(E::custom)
            }
        }
        d.deserialize_str(V)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "UPPERCASE")]
pub enum Side {
    Buy,
    Sell,
}

impl Side {
    pub fn opposite(self) -> Side {
        match self {
            Side::Buy => Side::Sell,
            Side::Sell => Side::Buy,
        }
    }
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "UPPERCASE")]
pub enum OrdType {
    #[default]
    Limit,
    Market,
}

/// A new order as submitted by a client.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct NewOrder {
    pub order_id: OrderId,
    #[serde(default)]
    pub account_id: AccountId,
    pub symbol: Symbol,
    pub side: Side,
    #[serde(default)]
    pub ord_type: OrdType,
    pub qty: Qty,
    /// Required for `LIMIT`, forbidden for `MARKET`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub price_ticks: Option<Price>,
}

/// A client command before sequencing. This is the wire format accepted by
/// the gateway and the CLI (one JSON object per line).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "SCREAMING_SNAKE_CASE")]
pub enum Command {
    New(NewOrder),
    Cancel {
        order_id: OrderId,
    },
    /// Cancel/replace. `qty` is the new *remaining* quantity.
    Replace {
        order_id: OrderId,
        qty: Qty,
        price_ticks: Price,
    },
}

/// A command with its place in the total order. `ts_ns` is venue time
/// assigned by the sequencer (UTC nanoseconds since the Unix epoch, or a
/// logical counter in tests); it is *not* the client's or gateway's local
/// receive time, and the matching core never reads a clock itself.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct SequencedCommand {
    pub seq: Seq,
    pub ts_ns: u64,
    pub cmd: Command,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum RejectReason {
    DuplicateOrderId,
    UnknownOrder,
    InvalidQty,
    InvalidPrice,
    /// `LIMIT` order without `price_ticks`.
    MissingPrice,
    /// `MARKET` order with `price_ticks`.
    UnexpectedPrice,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum CancelReason {
    UserRequested,
    /// Unfilled remainder of a market order (market orders never rest).
    NoLiquidity,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "UPPERCASE")]
pub enum Role {
    Maker,
    Taker,
}

/// An output of the matching core. `seq` is a gap-free, monotonically
/// increasing event sequence; `cmd_seq`/`ts_ns` are copied from the command
/// that caused the event.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Event {
    pub seq: Seq,
    pub cmd_seq: Seq,
    pub ts_ns: u64,
    #[serde(flatten)]
    pub kind: EventKind,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "SCREAMING_SNAKE_CASE")]
pub enum EventKind {
    Accepted {
        order_id: OrderId,
        account_id: AccountId,
        symbol: Symbol,
        side: Side,
        ord_type: OrdType,
        qty: Qty,
        price_ticks: Option<Price>,
    },
    Rejected {
        order_id: OrderId,
        reason: RejectReason,
    },
    /// Public trade print. The price is always the maker's resting price.
    Trade {
        trade_id: u64,
        symbol: Symbol,
        price_ticks: Price,
        qty: Qty,
        maker_order_id: OrderId,
        taker_order_id: OrderId,
        taker_side: Side,
    },
    /// Per-order execution report; two are emitted for every trade
    /// (maker first, then taker).
    Execution {
        order_id: OrderId,
        trade_id: u64,
        role: Role,
        side: Side,
        price_ticks: Price,
        qty: Qty,
        leaves_qty: Qty,
    },
    Canceled {
        order_id: OrderId,
        symbol: Symbol,
        canceled_qty: Qty,
        reason: CancelReason,
    },
    Replaced {
        order_id: OrderId,
        symbol: Symbol,
        qty: Qty,
        price_ticks: Price,
        /// True when the order kept its place in the FIFO queue
        /// (same price, quantity not increased).
        priority_retained: bool,
    },
    /// Top of book after a command changed it.
    BookTop {
        symbol: Symbol,
        bid_px: Option<Price>,
        bid_qty: Qty,
        ask_px: Option<Price>,
        ask_qty: Qty,
    },
}
