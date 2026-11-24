//! Deterministic simulated venue with fault injection.
//!
//! All data produced here is synthetic. A [`SimSession`] owns one "true" book
//! per symbol, mutates it with a seeded random process, and emits venue-format
//! frames. Faults (loss, duplication, reordering, corruption, crossed quotes,
//! clock skew) are applied between the true book and the wire, so the
//! consumer's reconstruction can be scored against ground truth via
//! [`Checkpoint`]s.

use crate::rng::Rng;
use crate::wire::{encode_level, encode_side, Instrument, RawMsg};
use book_core::{Action, BookDelta, OrderBook, PriceTicks, Qty, Seq, Side, TsMicros};
use serde::{Deserialize, Serialize};
use std::collections::VecDeque;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SimSymbol {
    pub symbol: String,
    pub tick_size: String,
    pub lot_size: String,
    pub start_px_ticks: PriceTicks,
}

impl SimSymbol {
    pub fn new(symbol: &str, start_px_ticks: PriceTicks) -> Self {
        Self {
            symbol: symbol.to_string(),
            tick_size: "0.01".to_string(),
            lot_size: "1".to_string(),
            start_px_ticks,
        }
    }

    pub fn instrument(&self) -> Instrument {
        Instrument {
            symbol: self.symbol.clone(),
            tick_size: self.tick_size.clone(),
            lot_size: self.lot_size.clone(),
        }
    }
}

#[derive(Debug, Clone)]
pub struct SimConfig {
    pub symbols: Vec<SimSymbol>,
    pub seed: u64,
    /// Price levels maintained on each side of the reference price.
    pub levels: i64,
    /// Emit an unsolicited snapshot of every book each N events (0 = never).
    pub snapshot_every: u64,
    /// Events that pass before a requested snapshot is delivered, standing in
    /// for the round trip of a real snapshot request.
    pub snapshot_delay: u64,
    /// Record a ground-truth checkpoint each N deltas per symbol (0 = never).
    pub checkpoint_every: u64,
    pub checkpoint_depth: usize,
    /// Venue clock at the first event when running on virtual time.
    pub start_ts: TsMicros,
    /// Mean gap between events on virtual time.
    pub mean_interval_us: u64,
}

/// 2026-01-01T00:00:00Z, the fixed epoch of virtual-time sessions.
pub const VIRTUAL_EPOCH_US: TsMicros = 1_767_225_600_000_000;

impl Default for SimConfig {
    fn default() -> Self {
        Self {
            symbols: vec![SimSymbol::new("XYZ", 10_000)],
            seed: 42,
            levels: 20,
            snapshot_every: 0,
            snapshot_delay: 25,
            checkpoint_every: 0,
            checkpoint_depth: 10,
            start_ts: VIRTUAL_EPOCH_US,
            mean_interval_us: 1_000,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "fault", rename_all = "snake_case")]
pub enum Fault {
    /// Lose the next `count` deltas.
    Drop { count: u32 },
    /// Deliver the next delta twice.
    Duplicate,
    /// Deliver the next two deltas in swapped order.
    Reorder,
    /// Truncate the next delta so it cannot be decoded.
    Garbage,
    /// The venue itself publishes a bid above the best ask for a while.
    Cross,
    /// Shift venue timestamps of the next `count` deltas by `offset_ms`.
    ClockSkew { offset_ms: i64, count: u32 },
    /// Go silent for `ms` (live sessions only; ignored on virtual time).
    Stall { ms: u64 },
}

/// Parses a schedule such as `drop@500:3,dup@900,reorder@1300,cross@1700`.
/// Each entry is `name@event_index[:arg]`.
pub fn parse_fault_plan(spec: &str) -> Result<Vec<(u64, Fault)>, String> {
    let mut plan = Vec::new();
    for entry in spec.split(',').map(str::trim).filter(|e| !e.is_empty()) {
        let (name, rest) = entry
            .split_once('@')
            .ok_or_else(|| format!("`{entry}`: expected name@index"))?;
        let (at, arg) = match rest.split_once(':') {
            Some((at, arg)) => (at, Some(arg)),
            None => (rest, None),
        };
        let at: u64 = at
            .parse()
            .map_err(|_| format!("`{entry}`: bad event index"))?;
        let num = |default: i64| -> Result<i64, String> {
            arg.map_or(Ok(default), |a| {
                a.parse().map_err(|_| format!("`{entry}`: bad argument"))
            })
        };
        let fault = match name {
            "drop" => Fault::Drop {
                count: num(1)?.max(1) as u32,
            },
            "dup" | "duplicate" => Fault::Duplicate,
            "reorder" => Fault::Reorder,
            "garbage" => Fault::Garbage,
            "cross" => Fault::Cross,
            "skew" => Fault::ClockSkew {
                offset_ms: num(5_000)?,
                count: 50,
            },
            other => return Err(format!("unknown fault `{other}`")),
        };
        plan.push((at, fault));
    }
    plan.sort_by_key(|(at, _)| *at);
    Ok(plan)
}

/// Ground truth at one sequence number.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Checkpoint {
    pub symbol: String,
    pub seq: Seq,
    /// Hex FNV-1a checksum over the top `depth` levels per side.
    pub checksum: String,
    pub best_bid: Option<PriceTicks>,
    pub best_ask: Option<PriceTicks>,
}

pub fn checksum_hex(sum: u64) -> String {
    format!("{sum:016x}")
}

/// One frame as received: the raw line plus the local receive time.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Frame {
    /// Local receive time (microseconds since the Unix epoch, UTC).
    pub recv_ts: TsMicros,
    /// The venue's bytes, untouched.
    pub raw: String,
}

const CROSS_LIFETIME: u32 = 30;

struct SimVenue {
    inst: Instrument,
    book: OrderBook,
    ref_px: PriceTicks,
    seq: Seq,
    levels: i64,
    pending: VecDeque<(Side, PriceTicks, Qty)>,
    cross: Option<(u32, PriceTicks)>,
}

impl SimVenue {
    fn new(sym: &SimSymbol, levels: i64, rng: &mut Rng) -> Self {
        let mut v = Self {
            inst: sym.instrument(),
            book: OrderBook::new(sym.symbol.clone()),
            ref_px: sym.start_px_ticks.max(levels + 2),
            seq: 0,
            levels,
            pending: VecDeque::new(),
            cross: None,
        };
        for off in 0..levels {
            for (side, px) in [(Side::Bid, v.ref_px - off), (Side::Ask, v.ref_px + 1 + off)] {
                let qty = random_qty(rng);
                v.mutate(side, px, qty, 0);
            }
        }
        v
    }

    fn mutate(&mut self, side: Side, price: PriceTicks, qty: Qty, ts: TsMicros) {
        let delta = BookDelta {
            symbol: self.inst.symbol.clone(),
            seq: self.seq,
            side,
            price_ticks: price,
            qty,
            action: if qty == 0 {
                Action::Delete
            } else {
                Action::Set
            },
            event_ts: ts,
        };
        self.book
            .apply_delta(&delta)
            .expect("simulator emits valid deltas");
    }

    fn inject_cross(&mut self) {
        if let Some(ask) = self.book.best(Side::Ask) {
            let px = ask.price_ticks + 1;
            self.pending.push_front((Side::Bid, px, 100));
            self.cross = Some((CROSS_LIFETIME, px));
        }
    }

    fn next_change(&mut self, rng: &mut Rng) -> (Side, PriceTicks, Qty) {
        loop {
            if let Some((left, px)) = self.cross {
                if left == 0 {
                    self.cross = None;
                    if self.book.get(Side::Bid, px).is_some() {
                        return (Side::Bid, px, 0);
                    }
                } else {
                    self.cross = Some((left - 1, px));
                }
            }
            if let Some((side, px, qty)) = self.pending.pop_front() {
                if qty == 0 && self.book.get(side, px).is_none() {
                    continue;
                }
                return (side, px, qty);
            }
            if rng.chance(1, 10) {
                // Move the reference price one tick. Levels that would end up
                // on the wrong side are pulled first, so the true book never
                // crosses on its own; the far edge is trimmed to bound depth.
                // The side that gained room gets a new far level so depth
                // stays roughly constant.
                let n = self.levels;
                if rng.chance(1, 2) {
                    self.ref_px += 1;
                    self.pending.push_back((Side::Ask, self.ref_px, 0));
                    self.pending.push_back((Side::Bid, self.ref_px - n, 0));
                    self.pending
                        .push_back((Side::Ask, self.ref_px + n, random_qty(rng)));
                } else if self.ref_px > n + 2 {
                    self.pending.push_back((Side::Bid, self.ref_px, 0));
                    self.pending.push_back((Side::Ask, self.ref_px + n, 0));
                    self.ref_px -= 1;
                    self.pending
                        .push_back((Side::Bid, self.ref_px - n + 1, random_qty(rng)));
                }
                continue;
            }
            let side = if rng.chance(1, 2) {
                Side::Bid
            } else {
                Side::Ask
            };
            let n = self.levels as u64;
            let off = rng.below(n).min(rng.below(n)) as i64;
            let px = match side {
                Side::Bid => self.ref_px - off,
                Side::Ask => self.ref_px + 1 + off,
            };
            let existing = self.book.get(side, px);
            if existing.is_some() && rng.chance(15, 100) {
                return (side, px, 0);
            }
            let qty = random_qty(rng);
            if existing != Some(qty) {
                return (side, px, qty);
            }
        }
    }

    fn step(&mut self, rng: &mut Rng, ts: TsMicros) -> RawMsg {
        let (side, px, qty) = self.next_change(rng);
        self.seq += 1;
        self.mutate(side, px, qty, ts);
        let [price, size] = encode_level(
            &self.inst,
            book_core::Level {
                price_ticks: px,
                qty,
            },
        );
        RawMsg::Delta {
            symbol: self.inst.symbol.clone(),
            seq: self.seq,
            ts,
            side: encode_side(side).to_string(),
            price,
            size,
        }
    }

    fn snapshot(&self, ts: TsMicros) -> RawMsg {
        let side = |s| {
            self.book
                .top(s, usize::MAX)
                .into_iter()
                .map(|l| encode_level(&self.inst, l))
                .collect()
        };
        RawMsg::Snapshot {
            symbol: self.inst.symbol.clone(),
            seq: self.seq,
            ts,
            bids: side(Side::Bid),
            asks: side(Side::Ask),
        }
    }
}

fn random_qty(rng: &mut Rng) -> Qty {
    (1 + rng.below(50)) * 10
}

pub struct SimSession {
    cfg: SimConfig,
    rng: Rng,
    venues: Vec<SimVenue>,
    clock: TsMicros,
    event_idx: u64,
    out: VecDeque<Frame>,
    plan: VecDeque<(u64, Fault)>,
    requests: Vec<(usize, u64)>,
    drop_left: u32,
    duplicate: bool,
    reorder: bool,
    held: Option<Frame>,
    garbage: bool,
    cross: bool,
    skew: Option<(i64, u32)>,
    stall_ms: Option<u64>,
    checkpoints: Vec<Checkpoint>,
}

/// Fixed stand-in for network transit on virtual time: receive = venue + this.
const VIRTUAL_TRANSIT_US: i64 = 120;

impl SimSession {
    pub fn new(cfg: SimConfig) -> Self {
        Self::with_plan(cfg, Vec::new())
    }

    pub fn with_plan(cfg: SimConfig, mut plan: Vec<(u64, Fault)>) -> Self {
        assert!(!cfg.symbols.is_empty(), "at least one symbol is required");
        assert!(cfg.levels >= 2, "at least two levels per side are required");
        plan.sort_by_key(|(at, _)| *at);
        let mut rng = Rng::new(cfg.seed);
        let venues: Vec<SimVenue> = cfg
            .symbols
            .iter()
            .map(|s| SimVenue::new(s, cfg.levels, &mut rng))
            .collect();
        let mut session = Self {
            clock: cfg.start_ts,
            rng,
            venues,
            event_idx: 0,
            out: VecDeque::new(),
            plan: plan.into(),
            requests: Vec::new(),
            drop_left: 0,
            duplicate: false,
            reorder: false,
            held: None,
            garbage: false,
            cross: false,
            skew: None,
            stall_ms: None,
            checkpoints: Vec::new(),
            cfg,
        };
        // A session opens with a full image of every book.
        for i in 0..session.venues.len() {
            session.push_snapshot(i, session.clock, session.clock + VIRTUAL_TRANSIT_US);
        }
        session
    }

    pub fn instruments(&self) -> Vec<Instrument> {
        self.venues.iter().map(|v| v.inst.clone()).collect()
    }

    pub fn events(&self) -> u64 {
        self.event_idx
    }

    /// Asks the venue for a fresh snapshot; it is delivered in-band after
    /// `snapshot_delay` further events. Repeat requests coalesce.
    pub fn request_snapshot(&mut self, symbol: &str) {
        if let Some(i) = self.venues.iter().position(|v| v.inst.symbol == symbol) {
            if !self.requests.iter().any(|(v, _)| *v == i) {
                self.requests.push((i, self.cfg.snapshot_delay));
            }
        }
    }

    pub fn inject(&mut self, fault: Fault) {
        match fault {
            Fault::Drop { count } => self.drop_left += count,
            Fault::Duplicate => self.duplicate = true,
            Fault::Reorder => self.reorder = true,
            Fault::Garbage => self.garbage = true,
            Fault::Cross => self.cross = true,
            Fault::ClockSkew { offset_ms, count } => self.skew = Some((offset_ms * 1_000, count)),
            Fault::Stall { ms } => self.stall_ms = Some(ms),
        }
    }

    /// A pending stall, for the live driver to honour by sleeping.
    pub fn take_stall(&mut self) -> Option<u64> {
        self.stall_ms.take()
    }

    pub fn take_checkpoints(&mut self) -> Vec<Checkpoint> {
        std::mem::take(&mut self.checkpoints)
    }

    /// Ground truth for a symbol right now (bypasses the fault layer).
    pub fn truth(&self, symbol: &str) -> Option<(Seq, &OrderBook)> {
        self.venues
            .iter()
            .find(|v| v.inst.symbol == symbol)
            .map(|v| (v.seq, &v.book))
    }

    /// Returns the next frame on the wire. `now` is the wall clock for live
    /// sessions; `None` advances a deterministic virtual clock instead.
    pub fn next_frame(&mut self, now: Option<TsMicros>) -> Frame {
        loop {
            if let Some(f) = self.out.pop_front() {
                return f;
            }
            self.advance(now);
        }
    }

    fn push_snapshot(&mut self, venue: usize, ts: TsMicros, recv_ts: TsMicros) {
        let msg = self.venues[venue].snapshot(ts);
        self.out.push_back(Frame {
            recv_ts,
            raw: serde_json::to_string(&msg).expect("raw messages serialize"),
        });
    }

    fn advance(&mut self, now: Option<TsMicros>) {
        self.event_idx += 1;
        let (ts, recv_ts) = match now {
            Some(n) => (n, n),
            None => {
                let step = 1 + self.rng.below(2 * self.cfg.mean_interval_us.max(1));
                self.clock += step as i64;
                (self.clock, self.clock + VIRTUAL_TRANSIT_US)
            }
        };

        while self
            .plan
            .front()
            .is_some_and(|(at, _)| *at <= self.event_idx)
        {
            let (_, fault) = self.plan.pop_front().expect("peeked above");
            self.inject(fault);
        }

        let mut due = Vec::new();
        self.requests.retain_mut(|(venue, left)| {
            if *left == 0 {
                due.push(*venue);
                false
            } else {
                *left -= 1;
                true
            }
        });
        for venue in due {
            self.push_snapshot(venue, ts, recv_ts);
        }
        if self.cfg.snapshot_every > 0 && self.event_idx % self.cfg.snapshot_every == 0 {
            for venue in 0..self.venues.len() {
                self.push_snapshot(venue, ts, recv_ts);
            }
        }

        let vi = self.rng.below(self.venues.len() as u64) as usize;
        if std::mem::take(&mut self.cross) {
            self.venues[vi].inject_cross();
        }
        let venue_ts = match &mut self.skew {
            Some((offset, left)) => {
                let shifted = ts + *offset;
                *left -= 1;
                if *left == 0 {
                    self.skew = None;
                }
                shifted
            }
            None => ts,
        };
        let msg = self.venues[vi].step(&mut self.rng, venue_ts);

        let v = &self.venues[vi];
        if self.cfg.checkpoint_every > 0 && v.seq % self.cfg.checkpoint_every == 0 {
            self.checkpoints.push(Checkpoint {
                symbol: v.inst.symbol.clone(),
                seq: v.seq,
                checksum: checksum_hex(v.book.checksum(self.cfg.checkpoint_depth)),
                best_bid: v.book.best(Side::Bid).map(|l| l.price_ticks),
                best_ask: v.book.best(Side::Ask).map(|l| l.price_ticks),
            });
        }

        if self.drop_left > 0 {
            self.drop_left -= 1;
            return;
        }
        let mut raw = serde_json::to_string(&msg).expect("raw messages serialize");
        if std::mem::take(&mut self.garbage) {
            raw.truncate(raw.len() / 2);
        }
        let frame = Frame { recv_ts, raw };
        if std::mem::take(&mut self.reorder) {
            self.held = Some(frame);
            return;
        }
        if std::mem::take(&mut self.duplicate) {
            self.out.push_back(frame.clone());
        }
        self.out.push_back(frame);
        if let Some(mut held) = self.held.take() {
            // Arrives late, so it is stamped with the later receive time.
            held.recv_ts = recv_ts;
            self.out.push_back(held);
        }
    }
}
