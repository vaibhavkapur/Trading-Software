//! `analytics`: spread, depth, imbalance, mid, microprice and order-flow
//! imbalance derived from a reconstructed book.
//!
//! Outputs here are descriptive statistics for display and storage. Ratios
//! and the microprice are `f64`; everything that feeds book state stays in
//! integer ticks/lots inside `book-core`.

use book_core::{Level, PriceTicks, Qty, Seq, TsMicros};
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct BestQuote {
    pub symbol: String,
    pub bid_px: Option<PriceTicks>,
    pub bid_qty: Option<Qty>,
    pub ask_px: Option<PriceTicks>,
    pub ask_qty: Option<Qty>,
    /// Venue time of the event that produced this quote.
    pub asof: TsMicros,
}

impl BestQuote {
    pub fn from_levels(symbol: &str, bids: &[Level], asks: &[Level], asof: TsMicros) -> Self {
        Self {
            symbol: symbol.to_string(),
            bid_px: bids.first().map(|l| l.price_ticks),
            bid_qty: bids.first().map(|l| l.qty),
            ask_px: asks.first().map(|l| l.price_ticks),
            ask_qty: asks.first().map(|l| l.qty),
            asof,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct BookMetric {
    pub symbol: String,
    /// Venue time of the last event included.
    pub ts: TsMicros,
    pub seq: Seq,
    /// `ask - bid` in ticks. Negative when the book is crossed.
    pub spread_ticks: Option<i64>,
    /// `bid + ask`: twice the mid, kept as an exact integer.
    pub mid_ticks_x2: Option<i64>,
    /// Mid price in ticks (may end in .5).
    pub mid_px: Option<f64>,
    /// Size-weighted mid in ticks: `(bid*ask_qty + ask*bid_qty)/(bid_qty+ask_qty)`.
    pub microprice: Option<f64>,
    /// Top-of-book imbalance in [-1, 1]: `(bid_qty - ask_qty)/(bid_qty + ask_qty)`.
    pub imbalance: Option<f64>,
    /// Same ratio over the levels passed in (the configured metric depth).
    pub depth_imbalance: Option<f64>,
    pub bid_depth: Qty,
    pub ask_depth: Qty,
    pub bid_levels: usize,
    pub ask_levels: usize,
    /// Order-flow imbalance accumulated since the previous metric, in lots.
    pub ofi: i64,
}

fn ratio(bid: Qty, ask: Qty) -> Option<f64> {
    let total = bid as f64 + ask as f64;
    (total > 0.0).then(|| (bid as f64 - ask as f64) / total)
}

/// Computes metrics over `bids`/`asks` (best first). Pass exactly the levels
/// that should count towards depth.
pub fn compute_metric(
    symbol: &str,
    ts: TsMicros,
    seq: Seq,
    bids: &[Level],
    asks: &[Level],
    ofi: i64,
) -> BookMetric {
    let bid_depth: Qty = bids.iter().map(|l| l.qty).sum();
    let ask_depth: Qty = asks.iter().map(|l| l.qty).sum();
    let both = bids.first().zip(asks.first());
    BookMetric {
        symbol: symbol.to_string(),
        ts,
        seq,
        spread_ticks: both.map(|(b, a)| a.price_ticks - b.price_ticks),
        mid_ticks_x2: both.map(|(b, a)| a.price_ticks + b.price_ticks),
        mid_px: both.map(|(b, a)| (a.price_ticks + b.price_ticks) as f64 / 2.0),
        microprice: both.and_then(|(b, a)| {
            let total = b.qty as f64 + a.qty as f64;
            (total > 0.0).then(|| {
                (b.price_ticks as f64 * a.qty as f64 + a.price_ticks as f64 * b.qty as f64) / total
            })
        }),
        imbalance: both.and_then(|(b, a)| ratio(b.qty, a.qty)),
        depth_imbalance: ratio(bid_depth, ask_depth),
        bid_depth,
        ask_depth,
        bid_levels: bids.len(),
        ask_levels: asks.len(),
        ofi,
    }
}

/// Order-flow imbalance (Cont, Kukanov & Stoikov, 2014) from consecutive
/// best quotes. Positive values mean net buying pressure at the touch.
#[derive(Debug, Clone, Default)]
pub struct OfiTracker {
    prev: Option<(Option<Level>, Option<Level>)>,
    acc: i64,
}

impl OfiTracker {
    /// Feeds the best bid/ask after an applied event.
    pub fn update(&mut self, bid: Option<Level>, ask: Option<Level>) {
        if let Some((pb, pa)) = self.prev {
            self.acc += bid_flow(pb, bid) - ask_flow(pa, ask);
        }
        self.prev = Some((bid, ask));
    }

    /// Returns the accumulated value and starts a new window.
    pub fn take(&mut self) -> i64 {
        std::mem::take(&mut self.acc)
    }

    /// Call after a resync: flow across a discontinuity is meaningless.
    pub fn reset(&mut self) {
        self.prev = None;
        self.acc = 0;
    }
}

fn q(l: Option<Level>) -> i64 {
    l.map_or(0, |l| l.qty as i64)
}

fn bid_flow(prev: Option<Level>, now: Option<Level>) -> i64 {
    match (prev, now) {
        (Some(p), Some(n)) if n.price_ticks > p.price_ticks => n.qty as i64,
        (Some(p), Some(n)) if n.price_ticks == p.price_ticks => n.qty as i64 - p.qty as i64,
        (Some(p), Some(_)) => -(p.qty as i64),
        (p, n) => q(n) - q(p),
    }
}

fn ask_flow(prev: Option<Level>, now: Option<Level>) -> i64 {
    match (prev, now) {
        (Some(p), Some(n)) if n.price_ticks < p.price_ticks => n.qty as i64,
        (Some(p), Some(n)) if n.price_ticks == p.price_ticks => n.qty as i64 - p.qty as i64,
        (Some(p), Some(_)) => -(p.qty as i64),
        (p, n) => q(n) - q(p),
    }
}
