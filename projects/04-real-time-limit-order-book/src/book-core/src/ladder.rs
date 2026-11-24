//! Price-level containers. Two interchangeable implementations exist so their
//! behaviour can be cross-checked in tests and their cost compared with
//! `lob bench`.

use crate::types::{Level, PriceTicks, Qty, Side};
use std::collections::BTreeMap;

pub trait Ladder: Default + Clone + std::fmt::Debug {
    const NAME: &'static str;

    /// Sets the aggregate quantity at a level, returning the previous one.
    fn set(&mut self, side: Side, price: PriceTicks, qty: Qty) -> Option<Qty>;
    fn remove(&mut self, side: Side, price: PriceTicks) -> Option<Qty>;
    fn get(&self, side: Side, price: PriceTicks) -> Option<Qty>;
    fn best(&self, side: Side) -> Option<Level>;
    /// Up to `n` levels, best first.
    fn top(&self, side: Side, n: usize) -> Vec<Level>;
    fn levels(&self, side: Side) -> usize;
    fn clear(&mut self);
}

/// Ordered-map ladder: O(log n) everywhere, the default.
#[derive(Debug, Clone, Default)]
pub struct BTreeLadder {
    bids: BTreeMap<PriceTicks, Qty>,
    asks: BTreeMap<PriceTicks, Qty>,
}

impl BTreeLadder {
    fn side(&self, side: Side) -> &BTreeMap<PriceTicks, Qty> {
        match side {
            Side::Bid => &self.bids,
            Side::Ask => &self.asks,
        }
    }
    fn side_mut(&mut self, side: Side) -> &mut BTreeMap<PriceTicks, Qty> {
        match side {
            Side::Bid => &mut self.bids,
            Side::Ask => &mut self.asks,
        }
    }
}

fn lvl((price_ticks, qty): (&PriceTicks, &Qty)) -> Level {
    Level {
        price_ticks: *price_ticks,
        qty: *qty,
    }
}

impl Ladder for BTreeLadder {
    const NAME: &'static str = "btree";

    fn set(&mut self, side: Side, price: PriceTicks, qty: Qty) -> Option<Qty> {
        self.side_mut(side).insert(price, qty)
    }
    fn remove(&mut self, side: Side, price: PriceTicks) -> Option<Qty> {
        self.side_mut(side).remove(&price)
    }
    fn get(&self, side: Side, price: PriceTicks) -> Option<Qty> {
        self.side(side).get(&price).copied()
    }
    fn best(&self, side: Side) -> Option<Level> {
        match side {
            Side::Bid => self.bids.last_key_value().map(lvl),
            Side::Ask => self.asks.first_key_value().map(lvl),
        }
    }
    fn top(&self, side: Side, n: usize) -> Vec<Level> {
        match side {
            Side::Bid => self.bids.iter().rev().take(n).map(lvl).collect(),
            Side::Ask => self.asks.iter().take(n).map(lvl).collect(),
        }
    }
    fn levels(&self, side: Side) -> usize {
        self.side(side).len()
    }
    fn clear(&mut self) {
        self.bids.clear();
        self.asks.clear();
    }
}

/// Sorted-vector ladder with the best level at the *end* of each vector, so
/// churn near the touch moves few elements. O(log n) search, O(n) worst-case
/// insert/remove, contiguous memory.
#[derive(Debug, Clone, Default)]
pub struct VecLadder {
    /// Ascending by price (best bid last).
    bids: Vec<Level>,
    /// Descending by price (best ask last).
    asks: Vec<Level>,
}

impl VecLadder {
    fn side(&self, side: Side) -> &Vec<Level> {
        match side {
            Side::Bid => &self.bids,
            Side::Ask => &self.asks,
        }
    }
    fn locate(&self, side: Side, price: PriceTicks) -> Result<usize, usize> {
        match side {
            Side::Bid => self.bids.binary_search_by(|l| l.price_ticks.cmp(&price)),
            Side::Ask => self.asks.binary_search_by(|l| price.cmp(&l.price_ticks)),
        }
    }
    fn side_mut(&mut self, side: Side) -> &mut Vec<Level> {
        match side {
            Side::Bid => &mut self.bids,
            Side::Ask => &mut self.asks,
        }
    }
}

impl Ladder for VecLadder {
    const NAME: &'static str = "sorted-vec";

    fn set(&mut self, side: Side, price: PriceTicks, qty: Qty) -> Option<Qty> {
        match self.locate(side, price) {
            Ok(i) => Some(std::mem::replace(&mut self.side_mut(side)[i].qty, qty)),
            Err(i) => {
                self.side_mut(side).insert(
                    i,
                    Level {
                        price_ticks: price,
                        qty,
                    },
                );
                None
            }
        }
    }
    fn remove(&mut self, side: Side, price: PriceTicks) -> Option<Qty> {
        let i = self.locate(side, price).ok()?;
        Some(self.side_mut(side).remove(i).qty)
    }
    fn get(&self, side: Side, price: PriceTicks) -> Option<Qty> {
        let i = self.locate(side, price).ok()?;
        Some(self.side(side)[i].qty)
    }
    fn best(&self, side: Side) -> Option<Level> {
        self.side(side).last().copied()
    }
    fn top(&self, side: Side, n: usize) -> Vec<Level> {
        self.side(side).iter().rev().take(n).copied().collect()
    }
    fn levels(&self, side: Side) -> usize {
        self.side(side).len()
    }
    fn clear(&mut self) {
        self.bids.clear();
        self.asks.clear();
    }
}
