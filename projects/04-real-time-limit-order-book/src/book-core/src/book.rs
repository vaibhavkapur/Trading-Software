//! A single level-2 book: validated mutation, integrity classification and a
//! deterministic checksum used for replay checkpoints and drift detection.

use crate::ladder::{BTreeLadder, Ladder};
use crate::types::{Action, BookDelta, BookSnapshot, Level, PriceTicks, Qty, Side};
use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum BookError {
    #[error("price must be a positive number of ticks, got {price}")]
    InvalidPrice { price: PriceTicks },
    #[error("snapshot level at {price} has zero quantity")]
    ZeroQtyInSnapshot { price: PriceTicks },
    #[error("snapshot lists {side:?} level {price} more than once")]
    DuplicateLevel { side: Side, price: PriceTicks },
    #[error("event for symbol {got} applied to book {expected}")]
    SymbolMismatch { expected: String, got: String },
}

/// Relationship between best bid and best ask.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Integrity {
    Ok,
    /// Best bid equals best ask.
    Locked,
    /// Best bid is above best ask.
    Crossed,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum DeltaEffect {
    Inserted,
    Updated {
        prev: Qty,
    },
    Removed {
        prev: Qty,
    },
    /// Delete for a level that was not present. Applied as a no-op; callers
    /// count it because it hints at earlier loss.
    RemoveMissing,
}

#[derive(Debug, Clone)]
pub struct OrderBook<L: Ladder = BTreeLadder> {
    symbol: String,
    ladder: L,
}

impl<L: Ladder> OrderBook<L> {
    pub fn new(symbol: impl Into<String>) -> Self {
        Self {
            symbol: symbol.into(),
            ladder: L::default(),
        }
    }

    pub fn symbol(&self) -> &str {
        &self.symbol
    }

    fn check_symbol(&self, got: &str) -> Result<(), BookError> {
        if got == self.symbol {
            Ok(())
        } else {
            Err(BookError::SymbolMismatch {
                expected: self.symbol.clone(),
                got: got.to_string(),
            })
        }
    }

    /// Validates a snapshot without touching the book.
    pub fn validate_snapshot(&self, snap: &BookSnapshot) -> Result<(), BookError> {
        self.check_symbol(&snap.symbol)?;
        for (side, levels) in [(Side::Bid, &snap.bids), (Side::Ask, &snap.asks)] {
            let mut seen = std::collections::HashSet::with_capacity(levels.len());
            for l in levels {
                if l.price_ticks <= 0 {
                    return Err(BookError::InvalidPrice {
                        price: l.price_ticks,
                    });
                }
                if l.qty == 0 {
                    return Err(BookError::ZeroQtyInSnapshot {
                        price: l.price_ticks,
                    });
                }
                if !seen.insert(l.price_ticks) {
                    return Err(BookError::DuplicateLevel {
                        side,
                        price: l.price_ticks,
                    });
                }
            }
        }
        Ok(())
    }

    /// Replaces the whole book. All-or-nothing: an invalid snapshot leaves the
    /// current state untouched.
    pub fn apply_snapshot(&mut self, snap: &BookSnapshot) -> Result<(), BookError> {
        self.validate_snapshot(snap)?;
        self.ladder.clear();
        for l in &snap.bids {
            self.ladder.set(Side::Bid, l.price_ticks, l.qty);
        }
        for l in &snap.asks {
            self.ladder.set(Side::Ask, l.price_ticks, l.qty);
        }
        Ok(())
    }

    pub fn validate_delta(&self, d: &BookDelta) -> Result<(), BookError> {
        self.check_symbol(&d.symbol)?;
        if d.price_ticks <= 0 {
            return Err(BookError::InvalidPrice {
                price: d.price_ticks,
            });
        }
        Ok(())
    }

    /// Applies one level update. `Set` with quantity zero is treated as a
    /// delete, which is how most venues encode level removal.
    pub fn apply_delta(&mut self, d: &BookDelta) -> Result<DeltaEffect, BookError> {
        self.validate_delta(d)?;
        let delete = d.action == Action::Delete || d.qty == 0;
        Ok(if delete {
            match self.ladder.remove(d.side, d.price_ticks) {
                Some(prev) => DeltaEffect::Removed { prev },
                None => DeltaEffect::RemoveMissing,
            }
        } else {
            match self.ladder.set(d.side, d.price_ticks, d.qty) {
                Some(prev) => DeltaEffect::Updated { prev },
                None => DeltaEffect::Inserted,
            }
        })
    }

    pub fn best(&self, side: Side) -> Option<Level> {
        self.ladder.best(side)
    }

    pub fn top(&self, side: Side, n: usize) -> Vec<Level> {
        self.ladder.top(side, n)
    }

    pub fn levels(&self, side: Side) -> usize {
        self.ladder.levels(side)
    }

    pub fn get(&self, side: Side, price: PriceTicks) -> Option<Qty> {
        self.ladder.get(side, price)
    }

    pub fn integrity(&self) -> Integrity {
        match (self.best(Side::Bid), self.best(Side::Ask)) {
            (Some(b), Some(a)) if b.price_ticks > a.price_ticks => Integrity::Crossed,
            (Some(b), Some(a)) if b.price_ticks == a.price_ticks => Integrity::Locked,
            _ => Integrity::Ok,
        }
    }

    /// FNV-1a (64-bit) over the top `depth` levels of each side. Stable across
    /// platforms and ladder implementations.
    pub fn checksum(&self, depth: usize) -> u64 {
        checksum_levels(&self.top(Side::Bid, depth), &self.top(Side::Ask, depth))
    }
}

/// Checksum of explicit level lists (best first), matching [`OrderBook::checksum`].
pub fn checksum_levels(bids: &[Level], asks: &[Level]) -> u64 {
    const OFFSET: u64 = 0xcbf2_9ce4_8422_2325;
    const PRIME: u64 = 0x0000_0100_0000_01b3;
    let mut h = OFFSET;
    let mut eat = |bytes: &[u8]| {
        for b in bytes {
            h ^= u64::from(*b);
            h = h.wrapping_mul(PRIME);
        }
    };
    for (tag, levels) in [(b'B', bids), (b'A', asks)] {
        for l in levels {
            eat(&[tag]);
            eat(&l.price_ticks.to_le_bytes());
            eat(&l.qty.to_le_bytes());
        }
    }
    h
}
