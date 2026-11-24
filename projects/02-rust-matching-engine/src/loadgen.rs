//! Seeded, dependency-free traffic generator.
//!
//! The same [`LoadProfile`] always yields the same command stream, on any
//! platform, so benchmark runs and replay fixtures are reproducible.

use crate::matching_core::{Command, NewOrder, OrdType, OrderId, Price, Qty, Side, Symbol};

/// SplitMix64. Small, fast and fully specified, so streams never change
/// underneath a pinned fixture the way a library RNG's might.
#[derive(Clone, Debug)]
pub struct Rng(u64);

impl Rng {
    pub fn new(seed: u64) -> Self {
        Rng(seed)
    }

    pub fn next_u64(&mut self) -> u64 {
        self.0 = self.0.wrapping_add(0x9E37_79B9_7F4A_7C15);
        let mut z = self.0;
        z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
        z ^ (z >> 31)
    }

    /// Uniform-ish value in `0..n` (`n > 0`). Modulo bias is irrelevant here.
    pub fn below(&mut self, n: u64) -> u64 {
        self.next_u64() % n
    }
}

#[derive(Clone, Debug)]
pub struct LoadProfile {
    pub seed: u64,
    pub symbols: Vec<Symbol>,
    /// Starting mid price for every symbol, in ticks.
    pub mid_ticks: Price,
    /// Limit prices are drawn from `band` ticks on the passive side of mid.
    pub band: Price,
    /// ...up to `cross` ticks through mid, which is what produces trades.
    pub cross: Price,
    pub max_qty: Qty,
    /// Message mix in percent; the remainder is new limit orders.
    pub pct_market: u64,
    pub pct_cancel: u64,
    pub pct_replace: u64,
    /// Once this many ids are tracked as possibly-live, new orders are
    /// replaced by cancels so the book stays bounded.
    pub max_tracked: usize,
}

impl LoadProfile {
    pub fn new(seed: u64, n_symbols: usize) -> Self {
        let symbols = (0..n_symbols.max(1))
            .map(|i| Symbol::new(&format!("SIM{i}")).expect("generated symbol is valid"))
            .collect();
        LoadProfile {
            seed,
            symbols,
            mid_ticks: 100_000,
            band: 20,
            cross: 2,
            max_qty: 100,
            pct_market: 5,
            pct_cancel: 25,
            pct_replace: 10,
            max_tracked: 20_000,
        }
    }

    /// One-line description for benchmark reports.
    pub fn describe(&self) -> String {
        let limit = 100 - self.pct_market - self.pct_cancel - self.pct_replace;
        format!(
            "seed {}, {} symbol(s), mix {limit}% new limit / {}% market / {}% cancel / {}% replace, \
             limit prices uniform in [mid-{}, mid+{}] ticks on the passive..aggressive axis, \
             qty uniform 1..={}, mid random-walks ±1 tick, tracked ids capped at {}",
            self.seed,
            self.symbols.len(),
            self.pct_market,
            self.pct_cancel,
            self.pct_replace,
            self.band,
            self.cross,
            self.max_qty,
            self.max_tracked
        )
    }
}

/// Infinite iterator of commands. Ids it cancels or replaces may already
/// have been filled, so a realistic share of commands is rejected.
pub struct LoadGen {
    profile: LoadProfile,
    rng: Rng,
    next_id: OrderId,
    tracked: Vec<(OrderId, usize, Side)>,
    mids: Vec<Price>,
}

impl LoadGen {
    pub fn new(profile: LoadProfile) -> Self {
        LoadGen {
            rng: Rng::new(profile.seed),
            next_id: 1,
            tracked: Vec::new(),
            mids: vec![profile.mid_ticks; profile.symbols.len()],
            profile,
        }
    }

    fn price(&mut self, sym: usize, side: Side) -> Price {
        let p = &self.profile;
        let offset = self.rng.below((p.band + p.cross + 1) as u64) as Price - p.band;
        match side {
            Side::Buy => self.mids[sym] + offset,
            Side::Sell => self.mids[sym] - offset,
        }
    }

    fn qty(&mut self) -> Qty {
        1 + self.rng.below(self.profile.max_qty)
    }

    fn side(&mut self) -> Side {
        if self.rng.below(2) == 0 {
            Side::Buy
        } else {
            Side::Sell
        }
    }

    fn new_order(&mut self, ord_type: OrdType) -> Command {
        let sym = self.rng.below(self.profile.symbols.len() as u64) as usize;
        // Random-walk the mid, staying well clear of non-positive prices.
        match self.rng.below(16) {
            0 => self.mids[sym] += 1,
            1 if self.mids[sym] > 2 * self.profile.band + 10 => self.mids[sym] -= 1,
            _ => {}
        }
        let side = self.side();
        let order_id = self.next_id;
        self.next_id += 1;
        let price_ticks = match ord_type {
            OrdType::Limit => {
                self.tracked.push((order_id, sym, side));
                Some(self.price(sym, side))
            }
            OrdType::Market => None,
        };
        Command::New(NewOrder {
            order_id,
            account_id: 1 + self.rng.below(8) as u32,
            symbol: self.profile.symbols[sym],
            side,
            ord_type,
            qty: self.qty(),
            price_ticks,
        })
    }

    fn cancel(&mut self) -> Command {
        let i = self.rng.below(self.tracked.len() as u64) as usize;
        let (order_id, _, _) = self.tracked.swap_remove(i);
        Command::Cancel { order_id }
    }
}

impl Iterator for LoadGen {
    type Item = Command;

    fn next(&mut self) -> Option<Command> {
        let p = &self.profile;
        let (market, cancel, replace) = (p.pct_market, p.pct_cancel, p.pct_replace);
        let roll = self.rng.below(100);
        let cmd = if self.tracked.is_empty() {
            self.new_order(OrdType::Limit)
        } else if roll < cancel || self.tracked.len() >= self.profile.max_tracked {
            self.cancel()
        } else if roll < cancel + replace {
            let i = self.rng.below(self.tracked.len() as u64) as usize;
            let (order_id, sym, side) = self.tracked[i];
            Command::Replace {
                order_id,
                qty: self.qty(),
                price_ticks: self.price(sym, side),
            }
        } else if roll < cancel + replace + market {
            self.new_order(OrdType::Market)
        } else {
            self.new_order(OrdType::Limit)
        };
        Some(cmd)
    }
}
